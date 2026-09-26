package plan

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// What validate says beyond the plan: the apps that declare no backup —
// a new app's block with the two lines forgotten is otherwise silent.
func TestInspectNamesTheAppsThatDeclareNoBackup(t *testing.T) {
	fakeHotserve(t)
	file := filepath.Join(t.TempDir(), "Caddyfile")
	write(t, file, "app blog\napp shop\n")
	got, err := Inspect(context.Background(), file, filepath.Dir(file))
	if err != nil {
		t.Fatal(err)
	}
	if got.Plan == nil || len(got.Plan.Apps) != 1 || got.Plan.Apps["blog"] == nil {
		t.Fatalf("the plan: %+v", got.Plan)
	}
	if !slices.Equal(got.Undeclared, []string{"cart", "shop"}) {
		t.Fatalf("undeclared: %q, want cart and shop", got.Undeclared)
	}
	made, err := Make(context.Background(), file)
	if err != nil || len(made.Apps) != 1 {
		t.Fatalf("Make no longer agrees: %+v, %v", made, err)
	}
}

func TestInspectWithEveryAppDeclared(t *testing.T) {
	fakeHotserve(t)
	file := filepath.Join(t.TempDir(), "Caddyfile")
	write(t, file, "app blog\n")
	got, err := Inspect(context.Background(), file, filepath.Dir(file))
	if err != nil || len(got.Undeclared) != 0 {
		t.Fatalf("%+v, %v", got, err)
	}
}

// An import glob that matches nothing is a warning to the adapter and a
// refusal here: inside a run's view a glob reaching outside it — or
// through a directory the run may not list — matches nothing, and the
// run would plan without what is declared there. The adapter's warning
// comes after its own snippets, heredocs and {$NAME} [measured with the
// real one; the stand-in here warns for the file's own lines].
func TestAnImportGlobThatMatchesNothingIsRefused(t *testing.T) {
	fakeHotserve(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "Caddyfile")
	for _, line := range []string{"import /srv/apps/*.caddy", "import sites/*.caddy"} {
		write(t, file, line+"\napp blog\n")
		for what, err := range map[string]error{"Inspect": second(Inspect(context.Background(), file, filepath.Dir(file))), "Make": second(Make(context.Background(), file))} {
			pattern := strings.TrimPrefix(line, "import ")
			if err == nil || !strings.Contains(err.Error(), pattern+", which matches no file") || !strings.Contains(err.Error(), "its own directory") {
				t.Errorf("%s of %q: %v", what, line, err)
			}
		}
	}
	// One that matches is no business of this.
	write(t, filepath.Join(dir, "sites", "blog.caddy"), "# nothing\n")
	write(t, file, "import sites/*.caddy\napp blog\n")
	if _, err := Make(context.Background(), file); err != nil {
		t.Fatalf("a glob that matches: %v", err)
	}
}

// An empty glob with no variable in it is not the variables' doing: it
// is said as it is, and no made-up value is tried in its place.
func TestAnEmptyGlobIsNotBlamedOnAVariable(t *testing.T) {
	fakeHotserve(t)
	file := filepath.Join(t.TempDir(), "Caddyfile")
	write(t, file, "email {$ACME_EMAIL}\nimport /srv/none/*.caddy\napp blog\n")
	_, err := Make(context.Background(), file)
	if err == nil || !strings.Contains(err.Error(), "/srv/none/*.caddy, which matches no file") || strings.Contains(err.Error(), "made-up values") {
		t.Fatalf("%v", err)
	}
}

// The adapter's warning is read as the adapter writes it, a JSON line
// among others on stderr, and never taken from anything else there.
func TestEmptyGlobsAreReadFromTheAdaptersOwnWarning(t *testing.T) {
	stderr := []byte(`{"level":"info","msg":"using config from file","file":"/etc/hotserve/Caddyfile"}
{"level":"warn","ts":1790355132.8,"msg":"No files matching import glob pattern","pattern":"/srv/apps/*.caddy"}
No files matching import glob pattern: not JSON, not the adapter's
{"level":"warn","msg":"Caddyfile input is not formatted"}
{"level":"warn","msg":"No files matching import glob pattern","pattern":"sites/*.caddy"}
`)
	if got := emptyGlobs(stderr); !slices.Equal(got, []string{"/srv/apps/*.caddy", "sites/*.caddy"}) {
		t.Fatalf("%q", got)
	}
}

