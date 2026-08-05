package hotswap

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestFileStateStoreRoundTrip(t *testing.T) {
	store := &fileStateStore{path: filepath.Join(t.TempDir(), "app", "state.json")}

	if _, ok, err := store.load(); ok || err != nil {
		t.Fatalf("missing file must be (ok=false, err=nil), got ok=%v err=%v", ok, err)
	}

	want := appState{
		CurrentVersion: "v3",
		Port:           8123,
		Handle:         handleState{PID: 999, StartedAt: time.Unix(1_700_000_000, 0).UTC()},
		UpdatedAt:      time.Unix(1_700_000_100, 0).UTC(),
	}
	must(t, store.save(want))
	got, ok, err := store.load()
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if got.CurrentVersion != want.CurrentVersion || got.Port != want.Port || got.Handle.PID != want.Handle.PID {
		t.Fatalf("round trip mismatch: %+v != %+v", got, want)
	}
	// No stray temp file left behind.
	if _, err := os.Stat(store.path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file left behind after save")
	}
}

func TestFileStateStoreCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	must(t, os.WriteFile(path, []byte("{nope"), 0o600))
	store := &fileStateStore{path: path}
	if _, _, err := store.load(); err == nil {
		t.Fatal("corrupt state must error, not silently reset")
	}
}

func TestGCReleases(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-time.Hour)
	// v1 oldest ... v4 newest, plus a staging dir and a stray file.
	for i, v := range []string{"v1", "v2", "v3", "v4"} {
		p := filepath.Join(dir, v)
		must(t, os.MkdirAll(p, 0o755))
		mt := base.Add(time.Duration(i) * time.Minute)
		must(t, os.Chtimes(p, mt, mt))
	}
	must(t, os.MkdirAll(filepath.Join(dir, ".extract-v5"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "README"), []byte("x"), 0o600))

	// keep 2, protect the OLDEST (simulating a rollback still serving v1).
	gcReleases(dir, 2, "v1", zap.NewNop())

	for _, v := range []string{"v1", "v3", "v4"} {
		if _, err := os.Stat(filepath.Join(dir, v)); err != nil {
			t.Errorf("%s should survive: %v", v, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "v2")); !os.IsNotExist(err) {
		t.Error("v2 should have been pruned")
	}
	if _, err := os.Stat(filepath.Join(dir, ".extract-v5")); err != nil {
		t.Error("staging dirs are not GC's business")
	}
	if _, err := os.Stat(filepath.Join(dir, "README")); err != nil {
		t.Error("plain files are not GC's business")
	}
}
