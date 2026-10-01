//go:build integration

package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/smallhoursorg/hotserve/backups/unit"
	"github.com/smallhoursorg/hotserve/liveswap/backupdecl"
)

// verify leans on one thing restic does: given a directory, `ls` lists
// it and its direct children, and no deeper. That is what keeps a
// listing a handful of lines however large the app is — it is written
// under /run, which is memory. If a later restic lists recursively by
// default, this is what says so.
func TestIntegrationResticLsOfADirectoryIsNotRecursive(t *testing.T) {
	const restic = "/usr/bin/restic"
	if _, err := os.Stat(restic); err != nil {
		t.Fatalf("%s is not installed in the integration image: %v", restic, err)
	}
	base := t.TempDir()
	deep := filepath.Join(base, "backup", "blog", "files", "uploads", "deep", "deeper")
	must(t, os.MkdirAll(deep, 0o755))
	for i := 0; i < 300; i++ {
		must(t, os.WriteFile(filepath.Join(deep, fmt.Sprintf("f%d", i)), []byte("x"), 0o644))
	}
	must(t, os.MkdirAll(filepath.Join(base, "backup", "blog", "sqlite"), 0o755))
	must(t, os.WriteFile(filepath.Join(base, "backup", "blog", "sqlite", "app.db"), []byte("db"), 0o644))

	// A local repository, as a measuring stick only: the product has none.
	env := append(os.Environ(), "RESTIC_PASSWORD=pw", "RESTIC_REPOSITORY="+filepath.Join(base, "repo"), "RESTIC_CACHE_DIR="+filepath.Join(base, "cache"))
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(restic, args...)
		cmd.Env, cmd.Dir = env, base
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("restic %v: %v", args, err)
		}
		return string(out)
	}
	run("init", "-q")
	summary := run("backup", "--quiet", "--json", "backup/blog")
	id := regexp.MustCompile(`"snapshot_id":"([0-9a-f]{64})"`).FindStringSubmatch(summary)
	if id == nil {
		t.Fatalf("no snapshot id in %q", summary)
	}
	root := "/" + strings.TrimPrefix(filepath.ToSlash(filepath.Join("backup", "blog")), "/")
	nodes := func(args ...string) int {
		return strings.Count(run(append([]string{"ls", "--json", "--no-lock", "--", id[1]}, args...)...), `"struct_type":"node"`)
	}
	everything := nodes()
	if everything < 300 {
		t.Fatalf("the whole snapshot lists %d nodes: the tree was not backed up", everything)
	}
	// As verify asks: the parents of the declared items.
	if got := nodes(root+"/files", root+"/sqlite"); got > 10 {
		t.Fatalf("listing two parents returned %d nodes of the snapshot's %d: restic ls recurses by default, and verify's listing is no longer small", got, everything)
	}
	if got := nodes(root); got > 10 {
		t.Fatalf("listing the app's root (what `files .` asks for) returned %d nodes of %d", got, everything)
	}
}

