package backup

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type call struct {
	name string
	args []string
}

type recorder struct {
	calls []call
	fail  map[string]error
	// failIf decides per invocation, for the commands whose outcome
	// is the answer rather than a failure (restic forget in init).
	failIf func(name string, args []string) error
	// touch makes the fake sqlite3 write the file VACUUM INTO would,
	// so a second run has a stale copy to clear.
	touch bool
}

func (r *recorder) run(_ context.Context, name string, args ...string) error {
	r.calls = append(r.calls, call{name, args})
	if r.touch && name == "sqlite3" && len(args) > 0 {
		// The SQL is the last argument, after -cmd and the URI.
		if dst := stagedPathFromSQL(args[len(args)-1]); dst != "" {
			_ = os.WriteFile(dst, []byte("db"), 0o600)
		}
	}
	if err, ok := r.fail[name]; ok {
		return err
	}
	if r.failIf != nil {
		return r.failIf(name, args)
	}
	return nil
}

func stagedPathFromSQL(sql string) string {
	const prefix = "VACUUM INTO '"
	if !strings.HasPrefix(sql, prefix) || !strings.HasSuffix(sql, "'") {
		return ""
	}
	return strings.ReplaceAll(sql[len(prefix):len(sql)-1], "''", "'")
}

func testEnv(string) string { return "set" }

