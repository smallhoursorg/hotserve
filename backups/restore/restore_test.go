package restore

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// fetched is a snapshot as it lies on disk after the fetch and the
// hand-over, and target an app's shared dir beside a sibling's.
type dirs struct{ staged, target, sibling string }

func newDirs(t *testing.T, plan string, files map[string]string) dirs {
	t.Helper()
	base := t.TempDir()
	d := dirs{filepath.Join(base, "restore"), filepath.Join(base, "blog", "shared"), filepath.Join(base, "shop", "shared")}
	for _, dir := range []string{d.staged, d.target, d.sibling} {
		must(t, os.MkdirAll(dir, 0o755))
	}
	if plan != "" {
		must(t, os.WriteFile(filepath.Join(d.staged, "plan.json"), []byte(plan), 0o644))
	}
	for name, body := range files {
		write(t, filepath.Join(d.staged, name), body)
	}
	return d
}

func write(t *testing.T, file, body string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(file), 0o755))
	must(t, os.WriteFile(file, []byte(body), 0o640))
}

func read(t *testing.T, file string) string {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(raw)
}

func classes(a Answer) string {
	var out []string
	for _, it := range a.Items {
		out = append(out, it.Kind+" "+it.Path+": "+string(it.Class))
	}
	return strings.Join(out, ", ")
}

const uploadsPlan = `{"files":["uploads"]}`

func TestFilesGoInAndWhatTheSnapshotDoesNotHoldIsLeftAndListed(t *testing.T) {
	d := newDirs(t, uploadsPlan, map[string]string{"files/uploads/a.png": "img", "files/uploads/deep/er/b.png": "img2"})
	write(t, filepath.Join(d.target, "uploads", "a.png"), "overwritten since")
	write(t, filepath.Join(d.target, "uploads", "new.png"), "later")
	write(t, filepath.Join(d.target, "uploads", "newdir", "c.png"), "later")
	write(t, filepath.Join(d.target, "private.txt"), "undeclared")

	a := Run(context.Background(), d.staged, d.target, AllOrNothing)
	if a.Error != "" || classes(a) != "files uploads: ok" {
		t.Fatalf("%+v", a)
	}
	if got := read(t, filepath.Join(d.target, "uploads", "a.png")); got != "img" {
		t.Errorf("a.png: %q", got)
	}
	if got := read(t, filepath.Join(d.target, "uploads", "deep", "er", "b.png")); got != "img2" {
		t.Errorf("deep/er/b.png: %q", got)
	}
	if st, err := os.Stat(filepath.Join(d.target, "uploads", "a.png")); err != nil || st.Mode().Perm() != 0o640 {
		t.Errorf("a.png's mode: %v, %v", st, err)
	}
	// Nothing is removed, and what was not declared is not looked at.
	if read(t, filepath.Join(d.target, "uploads", "new.png")) != "later" || read(t, filepath.Join(d.target, "private.txt")) != "undeclared" {
		t.Error("a restore removed or changed something the snapshot does not hold")
	}
	// A directory the snapshot does not hold is named, not listed.
	if got := strings.Join(a.Left, " "); got != "uploads/new.png uploads/newdir" {
		t.Errorf("left: %q", got)
	}
	left, _ := filepath.Glob(filepath.Join(d.target, "uploads", tmpPrefix+"*"))
	if len(left) != 0 {
		t.Errorf("a file the restore was writing is still there: %v", left)
	}
}

