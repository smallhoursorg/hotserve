package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

type call struct {
	name string
	args []string
}

type recorder struct {
	calls []call
	// all is every call since the fixture was made: tests clear calls
	// between runs, and the fake repository (remembered) needs the runs
	// before.
	all  []call
	fail map[string]error
	// say answers the commands whose stdout is read. summary is what
	// `restic backup --json` prints when it exits cleanly; unset, it
	// names snapshot s1full.
	say     says
	summary string
	// failIf decides per invocation, for the commands whose outcome
	// is the answer rather than a failure (restic forget in init).
	failIf func(name string, args []string) error
	// touch makes the fake sqlite3 write the file VACUUM INTO would,
	// so a second run has a stale copy to clear.
	touch bool
}

// lastClean is what the repository would hold of this fixture's newest
// clean run: the id its clean-run record vouches for, and the paths the
// backup before that record was given.
func (r *recorder) lastClean() (id string, paths []string) {
	var targets []string
	for _, c := range r.all {
		if c.name != "restic" || len(c.args) == 0 || c.args[0] != "backup" {
			continue
		}
		if !slices.Contains(c.args, CleanTag) {
			targets = nil
			for _, a := range c.args {
				if strings.HasPrefix(a, "/") {
					targets = append(targets, a)
				}
			}
			continue
		}
		for _, a := range c.args {
			if of, ok := strings.CutPrefix(a, "clean-of:"); ok {
				id, paths = of, targets
			}
		}
	}
	return id, paths
}

// remembered answers, in front of next, the two questions a job asks the
// repository about its own past: this host's newest clean-run record,
// and the snapshot that record vouches for.
func remembered(rec *recorder, next says) says {
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "restic" && len(args) > 0 && args[0] == "snapshots" {
			last := args[len(args)-1]
			id, paths := rec.lastClean()
			switch {
			case strings.HasPrefix(last, CleanTag+",") && id == "":
				return []byte(`[]`), nil
			case strings.HasPrefix(last, CleanTag+","):
				return json.Marshal([]Snapshot{{ID: "rec1", ShortID: "rec1", Tags: []string{CleanTag, cleanAppTag("blog"), cleanOfTag(id)}}})
			case id != "" && last == id:
				return json.Marshal([]Snapshot{{ID: id, ShortID: id, Paths: paths}})
			}
		}
		return next(ctx, name, args...)
	}
}

// summaryOf is restic's own summary line for a backup (0.18.0, measured),
// naming the snapshot it wrote.
func summaryOf(id string) string {
	return `{"message_type":"summary","files_new":2,"files_changed":0,"data_added_packed":1686,"total_duration":0.7,"snapshot_id":"` + id + `"}` + "\n"
}

// exec is the fixture's Exec. A backup is recorded like any command that
// only runs, and then prints its summary; every other command whose
// stdout is read is answered by say.
func (r *recorder) exec(ctx context.Context, c Cmd) error {
	backup := c.Name == "restic" && len(c.Args) > 0 && c.Args[0] == "backup" && slices.Contains(c.Args, "--json")
	if c.Stdout == nil || backup {
		err := r.run(ctx, c.Name, c.Args...)
		if err == nil && backup {
			summary := r.summary
			if summary == "" {
				summary = summaryOf("s1full")
			}
			_, _ = c.Stdout.Write([]byte(summary))
		}
		return err
	}
	return fake(nil, r.say)(ctx, c)
}

func (r *recorder) run(_ context.Context, name string, args ...string) error {
	r.calls = append(r.calls, call{name, args})
	r.all = append(r.all, call{name, args})
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

// testEnv is a job's environment as systemd builds it from settings that
// init wrote: a backend URL and a password, and nothing else.
func testEnv(key string) string {
	switch key {
	case "RESTIC_REPOSITORY":
		return "s3:s3.example.com/bucket"
	case "RESTIC_PASSWORD":
		return "the-repository-password"
	}
	return ""
}

// newJob builds a job over a real shared dir holding the declared
// data: a declared database that is missing is an error (it is almost
// always a typo), so the fixture has to look like an app that has
// actually run.
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
		Exec:      rec.exec,
		Log:       io.Discard,
		Env:       testEnv,
	}
	rec.say = remembered(rec, snapshotHolding(heldBy(job)))
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
func snapshotHolding(held map[string]string) says {
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
	// The copies are plaintext, and the size of the databases: they go
	// when the run is over, so a box needs room for one while a run
	// lasts and not for ever.
	if _, err := os.Stat(StagingData(job.Staging)); !os.IsNotExist(err) {
		t.Errorf("the staged copies must not outlive the run: %v", err)
	}
	restic := rec.calls[2]
	if restic.name != "restic" {
		t.Fatalf("last command = %q, want restic", restic.name)
	}
	want := []string{"backup", "--json", "--quiet", "--tag", "hotserve", "--tag", "app:blog",
		StagingData(job.Staging), filepath.Join(job.Shared, "uploads")}
	if strings.Join(restic.args, " ") != strings.Join(want, " ") {
		t.Errorf("restic argv:\n got %v\nwant %v", restic.args, want)
	}
}

