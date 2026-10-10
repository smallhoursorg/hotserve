package proof

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

// Verifier runs ssh-keygen -Y verify over a commit's signature. Which
// listed key signed is the box's own decision — the SSHSIG blob carries
// its public key, matched byte for byte against the signer list — and
// ssh-keygen's exit status is the cryptographic verdict. Because that
// status is the same for "the signature is wrong" and "I could not
// read my files", a failure is taken as a verdict only after a built-in
// control signature verifies in the same environment; otherwise it is
// the box's error. Output is bounded into the error of a run that
// could not answer.
type Verifier struct {
	// SSHKeygen is the program; empty looks `ssh-keygen` up on PATH
	// once, at the first call, and keeps what it found.
	SSHKeygen string
	// RunAs is the uid (and gid) the children run as; zero leaves the
	// caller's. The applier passes 65534 (DESIGN-box.md, step 12), so
	// its CapabilityBoundingSet keeps CAP_SETUID and CAP_SETGID, and
	// CAP_KILL so that the deadline below can end a child of that uid.
	RunAs uint32
	// TempDir is where the signature and the allowed_signers file are
	// written for the children to read: a directory 0755 with files
	// 0644, so a child of another uid can read them, removed after.
	// Empty is os.TempDir() — the unit's PrivateTmp.
	TempDir string
	// Timeout bounds each child; zero is 30 seconds.
	Timeout time.Duration
	// ChainTimeout bounds a whole VerifyChain, every child of every
	// commit together; zero is 10 minutes. A chain is at most 500
	// commits and a verification is milliseconds, so a legitimate push
	// is minutes from this bound on the slowest box; without it, a
	// child that stalls at each commit could hold the applier — and
	// the admission lock — for the sum of its per-child deadlines.
	ChainTimeout time.Duration

	lookup   sync.Once
	found    string
	foundErr error
}

const (
	defaultVerifyTimeout = 30 * time.Second
	defaultChainTimeout  = 10 * time.Minute
	maxChildOutput       = 4 << 10
)

// The control vector: a throwaway ed25519 key's signature, in the git
// namespace, over controlPayload. Public key and signature only; the
// private key was never kept. When ssh-keygen verifies this and not the
// push's signature, the push's is wrong; when it verifies neither, the
// box cannot verify anything right now.
const (
	controlPrincipal = "control"
	controlPayload   = "hotserve box verifier control\n"
	controlAllowed   = controlPrincipal + ` namespaces="git" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJdMJ0Oxfpgsaaqt+xg4HPptSWfkEqcYKc5ZXY0a19zQ` + "\n"
	controlSignature = `-----BEGIN SSH SIGNATURE-----
U1NIU0lHAAAAAQAAADMAAAALc3NoLWVkMjU1MTkAAAAgl0wnQ7F+mCxpqq37GDgc+m1JZ+
QSpxgpzlldjRrX3NAAAAADZ2l0AAAAAAAAAAZzaGE1MTIAAABTAAAAC3NzaC1lZDI1NTE5
AAAAQJAdqp0Lh8ioeESTHi9TvSCr7PV4b0ESrIKsJVkGzJgzjgeE3SszsHIcnWQL7WEq2A
/HT/++7LaSbZF/F3qpCw8=
-----END SSH SIGNATURE-----
`
)

// Verify is step 12 for one commit: a refusal by name for a commit with
// no gpgsig, an OpenPGP one, one the box cannot read, or a key the
// signers do not list; the principal of the signer whose key verifies
// otherwise. The payload is the commit object without its gpgsig
// header, handed to ssh-keygen on stdin; the namespace is `git`, as git
// signs. A signature by a listed key that does not verify over the
// payload is refused too: the commit was altered after it was signed.
func (v *Verifier) Verify(ctx context.Context, c *Commit, signers Signers) (string, error) {
	allowed, err := signers.AllowedSigners()
	if err != nil {
		return "", err
	}
	return v.verify(ctx, c, signers, allowed)
}