func TestALinkWhereADirectoryGoesIsRefusedAndNothingIsWritten(t *testing.T) {
	for name, plant := range map[string]func(t *testing.T, d dirs){
		"the declared directory": func(t *testing.T, d dirs) { must(t, os.Symlink(d.sibling, filepath.Join(d.target, "uploads"))) },
		"a directory inside it": func(t *testing.T, d dirs) {
			must(t, os.MkdirAll(filepath.Join(d.target, "uploads"), 0o755))
			must(t, os.Symlink(d.sibling, filepath.Join(d.target, "uploads", "deep")))
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := newDirs(t, uploadsPlan, map[string]string{"files/uploads/a.png": "img", "files/uploads/deep/b.png": "img2"})
			plant(t, d)
			a := Run(context.Background(), d.staged, d.target, AllOrNothing)
			if len(a.Items) != 1 || a.Items[0].Class != Refused || !strings.Contains(a.Items[0].Detail, "symbolic link") {
				t.Fatalf("%+v", a)
			}
			if left, _ := os.ReadDir(d.sibling); len(left) != 0 {
				t.Fatalf("the sibling's data dir now holds %v", left)
			}
			if _, err := os.Lstat(filepath.Join(d.target, "uploads", "a.png")); err == nil && name != "the declared directory" {
				t.Error("every check comes before any change: a.png was installed although deep/ is refused")
			}
		})
	}
}

func TestALinkWhereAFileGoesIsReplacedNeverWrittenThrough(t *testing.T) {
	d := newDirs(t, uploadsPlan, map[string]string{"files/uploads/a.png": "img"})
	write(t, filepath.Join(d.sibling, "r.txt"), "the sibling's")
	must(t, os.MkdirAll(filepath.Join(d.target, "uploads"), 0o755))
	must(t, os.Symlink(filepath.Join(d.sibling, "r.txt"), filepath.Join(d.target, "uploads", "a.png")))

	a := Run(context.Background(), d.staged, d.target, AllOrNothing)
	if classes(a) != "files uploads: ok" {
		t.Fatalf("%+v", a)
	}
	if got := read(t, filepath.Join(d.sibling, "r.txt")); got != "the sibling's" {
		t.Fatalf("the sibling's file was written through the link: %q", got)
	}
	st, err := os.Lstat(filepath.Join(d.target, "uploads", "a.png"))
	if err != nil || !st.Mode().IsRegular() || read(t, filepath.Join(d.target, "uploads", "a.png")) != "img" {
		t.Fatalf("a.png: %v, %v", st, err)
	}
}

// A FIFO, a socket or a device cannot be app data and cannot be put
// back: each is listed and left out. A symbolic link is neither — see
// TestSymlinksAreRestored.
func TestWhatCannotBeDataIsListedNeverInstalled(t *testing.T) {
	d := newDirs(t, uploadsPlan, map[string]string{"files/uploads/a.png": "img"})
	must(t, syscall.Mkfifo(filepath.Join(d.staged, "files", "uploads", "trap.fifo"), 0o644))
	must(t, unix.Mknod(filepath.Join(d.staged, "files", "uploads", "sock"), unix.S_IFSOCK|0o644, 0))
	for _, mode := range []Mode{CheckOnly, AllOrNothing} {
		a := Run(context.Background(), d.staged, d.target, mode)
		if classes(a) != "files uploads: ok" {
			t.Fatalf("%+v", a)
		}
		if len(a.Skipped) != 2 || a.Skipped[0] != (Skipped{"uploads/sock", "a socket"}) || a.Skipped[1] != (Skipped{"uploads/trap.fifo", "a fifo"}) {
			t.Fatalf("skipped: %+v", a.Skipped)
		}
	}
	for _, name := range []string{"trap.fifo", "sock"} {
		if _, err := os.Lstat(filepath.Join(d.target, "uploads", name)); err == nil {
			t.Errorf("%s was installed", name)
		}
	}
	if read(t, filepath.Join(d.target, "uploads", "a.png")) != "img" {
		t.Error("the file beside them was not installed")
	}
}

