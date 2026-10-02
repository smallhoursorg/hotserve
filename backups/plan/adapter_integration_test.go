//go:build integration

package plan

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	realAdapter(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "Caddyfile")
	site := "http://:8080 {\n\trespond hi\n}\n"
	outside := filepath.Join(t.TempDir(), "apps", "*.caddy")
	if err := os.WriteFile(file, []byte("import "+outside+"\n"+site), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Make(context.Background(), file)
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

// The adapter, hotserve built from this repository: once, by the first
// test that asks, and removed after the last (TestMain).
var (
	adapterOnce sync.Once
	adapterDir  string
	adapterErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if adapterDir != "" {
		_ = os.RemoveAll(adapterDir)
	}
	os.Exit(code)
}

// realAdapter has Make adapt with the real adapter for the rest of the
// test.
func realAdapter(t *testing.T) {
	t.Helper()
	adapterOnce.Do(func() {
		root, err := filepath.Abs("../..") // the workspace: the hotserve module is its root
		if err != nil {
			adapterErr = err
			return
		}
		if adapterDir, adapterErr = os.MkdirTemp("", "plan-adapter-"); adapterErr != nil {
			return
		}
		build := exec.Command("go", "build", "-o", filepath.Join(adapterDir, "hotserve"), "./cmd/hotserve")
		build.Dir = root
		if out, err := build.CombinedOutput(); err != nil {
			adapterErr = fmt.Errorf("building hotserve: %w\n%s", err, out)
		}
	})
	if adapterErr != nil {
		t.Fatal(adapterErr)
	}
	old := hotserve
	hotserve = filepath.Join(adapterDir, "hotserve")
	t.Cleanup(func() { hotserve = old })
}

// A run adapts the Caddyfile without the server's environment, and what
// it reads of {$NAME} rests on how Caddy substitutes it: in the text,
// before any module sees a token; unset, with no default, as nothing —
// which makes `email {$ACME_EMAIL}` no Caddyfile at all; and
// {$NAME:default} as the default. So Make gives a variable with no
// default a made-up value of the kind its place takes, and refuses one
// whose value moves the root or a backup path, naming it (Inspect).
// Held to the real adapter, as the empty-glob warning is.
func TestIntegrationTheAdapterSubstitutesVariablesAsMakeReadsThem(t *testing.T) {
	realAdapter(t)
	const apps = `
	liveswap {
		root %s
		artifact_allowlist example.com/
		deploy_trust github {
			audience hotserve
			claim repository your-org/app
		}
		app blog {
			command ./server
			backup {
				sqlite app.db
				files  uploads
			}
		}
	}
`
	write := func(global, root, sites string) string {
		t.Helper()
		file := filepath.Join(t.TempDir(), "Caddyfile")
		if err := os.WriteFile(file, []byte("{\n\tadmin off\n"+global+strings.Replace(apps, "%s", root, 1)+"}\n\n"+sites), 0o644); err != nil {
			t.Fatal(err)
		}
		return file
	}

	// An ordinary production Caddyfile: variables with no default.
	file := write("\temail {$ACME_EMAIL}\n", "/var/lib/liveswap", ":{$METRICS_PORT} {\n\trespond metrics\n}\n\n{$SITE_DOMAIN} {\n\trespond ok\n}\n")
	adapt := exec.Command(hotserve, "adapt", "--adapter", "caddyfile", "--config", file)
	adapt.Env = []string{}
	if out, err := adapt.CombinedOutput(); err == nil {
		t.Fatalf("fixture: with an empty environment this Caddyfile adapts, so the test proves nothing: %s", out)
	}
	p, err := Make(context.Background(), file)
	if err != nil || p.Root != "/var/lib/liveswap" || p.Apps["blog"] == nil {
		t.Fatalf("Make of a Caddyfile whose variables have no default: %+v, %v", p, err)
	}

	// A root the server and a run would read differently.
	file = write("", "{$LIVESWAP_ROOT:/var/lib/liveswap}", ":8080 {\n\trespond ok\n}\n")
	if _, err := Make(context.Background(), file); err == nil || !strings.Contains(err.Error(), "depends on the environment variable(s) LIVESWAP_ROOT") {
		t.Fatalf("Make of a root written {$LIVESWAP_ROOT:…}: %v", err)
	}
}
