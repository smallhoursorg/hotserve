// Package dump takes a consistent copy of each SQLite database an app
// declares, into a staging directory, and checks the copy.
//
// It is what runs inside the dump unit: as the hotserve uid, in the
// app's own kind of sandbox, with no network and no credential,
// because the bytes it hands to sqlite3 are the app's to choose. It
// trusts nothing about them: not that a declared path is a file, not
// that it stays one, not that the shared dir holds no .sqliterc.
package dump

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// sqlite3 is Debian's, by absolute path.
var sqlite3 = "/usr/bin/sqlite3"

// busyTimeout is how long sqlite3 waits on a locked database; its own
// default is not to wait at all.
const busyTimeout = 30 * time.Second

// boundFor is how long one sqlite3 may run against a database of this
// size: a minute, and a second for every megabyte. VACUUM INTO copies
// some two hundred megabytes a second, so this is a bound no real copy
// comes near — and it is not there for a slow copy. sqlite3 opens the
// path it is given, the app owns the directory, and a FIFO swapped in
// after the checks below blocks that open for ever; without a bound
// one app would hold up every other app's backup on the box. A
// variable so a test can wait seconds for it, not minutes.
var boundFor = func(size int64) time.Duration {
	return time.Minute + time.Duration(size/(1<<20))*time.Second
}

// beforeSqlite runs between the checks and the first sqlite3; a test
// uses it to be the app that swaps the file.
var beforeSqlite = func() {}

// Class says how one database's dump ended, in words a status report
// can use.
type Class string

const (
	OK            Class = "ok"
	Missing       Class = "missing"
	NotADatabase  Class = "not a database"
	DidNotFinish  Class = "did not finish"
	DiskFull      Class = "disk full"
	Busy          Class = "busy"
	CopyIsDamaged Class = "copy is damaged"
	Failed        Class = "failed"
)

// Result is one declared database.
type Result struct {
	Path   string `json:"path"`
	Class  Class  `json:"class"`
	Detail string `json:"detail,omitempty"`
	Bytes  int64  `json:"bytes,omitempty"`
}

const header = "SQLite format 3\x00"

// Databases dumps every path in turn. One failing does not stop the
// rest: each is reported on its own.
func Databases(ctx context.Context, shared, staging string, paths []string) []Result {
	out := make([]Result, 0, len(paths))
	for _, p := range paths {
		r := one(ctx, shared, staging, p)
		r.Path = p
		out = append(out, r)
	}
	return out
}

func one(ctx context.Context, shared, staging, rel string) Result {
	size, res := inspect(shared, rel)
	if res.Class != OK {
		return res
	}
	target := filepath.Join(staging, rel)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return Result{Class: Failed, Detail: err.Error()}
	}
	// VACUUM INTO refuses a target that exists; staging is this
	// package's own, so whatever is there is a leftover.
	for _, leftover := range []string{target, target + "-wal", target + "-shm", target + "-journal"} {
		if err := os.Remove(leftover); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return Result{Class: Failed, Detail: err.Error()}
		}
	}
	beforeSqlite()

	bound := boundFor(size)
	source := filepath.Join(shared, rel)
	if _, stderr, err := run(ctx, bound, source, "VACUUM INTO '"+strings.ReplaceAll(target, "'", "''")+"'"); err != nil {
		_ = os.Remove(target) // a partial copy, if there is one; the clean unit empties staging whatever happens here
		return classify(err, stderr, bound)
	}
	stdout, stderr, err := run(ctx, bound, target, "PRAGMA integrity_check")
	// A damaged copy is reported in two ways [measured]: what is wrong
	// on stdout with exit 0, or "malformed" on stderr with exit 1. Only
	// exactly "ok" and exit 0 is a good copy.
	if err != nil || stdout != "ok\n" {
		_ = os.Remove(target) // as above
		if r := classify(err, stderr, bound); err != nil && r.Class == DidNotFinish {
			return r
		}
		return Result{Class: CopyIsDamaged, Detail: firstLine(stdout + stderr)}
	}
	_ = os.Remove(target + "-wal") // absent after a clean close, which is the usual case
	_ = os.Remove(target + "-shm") // as above
	st, err := os.Stat(target)
	if err != nil {
		return Result{Class: Failed, Detail: err.Error()}
	}
	return Result{Class: OK, Bytes: st.Size()}
}