// A symbolic link inside a declared path — uploads/current -> releases/3
// — comes back as the same link, its text stored and never followed. A
// link at the declared path itself comes back too.
func TestSymlinksAreRestored(t *testing.T) {
	d := newDirs(t, `{"files":["uploads","current"]}`, map[string]string{"files/uploads/rel/3/a.png": "img"})
	must(t, os.Symlink("rel/3", filepath.Join(d.staged, "files", "uploads", "current")))
	must(t, os.Symlink("/etc/passwd", filepath.Join(d.staged, "files", "uploads", "abs")))
	must(t, os.Symlink("uploads/rel/3", filepath.Join(d.staged, "files", "current")))
	// In place: a link is replaced, a file under a link's name is replaced.
	must(t, os.MkdirAll(filepath.Join(d.target, "uploads"), 0o755))
	must(t, os.Symlink("somewhere/else", filepath.Join(d.target, "uploads", "current")))
	a := Run(context.Background(), d.staged, d.target, AllOrNothing)
	if classes(a) != "files uploads: ok, files current: ok" {
		t.Fatalf("%+v", a)
	}
	for name, want := range map[string]string{"uploads/current": "rel/3", "uploads/abs": "/etc/passwd", "current": "uploads/rel/3"} {
		got, err := os.Readlink(filepath.Join(d.target, name))
		if err != nil || got != want {
			t.Errorf("%s -> %q, want %q (%v)", name, got, want, err)
		}
	}
	if read(t, filepath.Join(d.target, "uploads", "rel", "3", "a.png")) != "img" {
		t.Error("the file the link points at was not restored")
	}
	// A directory where a link goes is refused, not removed.
	d = newDirs(t, `{"files":["uploads"]}`, map[string]string{"files/uploads/a.png": "img"})
	must(t, os.Symlink("x", filepath.Join(d.staged, "files", "uploads", "link")))
	must(t, os.MkdirAll(filepath.Join(d.target, "uploads", "link"), 0o755))
	if a := Run(context.Background(), d.staged, d.target, AllOrNothing); a.Items[0].Class != Refused {
		t.Fatalf("a directory where a link goes: %+v", a)
	}
}

func TestAPlanThatIsNotADeclarationIsAnErrorAndNothingIsInstalled(t *testing.T) {
	for name, plan := range map[string]string{
		"none":             "",
		"not json":         "not json",
		"an unknown field": `{"files":["uploads"],"install_to":"/etc"}`,
		"leaves the app":   `{"files":["../../shop/shared"]}`,
		"absolute":         `{"files":["/etc"]}`,
		"declares nothing": `{}`,
		"something after":  `{"files":["uploads"]} {"files":["/etc"]}`,
		"garbage after":    `{"files":["uploads"]} not json`,
	} {
		t.Run(name, func(t *testing.T) {
			d := newDirs(t, plan, map[string]string{"files/uploads/a.png": "img"})
			a := Run(context.Background(), d.staged, d.target, WhatIsSound)
			if a.Error == "" || !strings.Contains(a.Error, "plan.json") || len(a.Items) != 0 {
				t.Fatalf("%+v", a)
			}
			if name == "an unknown field" && !strings.Contains(a.Error, "install_to") {
				t.Errorf("the error does not name the field: %s", a.Error)
			}
			if left, _ := os.ReadDir(d.target); len(left) != 0 {
				t.Fatalf("installed: %v", left)
			}
		})
	}
	// Not a file a declaration would be: a link, and a FIFO, which must
	// not be waited on.
	d := newDirs(t, "", nil)
	must(t, os.Symlink("/etc/passwd", filepath.Join(d.staged, "plan.json")))
	if a := Run(context.Background(), d.staged, d.target, CheckOnly); a.Error == "" {
		t.Fatalf("a plan.json that is a link: %+v", a)
	}
	d = newDirs(t, "", nil)
	must(t, syscall.Mkfifo(filepath.Join(d.staged, "plan.json"), 0o644))
	if a := Run(context.Background(), d.staged, d.target, CheckOnly); a.Error == "" {
		t.Fatalf("a plan.json that is a fifo: %+v", a)
	}
}

