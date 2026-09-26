package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// tty is the operator's terminal: /dev/tty, which is the terminal the
// command was started from whatever its standard streams are, so a
// secret is typed to nothing a pipe or a log could hold. With echo off
// for a secret, and on again whatever ends the question.
type tty struct {
	f  *os.File
	in *bufio.Reader
	// failed is the first write that did not reach the terminal: after
	// it, what was to be shown may not have been, and no question is
	// asked — least of all whether a password was stored.
	failed error
}

// openTTY opens the terminal; with none, the refusal names the file a
// provisioning tool writes instead.
func openTTY(envFile string) (*tty, error) {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("setup asks for secrets at a terminal, and there is none here; from a provisioning tool, write %s by hand (backups/README.md, \"By hand\")", envFile)
	}
	return &tty{f: f, in: bufio.NewReader(f)}, nil
}

func (t *tty) Close() { _ = t.f.Close() }

// Say goes to the terminal too, not to standard output: the one thing
// setup shows that is a secret — a new repository's password — must
// reach the person at the terminal and nothing that stdout was sent to.
func (t *tty) Say(line string) {
	if _, err := fmt.Fprintln(t.f, line); err != nil && t.failed == nil {
		t.failed = err
	}
}

// Ask puts the prompt on the terminal and reads one line from it. A
// secret is read with echo off — off before the prompt is shown, so
// that nothing typed ahead of it is echoed after it — and echo is put
// back before the answer is returned, and before an interrupt or
// silence ends the question. An interrupt is no answer; so is silence
// for answerWithin.
func (t *tty) Ask(ctx context.Context, prompt string, secret bool) (string, error) {
	if t.failed != nil {
		return "", fmt.Errorf("the terminal could not be written to, so what was to be shown may not have been: %w", t.failed)
	}
	if secret {
		restore, err := t.echoOff()
		if err != nil {
			return "", err
		}
		defer restore()
		defer func() { _, _ = fmt.Fprintln(t.f) }()
	}
	if _, err := fmt.Fprint(t.f, prompt); err != nil {
		return "", fmt.Errorf("the terminal could not be written to: %w", err)
	}
	line, err := readLine(ctx, t.in)
	if errors.Is(err, errNoAnswer) {
		return "", fmt.Errorf("no answer in %s", answerWithin)
	}
	return line, err
}

// echoOff turns the terminal's echo off and returns what turns it on
// again.
func (t *tty) echoOff() (func(), error) {
	fd := int(t.f.Fd())
	was, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, fmt.Errorf("reading the terminal's settings: %w", err)
	}
	now := *was
	now.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &now); err != nil {
		return nil, fmt.Errorf("turning the terminal's echo off: %w", err)
	}
	return func() { _ = unix.IoctlSetTermios(fd, unix.TCSETS, was) }, nil
}