// As `run` launches them: the copy in one unit, the upload in another.
// The upload opens no database — it has the app's data read-only — and
// refuses to go on without the copies the step before it left.
func TestTheUploadStepUsesTheCopiesTheStagingStepLeft(t *testing.T) {
	rec := &recorder{touch: true}
	job := newJob(t, rec, []string{"app.db", "data/sessions.db"}, []string{"uploads"})
	if err := job.Stage(context.Background()); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if len(rec.calls) != 2 || rec.calls[0].name != "sqlite3" || rec.calls[1].name != "sqlite3" {
		t.Fatalf("the staging step copies the databases and runs nothing else: %+v", rec.calls)
	}
	rec.calls = nil
	job.Staged = true
	// The staging step needs neither the repository nor its settings;
	// only the upload does.
	if err := job.Execute(context.Background()); err != nil {
		t.Fatalf("upload: %v", err)
	}
	for _, c := range rec.calls {
		if c.name == "sqlite3" {
			t.Errorf("the upload step must open no database: %v", c.args)
		}
	}
	if cleanRecord(rec) == nil {
		t.Errorf("the upload step backs up, reads back and records: %+v", rec.calls)
	}

	// No copies: the staging step did not run, or failed.
	rec = &recorder{}
	job = newJob(t, rec, []string{"app.db"}, nil)
	job.Staged = true
	err := job.Execute(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no staged copy of app.db") {
		t.Fatalf("want a failure naming the missing copy, got %v", err)
	}
	if len(rec.calls) != 0 {
		t.Errorf("nothing may be uploaded without the copies: %+v", rec.calls)
	}
}

