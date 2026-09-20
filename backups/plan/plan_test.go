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
	got, bare, imported, _, err := envNames(dir + "/Caddyfile")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"CONF"}; !reflect.DeepEqual(imported, want) {
		t.Fatalf("used in an import = %v, want %v", imported, want)
	}
	// DOMAIN and CONF are only ever written with a default.
	if want := []string{"ACME_EMAIL", "BEHIND_A_DEFAULT", "GREETING", "QUOTED", "WEBROOT"}; !reflect.DeepEqual(bare, want) {
		t.Fatalf("with no default = %v, want %v", bare, want)
	}
	if want := []string{"ACME_EMAIL", "BEHIND_A_DEFAULT", "CONF", "DOMAIN", "GREETING", "QUOTED", "WEBROOT"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
}

// fakeHotserve stands in for `hotserve adapt`, failing where and how
// the real one does [measured]: a variable with no default, unset or
// empty, is a parse error ({$ACME_EMAIL}, {$ROOT_NO_DEFAULT}, {$MAGIC});
// a port ({$METRICS_PORT}, {$ADMIN_PORT}, {$PORT:8080}) has to be a
// number, and the error quotes the value that is not one; {$ENV:prod}
// names a file to import, so nothing but "prod" adapts; {$MAGIC} takes
// one value nobody would guess. The root comes from
// {$LIVESWAP_ROOT:/var/lib/liveswap} or {$ROOT_NO_DEFAULT}, a backup
// path from {$DB:app.db}, an app's name from {$APP_NAME:blog}, and
// {$DOMAIN} changes nothing a plan holds.
// Which of them the "Caddyfile" uses is read from the file. It fails if
// anything of the caller's environment reaches it.
func fakeHotserve(t *testing.T) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "hotserve")
	write(t, script, `#!/bin/sh
[ -n "$CALLER_SECRET" ] && { echo "the caller's environment reached the adapter" >&2; exit 1; }
for last; do :; done
uses() { grep -q "{\$$1[:}]" "$last"; }
for v in ACME_EMAIL ROOT_NO_DEFAULT MAGIC; do
	if uses $v && eval "[ -z \"\${$v}\" ]"; then echo "Error: wrong argument count or unexpected line ending after '$v'" >&2; exit 1; fi
done
if uses MAGIC && [ "$MAGIC" != xyzzy ]; then echo "Error: unrecognized value '$MAGIC'" >&2; exit 1; fi
for v in METRICS_PORT ADMIN_PORT; do
	if uses $v; then
		eval "p=\${$v}"
		case "$p" in ""|*[!0-9]*) echo "Error: parsing key: invalid port '$p': strconv.Atoi" >&2; exit 1;; esac
	fi
done
case "${PORT:-8080}" in *[!0-9]*) echo "Error: invalid port '$PORT'" >&2; exit 1;; esac
[ "${ENV:-prod}" = prod ] || { echo "Error: File to import not found: sites/$ENV.caddy" >&2; exit 1; }
root=${LIVESWAP_ROOT:-/var/lib/liveswap}
uses ROOT_NO_DEFAULT && root=$ROOT_NO_DEFAULT
more=""
grep -q "^app shop" "$last" && more=',"shop":{},"cart":{"backup":null}'
uses SHOP_NAME && more=",\"${SHOP_NAME:-shop}\":{}"
printf '{"apps":{"http":{"domain":"%s"},"liveswap":{"root":"%s","apps":{"%s":{"backup":{"sqlite":["%s"]}}%s}}}}' "${DOMAIN:-example.com}" "$root" "${APP_NAME:-blog}" "${DB:-app.db}" "$more"
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
		"a variable that names a file to import": {"import sites/{$ENV:prod}.caddy\n", []string{"ENV", "imports by"}},
		// A glob that matches nothing is not an error, so this adapts with
		// any value at all — and says nothing of what the server reads.
		"a variable in an import glob": {"import sites/{$ENV:prod}/*.caddy\n", []string{"ENV", "imports by"}},
		"the root":                     {"liveswap {\n\troot {$LIVESWAP_ROOT:/var/lib/liveswap}\n}\n", []string{"LIVESWAP_ROOT"}},
		"a backup path":                {"backup {\n\tsqlite {$DB:app.db}\n}\n", []string{"DB"}},
		"both, among others":           {"{$DOMAIN:example.com} :{$PORT:80} {$LIVESWAP_ROOT:/var/lib/liveswap} {$DB:app.db}\n", []string{"DB, LIVESWAP_ROOT"}},
		// The ordinary production file: variables with no default, which
		// an empty environment makes a parse error of. Made-up values, of
		// the kind the adapter takes in each place, and the plan is what
		// it would be anyway.
		"an email with no default":             {"email {$ACME_EMAIL}\n", nil},
		"and a port, which has to be a number": {"email {$ACME_EMAIL}\n:{$METRICS_PORT} {\n}\n", nil},
		"and a second port":                    {"email {$ACME_EMAIL}\n:{$METRICS_PORT} {\n}\n:{$ADMIN_PORT} {\n}\n", nil},
		"an app's name":                        {"app {$APP_NAME:blog} {\n}\n", []string{"APP_NAME", "depends on"}},
		"a root with no default":               {"root {$ROOT_NO_DEFAULT}\n", []string{"ROOT_NO_DEFAULT", "depends on"}},
		"a value nothing made up will do for":  {"thing {$MAGIC}\n", []string{"unrecognized value", "MAGIC", "default"}},
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
