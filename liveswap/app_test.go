package liveswap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

// fakeClock advances instantly on Sleep so pipeline tests never wait.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1_700_000_000, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeHandle is a runner handle whose liveness tests control.
type fakeHandle struct {
	id    string
	alive bool
	mu    sync.Mutex
}

func (h *fakeHandle) state() handleState { return handleState{PID: 4242} }

func (h *fakeHandle) isAlive() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.alive
}

func (h *fakeHandle) setAlive(v bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.alive = v
}

// fakeRunner records starts and stops; RunOnce failure is scriptable.
type fakeRunner struct {
	mu         sync.Mutex
	started    []startSpec
	handles    []*fakeHandle
	stopped    []handle
	runOnceErr error
	startErr   error
	reattachOK bool
}

func (r *fakeRunner) Start(spec startSpec) (handle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.startErr != nil {
		return nil, r.startErr
	}
	h := &fakeHandle{id: fmt.Sprintf("h%d", len(r.handles)), alive: true}
	r.started = append(r.started, spec)
	r.handles = append(r.handles, h)
	return h, nil
}

func (r *fakeRunner) RunOnce(_ context.Context, spec startSpec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started = append(r.started, spec)
	return r.runOnceErr
}

func (r *fakeRunner) Alive(h handle) bool {
	fh, ok := h.(*fakeHandle)
	return ok && fh.isAlive()
}

func (r *fakeRunner) Stop(h handle, _ time.Duration) error {
	r.mu.Lock()
	r.stopped = append(r.stopped, h)
	r.mu.Unlock()
	if fh, ok := h.(*fakeHandle); ok {
		fh.setAlive(false)
	}
	return nil
}

func (r *fakeRunner) Reattach(_ handleState) (handle, bool) {
	if !r.reattachOK {
		return nil, false
	}
	h := &fakeHandle{id: "reattached", alive: true}
	r.mu.Lock()
	r.handles = append(r.handles, h)
	r.mu.Unlock()
	return h, true
}

func (r *fakeRunner) stopCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.stopped)
}

// fakeProber approves or rejects the health gate by script.
type fakeProber struct {
	err error
}

func (p *fakeProber) waitHealthy(_ context.Context, _ string, alive func() bool, _ healthConfig) error {
	if !alive() {
		return errors.New("process exited before becoming healthy")
	}
	return p.err
}

// fakeFetcher materializes a release dir without any network.
type fakeFetcher struct {
	err error
}

