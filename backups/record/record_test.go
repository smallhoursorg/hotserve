package record

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestWriteThenRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	if s, err := Read(path); err != nil || s.Apps == nil || len(s.Apps) != 0 {
		t.Fatalf("before any run: %+v, %v", s, err)
	}
	when := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	want := &Status{Started: when, Finished: when.Add(time.Minute), Root: "/var/lib/liveswap", Apps: map[string]*App{
		"blog": {Class: OK, Looked: "/var/lib/liveswap/blog/shared", Snapshot: &Snapshot{ID: "abc", Time: when},
			Items: []Item{{Kind: "sqlite", Path: "app.db", OK: true}}, LastOK: &Snapshot{ID: "abc", Time: when}},
	}}
	for i := 0; i < 2; i++ { // the second write replaces the first
		if err := Write(path, want); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Read(path)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("read back %+v, %v", got, err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o644 {
		t.Errorf("mode %v: the record is for anyone to read", st.Mode().Perm())
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".status-*")); len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
}

func TestReadRefusesWhatIsNotARecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path); err == nil {
		t.Fatal("garbage read as a record")
	}
}

// Every command prints a snapshot by its first eight characters, and a
// time to the minute in UTC — the zone the record and every restic unit
// keep — whatever zone the time was read in: one form, so that status,
// a run and a restore name the same snapshot and the same minute alike.
func TestASnapshotAndATimeArePrintedOneWay(t *testing.T) {
	for id, want := range map[string]string{
		"5f422bce75a41b7f86515ed9050c3d535df6b5728b7815f6c146eb774197d91f": "5f422bce",
		"5f42":                      "5f42",
		"\x1b[2J5f422bce75a41b7f86": "[2J5f422",
	} {
		if got := Short(id); got != want {
			t.Errorf("Short(%q) = %q, want %q", id, got, want)
		}
	}
	// As JSON gives back a time written at +02:00: a zone with no name.
	berlin := time.FixedZone("", 2*60*60)
	for _, at := range []time.Time{
		time.Date(2026, 10, 1, 12, 34, 56, 0, time.UTC),
		time.Date(2026, 10, 1, 14, 34, 56, 0, berlin),
	} {
		if got := When(at); got != "2026-10-01 12:34 UTC" {
			t.Errorf("When(%s) = %q, want 2026-10-01 12:34 UTC", at, got)
		}
	}
}
