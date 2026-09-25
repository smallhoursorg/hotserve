//go:build integration

package plan

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The plan step's refusal of an import glob that matches nothing rests
// on the words of Caddy's own warning, in the form Caddy logs it when
// its stderr is not a terminal (emptyGlobs). Were a Caddy bump to word
// or shape that line otherwise, the refusal would be gone, and with it
// the one thing that stops a run planning without the apps a view
// cannot see — silently. So it is held to the real adapter, built from
// this repository, here and not only in e2e.
func TestIntegrationTheAdapterWarnsOfAnEmptyGlobAsTheRefusalReadsIt(t *testing.T) {
	root, err := filepath.Abs("../..") // the workspace: the hotserve module is its root
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "hotserve")
	build := exec.Command("go", "build", "-o", bin, "./cmd/hotserve")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building hotserve: %v\n%s", err, out)
	}
	old := hotserve
	hotserve = bin
	t.Cleanup(func() { hotserve = old })

	dir := t.TempDir()
	file := filepath.Join(dir, "Caddyfile")
	site := "http://:8080 {\n\trespond hi\n}\n"
	outside := filepath.Join(t.TempDir(), "apps", "*.caddy")
	if err := os.WriteFile(file, []byte("import "+outside+"\n"+site), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Make(context.Background(), file)
	if err == nil || !strings.Contains(err.Error(), outside+", which matches no file") {
		t.Fatalf("Make of a Caddyfile whose import glob matches nothing: %v", err)
	}

	// And one that matches is taken: the refusal is of the warning, not
	// of every import.
	if err := os.MkdirAll(filepath.Join(dir, "sites"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sites", "a.caddy"), []byte("# nothing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("import sites/*.caddy\n"+site), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Make(context.Background(), file); err != nil {
		t.Fatalf("Make of a glob that matches: %v", err)
	}
}
