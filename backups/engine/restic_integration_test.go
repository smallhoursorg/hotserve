//go:build integration

package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
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
	if !strings.Contains(said, "Ignoring") || !strings.Contains(said, filepath.Base(files[0])) {
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
