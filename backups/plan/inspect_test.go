package plan

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// What validate says beyond the plan: the apps that declare no backup —
// a new app's block with the two lines forgotten is otherwise silent —
// and the files the Caddyfile imports, since a run reads them from
// inside a view that holds /etc/hotserve and nothing else.
func TestInspectNamesTheAppsThatDeclareNoBackup(t *testing.T) {
	fakeHotserve(t)
	file := filepath.Join(t.TempDir(), "Caddyfile")
	write(t, file, "app blog\napp shop\n")
	got, err := Inspect(context.Background(), file)
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
	got, err := Inspect(context.Background(), file)
	if err != nil || len(got.Undeclared) != 0 || len(got.Imports) != 0 {
		t.Fatalf("%+v, %v", got, err)
	}
}

func TestInspectListsWhatIsImportedAndFromWhere(t *testing.T) {
	fakeHotserve(t)
	dir, elsewhere := t.TempDir(), t.TempDir()
	file := filepath.Join(dir, "Caddyfile")
	write(t, filepath.Join(dir, "sites", "blog.caddy"), "import deeper.caddy\n")
	write(t, filepath.Join(dir, "sites", "deeper.caddy"), "# nothing\n")
	write(t, filepath.Join(elsewhere, "x.caddy"), "# nothing\n")
	write(t, file, "import sites/blog.caddy\nimport "+filepath.Join(elsewhere, "x.caddy")+"\nimport (snippet)\napp blog\n")
	got, err := Inspect(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, imp := range got.Imports {
		files = append(files, imp.Files...)
	}
	want := []string{filepath.Join(dir, "sites", "blog.caddy"), filepath.Join(dir, "sites", "deeper.caddy"), filepath.Join(elsewhere, "x.caddy")}
	slices.Sort(files)
	slices.Sort(want)
	if !slices.Equal(files, want) {
		t.Fatalf("imported files: %q, want %q", files, want)
	}
	if out := got.ImportsOutside(dir, dir); !slices.Equal(out, []string{filepath.Join(elsewhere, "x.caddy")}) {
		t.Fatalf("outside %s: %q", dir, out)
	}
}

// What is outside is judged by how the import is written, before any
// file it matches: inside a run's view a glob that reaches outside
// matches nothing, which to the adapter is no error — the apps declared
// out there would just not be in the plan [Caddy: a glob matching
// nothing is a warning].
func TestAnImportIsOutsideByHowItIsWritten(t *testing.T) {
	fakeHotserve(t)
	dir, config := t.TempDir(), t.TempDir()
	for name, tc := range map[string]struct {
		line    string
		outside bool
	}{
		"relative, beside the Caddyfile":          {"import sites/*.caddy", false},
		"a snippet":                               {"import logging", false},
		"relative, climbing out":                  {"import ../apps/*.caddy", true},
		"relative, climbing out through a glob":   {"import sites/*/../../../apps/*.caddy", true},
		"absolute, in the config directory":       {"import " + config + "/sites/*.caddy", false},
		"absolute, matching nothing, outside":     {"import /srv/apps/*.caddy", true},
		"absolute, beside the copy being checked": {"import " + dir + "/sites/*.caddy", true},
		"a directory that only begins the same":   {"import " + config + "-old/*.caddy", true},
	} {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(dir, "Caddyfile")
			write(t, file, tc.line+"\napp blog\n")
			got, err := Inspect(context.Background(), file)
			if err != nil {
				t.Fatal(err)
			}
			if out := got.ImportsOutside(config, dir); (len(out) > 0) != tc.outside {
				t.Fatalf("outside: %q, want outside = %v", out, tc.outside)
			}
		})
	}
}

// A link inside the directory to a file outside it is outside: a run's
// view holds the link and not what it leads to.
func TestAnImportedLinkIsOutsideByWhereItLeads(t *testing.T) {
	fakeHotserve(t)
	dir, elsewhere := t.TempDir(), t.TempDir()
	write(t, filepath.Join(elsewhere, "x.caddy"), "# nothing\n")
	if err := os.Symlink(filepath.Join(elsewhere, "x.caddy"), filepath.Join(dir, "x.caddy")); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "Caddyfile")
	write(t, file, "import x.caddy\napp blog\n")
	got, err := Inspect(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	if out := got.ImportsOutside(dir, dir); len(out) != 1 {
		t.Fatalf("outside: %q", out)
	}
}