func (f *fakeFetcher) fetch(_ context.Context, spec *appSpec, req deployRequest, progress func(string)) (string, error) {
	progress("downloading")
	progress("extracting")
	if f.err != nil {
		return "", f.err
	}
	dir := spec.dirs.release(req.Version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// fakeStore is an in-memory stateStore.
type fakeStore struct {
	mu    sync.Mutex
	state appState
	ok    bool
	err   error
}

func (s *fakeStore) load() (appState, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, s.ok, s.err
}

func (s *fakeStore) save(st appState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = st
	s.ok = true
	return nil
}

// testSpec builds a fully-populated spec rooted in a temp dir.
func testSpec(t *testing.T) *appSpec {
	t.Helper()
	root := t.TempDir()
	return &appSpec{
		name:            "demo",
		command:         []string{"./server", "--version", "{version}"},
		env:             map[string]string{"DATA": "{shared_dir}/db"},
		secret:          "s3cret",
		healthPath:      "/health",
		healthInterval:  5 * time.Second,
		healthTimeout:   2 * time.Second,
		soak:            15 * time.Second,
		deadline:        5 * time.Minute,
		drain:           5 * time.Second,
		grace:           10 * time.Second,
		keep:            2,
		maxArtifactSize: 1 << 20,
		dirs:            newAppDirs(root, "demo"),
	}
}

type testRig struct {
	ma     *managedApp
	runner *fakeRunner
	prober *fakeProber
	fetch  *fakeFetcher
	clock  *fakeClock
	store  *fakeStore
	spec   *appSpec
}

func newTestRig(t *testing.T) *testRig {
	t.Helper()
	rig := &testRig{
		runner: &fakeRunner{},
		prober: &fakeProber{},
		fetch:  &fakeFetcher{},
		clock:  newFakeClock(),
		store:  &fakeStore{},
		spec:   testSpec(t),
	}
	ma := newManagedApp("demo")
	ma.spec = rig.spec
	ma.runner = rig.runner
	ma.prober = rig.prober
	ma.fetch = rig.fetch
	ma.clock = rig.clock
	ma.store = rig.store
	ma.logger = zap.NewNop()
	rig.ma = ma
	return rig
}

func TestDeployFirstVersion(t *testing.T) {
	rig := newTestRig(t)
	if err := rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/a.tgz", Version: "v1"}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if got := rig.ma.activePort.Load(); got == 0 {
		t.Fatal("activePort not published after deploy")
	}
	st, ok, _ := rig.store.load()
	if !ok || st.CurrentVersion != "v1" {
		t.Fatalf("state not persisted: %+v ok=%v", st, ok)
	}
	if rig.runner.stopCount() != 0 {
		t.Fatal("nothing should be stopped on a first deploy")
	}
	status := rig.ma.status()
	if status.CurrentVersion != "v1" || !status.Running || status.Phase != "idle" {
		t.Fatalf("unexpected status: %+v", status)
	}
	if status.LastDeploy == nil || status.LastDeploy.Status != "succeeded" {
		t.Fatalf("last deploy not recorded: %+v", status.LastDeploy)
	}
}

func TestDeploySecondVersionStopsOldAfterDrain(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	must(t, rig.ma.Deploy(ctx, deployRequest{URL: "https://x/1", Version: "v1"}))
	portV1 := rig.ma.activePort.Load()
	before := rig.clock.Now()

	must(t, rig.ma.Deploy(ctx, deployRequest{URL: "https://x/2", Version: "v2"}))
	if rig.ma.activePort.Load() == portV1 {
		t.Fatal("cutover did not change the active port")
	}
	if rig.runner.stopCount() != 1 {
		t.Fatalf("old instance not stopped exactly once: %d", rig.runner.stopCount())
	}
	if got := rig.ma.status().CurrentVersion; got != "v2" {
		t.Fatalf("current version = %s, want v2", got)
	}
	// Drain must have elapsed on the clock before the old stop.
	if rig.clock.Now().Sub(before) < rig.spec.drain {
		t.Fatal("drain period was not observed")
	}
}

func TestDeployPreStartFailureKeepsOldServing(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	must(t, rig.ma.Deploy(ctx, deployRequest{URL: "https://x/1", Version: "v1"}))
	portV1 := rig.ma.activePort.Load()

	rig.spec.preStart = []string{"./migrate"}
	rig.runner.runOnceErr = errors.New("migration exploded")
	err := rig.ma.Deploy(ctx, deployRequest{URL: "https://x/2", Version: "v2"})
	if err == nil || rig.ma.activePort.Load() != portV1 {
		t.Fatalf("pre_start failure must abort and keep old port; err=%v", err)
	}
	if got := rig.ma.status(); got.CurrentVersion != "v1" || !got.Running {
		t.Fatalf("old version must keep serving: %+v", got)
	}
	if got := rig.ma.status().LastDeploy; got.Status != "failed" || got.Phase != "preparing" {
		t.Fatalf("failure not recorded with phase: %+v", got)
	}
}

func TestDeployHealthFailureStopsNewKeepsOld(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	must(t, rig.ma.Deploy(ctx, deployRequest{URL: "https://x/1", Version: "v1"}))
	portV1 := rig.ma.activePort.Load()

	rig.prober.err = errors.New("never became healthy")
	err := rig.ma.Deploy(ctx, deployRequest{URL: "https://x/2", Version: "v2"})
	if err == nil {
		t.Fatal("expected health-gate failure")
	}
	if rig.ma.activePort.Load() != portV1 {
		t.Fatal("failed deploy must not move traffic")
	}
	// v1 still alive, v2 stopped: exactly one stop, and current still v1.
	if rig.runner.stopCount() != 1 {
		t.Fatalf("new instance should be stopped once, got %d stops", rig.runner.stopCount())
	}
	if got := rig.ma.status(); got.CurrentVersion != "v1" || !got.Running {
		t.Fatalf("old version must keep serving: %+v", got)
	}
}

func TestDeployRejectsSameRunningVersion(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	must(t, rig.ma.Deploy(ctx, deployRequest{URL: "https://x/1", Version: "v1"}))
	err := rig.ma.Deploy(ctx, deployRequest{URL: "https://x/1", Version: "v1"})
	var vErr validationError
	if !errors.As(err, &vErr) {
		t.Fatalf("expected validationError, got %v", err)
	}
}

func TestDeployConcurrentGets409Error(t *testing.T) {
	rig := newTestRig(t)
	rig.ma.deployMu.Lock()
	defer rig.ma.deployMu.Unlock()
	err := rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/1", Version: "v1"})
	if !errors.Is(err, errDeployInProgress) {
		t.Fatalf("expected errDeployInProgress, got %v", err)
	}
}

func TestDeployGCKeepsNewestReleases(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	// keep=2 in testSpec; deploy three versions, backdating each
	// BEFORE the next deploy so the GC that runs inside Deploy sees
	// deterministic mtime ordering (v1 oldest, v3 newest).
	for i, v := range []string{"v1", "v2", "v3"} {
		must(t, rig.ma.Deploy(ctx, deployRequest{URL: "https://x/a", Version: v}))
		mt := time.Now().Add(time.Duration(i-10) * time.Minute)
		_ = os.Chtimes(rig.spec.dirs.release(v), mt, mt)
	}
	// The final GC ran before v3 was backdated; run it once more the
	// way the next deploy would see the world.
	gcReleases(rig.spec.dirs.releases, rig.spec.keep, "v3", zap.NewNop())
	entries, err := os.ReadDir(rig.spec.dirs.releases)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected 2 releases kept, got %v", names)
	}
	if _, err := os.Stat(rig.spec.dirs.release("v3")); err != nil {
		t.Fatal("current release must survive GC")
	}
}

