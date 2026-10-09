package proof

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Verifier runs ssh-keygen -Y over a commit's signature. The verdict is
// the exit status alone; stdout of find-principals names the signer;
// stderr is bounded into the error of a run that could not answer.
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

// Verify is step 12 for one commit: a refusal by name for a commit with
// no gpgsig, an OpenPGP one, or a key the signers do not list; the
// principal of the signer whose key verifies otherwise. The payload is
// the commit object without its gpgsig header, handed to ssh-keygen on
// stdin; the namespace is `git`, as git signs. A signature by a listed
// key that does not verify over the payload is refused too: the commit
// was altered after it was signed.
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
		return "", refuse("%s is not signed; the box applies only commits signed by a key in its signer list", c.ID)
	case OpenPGPSig:
		return "", refuse("%s is signed by OpenPGP, not by an SSH key in the Caddyfile this box runs; GitHub's merge button cannot land config — merge on a laptop and push", c.ID)
	case OtherSig:
		return "", refuse("%s is signed, but not by an SSH key in the Caddyfile this box runs", c.ID)
	case SSHSig:
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
	sigFile, allowedFile := filepath.Join(dir, "sig"), filepath.Join(dir, "allowed_signers")
	for name, data := range map[string][]byte{sigFile: c.Signature, allowedFile: allowed} {
		if err := os.WriteFile(name, data, 0o644); err != nil { //nolint:gosec // readable by the child's uid; nothing secret
			return "", fmt.Errorf("verify: %w", err)
		}
		// The mode is set after the write, so the process umask has
		// no say: the child of another uid must be able to read it.
		if err := os.Chmod(name, 0o644); err != nil { //nolint:gosec // as above
			return "", fmt.Errorf("verify: %w", err)
		}
	}

	// Which listed key, if any, made the signature. ssh-keygen prints
	// the matching principals and exits non-zero when none matches.
	out, ok, err := v.run(ctx, keygen, dir, nil, "-Y", "find-principals", "-s", sigFile, "-f", allowedFile)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", refuse("%s is signed by a key that is not a signer in the Caddyfile this box runs", c.ID)
	}
	principal, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if !signers.Has(principal) {
		return "", fmt.Errorf("verify: ssh-keygen named a principal the signer list does not carry")
	}
	// The verdict: the signature, by that principal's key, over the
	// payload, in git's namespace.
	_, ok, err = v.run(ctx, keygen, dir, c.Payload, "-Y", "verify", "-f", allowedFile, "-I", principal, "-n", "git", "-s", sigFile)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", refuse("%s is signed by %s, but the signature does not verify: the commit was altered after it was signed", c.ID, principal)
	}
	return principal, nil
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

// run executes one bounded child with PATH alone in its environment
// and the verifier's uid, with stdin as given. ok is the exit status
// being zero; err is a run that gave no verdict (the program could not
// start, the deadline passed, the child was killed).
func (v *Verifier) run(ctx context.Context, program, dir string, stdin []byte, args ...string) (stdout []byte, ok bool, err error) {
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
	err = cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return o.Bytes(), true, nil
	case bounded.Err() != nil:
		return nil, false, fmt.Errorf("ssh-keygen %s: %w", args[1], bounded.Err())
	case errors.As(err, &exit) && exit.Exited():
		return o.Bytes(), false, nil
	default:
		return nil, false, fmt.Errorf("ssh-keygen %s: %w: %s", args[1], err, strings.TrimSpace(e.String()))
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