// verify holds a files item in the snapshot to the file that was given
// to the upload, and leans on two things for it [M63]: restic's listing
// says which file a node is — its inode, its uid, its gid — and what a
// bind mount shows is the bound file's own, so that what the pin's
// fstat says here is what restic says from inside the unit's view. A
// bare mount point in its place is another inode. If a later restic
// stops printing the inode, or prints another, every files item would
// be refused: this is what says so first.
func TestIntegrationResticLsSaysWhichFileANodeIs(t *testing.T) {
	const restic = "/usr/bin/restic"
	if _, err := os.Stat(restic); err != nil {
		t.Fatalf("%s is not installed in the integration image: %v", restic, err)
	}
	if os.Getuid() != 0 {
		t.Skip("a bind mount is root's to make")
	}
	base := t.TempDir()
	shared := filepath.Join(base, "shared")
	must(t, os.MkdirAll(filepath.Join(shared, "uploads"), 0o755))
	must(t, os.WriteFile(filepath.Join(shared, "uploads", "a.png"), []byte("pic"), 0o644))
	must(t, os.MkdirAll(filepath.Join(shared, "empty"), 0o755))
	// Not root's: what is given is told from a bare mount point by its
	// owner as well as by its inode.
	for _, p := range []string{"uploads", "uploads/a.png", "empty"} {
		must(t, os.Chown(filepath.Join(shared, p), 4242, 4243))
	}
	view := filepath.Join(base, "backup", "blog", "files")
	must(t, os.MkdirAll(view, 0o755))
	root, err := pinRoot(shared)
	must(t, err)
	defer root.close()
	given := map[string]identity{}
	for _, item := range []string{"uploads", "empty", "uploads/a.png"} {
		p, err := root.beneath(item)
		must(t, err)
		defer p.close()
		name := strings.ReplaceAll(item, "/", "-")
		unmount, err := p.mountAt(filepath.Join(view, name))
		must(t, err)
		defer unmount()
		given["/backup/blog/files/"+name], err = p.identity()
		must(t, err)
	}
	// And what the defect shows the upload: a directory of the item's
	// name that is no mount of anything.
	must(t, os.Mkdir(filepath.Join(view, "bare"), 0o700))

	env := append(os.Environ(), "RESTIC_PASSWORD=pw", "RESTIC_REPOSITORY="+filepath.Join(base, "repo"), "RESTIC_CACHE_DIR="+filepath.Join(base, "cache"))
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(restic, args...)
		cmd.Env, cmd.Dir = env, base
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("restic %v: %v", args, err)
		}
		return string(out)
	}
	run("init", "-q")
	id := regexp.MustCompile(`"snapshot_id":"([0-9a-f]{64})"`).FindStringSubmatch(run("backup", "--quiet", "--json", "backup/blog"))
	if id == nil {
		t.Fatal("no snapshot id")
	}
	listing := filepath.Join(base, "ls.json")
	must(t, os.WriteFile(listing, []byte(run("ls", "--json", "--no-lock", "--", id[1], "/backup/blog/files")), 0o600))
	wanted := map[string]bool{"/backup/blog/files/bare": true}
	for p := range given {
		wanted[p] = true
	}
	nodes, made, err := lsNodes(listing, wanted)
	must(t, err)
	// The snapshot's own line says when it was made: the time its
	// clean-run record is given.
	if made.IsZero() || time.Since(made) > time.Hour || time.Until(made) > time.Minute {
		t.Errorf("the listing says the snapshot was made %v", made)
	}
	for p, g := range given {
		n, ok := nodes[p]
		if !ok || n.Inode == nil {
			t.Fatalf("%s: the listing does not say which file it is: %+v (in it: %v)", p, n, ok)
		}
		if *n.Inode != g.inode || n.UID != g.uid || n.GID != g.gid || g.uid != 4242 || g.gid != 4243 {
			t.Errorf("%s: the listing says inode %d, owner %d:%d; what was bound is inode %d, owner %d:%d", p, *n.Inode, n.UID, n.GID, g.inode, g.uid, g.gid)
		}
	}
	// And what it is: verify holds a database copy to a file of more than
	// no bytes, and a files item to a file or a directory.
	for p, want := range map[string]lsNode{"/backup/blog/files/uploads": {Type: "dir"}, "/backup/blog/files/empty": {Type: "dir"}, "/backup/blog/files/uploads-a.png": {Type: "file", Size: 3}} {
		if n := nodes[p]; n.Type != want.Type || n.Size != want.Size {
			t.Errorf("%s: the listing says a %q of %d bytes, want a %q of %d", p, n.Type, n.Size, want.Type, want.Size)
		}
	}
	bare, ok := nodes["/backup/blog/files/bare"]
	if !ok || bare.Inode == nil {
		t.Fatalf("the bare directory: %+v (in the listing: %v)", bare, ok)
	}
	for p, g := range given {
		if *bare.Inode == g.inode && bare.UID == g.uid && bare.GID == g.gid {
			t.Errorf("a bare directory is told from %s by nothing: inode %d, owner %d:%d", p, *bare.Inode, bare.UID, bare.GID)
		}
	}
}

