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
