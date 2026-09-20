//go:build integration

package restore

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Against Debian's sqlite3: what a restore over a live database does to
// it, to a writer, and to a copy that is not sound.

const dbPlan = `{"sqlite":["app.db"]}`

func sql(t *testing.T, db string, statements string) string {
	t.Helper()
	out, err := exec.Command("/usr/bin/sqlite3", "-cmd", ".timeout 30000", db, statements).CombinedOutput()
	if err != nil {
		t.Fatalf("sqlite3 %s %q: %v: %s", db, statements, err, out)
	}
	return strings.TrimSpace(string(out))
}

// copyOf makes the snapshot's copy of a database the way a dump does.
func copyOf(t *testing.T, d dirs, rows string) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src.db")
	sql(t, src, "pragma journal_mode=wal; create table posts(n); insert into posts values "+rows+";")
	must(t, os.MkdirAll(filepath.Join(d.staged, "sqlite"), 0o755))
	copied := filepath.Join(d.staged, "sqlite", "app.db")
	sql(t, src, "vacuum into '"+copied+"'")
	return copied
}

func TestIntegrationALiveDatabaseIsRestoredOverUnderAWriter(t *testing.T) {
	d := newDirs(t, dbPlan, nil)
	copyOf(t, d, "(1),(2)")
	live := filepath.Join(d.target, "app.db")
	sql(t, live, "pragma journal_mode=wal; create table posts(n); insert into posts values (7);")

	var stop atomic.Bool
	failed := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			if out, err := exec.Command("/usr/bin/sqlite3", "-cmd", ".timeout 30000", live, "insert into posts values (99)").CombinedOutput(); err != nil {
				select {
				case failed <- string(out):
				default:
				}
				return
			}
		}
	}()
	time.Sleep(200 * time.Millisecond)
	a := Run(context.Background(), d.staged, d.target, AllOrNothing)
	time.Sleep(200 * time.Millisecond)
	stop.Store(true)
	<-done
	select {
	case out := <-failed:
		t.Fatalf("the writer saw an error: %s", out)
	default:
	}
	if classes(a) != "sqlite app.db: ok" {
		t.Fatalf("%+v", a)
	}
	got := sql(t, live, "pragma integrity_check; select count(*) from posts where n in (1,2); select count(*) from posts where n = 7; select count(*) > 0 from posts where n = 99; pragma journal_mode")
	if got != "ok\n2\n0\n1\nwal" {
		t.Fatalf("the live database after the restore: %q", got)
	}
	left, _ := filepath.Glob(filepath.Join(os.TempDir(), tmpPrefix+"*"))
	if len(left) != 0 {
		t.Errorf("the marker was left: %v", left)
	}
}

func TestIntegrationADamagedCopyIsNeverRestored(t *testing.T) {
	shapes := map[string]func(t *testing.T, copied string){
		// In words, with exit 0.
		"an index that lacks its rows": func(t *testing.T, copied string) {
			sql(t, copied, "create table other(n); create index posts_n on posts(n); create index other_n on other(n);")
			out, err := exec.Command("/usr/bin/sqlite3", copied, ".dbconfig defensive off", "pragma writable_schema=on; update sqlite_schema set rootpage = (select rootpage from sqlite_schema where name = 'other_n') where name = 'posts_n';").CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
		},
		// By refusing the file.
		"pages of noise": func(t *testing.T, copied string) {
			sql(t, copied, "insert into posts select hex(randomblob(3000)) from posts, posts, posts;")
			raw, err := os.ReadFile(copied)
			must(t, err)
			for i := 100; i < len(raw)-100; i += 7 {
				raw[i] ^= 0xff
			}
			must(t, os.WriteFile(copied, raw, 0o644))
		},
		"not a database at all": func(t *testing.T, copied string) {
			must(t, os.WriteFile(copied, []byte("only text"), 0o644))
		},
	}
	for name, damage := range shapes {
		t.Run(name, func(t *testing.T) {
			d := newDirs(t, `{"sqlite":["app.db"],"files":["uploads"]}`, map[string]string{"files/uploads/a.png": "img"})
			damage(t, copyOf(t, d, "(1),(2)"))
			live := filepath.Join(d.target, "app.db")
			sql(t, live, "create table posts(n); insert into posts values (7);")
			a := Run(context.Background(), d.staged, d.target, AllOrNothing)
			if got := classes(a); got != "sqlite app.db: damaged, files uploads: held back" && got != "sqlite app.db: not a database, files uploads: held back" {
				t.Fatalf("%s (%+v)", got, a.Items)
			}
			if got := sql(t, live, "select n from posts"); got != "7" {
				t.Fatalf("the live database was restored over: %q", got)
			}
			if _, err := os.Lstat(filepath.Join(d.target, "uploads")); err == nil {
				t.Error("the files were installed although the database copy is not sound")
			}
			// Into a directory of its own the files land, and no app.db.
			to := filepath.Join(t.TempDir(), "out")
			must(t, os.Mkdir(to, 0o700))
			a = Run(context.Background(), d.staged, to, WhatIsSound)
			if _, err := os.Lstat(filepath.Join(to, "app.db")); err == nil || read(t, filepath.Join(to, "uploads", "a.png")) != "img" {
				t.Fatalf("%s: app.db handed out, or the sound files held back", classes(a))
			}
		})
	}
}