// The run's own plan step refuses it too: validate is a courtesy, the
// plan unit is what stands between such an import and a smaller plan.
func TestMakeRefusesAnImportFromOutsideTheCaddyfilesDirectory(t *testing.T) {
	fakeHotserve(t)
	file := filepath.Join(t.TempDir(), "Caddyfile")
	write(t, file, "import /srv/apps/*.caddy\napp blog\n")
	if _, err := Make(context.Background(), file); err == nil || !strings.Contains(err.Error(), "/srv/apps/*.caddy") || !strings.Contains(err.Error(), filepath.Dir(file)) {
		t.Fatalf("Make: %v", err)
	}
	write(t, file, "import sites/*.caddy\napp blog\n")
	if _, err := Make(context.Background(), file); err != nil {
		t.Fatalf("an import beside the Caddyfile: %v", err)
	}
}

// Everything Make refuses, Inspect refuses in the same words: validate
// must not pass a Caddyfile a run would then fail on.
func TestInspectRefusesWhatMakeRefuses(t *testing.T) {
	fakeHotserve(t)
	file := filepath.Join(t.TempDir(), "Caddyfile")
	write(t, file, "root {$LIVESWAP_ROOT:/var/lib/liveswap}\napp blog\n")
	_, makeErr := Make(context.Background(), file)
	got, err := Inspect(context.Background(), file)
	if makeErr == nil || err == nil || err.Error() != makeErr.Error() || !strings.Contains(err.Error(), "LIVESWAP_ROOT") {
		t.Fatalf("Inspect: %v\nMake:    %v", err, makeErr)
	}
	if got != nil {
		t.Fatalf("a refusal came with an answer: %+v", got)
	}
	if _, err := Inspect(context.Background(), filepath.Join(t.TempDir(), "absent")); err == nil || !strings.Contains(err.Error(), "absent") {
		t.Fatalf("a file that is not there: %v", err)
	}
}

// The plan unit runs as an account that owns nothing under the config
// directory and is in no group that does: what it reads, it reads as
// "other". A file closed to others fails every run; a directory closed
// to others makes its glob match nothing, and the run plan without it.
func TestImportsClosedToTheAccountARunReadsThemAs(t *testing.T) {
	fakeHotserve(t)
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Chmod(dir, 0o755))
	write(t, filepath.Join(dir, "open", "a.caddy"), "# nothing\n")
	write(t, filepath.Join(dir, "open", "closed.caddy"), "# nothing\n")
	write(t, filepath.Join(dir, "shut", "b.caddy"), "# nothing\n")
	write(t, filepath.Join(dir, "noglob", "c.caddy"), "# nothing\n")
	must(os.Chmod(filepath.Join(dir, "open"), 0o755))
	must(os.Chmod(filepath.Join(dir, "open", "a.caddy"), 0o644))
	must(os.Chmod(filepath.Join(dir, "open", "closed.caddy"), 0o640))
	must(os.Chmod(filepath.Join(dir, "shut", "b.caddy"), 0o644))
	must(os.Chmod(filepath.Join(dir, "shut"), 0o750))
	must(os.Chmod(filepath.Join(dir, "noglob", "c.caddy"), 0o644))
	must(os.Chmod(filepath.Join(dir, "noglob"), 0o751)) // may be walked through, not listed: a glob finds nothing
	// Closed, and nothing in it matches — as it looks to an administrator
	// who may not list it either: only the pattern says it is there.
	must(os.MkdirAll(filepath.Join(dir, "unseen", "deeper"), 0o755))
	must(os.Chmod(filepath.Join(dir, "unseen"), 0o750))
	file := filepath.Join(dir, "Caddyfile")
	write(t, file, "import open/*.caddy\nimport shut/*.caddy\nimport noglob/*.caddy\nimport unseen/deeper/*/*.caddy\napp blog\n")
	got, err := Inspect(context.Background(), file)
	must(err)
	closed := got.ImportsClosedToOthers(dir)
	want := []string{filepath.Join(dir, "noglob"), filepath.Join(dir, "open", "closed.caddy"), filepath.Join(dir, "shut"), filepath.Join(dir, "unseen")}
	slices.Sort(closed)
	if !slices.Equal(closed, want) {
		t.Fatalf("closed to others: %q, want %q", closed, want)
	}
	// A copy being checked somewhere else says nothing of the modes on
	// the box.
	if closed := got.ImportsClosedToOthers(t.TempDir()); len(closed) != 0 {
		t.Fatalf("outside the config directory: %q", closed)
	}
}

