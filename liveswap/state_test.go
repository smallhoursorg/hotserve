package liveswap

import (
	"os"
	"path/filepath"
	"strings"
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
		Nonce:          "0a1b2c3d0a1b2c3d",
		Handle: handleState{
			PID:       999,
			Command:   []string{"/usr/bin/node", "server.js"},
			StartedAt: time.Unix(1_700_000_000, 0).UTC(),
			Unit:      "hotserve-blog.v3.0a1b2c3d0a1b2c3d.service",
		},
		UpdatedAt: time.Unix(1_700_000_100, 0).UTC(),
	}
	must(t, store.save(want))
	got, ok, err := store.load()
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if got.CurrentVersion != want.CurrentVersion || got.Nonce != want.Nonce || got.Handle.Unit != want.Handle.Unit {
		t.Fatalf("round trip mismatch: %+v != %+v", got, want)
	}
	if !got.Handle.StartedAt.Equal(want.Handle.StartedAt) {
		t.Fatalf("started_at must survive the round trip: %s != %s", got.Handle.StartedAt, want.Handle.StartedAt)
	}
	// Only what Reattach needs is recorded. PID and Command are read
	// live from the manager and would be stale the moment they were
	// written, so they must not reach the file at all — asserted on
	// the bytes, because a json:"-" that is dropped still round-trips
	// as a zero value and would look identical from load() alone.
	raw, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"pid", "command", "node", "999"} {
		if strings.Contains(string(raw), key) {
			t.Fatalf("state.json must not record %q; it holds:\n%s", key, raw)
		}
	}
	if got.Handle.PID != 0 || got.Handle.Command != nil {
		t.Fatalf("a loaded record must carry no pid or command, got pid=%d command=%v", got.Handle.PID, got.Handle.Command)
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
	must(t, os.MkdirAll(filepath.Join(dir, ".hidden"), 0o755))
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
	if _, err := os.Stat(filepath.Join(dir, ".extract-v5")); !os.IsNotExist(err) {
		t.Error("orphaned staging dir should have been removed (deploys are serialized, so any .extract-* seen by GC is a crash leftover)")
	}
	if _, err := os.Stat(filepath.Join(dir, ".hidden")); err != nil {
		t.Error("non-staging dotdirs are not GC's business")
	}
	if _, err := os.Stat(filepath.Join(dir, "README")); err != nil {
		t.Error("plain files are not GC's business")
	}
}