// A fetch's verdict leans on three things restic 0.18 does. Asked for
// `<id>:<path>` where the snapshot has no such path, it exits 1 — where
// `--include`, matching nothing, exits 0 having restored nothing, which
// is why a fetch never selects by pattern. Its --json output ends in a
// summary whose total_files and files_restored count every entry, the
// directories and special files too. And a field that is zero is not
// in the summary at all: a summary with no files_restored is a restore
// of nothing, not a summary to be read some other way.
func TestIntegrationResticRestoreSaysWhatItRestored(t *testing.T) {
	const restic = "/usr/bin/restic"
	if _, err := os.Stat(restic); err != nil {
		t.Fatalf("%s is not installed in the integration image: %v", restic, err)
	}
	base := t.TempDir()
	uploads := filepath.Join(base, "backup", "blog", "files", "uploads")
	must(t, os.MkdirAll(uploads, 0o755))
	must(t, os.MkdirAll(filepath.Join(base, "backup", "blog", "sqlite"), 0o755))
	must(t, os.WriteFile(filepath.Join(uploads, "a.png"), []byte("img"), 0o644))
	must(t, os.WriteFile(filepath.Join(base, "backup", "blog", "sqlite", "app.db"), []byte("db"), 0o644))
	must(t, os.Symlink("/etc/passwd", filepath.Join(uploads, "link")))
	const entries = 6 // files, uploads, a.png, link, sqlite, app.db

	env := append(os.Environ(), "RESTIC_PASSWORD=pw", "RESTIC_REPOSITORY="+filepath.Join(base, "repo"), "RESTIC_CACHE_DIR="+filepath.Join(base, "cache"))
	run := func(args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(restic, args...)
		cmd.Env, cmd.Dir = env, base
		out, err := cmd.Output()
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatalf("restic %v: %v", args, err)
		}
		return string(out), cmd.ProcessState.ExitCode()
	}
	run("init", "-q")
	summary, _ := run("backup", "--quiet", "--json", "backup/blog")
	id := regexp.MustCompile(`"snapshot_id":"([0-9a-f]{64})"`).FindStringSubmatch(summary)
	if id == nil {
		t.Fatalf("no snapshot id in %q", summary)
	}
	var said struct {
		MessageType   string `json:"message_type"`
		TotalFiles    *int   `json:"total_files"`
		FilesRestored *int   `json:"files_restored"`
	}
	last := func(out string) {
		t.Helper()
		lines := strings.Split(strings.TrimSpace(out), "\n")
		said.MessageType, said.TotalFiles, said.FilesRestored = "", nil, nil
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &said); err != nil {
			t.Fatalf("the last line of %q: %v", out, err)
		}
	}

	out, exit := run("restore", "--quiet", "--json", "--no-lock", id[1]+":/backup/blog", "--target", filepath.Join(base, "there"))
	last(out)
	if exit != 0 || said.MessageType != "summary" || said.TotalFiles == nil || said.FilesRestored == nil || *said.TotalFiles != entries || *said.FilesRestored != entries {
		t.Fatalf("a restore of %d entries exited %d and said %q", entries, exit, out)
	}
	if _, exit = run("restore", "--quiet", "--json", "--no-lock", id[1]+":/backup/ghost", "--target", filepath.Join(base, "absent")); exit != 1 {
		t.Fatalf("a path the snapshot does not hold: exit %d, want 1", exit)
	}
	out, exit = run("restore", "--quiet", "--json", "--no-lock", id[1], "--include", "/nothing", "--target", filepath.Join(base, "nothing"))
	last(out)
	if exit != 0 || said.MessageType != "summary" || said.FilesRestored != nil {
		t.Fatalf("a restore of nothing exited %d and said %q: a zero count is no longer left out, or nothing is no longer exit 0", exit, out)
	}
}

