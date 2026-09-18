package backup

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
)

// does and says are the two halves most fakes here are made of: what a
// command does (its exit status), and what one prints when somebody
// reads its stdout.
type (
	does func(ctx context.Context, name string, args ...string) error
	says func(ctx context.Context, name string, args ...string) ([]byte, error)
)

// fake is an Exec of those halves: a command whose stdout is read is
// answered by say — what it returns is written there, and its error is
// the exit status — and every other command by do. Either may be nil.
func fake(do does, say says) Exec {
	return func(ctx context.Context, c Cmd) error {
		if c.Stdout != nil && say != nil {
			out, err := say(ctx, c.Name, c.Args...)
			_, _ = c.Stdout.Write(out)
			return err
		}
		if do == nil {
			return nil
		}
		return do(ctx, c.Name, c.Args...)
	}
}

// Against a real process, not a fake: stdout is what a caller parses
// (JSON, for `restic snapshots --json`), so stderr must never be mixed
// into it — and a caller that sets Stderr gets it whatever the exit
// status, because restic exits 0 over a delete the storage refused and
// says so on stderr alone.
func TestOSExecKeepsTheStreamsApart(t *testing.T) {
	script := func(status string) Cmd {
		return Cmd{Name: "sh", Args: []string{"-c", `echo '[{"id":"x"}]'; echo 'Remove(<snapshot/x>) failed: 403 Forbidden' >&2; echo "$EXTRA" >&2; exit ` + status}, Env: []string{"EXTRA=from-env"}}
	}
	var stdout, stderr bytes.Buffer
	c := script("0")
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := osExec(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(stdout.String()); got != `[{"id":"x"}]` {
		t.Errorf("stdout must be the command's stdout alone, got %q", got)
	}
	if !strings.Contains(stderr.String(), "403 Forbidden") || !strings.Contains(stderr.String(), "from-env") {
		t.Errorf("stderr must arrive on exit 0 too, and Env must reach the command: %q", stderr.String())
	}
	// A question: both streams kept, stdout returned alone on success,
	// stderr after it on failure.
	out, err := Exec(osExec).asked(context.Background(), script("0"))
	if err != nil || strings.Contains(string(out), "403") {
		t.Errorf("a question that succeeds returns stdout alone: %q, %v", out, err)
	}
	out, err = Exec(osExec).asked(context.Background(), script("1"))
	if err == nil || !strings.Contains(string(out), "403 Forbidden") {
		t.Errorf("a question that fails returns why: %q, %v", out, err)
	}
	if _, err := Exec(osExec).output(context.Background(), Cmd{Name: "no-such-program-here"}); err == nil || !strings.Contains(err.Error(), "is not installed") {
		t.Errorf("a missing program must be named as missing, got %v", err)
	}
}

// A stream is handed over line by line as it arrives, however the
// writes fall, and a last line without a newline is not lost.
func TestLineWriter(t *testing.T) {
	var mu sync.Mutex
	var got []string
	w := &lineWriter{each: func(line string) { mu.Lock(); got = append(got, line); mu.Unlock() }}
	for _, chunk := range []string{"one\ntw", "o\n\n  three  \nfo", "ur"} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	w.flush()
	if strings.Join(got, "|") != "one|two|three|four" {
		t.Errorf("got %q", got)
	}
}