// An app that declares no backup and takes its name from the
// environment is named as the server names it only by luck: it is said
// as that, not under the name a default gives it here.
func TestAnUndeclaredAppNamedThroughTheEnvironment(t *testing.T) {
	fakeHotserve(t)
	file := filepath.Join(t.TempDir(), "Caddyfile")
	write(t, file, "app {$SHOP_NAME:shop}\n")
	got, err := Inspect(context.Background(), file, filepath.Dir(file))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(got.Undeclared, "shop") || !slices.Equal(got.UndeclaredByEnv, []string{"SHOP_NAME"}) {
		t.Fatalf("undeclared %q, by the environment %q", got.Undeclared, got.UndeclaredByEnv)
	}

	// A variable tried after it, that names nothing, is not blamed for it
	// — and one that also names an undeclared app is.
	write(t, file, "app {$SHOP_NAME:shop}\nemail {$Z_EMAIL:a@example.com}\n")
	got, err = Inspect(context.Background(), file, filepath.Dir(file))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.UndeclaredByEnv, []string{"SHOP_NAME"}) || len(got.Undeclared) != 0 {
		t.Fatalf("with an unrelated variable after it: undeclared %q, by the environment %q", got.Undeclared, got.UndeclaredByEnv)
	}
}

func second[T any](_ T, err error) error { return err }

// A root that depends on the environment is refused only where an app
// declares a backup: with none declared a run backs nothing up, and the
// root is not its business — `bin/push` on a box that uses no backups
// would otherwise refuse a Caddyfile that works today (the owner, 4a:
// decide in 4c). Such a plan carries no root at all, so that nothing
// downstream reads a placeholder as a path. What hides an app — an
// import through a variable, a glob that matches nothing — is refused
// as before, backup or no backup.
func TestAPlaceholderRootIsRefusedOnlyWhereABackupIsDeclared(t *testing.T) {
	fakeHotserve(t)
	for _, tc := range []struct {
		name, caddyfile string
		refused         string // in the error; empty means the plan is made
		root            string // of the plan made
	}{
		{"a defaulted root, a backup declared", "root {$LIVESWAP_ROOT:/var/lib/liveswap}\n", "LIVESWAP_ROOT", ""},
		{"a defaulted root, no backup declared", "# no app declares a backup\nroot {$LIVESWAP_ROOT:/var/lib/liveswap}\n", "", ""},
		{"a defaulted root, no app", "# no app\nroot {$LIVESWAP_ROOT:/var/lib/liveswap}\n", "", ""},
		{"a root with no default, a backup declared", "root {$ROOT_NO_DEFAULT}\n", "ROOT_NO_DEFAULT", ""},
		{"a root with no default, no backup declared", "# no app declares a backup\nroot {$ROOT_NO_DEFAULT}\n", "", ""},
		{"a literal root, no backup declared", "# no app declares a backup\nroot /var/lib/liveswap\n", "", ""},
		{"a literal root, a backup declared", "root /var/lib/liveswap\n", "", "/var/lib/liveswap"},
		{"an import through a variable, no backup declared", "# no app declares a backup\nimport sites/{$ENV:prod}.caddy\n", "ENV", ""},
		{"an empty glob, no backup declared", "# no app declares a backup\nimport nowhere/*.caddy\n", "matches no file", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "Caddyfile")
			write(t, file, tc.caddyfile)
			ins, err := Inspect(context.Background(), file, filepath.Dir(file))
			made, merr := Make(context.Background(), file)
			if tc.refused != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refused) || merr == nil || !strings.Contains(merr.Error(), tc.refused) {
					t.Fatalf("want a refusal naming %q: validate %v, run %v", tc.refused, err, merr)
				}
				return
			}
			if err != nil || merr != nil {
				t.Fatalf("refused: validate %v, run %v", err, merr)
			}
			if ins.Plan.Root != tc.root || made.Root != tc.root {
				t.Fatalf("root: validate %q, run %q, want %q", ins.Plan.Root, made.Root, tc.root)
			}
			if p := ins.Plan; tc.root == "" && len(p.Apps) != 0 {
				t.Fatalf("a plan with no root has apps: %+v", p.Apps)
			}
		})
	}
}