// What the engine's listing leans on: restic snapshots leaves out a
// snapshot it cannot load, exits 0 all the same, and says so on stderr
// alone. With a warm cache it does not even notice, which is why the
// cache goes first here.
func TestIntegrationResticSnapshotsLeavesOutWhatItCannotLoadAndExitsZero(t *testing.T) {
	const restic = "/usr/bin/restic"
	base := t.TempDir()
	must(t, os.WriteFile(filepath.Join(base, "f"), []byte("1"), 0o644))
	env := append(os.Environ(), "RESTIC_PASSWORD=pw", "RESTIC_REPOSITORY="+filepath.Join(base, "repo"), "RESTIC_CACHE_DIR="+filepath.Join(base, "cache"))
	run := func(args ...string) (stdout, stderr string) {
		t.Helper()
		cmd := exec.Command(restic, args...)
		var e strings.Builder
		cmd.Env, cmd.Dir, cmd.Stderr = env, base, &e
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("restic %v: %v: %s", args, err, e.String())
		}
		return string(out), e.String()
	}
	run("init", "-q")
	run("backup", "-q", "--host", "hotserve", "--tag", "app:blog", "f")
	must(t, os.WriteFile(filepath.Join(base, "f"), []byte("2"), 0o644))
	run("backup", "-q", "--host", "hotserve", "--tag", "app:blog", "f")
	if out, said := run("snapshots", "--json", "--no-lock"); strings.Count(out, `"short_id"`) != 2 || said != "" {
		t.Fatalf("fixture: %q, stderr %q", out, said)
	}
	files, err := filepath.Glob(filepath.Join(base, "repo", "snapshots", "*"))
	if err != nil || len(files) != 2 {
		t.Fatalf("fixture: snapshot files %v, %v", files, err)
	}
	must(t, os.Chmod(files[0], 0o600))
	must(t, os.WriteFile(files[0], []byte("not a snapshot, only forty bytes of noise"), 0o600))
	must(t, os.RemoveAll(filepath.Join(base, "cache")))
	out, said := run("snapshots", "--json", "--no-lock") // run fails the test on a non-zero exit
	if strings.Count(out, `"short_id"`) != 1 {
		t.Errorf("listed: %q", out)
	}
	// As besides reads it, to say which.
	if m := ignoringRe.FindStringSubmatch(said); m == nil || m[1] != filepath.Base(files[0]) {
		t.Errorf("stderr: %q", said)
	}
}

// Setup leans on what restic 0.18 says of a repository [M39, M40]:
// `init --json` prints one line, message_type initialized, with the
// repository's id; asked to init a repository that exists it exits 1
// and says so — on stderr, as an exit_error line; "config file already
// exists" here, "already initialized" against the S3 fixture —
// whatever password it was given; `cat config --no-lock` prints the
// config with that id for the right password, exits 12 for a wrong
// one and 10 where there is no repository. A wrong password is not
// something init can tell: the probe is what tells it.
func TestIntegrationResticInitSaysWhenTheRepositoryExists(t *testing.T) {
	const restic = "/usr/bin/restic"
	if _, err := os.Stat(restic); err != nil {
		t.Fatalf("%s is not installed in the integration image: %v", restic, err)
	}
	base := t.TempDir()
	run := func(password, repo string, args ...string) (stdout, stderr string, exit int) {
		t.Helper()
		cmd := exec.Command(restic, args...)
		cmd.Env = append(os.Environ(), "RESTIC_PASSWORD="+password, "RESTIC_REPOSITORY="+filepath.Join(base, repo), "RESTIC_CACHE_DIR="+filepath.Join(base, "cache"))
		var out, errOut strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errOut
		err := cmd.Run()
		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			t.Fatalf("restic %v: %v", args, err)
		}
		return out.String(), errOut.String(), cmd.ProcessState.ExitCode()
	}
	out, errOut, exit := run("pw", "repo", "init", "--json")
	if exit != 0 || errOut != "" {
		t.Fatalf("init: exit %d, stderr %q", exit, errOut)
	}
	must(t, os.WriteFile(filepath.Join(base, "init.out"), []byte(out), 0o600))
	id := initializedID(filepath.Join(base, "init.out"))
	if id == "" || strings.Count(strings.TrimSpace(out), "\n") != 0 {
		t.Fatalf("init --json printed %q; initializedID read %q", out, id)
	}
	for _, password := range []string{"pw", "another"} {
		_, errOut, exit = run(password, "repo", "init", "--json")
		must(t, os.WriteFile(filepath.Join(base, "init.err"), []byte(errOut), 0o600))
		if msg := resticMessage(filepath.Join(base, "init.err")); exit != 1 || !repositoryExists(msg) {
			t.Fatalf("init on an existing repository with password %q: exit %d, message %q", password, exit, msg)
		}
	}
	out, _, exit = run("pw", "repo", "cat", "config", "--no-lock")
	must(t, os.WriteFile(filepath.Join(base, "probe.out"), []byte(out), 0o600))
	if exit != 0 || configID(filepath.Join(base, "probe.out")) != id {
		t.Fatalf("cat config: exit %d, %q; want id %s", exit, out, id)
	}
	if _, _, exit = run("wrong", "repo", "cat", "config", "--no-lock"); exit != 12 {
		t.Fatalf("cat config with a wrong password: exit %d, want 12", exit)
	}
	if _, _, exit = run("pw", "nothing-here", "cat", "config", "--no-lock"); exit != 10 {
		t.Fatalf("cat config where there is no repository: exit %d, want 10", exit)
	}
}