func TestEveryCheckComesBeforeAnyChange(t *testing.T) {
	const plan = `{"files":["avatars","uploads"]}`
	files := map[string]string{"files/uploads/a.png": "img"} // no avatars
	d := newDirs(t, plan, files)
	a := Run(context.Background(), d.staged, d.target, AllOrNothing)
	if classes(a) != "files avatars: missing, files uploads: held back" {
		t.Fatalf("%s", classes(a))
	}
	if left, _ := os.ReadDir(d.target); len(left) != 0 {
		t.Fatalf("installed although the snapshot lacks something it declares: %v", left)
	}
	// Into a directory of its own, what is sound lands.
	d = newDirs(t, plan, files)
	a = Run(context.Background(), d.staged, d.target, WhatIsSound)
	if classes(a) != "files avatars: missing, files uploads: ok" || read(t, filepath.Join(d.target, "uploads", "a.png")) != "img" {
		t.Fatalf("%s", classes(a))
	}
	// And a drill never opens a target at all.
	d = newDirs(t, plan, files)
	a = Run(context.Background(), d.staged, "/nonexistent/target", CheckOnly)
	if a.Error != "" || classes(a) != "files avatars: missing, files uploads: ok" {
		t.Fatalf("%+v", a)
	}
}

// Snapshots made before the engine excluded them hold an empty, mode-0
// stand-in where a live database inside a files path was masked.
func TestADeclaredDatabaseIsNeverInstalledFromFiles(t *testing.T) {
	d := newDirs(t, `{"sqlite":["data/shop.db"],"files":["."]}`, map[string]string{
		"files/r.txt": "receipt", "files/data/shop.db": "", "files/data/shop.db-wal": "", "files/data/other.txt": "x",
	})
	write(t, filepath.Join(d.target, "data", "shop.db"), "the live database")
	a := Run(context.Background(), d.staged, d.target, WhatIsSound)
	if classes(a) != "sqlite data/shop.db: missing, files .: ok" {
		t.Fatalf("%s", classes(a))
	}
	if got := read(t, filepath.Join(d.target, "data", "shop.db")); got != "the live database" {
		t.Fatalf("the stand-in under files/ was installed over the database: %q", got)
	}
	if _, err := os.Lstat(filepath.Join(d.target, "data", "shop.db-wal")); err == nil {
		t.Error("a sidecar's stand-in was installed")
	}
	if read(t, filepath.Join(d.target, "data", "other.txt")) != "x" || read(t, filepath.Join(d.target, "r.txt")) != "receipt" {
		t.Error("the files beside it were not installed")
	}
	for _, l := range a.Left {
		if strings.Contains(l, "shop.db") {
			t.Errorf("the database is listed as left over: %v", a.Left)
		}
	}
}

func TestADeclaredFileIsInstalledAsAFile(t *testing.T) {
	d := newDirs(t, `{"files":["conf/site.toml"]}`, map[string]string{"files/conf/site.toml": "a = 1"})
	a := Run(context.Background(), d.staged, d.target, AllOrNothing)
	if classes(a) != "files conf/site.toml: ok" || read(t, filepath.Join(d.target, "conf", "site.toml")) != "a = 1" {
		t.Fatalf("%+v", a)
	}
	// A directory where the file goes is in the way, and is not removed.
	d = newDirs(t, `{"files":["conf/site.toml"]}`, map[string]string{"files/conf/site.toml": "a = 1"})
	must(t, os.MkdirAll(filepath.Join(d.target, "conf", "site.toml"), 0o755))
	if a := Run(context.Background(), d.staged, d.target, AllOrNothing); a.Items[0].Class != Refused {
		t.Fatalf("%+v", a)
	}
}