func TestEnsureRunningRelaunchesFromState(t *testing.T) {
	rig := newTestRig(t)
	rig.store.state = appState{CurrentVersion: "v7", Port: 12345, Handle: handleState{PID: 1}}
	rig.store.ok = true
	if err := os.MkdirAll(rig.spec.dirs.release("v7"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := rig.ma.ensureRunning(); err != nil {
		t.Fatalf("ensureRunning: %v", err)
	}
	if rig.ma.activePort.Load() == 0 {
		t.Fatal("recovered instance not published")
	}
	if got := rig.ma.status().CurrentVersion; got != "v7" {
		t.Fatalf("recovered version = %s, want v7", got)
	}
	if len(rig.runner.started) != 1 {
		t.Fatalf("expected exactly one Start, got %d", len(rig.runner.started))
	}
}

func TestEnsureRunningReattachesWhenRunnerCan(t *testing.T) {
	rig := newTestRig(t)
	rig.runner.reattachOK = true
	rig.store.state = appState{CurrentVersion: "v7", Port: 12345, Handle: handleState{PID: 1}}
	rig.store.ok = true
	if err := os.MkdirAll(rig.spec.dirs.release("v7"), 0o755); err != nil {
		t.Fatal(err)
	}
	must(t, rig.ma.ensureRunning())
	if got := rig.ma.activePort.Load(); got != 12345 {
		t.Fatalf("reattach must keep the recorded port, got %d", got)
	}
	if len(rig.runner.started) != 0 {
		t.Fatal("reattach must not start a new process")
	}
}

func TestEnsureRunningNoStateIsNoop(t *testing.T) {
	rig := newTestRig(t)
	must(t, rig.ma.ensureRunning())
	if rig.ma.activePort.Load() != 0 || len(rig.runner.started) != 0 {
		t.Fatal("nothing should happen without persisted state")
	}
}

func TestEnsureRunningSkipsWhenAlreadyAlive(t *testing.T) {
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/1", Version: "v1"}))
	started := len(rig.runner.started)
	must(t, rig.ma.ensureRunning())
	if len(rig.runner.started) != started {
		t.Fatal("ensureRunning must be a no-op while the instance is alive (reload case)")
	}
}

func TestDestructStopsCurrentInstance(t *testing.T) {
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/1", Version: "v1"}))
	must(t, rig.ma.Destruct())
	if rig.runner.stopCount() != 1 {
		t.Fatalf("Destruct must stop the running instance, got %d stops", rig.runner.stopCount())
	}
}

func TestBuildEnvPrecedenceAndPlaceholders(t *testing.T) {
	spec := testSpec(t)
	envFile := filepath.Join(t.TempDir(), "app.env")
	must(t, os.WriteFile(envFile, []byte("# comment\nexport FROM_FILE=yes\nOVERRIDE=\"file\"\n\n"), 0o600))
	spec.envFile = envFile
	spec.env = map[string]string{"OVERRIDE": "inline", "DB": "sqlite:{shared_dir}/app.db", "V": "{version}:{port}"}

	env, err := buildEnv(spec, "v9", 8123, spec.dirs.release("v9"))
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]string{}
	for _, kv := range env {
		k, v := stringsCut(kv)
		byKey[k] = v // later entries win, matching exec env semantics
	}
	for k, want := range map[string]string{
		"FROM_FILE": "yes",
		"OVERRIDE":  "inline",
		"DB":        "sqlite:" + spec.dirs.shared + "/app.db",
		"V":         "v9:8123",
		"PORT":      "8123",
		"HOST":      "127.0.0.1",
	} {
		if byKey[k] != want {
			t.Errorf("%s = %q, want %q", k, byKey[k], want)
		}
	}
}

func TestBuildEnvDoesNotLeakSupervisorSecrets(t *testing.T) {
	t.Setenv("LIVESWAP_SECRET", "hunter2")
	t.Setenv("SOME_ACME_TOKEN", "tok")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("LC_ALL", "C.UTF-8")

	env, err := buildEnv(testSpec(t), "v1", 8123, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]string{}
	for _, kv := range env {
		k, v := stringsCut(kv)
		byKey[k] = v
	}
	for _, k := range []string{"LIVESWAP_SECRET", "SOME_ACME_TOKEN"} {
		if _, leaked := byKey[k]; leaked {
			t.Errorf("%s must not leak from the supervisor env into apps", k)
		}
	}
	if byKey["PATH"] != "/usr/bin:/bin" {
		t.Errorf("PATH = %q, want inherited /usr/bin:/bin", byKey["PATH"])
	}
	if byKey["LC_ALL"] != "C.UTF-8" {
		t.Errorf("LC_ALL = %q, want inherited C.UTF-8", byKey["LC_ALL"])
	}
}

func stringsCut(kv string) (string, string) {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			return kv[:i], kv[i+1:]
		}
	}
	return kv, ""
}

