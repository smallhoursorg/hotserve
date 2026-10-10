package liveswap

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// Rule 4 (redact.go): a directory under releases/ is a release only
// under a version's name. Anything else — a staging dir, a dotfile, a
// name nothing could deploy or roll back to — is not listed, so it
// is neither offered to an operator nor put on the filter's safe list.
func TestListReleasesSkipsNamesThatAreNotVersions(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"v1", "2026.09.14", ".extract-abc", "a b", "..x", strings.Repeat("v", 65)} {
		must(t, os.MkdirAll(filepath.Join(dir, n), 0o755))
	}
	must(t, os.WriteFile(filepath.Join(dir, "v9"), nil, 0o600)) // a file, not a release
	got := listReleases(dir)
	sort.Strings(got)
	if strings.Join(got, ",") != "2026.09.14,v1" {
		t.Fatalf("listReleases = %v", got)
	}
}

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
	// No stray temp file left behind: state.json is all there is.
	assertOnlyEntries(t, filepath.Dir(store.path), "state.json")
	if fi, err := os.Lstat(store.path); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state.json must be a regular file, mode 0600: %v %v", fi, err)
	}
}

// assertOnlyEntries fails unless dir holds exactly the names given.
func assertOnlyEntries(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	must(t, err)
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	sort.Strings(got)
	sort.Strings(names)
	if strings.Join(got, ",") != strings.Join(names, ",") {
		t.Fatalf("%s holds %v, want %v", dir, got, names)
	}
}

// The state at its largest — the longest version and app name, both
// timestamps with nanoseconds and a zone — sits far under the read
// bound, so the bound refuses nothing hotserve wrote; and it loads.
func TestStateMaxBytesBoundsTheLargestState(t *testing.T) {
	version := strings.Repeat("v", 64)
	app := strings.Repeat("a", appNameMaxLen)
	if !validVersion(version) || !appNameRe.MatchString(app) {
		t.Fatal("the fixture must be the longest valid version and app name")
	}
	stamp := time.Date(2026, 10, 10, 12, 34, 56, 123456789, time.FixedZone("", -34200)) // -09:30: a zone, with minutes
	st := appState{
		CurrentVersion: version,
		Nonce:          "0a1b2c3d0a1b2c3d",
		Handle:         handleState{StartedAt: stamp, Unit: unitPrefix + app + "." + version + ".0a1b2c3d0a1b2c3d.service"},
		UpdatedAt:      stamp,
	}
	store := &fileStateStore{path: filepath.Join(t.TempDir(), "state.json")}
	must(t, store.save(st))
	fi, err := os.Stat(store.path)
	must(t, err)
	if fi.Size() >= 512 { // what stateMaxBytes's margin is reckoned against
		t.Fatalf("the largest state is %d bytes; stateMaxBytes (%d) assumes under 512", fi.Size(), stateMaxBytes)
	}
	if got, ok, err := store.load(); err != nil || !ok || got.CurrentVersion != version {
		t.Fatalf("the largest state must load: ok=%v err=%v", ok, err)
	}
}

// Rule 4 (deploys.go) for state.json's temp file: an earlier hotserve
// wrote through the fixed state.json.tmp, so a link planted there —
// or at a name of save's own temp pattern — must not have the next
// save write through it. Save succeeds, the targets are untouched, and
// the links are gone.
func TestFileStateStoreSaveDoesNotWriteThroughAPlantedTempLink(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "app")
	must(t, os.MkdirAll(app, 0o750))
	victim := filepath.Join(root, "victim.txt") // outside the app dir
	must(t, os.WriteFile(victim, []byte("precious"), 0o600))
	must(t, os.Symlink(victim, filepath.Join(app, "state.json.tmp")))
	must(t, os.Symlink(victim, filepath.Join(app, ".state-planted.tmp")))
	store := &fileStateStore{path: filepath.Join(app, "state.json")}
	must(t, store.save(appState{CurrentVersion: "v1", Nonce: "0a1b2c3d0a1b2c3d"}))
	if b, err := os.ReadFile(victim); err != nil || string(b) != "precious" {
		t.Fatalf("a planted link's target was written through: %q %v", b, err)
	}
	assertOnlyEntries(t, app, "state.json")
	if got, ok, err := store.load(); err != nil || !ok || got.CurrentVersion != "v1" {
		t.Fatalf("load after save: %+v ok=%v err=%v", got, ok, err)
	}
}