// The unit's umask cuts a mode given to mkdir; a directory the restore
// makes gets the snapshot's mode all the same — last, so that one closed
// to writing can still be filled.
func TestDirectoriesGetTheModesTheyHad(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	d := newDirs(t, uploadsPlan, map[string]string{"files/uploads/pub/b.png": "img", "files/uploads/ro/f": "x"})
	must(t, os.Chmod(filepath.Join(d.staged, "files", "uploads", "pub"), 0o755))
	must(t, os.Chmod(filepath.Join(d.staged, "files", "uploads", "pub", "b.png"), 0o644))
	must(t, os.Chmod(filepath.Join(d.staged, "files", "uploads", "ro"), 0o555))
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(d.staged, "files", "uploads", "ro"), 0o755)
		_ = os.Chmod(filepath.Join(d.target, "uploads", "ro"), 0o755)
	})
	a := Run(context.Background(), d.staged, d.target, AllOrNothing)
	if classes(a) != "files uploads: ok" || !a.Changed {
		t.Fatalf("%+v", a)
	}
	for name, want := range map[string]os.FileMode{"uploads/pub": 0o755, "uploads/pub/b.png": 0o644, "uploads/ro": 0o555} {
		if st, err := os.Stat(filepath.Join(d.target, name)); err != nil || st.Mode().Perm() != want {
			t.Errorf("%s: %v, want %o (%v)", name, st.Mode().Perm(), want, err)
		}
	}
	if read(t, filepath.Join(d.target, "uploads", "ro", "f")) != "x" {
		t.Error("the read-only directory was closed before it was filled")
	}
	// The staged tree is scratch a run opens as it reads it (ro became
	// 0755 above); a restore fetches afresh, so the fixture is set back.
	reset := func() { must(t, os.Chmod(filepath.Join(d.staged, "files", "uploads", "ro"), 0o555)) }
	reset()
	// A directory in place with another mode gets the snapshot's.
	must(t, os.Chmod(filepath.Join(d.target, "uploads", "pub"), 0o700))
	if a := Run(context.Background(), d.staged, d.target, AllOrNothing); classes(a) != "files uploads: ok" {
		t.Fatalf("over a directory with another mode: %+v", a)
	}
	if st, _ := os.Stat(filepath.Join(d.target, "uploads", "pub")); st.Mode().Perm() != 0o755 {
		t.Errorf("uploads/pub in place at 0700 was left so; the snapshot says 0755: %v", st.Mode().Perm())
	}
	// And again, over what is now in place: a directory that is closed to
	// writing is restored into all the same, and is closed again after.
	must(t, os.Chmod(filepath.Join(d.target, "uploads", "ro"), 0o755))
	must(t, os.WriteFile(filepath.Join(d.target, "uploads", "ro", "f"), []byte("changed since"), 0o644))
	must(t, os.Chmod(filepath.Join(d.target, "uploads", "ro"), 0o555))
	reset()
	if a := Run(context.Background(), d.staged, d.target, AllOrNothing); classes(a) != "files uploads: ok" {
		t.Fatalf("over a read-only directory in place: %+v", a)
	}
	if st, _ := os.Stat(filepath.Join(d.target, "uploads", "ro")); read(t, filepath.Join(d.target, "uploads", "ro", "f")) != "x" || st.Mode().Perm() != 0o555 {
		t.Errorf("uploads/ro after the second restore: %v", st.Mode().Perm())
	}
}

