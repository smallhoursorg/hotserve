//go:build integration

package engine

import (
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