// newJob builds a job over a real shared dir holding the declared
// data: a declared database that is missing is an error now (it is
// almost always a typo), so the fixture has to look like an app that
// has actually run.
func newJob(t *testing.T, rec *recorder, dbs, files []string) Job {
	t.Helper()
	shared := t.TempDir()
	for _, rel := range dbs {
		p := filepath.Join(shared, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("db"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, rel := range files {
		if !filepath.IsLocal(filepath.Clean(rel)) || filepath.Clean(rel) == "." {
			continue // the containment tests pass paths that must be refused
		}
		if err := os.MkdirAll(filepath.Join(shared, rel), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	return Job{
		App:       "blog",
		Shared:    shared,
		Staging:   t.TempDir(),
		Databases: dbs,
		Files:     files,
		Run:       rec.run,
		Log:       io.Discard,
		Env:       testEnv,
	}
}

// The order matters: every database is copied before restic runs, so
// one snapshot holds a consistent set rather than a mix of moments.
func TestExecuteStagesDatabasesThenBacksUp(t *testing.T) {
	rec := &recorder{}
	job := newJob(t, rec, []string{"app.db", "data/sessions.db"}, []string{"uploads"})
	if err := job.Execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(rec.calls) != 3 {
		t.Fatalf("want 3 commands, got %d: %+v", len(rec.calls), rec.calls)
	}
	if rec.calls[0].name != "sqlite3" || rec.calls[1].name != "sqlite3" {
		t.Fatalf("databases must be staged first: %+v", rec.calls)
	}
	// -cmd .timeout <ms> comes first: sqlite3 fails immediately on a
	// busy database otherwise, and an app mid-transaction is normal.
	if got := strings.Join(rec.calls[0].args[:2], " "); got != "-cmd .timeout "+sqliteBusyTimeoutMS {
		t.Errorf("no busy timeout before the copy: %q", got)
	}
	if got, want := rec.calls[0].args[2], "file://"+filepath.Join(job.Shared, "app.db")+"?mode=ro"; got != want {
		t.Errorf("source = %q, want %q (opened read-only: the job reads the app's data, never writes it)", got, want)
	}
	wantDst := filepath.Join(StagingData(job.Staging), "data/sessions.db")
	if got := stagedPathFromSQL(rec.calls[1].args[len(rec.calls[1].args)-1]); got != wantDst {
		t.Errorf("staged copy = %q, want %q (the layout under shared/ is kept)", got, wantDst)
	}
	if _, err := os.Stat(filepath.Dir(wantDst)); err != nil {
		t.Errorf("staging subdir not created: %v", err)
	}
	restic := rec.calls[2]
	if restic.name != "restic" {
		t.Fatalf("last command = %q, want restic", restic.name)
	}
	want := []string{"backup", "--quiet", "--tag", "hotserve", "--tag", "app:blog",
		StagingData(job.Staging), filepath.Join(job.Shared, "uploads")}
	if strings.Join(restic.args, " ") != strings.Join(want, " ") {
		t.Errorf("restic argv:\n got %v\nwant %v", restic.args, want)
	}
}

// An app with no database still gets its files backed up, and restic
// is not handed an empty staging dir.
func TestExecuteFilesOnly(t *testing.T) {
	rec := &recorder{}
	job := newJob(t, rec, nil, []string{"uploads", "avatars"})
	if err := job.Execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(rec.calls) != 1 || rec.calls[0].name != "restic" {
		t.Fatalf("want one restic call, got %+v", rec.calls)
	}
	for _, a := range rec.calls[0].args {
		if a == job.Staging || a == StagingData(job.Staging) {
			t.Error("staging dir must not be backed up when nothing was staged")
		}
	}
}

// VACUUM INTO refuses to write a file that exists, so the job must
// clear last run's copy — the failure the systemd-run spike hit.
func TestExecuteClearsStaleStagedCopy(t *testing.T) {
	rec := &recorder{touch: true}
	job := newJob(t, rec, []string{"app.db"}, nil)
	for i := range 2 {
		if err := job.Execute(context.Background()); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	staged := filepath.Join(StagingData(job.Staging), "app.db")
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("second run should have re-staged the copy: %v", err)
	}
}

// A database whose `state` line was removed leaves its last copy in
// staging. Without a clear-out every later snapshot would keep
// carrying that frozen copy, and a restore would put it back beside
// the live database.
func TestExecuteDropsStagedCopiesOfUndeclaredDatabases(t *testing.T) {
	rec := &recorder{touch: true}
	job := newJob(t, rec, []string{"app.db", "old.db"}, nil)
	if err := job.Execute(context.Background()); err != nil {
		t.Fatalf("first run: %v", err)
	}
	stale := filepath.Join(StagingData(job.Staging), "old.db")
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("setup: old.db should have been staged: %v", err)
	}

	job.Databases = []string{"app.db"} // the declaration was removed
	if err := job.Execute(context.Background()); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the staged copy of an undeclared database must not survive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(StagingData(job.Staging), "app.db")); err != nil {
		t.Errorf("the declared database should still be staged: %v", err)
	}
}

// The obvious way to move an app's uploads to a data disk is a
// symlink — and restic stores a symlink AS a symlink (it has no option
// to follow one), so the snapshot would hold the link and none of the
// files, hourly, with every run reporting success. Measured against
// restic 0.18 on Debian 13: exit 0, snapshot size 0 B. A backup that
// silently holds nothing is the one outcome worth failing for.
func TestExecuteRefusesADeclaredPathThatIsASymlink(t *testing.T) {
	for _, tc := range []struct{ name, target string }{
		{"to a directory on another disk", "dir"},
		{"dangling", "not-created-yet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			job := newJob(t, rec, nil, nil)
			target := filepath.Join(job.Shared, tc.target)
			if tc.target == "dir" {
				if err := os.MkdirAll(target, 0o750); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(target, filepath.Join(job.Shared, "uploads")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			job.Files = []string{"uploads"}
			err := job.Execute(context.Background())
			if err == nil {
				t.Fatalf("a symlinked path must not be backed up as if it were the data: %+v", rec.calls)
			}
			for _, want := range []string{"symlink", "back up nothing"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal must say what would happen (%q): %v", want, err)
				}
			}
			if len(rec.calls) != 0 {
				t.Errorf("nothing should have run: %+v", rec.calls)
			}
		})
	}
}

func TestExecuteRefusesPathsOutsideShared(t *testing.T) {
	for _, tc := range []struct{ name, path string }{
		{"parent", "../../../etc/shadow"},
		{"absolute", "/var/lib/hotserve/caddy"},
		{"shared itself", "."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			job := newJob(t, rec, nil, []string{tc.path})
			err := job.Execute(context.Background())
			if err == nil || !strings.Contains(err.Error(), "stay inside") {
				t.Fatalf("want containment error, got %v", err)
			}
			if len(rec.calls) != 0 {
				t.Errorf("nothing should run: %+v", rec.calls)
			}
		})
	}
}

// `?`, `#` and `%` are legal in a filename and meaningful in a URI.
// Concatenated, `a?b.db` opens `a` with a stray parameter — sqlite3
// creates that file and reports "no such table", so the backup holds
// an empty database and says nothing (measured on Debian 13).
func TestSQLiteURIEscapesPathsThatLookLikeURIs(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"/s/a?b.db", "file:///s/a%3Fb.db?mode=ro"},
		{"/s/c#d.db", "file:///s/c%23d.db?mode=ro"},
		{"/s/e%2Ff.db", "file:///s/e%252Ff.db?mode=ro"},
		{"/s/plain.db", "file:///s/plain.db?mode=ro"},
	} {
		if got := sqliteURI(tc.path); got != tc.want {
			t.Errorf("sqliteURI(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// A path with a quote in it is legal under liveswap's validation, so
// the SQL literal has to survive one.
func TestVacuumIntoEscapesQuotes(t *testing.T) {
	got := vacuumInto("/staging/o'brien.db")
	if want := "VACUUM INTO '/staging/o''brien.db'"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestExecuteRequiresResticEnvironment(t *testing.T) {
	rec := &recorder{}
	job := newJob(t, rec, []string{"app.db"}, nil)
	job.Env = func(string) string { return "" }
	err := job.Execute(context.Background())
	if err == nil || !strings.Contains(err.Error(), "RESTIC_REPOSITORY") {
		t.Fatalf("want a missing-repository error naming the env file, got %v", err)
	}
	job.Env = func(k string) string {
		if k == "RESTIC_REPOSITORY" {
			return "s3:example/bucket"
		}
		return ""
	}
	err = job.Execute(context.Background())
	if err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("want a missing-password error, got %v", err)
	}
}

func TestQuoteArgsOnlyQuotesWhatNeedsIt(t *testing.T) {
	got := quoteArgs([]string{"backup", "--tag", "app:blog", "/var/lib/x", "a b", "it's"})
	want := `backup --tag app:blog /var/lib/x 'a b' 'it'\''s'`
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