// nobody runs restic as the backup account does: a user of its own, no
// capability.
var nobody = &syscall.Credential{Uid: 65534, Gid: 65534}

// openBase is a directory others may enter, for a restic run as nobody:
// t.TempDir's parents are closed to everyone but root.
func openBase(t *testing.T) string {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root, to run restic as another user")
	}
	base, err := os.MkdirTemp("/var/tmp", "restic-pin-")
	must(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	must(t, os.Chmod(base, 0o755))
	return base
}

// restic writes a snapshot even when it could not read everything,
// exits 3, and names that snapshot in its summary all the same [H]: a
// run records the app incomplete, with the snapshot that holds the rest
// (upload), rather than failed with none. As the backup account, over a
// file only root may read.
func TestIntegrationResticNamesTheSnapshotOfAnIncompleteBackup(t *testing.T) {
	const restic = "/usr/bin/restic"
	base := openBase(t)
	app := filepath.Join(base, "backup", "shop")
	must(t, os.MkdirAll(app, 0o755))
	must(t, os.WriteFile(filepath.Join(app, "r.txt"), []byte("readable"), 0o644))
	must(t, os.WriteFile(filepath.Join(app, "s.txt"), []byte("root's alone"), 0o600))
	resticIn(t, base, time.Minute, restic, "init", "-q")
	must(t, exec.Command("chmod", "-R", "a+rwX", filepath.Join(base, "repo")).Run())
	out, said, exit := resticAs(t, base, nobody, time.Minute, restic, "backup", "--quiet", "--json", "backup/shop")
	must(t, os.WriteFile(filepath.Join(base, "summary"), []byte(out), 0o600))
	if id := summaryID(filepath.Join(base, "summary")); exit != 3 || id == "" {
		t.Fatalf("a backup that could not read s.txt: exit %d, snapshot %q, want exit 3 and the snapshot it made: %s %s", exit, id, out, said)
	}
}

// A run leans on restic's exit 11 for a lock something else held for the
// whole of --retry-lock (resticFailure): every app would meet it, so the
// run stops asking — where any other status would have each app after
// wait retryLock again. Held here with a real exclusive lock: a check's,
// kept by an index file made a FIFO, which the check opens once it has
// locked, and waits on for ever.
func TestIntegrationALockHeldForTheWholeRetryIsExit11(t *testing.T) {
	const restic = "/usr/bin/restic"
	base := t.TempDir()
	must(t, os.WriteFile(filepath.Join(base, "f"), []byte("data"), 0o644))
	resticIn(t, base, time.Minute, restic, "init", "-q")
	resticIn(t, base, time.Minute, restic, "backup", "-q", "f")
	indexes, err := filepath.Glob(filepath.Join(base, "repo", "index", "*"))
	if err != nil || len(indexes) == 0 {
		t.Fatalf("fixture: no index file: %v", err)
	}
	must(t, os.Remove(indexes[0]))
	must(t, syscall.Mkfifo(indexes[0], 0o600))
	holder := exec.Command(restic, "check")
	holder.Env, holder.Dir = resticEnv(base), base
	must(t, holder.Start())
	// SIGKILL: blocked opening the FIFO, it does not end on SIGTERM.
	t.Cleanup(func() { _ = holder.Process.Kill(); _ = holder.Wait() })
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if locks, _ := os.ReadDir(filepath.Join(base, "repo", "locks")); len(locks) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture: the check took no lock within 20s")
		}
	}
	began := time.Now()
	_, said, exit := resticIn(t, base, time.Minute, restic, "backup", "--quiet", "--json", "--retry-lock", "2s", "f")
	if took := time.Since(began); exit != 11 || took < 2*time.Second {
		t.Fatalf("a backup meeting an exclusive lock for the whole of --retry-lock 2s: exit %d after %s, want 11 after 2s: %s", exit, took.Round(time.Millisecond), said)
	}
	if detail, wide := resticFailure(unit.Outcome{Result: "exit-code", ExitStatus: 11}); !wide || !strings.Contains(detail, "locked") {
		t.Errorf("exit 11 is said as %q, repository-wide %v", detail, wide)
	}
}

