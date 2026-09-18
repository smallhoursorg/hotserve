package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// restoreFixture is a restore of app blog from snapshot s1, whose
// listing holds `held` (path → restic's JSON node; "" leaves it out),
// against a fake restic and sqlite3 that record what they were asked.
type restoreFixture struct {
	job   RestoreJob
	held  map[string]string
	calls []call
	// integrity is what `PRAGMA integrity_check` answers.
	integrity string
	dumped    map[string]string // dst → the snapshot path written there
}

func newRestore(t *testing.T, dbs, files []string) *restoreFixture {
	t.Helper()
	f := &restoreFixture{integrity: "ok", dumped: map[string]string{}}
	f.job = RestoreJob{
		App:       "blog",
		Shared:    t.TempDir(),
		Staging:   t.TempDir(),
		Snapshot:  "s1full",
		Databases: dbs,
		Files:     files,
		Log:       io.Discard,
	}
	f.held = map[string]string{}
	for _, rel := range dbs {
		p := filepath.Join(StagingData(f.job.Staging), rel)
		f.held[p] = fmt.Sprintf(`{"struct_type":"node","path":%q,"type":"file","size":4096,"mode":420}`, p)
	}
	for _, rel := range files {
		p := filepath.Join(f.job.Shared, rel)
		f.held[p] = fmt.Sprintf(`{"struct_type":"node","path":%q,"type":"dir","mode":2147484141}`, p)
	}
	f.job.Run = func(_ context.Context, name string, args ...string) error {
		f.calls = append(f.calls, call{name, args})
		return nil
	}
	f.job.Capture = func(_ context.Context, name string, args ...string) ([]byte, error) {
		f.calls = append(f.calls, call{name, args})
		switch {
		case name == "sqlite3":
			return []byte(f.integrity + "\n"), nil
		case name == "restic" && args[0] == "ls":
			var b strings.Builder
			for _, dir := range args[slices.Index(args, "s1full")+1:] {
				for p, node := range f.held {
					if filepath.Dir(p) == dir && node != "" {
						b.WriteString(node + "\n")
					}
				}
			}
			return []byte(b.String()), nil
		}
		return nil, fmt.Errorf("unexpected %s %v", name, args)
	}
	f.job.Dump = func(_ context.Context, snapshot, path, dst string) error {
		f.calls = append(f.calls, call{"dump", []string{snapshot, path, dst}})
		f.dumped[dst] = path
		file, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, _ = file.WriteString("from the snapshot")
		return file.Close()
	}
	return f
}

// what the restore changed on the live side: sqlite3 .restore and
// restic restore. Everything else it runs only reads.
func (f *restoreFixture) writes() []call {
	var out []call
	for _, c := range f.calls {
		if (c.name == "sqlite3" && slices.ContainsFunc(c.args, func(a string) bool { return strings.HasPrefix(a, ".restore ") })) ||
			(c.name == "restic" && c.args[0] == "restore") {
			out = append(out, c)
		}
	}
	return out
}

func TestRestorePutsTheDatabaseBackThroughSQLiteAndTheFilesThroughRestic(t *testing.T) {
	f := newRestore(t, []string{"app.db"}, []string{"uploads"})
	if err := f.job.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := f.writes()
	if len(w) != 2 {
		t.Fatalf("want one database and one directory restored, got %v", w)
	}
	live := filepath.Join(f.job.Shared, "app.db")
	copyPath := filepath.Join(StagingRestore(f.job.Staging), "copies", "0.db")
	if got := w[0].args; !slices.Equal(got, []string{"-cmd", ".timeout " + sqliteBusyTimeoutMS, live, ".restore " + copyPath}) {
		t.Errorf("the database must go back through sqlite3 .restore into the live file, got %v", got)
	}
	uploads := filepath.Join(f.job.Shared, "uploads")
	if got := w[1].args; !slices.Equal(got, []string{"restore", "s1full:" + uploads, "--target", uploads}) {
		t.Errorf("files go back as the snapshot's copy of that one directory, without --delete unless asked, got %v", got)
	}
	if f.dumped[copyPath] != filepath.Join(StagingData(f.job.Staging), "app.db") {
		t.Errorf("the database comes out of the snapshot from the copy the backup took, got %v", f.dumped)
	}
	if _, err := os.Stat(filepath.Dir(copyPath)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the plaintext copies must be gone after a restore: %v", err)
	}
}

