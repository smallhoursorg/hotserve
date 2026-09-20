//go:build integration

package dump

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These need a real sqlite3 (make test-integration): what is tested is
// how sqlite3 behaves, and no stand-in behaves like it.

func dirs(t *testing.T) (shared, staging string) {
	t.Helper()
	if _, err := os.Stat(sqlite3); err != nil {
		t.Fatalf("%s is not installed in the integration image: %v", sqlite3, err)
	}
	return t.TempDir(), t.TempDir()
}

func sql(t *testing.T, db, stmt string) string {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(db), 0o755))
	out, err := exec.Command(sqlite3, db, stmt).CombinedOutput()
	if err != nil {
		t.Fatalf("sqlite3 %s: %v: %s", stmt, err, out)
	}
	return string(out)
}

func single(t *testing.T, shared, staging, rel string) Result {
	t.Helper()
	res := Databases(context.Background(), shared, staging, []string{rel})
	if len(res) != 1 || res[0].Path != rel {
		t.Fatalf("results = %+v", res)
	}
	return res[0]
}

func TestIntegrationDumpsALiveWALDatabase(t *testing.T) {
	shared, staging := dirs(t)
	db := filepath.Join(shared, "data", "app's.db") // a quote, for the SQL the path goes into
	sql(t, db, "pragma journal_mode=wal; create table t(x); insert into t values (1),(2),(3);")

	// A writer that is really there: a connection held open over stdin,
	// in the middle of a write transaction, as an app is at any moment.
	// (Given as an argument, SQL followed by a dot-command is a syntax
	// error and sqlite3 is gone in milliseconds.) It says "ready" once
	// the transaction is open, and the dump starts only then.
	writer := exec.Command(sqlite3, db)
	stdin, err := writer.StdinPipe()
	must(t, err)
	stdout, err := writer.StdoutPipe()
	must(t, err)
	must(t, writer.Start())
	_, err = fmt.Fprintln(stdin, "begin immediate; insert into t values (4); select 'ready';")
	must(t, err)
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("the writer did not get as far as its transaction: %q, %v", line, err)
	}
	if _, err := os.Stat(db + "-wal"); err != nil {
		t.Fatalf("there is no -wal beside the database, so nothing is writing it: %v", err)
	}

	res := single(t, shared, staging, "data/app's.db")
	if res.Class != OK || res.Bytes == 0 {
		t.Fatalf("%+v", res)
	}
	// The copy is of what was committed: the writer's fourth row is not.
	if got := strings.TrimSpace(sql(t, filepath.Join(staging, "data", "app's.db"), "select count(*) from t")); got != "3" {
		t.Fatalf("the copy holds %s rows, want the 3 that were committed", got)
	}
	for _, side := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(filepath.Join(staging, "data", "app's.db") + side); err == nil {
			t.Errorf("the copy has a %s beside it", side)
		}
	}

	// And the writer was there throughout: it can still commit.
	_, err = fmt.Fprintln(stdin, "commit; select count(*) from t;")
	must(t, err)
	must(t, stdin.Close())
	rest, _ := io.ReadAll(stdout)
	if err := writer.Wait(); err != nil || strings.TrimSpace(string(rest)) != "4" {
		t.Fatalf("the writer, after the dump: %q, %v", rest, err)
	}
}

func TestIntegrationASecondRunReplacesTheCopy(t *testing.T) {
	shared, staging := dirs(t)
	sql(t, filepath.Join(shared, "app.db"), "create table t(x); insert into t values (1);")
	for i := 0; i < 2; i++ {
		if res := single(t, shared, staging, "app.db"); res.Class != OK {
			t.Fatalf("run %d: %+v", i+1, res)
		}
	}
}

