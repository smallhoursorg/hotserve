//go:build integration

package dump

import (
	"context"
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
	must(t, os.MkdirAll(filepath.Dir(db), 0o755))
	sql(t, db, "pragma journal_mode=wal; create table t(x); insert into t values (1),(2),(3);")
	// A writer holding the database open, mid-transaction-free, as an
	// app does.
	writer := exec.Command(sqlite3, db, "insert into t values (4); select 1; .system sleep 3")
	must(t, writer.Start())
	defer writer.Wait() //nolint:errcheck // only there to hold the database open

	res := single(t, shared, staging, "data/app's.db")
	if res.Class != OK || res.Bytes == 0 {
		t.Fatalf("%+v", res)
	}
	if got := strings.TrimSpace(sql(t, filepath.Join(staging, "data", "app's.db"), "select count(*) from t")); got != "3" && got != "4" {
		t.Fatalf("the copy holds %s rows", got)
	}
	for _, side := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(filepath.Join(staging, "data", "app's.db") + side); err == nil {
			t.Errorf("the copy has a %s beside it", side)
		}
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
		"sub/escape/outside.db": Failed, // a link out of the shared dir is refused, not followed
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
// socket; nothing at all. Each is an error at once, nothing is created
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