func TestIntegrationWhereThereIsNoDatabaseTheCopyIsPutThere(t *testing.T) {
	d := newDirs(t, `{"sqlite":["data/app.db"]}`, nil)
	copied := copyOf(t, d, "(1),(2)")
	must(t, os.MkdirAll(filepath.Join(d.staged, "sqlite", "data"), 0o755))
	must(t, os.Rename(copied, filepath.Join(d.staged, "sqlite", "data", "app.db")))
	// Sidecars of a database that is gone.
	write(t, filepath.Join(d.target, "data", "app.db-wal"), "stale")
	write(t, filepath.Join(d.target, "data", "app.db-shm"), "stale")
	a := Run(context.Background(), d.staged, d.target, AllOrNothing)
	if classes(a) != "sqlite data/app.db: ok" {
		t.Fatalf("%+v", a)
	}
	if got := sql(t, filepath.Join(d.target, "data", "app.db"), "pragma integrity_check; select count(*) from posts"); got != "ok\n2" {
		t.Fatalf("%q", got)
	}
}

func TestIntegrationADatabaseThatIsALinkIsRefused(t *testing.T) {
	d := newDirs(t, dbPlan, nil)
	copyOf(t, d, "(1),(2)")
	theirs := filepath.Join(d.sibling, "shop.db")
	sql(t, theirs, "create table posts(n); insert into posts values (41);")
	must(t, os.Symlink(theirs, filepath.Join(d.target, "app.db")))
	a := Run(context.Background(), d.staged, d.target, AllOrNothing)
	if len(a.Items) != 1 || a.Items[0].Class != Refused || !strings.Contains(a.Items[0].Detail, "symbolic link") || !strings.Contains(a.Items[0].Detail, "app.db") {
		t.Fatalf("%+v", a)
	}
	if got := sql(t, theirs, "select n from posts"); got != "41" {
		t.Fatalf("the sibling's database was restored over: %q", got)
	}
}

func TestIntegrationALiveDatabaseWithDamagedPagesIsRestoredOver(t *testing.T) {
	d := newDirs(t, dbPlan, nil)
	copyOf(t, d, "(1),(2)")
	live := filepath.Join(d.target, "app.db")
	sql(t, live, "create table posts(n); insert into posts values (7); insert into posts select hex(randomblob(3000)) from posts, posts, posts;  insert into posts select hex(randomblob(3000)) from posts limit 20;")
	raw, err := os.ReadFile(live)
	must(t, err)
	for i := 4200; i < len(raw)-100; i += 5 { // the first page, which says what the file is, stays
		raw[i] ^= 0xff
	}
	must(t, os.WriteFile(live, raw, 0o644))
	if out, _ := exec.Command("/usr/bin/sqlite3", live, "pragma integrity_check").CombinedOutput(); strings.TrimSpace(string(out)) == "ok" {
		t.Fatal("the fixture is not damaged: the test proves nothing")
	}
	a := Run(context.Background(), d.staged, d.target, AllOrNothing)
	if classes(a) != "sqlite app.db: ok" {
		t.Fatalf("%+v", a)
	}
	if got := sql(t, live, "pragma integrity_check; select count(*) from posts"); got != "ok\n2" {
		t.Fatalf("%q", got)
	}
}

// A directory closed to writing in place is opened for the database's
// restore and closed again after, as for files — once, at the end.
func TestIntegrationADatabasesClosedDirectoryIsClosedAgain(t *testing.T) {
	d := newDirs(t, `{"sqlite":["data/app.db"]}`, nil)
	copied := copyOf(t, d, "(1),(2)")
	must(t, os.MkdirAll(filepath.Join(d.staged, "sqlite", "data"), 0o755))
	must(t, os.Rename(copied, filepath.Join(d.staged, "sqlite", "data", "app.db")))
	must(t, os.MkdirAll(filepath.Join(d.target, "data"), 0o500))
	a := Run(context.Background(), d.staged, d.target, AllOrNothing)
	if classes(a) != "sqlite data/app.db: ok" {
		t.Fatalf("%+v", a)
	}
	if st, err := os.Stat(filepath.Join(d.target, "data")); err != nil || st.Mode().Perm() != 0o500 {
		t.Errorf("data/ after the restore: %v", st.Mode().Perm())
	}
}

