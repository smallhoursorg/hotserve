package liveswap

import (
	"context"
	"encoding/json"
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
// external program hotserve executes. The argument lists are built
// here, of unit names, pids the manager recorded, stream ids journald
// printed, a count and a timestamp; the output is bounded
// (deployLogMaxBytes, and the line count the operator sets with
// deploy_log_lines); a journalctl that is missing or fails leaves the
// tail out and says why.
//
// Everything read here passes the response filter (redact.go) like
// any other body, and deploy_log_lines 0 keeps app output on the box
// entirely: no journal tail, and no health probe body either
// (failureDetail, app.go) — the exit status and the probe's status
// code are hotserve's observations, not the app's bytes, and stay.

// journalReader is the seam: the real reader runs journalctl, tests
// script one.
type journalReader interface {
	// tail returns up to n of the most recent lines a launch's units
	// wrote since a point in time, oldest first.
	tail(ctx context.Context, of tailOf, since time.Time, n int) ([]string, error)
}

// tailOf is whose lines a tail reads: a launch's units; the pids
// their main processes had, as the manager recorded them, where one
// ended; and the identifier every unit of the app writes under.
type tailOf struct {
	units []string
	pids  []int
	ident string
}

// journalctlPath is where Debian's systemd package puts journalctl —
// an absolute path, so what runs never depends on PATH.
const journalctlPath = "/usr/bin/journalctl"

// journalctlReader reads the hotserve user's own journal, where the
// units on its user manager write.
//
// journald names a line's unit from /proc/<pid> of the process that
// wrote it, when it gets to the line, and a line whose writer was
// reaped first is stored with no unit field at all: a child that
// prints its error and exits, every time; any line, while journald is
// behind. What every line a unit writes to stdout or stderr does
// carry, its children's included, is the unit's one stream: journald
// gives the stream its id, a client cannot set it, and only the
// unit's own processes hold the stream. So the tail is read in two
// runs: the streams first, from the lines that kept their unit and
// from those the unit's main process wrote — by its pid, as the
// manager recorded it, under the app's identifier — and then every
// line of those streams, with any line journald attributed to the
// units otherwise (one an app sent the journal directly).
type journalctlReader struct{}

func (journalctlReader) tail(ctx context.Context, of tailOf, since time.Time, n int) ([]string, error) {
	if n <= 0 || len(of.units) == 0 {
		return nil, nil
	}
	found, err := runJournalctl(ctx, streamArgs(of, since, n), streamsMaxBytes)
	if err != nil {
		return nil, err
	}
	// The line count bounds the entries, not their size: a single entry
	// can be as long as journald lets a line be. The output streams
	// through a writer that keeps only the last bytes, more than the
	// response cap so that capTail still sees "there was more" (a
	// dropped prefix leaves a partial first line for it to cut), and
	// memory is fixed whatever the app wrote.
	out, err := runJournalctl(ctx, tailArgs(of.units, streamIDs(found), since, n), 2*deployLogMaxBytes)
	if err != nil {
		return nil, err
	}
	text := strings.TrimRight(string(out), "\n")
	if text == "" {
		return nil, nil
	}
	return strings.Split(text, "\n"), nil
}

// journalWindow is what both runs share. A second of slack: the
// deploy's clock and the journal's need not agree to the millisecond,
// and a line a moment before the start is harmless where a line lost
// is not. One line more than asked, so the caller can tell "all of
// it" from "there was more" (capTail sets the flag). The streams are
// learnt in the same window: a launch's units run one after the
// other, so a stream with none of its named lines among the last n+1
// has none of its lines among the tail's last n+1 either.
func journalWindow(since time.Time, n int) []string {
	return []string{"--user", "--no-pager", "--quiet", "-n", strconv.Itoa(n + 1),
		"--since", "@" + strconv.FormatInt(since.Add(-time.Second).Unix(), 10)}
}

// streamArgs asks for the stream ids of the lines that name the
// launch: the unit field, matched directly rather than through -u
// (which also selects the manager's own lines about the unit,
// "Starting …", "Main process exited, code=exited, status=2" — what
// `exit` already says); or a main process's pid together with the
// app's identifier. Several matches of one field are OR'd, different
// fields AND'd, and "+" ORs the two groups.
func streamArgs(of tailOf, since time.Time, n int) []string {
	args := append(journalWindow(since, n), "-o", "json", "--output-fields=_STREAM_ID")
	args = append(args, unitMatches(of.units)...)
	var pids []string
	for _, p := range of.pids {
		if p > 0 {
			pids = append(pids, "_PID="+strconv.Itoa(p))
		}
	}
	if len(pids) > 0 && of.ident != "" {
		args = append(append(append(args, "+"), pids...), "SYSLOG_IDENTIFIER="+of.ident)
	}
	return args
}

// tailArgs asks for the lines themselves: the units', and every line
// of their streams.
func tailArgs(units, streams []string, since time.Time, n int) []string {
	args := append(journalWindow(since, n), "-o", "cat")
	args = append(args, unitMatches(units)...)
	if len(streams) > 0 {
		args = append(args, "+")
		for _, s := range streams {
			args = append(args, "_STREAM_ID="+s)
		}
	}
	return args
}

func unitMatches(units []string) []string {
	m := make([]string, 0, len(units))
	for _, u := range units {
		m = append(m, "_SYSTEMD_USER_UNIT="+u)
	}
	return m
}

// streamsMaxBytes bounds the first run's output: an entry with its
// one field asked for is some 420 bytes of JSON, and deploy_log_lines
// is at most 1000.
const streamsMaxBytes = 1 << 20

// streamIDs reads journalctl's JSON entries for their distinct
// stream ids, in the order first met. An entry it cannot read — the
// first, cut by the byte bound — or an id not shaped as journald
// prints one is passed over: it becomes an argument of the next run.
func streamIDs(out []byte) []string {
	var ids []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		var e struct {
			ID string `json:"_STREAM_ID"`
		}
		if json.Unmarshal([]byte(line), &e) != nil || !isID128(e.ID) || seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		ids = append(ids, e.ID)
	}
	return ids
}

// isID128 reports an id as journald prints one: 32 lowercase hex
// digits.
func isID128(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// runJournalctl runs journalctl, keeping the last limit bytes of what
// it prints.
func runJournalctl(ctx context.Context, args []string, limit int) ([]byte, error) {
	out := &tailWriter{max: limit}
	stderr := &tailWriter{max: 1024}
	cmd := exec.CommandContext(ctx, journalctlPath, args...) //nolint:gosec // a fixed program by absolute path; the arguments are validated unit names, pids the manager recorded, stream ids of journald's shape, a count and a timestamp built here, never request input
	cmd.Stdout, cmd.Stderr = out, stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(string(stderr.buf)); msg != "" {
			return nil, fmt.Errorf("journalctl: %s", msg)
		}
		return nil, fmt.Errorf("journalctl: %w", err)
	}
	return out.buf, nil
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