// A backup reads with a capability; a restore is the file's owner and no
// more. A file the snapshot holds mode 0 (or a directory closed to its
// owner) is read from the scratch copy all the same, and comes back with
// the mode it had. Root reads regardless of mode, so under root the test
// runs itself again as an unprivileged uid, over directories that uid
// can reach [the way the mode-0 file went unread in a real unit, once].
func TestAFileItsOwnerCannotReadIsRestoredWithItsMode(t *testing.T) {
	const asUID = 65534
	var d dirs
	if os.Getuid() == 0 {
		if dirsEnv := os.Getenv("RESTORE_TEST_DIRS"); dirsEnv == "" {
			base, err := os.MkdirTemp("/var/tmp", "restore-mode0-")
			must(t, err)
			t.Cleanup(func() { _ = os.RemoveAll(base) })
			must(t, os.Chmod(base, 0o755))
			must(t, os.Chown(base, asUID, asUID))
			// The test binary sits where only root can go; a copy the uid
			// can run.
			self, err := os.ReadFile(os.Args[0])
			must(t, err)
			must(t, os.WriteFile(filepath.Join(base, "restore.test"), self, 0o755))
			child := exec.Command(filepath.Join(base, "restore.test"), "-test.run=^TestAFileItsOwnerCannotReadIsRestoredWithItsMode$", "-test.v")
			child.Env = append(os.Environ(), "RESTORE_TEST_DIRS="+base)
			child.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: asUID, Gid: asUID}}
			out, err := child.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "--- PASS") {
				t.Fatalf("as uid %d: %v\n%s", asUID, err, out)
			}
			return
		}
	}
	if base := os.Getenv("RESTORE_TEST_DIRS"); base != "" {
		d = dirs{filepath.Join(base, "restore"), filepath.Join(base, "blog", "shared"), filepath.Join(base, "shop", "shared")}
		for _, dir := range []string{d.staged, d.target, d.sibling} {
			must(t, os.MkdirAll(dir, 0o755))
		}
		must(t, os.WriteFile(filepath.Join(d.staged, "plan.json"), []byte(`{"files":["uploads"]}`), 0o644))
		write(t, filepath.Join(d.staged, "files/uploads/zero"), "secret")
		write(t, filepath.Join(d.staged, "files/uploads/shut/f"), "deep")
		// ajar is written below, for both paths.
	} else {
		d = newDirs(t, `{"files":["uploads"]}`, map[string]string{"files/uploads/zero": "secret", "files/uploads/shut/f": "deep"})
	}
	write(t, filepath.Join(d.staged, "files/uploads/ajar/g"), "readable, not searchable")
	must(t, os.Chmod(filepath.Join(d.staged, "files", "uploads", "zero"), 0o000))
	must(t, os.Chmod(filepath.Join(d.staged, "files", "uploads", "shut"), 0o000))
	must(t, os.Chmod(filepath.Join(d.staged, "files", "uploads", "ajar"), 0o400)) // opens for reading; its children do not
	a := Run(context.Background(), d.staged, d.target, AllOrNothing)
	if classes(a) != "files uploads: ok" {
		t.Fatalf("%+v", a)
	}
	_ = os.Chmod(filepath.Join(d.target, "uploads", "ajar"), 0o700)
	if read(t, filepath.Join(d.target, "uploads", "ajar", "g")) != "readable, not searchable" {
		t.Errorf("a file under a 0400 directory was lost")
	}
	_ = os.Chmod(filepath.Join(d.target, "uploads", "zero"), 0o600) // to read it back
	_ = os.Chmod(filepath.Join(d.target, "uploads", "shut"), 0o700)
	if read(t, filepath.Join(d.target, "uploads", "zero")) != "secret" {
		t.Errorf("a mode-0 file's contents were lost")
	}
	if read(t, filepath.Join(d.target, "uploads", "shut", "f")) != "deep" {
		t.Errorf("a file under a mode-0 directory was lost")
	}
	must(t, os.Chmod(filepath.Join(d.target, "uploads", "zero"), 0o000))
	must(t, os.Chmod(filepath.Join(d.target, "uploads", "shut"), 0o000))
	for _, name := range []string{"uploads/zero", "uploads/shut"} {
		if st, err := os.Lstat(filepath.Join(d.target, name)); err != nil || st.Mode().Perm() != 0 {
			t.Errorf("%s came back mode %v (%v)", name, st.Mode().Perm(), err)
		}
	}
}

