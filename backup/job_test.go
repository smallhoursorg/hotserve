package backup

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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
	job := Job{
		App:       "blog",
		Shared:    shared,
		Staging:   t.TempDir(),
		Databases: dbs,
		Files:     files,
		Run:       rec.run,
		Log:       io.Discard,
		Env:       testEnv,
	}
	job.Capture = snapshotHolding(heldBy(job))
	return job
}

// heldBy is what a snapshot of this job holds when everything went
// right: each staged database as a file with data in it, each declared
// files path as a directory. Keyed by path, valued by restic's JSON for
// the node; tests edit it to build a snapshot that does NOT hold what
// was declared ("" removes a node).
func heldBy(j Job) map[string]string {
	held := map[string]string{}
	for _, rel := range j.Databases {
		p := filepath.Join(StagingData(j.Staging), rel)
		held[p] = fmt.Sprintf(`{"struct_type":"node","path":%q,"type":"file","size":4096}`, p)
	}
	for _, rel := range j.Files {
		if p, err := sharedPath(j.Shared, rel); err == nil {
			held[p] = fmt.Sprintf(`{"struct_type":"node","path":%q,"type":"dir"}`, p)
		}
	}
	return held
}

// snapshotHolding answers the job's read-back the way restic does for
// a snapshot holding `held`: one snapshot, and `ls` of a directory
// lists that directory's entries in the snapshot — which are only the
// ones that were backed up, never the rest of what was on disk.
func snapshotHolding(held map[string]string) Capturer {
	return func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch args[0] {
		case "snapshots":
			return []byte(`[{"id":"s1full","short_id":"s1","time":"2026-09-18T12:00:00Z","tags":["hotserve","app:blog"]}]`), nil
		case "ls":
			var b strings.Builder
			b.WriteString(`{"struct_type":"snapshot","short_id":"s1"}` + "\n")
			for _, dir := range args[3:] {
				fmt.Fprintf(&b, `{"struct_type":"node","path":%q,"type":"dir"}`+"\n", dir)
				for p, node := range held {
					if filepath.Dir(p) == dir && node != "" {
						b.WriteString(node + "\n")
					}
				}
			}
			return []byte(b.String()), nil
		}
		return nil, fmt.Errorf("unexpected restic %v", args)
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
	// Two copies, the backup, then the clean-run record (after the
	// read-back, which captures rather than runs).
	if len(rec.calls) != 4 || cleanRecord(rec) == nil {
		t.Fatalf("want 4 commands ending in the record, got %d: %+v", len(rec.calls), rec.calls)
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
	if len(rec.calls) != 2 || rec.calls[0].name != "restic" || cleanRecord(rec) == nil {
		t.Fatalf("want the backup then its clean-run record, got %+v", rec.calls)
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

// cleanRecord is the clean-run record a job wrote, if it wrote one.
func cleanRecord(rec *recorder) []string {
	for _, c := range rec.calls {
		if c.name == "restic" && slices.Contains(c.args, CleanTag) {
			return c.args
		}
	}
	return nil
}

// Every check before the read-back is about the job's own view; none
// of them is the backup. A snapshot that does not hold a declared
// path, holds it as a link, or holds an empty database copy must fail
// the run — so it never earns the clean-run record that `status` reads
// as "backed up" and restore chooses by.
func TestExecuteFailsWhenTheSnapshotDoesNotHoldWhatWasDeclared(t *testing.T) {
	uploads := func(j Job) string { return filepath.Join(j.Shared, "uploads") }
	appDB := func(j Job) string { return filepath.Join(StagingData(j.Staging), "app.db") }
	for _, tc := range []struct {
		name, want string
		edit       func(j Job, held map[string]string)
	}{
		{"a files path missing from the snapshot", "does not contain", func(j Job, held map[string]string) {
			held[uploads(j)] = ""
		}},
		{"a files path stored as a link", "as a symlink", func(j Job, held map[string]string) {
			held[uploads(j)] = fmt.Sprintf(`{"struct_type":"node","path":%q,"type":"symlink"}`, uploads(j))
		}},
		{"an empty database copy", "empty", func(j Job, held map[string]string) {
			held[appDB(j)] = fmt.Sprintf(`{"struct_type":"node","path":%q,"type":"file","size":0}`, appDB(j))
		}},
		{"a database copy missing from the snapshot", "does not contain", func(j Job, held map[string]string) {
			held[appDB(j)] = ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			job := newJob(t, rec, []string{"app.db"}, []string{"uploads"})
			held := heldBy(job)
			tc.edit(job, held)
			job.Capture = snapshotHolding(held)
			err := job.Execute(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a failure saying %q, got %v", tc.want, err)
			}
			if r := cleanRecord(rec); r != nil {
				t.Errorf("a run whose snapshot does not hold what was declared must not be recorded as clean: %v", r)
			}
		})
	}
}

// The read-back lists each declared path's PARENT, never the path
// itself: restic lists a named directory's direct children, so naming
// an uploads dir would return a line per upload (measured: 501 lines
// for 500 files, against 2 for its parent). In the snapshot a parent
// holds only what was backed up from it. And it asks for this box's
// snapshots only, so another box writing to the same repository is
// never the one checked.
func TestExecuteReadsBackThroughTheParentsOfWhatWasDeclared(t *testing.T) {
	var lsArgs, snapshotArgs []string
	rec := &recorder{}
	job := newJob(t, rec, []string{"app.db", "data/sessions.db"}, []string{"uploads"})
	holding := job.Capture
	job.Capture = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch args[0] {
		case "ls":
			lsArgs = args
		case "snapshots":
			snapshotArgs = args
		}
		return holding(ctx, name, args...)
	}
	if err := job.Execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	want := []string{"ls", "--json", "s1",
		StagingData(job.Staging),
		filepath.Join(StagingData(job.Staging), "data"),
		job.Shared,
	}
	if strings.Join(lsArgs, " ") != strings.Join(want, " ") {
		t.Errorf("read-back:\n got %v\nwant %v", lsArgs, want)
	}
	for _, a := range lsArgs {
		if a == "--recursive" || a == filepath.Join(job.Shared, "uploads") {
			t.Errorf("the read-back must not list the declared directory's own contents: %v", lsArgs)
		}
	}
	host, _ := os.Hostname()
	if !strings.Contains(strings.Join(snapshotArgs, " "), "--host "+host) {
		t.Errorf("the snapshot checked must be this box's: %v", snapshotArgs)
	}
	// A snapshot holding everything declared is a success, recorded in
	// the repository against that snapshot's full id, and never tagged
	// as a backup itself.
	r := cleanRecord(rec)
	if r == nil || !slices.Contains(r, "clean-of:s1full") || !slices.Contains(r, "clean-app:blog") || slices.Contains(r, "hotserve") {
		t.Errorf("want a clean-run record for s1full, tagged for blog and not as a backup, got %v", r)
	}
}

// A declared path that goes missing after it has been backed up must
// fail the run. Skipping it — the right answer for a path the app has
// not created yet — would let the other declarations keep the app green
// while this one is never backed up again.
func TestExecuteFailsWhenABackedUpPathGoesMissing(t *testing.T) {
	rec := &recorder{}
	job := newJob(t, rec, []string{"app.db"}, []string{"uploads", "avatars"})
	if err := job.Execute(context.Background()); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(job.Shared, "uploads")); err != nil {
		t.Fatal(err)
	}
	rec.calls = nil
	err := job.Execute(context.Background())
	if err == nil || !strings.Contains(err.Error(), "backed up before") {
		t.Fatalf("want a failure naming the vanished path, got %v", err)
	}
	if r := cleanRecord(rec); r != nil {
		t.Errorf("a run that lost a declared path must not be recorded as clean: %v", r)
	}
}

// A path the app has never created is still a note, not a failure: a new
// app declares where its uploads will go before anyone has uploaded.
func TestExecuteSkipsADeclaredPathThatNeverExisted(t *testing.T) {
	job := newJob(t, &recorder{}, []string{"app.db"}, nil)
	job.Files = []string{"uploads"} // declared, never created
	for i := range 2 {
		if err := job.Execute(context.Background()); err != nil {
			t.Fatalf("run %d: a path that was never there is not a failure: %v", i+1, err)
		}
	}
}

// Removing a `state files` line removes the path from what is expected:
// deleting that directory afterwards is not a failure.
func TestExecuteForgetsAPathThatIsNoLongerDeclared(t *testing.T) {
	job := newJob(t, &recorder{}, nil, []string{"uploads", "avatars"})
	if err := job.Execute(context.Background()); err != nil {
		t.Fatalf("first run: %v", err)
	}
	job.Files = []string{"uploads"}
	if err := os.RemoveAll(filepath.Join(job.Shared, "avatars")); err != nil {
		t.Fatal(err)
	}
	job.Capture = snapshotHolding(heldBy(job))
	if err := job.Execute(context.Background()); err != nil {
		t.Fatalf("an undeclared path's absence is not a failure: %v", err)
	}
	if seen := readSeen(SeenPaths(job.Staging)); seen["avatars"] || !seen["uploads"] {
		t.Errorf("the list should now hold exactly what was declared and present: %v", seen)
	}
}

// `state files uploads/` and `state files uploads` name one directory;
// changing the spelling must not make a vanished path look new.
func TestTheSeenListIgnoresHowAPathIsSpelled(t *testing.T) {
	job := newJob(t, &recorder{}, nil, []string{"uploads/"})
	if err := job.Execute(context.Background()); err != nil {
		t.Fatalf("first run: %v", err)
	}
	job.Files = []string{"./uploads"}
	if err := os.RemoveAll(filepath.Join(job.Shared, "uploads")); err != nil {
		t.Fatal(err)
	}
	if err := job.Execute(context.Background()); err == nil || !strings.Contains(err.Error(), "backed up before") {
		t.Fatalf("a respelled declaration of a vanished path must still fail, got %v", err)
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