// verify is Verify with the allowed_signers file already rendered, so
// a chain renders it once rather than once per commit.
func (v *Verifier) verify(ctx context.Context, c *Commit, signers Signers, allowed []byte) (string, error) {
	switch c.Kind {
	case Unsigned:
		return "", refuseCode(codeUnsigned, "%s is not signed; the box applies only commits signed by a key in its signer list", c.ID)
	case OpenPGPSig:
		return "", refuseCode(codeUnsigned, "%s is signed by OpenPGP, not by an SSH key in the Caddyfile this box runs; GitHub's merge button cannot land config — merge on a laptop and push", c.ID)
	case OtherSig:
		return "", refuseCode(codeUnsigned, "%s is signed, but not by an SSH key in the Caddyfile this box runs", c.ID)
	case SSHSig:
	}
	// Which key signed: the signature says, and the list decides. The
	// namespace is checked here too, by name: ssh-keygen would refuse
	// a `file` signature over the commit the same way as a wrong one.
	key, namespace, err := SignatureKey(c.Signature)
	if err != nil {
		return "", refuseCode(codeUnsigned, "%s is signed, but the signature is not one the box can read", c.ID)
	}
	if namespace != "git" {
		return "", refuseCode(codeUnsigned, "%s is signed in the %s namespace, not git; the box applies only commits git signed", c.ID, Bound(namespace))
	}
	principal, ok := signers.PrincipalFor(key)
	if !ok {
		return "", refuseCode(codeUnlisted, "%s is signed by a key that is not a signer in the Caddyfile this box runs", c.ID)
	}

	keygen, err := v.program()
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(v.TempDir, "box-verify-"+c.ID+".")
	if err != nil {
		return "", fmt.Errorf("verify: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()     // the unit's private /tmp goes with it in any case
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // a child of another uid must traverse it; the files are a public key list and a signature
		return "", fmt.Errorf("verify: %w", err)
	}
	if err := writeReadable(dir, map[string][]byte{"sig": c.Signature, "allowed_signers": allowed}); err != nil {
		return "", err
	}

	// The verdict: the signature, by that principal's key, over the
	// payload, in git's namespace.
	res, err := v.run(ctx, keygen, dir, c.Payload, "-Y", "verify", "-f", "allowed_signers", "-I", principal, "-n", "git", "-s", "sig")
	if err != nil {
		return "", err
	}
	if res.ok {
		return principal, nil
	}
	// Not verified — but ssh-keygen says that the same way for a wrong
	// signature and for a file it could not read. The control tells
	// which: a known-good signature, verified the same way, as the
	// same uid, in the same directory. Written only now, on the path
	// that needs it.
	if err := writeReadable(dir, map[string][]byte{"control.sig": []byte(controlSignature), "control.allowed": []byte(controlAllowed)}); err != nil {
		return "", err
	}
	control, err := v.run(ctx, keygen, dir, []byte(controlPayload), "-Y", "verify", "-f", "control.allowed", "-I", controlPrincipal, "-n", "git", "-s", "control.sig")
	if err != nil {
		return "", err
	}
	if !control.ok {
		return "", fmt.Errorf("ssh-keygen could not verify a known-good signature, so it could not verify %s either: %s", c.ID, Bound(strings.TrimSpace(control.stderr)))
	}
	// The environment is fine and the key is listed, so what remains
	// is the signature itself: made over other bytes, or by a form of
	// key this ssh-keygen will not take. Both are the push's problem.
	return "", refuseCode(codeAltered, "%s is signed by %s, but the signature does not verify over the commit", c.ID, principal)
}

// writeReadable writes files a child of another uid can read: 0644,
// set after the write so the process umask has no say.
func writeReadable(dir string, files map[string][]byte) error {
	for name, data := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil { //nolint:gosec // readable by the child's uid; nothing secret
			return fmt.Errorf("verify: %w", err)
		}
		if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // as above
			return fmt.Errorf("verify: %w", err)
		}
	}
	return nil
}