// D4: an upload, a fetch and the repository check have no backstop —
// their length is the data's — because restic bounds its own waiting:
// a request that moves nothing is retried after five minutes [M10], and
// the retries end [M15]. None of them passes the flag; what is leaned on
// is its default. A restic with another, or none, is a run that may hold
// the run lock for as long as a storage takes connections and never
// answers.
func TestIntegrationResticRetriesAStuckRequestByItself(t *testing.T) {
	out, said, exit := resticIn(t, t.TempDir(), time.Minute, "/usr/bin/restic", "backup", "--help")
	if exit != 0 || !regexp.MustCompile(`(?m)^\s*--stuck-request-timeout duration\s.*\(default 5m0s\)\s*$`).MatchString(out) {
		t.Fatalf("restic backup --help (exit %d) does not give --stuck-request-timeout a default of 5m0s:\n%s%s", exit, out, said)
	}
}

// Every fetch — a restore's, a drill's — runs restic as the backup
// account, which holds no CAP_CHOWN: restic 0.18 tries to give each entry
// its owner by number, is refused, overlooks it, and exits 0 with every
// entry restored, each the restorer's [M6] — the handover unit gives them
// to the data user after. A restic that took the refusal for an error
// would fail every restore and drill.
func TestIntegrationResticRestoresAsAnUnprivilegedUserWhateverTheOwners(t *testing.T) {
	const restic = "/usr/bin/restic"
	base := openBase(t)
	uploads := filepath.Join(base, "backup", "blog", "files", "uploads")
	must(t, os.MkdirAll(uploads, 0o755))
	must(t, os.WriteFile(filepath.Join(uploads, "a.png"), []byte("img"), 0o644))
	must(t, os.Symlink("/etc/passwd", filepath.Join(uploads, "link")))
	must(t, exec.Command("chown", "-hR", "4242:4243", filepath.Join(base, "backup")).Run())
	resticIn(t, base, time.Minute, restic, "init", "-q")
	out, said, exit := resticIn(t, base, time.Minute, restic, "backup", "--quiet", "--json", "backup/blog")
	must(t, os.WriteFile(filepath.Join(base, "summary"), []byte(out), 0o600))
	id := summaryID(filepath.Join(base, "summary"))
	if exit != 0 || id == "" {
		t.Fatalf("fixture: exit %d, %q %s", exit, out, said)
	}
	must(t, exec.Command("chmod", "-R", "a+rwX", filepath.Join(base, "repo")).Run())
	target := filepath.Join(base, "restore")
	must(t, os.Mkdir(target, 0o700))
	must(t, os.Chown(target, int(nobody.Uid), int(nobody.Gid)))
	// As fetch asks, but for its target.
	out, said, exit = resticAs(t, base, nobody, time.Minute, restic, "restore", "--quiet", "--json", "--retry-lock", "1m", id+":/backup/blog", "--target", target)
	must(t, os.WriteFile(filepath.Join(base, "fetch.json"), []byte(out), 0o600))
	if err := fetchedAll(filepath.Join(base, "fetch.json"), id); exit != 0 || err != nil {
		t.Fatalf("a restore as uid %d of files owned 4242:4243: exit %d, %v: %s %s", nobody.Uid, exit, err, out, said)
	}
	must(t, filepath.WalkDir(target, func(p string, _ fs.DirEntry, err error) error {
		must(t, err)
		st, err := os.Lstat(p)
		must(t, err)
		if s := st.Sys().(*syscall.Stat_t); s.Uid != nobody.Uid || s.Gid != nobody.Gid {
			t.Errorf("%s is owned %d:%d, not by the user who restored it", p, s.Uid, s.Gid)
		}
		return nil
	}))
}

