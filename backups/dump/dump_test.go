package dump

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// inspect needs no sqlite3, so what it turns away is pinned here, in
// the lane that runs everywhere.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestInspect(t *testing.T) {
	shared := t.TempDir()
	outside := t.TempDir()
	db := append([]byte(header), make([]byte, 4080)...)
	must(t, os.WriteFile(filepath.Join(shared, "real.db"), db, 0o644))
	must(t, os.WriteFile(filepath.Join(outside, "outside.db"), db, 0o644))
	must(t, syscall.Mkfifo(filepath.Join(shared, "fifo.db"), 0o644))
	must(t, os.WriteFile(filepath.Join(shared, "empty.db"), nil, 0o644))
	must(t, os.WriteFile(filepath.Join(shared, "short.db"), []byte("SQLite"), 0o644))
	must(t, os.WriteFile(filepath.Join(shared, "text.db"), []byte("not a database, but longer than a header"), 0o644))
	must(t, os.Mkdir(filepath.Join(shared, "dir.db"), 0o755))
	must(t, os.Symlink("real.db", filepath.Join(shared, "link.db")))
	must(t, os.Symlink(filepath.Join(outside, "outside.db"), filepath.Join(shared, "out-link.db")))
	must(t, os.Mkdir(filepath.Join(shared, "sub"), 0o755))
	must(t, os.Symlink(outside, filepath.Join(shared, "sub", "escape")))
	must(t, os.Symlink(".", filepath.Join(shared, "sub", "inside")))

	for rel, want := range map[string]Class{
		"real.db":               OK,
		"sub/inside/../real.db": OK, // a directory link that stays inside is the app's own business
		"fifo.db":               NotADatabase,
		"empty.db":              NotADatabase,
		"short.db":              NotADatabase,
		"text.db":               NotADatabase,
		"dir.db":                NotADatabase,
		"link.db":               NotADatabase,
		"out-link.db":           NotADatabase,
		"absent.db":             Missing,
		"sub/escape/outside.db": Failed, // a way out of the shared dir is refused, not followed
	} {
		done := make(chan Result, 1)
		go func() { _, r := inspect(shared, rel); done <- r }()
		select {
		case res := <-done:
			if res.Class != want {
				t.Errorf("%s: %+v, want %s", rel, res, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: inspecting it blocked", rel)
		}
	}
}

func TestBoundGrowsWithTheDatabase(t *testing.T) {
	if got := boundFor(0); got != time.Minute {
		t.Errorf("an empty database: %s", got)
	}
	// 50 GiB at the measured ~200 MB/s is about four minutes.
	if got := boundFor(50 << 30); got < 10*time.Hour {
		t.Errorf("50 GiB is given %s", got)
	}
}
