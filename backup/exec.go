package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Cmd is one external command: what runs, what is added to its
// environment, and where its two streams go. Everything about a command
// is in it, so a test sees exactly what the real thing would get —
// nothing reaches a command any other way.
type Cmd struct {
	Name string
	Args []string
	// Env is added to this process's own environment. The repository
	// settings travel here: they are never put into this process's
	// environment, where everything it later starts would inherit them.
	Env []string
	// Stdout and Stderr default to this process's stderr — the journal
	// when the timer started it, the terminal when a person did: restic
	// and sqlite3 explain their own failures better than a wrapper can.
	// A caller that reads a stream sets it, and the other is unaffected:
	// stdout is often parsed (JSON), and stderr must never be mixed in.
	Stdout io.Writer
	Stderr io.Writer
}

// Exec runs one command. Everything this package starts — restic,
// sqlite3, systemd-run — goes through one.
type Exec func(ctx context.Context, c Cmd) error

// restic is the command most of this package runs.
func restic(args ...string) Cmd { return Cmd{Name: "restic", Args: args} }

// output runs c and returns what it wrote to stdout, whatever its exit
// status: sqlite3's integrity check says what is wrong on stdout and
// then exits 1.
func (x Exec) output(ctx context.Context, c Cmd) ([]byte, error) {
	var out bytes.Buffer
	c.Stdout = &out
	err := x(ctx, c)
	return out.Bytes(), err
}

// asked runs a command whose failure is an answer, not a fault — "is
// there a repository here already?" — so neither stream is shown. It
// returns stdout, which is what a caller parses; and when the command
// fails, stderr after it, which is where restic says why.
func (x Exec) asked(ctx context.Context, c Cmd) ([]byte, error) {
	var out, errOut bytes.Buffer
	c.Stdout, c.Stderr = &out, &errOut
	if err := x(ctx, c); err != nil {
		return append(out.Bytes(), errOut.Bytes()...), err
	}
	return out.Bytes(), nil
}

// withEnv adds env to every command x runs.
func (x Exec) withEnv(env []string) Exec {
	return func(ctx context.Context, c Cmd) error {
		c.Env = append(append([]string{}, c.Env...), env...)
		return x(ctx, c)
	}
}

// osExec is the real Exec.
func osExec(ctx context.Context, c Cmd) error {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...) //nolint:gosec // restic, sqlite3, systemd-run and systemctl, with an argv built here from the running config, never from a request
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if c.Stdout != nil {
		cmd.Stdout = c.Stdout
	}
	if c.Stderr != nil {
		cmd.Stderr = c.Stderr
	}
	if err := cmd.Run(); err != nil {
		if _, lookErr := exec.LookPath(c.Name); lookErr != nil {
			return fmt.Errorf("%s is not installed: %w", c.Name, lookErr)
		}
		return err
	}
	return nil
}

// resticMessage is one line of what restic writes with --json: progress
// and the summary on stdout, errors and the exit on stderr.
type resticMessage struct {
	Type  string `json:"message_type"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
	During string `json:"during"`
	Item   string `json:"item"`
	// Message is the text of an exit_error.
	Message string `json:"message"`
	// SnapshotID is in a backup's summary.
	SnapshotID     string  `json:"snapshot_id"`
	FilesNew       int     `json:"files_new"`
	FilesChanged   int     `json:"files_changed"`
	DataAdded      uint64  `json:"data_added_packed"`
	TotalDuration  float64 `json:"total_duration"`
	BytesProcessed uint64  `json:"total_bytes_processed"`
}

// parseResticLine reads one line of restic's --json output. A line that
// is not one of its messages — a warning restic prints plainly whatever
// the mode — comes back with ok false.
func parseResticLine(line string) (m resticMessage, ok bool) {
	if err := json.Unmarshal([]byte(line), &m); err != nil || m.Type == "" {
		return resticMessage{}, false
	}
	return m, true
}

// resticSays is a line of restic's --json stderr as a person reads it:
// the error's own words, not the JSON around them.
func resticSays(line string) string {
	m, ok := parseResticLine(line)
	switch {
	case ok && m.Type == "error" && m.Item != "":
		return m.Error.Message + " (" + m.Item + ")"
	case ok && m.Type == "error":
		return m.Error.Message
	case ok && m.Type == "exit_error":
		return strings.TrimSpace(m.Message)
	}
	return line
}

// lineWriter hands each line written to it to each, as it is completed:
// a command's stream is read while the command runs, and a line per file
// is never held as a whole output. Set as a Cmd's Stdout or Stderr; call
// flush once the command has ended, for a last line without a newline.
//
// Two of them on one command are written from two goroutines (os/exec
// copies each stream on its own), so what their each funcs share is
// theirs to guard.
type lineWriter struct {
	each func(line string)
	buf  []byte
}

// A line longer than this is handed over in pieces rather than held.
const maxLine = 1 << 20

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 && len(w.buf) < maxLine {
			return len(p), nil
		}
		if i < 0 {
			i = len(w.buf) - 1
		}
		if line := strings.TrimSpace(string(w.buf[:i+1])); line != "" {
			w.each(line)
		}
		w.buf = w.buf[i+1:]
	}
}

func (w *lineWriter) flush() {
	if line := strings.TrimSpace(string(w.buf)); line != "" {
		w.each(line)
	}
	w.buf = nil
}