func TestIntegrationWhatIsNotADatabaseIsNeverGivenToSqlite3(t *testing.T) {
	shared, staging := dirs(t)
	old := beforeSqlite
	beforeSqlite = func() { t.Error("sqlite3 was about to be run") }
	defer func() { beforeSqlite = old }()

	must(t, syscall.Mkfifo(filepath.Join(shared, "fifo.db"), 0o644))
	must(t, os.WriteFile(filepath.Join(shared, "empty.db"), nil, 0o644))
	must(t, os.WriteFile(filepath.Join(shared, "text.db"), []byte("not a database, but longer than a header"), 0o644))
	must(t, os.Mkdir(filepath.Join(shared, "dir.db"), 0o755))
	sql(t, filepath.Join(shared, "real.db"), "create table t(x);")
	must(t, os.Symlink("real.db", filepath.Join(shared, "link.db")))
	outside := filepath.Join(t.TempDir(), "outside.db")
	sql(t, outside, "create table t(x);")
	must(t, os.Mkdir(filepath.Join(shared, "sub"), 0o755))
	must(t, os.Symlink(filepath.Dir(outside), filepath.Join(shared, "sub", "escape")))

	for rel, want := range map[string]Class{
		"fifo.db":               NotADatabase,
		"empty.db":              NotADatabase,
		"text.db":               NotADatabase,
		"dir.db":                NotADatabase,
		"link.db":               NotADatabase,
		"absent.db":             Missing,
		"sub/escape/outside.db": NotADatabase, // a link on the way is refused, not followed
	} {
		done := make(chan Result, 1)
		go func() { done <- single(t, shared, staging, rel) }()
		select {
		case res := <-done:
			if res.Class != want {
				t.Errorf("%s: %+v, want %s", rel, res, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: inspecting it blocked", rel)
		}
	}
}

// The app swaps its database for something else after the checks — a
// FIFO, which blocks a sqlite3 given the path directly for ever; a
// directory; a text file; nothing at all. Each is an error at once, nothing is created
// in the app's directory, and the next database is still dumped.
func TestIntegrationWhatIsSwappedInAfterTheChecksFailsAtOnce(t *testing.T) {
	for what, swap := range map[string]func(string) error{
		"a fifo":  func(p string) error { return syscall.Mkfifo(p, 0o644) },
		"a dir":   func(p string) error { return os.Mkdir(p, 0o755) },
		"nothing": func(string) error { return nil },
		"text":    func(p string) error { return os.WriteFile(p, []byte("not a database at all, any more"), 0o644) },
	} {
		t.Run(what, func(t *testing.T) {
			shared, staging := dirs(t)
			victim := filepath.Join(shared, "swapped.db")
			sql(t, victim, "create table t(x);")
			sql(t, filepath.Join(shared, "my dir%20#?", "next.db"), "create table t(x); insert into t values (1);")

			oldHook, swapped := beforeSqlite, false
			beforeSqlite = func() {
				if !swapped {
					swapped = true
					must(t, os.Remove(victim))
					must(t, swap(victim))
				}
			}
			defer func() { beforeSqlite = oldHook }()

			done := make(chan []Result, 1)
			go func() {
				done <- Databases(context.Background(), shared, staging, []string{"swapped.db", "my dir%20#?/next.db"})
			}()
			var res []Result
			select {
			case res = <-done:
			case <-time.After(15 * time.Second):
				t.Fatal("still running after 15s: a sqlite3 is waiting on what was swapped in")
			}
			if res[0].Class == OK {
				t.Fatalf("the swapped database: %+v", res[0])
			}
			if res[1].Class != OK {
				t.Fatalf("the database after it: %+v", res[1])
			}
			if _, err := os.Stat(filepath.Join(staging, "swapped.db")); err == nil {
				t.Fatal("a copy of the swapped database was left in staging")
			}
			if what == "nothing" {
				if _, err := os.Lstat(victim); err == nil {
					t.Fatal("sqlite3 created the missing database in the app's directory")
				}
			}
		})
	}
}

// What the ATTACH form does not close: the read-write open of a FIFO
// the app has made read-only is refused, SQLite retries read-only, and
// that open waits for ever. Only as an unprivileged user — root's opens
// ignore mode bits — so sqlite3 runs as hotserve here, as it does in
// the dump unit. A real sqlite3, really blocked; the bound on opening is
// what ends it, and the next database is still dumped.
func TestIntegrationAReadOnlyFifoIsKilledAtTheOpenBound(t *testing.T) {
	if exec.Command("id", "-u", "hotserve").Run() != nil {
		if out, err := exec.Command("useradd", "--system", "--no-create-home", "--shell", "/usr/sbin/nologin", "hotserve").CombinedOutput(); err != nil {
			t.Fatalf("useradd: %v: %s", err, out)
		}
	}
	var uid, gid uint32
	out, _ := exec.Command("id", "-u", "hotserve").Output()
	_, err := fmt.Sscan(string(out), &uid)
	must(t, err)
	out, _ = exec.Command("id", "-g", "hotserve").Output()
	_, err = fmt.Sscan(string(out), &gid)
	must(t, err)

	base, err := os.MkdirTemp("/var/tmp", "dump-ro-fifo-")
	must(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	shared, staging := filepath.Join(base, "shared"), filepath.Join(base, "staging")
	must(t, os.Mkdir(shared, 0o755))
	must(t, os.Mkdir(staging, 0o755))
	victim := filepath.Join(shared, "swapped.db")
	sql(t, victim, "create table t(x);")
	sql(t, filepath.Join(shared, "next.db"), "create table t(x); insert into t values (1);")
	must(t, exec.Command("chown", "-R", "hotserve:hotserve", base).Run())
	must(t, os.Chmod(base, 0o755))

	oldHook, oldWithin, oldTweak, swapped := beforeSqlite, openWithin, tweakCmd, false
	openWithin = 3 * time.Second
	tweakCmd = func(c *exec.Cmd) {
		c.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid}}
	}
	beforeSqlite = func() {
		if !swapped {
			swapped = true
			must(t, os.Remove(victim))
			must(t, syscall.Mkfifo(victim, 0o400))
			must(t, os.Chown(victim, int(uid), int(gid)))
		}
	}
	defer func() { beforeSqlite, openWithin, tweakCmd = oldHook, oldWithin, oldTweak }()

	start := time.Now()
	res := Databases(context.Background(), shared, staging, []string{"swapped.db", "next.db"})
	took := time.Since(start)
	if res[0].Class != NeverOpened {
		t.Fatalf("the read-only FIFO: %+v (if it failed at once, this sqlite3 does not retry read-only, and the bound has nothing to do)", res[0])
	}
	if took < 3*time.Second || took > 20*time.Second {
		t.Fatalf("took %s with a 3s bound", took)
	}
	if res[1].Class != OK {
		t.Fatalf("the database after it: %+v", res[1])
	}
	if out, _ := exec.Command("pgrep", "-f", "sqlite3.*swapped.db").Output(); len(out) != 0 {
		t.Fatalf("a sqlite3 is still running: %s", out)
	}
}

// Where sqlite3 waits for a locked database: before it creates the
// target, not after. That ordering is why openWithin has to exceed
// busyTimeout (TestOpenBoundExceedsTheBusyTimeout), so it is pinned
// here: if a later SQLite creates the target first, this fails and the
// comment on openWithin is out of date.
func TestIntegrationALockedDatabaseIsWaitedForBeforeTheTargetExists(t *testing.T) {
	shared, staging := dirs(t)
	db := filepath.Join(shared, "app.db")
	sql(t, db, "create table t(x); insert into t values (1);")
	holder := exec.Command(sqlite3, db)
	stdin, err := holder.StdinPipe()
	must(t, err)
	must(t, holder.Start())
	_, err = fmt.Fprintln(stdin, "begin exclusive; insert into t values (2);")
	must(t, err)
	time.Sleep(500 * time.Millisecond)

	const held = 3 * time.Second
	start := time.Now()
	appeared := make(chan time.Duration, 1)
	go func() {
		for {
			if _, err := os.Lstat(filepath.Join(staging, "app.db")); err == nil {
				appeared <- time.Since(start)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	go func() {
		time.Sleep(held)
		_, _ = fmt.Fprintln(stdin, "commit;")
		_ = stdin.Close()
	}()
	res := single(t, shared, staging, "app.db")
	_ = holder.Wait()
	if res.Class != OK {
		t.Fatalf("a database locked for %s, well inside the busy timeout: %+v", held, res)
	}
	if at := <-appeared; at < held-500*time.Millisecond {
		t.Fatalf("the target appeared after %s, while the database was still locked: this sqlite3 creates it first", at)
	}
}

// A database locked for longer than the busy timeout: sqlite3's exit
// status is SQLite's result code, 5, and that is what says "busy".
func TestIntegrationALockedDatabaseIsBusyByExitStatus(t *testing.T) {
	shared, staging := dirs(t)
	db := filepath.Join(shared, "app.db")
	sql(t, db, "create table t(x); insert into t values (1);")
	holder := exec.Command(sqlite3, db)
	stdin, err := holder.StdinPipe()
	must(t, err)
	must(t, holder.Start())
	_, err = fmt.Fprintln(stdin, "begin exclusive; insert into t values (2);")
	must(t, err)
	time.Sleep(500 * time.Millisecond)
	old := busyTimeout
	busyTimeout = time.Second
	defer func() { busyTimeout = old; _ = stdin.Close(); _ = holder.Wait() }()
	if res := single(t, shared, staging, "app.db"); res.Class != Busy {
		t.Fatalf("%+v", res)
	}
}

// HOME is where sqlite3 looks for .sqliterc, and an app's HOME is its
// shared dir.
func TestIntegrationAPlantedSqlitercIsNotRun(t *testing.T) {
	shared, staging := dirs(t)
	marker := filepath.Join(t.TempDir(), "ran")
	must(t, os.WriteFile(filepath.Join(shared, ".sqliterc"), []byte(".shell touch "+marker+"\n"), 0o644))
	sql(t, filepath.Join(shared, "app.db"), "create table t(x);")
	t.Setenv("HOME", shared)
	if res := single(t, shared, staging, "app.db"); res.Class != OK {
		t.Fatalf("%+v", res)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the .sqliterc in the shared dir was run")
	}
}

func TestIntegrationADamagedDatabaseIsNotAnOKCopy(t *testing.T) {
	shared, staging := dirs(t)
	db := filepath.Join(shared, "app.db")
	sql(t, db, "create table t(x); insert into t select hex(randomblob(2000)) from (select 1 union all select 2 union all select 3), (select 1 union all select 2 union all select 3), (select 1 union all select 2 union all select 3); create index i on t(x);")
	raw, _ := os.ReadFile(db)
	for i := 4096 + 100; i < len(raw)-100; i += 997 { // keep the header page
		raw[i] ^= 0xff
	}
	must(t, os.WriteFile(db, raw, 0o644))
	res := single(t, shared, staging, "app.db")
	if res.Class == OK {
		t.Fatalf("a corrupted database dumped as ok: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(staging, "app.db")); err == nil {
		t.Fatal("the damaged copy was left in staging")
	}
}

// The same swap, against a restore: the live database is a read-only
// FIFO by the time sqlite3 opens it. sqlite3 is killed at the bound, the
// marker never having appeared, and nothing was restored anywhere.
func TestIntegrationRestoreOverAReadOnlyFIFOIsKilledAtTheBound(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root, to run sqlite3 as another user: root ignores a FIFO's mode")
	}
	// Not t.TempDir: its parents are closed to everyone but root.
	base, err := os.MkdirTemp("/var/tmp", "restore-ro-fifo-")
	must(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	must(t, os.Chmod(base, 0o755))
	shared, staging := filepath.Join(base, "shared"), filepath.Join(base, "staging")
	must(t, os.Mkdir(shared, 0o755))
	must(t, os.Mkdir(staging, 0o755))
	copyOf := filepath.Join(staging, "copy.db")
	sql(t, copyOf, "create table t(x); insert into t values (1);")
	live := filepath.Join(shared, "app.db")
	must(t, syscall.Mkfifo(live, 0o400))
	const uid, gid = 65534, 65534
	for _, p := range []string{shared, staging, live, copyOf} {
		must(t, os.Chown(p, uid, gid))
	}
	oldWithin, oldTweak := openWithin, tweakCmd
	openWithin = 3 * time.Second
	tweakCmd = func(c *exec.Cmd) {
		c.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid}}
	}
	defer func() { openWithin, tweakCmd = oldWithin, oldTweak }()

	marker := filepath.Join(staging, "marker")
	start := time.Now()
	err = RestoreOver(context.Background(), live, copyOf, marker)
	if !errors.Is(err, ErrNeverOpened) {
		t.Fatalf("after %s: %v", time.Since(start), err)
	}
	if took := time.Since(start); took < 3*time.Second || took > 20*time.Second {
		t.Errorf("killed after %s, want the bound", took)
	}
	if _, err := os.Lstat(marker); err == nil {
		t.Error("the marker appeared although the database was never opened")
	}

	// And a restore that does open is not bounded: the marker appears,
	// and from then on it takes as long as it takes.
	must(t, os.Remove(live))
	sql(t, live, "create table t(x); insert into t values (7);")
	must(t, os.Chown(live, uid, gid))
	if err := RestoreOver(context.Background(), live, copyOf, marker); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(marker); err != nil {
		t.Errorf("a restore that worked made no marker: the bound would have killed a long one: %v", err)
	}
}

// In rollback-journal mode a reader and a writer exclude each other, so
// a restore over a database the app is reading waits, and then says the
// database stayed locked — whichever of its two steps met the lock.
func TestIntegrationRestoreOverALockedDatabaseIsBusy(t *testing.T) {
	for name, hold := range map[string]string{
		"a writer, met on the first read": "begin exclusive; select 1;",
		"a reader, met by the restore":    "begin; select * from t;",
	} {
		t.Run(name, func(t *testing.T) {
			shared, staging := dirs(t)
			live, copyOf := filepath.Join(shared, "app.db"), filepath.Join(staging, "copy.db")
			sql(t, live, "create table t(x); insert into t values (7);")
			sql(t, copyOf, "create table t(x); insert into t values (1);")
			holder := exec.Command(sqlite3, live, hold, ".shell sleep 4", "commit;")
			must(t, holder.Start())
			time.Sleep(500 * time.Millisecond)
			old := busyTimeout
			busyTimeout = time.Second
			defer func() { busyTimeout = old }()
			if err := RestoreOver(context.Background(), live, copyOf, filepath.Join(staging, "marker")); !errors.Is(err, ErrBusy) {
				t.Fatalf("%v", err)
			}
			must(t, holder.Wait())
			if got := sql(t, live, "select x from t"); got != "7\n" {
				t.Fatalf("a restore that said busy changed the database: %q", got)
			}
		})
	}
}