func TestRestoreDeletesOnlyWhenAsked(t *testing.T) {
	f := newRestore(t, nil, []string{"uploads"})
	f.job.Delete = true
	if err := f.job.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w := f.writes(); len(w) != 1 || !slices.Contains(w[0].args, "--delete") {
		t.Errorf("--delete must reach restic restore, got %v", w)
	}
}

// A copy that fails its integrity check, a database the snapshot does
// not hold, a path held as something unexpected: each is found before
// the first write, so the live data is exactly as it was.
func TestRestoreChecksEverythingBeforeItChangesAnything(t *testing.T) {
	cases := map[string]func(f *restoreFixture){
		"a database copy fails its integrity check": func(f *restoreFixture) { f.integrity = "*** in database main ***\nPage 3 is never used" },
		"the snapshot has no copy of a database": func(f *restoreFixture) {
			f.held[filepath.Join(StagingData(f.job.Staging), "app.db")] = ""
		},
		"the database copy in the snapshot is empty": func(f *restoreFixture) {
			p := filepath.Join(StagingData(f.job.Staging), "app.db")
			f.held[p] = fmt.Sprintf(`{"struct_type":"node","path":%q,"type":"file","size":0}`, p)
		},
		"a files path is held as a symlink": func(f *restoreFixture) {
			p := filepath.Join(f.job.Shared, "uploads")
			f.held[p] = fmt.Sprintf(`{"struct_type":"node","path":%q,"type":"symlink"}`, p)
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRestore(t, []string{"app.db"}, []string{"uploads", "config.json"})
			// A declared single file too: it is put back with dump and a
			// rename rather than a command writes() sees, so its bytes
			// are what show whether it was touched.
			cfg := filepath.Join(f.job.Shared, "config.json")
			f.held[cfg] = fmt.Sprintf(`{"struct_type":"node","path":%q,"type":"file","size":17,"mode":420}`, cfg)
			if err := os.WriteFile(cfg, []byte("live"), 0o600); err != nil {
				t.Fatal(err)
			}
			breakIt(f)
			err := f.job.Execute(context.Background())
			if err == nil {
				t.Fatal("the restore went ahead")
			}
			if w := f.writes(); len(w) != 0 {
				t.Errorf("nothing live may change when the snapshot cannot restore everything; changed %v", w)
			}
			if got, _ := os.ReadFile(cfg); string(got) != "live" {
				t.Errorf("config.json was replaced before the checks finished: %q", got)
			}
			if !strings.Contains(err.Error(), "nothing was restored") {
				t.Errorf("the error must say the data was left alone: %v", err)
			}
		})
	}
}

// A directory restore is not atomic, so its failure must not read as
// "left as it was" — and a database's must, because .restore is.
func TestRestoreSaysWhatAFailurePartWayLeaves(t *testing.T) {
	f := newRestore(t, []string{"app.db"}, []string{"uploads"})
	f.job.Run = func(_ context.Context, name string, args ...string) error {
		f.calls = append(f.calls, call{name, args})
		if name == "restic" {
			return errors.New("exit status 1")
		}
		return nil
	}
	err := f.job.Execute(context.Background())
	if err == nil || !strings.Contains(err.Error(), "may be partly restored") || !strings.Contains(err.Error(), "run the restore again") {
		t.Errorf("a directory that failed part-way must be called partly restored, got %v", err)
	}
	f = newRestore(t, []string{"app.db"}, nil)
	f.job.Run = func(context.Context, string, ...string) error { return errors.New("exit status 1") }
	if err := f.job.Execute(context.Background()); err == nil || !strings.Contains(err.Error(), "it is as it was") {
		t.Errorf("a failed .restore leaves the database as it was, and must say so, got %v", err)
	}
}

// A repository on this box is read-only to the restore, so restic runs
// without a lock; a remote one is locked, so a prune elsewhere waits.
func TestRestoreLocksARemoteRepository(t *testing.T) {
	for _, noLock := range []bool{false, true} {
		f := newRestore(t, nil, []string{"uploads"})
		f.job.NoLock = noLock
		if err := f.job.Execute(context.Background()); err != nil {
			t.Fatal(err)
		}
		for _, c := range f.calls {
			if c.name == "restic" && slices.Contains(c.args, "--no-lock") != noLock {
				t.Errorf("NoLock=%v but restic %v", noLock, c.args)
			}
		}
	}
}

