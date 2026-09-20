package backupdecl

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestValidateAccepts(t *testing.T) {
	cases := []struct {
		name string
		c    Config
	}{
		{"the two-line declaration", Config{SQLite: []string{"app.db"}, Files: []string{"uploads"}}},
		{"a database and nothing else", Config{SQLite: []string{"app.db"}}},
		{"files and nothing else", Config{Files: []string{"uploads"}}},
		{"the whole shared dir", Config{Files: []string{"."}}},
		{"a database inside a files path", Config{SQLite: []string{"data/app.db"}, Files: []string{"data"}}},
		{"a database inside the whole shared dir", Config{SQLite: []string{"app.db"}, Files: []string{"."}}},
		{"nested paths", Config{SQLite: []string{"db/main.sqlite3", "db/jobs.sqlite3"}, Files: []string{"media/uploads", "media/avatars"}}},
		{"a name that only starts like a sibling", Config{Files: []string{"uploads", "uploads-old"}}},
		{"spaces, colons, percent and dollar", Config{SQLite: []string{"my data/app:1.db"}, Files: []string{"50% $HOME"}}},
		{"dots that are not traversal", Config{Files: []string{"..data", ".cache", "a..b", "..."}}},
		// The neighbours of the control ranges, and ordinary non-ASCII.
		{"what sits beside the control characters", Config{Files: []string{"a b", "a~b", "a" + string(rune(0xa0)) + "b", "caf" + string(rune(0xe9)), "up" + string(rune(0x2028)) + "loads"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.c.Validate(); err != nil {
				t.Fatalf("rejected: %v", err)
			}
		})
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name string
		c    Config
		want string
	}{
		{"nothing declared", Config{}, "declares nothing"},
		{"empty path", Config{Files: []string{""}}, "is empty"},
		{"absolute path", Config{SQLite: []string{"/var/lib/liveswap/blog/shared/app.db"}}, "relative to the app's shared dir"},
		{"the shared_dir placeholder", Config{SQLite: []string{"{shared_dir}/app.db"}}, "placeholder"},
		{"an env placeholder", Config{Files: []string{"{env.UPLOADS}"}}, "placeholder"},
		{"a lone brace", Config{Files: []string{"up}loads"}}, "placeholder"},
		{"parent traversal", Config{Files: []string{"../shop/shared"}}, "outside the app's shared dir"},
		{"bare parent", Config{Files: []string{".."}}, "outside the app's shared dir"},
		{"traversal hidden mid-path", Config{Files: []string{"uploads/../../shop"}}, "outside the app's shared dir"},
		{"a detour that stays inside", Config{Files: []string{"uploads/../media"}}, `"media"`},
		{"trailing slash", Config{Files: []string{"uploads/"}}, `"uploads"`},
		{"leading dot-slash", Config{SQLite: []string{"./app.db"}}, `"app.db"`},
		{"doubled separator", Config{Files: []string{"media//uploads"}}, "clean"},
		{"a newline", Config{Files: []string{"up\nloads"}}, "control character"},
		{"a NUL", Config{SQLite: []string{"app\x00.db"}}, "control character"},
		{"too long", Config{SQLite: []string{strings.Repeat("d/", 200) + "x.db"}}, "at most"},
		{"DEL", Config{Files: []string{"up" + string(rune(0x7f)) + "loads"}}, "control character"},
		// C1 controls are valid UTF-8, so the UTF-8 rule does not catch
		// them: NEL is a line break to whatever prints the path.
		{"a C1 control, NEL", Config{Files: []string{"up" + string(rune(0x85)) + "loads"}}, "control character"},
		{"the last C1 control", Config{Files: []string{"up" + string(rune(0x9f)) + "loads"}}, "control character"},
		{"not UTF-8", Config{Files: []string{"up\xffloads"}}, "UTF-8"},
		{"a database that is the shared dir", Config{SQLite: []string{"."}}, "names a directory"},
		{"the same database twice", Config{SQLite: []string{"app.db", "app.db"}}, "more than once"},
		{"the same files path twice", Config{Files: []string{"uploads", "uploads"}}, "more than once"},
		{"one path as both kinds", Config{SQLite: []string{"app.db"}, Files: []string{"app.db"}}, "both"},
		{"files inside files", Config{Files: []string{"media", "media/uploads"}}, "already covered"},
		{"files inside the whole shared dir", Config{Files: []string{".", "uploads"}}, "already covered"},
		{"files under a database", Config{SQLite: []string{"app.db"}, Files: []string{"app.db/pages"}}, "is a database"},
		{"a database under a database", Config{SQLite: []string{"app.db", "app.db/inner.db"}}, "is a database"},
		{"a database's own wal as files", Config{SQLite: []string{"app.db"}, Files: []string{"app.db-wal"}}, "belongs to the database"},
		{"a database's own journal as a database", Config{SQLite: []string{"app.db", "app.db-journal"}}, "belongs to the database"},
		{"a database's own shm as files", Config{SQLite: []string{"app.db"}, Files: []string{"app.db-shm"}}, "belongs to the database"},
		{"a path under a database's wal", Config{SQLite: []string{"app.db"}, Files: []string{"app.db-wal/segments"}}, "belongs to the database"},
		// The same rules with the paths declared the other way round:
		// each is one the pairwise check reaches only through its
		// second ordering.
		{"a wal declared before its database", Config{SQLite: []string{"app.db-wal", "app.db"}}, `sqlite "app.db-wal" belongs to the database "app.db"`},
		{"a database declared after one under it", Config{SQLite: []string{"app.db/inner.db", "app.db"}}, `sqlite "app.db/inner.db" is under "app.db"`},
		{"files declared before the files that cover them", Config{Files: []string{"media/uploads", "media"}}, `files "media/uploads" is already covered by files "media"`},
		{"files declared before the whole shared dir", Config{Files: []string{"uploads", "."}}, `files "uploads" is already covered by files "."`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// The error is read by whoever pushed the Caddyfile, so it names the
// line they wrote: the kind and the path, as written.
func TestValidateNamesTheOffendingLine(t *testing.T) {
	err := (&Config{SQLite: []string{"ok.db"}, Files: []string{"fine", "/etc"}}).Validate()
	if err == nil || !strings.Contains(err.Error(), `files "/etc"`) {
		t.Fatalf("want the error to name `files \"/etc\"`, got %v", err)
	}
}

// What the package doc promises, and the reason the package exists:
// a program that shares these rules must not have to link Caddy.
func TestImportsOnlyTheStandardLibrary(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no source files found: %v", err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if first, _, _ := strings.Cut(p, "/"); strings.Contains(first, ".") {
				t.Errorf("%s imports %s, which is not in the standard library", f, p)
			}
		}
	}
}
