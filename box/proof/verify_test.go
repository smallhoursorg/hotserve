package proof

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBoundedBuffer holds the cap on a child's output through both
// paths os/exec and io.Copy can take: a plain Write, a WriterTo source,
// and a real child process writing more than the cap.
func TestBoundedBuffer(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 100_000)
	var b boundedBuffer
	if n, err := b.Write(big); err != nil || n != len(big) || len(b.Bytes()) != maxChildOutput {
		t.Fatalf("%d %v %d", n, err, len(b.Bytes()))
	}
	var c boundedBuffer
	if _, err := io.Copy(&c, bytes.NewReader(big)); err != nil || len(c.String()) != maxChildOutput {
		t.Fatalf("%v %d", err, len(c.String()))
	}
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Skip("no cat")
	}
	v := &Verifier{Timeout: 10 * time.Second}
	res, err := v.run(context.Background(), cat, t.TempDir(), big)
	if err != nil || !res.ok || len(res.stdout) != maxChildOutput {
		t.Fatalf("%v %v %d", err, res.ok, len(res.stdout))
	}
}

// TestControlVector: the built-in signature verifies with the system's
// ssh-keygen, and a wrong payload does not — which is what lets a
// failed verification be read as a verdict.
func TestControlVector(t *testing.T) {
	needTools(t, "ssh-keygen")
	key, namespace, err := SignatureKey([]byte(controlSignature))
	if err != nil || namespace != "git" {
		t.Fatal(namespace, err)
	}
	f := strings.Fields(controlAllowed)
	ctl, err := ParseSigner(f[0], f[2], f[3])
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := (Signers{ctl}).PrincipalFor(key); !ok || p != controlPrincipal {
		t.Fatalf("%q %v", p, ok)
	}
	v := &Verifier{TempDir: t.TempDir()}
	c := &Commit{ID: zeroID, Kind: SSHSig, Signature: []byte(controlSignature), Payload: []byte(controlPayload)}
	if p, err := v.Verify(context.Background(), c, Signers{ctl}); err != nil || p != controlPrincipal {
		t.Fatalf("%q %v", p, err)
	}
	wrong := *c
	wrong.Payload = []byte("something else\n")
	_, err = v.Verify(context.Background(), &wrong, Signers{ctl})
	refusalContaining(t, err, zeroID+" is signed by control, but the signature does not verify")
	// With a broken verifier the same failure is the box's error.
	broken := &Verifier{SSHKeygen: "/bin/false", TempDir: t.TempDir()}
	_, err = broken.Verify(context.Background(), c, Signers{ctl})
	var r *Refusal
	if err == nil || errors.As(err, &r) || !strings.Contains(err.Error(), "could not verify a known-good signature") {
		t.Fatalf("%v", err)
	}
	// And the key the signature names is what the list is matched on.
	for _, bad := range []string{"", "-----BEGIN SSH SIGNATURE-----\nU1NIU0lH\n-----END SSH SIGNATURE-----\n", "-----BEGIN SSH SIGNATURE-----\n!!!!\n-----END SSH SIGNATURE-----\n", "-----BEGIN SSH SIGNATURE-----\nAAAA\n-----END SSH SIGNATURE-----\n"} {
		if _, _, err := SignatureKey([]byte(bad)); err == nil {
			t.Fatalf("%q parsed", bad)
		}
	}
	unreadable := *c
	unreadable.Signature = []byte("-----BEGIN SSH SIGNATURE-----\nU1NIU0lH\n-----END SSH SIGNATURE-----\n")
	_, err = v.Verify(context.Background(), &unreadable, Signers{ctl})
	refusalContaining(t, err, zeroID+" is signed, but the signature is not one the box can read")

	// A signature in another namespace is refused by name, not as a
	// verification failure: made here with a fresh key over the same
	// payload, in the `file` namespace.
	d := t.TempDir()
	run(t, d, nil, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", "k", "-C", "k")
	if err := os.WriteFile(filepath.Join(d, "msg"), []byte(controlPayload), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, d, nil, "ssh-keygen", "-Y", "sign", "-f", "k", "-n", "file", "msg")
	sig, err := os.ReadFile(filepath.Join(d, "msg.sig"))
	if err != nil {
		t.Fatal(err)
	}
	pub, err := os.ReadFile(filepath.Join(d, "k.pub"))
	if err != nil {
		t.Fatal(err)
	}
	f = strings.Fields(string(pub))
	k, err := ParseSigner("k", f[0], f[1])
	if err != nil {
		t.Fatal(err)
	}
	other := &Commit{ID: zeroID, Kind: SSHSig, Signature: sig, Payload: []byte(controlPayload)}
	_, err = v.Verify(context.Background(), other, Signers{k})
	refusalContaining(t, err, zeroID+" is signed in the file namespace, not git")
}

// TestRunVerdicts: an exit status is a verdict, a deadline is not.
func TestRunVerdicts(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	v := &Verifier{Timeout: 10 * time.Second}
	ctx := context.Background()
	if res, err := v.run(ctx, sh, t.TempDir(), nil, "-c", "echo hi"); err != nil || !res.ok || strings.TrimSpace(res.stdout) != "hi" {
		t.Fatalf("%+v %v", res, err)
	}
	if res, err := v.run(ctx, sh, t.TempDir(), nil, "-c", "echo no >&2; exit 3"); err != nil || res.ok || res.stdout != "" || strings.TrimSpace(res.stderr) != "no" {
		t.Fatalf("%+v %v", res, err)
	}
	// A run with no subcommand still names the program on the error path.
	none := &Verifier{Timeout: 200 * time.Millisecond}
	if _, err := none.run(ctx, sh, t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	slow := &Verifier{Timeout: 200 * time.Millisecond}
	start := time.Now()
	_, err = slow.run(ctx, sh, t.TempDir(), nil, "-c", "sleep 5")
	if err == nil || !strings.Contains(err.Error(), "deadline") || time.Since(start) > 3*time.Second {
		t.Fatalf("%v after %s", err, time.Since(start))
	}
	// The program is looked up once and kept.
	v = &Verifier{}
	p1, err1 := v.program()
	p2, err2 := v.program()
	if err1 != nil || err2 != nil || p1 == "" || p1 != p2 {
		t.Fatalf("%q %v %q %v", p1, err1, p2, err2)
	}
}