// The backup skips a declared path the app had not created yet, so a
// snapshot without it is ordinary: the rest is restored and it is left.
func TestRestoreLeavesAPathTheSnapshotNeverHad(t *testing.T) {
	f := newRestore(t, nil, []string{"uploads", "avatars"})
	f.held[filepath.Join(f.job.Shared, "avatars")] = ""
	if err := f.job.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := f.writes()
	if len(w) != 1 || !strings.HasSuffix(w[0].args[len(w[0].args)-1], "/uploads") {
		t.Errorf("want uploads restored and avatars left alone, got %v", w)
	}
}

// A declared single file comes out with dump and replaces the live one
// in a rename, with its mode — and a link the app left at the name the
// restore writes to is removed, not written through.
func TestRestoreReplacesADeclaredFileWithoutFollowingALink(t *testing.T) {
	f := newRestore(t, nil, []string{"config.json"})
	live := filepath.Join(f.job.Shared, "config.json")
	f.held[live] = fmt.Sprintf(`{"struct_type":"node","path":%q,"type":"file","size":17,"mode":493}`, live)
	if err := os.WriteFile(live, []byte("changed since"), 0o600); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(t.TempDir(), "not-the-apps")
	if err := os.WriteFile(elsewhere, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(f.job.Shared, ".config.json.hotserve-restore")); err != nil {
		t.Fatal(err)
	}
	if err := f.job.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(live); string(got) != "from the snapshot" {
		t.Errorf("config.json holds %q, want the snapshot's", got)
	}
	if info, _ := os.Stat(live); info.Mode().Perm() != 0o755 {
		t.Errorf("config.json has mode %v, want the snapshot's 0755", info.Mode().Perm())
	}
	// A recorded mode of 0 is a mode, not a missing one.
	f = newRestore(t, nil, []string{"secret.key"})
	locked := filepath.Join(f.job.Shared, "secret.key")
	f.held[locked] = fmt.Sprintf(`{"struct_type":"node","path":%q,"type":"file","size":17,"mode":0}`, locked)
	if err := f.job.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(locked); info.Mode().Perm() != 0 {
		t.Errorf("secret.key has mode %v, want the snapshot's 0000", info.Mode().Perm())
	}
	if got, _ := os.ReadFile(elsewhere); string(got) != "untouched" {
		t.Errorf("the restore wrote through a link to %s", elsewhere)
	}
}

func TestPickSnapshot(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 9, 18, h, 0, 0, 0, time.UTC) }
	snaps := []Snapshot{
		{ID: "aaaa1111bbbb", ShortID: "aaaa1111", Time: at(3)},
		{ID: "cccc2222dddd", ShortID: "cccc2222", Time: at(5)},
		{ID: "aaaa1111eeee", ShortID: "aaaa1111", Time: at(1)},
	}
	allClean := map[string]bool{"aaaa1111bbbb": true, "cccc2222dddd": true, "aaaa1111eeee": true}
	if s, note, err := pickSnapshot("blog", snaps, "", allClean); err != nil || s.ID != "cccc2222dddd" || note != "" {
		t.Errorf("unset picks the newest clean one, from any box, got %v %q %v", s.ID, note, err)
	}
	if s, _, err := pickSnapshot("blog", snaps, "cccc2222", allClean); err != nil || s.ID != "cccc2222dddd" {
		t.Errorf("a short id picks that snapshot, got %v %v", s.ID, err)
	}
	if s, _, err := pickSnapshot("blog", snaps, "aaaa1111eeee", allClean); err != nil || s.ID != "aaaa1111eeee" {
		t.Errorf("a full id picks that snapshot, got %v %v", s.ID, err)
	}
	if _, _, err := pickSnapshot("blog", snaps, "aaaa1111", allClean); err == nil || !strings.Contains(err.Error(), "matches 2") {
		t.Errorf("an ambiguous id must be refused, got %v", err)
	}
	// Only this app's snapshots are listed, so another app's id is not
	// among them: refused, rather than restored into this app.
	if _, _, err := pickSnapshot("blog", snaps, "ffff9999", allClean); err == nil {
		t.Error("an id that is not one of this app's snapshots must be refused")
	}
	if _, _, err := pickSnapshot("blog", nil, "", allClean); err == nil {
		t.Error("an app with no snapshots must be refused")
	}
	// The run at 05:00 left a snapshot but no clean-run record: it may be
	// missing files, so it is named, not chosen — restored with --delete
	// it would delete them. This holds on a rebuilt box too: the record
	// is in the repository, not on the box that died.
	s, note, err := pickSnapshot("blog", snaps, "", map[string]bool{"aaaa1111bbbb": true})
	if err != nil || s.ID != "aaaa1111bbbb" || !strings.Contains(note, "cccc2222") {
		t.Errorf("want the 03:00 snapshot and a note naming cccc2222, got %v %q %v", s.ID, note, err)
	}
	// Asked for by id, it is what was asked for.
	if s, _, err := pickSnapshot("blog", snaps, "cccc2222", nil); err != nil || s.ID != "cccc2222dddd" {
		t.Errorf("--snapshot overrides, got %v %v", s.ID, err)
	}
	// No record at all: nothing vouches for any of them, so the choice
	// is the operator's, and the error names the newest to start from.
	if _, _, err := pickSnapshot("blog", snaps, "", nil); err == nil || !strings.Contains(err.Error(), "--snapshot") || !strings.Contains(err.Error(), "cccc2222") {
		t.Errorf("with no clean record a default must be refused, naming --snapshot and the newest, got %v", err)
	}
}

