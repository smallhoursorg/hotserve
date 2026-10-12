//go:build linux

package examples

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// make secret's prompt, on a pseudoterminal of Linux's own (/dev/ptmx,
// no module beyond the standard library): what is typed is never
// echoed; Backspace, ^U and ^W edit it; a paste longer than a terminal
// line (4096 bytes here, 1024 on macOS) is taken whole; any other
// control character is refused; and the terminal is left as it was,
// after Ctrl-C too.

type pty struct{ master, slave *os.File }

func ioctl(fd, req, arg uintptr) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg); e != 0 {
		return e
	}
	return nil
}

func openPTY(t *testing.T) *pty {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudoterminal here: %v", err)
	}
	var n uint32
	if err := ioctl(m.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); err != nil {
		t.Fatal(err)
	}
	var unlock int32
	if err := ioctl(m.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); err != nil {
		t.Fatal(err)
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(); _ = s.Close() })
	return &pty{m, s}
}

func (p *pty) termios(t *testing.T) syscall.Termios {
	t.Helper()
	var tio syscall.Termios
	if err := ioctl(p.slave.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&tio))); err != nil {
		t.Fatal(err)
	}
	return tio
}

// typeAtPrompt runs bin/secrets set on a terminal of its own and types
// keys once the prompt shows. It returns what the terminal showed and
// the exit status, and checks the terminal is as it was.
func (r *boxRepo) typeAtPrompt(t *testing.T, keys string, env ...string) (string, int) {
	t.Helper()
	p := openPTY(t)
	before := p.termios(t)
	cmd := exec.Command(filepath.Join(r.dir, "bin", "secrets"), "set")
	cmd.Dir, cmd.Env = r.dir, append(append([]string{}, r.env...), env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = p.slave, p.slave, p.slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var shown bytes.Buffer
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := p.master.Read(buf)
			mu.Lock()
			shown.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	seen := func(s string) bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(shown.String(), s)
	}
	for deadline := time.Now().Add(30 * time.Second); !seen("(typing is not shown): "); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatalf("no prompt in 30 s")
		}
	}
	if _, err := p.master.Write([]byte(keys)); err != nil {
		t.Fatal(err)
	}
	code := 0
	if err := cmd.Wait(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatal(err)
		}
		code = exit.ExitCode()
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			code = 128 + int(ws.Signal())
		}
	}
	// What it printed last is in the terminal's buffer: read it out.
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if seen("secret: ") {
			time.Sleep(50 * time.Millisecond)
			break
		}
	}
	if after := p.termios(t); after != before {
		t.Errorf("the terminal was left changed: lflag %#o, was %#o", after.Lflag, before.Lflag)
	}
	mu.Lock()
	defer mu.Unlock()
	return shown.String(), code
}

func TestBoxSecretPrompt(t *testing.T) {
	r := secretsRepo(t)
	r.key("id_ed25519")
	if out, code := r.run("signer"); code != 0 {
		t.Fatalf("make signer: exit %d\n%s", code, out)
	}
	boxID, box := r.boxKey("box")
	at := func(v string) string { return "deploy.example.com/secrets/example/" + v + ".age" }

	// Enter is a carriage return at a terminal, as here.
	out, code := r.typeAtPrompt(t, "hunter2-xyzz\x7f\r", "APP=example", "VAR=TYPED", "BOX_KEY="+box)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	r.mustDecrypt(boxID, at("TYPED"), "hunter2-xyz")
	if strings.Contains(out, "hunter2") {
		t.Errorf("the value was echoed:\n%q", out)
	}
	for _, c := range []struct{ name, keys, want string }{
		{"KILLED", "wrong\x15right\r", "right"},
		{"WORD", "one two\x17three\r", "one three"},
		{"WIDE", "caf" + string(rune(0xe9)) + "\x7fe\r", "cafe"}, // Backspace takes the whole é, both bytes
		{"BS", "ab\bc\r", "ac"},                                  // ^H, as some terminals send it
		{"LONG", strings.Repeat("a", 5000) + "\r", strings.Repeat("a", 5000)},
	} {
		if out, code := r.typeAtPrompt(t, c.keys, "APP=example", "VAR="+c.name); code != 0 {
			t.Fatalf("%s: exit %d\n%s", c.name, code, out)
		}
		r.mustDecrypt(boxID, at(c.name), c.want)
	}

	// A NUL typed (^@) is refused, not dropped on the way.
	if out, code := r.typeAtPrompt(t, "ab\x00c\r", "APP=example", "VAR=NUL"); code != 1 || !strings.Contains(out, "NUL has a control character in it") || r.exists(at("NUL")) {
		t.Errorf("a typed NUL: exit %d\n%s", code, out)
	}
	// An arrow key is a control sequence, not part of a value.
	if out, code := r.typeAtPrompt(t, "ab\x1b[Dc\r", "APP=example", "VAR=ARROW"); code != 1 || !strings.Contains(out, "ARROW has a control character in it (an arrow key, say)") || r.exists(at("ARROW")) {
		t.Errorf("an arrow key: exit %d\n%s", code, out)
	}
	// Ctrl-C leaves, and leaves the terminal as it found it.
	if out, code := r.typeAtPrompt(t, "abc\x03", "APP=example", "VAR=GONE"); code != 130 || r.exists(at("GONE")) {
		t.Errorf("Ctrl-C: exit %d\n%s", code, out)
	}
}