// The staging step runs with no repository settings at all.
func TestTheStagingStepNeedsNoRepositorySettings(t *testing.T) {
	rec := &recorder{touch: true}
	job := newJob(t, rec, []string{"app.db"}, nil)
	job.Env = func(string) string { return "" }
	if err := job.Stage(context.Background()); err != nil {
		t.Fatalf("stage without RESTIC_*: %v", err)
	}
	if err := job.Execute(context.Background()); err == nil || !strings.Contains(err.Error(), "RESTIC_REPOSITORY") {
		t.Fatalf("the upload still requires them, got %v", err)
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

// VACUUM INTO refuses to write a file that exists, so the copy clears
// what a run that was cut off left there.
func TestStageClearsACopyAnInterruptedRunLeft(t *testing.T) {
	rec := &recorder{touch: true}
	job := newJob(t, rec, []string{"app.db"}, nil)
	staged := filepath.Join(StagingData(job.Staging), "app.db")
	if err := os.MkdirAll(filepath.Dir(staged), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("left by a run that was cut off"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := job.Stage(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(staged); err != nil || string(got) != "db" {
		t.Fatalf("want this run's copy in place of the one left behind, got %q, %v", got, err)
	}
}

// A failed upload removes the copies too: they are no more use after a
// run that failed than after one that did not.
func TestExecuteRemovesTheStagedCopiesWhenTheUploadFails(t *testing.T) {
	rec := &recorder{touch: true, fail: map[string]error{"restic": errors.New("exit status 1")}}
	job := newJob(t, rec, []string{"app.db"}, nil)
	if err := job.Execute(context.Background()); err == nil {
		t.Fatal("setup: the upload should have failed")
	}
	if _, err := os.Stat(StagingData(job.Staging)); !os.IsNotExist(err) {
		t.Errorf("the staged copies must not outlive a failed run: %v", err)
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
	// A copy a run that was cut off left behind, of a database since
	// undeclared: the next copy clears it with the rest.
	stale := filepath.Join(StagingData(job.Staging), "old.db")
	if err := os.MkdirAll(filepath.Dir(stale), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}

	job.Databases = []string{"app.db"} // the declaration was removed
	if err := job.Stage(context.Background()); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the staged copy of an undeclared database must not survive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(StagingData(job.Staging), "app.db")); err != nil {
		t.Errorf("the declared database should be staged: %v", err)
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
			rec.say = snapshotHolding(held)
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
// holds only what was backed up from it. And it lists the snapshot
// restic named in its summary — asking the repository which snapshot is
// "this run's" is a guess — so neither another box writing to the same
// repository nor an earlier run of this one is ever the one checked.
func TestExecuteReadsBackThroughTheParentsOfWhatWasDeclared(t *testing.T) {
	var lsArgs []string
	var looked int
	rec := &recorder{}
	job := newJob(t, rec, []string{"app.db", "data/sessions.db"}, []string{"uploads"})
	holding := rec.say
	rec.say = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch args[0] {
		case "ls":
			lsArgs = args
		case "snapshots":
			looked++
		}
		return holding(ctx, name, args...)
	}
	if err := job.Execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	want := []string{"ls", "--json", "s1full",
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
	if looked != 0 {
		t.Errorf("the snapshot to check is the one restic named, not one looked up: %d lookups", looked)
	}
	// A snapshot holding everything declared is a success, recorded in
	// the repository against that snapshot's full id, and never tagged
	// as a backup itself.
	r := cleanRecord(rec)
	if r == nil || !slices.Contains(r, "clean-of:s1full") || !slices.Contains(r, "clean-app:blog") || slices.Contains(r, "hotserve") {
		t.Errorf("want a clean-run record for s1full, tagged for blog and not as a backup, got %v", r)
	}
}

// The clean-run record vouches for one snapshot by id, and the id is the
// one restic gave for this run's backup. A run restic does not name a
// snapshot for has nothing to read back, and records nothing.
func TestTheSnapshotIsTheOneResticNamed(t *testing.T) {
	rec := &recorder{summary: summaryOf("9f3a77c2e1d04b5a")}
	job := newJob(t, rec, []string{"app.db"}, nil)
	var listed string
	holding := rec.say
	rec.say = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if args[0] == "ls" {
			listed = args[2]
		}
		return holding(ctx, name, args...)
	}
	if err := job.Execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if listed != "9f3a77c2e1d04b5a" {
		t.Errorf("read back snapshot %q, want the one restic named", listed)
	}
	if r := cleanRecord(rec); r == nil || !slices.Contains(r, "clean-of:9f3a77c2e1d04b5a") {
		t.Errorf("the clean-run record must vouch for the snapshot restic named: %v", r)
	}

	for name, summary := range map[string]string{
		"nothing on stdout":            " ",
		"a summary with no id":         `{"message_type":"summary","files_new":2}` + "\n",
		"a line that is not a message": "backup done\n",
	} {
		t.Run(name, func(t *testing.T) {
			rec := &recorder{summary: summary}
			job := newJob(t, rec, []string{"app.db"}, nil)
			err := job.Execute(context.Background())
			if err == nil || !strings.Contains(err.Error(), "without naming the snapshot") {
				t.Fatalf("want a failure saying no snapshot was named, got %v", err)
			}
			if r := cleanRecord(rec); r != nil {
				t.Errorf("no snapshot was identified, so none may be vouched for: %v", r)
			}
		})
	}
}

// restic's errors arrive as JSON with --json, and the journal is read by
// people: each is put back into its own words, with the file it is about.
func TestResticsErrorsReachTheLogInWords(t *testing.T) {
	rec := &recorder{}
	job := newJob(t, rec, nil, []string{"uploads"})
	var log strings.Builder
	job.Log = &log
	job.Exec = func(ctx context.Context, c Cmd) error {
		if c.Name == "restic" && c.Args[0] == "backup" && c.Stderr != nil {
			_, _ = c.Stderr.Write([]byte(`{"message_type":"error","error":{"message":"open /data/uploads/locked: permission denied"},"during":"archival","item":"/data/uploads/locked"}` + "\n" +
				`{"message_type":"exit_error","code":3,"message":"Warning: at least one source file could not be read"}` + "\n"))
			_, _ = c.Stdout.Write([]byte(summaryOf("partial1")))
			return errors.New("exit status 3")
		}
		return rec.exec(ctx, c)
	}
	err := job.Execute(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("want the run to fail on restic's exit status, got %v", err)
	}
	for _, want := range []string{"restic: open /data/uploads/locked: permission denied (/data/uploads/locked)", "restic: Warning: at least one source file could not be read"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("missing %q in the log:\n%s", want, log.String())
		}
	}
	if strings.Contains(log.String(), "message_type") {
		t.Errorf("JSON reached the journal:\n%s", log.String())
	}
	if r := cleanRecord(rec); r != nil {
		t.Errorf("a snapshot restic wrote on its way to exit 3 must not be vouched for: %v", r)
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
	rec := &recorder{}
	job := newJob(t, rec, nil, []string{"uploads", "avatars"})
	if err := job.Execute(context.Background()); err != nil {
		t.Fatalf("first run: %v", err)
	}
	job.Files = []string{"uploads"}
	if err := os.RemoveAll(filepath.Join(job.Shared, "avatars")); err != nil {
		t.Fatal(err)
	}
	rec.say = remembered(rec, snapshotHolding(heldBy(job)))
	if err := job.Execute(context.Background()); err != nil {
		t.Fatalf("an undeclared path's absence is not a failure: %v", err)
	}
	if _, paths := rec.lastClean(); !slices.Equal(paths, []string{filepath.Join(job.Shared, "uploads")}) {
		t.Errorf("the newest clean run should hold exactly what is declared and present: %v", paths)
	}
}

// What a box has backed up before is in the repository, so a run whose
// declared paths are all there asks it nothing: two calls to read its
// snapshot back, and no more. One that finds a path missing asks once,
// for this host's newest clean run.
func TestTheRepositoryIsAskedAboutThePastOnlyWhenAPathIsMissing(t *testing.T) {
	rec := &recorder{}
	job := newJob(t, rec, nil, []string{"uploads", "avatars", "exports"})
	var asked [][]string
	inner := rec.say
	rec.say = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if args[0] == "snapshots" && strings.HasPrefix(args[len(args)-1], CleanTag+",") {
			asked = append(asked, args)
		}
		return inner(ctx, name, args...)
	}
	if err := job.Execute(context.Background()); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if len(asked) != 0 {
		t.Fatalf("every declared path was there, and the repository was still asked: %v", asked)
	}
	// Two paths the app never made: both are skipped on one question.
	job.Files = append(job.Files, "later", "later-still")
	if err := job.Execute(context.Background()); err != nil {
		t.Fatalf("paths that were never there are not a failure: %v", err)
	}
	if len(asked) != 1 {
		t.Fatalf("want one question for the whole run, got %d: %v", len(asked), asked)
	}
	host, _ := os.Hostname()
	if got := strings.Join(asked[0], " "); !strings.Contains(got, "--latest 1 --host "+host) || !strings.HasSuffix(got, CleanTag+",clean-app:blog") {
		t.Errorf("the question must be this host's newest clean run of this app: %s", got)
	}
}

// Not knowing is not "not created yet". A run that finds a declared
// path missing and cannot ask the repository fails, rather than dropping
// the path from every backup from then on.
func TestAMissingPathFailsTheRunWhenThePastCannotBeRead(t *testing.T) {
	rec := &recorder{}
	job := newJob(t, rec, []string{"app.db"}, nil)
	job.Files = []string{"uploads"} // declared, not there
	inner := rec.say
	rec.say = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if args[0] == "snapshots" && strings.HasPrefix(args[len(args)-1], CleanTag+",") {
			return nil, errors.New("exit status 1")
		}
		return inner(ctx, name, args...)
	}
	err := job.Execute(context.Background())
	if err == nil || !strings.Contains(err.Error(), "uploads is not there") || !strings.Contains(err.Error(), "could not be read from the repository") {
		t.Fatalf("want a failure saying what could not be known, got %v", err)
	}
	for _, c := range rec.calls {
		if c.name == "restic" {
			t.Errorf("nothing may be backed up by a run that does not know what it is dropping: %v", c.args)
		}
	}
}

// `state files uploads/` and `state files uploads` name one directory;
// changing the spelling must not make a vanished path look new.
func TestABackedUpPathIsKnownHoweverItIsSpelled(t *testing.T) {
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

// Every app's clean-run record has a file name of its own: `restic
// forget` applies a policy per group of host and paths, and with one name
// for every app the documented retention keeps one app's records and
// forgets the others' (measured, restic 0.18).
func TestEachAppsCleanRecordsAreAGroupOfTheirOwn(t *testing.T) {
	name := func(app string) string {
		args := cleanRecordArgs(app, "s1full")
		return args[slices.Index(args, "--stdin-filename")+1]
	}
	if name("blog") == name("shop") {
		t.Errorf("two apps' records share the name %q, and so one forget group", name("blog"))
	}
	if strings.Contains(name("blog"), "/") {
		t.Errorf("restic 0.18 cannot save a --stdin-filename with a slash in it: %q", name("blog"))
	}
}

// An app's whole shared dir gone is "never deployed" only when this box
// has never backed the app up. Otherwise it is the app's data gone — or
// its volume not mounted — and a run that skipped it would be green every
// hour over nothing.
func TestMissingDataIsNoDataOnlyForAnAppNeverBackedUp(t *testing.T) {
	rec := &recorder{}
	job := newJob(t, rec, nil, []string{"uploads"})
	if err := job.missingData(context.Background()); !errors.Is(err, errNoData) {
		t.Fatalf("no clean run yet: want errNoData, got %v", err)
	}
	if err := job.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := job.missingData(context.Background())
	if err == nil || errors.Is(err, errNoData) || !strings.Contains(err.Error(), "backed this app up before") {
		t.Fatalf("after a clean run a missing data dir is a failure, got %v", err)
	}
	// Not knowing is not "never deployed".
	job.Exec = func(_ context.Context, c Cmd) error { return errors.New("repository unreachable") }
	if err := job.missingData(context.Background()); err == nil || errors.Is(err, errNoData) {
		t.Fatalf("a repository that cannot be asked fails the run, got %v", err)
	}
}

// A run with nothing to hand restic is not a backup, and must not read
// as one: no restic runs, and the job's answer is errNoData, which the
// command turns into a status the run reports as a skip — not "backed
// up", and not "ok".
func TestExecuteWithNothingToCopyIsNotABackup(t *testing.T) {
	rec := &recorder{}
	job := newJob(t, rec, nil, []string{"uploads"})
	if err := os.RemoveAll(filepath.Join(job.Shared, "uploads")); err != nil {
		t.Fatal(err)
	}
	if err := job.Execute(context.Background()); !errors.Is(err, errNoData) {
		t.Fatalf("want errNoData, got %v", err)
	}
	for _, c := range rec.calls {
		if c.name == "restic" && len(c.args) > 0 && c.args[0] == "backup" {
			t.Errorf("nothing to back up, and restic backup ran: %v", c.args)
		}
	}
}

// A restore puts back files and directories. A declared path that is
// anything else — a FIFO, a socket — is refused before restic sees it,
// rather than backed up, vouched for, and then found unrestorable; and a
// snapshot that holds one all the same does not earn a clean-run record.
func TestOnlyWhatARestorePutsBackIsBackedUpAsClean(t *testing.T) {
	rec := &recorder{}
	job := newJob(t, rec, nil, []string{"uploads"})
	pipe := filepath.Join(job.Shared, "events")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Skipf("no FIFOs here: %v", err)
	}
	job.Files = []string{"uploads", "events"}
	err := job.Execute(context.Background())
	if err == nil || !strings.Contains(err.Error(), "named pipe") || !strings.Contains(err.Error(), "not a file or a directory") {
		t.Fatalf("want a declared FIFO refused by name, got %v", err)
	}
	if len(rec.calls) != 0 {
		t.Errorf("nothing should have run: %v", rec.calls)
	}

	for _, typ := range []string{"fifo", "socket", "chardev"} {
		x := fake(nil, func(context.Context, string, ...string) ([]byte, error) {
			return []byte(`{"struct_type":"node","path":"/data/events","type":"` + typ + `"}` + "\n"), nil
		})
		err := Job{App: "blog", Exec: x, Log: io.Discard}.verifySnapshot(context.Background(), "s1full", []expectedNode{{path: "/data/events"}})
		if err == nil || !strings.Contains(err.Error(), "does not put back") {
			t.Errorf("a snapshot holding a %s must not read back as clean: %v", typ, err)
		}
	}
}

// A declared database is a file, or a link to one. A named pipe at its
// path would have sqlite3 wait for a writer that never comes — and with
// no limit on a job's time, hold up every app after it.
func TestStageRefusesADatabaseThatIsNotAFile(t *testing.T) {
	rec := &recorder{touch: true}
	job := newJob(t, rec, []string{"app.db"}, nil)
	live := filepath.Join(job.Shared, "app.db")
	if err := os.Remove(live); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(live, 0o600); err != nil {
		t.Skipf("no FIFOs here: %v", err)
	}
	err := job.Stage(context.Background())
	if err == nil || !strings.Contains(err.Error(), "named pipe") {
		t.Fatalf("want the FIFO refused by name, got %v", err)
	}
	if len(rec.calls) != 0 {
		t.Errorf("sqlite3 must not be given it: %v", rec.calls)
	}
	// A link to the database is followed: the copy is of the data.
	if err := os.Remove(live); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(job.Shared, "real.db")
	if err := os.WriteFile(real, []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, live); err != nil {
		t.Fatal(err)
	}
	if err := job.Stage(context.Background()); err != nil {
		t.Errorf("a link to a database file is a database: %v", err)
	}
}
