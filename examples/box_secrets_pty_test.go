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
// echoed; a paste longer than a terminal line (4096 bytes here, 1024 on
// macOS) is taken whole; nothing is edited, so a Backspace, an arrow
// key, a NUL or any other control character refuses the value; and the
// terminal is left as it was, after Ctrl-C too.

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
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	var err error
	select {
	case err = <-waited:
	case <-time.After(30 * time.Second):
		// Stopped (^Z) or waiting for more: either way, not done.
		_ = cmd.Process.Kill()
		<-waited
		t.Fatalf("still running 30 s after the keys %q", keys)
	}
	if err != nil {
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
	out, code := r.typeAtPrompt(t, "hunter2-xyz\r", "APP=example", "VAR=TYPED", "BOX_KEY="+box)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	r.mustDecrypt(boxID, at("TYPED"), "hunter2-xyz")
	if strings.Contains(out, "hunter2") {
		t.Errorf("the value was echoed:\n%q", out)
	}
	for _, c := range []struct{ name, keys, want string }{
		{"LONG", strings.Repeat("a", 5000) + "\r", strings.Repeat("a", 5000)},
		{"WIDE", "caf" + string(rune(0xe9)) + " " + string(rune(0x1f600)) + "\r", "caf" + string(rune(0xe9)) + " " + string(rune(0x1f600))},
	} {
		if out, code := r.typeAtPrompt(t, c.keys, "APP=example", "VAR="+c.name); code != 0 {
			t.Fatalf("%s: exit %d\n%s", c.name, code, out)
		}
		r.mustDecrypt(boxID, at(c.name), c.want)
	}

	// Nothing typed is edited, and no key becomes part of a value:
	// Backspace (DEL or ^H), ^U, an arrow key, a NUL (which a shell
	// variable would have dropped on the way), a tab.
	for _, c := range []struct{ name, keys string }{
		{"DEL", "hunter2-xyzz\x7f\r"},
		{"BS", "ab\bc\r"},
		{"KILL", "wrong\x15right\r"},
		{"ARROW", "ab\x1b[Dc\r"},
		{"NUL", "ab\x00c\r"},
		{"TAB", "a\tb\r"},
		// Keys the terminal would act on itself, unseen: flow control,
		// ^V's literal next, ^O's discard, and quit and suspend, which
		// would kill or stop this with echo off.
		{"XOFF", "a\x13b\r"},
		{"XON", "a\x11b\r"},
		{"LNEXT", "a\x16b\r"},
		{"DISCARD", "a\x0fb\r"},
		{"QUIT", "a\x1cb\r"},
		{"SUSP", "a\x1ab\r"},
		{"EOF", "a\x04b\r"},
	} {
		out, code := r.typeAtPrompt(t, c.keys, "APP=example", "VAR="+c.name)
		if code != 1 || !strings.Contains(out, c.name+" has a control character in it: typing is not shown, so the prompt takes no edits") || r.exists(at(c.name)) {
			t.Errorf("%s: exit %d\n%s", c.name, code, out)
		}
	}
	// Ctrl-C leaves, and leaves the terminal as it found it.
	if out, code := r.typeAtPrompt(t, "abc\x03", "APP=example", "VAR=GONE"); code != 130 || r.exists(at("GONE")) {
		t.Errorf("Ctrl-C: exit %d\n%s", code, out)
	}
}