// A state.json that is a link is refused, not followed — even to a
// state that would load — and the refusal names the file and why. The
// next save replaces the link itself, never its target.
func TestFileStateStoreLoadRefusesALink(t *testing.T) {
	root := t.TempDir()
	elsewhere := &fileStateStore{path: filepath.Join(root, "other", "state.json")}
	must(t, elsewhere.save(appState{CurrentVersion: "v9", Nonce: "0a1b2c3d0a1b2c3d"}))
	before, err := os.ReadFile(elsewhere.path)
	must(t, err)
	app := filepath.Join(root, "app")
	must(t, os.MkdirAll(app, 0o750))
	store := &fileStateStore{path: filepath.Join(app, "state.json")}
	must(t, os.Symlink(elsewhere.path, store.path))

	_, ok, err := store.load()
	if err == nil || ok {
		t.Fatalf("a linked state.json was followed: ok=%v err=%v", ok, err)
	}
	if msg := err.Error(); !strings.Contains(msg, store.path) || !strings.Contains(msg, "is a link") {
		t.Fatalf("the refusal must name the file and say it is a link: %v", err)
	}

	must(t, store.save(appState{CurrentVersion: "v1", Nonce: "0a1b2c3d0a1b2c3d"}))
	if after, err := os.ReadFile(elsewhere.path); err != nil || string(after) != string(before) {
		t.Fatalf("save wrote through the link: %s %v", after, err)
	}
	if fi, err := os.Lstat(store.path); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("save must replace the link with a regular file: %v %v", fi, err)
	}
}

// A FIFO at state.json must not hold the open — and with it recovery,
// and the deploy lock it holds — for good: the open does not block on
// it, and it is refused as not a regular file.
func TestFileStateStoreLoadDoesNotBlockOnAFIFO(t *testing.T) {
	store := &fileStateStore{path: filepath.Join(t.TempDir(), "state.json")}
	must(t, syscall.Mkfifo(store.path, 0o600))
	done := make(chan error, 1)
	go func() {
		_, _, err := store.load()
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), store.path) || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("a FIFO at state.json must be refused by name: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a planted FIFO blocked the state store")
	}
}

// A state.json larger than any state hotserve writes is refused, and
// not read whole into memory; one at the bound still loads.
func TestFileStateStoreLoadRefusesAnOversizedFile(t *testing.T) {
	store := &fileStateStore{path: filepath.Join(t.TempDir(), "state.json")}
	padded := func(size int) []byte { // a state that would load, padded with an unknown field
		head := `{"current_version":"v1","nonce":"0a1b2c3d0a1b2c3d","pad":"`
		return []byte(head + strings.Repeat("x", size-len(head)-2) + `"}`)
	}
	must(t, os.WriteFile(store.path, padded(stateMaxBytes+1), 0o600))
	_, ok, err := store.load()
	if err == nil || ok || !strings.Contains(err.Error(), store.path) || !strings.Contains(err.Error(), "larger than a state file can be") {
		t.Fatalf("an oversized state.json must be refused by name: ok=%v err=%v", ok, err)
	}
	must(t, os.WriteFile(store.path, padded(stateMaxBytes), 0o600))
	if got, ok, err := store.load(); err != nil || !ok || got.CurrentVersion != "v1" {
		t.Fatalf("a state.json at the bound must load: ok=%v err=%v", ok, err)
	}
}

