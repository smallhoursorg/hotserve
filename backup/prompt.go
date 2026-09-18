package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// Prompter asks the operator for one value at the terminal. A secret is
// read without echo, so it never appears on screen — and, being typed
// at a prompt rather than on the command line, it is in no shell
// history and in no process's arguments (/proc/*/cmdline is readable by
// every user on the box).
type Prompter func(label string, secret bool) (string, error)

// terminalPrompter asks on in when it is a terminal, and is nil when it
// is not: a script, a pipe or a CI job gets no questions, and keeps
// --credentials-file and --password-file for the same answers.
//
// ctx is the command's interrupt: the commands catch SIGINT so that
// their cleanups run, which would otherwise leave Ctrl-C at a prompt
// doing nothing until Enter — and then the next step failing with an
// error that blames the answer. Cancelled, the question ends at once,
// with the terminal's echo put back.
func terminalPrompter(ctx context.Context, in *os.File, out io.Writer) Prompter {
	fd := int(in.Fd())
	if !term.IsTerminal(fd) {
		return nil
	}
	return func(label string, secret bool) (string, error) {
		_, _ = fmt.Fprintf(out, "%s: ", label)
		saved, err := term.GetState(fd)
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", strings.ToLower(label), err)
		}
		type result struct {
			s   string
			err error
		}
		done := make(chan result, 1)
		go func() {
			if secret {
				b, err := term.ReadPassword(fd)
				done <- result{string(b), err}
				return
			}
			line, err := readLine(in)
			done <- result{line, err}
		}()
		var r result
		select {
		case r = <-done:
		case <-ctx.Done():
			// The read is abandoned; the command is ending.
			_ = term.Restore(fd, saved)
			_, _ = fmt.Fprintln(out)
			return "", errors.New("interrupted")
		}
		if secret {
			_, _ = fmt.Fprintln(out) // the Enter that ReadPassword did not echo
		}
		if r.err != nil {
			return "", fmt.Errorf("reading %s: %w", strings.ToLower(label), r.err)
		}
		// A secret is taken as typed — a password may end in a space —
		// less the line ending; anything else is trimmed.
		answer := strings.TrimSpace(r.s)
		if secret {
			answer = strings.TrimRight(r.s, "\r\n")
		}
		if answer == "" {
			return "", fmt.Errorf("no %s entered", strings.ToLower(label))
		}
		return answer, nil
	}
}

// readLine reads one line a byte at a time. A buffered reader would read
// ahead past the newline, and the bytes it swallowed would never reach
// the next question — which reads the terminal directly, to hide what
// is typed.
func readLine(f *os.File) (string, error) {
	var b strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := f.Read(buf)
		if n == 1 {
			if buf[0] == '\n' {
				return strings.TrimRight(b.String(), "\r"), nil
			}
			b.WriteByte(buf[0])
		}
		if errors.Is(err, io.EOF) {
			return b.String(), nil
		}
		if err != nil {
			return "", err
		}
	}
}

// credential is one provider setting init can ask for.
type credential struct {
	key    string
	label  string
	secret bool
}

// missingCredentials is what init asks for, for this repository, when
// it was not given. Only the providers the docs set up are asked about:
// an S3-compatible store (Backblaze B2's S3 endpoint among them) and
// B2's own API. Others need files rather than typed secrets, and keep
// --credentials-file.
func missingCredentials(repository string, given []string) []credential {
	var want []credential
	switch {
	case strings.HasPrefix(repository, "s3:"):
		want = []credential{
			{key: "AWS_ACCESS_KEY_ID", label: "Access key ID"},
			{key: "AWS_SECRET_ACCESS_KEY", label: "Secret access key (not shown)", secret: true},
		}
	case strings.HasPrefix(repository, "b2:"):
		want = []credential{
			{key: "B2_ACCOUNT_ID", label: "B2 application key ID"},
			{key: "B2_ACCOUNT_KEY", label: "B2 application key (not shown)", secret: true},
		}
	}
	var missing []credential
	for _, c := range want {
		have := false
		for _, kv := range given {
			if strings.HasPrefix(kv, c.key+"=") {
				have = true
			}
		}
		if !have {
			missing = append(missing, c)
		}
	}
	return missing
}
