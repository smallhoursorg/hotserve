package proof

import (
	"bytes"
	"context"
	"io"
	"os/exec"
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
	out, ok, err := v.run(context.Background(), cat, t.TempDir(), big)
	if err != nil || !ok || len(out) != maxChildOutput {
		t.Fatalf("%v %v %d", err, ok, len(out))
	}
}

// TestRunVerdicts: an exit status is a verdict, a deadline is not.
func TestRunVerdicts(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	v := &Verifier{Timeout: 10 * time.Second}
	ctx := context.Background()
	if out, ok, err := v.run(ctx, sh, t.TempDir(), nil, "-c", "echo hi"); err != nil || !ok || strings.TrimSpace(string(out)) != "hi" {
		t.Fatalf("%q %v %v", out, ok, err)
	}
	if out, ok, err := v.run(ctx, sh, t.TempDir(), nil, "-c", "echo no >&2; exit 3"); err != nil || ok || len(out) != 0 {
		t.Fatalf("%q %v %v", out, ok, err)
	}
	slow := &Verifier{Timeout: 200 * time.Millisecond}
	start := time.Now()
	_, _, err = slow.run(ctx, sh, t.TempDir(), nil, "-c", "sleep 5")
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
