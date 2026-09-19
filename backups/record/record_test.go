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