func TestConfirmRestoreWantsTheAppsName(t *testing.T) {
	if err := confirmRestore("blog", nil); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("without a terminal a restore must refuse and name --yes, got %v", err)
	}
	answer := func(s string) Prompter { return func(string, bool) (string, error) { return s, nil } }
	if err := confirmRestore("blog", answer("y")); err == nil {
		t.Error("a y must not confirm a restore")
	}
	if err := confirmRestore("blog", answer("blog")); err != nil {
		t.Errorf("typing the name confirms: %v", err)
	}
}

// A restore runs as the app's backup job does — the same unit name,
// so the two can never overlap, and the same sandbox — with two
// differences, both deliberate: the app's data is writable, and a
// repository on this box is read-only.
func TestRestoreRunsInTheBackupJobsUnitAndSandbox(t *testing.T) {
	app := App{Name: "blog", Shared: "/var/lib/liveswap/blog/shared", State: []StateEntry{{Kind: KindFiles, Path: "uploads"}}}
	o := LaunchOptions{Self: "/usr/bin/hotserve", StagingRoot: "/var/lib/hotserve-backup", EnvFile: "/etc/hotserve/backup.env", User: "hotserve", RepositoryPath: "/srv/restic"}
	backup, restore := LaunchArgs(app, o), RestoreArgs(app, o, "s1full", false)
	props := func(args []string) []string {
		var out []string
		for _, a := range args {
			if strings.HasPrefix(a, "--property=") || strings.HasPrefix(a, "--unit=") {
				out = append(out, a)
			}
		}
		return out
	}
	staging := o.StagingRoot + "/blog"
	swap := map[string]string{
		"--property=BindReadOnlyPaths=" + app.Shared: "--property=BindPaths=" + app.Shared,
		"--property=BindPaths=" + o.RepositoryPath:   "--property=BindReadOnlyPaths=" + o.RepositoryPath,
		// Its own part of staging, not the backup's bookkeeping.
		"--property=BindPaths=" + staging:                                "--property=BindPaths=" + StagingRestore(staging),
		"--property=Environment=HOME=" + staging:                         "--property=Environment=HOME=" + StagingRestore(staging),
		"--property=Environment=XDG_CACHE_HOME=" + StagingCache(staging): "--property=Environment=XDG_CACHE_HOME=" + StagingCache(StagingRestore(staging)),
	}
	var want []string
	for _, p := range props(backup) {
		if s, ok := swap[p]; ok {
			p = s
		}
		want = append(want, p)
	}
	if got := props(restore); !slices.Equal(got, want) {
		t.Errorf("restore unit differs from the backup's by more than its binds and HOME:\n got %v\nwant %v", got, want)
	}
	if !slices.Contains(restore, "--pipe") {
		t.Error("the operator is at the terminal: the restore's output must reach it")
	}
	if cmd := jobCommand(restore); !slices.Equal(cmd[:3], []string{o.Self, "backup", "restore-app"}) || !slices.Contains(cmd, "--snapshot=s1full") || !slices.Contains(cmd, "--no-lock") {
		t.Errorf("restore unit runs %v", cmd)
	}
}