// inspect decides whether rel is a SQLite database file, without ever
// blocking on it and without following a link out of the shared dir.
func inspect(shared, rel string) (int64, Result) {
	root, err := os.OpenRoot(shared)
	if err != nil {
		return 0, Result{Class: Failed, Detail: err.Error()}
	}
	defer root.Close() //nolint:errcheck // read-only
	// A declared path names the database file itself. os.Root follows a
	// link that stays inside the shared dir (and refuses one that does
	// not), so a link is turned away here, by name: the copy, and any
	// restore of it, would otherwise sit at a path that is not where
	// the data is.
	if st, err := root.Lstat(rel); err == nil && st.Mode()&fs.ModeSymlink != 0 {
		return 0, Result{Class: NotADatabase, Detail: "a symbolic link; declare the file it points to"}
	}
	// O_NONBLOCK: opening a FIFO for reading otherwise waits for a
	// writer.
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return 0, Result{Class: Missing, Detail: "no such file under the shared dir"}
	case err != nil:
		return 0, Result{Class: Failed, Detail: err.Error()}
	}
	defer f.Close() //nolint:errcheck // read-only
	st, err := f.Stat()
	if err != nil {
		return 0, Result{Class: Failed, Detail: err.Error()}
	}
	if !st.Mode().IsRegular() {
		return 0, Result{Class: NotADatabase, Detail: "not a regular file: " + st.Mode().Type().String()}
	}
	buf := make([]byte, len(header))
	if _, err := io.ReadFull(f, buf); err != nil || string(buf) != header {
		// An empty file too: sqlite3 opens one as a valid, empty
		// database, and a backup of that is a backup of nothing.
		return 0, Result{Class: NotADatabase, Detail: fmt.Sprintf("no SQLite header (%d bytes)", st.Size())}
	}
	return st.Size(), Result{Class: OK}
}

// run is one sqlite3, killed when bound passes. The kill is of this
// process's own child, and Wait returns once it is dead.
func run(ctx context.Context, bound time.Duration, db, sql string) (stdout, stderr string, err error) {
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	// -init /dev/null: sqlite3 otherwise reads ~/.sqliterc, and HOME may
	// be a directory the app writes. An absolute db path is never an
	// option and never a URI.
	//nolint:gosec // the program is a constant path; db is a declared path under the shared dir, which is the point
	cmd := exec.CommandContext(ctx, sqlite3, "-batch", "-bail", "-init", "/dev/null",
		"-cmd", fmt.Sprintf(".timeout %d", busyTimeout.Milliseconds()), db, sql)
	cmd.Env = []string{"HOME=/nonexistent", "LC_ALL=C"}
	cmd.WaitDelay = 5 * time.Second
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err = cmd.Run()
	if ctx.Err() != nil && err != nil {
		err = fmt.Errorf("%w: %w", context.DeadlineExceeded, err)
	}
	return o.String(), e.String(), err
}

func classify(err error, stderr string, bound time.Duration) Result {
	low := strings.ToLower(stderr)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return Result{Class: DidNotFinish, Detail: fmt.Sprintf("sqlite3 was killed after %s: the file stopped being a database it could open, or never finished copying", bound)}
	case strings.Contains(low, "disk is full") || strings.Contains(low, "disk full"):
		return Result{Class: DiskFull, Detail: "the copy needs as much free space as the database is large"}
	case strings.Contains(low, "database is locked") || strings.Contains(low, "busy"):
		return Result{Class: Busy, Detail: fmt.Sprintf("still locked after %s", busyTimeout)}
	}
	return Result{Class: Failed, Detail: firstLine(stderr)}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}