// A line that begins with "import" inside a quoted token — a respond
// body, a heredoc — is text, not an import: the refusal of imports from
// outside must not be set off by a page's own JavaScript.
func TestAnImportInsideAQuotedTokenIsNotAnImport(t *testing.T) {
	fakeHotserve(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "Caddyfile")
	for name, body := range map[string]string{
		"backquoted": "respond `\nimport \"/static/app.js\"\n`\napp blog\n",
		"quoted":     "respond \"\nimport /static/app.js\n\"\napp blog\n",
		"heredoc":    "respond <<JS\nimport \"/static/app.js\"\nJS\napp blog\n",
		"a comment":  "# import /srv/apps/*.caddy\napp blog\n",
	} {
		t.Run(name, func(t *testing.T) {
			write(t, file, body)
			got, err := Inspect(context.Background(), file)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Imports) != 0 {
				t.Fatalf("read as imports: %+v", got.Imports)
			}
			if _, err := Make(context.Background(), file); err != nil {
				t.Fatalf("Make: %v", err)
			}
		})
	}
	// And after a quoted token closes, an import is an import again.
	write(t, file, "respond `\nimport \"/static/app.js\"\n`\nimport /srv/apps/*.caddy\napp blog\n")
	got, err := Inspect(context.Background(), file)
	if err != nil || len(got.Imports) != 1 || got.Imports[0].Pattern != "/srv/apps/*.caddy" {
		t.Fatalf("%+v, %v", got, err)
	}
}

// A directory under the config directory that is a link leading out of
// it: inside a run's view the link dangles, its glob matches nothing,
// and nothing but the pattern's own directories says so — there, and
// here, where it resolves.
func TestAnImportThroughALinkedDirectoryIsOutside(t *testing.T) {
	fakeHotserve(t)
	dir, elsewhere := t.TempDir(), t.TempDir()
	write(t, filepath.Join(elsewhere, "blog.caddy"), "# nothing\n")
	if err := os.Symlink(elsewhere, filepath.Join(dir, "sites")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "nowhere"), filepath.Join(dir, "dangling")); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "Caddyfile")
	for _, line := range []string{"import sites/*.caddy", "import sites/none-match-*.caddy", "import dangling/*.caddy"} {
		write(t, file, line+"\napp blog\n")
		got, err := Inspect(context.Background(), file)
		if err != nil {
			t.Fatal(err)
		}
		if out := got.ImportsOutside(dir, dir); len(out) == 0 {
			t.Errorf("%s: not outside", line)
		}
		if _, err := Make(context.Background(), file); err == nil {
			t.Errorf("%s: Make took it", line)
		}
	}
	write(t, file, "import not-there-yet/*.caddy\napp blog\n")
	if _, err := Make(context.Background(), file); err != nil {
		t.Errorf("a directory that is simply not there: %v", err)
	}
}