// A sidecar of an absent database that is not a file cannot be removed
// for the copy to go in: found at the checks, nothing changed.
func TestIntegrationASidecarThatCannotBeRemovedIsRefusedBeforeAnything(t *testing.T) {
	d := newDirs(t, `{"sqlite":["app.db"],"files":["uploads"]}`, map[string]string{"files/uploads/a.png": "img"})
	copyOf(t, d, "(1),(2)")
	must(t, os.MkdirAll(filepath.Join(d.target, "app.db-wal"), 0o755))
	a := Run(context.Background(), d.staged, d.target, AllOrNothing)
	if len(a.Items) != 2 || a.Items[0].Class != Refused || !strings.Contains(a.Items[0].Detail, "app.db-wal") || a.Items[1].Class != HeldBack || a.Changed {
		t.Fatalf("%+v", a)
	}
	if _, err := os.Lstat(filepath.Join(d.target, "app.db")); err == nil {
		t.Error("the copy was installed beside a sidecar that could not be removed")
	}
}

// A directory first made on the way to a database — 0700, nothing
// better known — gets the mode the files item then supplies for it.
func TestIntegrationADirectoryMadeForADatabaseGetsTheModeTheFilesKnow(t *testing.T) {
	d := newDirs(t, `{"sqlite":["data/app.db"],"files":["."]}`, map[string]string{"files/data/other.txt": "x", "files/r.txt": "receipt"})
	copied := copyOf(t, d, "(1),(2)")
	must(t, os.MkdirAll(filepath.Join(d.staged, "sqlite", "data"), 0o755))
	must(t, os.Rename(copied, filepath.Join(d.staged, "sqlite", "data", "app.db")))
	must(t, os.Chmod(filepath.Join(d.staged, "files", "data"), 0o755))
	a := Run(context.Background(), d.staged, d.target, AllOrNothing)
	if classes(a) != "sqlite data/app.db: ok, files .: ok" {
		t.Fatalf("%+v", a)
	}
	if st, err := os.Stat(filepath.Join(d.target, "data")); err != nil || st.Mode().Perm() != 0o755 {
		t.Errorf("data/: %v, want 755 (%v)", st.Mode().Perm(), err)
	}
}

// Directories are closed deepest first whatever order they were seen
// in: the database step makes data/ (nothing known of it), the files
// item then names data/ read-only and data/deep in it — closed in the
// order seen, data/ would shut before deep/ could be closed.
func TestIntegrationDirectoriesAreClosedDeepestFirst(t *testing.T) {
	d := newDirs(t, `{"sqlite":["data/app.db"],"files":["."]}`, map[string]string{"files/data/deep/f": "x"})
	copied := copyOf(t, d, "(1),(2)")
	must(t, os.MkdirAll(filepath.Join(d.staged, "sqlite", "data"), 0o755))
	must(t, os.Rename(copied, filepath.Join(d.staged, "sqlite", "data", "app.db")))
	must(t, os.Chmod(filepath.Join(d.staged, "files", "data", "deep"), 0o500))
	must(t, os.Chmod(filepath.Join(d.staged, "files", "data"), 0o500))
	t.Cleanup(func() {
		for _, p := range []string{filepath.Join(d.staged, "files", "data"), filepath.Join(d.target, "data")} {
			_ = os.Chmod(p, 0o755)
			_ = os.Chmod(filepath.Join(p, "deep"), 0o755)
		}
	})
	a := Run(context.Background(), d.staged, d.target, AllOrNothing)
	if classes(a) != "sqlite data/app.db: ok, files .: ok" {
		t.Fatalf("%+v", a)
	}
	for name, want := range map[string]os.FileMode{"data": 0o500, "data/deep": 0o500} {
		if st, err := os.Stat(filepath.Join(d.target, name)); err != nil || st.Mode().Perm() != want {
			t.Errorf("%s: %v, want %o (%v)", name, st.Mode().Perm(), want, err)
		}
	}
}

// A live file sqlite3 could not open — noise where the first page is —
// is found at the checks, not once the restore has begun.
func TestIntegrationALiveFileThatIsNotADatabaseIsFoundAtTheChecks(t *testing.T) {
	d := newDirs(t, `{"sqlite":["app.db"],"files":["uploads"]}`, map[string]string{"files/uploads/a.png": "img"})
	copyOf(t, d, "(1),(2)")
	write(t, filepath.Join(d.target, "app.db"), "noise where a database was, and not a header in sight")
	a := Run(context.Background(), d.staged, d.target, AllOrNothing)
	if len(a.Items) != 2 || a.Items[0].Class != Refused || !strings.Contains(a.Items[0].Detail, "move it aside") || a.Items[1].Class != HeldBack || a.Changed {
		t.Fatalf("%+v", a)
	}
	// An empty file is an empty database, and is restored over.
	d = newDirs(t, `{"sqlite":["app.db"]}`, nil)
	copyOf(t, d, "(1),(2)")
	write(t, filepath.Join(d.target, "app.db"), "")
	if a := Run(context.Background(), d.staged, d.target, AllOrNothing); classes(a) != "sqlite app.db: ok" {
		t.Fatalf("an empty file: %+v", a)
	}
}
