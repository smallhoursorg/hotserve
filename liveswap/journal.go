package liveswap

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The app's own output goes to the journal (StandardOutput=journal on
// the unit, see systemd_dbus.go), and a failed deploy's response
// carries the last lines of it: what a migration printed before it
// exited 3, the stack trace of an app that died on start. Reading the
// journal back means running journalctl — the product binary is built
// without cgo, so the sd-journal library is out — which is the one
// external program hotserve executes. The argument list is fixed and
// the output is bounded (deployLogMaxBytes, and the line count the
// operator sets with deploy_log_lines); a journalctl that is missing
// or fails leaves the tail out and says why.
//
// Everything read here passes the response filter (redact.go) like
// any other body, and deploy_log_lines 0 keeps app output on the box
// entirely: no journal tail, and no health probe body either
// (failureDetail, app.go) — the exit status and the probe's status
// code are hotserve's observations, not the app's bytes, and stay.

// journalReader is the seam: the real reader runs journalctl, tests
// script one.
type journalReader interface {
	// tail returns up to n of the most recent lines the named units
	// wrote since a point in time, oldest first.
	tail(ctx context.Context, units []string, since time.Time, n int) ([]string, error)
}

// journalctlPath is where Debian's systemd package puts journalctl —
// an absolute path, so what runs never depends on PATH.
const journalctlPath = "/usr/bin/journalctl"

// journalctlReader reads the hotserve user's own journal, where the
// units on its user manager write.
type journalctlReader struct{}

func (journalctlReader) tail(ctx context.Context, units []string, since time.Time, n int) ([]string, error) {
	if n <= 0 || len(units) == 0 {
		return nil, nil
	}
	// A second of slack: the deploy's clock and the journal's need not
	// agree to the millisecond, and a line a moment before the start is
	// harmless where a line lost is not.
	// The unit field, matched directly rather than through -u: -u also
	// selects the manager's own lines about the unit ("Starting …",
	// "Main process exited, code=exited, status=2"), which say what
	// `exit` already says. Several matches of one field are OR'd. One
	// line more than asked, so the caller can tell "all of it" from
	// "there was more" (capTail sets the flag).
	args := []string{"--user", "--no-pager", "--quiet", "-o", "cat",
		"-n", strconv.Itoa(n + 1), "--since", "@" + strconv.FormatInt(since.Add(-time.Second).Unix(), 10)}
	for _, u := range units {
		args = append(args, "_SYSTEMD_USER_UNIT="+u)
	}
	// The line count bounds the entries, not their size: a single entry
	// can be as long as journald lets a line be. The output streams
	// through a writer that keeps only the last bytes, more than the
	// response cap so that capTail still sees "there was more" (a
	// dropped prefix leaves a partial first line for it to cut), and
	// memory is fixed whatever the app wrote.
	out := &tailWriter{max: 2 * deployLogMaxBytes}
	stderr := &tailWriter{max: 1024}
	cmd := exec.CommandContext(ctx, journalctlPath, args...) //nolint:gosec // a fixed program by absolute path; the arguments are validated unit names, a count and a timestamp built here, never request input
	cmd.Stdout, cmd.Stderr = out, stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(string(stderr.buf)); msg != "" {
			return nil, fmt.Errorf("journalctl: %s", msg)
		}
		return nil, fmt.Errorf("journalctl: %w", err)
	}
	text := strings.TrimRight(string(out.buf), "\n")
	if text == "" {
		return nil, nil
	}
	return strings.Split(text, "\n"), nil
}

// tailWriter keeps the last max bytes written to it.
type tailWriter struct {
	max int
	buf []byte
}

func (w *tailWriter) Write(p []byte) (int, error) {
	if len(p) >= w.max {
		w.buf = append(w.buf[:0], p[len(p)-w.max:]...)
		return len(p), nil
	}
	if over := len(w.buf) + len(p) - w.max; over > 0 {
		w.buf = append(w.buf[:0], w.buf[over:]...)
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

// deployLogMaxBytes bounds the tail in bytes whatever deploy_log_lines
// says: a response is read in a CI log, not a log viewer.
const deployLogMaxBytes = 8 * 1024

// journalTailTimeout bounds the journalctl run: a failed deploy's
// response must not hang on the journal.
const journalTailTimeout = 5 * time.Second

// capTail keeps the last lines that fit both caps, and reports whether
// anything was dropped. A last line that alone exceeds the byte cap —
// a one-line JSON stack trace — is kept as its tail with a marker,
// not dropped whole.
func capTail(lines []string, maxLines, maxBytes int) ([]string, bool) {
	truncated := false
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
		truncated = true
	}
	size := 0
	for i := len(lines) - 1; i >= 0; i-- {
		size += len(lines[i]) + 1
		if size > maxBytes {
			if i == len(lines)-1 {
				const marker = "…"
				keep := maxBytes - len(marker) - 1
				if keep < 0 {
					keep = 0
				}
				// Cut on a rune boundary: a UTF-8 sequence split at
				// its start would reach the response as U+FFFD.
				start := len(lines[i]) - keep
				for start < len(lines[i]) && !utf8.RuneStart(lines[i][start]) {
					start++
				}
				lines = []string{marker + lines[i][start:]}
			} else {
				lines = lines[i+1:]
			}
			truncated = true
			break
		}
	}
	if len(lines) == 0 {
		return nil, truncated
	}
	return lines, truncated
}
