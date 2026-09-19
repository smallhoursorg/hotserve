package plan

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/smallhoursorg/hotserve/liveswap/backupdecl"
)

func TestFromAdapted(t *testing.T) {
	p, err := FromAdapted([]byte(`{"apps":{"http":{},"liveswap":{"apps":{
		"blog":{"command":["./server"],"backup":{"sqlite":["app.db"],"files":["uploads"]}},
		"api":{"command":["./api"]}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := &Plan{Root: "/var/lib/liveswap", Apps: map[string]*backupdecl.Config{"blog": {SQLite: []string{"app.db"}, Files: []string{"uploads"}}}}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("plan = %+v, want the default root and only the app that declares a backup", p)
	}
	if p, err := FromAdapted([]byte(`{"apps":{"liveswap":{"root":"/srv/live swap"}}}`)); err != nil || p.Root != "/srv/live swap" {
		t.Fatalf("an explicit root: %+v, %v", p, err)
	}
	if p, err := FromAdapted([]byte(`{"apps":{"http":{}}}`)); err != nil || len(p.Apps) != 0 {
		t.Fatalf("a config with no liveswap app is an empty plan, got %+v, %v", p, err)
	}
}

// Nothing is trusted because `hotserve validate` would have refused
// it: the file may never have been validated.
func TestFromAdaptedRefuses(t *testing.T) {
	for name, tc := range map[string]struct{ raw, want string }{
		"not JSON":                 {`Error: adapting config`, "reading the adapted config"},
		"a placeholder root":       {`{"apps":{"liveswap":{"root":"{env.ROOT}","apps":{"blog":{"backup":{"files":["."]}}}}}}`, "placeholder"},
		"a relative root":          {`{"apps":{"liveswap":{"root":"srv/liveswap"}}}`, "not an absolute path"},
		"an unclean root":          {`{"apps":{"liveswap":{"root":"/srv/../etc"}}}`, "not a clean path"},
		"a bad app name":           {`{"apps":{"liveswap":{"apps":{"../etc":{"backup":{"files":["."]}}}}}}`, "is not one liveswap accepts"},
		"a path outside shared":    {`{"apps":{"liveswap":{"apps":{"blog":{"backup":{"files":["../shop/shared"]}}}}}}`, "outside the app's shared dir"},
		"a declaration of nothing": {`{"apps":{"liveswap":{"apps":{"blog":{"backup":{}}}}}}`, "declares nothing"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := FromAdapted([]byte(tc.raw)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestDecodeIsStrict(t *testing.T) {
	good := `{"root":"/var/lib/liveswap","apps":{"blog":{"sqlite":["app.db"]}}}`
	if _, err := Decode([]byte(good)); err != nil {
		t.Fatalf("a good plan: %v", err)
	}
	for name, tc := range map[string]struct{ raw, want string }{
		"an unknown field":           {`{"root":"/var/lib/liveswap","apps":{},"exec":"/bin/sh"}`, "unknown field"},
		"an unknown field in an app": {`{"root":"/var/lib/liveswap","apps":{"blog":{"sqlite":["a.db"],"user":"root"}}}`, "unknown field"},
		"something after the plan":   {good + `{"root":"/"}`, "something follows"},
		"no root":                    {`{"apps":{}}`, "not an absolute path"},
		"a null declaration":         {`{"root":"/var/lib/liveswap","apps":{"blog":null}}`, "is null"},
		"an app name with a slash":   {`{"root":"/var/lib/liveswap","apps":{"a/b":{"files":["."]}}}`, "is not one liveswap accepts"},
		"a path that escapes":        {`{"root":"/var/lib/liveswap","apps":{"blog":{"files":["../x"]}}}`, "outside"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode([]byte(tc.raw)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestEnvNamesFollowsImports(t *testing.T) {
	dir := t.TempDir()
	write(t, dir+"/Caddyfile", "{\n\temail {$ACME_EMAIL}\n}\nimport sites/*.caddy\nimport snippet-name arg\nimport "+dir+"/abs.caddy\nimport \"quoted dir/q.caddy\"\nimport {$CONF:byenv}/e.caddy\n")
	write(t, dir+"/quoted dir/q.caddy", "respond {$QUOTED}\n")
	write(t, dir+"/byenv/e.caddy", "respond {$BEHIND_A_DEFAULT}\n")
	write(t, dir+"/sites/a.caddy", "{$DOMAIN:example.com} {\n\timport ../Caddyfile\n\timport deeper/*\n}\n")
	write(t, dir+"/sites/deeper/b", "root * {$WEBROOT}\n")
	write(t, dir+"/abs.caddy", "respond {$GREETING}\n")
	got, err := envNames(dir + "/Caddyfile")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ACME_EMAIL", "BEHIND_A_DEFAULT", "CONF", "DOMAIN", "GREETING", "QUOTED", "WEBROOT"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
}

// fakeHotserve stands in for `hotserve adapt`: the root comes from
// {$LIVESWAP_ROOT:/var/lib/liveswap}, a backup path from {$DB:app.db},
// the port from {$PORT:8080} — refused unless numeric, as the real
// adapter refuses a site address — {$DOMAIN} changes nothing a plan
// holds, and {$ENV:prod} names a file to import, so that nothing but
// "prod" adapts. It fails if anything of the caller's environment
// reaches it.
func fakeHotserve(t *testing.T) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "hotserve")
	write(t, script, `#!/bin/sh
[ -n "$CALLER_SECRET" ] && { echo "the caller's environment reached the adapter" >&2; exit 1; }
case "${PORT:-8080}" in *[!0-9]*) echo "Error: invalid port" >&2; exit 1;; esac
[ "${ENV:-prod}" = prod ] || { echo "Error: File to import not found: sites/$ENV.caddy" >&2; exit 1; }
printf '{"apps":{"http":{"domain":"%s"},"liveswap":{"root":"%s","apps":{"blog":{"backup":{"sqlite":["%s"]}}}}}}' "${DOMAIN:-example.com}" "${LIVESWAP_ROOT:-/var/lib/liveswap}" "${DB:-app.db}"
`)
	old := hotserve
	hotserve = script
	t.Cleanup(func() { hotserve = old })
}

func TestMakeRefusesAPlanThatDependsOnTheEnvironment(t *testing.T) {
	fakeHotserve(t)
	t.Setenv("CALLER_SECRET", "must not reach the adapter")
	t.Setenv("LIVESWAP_ROOT", "/the/callers/own") // nor may this decide anything
	for name, tc := range map[string]struct {
		caddyfile string
		want      []string // in the error; nil means the plan is made
	}{
		"no variables":                         {"liveswap {\n}\n", nil},
		"a variable that is not the plan's":    {"{$DOMAIN} {\n}\n", nil},
		"a variable an arbitrary value breaks": {":{$PORT:8080} {\n}\n", nil}, // cleared by the trial value "1"
		// The server, with ENV=staging, imports another file, which may
		// say anything about the root: not knowable here, so refused.
		"a variable no trial value adapts with": {"import sites/{$ENV:prod}.caddy\n", []string{"ENV", "does not adapt"}},
		"the root":                              {"liveswap {\n\troot {$LIVESWAP_ROOT:/var/lib/liveswap}\n}\n", []string{"LIVESWAP_ROOT"}},
		"a backup path":                         {"backup {\n\tsqlite {$DB:app.db}\n}\n", []string{"DB"}},
		"both, among others":                    {"{$DOMAIN} :{$PORT} {$LIVESWAP_ROOT} {$DB}\n", []string{"DB, LIVESWAP_ROOT"}},
	} {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "Caddyfile")
			write(t, file, tc.caddyfile)
			p, err := Make(context.Background(), file)
			if tc.want == nil {
				if err != nil || p.Root != "/var/lib/liveswap" {
					t.Fatalf("want the plan, made with no environment at all, got %+v, %v", p, err)
				}
				return
			}
			for _, w := range tc.want {
				if err == nil || !strings.Contains(err.Error(), w) {
					t.Fatalf("want an error naming %q, got %v", w, err)
				}
			}
			if strings.Contains(err.Error(), "DOMAIN") || strings.Contains(err.Error(), "PORT") {
				t.Fatalf("the error blames a variable the plan does not depend on: %v", err)
			}
		})
	}
}