// Before every fetch, restic is asked how much room it needs: the size
// of the snapshot once restored [M33]. Of an id the repository does not
// hold it says zero of zero snapshots and exits 0, so only one snapshot,
// counted, is an answer (restoreSize).
func TestIntegrationResticSaysHowLargeASnapshotIsOnceRestored(t *testing.T) {
	const restic = "/usr/bin/restic"
	base := t.TempDir()
	app := filepath.Join(base, "backup", "blog")
	must(t, os.MkdirAll(filepath.Join(app, "files"), 0o755))
	must(t, os.WriteFile(filepath.Join(app, "files", "a.png"), make([]byte, 1000), 0o644))
	must(t, os.WriteFile(filepath.Join(app, "plan.json"), make([]byte, 24), 0o644))
	resticIn(t, base, time.Minute, restic, "init", "-q")
	out, said, exit := resticIn(t, base, time.Minute, restic, "backup", "--quiet", "--json", "backup/blog")
	must(t, os.WriteFile(filepath.Join(base, "summary"), []byte(out), 0o600))
	id := summaryID(filepath.Join(base, "summary"))
	if exit != 0 || id == "" {
		t.Fatalf("fixture: exit %d, %q %s", exit, out, said)
	}
	size := func(id string) (uint64, error) {
		t.Helper()
		out, said, exit := resticIn(t, base, time.Minute, restic, "stats", "--quiet", "--json", "--no-lock", "--mode", "restore-size", id)
		if exit != 0 {
			t.Fatalf("stats of %.8s: exit %d: %s", id, exit, said)
		}
		must(t, os.WriteFile(filepath.Join(base, "size.json"), []byte(out), 0o600))
		return restoreSize(filepath.Join(base, "size.json"), id)
	}
	if got, err := size(id); err != nil || got != 1024 {
		t.Errorf("a snapshot of 1024 bytes: %d, %v", got, err)
	}
	if got, err := size(strings.Repeat("0", 64)); err == nil {
		t.Errorf("an id the repository does not hold was taken for an answer: %d", got)
	}
}

// A declared database inside a declared files path is left out of the
// upload by an exclude file (excludes): the mask over its live bytes
// covers only what exists as the unit starts. restic reads each line as
// a pattern and expands $VAR in it [M24], so every pattern character is
// escaped and every dollar doubled: each database goes, with its
// sidecars, and nothing that only looks like one. Wrong, a file the app
// keeps beside them is not backed up, and nothing says so. restic
// matches against the path on disk, so the app is where the upload
// unit's view puts it.
func TestIntegrationTheExcludeFileLeavesOutTheDeclaredDatabasesAndNothingElse(t *testing.T) {
	const restic = "/usr/bin/restic"
	if os.Geteuid() != 0 {
		t.Skip("needs root, for /backup")
	}
	const app = "pin-exclude"
	files := filepath.Join("/backup", app, "files")
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join("/backup", app)); _ = os.Remove("/backup") })
	databases := []string{"app*.db", "da[t]a/x.db", "$HOME_SECRET.db", `back\slash.db`, "q?.db"}
	lookalikes := []string{"appX.db", "appX.db-wal", "data/x.db", "leaked.db", "qZ.db"}
	for _, f := range append(append(slices.Clone(databases), "app*.db-wal", "q?.db-journal"), lookalikes...) {
		must(t, os.MkdirAll(filepath.Dir(filepath.Join(files, f)), 0o755))
		must(t, os.WriteFile(filepath.Join(files, f), []byte("x"), 0o644))
	}
	t.Setenv("HOME_SECRET", "leaked")
	base := t.TempDir()
	must(t, os.WriteFile(filepath.Join(base, "exclude"), []byte(excludes(app, &backupdecl.Config{SQLite: databases, Files: []string{"."}})), 0o600))
	resticIn(t, base, time.Minute, restic, "init", "-q")
	out, said, exit := resticIn(t, base, time.Minute, restic, "backup", "--quiet", "--json", "--exclude-file", filepath.Join(base, "exclude"), filepath.Join("/backup", app))
	must(t, os.WriteFile(filepath.Join(base, "summary"), []byte(out), 0o600))
	id := summaryID(filepath.Join(base, "summary"))
	if exit != 0 || id == "" {
		t.Fatalf("the backup: exit %d, %q %s", exit, out, said)
	}
	listing, said, exit := resticIn(t, base, time.Minute, restic, "ls", "--json", "--no-lock", id)
	if exit != 0 {
		t.Fatalf("ls: exit %d: %s", exit, said)
	}
	in := map[string]bool{}
	for _, line := range strings.Split(listing, "\n") {
		var n struct {
			StructType string `json:"struct_type"`
			Path       string `json:"path"`
		}
		if json.Unmarshal([]byte(line), &n) == nil && n.StructType == "node" {
			in[strings.TrimPrefix(n.Path, files+"/")] = true
		}
	}
	for _, db := range databases {
		for _, f := range []string{db, db + "-wal", db + "-journal"} {
			if in[f] {
				t.Errorf("%s is in the snapshot: the exclude file did not leave it out", f)
			}
		}
	}
	for _, f := range lookalikes {
		if !in[f] {
			t.Errorf("%s is not in the snapshot: the exclude file left out what only looks like a declared database", f)
		}
	}
}