// A save removes what an interrupted one left: a temp file of its own
// pattern, and the fixed state.json.tmp an earlier hotserve wrote —
// and nothing else that happens to end in .tmp (persistState's
// current.tmp is its own).
func TestFileStateStoreSaveRemovesLeftovers(t *testing.T) {
	app := filepath.Join(t.TempDir(), "app")
	must(t, os.MkdirAll(app, 0o750))
	for _, n := range []string{".state-123456.tmp", "state.json.tmp", "other.tmp"} {
		must(t, os.WriteFile(filepath.Join(app, n), []byte("{"), 0o600))
	}
	must(t, os.Symlink("releases/v1", filepath.Join(app, "current.tmp")))
	must(t, os.Mkdir(filepath.Join(app, ".state-dir.tmp"), 0o750)) // a directory is not a temp file of save's
	store := &fileStateStore{path: filepath.Join(app, "state.json")}
	must(t, store.save(appState{CurrentVersion: "v1", Nonce: "0a1b2c3d0a1b2c3d"}))
	assertOnlyEntries(t, app, "state.json", "other.tmp", "current.tmp", ".state-dir.tmp")
}

// A save that fails after its temp file exists removes the temp file:
// here the rename, over a directory standing at state.json.
func TestFileStateStoreFailedSaveLeavesNoTempFile(t *testing.T) {
	app := filepath.Join(t.TempDir(), "app")
	store := &fileStateStore{path: filepath.Join(app, "state.json")}
	must(t, os.MkdirAll(filepath.Join(store.path, "x"), 0o750))
	if err := store.save(appState{CurrentVersion: "v1"}); err == nil {
		t.Fatal("a save over a directory must fail")
	}
	assertOnlyEntries(t, app, "state.json")
}

// Recovery reads state.json through the store's rules: a link, a FIFO
// or an oversized file there is a permanent recovery error — never a
// silent reset, never a launch — and the journal line names the file
// and the reason.
func TestRecoveryRefusesAPlantedStateFile(t *testing.T) {
	cases := []struct {
		name, reason string
		plant        func(t *testing.T, path string)
	}{
		{"link", "is a link", func(t *testing.T, path string) {
			elsewhere := &fileStateStore{path: filepath.Join(shortTempDir(t), "state.json")}
			must(t, elsewhere.save(appState{CurrentVersion: "v7", Nonce: recordedNonce}))
			must(t, os.Symlink(elsewhere.path, path))
		}},
		{"fifo", "not a regular file", func(t *testing.T, path string) {
			must(t, syscall.Mkfifo(path, 0o600))
		}},
		{"oversized", "larger than a state file can be", func(t *testing.T, path string) {
			must(t, os.WriteFile(path, []byte(`{"current_version":"v7","pad":"`+strings.Repeat("x", stateMaxBytes)+`"}`), 0o600))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTestRig(t)
			rig.ma.store = &fileStateStore{path: rig.spec.dirs.state}
			must(t, os.MkdirAll(rig.spec.dirs.release("v7"), 0o755))
			tc.plant(t, rig.spec.dirs.state)
			core, logs := observer.New(zap.ErrorLevel)
			done := make(chan struct{})
			go func() { rig.ma.recover(context.Background(), zap.New(core)); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("recovery must give up on a planted state.json, not block or retry")
			}
			entries := logs.FilterMessage("recovery failed").All()
			if len(entries) != 1 {
				t.Fatalf("want one recovery failed line, got %v", logs.All())
			}
			msg, _ := entries[0].ContextMap()["error"].(string)
			if !strings.Contains(msg, rig.spec.dirs.state) || !strings.Contains(msg, tc.reason) {
				t.Fatalf("the journal must name the file and %q: %q", tc.reason, msg)
			}
			if rig.runner.startCount() != 0 || len(rig.runner.reattachSeen) != 0 {
				t.Fatalf("a refused state.json reached the runner: starts=%d reattach=%+v", rig.runner.startCount(), rig.runner.reattachSeen)
			}
		})
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