// A directory a wildcard matches can be a link out as well: from inside
// a run's view it dangles and the glob matches nothing, and on the host,
// where the adapter follows it, it may match nothing yet. So each
// directory the pattern leads through is followed, a segment at a time.
func TestAnImportThroughAWildcardsLinkedDirectoryIsOutside(t *testing.T) {
	fakeHotserve(t)
	for name, tc := range map[string]struct {
		link, to, line string
		outside        bool
	}{
		"dangling, as in a run's view":      {"ext", "nowhere", "import */*.caddy", true},
		"out, matching nothing yet":         {"ext", "", "import */none-match-*.caddy", true},
		"out, after a literal segment":      {"sites/a/conf", "", "import sites/*/conf/*.caddy", true},
		"out, a segment after the wildcard": {"sites/a", "", "import sites/*/conf/*.caddy", true},
		"in, to a directory beside it":      {"ext", "real", "import */*.caddy", false},
	} {
		t.Run(name, func(t *testing.T) {
			dir, elsewhere := t.TempDir(), t.TempDir()
			write(t, filepath.Join(elsewhere, "conf", "x.caddy"), "# nothing\n")
			write(t, filepath.Join(dir, "real", "x.caddy"), "# nothing\n")
			to := elsewhere
			if tc.to != "" {
				to = filepath.Join(dir, tc.to)
			}
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, tc.link)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(to, filepath.Join(dir, tc.link)); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(dir, "Caddyfile")
			write(t, file, tc.line+"\napp blog\n")
			got, err := Inspect(context.Background(), file)
			if err != nil {
				t.Fatal(err)
			}
			if out := got.ImportsOutside(dir, dir); (len(out) > 0) != tc.outside {
				t.Fatalf("outside: %q, want outside = %v", out, tc.outside)
			}
			if _, err := Make(context.Background(), file); (err != nil) != tc.outside {
				t.Fatalf("Make: %v", err)
			}
		})
	}
}

