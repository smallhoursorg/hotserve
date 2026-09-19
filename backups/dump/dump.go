// Package dump takes a consistent copy of each SQLite database an app
// declares, into a staging directory, and checks the copy.
//
// It is what runs inside the dump unit: as the hotserve uid, in the
// app's own kind of sandbox, with no network and no credential,
// because the bytes it hands to sqlite3 are the app's to choose. It
// trusts nothing about them: not that a declared path is a file, not
// that it stays one, not that the shared dir holds no .sqliterc. The
// one step that can be made to wait on something that is not a file —
// the open — is the one step with a bound (openWithin).
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

// openWithin is how long sqlite3 has to open the database, which is not
// how long it has to copy it. The copy's target file appears some ten
// milliseconds after sqlite3 starts, whatever the database's size, and
// never appears while the open is blocked [measured] — and an open can
// still be blocked: the ATTACH form in run fails at once on most things
// that are not a database file, but when a read-write open is refused
// SQLite retries read-only, so a FIFO the app has made mode 0400 waits
// for ever. Left alone that would end every app's backups on the box,
// not only this app's. It has to exceed busyTimeout: sqlite3 waits for
// a locked database before it creates the target, not after [measured],
// so until busyTimeout has passed "no target yet" may be an honest
// wait. The rest of five minutes is for a very large WAL being
// recovered on open. Once the target exists, nothing bounds the copy.
// A variable so a test need not wait for it.
var openWithin = 5 * time.Minute

// tweakCmd lets a test run sqlite3 as an unprivileged user: root's
// opens ignore the mode bits the case above turns on.
var tweakCmd = func(*exec.Cmd) {}

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
	NeverOpened   Class = "never opened"
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
	if res := inspect(shared, rel); res.Class != OK {
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

	source := filepath.Join(shared, rel)
	if _, stderr, err := run(ctx, source, "VACUUM src INTO "+quote(target), target); err != nil {
		_ = os.Remove(target) // a partial copy, if there is one; the clean unit empties staging whatever happens here
		if errors.Is(err, errNeverOpened) {
			return Result{Class: NeverOpened, Detail: fmt.Sprintf("sqlite3 had not opened the database after %s and was killed: the path stopped being a file it could open", openWithin)}
		}
		return classify(stderr)
	}
	stdout, stderr, err := run(ctx, target, "PRAGMA src.integrity_check", "")
	// A damaged copy is reported in two ways [measured]: what is wrong
	// on stdout with exit 0, or "malformed" on stderr with exit 1. Only
	// exactly "ok" and exit 0 is a good copy.
	if err != nil || stdout != "ok\n" {
		_ = os.Remove(target) // as above
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
func inspect(shared, rel string) Result {
	root, err := os.OpenRoot(shared)
	if err != nil {
		return Result{Class: Failed, Detail: err.Error()}
	}
	defer root.Close() //nolint:errcheck // read-only
	// A declared path names the database file itself. os.Root follows a
	// link that stays inside the shared dir (and refuses one that does
	// not), so a link is turned away here, by name: the copy, and any
	// restore of it, would otherwise sit at a path that is not where
	// the data is.
	if st, err := root.Lstat(rel); err == nil && st.Mode()&fs.ModeSymlink != 0 {
		return Result{Class: NotADatabase, Detail: "a symbolic link; declare the file it points to"}
	}
	// O_NONBLOCK: opening a FIFO for reading otherwise waits for a
	// writer.
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Result{Class: Missing, Detail: "no such file under the shared dir"}
	case err != nil:
		return Result{Class: Failed, Detail: err.Error()}
	}
	defer f.Close() //nolint:errcheck // read-only
	st, err := f.Stat()
	if err != nil {
		return Result{Class: Failed, Detail: err.Error()}
	}
	if !st.Mode().IsRegular() {
		return Result{Class: NotADatabase, Detail: "not a regular file: " + st.Mode().Type().String()}
	}
	buf := make([]byte, len(header))
	if _, err := io.ReadFull(f, buf); err != nil || string(buf) != header {
		// An empty file too: sqlite3 opens one as a valid, empty
		// database, and a backup of that is a backup of nothing.
		return Result{Class: NotADatabase, Detail: fmt.Sprintf("no SQLite header (%d bytes)", st.Size())}
	}
	return Result{Class: OK}
}

// run is one sqlite3 over the database at db, which sql names src.
//
// The shell is started on an in-memory database and ATTACHes the real
// one, by a URI that says mode=rw. Given the path directly, the shell
// opens it read-only itself, to look at its type, before SQLite does —
// and that open blocks for ever on a FIFO, which the app can swap in
// after inspect has looked [measured: every flag the shell has]. By
// ATTACH the file is opened once, read-write, which does not block on a
// FIFO and fails on its first read: a FIFO, a socket or a directory is
// an error within milliseconds. (Not every FIFO: see openWithin.)
// mode=rw also means "do not create": a
// plain ATTACH of a missing path makes an empty file in the app's
// directory and copies that.
//
// appears, when not empty, is a file the statement creates as soon as
// it has the database open; if it is not there within openWithin,
// sqlite3 is killed — this process's own child — and the error is
// errNeverOpened.
func run(ctx context.Context, db, sql, appears string) (stdout, stderr string, err error) {
	// -init /dev/null: sqlite3 otherwise reads ~/.sqliterc, and HOME may
	// be a directory the app writes.
	//nolint:gosec // the program is a constant path; db is a declared path under the shared dir, which is the point
	cmd := exec.CommandContext(ctx, sqlite3, "-batch", "-bail", "-init", "/dev/null",
		"-cmd", fmt.Sprintf(".timeout %d", busyTimeout.Milliseconds()),
		":memory:", "ATTACH "+quote(uri(db))+" AS src; "+sql)
	cmd.Env = []string{"HOME=/nonexistent", "LC_ALL=C"}
	cmd.WaitDelay = 5 * time.Second
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	tweakCmd(cmd)
	if appears == "" {
		err = cmd.Run()
		return o.String(), e.String(), err
	}
	if err := cmd.Start(); err != nil {
		return "", "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.NewTimer(openWithin)
	defer deadline.Stop()
	poll := time.NewTicker(100 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case err := <-done:
			return o.String(), e.String(), err
		case <-poll.C:
			if _, err := os.Lstat(appears); err == nil {
				// Open, and copying: from here it takes as long as it takes.
				err := <-done
				return o.String(), e.String(), err
			}
		case <-deadline.C:
			if _, err := os.Lstat(appears); err == nil {
				err := <-done
				return o.String(), e.String(), err
			}
			_ = cmd.Process.Kill() // blocked in open(2), which a signal interrupts
			<-done
			return o.String(), e.String(), errNeverOpened
		}
	}
}

var errNeverOpened = errors.New("sqlite3 never opened the database")

// quote is a SQL string literal.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// uri is an absolute path as a SQLite URI that opens it read-write and
// never creates it. Everything but the unreserved characters and the
// separator is escaped: ?, # and % mean something in a URI.
func uri(abs string) string {
	var b strings.Builder
	b.WriteString("file:")
	for i := 0; i < len(abs); i++ {
		c := abs[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', strings.IndexByte("-._~/", c) >= 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	b.WriteString("?mode=rw")
	return b.String()
}

func classify(stderr string) Result {
	low := strings.ToLower(stderr)
	switch {
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