// SignatureKey is the public key an SSH signature carries, and the
// namespace it was made in: the SSHSIG blob under the armor is
// `SSHSIG`, a version, the key in wire format, the namespace, a
// reserved string, the hash algorithm and the signature itself. The
// key is what the signer list is matched against, the namespace must
// be git's; everything else is ssh-keygen's to judge.
func SignatureKey(armored []byte) (key []byte, namespace string, err error) {
	const begin, end = "-----BEGIN SSH SIGNATURE-----", "-----END SSH SIGNATURE-----"
	text := string(armored)
	i := strings.Index(text, begin)
	if i < 0 {
		return nil, "", errors.New("not an armored SSH signature")
	}
	body := text[i+len(begin):]
	// END is looked for after BEGIN: the two markers share their
	// dashes, so an END that overlaps BEGIN's tail is not an end.
	j := strings.Index(body, end)
	if j < 0 {
		return nil, "", errors.New("not an armored SSH signature")
	}
	var b64 strings.Builder
	for _, line := range strings.Split(body[:j], "\n") {
		b64.WriteString(strings.TrimSpace(line))
	}
	blob, err := base64.StdEncoding.DecodeString(b64.String())
	if err != nil {
		return nil, "", fmt.Errorf("signature armor: %w", err)
	}
	if !bytes.HasPrefix(blob, []byte("SSHSIG")) {
		return nil, "", errors.New("signature blob has no SSHSIG magic")
	}
	var sig struct {
		Version   uint32
		PublicKey []byte
		Namespace string
		Reserved  string
		HashAlg   string
		Signature []byte
	}
	if err := ssh.Unmarshal(blob[len("SSHSIG"):], &sig); err != nil {
		return nil, "", fmt.Errorf("signature blob: %w", err)
	}
	if sig.Version != 1 {
		return nil, "", fmt.Errorf("signature version %d", sig.Version)
	}
	if _, err := ssh.ParsePublicKey(sig.PublicKey); err != nil {
		return nil, "", fmt.Errorf("signature's key: %w", err)
	}
	return sig.PublicKey, sig.Namespace, nil
}

// program is the ssh-keygen to run: SSHKeygen as given, else the PATH
// lookup done once and kept, so every commit of a chain runs the same
// binary whatever PATH does meanwhile.
func (v *Verifier) program() (string, error) {
	if v.SSHKeygen != "" {
		return v.SSHKeygen, nil
	}
	v.lookup.Do(func() {
		v.found, v.foundErr = exec.LookPath("ssh-keygen")
		if v.foundErr != nil {
			v.foundErr = fmt.Errorf("ssh-keygen is not installed: %w", v.foundErr)
		}
	})
	return v.found, v.foundErr
}

// result is what one child came to: its exit status as a verdict, and
// its bounded output.
type result struct {
	ok             bool
	stdout, stderr string
}

// run executes one bounded child with PATH alone in its environment
// and the verifier's uid, in dir, with stdin as given. ok is the exit
// status being zero; err is a run that gave no verdict (the program
// could not start, the deadline passed, the child was killed).
func (v *Verifier) run(ctx context.Context, program, dir string, stdin []byte, args ...string) (result, error) {
	timeout := v.Timeout
	if timeout == 0 {
		timeout = defaultVerifyTimeout
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(bounded, program, args...) //nolint:gosec // ssh-keygen, with fixed flags and files the verifier wrote
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	if v.RunAs != 0 {
		// Groups empty and NoSetGroups false: setgroups(0, NULL), so
		// the child keeps none of the caller's supplementary groups.
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: v.RunAs, Gid: v.RunAs, Groups: []uint32{}}}
	}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var o, e boundedBuffer
	cmd.Stdout, cmd.Stderr = &o, &e
	// What the child leaves behind holding its pipes is not waited for.
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	what := program
	if len(args) > 1 {
		what = "ssh-keygen " + args[1]
	}
	res := result{stdout: o.String(), stderr: e.String()}
	var exit *exec.ExitError
	switch {
	case err == nil:
		res.ok = true
		return res, nil
	case bounded.Err() != nil:
		return result{}, fmt.Errorf("%s: %w", what, bounded.Err())
	case errors.As(err, &exit) && exit.Exited():
		return res, nil
	default:
		return result{}, fmt.Errorf("%s: %w: %s", what, err, Bound(strings.TrimSpace(res.stderr)))
	}
}

// boundedBuffer keeps the first maxChildOutput bytes written to it and
// drops the rest, so a child's output cannot grow the error text. It
// deliberately does not embed bytes.Buffer: os/exec copies a child's
// output with io.Copy, which would take the embedded ReadFrom and
// never call Write, and the cap would be dead.
type boundedBuffer struct {
	b []byte
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := maxChildOutput - len(b.b); len(p) > room {
		b.b = append(b.b, p[:room]...)
		return len(p), nil
	}
	b.b = append(b.b, p...)
	return len(p), nil
}

func (b *boundedBuffer) Bytes() []byte  { return b.b }
func (b *boundedBuffer) String() string { return string(b.b) }