// The config directory itself, and a directory a glob leads through.
func TestClosedDirectoriesAboveAndWithinAGlob(t *testing.T) {
	fakeHotserve(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "sites", "x", "conf", "a.caddy"), "# nothing\n")
	write(t, filepath.Join(dir, "sites", "y", "conf", "README"), "no caddy file here\n")
	for p, mode := range map[string]os.FileMode{"sites": 0o755, "sites/x": 0o755, "sites/x/conf": 0o755, "sites/x/conf/a.caddy": 0o644, "sites/y": 0o755, "sites/y/conf": 0o700} {
		if err := os.Chmod(filepath.Join(dir, p), mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "Caddyfile")
	write(t, file, "import sites/*/conf/*.caddy\napp blog\n")
	got, err := Inspect(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	closed := got.ImportsClosedToOthers(dir)
	want := []string{dir, filepath.Join(dir, "sites", "y", "conf")}
	slices.Sort(want)
	if !slices.Equal(closed, want) {
		t.Fatalf("closed: %q, want %q", closed, want)
	}
}

// Lines the import reader has to agree with the adapter's lexer about:
// a miss here is an import from outside that nothing refuses.
func TestImportsAsTheAdapterReadsThem(t *testing.T) {
	fakeHotserve(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "Caddyfile")
	for name, tc := range map[string]struct {
		body string
		want []string
	}{
		"continued on the next line":   {"import \\\n\t/srv/apps/*.caddy\napp blog\n", []string{"/srv/apps/*.caddy"}},
		"CRLF":                         {"import /srv/apps/*.caddy\r\napp blog\r\n", []string{"/srv/apps/*.caddy"}},
		"an escaped quote in the path": {"import \"/srv/a\\\"b/*.caddy\"\napp blog\n", []string{"/srv/a\"b/*.caddy"}},
		"in a snippet":                 {"(apps) {\n\timport /srv/apps/*.caddy\n}\napp blog\n", []string{"/srv/apps/*.caddy"}},
		"a heredoc closed by the word": {"respond <<import\nimport /static/app.js\nimport\napp blog\n", nil},
		"a CRLF heredoc":               {"respond <<JS\r\nimport /static/app.js\r\nJS\r\nimport /srv/apps/*.caddy\r\n", []string{"/srv/apps/*.caddy"}},
		// Caddy's own documented form: the marker ends the heredoc, and the
		// rest of its line is tokens.
		"a heredoc closed with more on the line": {"respond <<HTML\n\t<p>hi</p>\n\tHTML 200\nimport /srv/apps/*.caddy\napp blog\n", []string{"/srv/apps/*.caddy"}},
		"a marker that is part of a word":        {"respond <<END\nthe END 200\nimport /srv/apps/*.caddy\n", []string{"/srv/apps/*.caddy"}},
		"<< and a space is a token":              {"respond << x\nimport /srv/apps/*.caddy\n", []string{"/srv/apps/*.caddy"}},
	} {
		t.Run(name, func(t *testing.T) {
			write(t, file, tc.body)
			got, err := Inspect(context.Background(), file)
			if err != nil {
				t.Fatal(err)
			}
			var patterns []string
			for _, imp := range got.Imports {
				patterns = append(patterns, imp.Pattern)
			}
			if !slices.Equal(patterns, tc.want) {
				t.Fatalf("imports: %q, want %q", patterns, tc.want)
			}
		})
	}
}

// A link among the imported files that leads somewhere else under the
// config directory: it is what it leads to, and the way there, that the
// run's account has to be able to read.
func TestAnImportedLinkIsReadWhereItLeads(t *testing.T) {
	fakeHotserve(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "private", "x.caddy"), "# nothing\n")
	write(t, filepath.Join(dir, "open", "README"), "\n")
	for p, mode := range map[string]os.FileMode{"": 0o755, "open": 0o755, "private": 0o750, "private/x.caddy": 0o644} {
		if err := os.Chmod(filepath.Join(dir, p), mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../private/x.caddy", filepath.Join(dir, "open", "x.caddy")); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "Caddyfile")
	write(t, file, "import open/*.caddy\napp blog\n")
	got, err := Inspect(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	if closed := got.ImportsClosedToOthers(dir); !slices.Equal(closed, []string{filepath.Join(dir, "private")}) {
		t.Fatalf("closed: %q", closed)
	}
}

// An app that declares no backup and takes its name from the
// environment is named as the server names it only by luck: it is said
// as that, not under the name a default gives it here.
func TestAnUndeclaredAppNamedThroughTheEnvironment(t *testing.T) {
	fakeHotserve(t)
	file := filepath.Join(t.TempDir(), "Caddyfile")
	write(t, file, "app {$SHOP_NAME:shop}\n")
	got, err := Inspect(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(got.Undeclared, "shop") || !slices.Equal(got.UndeclaredByEnv, []string{"SHOP_NAME"}) {
		t.Fatalf("undeclared %q, by the environment %q", got.Undeclared, got.UndeclaredByEnv)
	}

	// A variable tried after it, that names nothing, is not blamed for it
	// — and one that also names an undeclared app is.
	write(t, file, "app {$SHOP_NAME:shop}\nemail {$Z_EMAIL:a@example.com}\n")
	got, err = Inspect(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.UndeclaredByEnv, []string{"SHOP_NAME"}) || len(got.Undeclared) != 0 {
		t.Fatalf("with an unrelated variable after it: undeclared %q, by the environment %q", got.Undeclared, got.UndeclaredByEnv)
	}
}

// An import whose path is a snippet's argument is a path this reader
// does not know: the adapter fills it in from wherever the snippet is
// used, and one of those uses can name files outside the directory —
// which a run would then plan without. It is refused, by the line.
func TestAnImportByASnippetArgumentIsRefused(t *testing.T) {
	fakeHotserve(t)
	file := filepath.Join(t.TempDir(), "Caddyfile")
	for _, body := range []string{
		"(apps) {\n\timport {args[0]}\n}\nimport apps /srv/apps/*.caddy\napp blog\n",
		"(apps) {\n\timport sites/{args[1]}/*.caddy\n}\napp blog\n",
		"(apps) {\n\timport {args.0}\n}\napp blog\n",
		"(apps) {\n\timport \"{args[:]}\"\n}\napp blog\n",
	} {
		write(t, file, body)
		for what, err := range map[string]error{"Inspect": second(Inspect(context.Background(), file)), "Make": second(Make(context.Background(), file))} {
			if err == nil || !strings.Contains(err.Error(), "{args") || !strings.Contains(err.Error(), "literally") {
				t.Errorf("%s of %q: %v", what, body, err)
			}
		}
	}
	// A snippet's argument anywhere else is nobody's business here, and
	// neither is a snippet used with arguments that imports nothing by them.
	write(t, file, "(site) {\n\trespond {args[0]}\n\timport sites/*.caddy\n}\nimport site hello\napp blog\n")
	if _, err := Make(context.Background(), file); err != nil {
		t.Errorf("an argument that is not an import's path: %v", err)
	}
}

// An import whose path is a heredoc is followed by the adapter like any
// other [measured: `import <<PATH` / an outside glob / `PATH` adapts
// with what that glob matches], and its body is quoted text to this
// reader. It is refused, by the line.
func TestAnImportByAHeredocIsRefused(t *testing.T) {
	fakeHotserve(t)
	file := filepath.Join(t.TempDir(), "Caddyfile")
	for _, body := range []string{
		"import <<PATH\n/srv/apps/*.caddy\nPATH\napp blog\n",
		"import <<PATH\r\n/srv/apps/*.caddy\r\nPATH\r\napp blog\r\n",
		"(apps) {\n\timport <<P\n\tsites/*.caddy\n\tP\n}\napp blog\n",
	} {
		write(t, file, body)
		for what, err := range map[string]error{"Inspect": second(Inspect(context.Background(), file)), "Make": second(Make(context.Background(), file))} {
			if err == nil || !strings.Contains(err.Error(), "heredoc") || !strings.Contains(err.Error(), "literally") {
				t.Errorf("%s of %q: %v", what, body, err)
			}
		}
	}
}

func second[T any](_ T, err error) error { return err }

// A directory a wildcard leads through that whoever is asking may not
// enter either: from there a glob of the whole way down simply does not
// match what is under it, so each segment is matched, and looked at,
// before the next. Root enters any directory, so under root the test
// builds the tree and runs itself again as an unprivileged uid — as an
// administrator's validate is.
func TestAClosedDirectoryOnAWildcardsWayIsSeenWithoutPrivilege(t *testing.T) {
	const asUID = 65534
	base := os.Getenv("PLAN_TEST_CONFIG_DIR")
	if base == "" {
		var err error
		if base, err = os.MkdirTemp("/var/tmp", "plan-closed-"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(base) })
		write(t, filepath.Join(base, "sites", "open", "conf", "a.caddy"), "# nothing\n")
		write(t, filepath.Join(base, "sites", "private", "conf", "b.caddy"), "app shop {\n}\n")
		write(t, filepath.Join(base, "Caddyfile"), "import sites/*/conf/*.caddy\napp blog\n")
		for p, mode := range map[string]os.FileMode{
			"": 0o755, "Caddyfile": 0o644, "sites": 0o755,
			"sites/open": 0o755, "sites/open/conf": 0o755, "sites/open/conf/a.caddy": 0o644,
			"sites/private/conf": 0o755, "sites/private/conf/b.caddy": 0o644, "sites/private": 0o700,
		} {
			if err := os.Chmod(filepath.Join(base, p), mode); err != nil {
				t.Fatal(err)
			}
		}
		if os.Getuid() == 0 {
			// The test binary sits where only root can go; a copy the uid
			// can run, outside the tree it is about to look at.
			bin, err := os.MkdirTemp("/var/tmp", "plan-closed-bin-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(bin) })
			self, err := os.ReadFile(os.Args[0])
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bin, "plan.test"), self, 0o755); err != nil {
				t.Fatal(err)
			}
			child := exec.Command(filepath.Join(bin, "plan.test"), "-test.run=^TestAClosedDirectoryOnAWildcardsWayIsSeenWithoutPrivilege$", "-test.v")
			child.Env = append(os.Environ(), "PLAN_TEST_CONFIG_DIR="+base)
			child.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: asUID, Gid: asUID}}
			out, err := child.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "--- PASS") {
				t.Fatalf("as uid %d: %v\n%s", asUID, err, out)
			}
			return
		}
	} else if _, err := os.ReadDir(filepath.Join(base, "sites", "private")); err == nil {
		t.Fatalf("fixture: uid %d can list sites/private: the test proves nothing", os.Getuid())
	}
	fakeHotserve(t)
	got, err := Inspect(context.Background(), filepath.Join(base, "Caddyfile"))
	if err != nil {
		t.Fatal(err)
	}
	if closed := got.ImportsClosedToOthers(base); !slices.Equal(closed, []string{filepath.Join(base, "sites", "private")}) {
		t.Fatalf("closed to others: %q, want sites/private alone", closed)
	}
}