func TestParseEnvFileRejectsGarbage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.env")
	must(t, os.WriteFile(p, []byte("NOT A VAR LINE\n"), 0o600))
	if _, err := parseEnvFile(p); err == nil {
		t.Fatal("expected error for malformed env file")
	}
}

func TestHostOfStripsUserinfoCredentials(t *testing.T) {
	cases := map[string]string{
		"https://example.com/artifact.tar.gz":             "example.com",
		"https://example.com:8443/a?token=x":              "example.com:8443",
		"https://user:s3cr3t@example.com/artifact.tar.gz": "example.com",
		"https://ci-token@host:443/x":                     "host:443",
		"not a url":                                       "",
	}
	for in, want := range cases {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
		}
		if got := hostOf(in); strings.Contains(got, "@") || strings.Contains(got, "s3cr3t") || strings.Contains(got, "ci-token") {
			t.Errorf("hostOf(%q) = %q leaks credentials", in, got)
		}
	}
}

func TestValidVersionRejectsDotSegments(t *testing.T) {
	for _, v := range []string{"v1", "1.2.3", "release_2024-01-01", "..foo", "..."} {
		if !validVersion(v) {
			t.Errorf("validVersion(%q) = false, want true", v)
		}
	}
	for _, v := range []string{".", "..", "", "has/slash", "with space", "..\x00"} {
		if validVersion(v) {
			t.Errorf("validVersion(%q) = true, want false (path-unsafe)", v)
		}
	}
}

func TestExpandArgs(t *testing.T) {
	spec := testSpec(t)
	got := expandArgs([]string{"run", "--rel={release_dir}", "{version}"}, spec, "v2", 9000, "/rel/v2")
	if got[1] != "--rel=/rel/v2" || got[2] != "v2" {
		t.Fatalf("placeholders not expanded: %v", got)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
