package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// tty is the operator's terminal: /dev/tty, which is the terminal the
// command was started from whatever its standard streams are, so a
// secret is typed to nothing a pipe or a log could hold. With echo off
// for a secret, and on again whatever ends the question.
type tty struct {
	f  *os.File
	in *bufio.Reader
}

func openTTY() (*tty, error) {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, errors.New("setup asks for secrets at a terminal, and there is none here; from a provisioning tool, write /etc/hotserve-backup/repository.env by hand (backups/README.md, \"By hand\")")
	}
	return &tty{f: f, in: bufio.NewReader(f)}, nil
}

func (t *tty) Close() { _ = t.f.Close() }

// Say goes to the terminal too, not to standard output: the one thing
// setup shows that is a secret — a new repository's password — must
// reach the person at the terminal and nothing that stdout was sent to.
func (t *tty) Say(line string) { _, _ = fmt.Fprintln(t.f, line) }

// Ask puts the prompt on the terminal and reads one line from it. A
// secret is read with echo off — off before the prompt is shown, so
// that nothing typed ahead of it is echoed after it — and echo is put
// back before the answer is returned, and before an interrupt or
// silence ends the question. An interrupt is no answer; so is silence
// for answerWithin.
func (t *tty) Ask(ctx context.Context, prompt string, secret bool) (string, error) {
	if secret {
		restore, err := t.echoOff()
		if err != nil {
			return "", err
		}
		defer restore()
		defer func() { _, _ = fmt.Fprintln(t.f) }()
	}
	_, _ = fmt.Fprint(t.f, prompt)
	type reply struct {
		line string
		err  error
	}
	said := make(chan reply, 1)
	go func() {
		line, err := t.in.ReadString('\n')
		said <- reply{line, err}
	}()
	select {
	case r := <-said:
		if r.err != nil && r.line == "" {
			return "", r.err
		}
		return strings.TrimRight(r.line, "\r\n"), nil
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(answerWithin):
		return "", fmt.Errorf("no answer in %s", answerWithin)
	}
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
