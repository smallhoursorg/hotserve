package proof

// The fixture generator: a repository made by git with SSH-signed
// commits, in a temporary directory, from which every commit, tree and
// blob is exported raw. TestLiveGit runs it and the table over the
// result on every `make test` (the dev container has git and
// ssh-keygen); with -update it also rewrites testdata/, which is what
// the committed set came from. Keys are fresh each run, so a
// regeneration changes every byte: that is expected, and the
// committed set is the one reviewed.

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/ from a fresh git repository")

func TestLiveGit(t *testing.T) {
	needTools(t, "git", "ssh-keygen")
	dir := t.TempDir()
	generate(t, dir)
	fx := loadFixtures(t, dir)
	runFixtureTable(t, fx)
	if *update {
		// The new set lands beside the old and is renamed over it, so a
		// failed copy leaves the committed fixtures where they were.
		if err := os.RemoveAll("testdata.new"); err != nil {
			t.Fatal(err)
		}
		if err := os.CopyFS("testdata.new", os.DirFS(dir)); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll("testdata"); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename("testdata.new", "testdata"); err != nil {
			t.Fatal(err)
		}
		t.Log("testdata/ rewritten")
	}
}

// generate makes the repository and writes the fixture set into out.
func generate(t *testing.T, out string) {
	t.Helper()
	work := t.TempDir()
	home := filepath.Join(work, "home")
	repo := filepath.Join(work, "repo")
	for _, d := range []string{home, repo, filepath.Join(out, "objects")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Keys: two listed (ed25519, ecdsa), one not.
	keys := map[string]string{}
	for name, kind := range map[string][]string{"alice": {"-t", "ed25519"}, "bob": {"-t", "ecdsa", "-b", "256"}, "mallory": {"-t", "ed25519"}} {
		path := filepath.Join(home, name)
		run(t, work, nil, "ssh-keygen", append(kind, "-q", "-N", "", "-C", name+"@example.com", "-f", path)...)
		pub, err := os.ReadFile(path + ".pub")
		if err != nil {
			t.Fatal(err)
		}
		keys[name] = path
		f := strings.Fields(string(pub))
		if err := os.WriteFile(filepath.Join(out, name+".signer"), []byte(name+"@example.com "+f[0]+" "+f[1]+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	env := []string{
		"HOME=" + home,
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Alice", "GIT_AUTHOR_EMAIL=alice@example.com",
		"GIT_COMMITTER_NAME=Alice", "GIT_COMMITTER_EMAIL=alice@example.com",
		"GIT_AUTHOR_DATE=2026-10-09T12:00:00+0000", "GIT_COMMITTER_DATE=2026-10-09T12:00:00+0000",
		"LC_ALL=C",
	}
	git := func(args ...string) string {
		t.Helper()
		return run(t, repo, env, "git", args...)
	}
	signed := func(key, msg string) string {
		t.Helper()
		git("-c", "gpg.format=ssh", "-c", "user.signingkey="+keys[key], "commit", "-q", "-S", "--allow-empty", "-m", msg)
		return strings.TrimSpace(git("rev-parse", "HEAD"))
	}
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", rel)
	}
	const path = "box1/Caddyfile"
	caddyfile := func(v int) string {
		return fmt.Sprintf("# box1, version %d\n{\n\tbox {\n\t\tdeploy_trust github {\n\t\t\taudience hotserve\n\t\t}\n\t\tsigner alice@example.com ssh-ed25519 AAAA\n\t}\n}\n\ndeploy.example.com {\n\tbox_webhook\n}\n", v)
	}

	git("init", "-q", "-b", "main")
	cases := map[string]string{}
	write(path, caddyfile(0))
	cases["base"] = signed("alice", "baseline")
	write(path, caddyfile(1))
	cases["mid"] = signed("alice", "change 1")
	write(path, caddyfile(2))
	cases["head"] = signed("alice", "change 2")

	// Above head, each on its own branch.
	git("checkout", "-q", "-b", "unsigned")
	write(path, caddyfile(3))
	git("commit", "-q", "--no-gpg-sign", "-m", "pushed by a token")
	cases["unsigned"] = strings.TrimSpace(git("rev-parse", "HEAD"))
	write(path, caddyfile(4))
	cases["after_unsigned"] = signed("alice", "on top of the unsigned one")

	git("checkout", "-q", "-b", "mallory", cases["head"])
	write(path, caddyfile(3))
	cases["mallory"] = signed("mallory", "not a signer")

	git("checkout", "-q", "-b", "bob", cases["head"])
	write(path, caddyfile(3))
	cases["bob"] = signed("bob", "a listed ecdsa key")

	git("checkout", "-q", "-b", "special", cases["head"])
	write("box1/dir/x", "x\n")
	linkBlob := hashObject(t, repo, env, "Caddyfile")
	git("update-index", "--add", "--cacheinfo", "120000,"+linkBlob+",box1/link")
	git("update-index", "--add", "--cacheinfo", "160000,"+cases["base"]+",box1/sub")
	cases["special"] = signed("alice", "a symlink, a submodule, a directory")

	git("checkout", "-q", "--orphan", "orphan")
	git("rm", "-rq", "--cached", ".")
	write(path, caddyfile(0))
	cases["orphan"] = signed("alice", "another history")

	// git's own verdicts, as the oracle for ours.
	allowed := filepath.Join(home, "allowed_signers")
	var lines []string
	for _, name := range []string{"alice", "bob"} {
		s, err := os.ReadFile(filepath.Join(out, name+".signer"))
		if err != nil {
			t.Fatal(err)
		}
		f := strings.Fields(string(s))
		lines = append(lines, f[0]+" namespaces=\"git\" "+f[1]+" "+f[2])
	}
	if err := os.WriteFile(allowed, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"base": true, "mid": true, "head": true, "bob": true, "after_unsigned": true, "unsigned": false, "mallory": false} {
		cmd := exec.CommandContext(context.Background(), "git", "-c", "gpg.ssh.allowedSignersFile="+allowed, "verify-commit", cases[name])
		cmd.Dir, cmd.Env = repo, withPath(env)
		if err := cmd.Run(); (err == nil) != want {
			t.Fatalf("git verify-commit %s (%s): %v, want ok=%v", name, cases[name], err, want)
		}
	}

	// Export every object, raw.
	m := manifest{Path: path, Cases: map[string]fixtureCase{}}
	for _, line := range strings.Split(strings.TrimSpace(git("cat-file", "--batch-check", "--batch-all-objects")), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			t.Fatalf("batch-check: %q", line)
		}
		id, kind := f[0], f[1]
		raw := run(t, repo, env, "git", "cat-file", kind, id)
		if ObjectID(kind, []byte(raw)) != id {
			t.Fatalf("%s %s does not hash to its id", kind, id)
		}
		if err := os.WriteFile(filepath.Join(out, "objects", id), []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, id := range cases {
		blob := strings.TrimSpace(git("rev-parse", id+":"+path))
		m.Cases[name] = fixtureCase{Commit: id, File: blob}
	}
	for _, name := range []string{"alice", "bob"} {
		s, err := os.ReadFile(filepath.Join(out, name+".signer"))
		if err != nil {
			t.Fatal(err)
		}
		m.Signers = append(m.Signers, strings.TrimSpace(string(s)))
	}
	s, err := os.ReadFile(filepath.Join(out, "mallory.signer"))
	if err != nil {
		t.Fatal(err)
	}
	m.Mallory = strings.TrimSpace(string(s))

	// A SHA-256 repository's commit.
	repo256 := filepath.Join(work, "repo256")
	if err := os.MkdirAll(repo256, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, repo256, env, "git", "init", "-q", "-b", "main", "--object-format=sha256")
	if err := os.WriteFile(filepath.Join(repo256, "Caddyfile"), []byte(caddyfile(0)), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, repo256, env, "git", "add", "Caddyfile")
	run(t, repo256, env, "git", "commit", "-q", "--no-gpg-sign", "-m", "sha256")
	id256 := strings.TrimSpace(run(t, repo256, env, "git", "rev-parse", "HEAD"))
	raw256 := run(t, repo256, env, "git", "cat-file", "commit", id256)
	m.SHA256 = "sha256-commit"
	if err := os.WriteFile(filepath.Join(out, "objects", m.SHA256), []byte(raw256), 0o644); err != nil {
		t.Fatal(err)
	}

	js, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "manifest.json"), append(js, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// hashObject is the blob id git gives those bytes, written to the
// repository.
func hashObject(t *testing.T, repo string, env []string, content string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "hash-object", "-w", "--stdin")
	cmd.Dir, cmd.Env = repo, withPath(env)
	cmd.Stdin = strings.NewReader(content)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// withPath is a fresh copy of env with the test process's PATH added,
// so the tools are found and no call's append aliases another's.
func withPath(env []string) []string {
	return append(append([]string(nil), env...), "PATH="+os.Getenv("PATH"))
}

// run executes a tool and returns its stdout; a failure is fatal and
// shows stderr.
func run(t *testing.T, dir string, env []string, program string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), program, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = withPath(env)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", program, strings.Join(args, " "), err, stderr.String())
	}
	return string(out)
}