// A file with a -wal beside it in place is a live database that the
// snapshot holds only as a file.
func TestAFileIsNeverRenamedOverALiveDatabase(t *testing.T) {
	d := newDirs(t, `{"files":["."]}`, map[string]string{"files/app.db": "a raw copy", "files/r.txt": "receipt"})
	write(t, filepath.Join(d.target, "app.db"), "the live database")
	write(t, filepath.Join(d.target, "app.db-wal"), "its log")
	a := Run(context.Background(), d.staged, d.target, AllOrNothing)
	if a.Items[0].Class != Refused || !strings.Contains(a.Items[0].Detail, "app.db") || a.Changed {
		t.Fatalf("%+v", a)
	}
	if read(t, filepath.Join(d.target, "app.db")) != "the live database" {
		t.Fatal("renamed over")
	}
}

// What a killed restore left half written under its temporary name is
// the app's from then on: listed as left in place, never removed, and
// restored like any file when a snapshot holds it — an app may call a
// file of its own anything, and a name is never a reason to remove one.
func TestWhatLooksLikeALeftoverIsTheAppsAllTheSame(t *testing.T) {
	const looksLikeOne = tmpPrefix + "0123456789abcdef"
	d := newDirs(t, uploadsPlan, map[string]string{"files/uploads/a.png": "img", "files/uploads/" + looksLikeOne: "in the snapshot"})
	write(t, filepath.Join(d.target, "uploads", looksLikeOne+"2"), "in place, not in the snapshot")
	a := Run(context.Background(), d.staged, d.target, AllOrNothing)
	if classes(a) != "files uploads: ok" {
		t.Fatalf("%+v", a)
	}
	if read(t, filepath.Join(d.target, "uploads", looksLikeOne)) != "in the snapshot" {
		t.Error("a file the snapshot holds was left out for its name")
	}
	if read(t, filepath.Join(d.target, "uploads", looksLikeOne+"2")) != "in place, not in the snapshot" || strings.Join(a.Left, " ") != "uploads/"+looksLikeOne+"2" {
		t.Errorf("a file in place was removed, or not listed, for its name: left %v", a.Left)
	}
}

// A declared path that is itself a FIFO is a snapshot a backup would
// not have made: refused, and listed with what is left out — never
// "restored", there being nothing of it to put back.
func TestADeclaredPathThatIsASpecialFileIsRefusedAndListed(t *testing.T) {
	d := newDirs(t, `{"files":["pipe","uploads"]}`, map[string]string{"files/uploads/a.png": "img"})
	must(t, syscall.Mkfifo(filepath.Join(d.staged, "files", "pipe"), 0o644))
	a := Run(context.Background(), d.staged, d.target, AllOrNothing)
	if classes(a) != "files pipe: refused, files uploads: held back" || len(a.Skipped) != 1 || a.Skipped[0].Path != "pipe" {
		t.Fatalf("%s; skipped %+v", classes(a), a.Skipped)
	}
	if _, err := os.Lstat(filepath.Join(d.target, "pipe")); err == nil {
		t.Error("a FIFO was installed")
	}
}

// A file in the snapshot where a declared database's directory has to
// be: caught at the checks, so that all-or-nothing stays so.
func TestAFileWhereADatabasesDirectoryMustBeIsRefused(t *testing.T) {
	d := newDirs(t, `{"sqlite":["data/app.db"],"files":["."]}`, map[string]string{"files/data": "a file, not a directory", "files/r.txt": "receipt", "sqlite/data/app.db": "SQLite format 3\x00…"})
	a := Run(context.Background(), d.staged, d.target, AllOrNothing)
	if len(a.Items) != 2 || a.Items[1].Class != Refused || !strings.Contains(a.Items[1].Detail, `"data"`) || a.Changed {
		t.Fatalf("%+v", a)
	}
	if left, _ := os.ReadDir(d.target); len(left) != 0 {
		t.Fatalf("installed: %v", left)
	}
}
