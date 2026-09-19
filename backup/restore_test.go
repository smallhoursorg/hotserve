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
	// integrity is what `PRAGMA integrity_check` answers, and
	// integrityErr its exit status.
	integrity    string
	integrityErr error
	// treeStderr is what `restic restore` of a directory writes to
	// stderr; treeErr is its exit status.
	treeStderr string
	treeErr    error
	// fail is the exit status of a command that only runs, by name.
	fail map[string]error
}

func newRestore(t *testing.T, dbs, files []string) *restoreFixture {
	t.Helper()
	f := &restoreFixture{integrity: "ok"}
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
	// One Exec, answering each command the restore runs the way the
	// real one does: a listing and an integrity check on stdout, a dumped
	// file's bytes on stdout, a directory restore's errors on stderr.
	f.job.Exec = func(_ context.Context, c Cmd) error {
		f.calls = append(f.calls, call{c.Name, c.Args})
		switch {
		case c.Name == "sqlite3" && c.Stdout != nil:
			_, _ = c.Stdout.Write([]byte(f.integrity + "\n"))
			return f.integrityErr
		case c.Name == "restic" && c.Args[0] == "ls":
			for _, dir := range c.Args[slices.Index(c.Args, "s1full")+1:] {
				for p, node := range f.held {
					if filepath.Dir(p) == dir && node != "" {
						_, _ = c.Stdout.Write([]byte(node + "\n"))
					}
				}
			}
			return nil
		case c.Name == "restic" && c.Args[0] == "dump":
			_, err := c.Stdout.Write([]byte("from the snapshot"))
			return err
		case c.Name == "restic" && c.Args[0] == "restore":
			_, _ = c.Stderr.Write([]byte(f.treeStderr))
			return f.treeErr
		}
		return f.fail[c.Name]
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
	if got := w[1].args; !slices.Equal(got, []string{"restore", "--json", "--quiet", "s1full:" + uploads, "--target", uploads}) {
		t.Errorf("files go back as the snapshot's copy of that one directory, without --delete unless asked, got %v", got)
	}
	if !slices.ContainsFunc(f.calls, func(c call) bool {
		return c.name == "restic" && slices.Equal(c.args, []string{"dump", "s1full", filepath.Join(StagingData(f.job.Staging), "app.db")})
	}) {
		t.Errorf("the database comes out of the snapshot from the copy the backup took, got %v", f.calls)
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
		// A page sqlite3 cannot read at all: it says what it can on
		// stdout, "malformed" on stderr, and exits 1 (3.46, measured).
		"sqlite3 exits 1 over a malformed copy": func(f *restoreFixture) {
			f.integrity = "*** in database main ***\nTree 2 page 2: btreeInitPage() returns error code 11"
			f.integrityErr = exited(1)
		},
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
	f.treeErr = errors.New("exit status 1")
	err := f.job.Execute(context.Background())
	if err == nil || !strings.Contains(err.Error(), "may be partly restored") || !strings.Contains(err.Error(), "run the restore again") {
		t.Errorf("a directory that failed part-way must be called partly restored, got %v", err)
	}
	f = newRestore(t, []string{"app.db"}, nil)
	f.fail = map[string]error{"sqlite3": errors.New("exit status 1")}
	if err := f.job.Execute(context.Background()); err == nil || !strings.Contains(err.Error(), "it is as it was") {
		t.Errorf("a failed .restore leaves the database as it was, and must say so, got %v", err)
	}
}

// restic 0.18.0's own stderr, captured on Debian 13 from `restic restore
// --json` run as a user whose namespace maps only itself, against a
// snapshot whose files another uid owned.
const (
	// Every file restored; each one's recorded owner refused.
	resticOwnerRefusals = `{"message_type":"error","error":{"message":"lchown /data/uploads/a.txt: invalid argument"},"during":"restore","item":"/a.txt"}
{"message_type":"error","error":{"message":"lchown /data/uploads/sub/b.txt: invalid argument"},"during":"restore","item":"/sub/b.txt"}
{"message_type":"error","error":{"message":"lchown /data/uploads/sub: invalid argument"},"during":"restore","item":"/sub"}
{"message_type":"exit_error","code":1,"message":"Fatal: There were 3 errors\n"}
`
	// The same, with sub/ unwritable: b.txt was never written. restic
	// reports that as a chmod and an lchown of a file that is not there
	// — and its summary still counted the file as restored.
	resticOwnerRefusalsAndALostFile = `{"message_type":"error","error":{"message":"chmod /data/uploads/sub/b.txt: no such file or directory"},"during":"restore","item":"/sub/b.txt"}
{"message_type":"error","error":{"message":"lchown /data/uploads/a.txt: invalid argument"},"during":"restore","item":"/a.txt"}
{"message_type":"error","error":{"message":"lchown /data/uploads/sub/b.txt: no such file or directory"},"during":"restore","item":"/sub/b.txt"}
{"message_type":"error","error":{"message":"lchown /data/uploads/sub: invalid argument"},"during":"restore","item":"/sub"}
{"message_type":"exit_error","code":1,"message":"Fatal: There were 4 errors\n"}
`
)

// Owner refusals alone are a restore that worked. Anything beside them
// — an lchown that failed another way included — is one that did not,
// and so is a count restic gives that this reading does not reach.
func TestReadRestoreErrorsToleratesOwnerRefusalsAndNothingElse(t *testing.T) {
	for name, tc := range map[string]struct {
		stderr    string
		ownerOnly bool
		shown     string
	}{
		"owner refusals only":    {resticOwnerRefusals, true, ""},
		"a file was not written": {resticOwnerRefusalsAndALostFile, false, "lchown /data/uploads/sub/b.txt: no such file or directory"},
		"restic counted more than were read": {
			strings.Replace(resticOwnerRefusals, "There were 3 errors", "There were 4 errors", 1), false, ""},
		"no closing count": {
			strings.SplitAfterN(resticOwnerRefusals, "\n", 4)[0], false, ""},
		"another fatal": {
			resticOwnerRefusals + `{"message_type":"exit_error","code":1,"message":"Fatal: unable to load index"}` + "\n", false, "unable to load index"},
		"an error outside the restore": {
			strings.Replace(resticOwnerRefusals, `"during":"restore","item":"/a.txt"`, `"during":"verify","item":"/a.txt"`, 1), false, "lchown /data/uploads/a.txt"},
		"not permitted is not this case": {
			strings.ReplaceAll(resticOwnerRefusals, "invalid argument", "operation not permitted"), false, "operation not permitted"},
		"a line that is not JSON": {
			"unable to open cache\n" + resticOwnerRefusals, false, "unable to open cache"},
		"nothing at all": {"", false, ""},
	} {
		t.Run(name, func(t *testing.T) {
			var shown strings.Builder
			errs := readRestoreErrors(strings.NewReader(tc.stderr), &shown)
			if errs.ownerOnly() != tc.ownerOnly {
				t.Errorf("ownerOnly = %v, want %v (%+v)", errs.ownerOnly(), tc.ownerOnly, errs)
			}
			if tc.shown != "" && !strings.Contains(shown.String(), tc.shown) {
				t.Errorf("an error that is not an owner refusal must be shown as it arrives; want %q in %q", tc.shown, shown.String())
			}
			if tc.ownerOnly && shown.String() != "" {
				t.Errorf("owner refusals are counted, never printed one per file: %q", shown.String())
			}
		})
	}
}

// A rebuilt box: restic exits 1 over owners alone. That directory is
// restored, the next declared one still gets its turn, and the log says
// whose the files are.
func TestRestoreCarriesOnPastOwnerRefusals(t *testing.T) {
	f := newRestore(t, nil, []string{"uploads", "avatars"})
	var log strings.Builder
	f.job.Log = &log
	f.treeStderr = resticOwnerRefusals
	f.treeErr = errors.New("exit status 1")
	if err := f.job.Execute(context.Background()); err != nil {
		t.Fatalf("owner refusals alone must not fail a restore: %v", err)
	}
	if w := f.writes(); len(w) != 2 {
		t.Errorf("both declared paths must be restored, got %v", w)
	}
	if !strings.Contains(log.String(), "3 files and directories belong to this box's user") {
		t.Errorf("the log must say whose the files are now:\n%s", log.String())
	}
}

// The same exit status with a file that was never written is a failure,
// and says what restic said.
func TestRestoreFailsWhenMoreThanOwnersWentWrong(t *testing.T) {
	f := newRestore(t, nil, []string{"uploads", "avatars"})
	f.treeStderr = resticOwnerRefusalsAndALostFile
	f.treeErr = errors.New("exit status 1")
	err := f.job.Execute(context.Background())
	if err == nil || !strings.Contains(err.Error(), "may be partly restored") || !strings.Contains(err.Error(), "no such file or directory") {
		t.Fatalf("want a part-way failure quoting restic, got %v", err)
	}
	if w := f.writes(); len(w) != 1 {
		t.Errorf("a failed directory stops the restore, got %v", w)
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
	// Asked for by id, it is what was asked for — and said to be
	// unvouched before the operator confirms, --delete above all.
	if s, note, err := pickSnapshot("blog", snaps, "cccc2222", nil); err != nil || s.ID != "cccc2222dddd" || !strings.Contains(note, "No clean run vouches for snapshot cccc2222") || !strings.Contains(note, "--delete") {
		t.Errorf("--snapshot overrides, with a note that nothing vouches for it, got %v %q %v", s.ID, note, err)
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
// differences, both deliberate: the app's data is writable, and its
// HOME is its own part of staging, not the backup's.
func TestRestoreRunsInTheBackupJobsUnitAndSandbox(t *testing.T) {
	app := App{Name: "blog", Shared: "/var/lib/liveswap/blog/shared", State: []StateEntry{{Kind: KindFiles, Path: "uploads"}}}
	o := LaunchOptions{Self: "/usr/bin/hotserve", StagingRoot: "/var/lib/hotserve-backup", EnvFile: "/etc/hotserve/backup.env", User: "hotserve"}
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
		"--property=BindReadOnlyPaths=-" + app.Shared: "--property=BindPaths=-" + app.Shared,
		// Its own part of staging, not the backup's bookkeeping: systemd
		// makes that dir and puts only it in the view.
		"--property=StateDirectory=hotserve-backup/blog":                 "--property=StateDirectory=hotserve-backup/blog/restore",
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
	if cmd := jobCommand(restore); !slices.Equal(cmd[:3], []string{o.Self, "backup", "restore-app"}) || !slices.Contains(cmd, "--snapshot=s1full") || slices.Contains(cmd, "--no-lock") {
		t.Errorf("restore unit runs %v", cmd)
	}
}

// What the operator reads before typing the app's name: which snapshot,
// from when and where, and what happens to each declared path — DELETED
// only when it was asked for.
func TestDescribeRestoreSaysWhatWillHappen(t *testing.T) {
	app := App{Name: "blog", State: []StateEntry{
		{Kind: KindSQLite, Path: "app.db"},
		{Kind: KindFiles, Path: "uploads/"},
	}}
	now := time.Date(2026, 9, 18, 15, 0, 0, 0, time.UTC)
	snap := Snapshot{ShortID: "a1b2c3d4", Hostname: "box-1", Time: now.Add(-3 * time.Hour)}

	var kept strings.Builder
	DescribeRestore(&kept, app, snap, false, now)
	for _, want := range []string{"Restoring blog from snapshot a1b2c3d4", "3 hours ago", "on box-1", "app.db", "replaced by the snapshot's copy", "uploads ", "files added since are kept", "is left as it is"} {
		if !strings.Contains(kept.String(), want) {
			t.Errorf("missing %q in:\n%s", want, kept.String())
		}
	}
	if strings.Contains(kept.String(), "DELETED") {
		t.Errorf("nothing is deleted unless asked:\n%s", kept.String())
	}

	var deleted strings.Builder
	DescribeRestore(&deleted, app, snap, true, now)
	if !strings.Contains(deleted.String(), "files added since are DELETED") {
		t.Errorf("--delete must be said in capitals before it is confirmed:\n%s", deleted.String())
	}
}

// A snapshot holding none of what the app declares — taken under another
// liveswap root, say, since a snapshot keeps absolute paths — restores
// nothing, and "restored" would be the one wrong thing to say.
func TestRestoreOfNothingIsAFailure(t *testing.T) {
	f := newRestore(t, nil, []string{"uploads", "avatars"})
	for p := range f.held {
		delete(f.held, p)
	}
	err := f.job.Execute(context.Background())
	if err == nil || !strings.Contains(err.Error(), "holds none of the paths") || !strings.Contains(err.Error(), "nothing was restored") {
		t.Fatalf("want a failure that says nothing was restored, got %v", err)
	}
	if w := f.writes(); len(w) != 0 {
		t.Errorf("nothing should have been written: %v", w)
	}
}

// `backup snapshots <app>`: newest first, each with whether a clean run
// vouches for it — the one thing restic's own listing cannot show, and the
// thing to know before giving one to --snapshot.
func TestFormatSnapshotsMarksTheOnesACleanRunVouchesFor(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 9, 18, h, 0, 0, 0, time.UTC) }
	snaps := []Snapshot{
		{ID: "aaaa1111bbbb", ShortID: "aaaa1111", Time: at(3), Hostname: "box-1"},
		{ID: "cccc2222dddd", ShortID: "cccc2222", Time: at(5), Hostname: "box-1"},
	}
	var out strings.Builder
	FormatSnapshots(&out, "blog", snaps, map[string]bool{"aaaa1111bbbb": true}, at(6))
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) < 3 || !strings.HasPrefix(lines[1], "cccc2222") || !strings.HasPrefix(lines[2], "aaaa1111") {
		t.Fatalf("want a header, then the newest first:\n%s", out.String())
	}
	if !strings.Contains(lines[1], "no — it may be missing files") || !strings.HasSuffix(strings.TrimSpace(lines[2]), "yes") {
		t.Errorf("want the 05:00 snapshot marked as unvouched and the 03:00 one as clean:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "box-1") || !strings.Contains(out.String(), "backup restore blog --snapshot") {
		t.Errorf("want the box each came from, and how to restore one:\n%s", out.String())
	}
}

// restore and snapshots read one listing, so they cannot disagree about
// which snapshots are clean.
func TestAppSnapshotsSplitsBackupsFromTheirCleanRecords(t *testing.T) {
	listing := []Snapshot{
		{ID: "s1", ShortID: "s1", Tags: []string{"hotserve", "app:blog"}},
		{ID: "s2", ShortID: "s2", Tags: []string{"hotserve", "app:blog"}},
		{ID: "r1", ShortID: "r1", Tags: []string{CleanTag, cleanAppTag("blog"), cleanOfTag("s1")}},
		{ID: "r9", ShortID: "r9", Tags: []string{CleanTag, cleanAppTag("shop"), cleanOfTag("s2")}},
	}
	body, err := json.Marshal(listing)
	if err != nil {
		t.Fatal(err)
	}
	x := fake(nil, func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if !slices.Contains(args, "hotserve,app:blog") || !slices.Contains(args, CleanTag) {
			t.Errorf("want this app's backups and every clean-run record asked for: %v", args)
		}
		return body, nil
	})
	snaps, clean, err := appSnapshots(context.Background(), x, "blog")
	if err != nil || len(snaps) != 2 {
		t.Fatalf("want the two backups and not the records: %v, %v", snaps, err)
	}
	if !clean["s1"] || clean["s2"] {
		t.Errorf("s1 is vouched for by blog's record; s2 only by another app's: %v", clean)
	}
}
