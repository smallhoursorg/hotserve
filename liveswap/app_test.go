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
// After registers a waiter that Advance/Sleep fire once the fake time
// passes its deadline, which is how watchdog tests drive the loop.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeWaiter
}

type fakeWaiter struct {
	at time.Time
	ch chan time.Time
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
	c.advanceLocked(d)
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.advanceLocked(d)
}

func (c *fakeClock) advanceLocked(d time.Duration) {
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
		} else {
			kept = append(kept, w)
		}
	}
	c.waiters = kept
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, fakeWaiter{at: c.now.Add(d), ch: ch})
	return ch
}

// fakeHandle is a runner handle whose liveness tests control. done
// mirrors execHandle's reaper channel: closed once the process "dies".
// Handles built as bare literals (no done channel) exercise the
// Wait-returns-nil polling fallback.
type fakeHandle struct {
	id    string
	alive bool
	done  chan struct{}
	mu    sync.Mutex
}

func (h *fakeHandle) state() handleState { return handleState{PID: 4242} }

func (h *fakeHandle) isAlive() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.alive
}

// dieQuietly makes the handle read as dead without closing done —
// the shape of a systemd unit whose exit the runner's state poll has
// not yet observed while health probes are already failing.
func (h *fakeHandle) dieQuietly() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.alive = false
}

func (h *fakeHandle) kill() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.alive = false
	if h.done != nil {
		select {
		case <-h.done:
		default:
			close(h.done)
		}
	}
}

// fakeRunner records starts and stops; RunOnce failure is scriptable.
type fakeRunner struct {
	mu              sync.Mutex
	started         []startSpec
	handles         []*fakeHandle
	stopped         []handle
	runOnceErr      error
	runOnceCount    int
	startErr        error
	reattachOK      bool
	reattachErrs    []error // consumed one per Reattach call before reattachOK applies
	reattachCalls   int
	stopErr         error   // Stop returns this
	stopLeavesAlive bool    // Stop does not actually kill the handle
	sweepErr        error   // Sweep returns this
	sweepErrs       []error // consumed one per Sweep call before sweepErr applies
	sweeps          []handle
}

func (r *fakeRunner) Start(spec startSpec) (handle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.startErr != nil {
		return nil, r.startErr
	}
	h := &fakeHandle{id: fmt.Sprintf("h%d", len(r.handles)), alive: true, done: make(chan struct{})}
	r.started = append(r.started, spec)
	r.handles = append(r.handles, h)
	return h, nil
}

func (r *fakeRunner) RunOnce(_ context.Context, spec startSpec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started = append(r.started, spec)
	r.runOnceCount++
	return r.runOnceErr
}

func (r *fakeRunner) Alive(h handle) bool {
	fh, ok := h.(*fakeHandle)
	return ok && fh.isAlive()
}

func (r *fakeRunner) Stop(h handle, _ time.Duration) error {
	r.mu.Lock()
	r.stopped = append(r.stopped, h)
	leave, serr := r.stopLeavesAlive, r.stopErr
	r.mu.Unlock()
	if fh, ok := h.(*fakeHandle); ok && !leave {
		fh.kill()
	}
	return serr
}

func (r *fakeRunner) Wait(h handle) <-chan struct{} {
	fh, ok := h.(*fakeHandle)
	if !ok || fh.done == nil {
		return nil
	}
	return fh.done
}

func (r *fakeRunner) Reattach(_ handleState) (handle, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reattachCalls++
	if len(r.reattachErrs) > 0 {
		err := r.reattachErrs[0]
		r.reattachErrs = r.reattachErrs[1:]
		return nil, false, err
	}
	if !r.reattachOK {
		return nil, false, nil
	}
	h := &fakeHandle{id: "reattached", alive: true}
	r.handles = append(r.handles, h)
	return h, true, nil
}

func (r *fakeRunner) Sweep(_ string, keep handle) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweeps = append(r.sweeps, keep)
	if len(r.sweepErrs) > 0 {
		err := r.sweepErrs[0]
		r.sweepErrs = r.sweepErrs[1:]
		return err
	}
	return r.sweepErr
}

func (r *fakeRunner) lastSweep() handle {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sweeps[len(r.sweeps)-1]
}

func (r *fakeRunner) sweepCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sweeps)
}

func (r *fakeRunner) stopCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.stopped)
}

func (r *fakeRunner) setStartErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.startErr = err
}

func (r *fakeRunner) startCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.started)
}

func (r *fakeRunner) handleAt(i int) *fakeHandle {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.handles[i]
}

// fakeProber approves or rejects the health gate by script. probeOnce
// (the watchdog path) consumes the result queue first, then repeats
// probeErr; both are settable while the watchdog goroutine runs.
type fakeProber struct {
	err error

	mu           sync.Mutex
	probeErr     error
	probeResults []error
	probeCalls   int
}

func (p *fakeProber) waitHealthy(_ context.Context, _ string, alive func() bool, _ healthConfig) error {
	if !alive() {
		return errors.New("process exited before becoming healthy")
	}
	return p.err
}

func (p *fakeProber) probeOnce(_ context.Context, _ string, _ time.Duration) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.probeCalls++
	if len(p.probeResults) > 0 {
		err := p.probeResults[0]
		p.probeResults = p.probeResults[1:]
		return err
	}
	return p.probeErr
}

func (p *fakeProber) setProbeErr(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.probeErr = err
}

func (p *fakeProber) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.probeCalls
}

// fakeFetcher materializes a release dir without any network.
type fakeFetcher struct {
	err     error
	lastReq deployRequest
}

func (f *fakeFetcher) fetch(_ context.Context, spec *appSpec, req deployRequest, progress func(string)) (string, error) {
	f.lastReq = req
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
		trust:           []trustSource{localTrust(appTestPub, "demo")},
		healthPath:      "/health",
		healthInterval:  5 * time.Second,
		healthTimeout:   2 * time.Second,
		soak:            15 * time.Second,
		deadline:        5 * time.Minute,
		drain:           5 * time.Second,
		grace:           10 * time.Second,
		watchdogOn:      true,
		wdFailures:      3,
		wdGrace:         30 * time.Second,
		wdRestarts:      5,
		wdWindow:        10 * time.Minute,
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
	ma.verifiers = resolveVerifiers(rig.spec.trust, nil)
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
	rig.ma.started.Store(true) // as App.Start would
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
	t.Setenv("ACME_DNS_API_TOKEN", "hunter2")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "tok")
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
	for _, k := range []string{"ACME_DNS_API_TOKEN", "AWS_SECRET_ACCESS_KEY"} {
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

func TestRollbackSkipsPreStart(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.preStart = []string{"./migrate"}

	// A normal deploy runs pre_start.
	if err := rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/a.tgz", Version: "v1"}); err != nil {
		t.Fatalf("deploy v1: %v", err)
	}
	if rig.runner.runOnceCount != 1 {
		t.Fatalf("deploy should run pre_start once, got %d", rig.runner.runOnceCount)
	}

	// Deploy a second version so v1 is no longer the running one.
	if err := rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/b.tgz", Version: "v2"}); err != nil {
		t.Fatalf("deploy v2: %v", err)
	}
	if rig.runner.runOnceCount != 2 {
		t.Fatalf("second deploy should run pre_start, got %d", rig.runner.runOnceCount)
	}
	if err := rig.ma.Deploy(context.Background(), deployRequest{Version: "v1", rollback: true}); err != nil {
		t.Fatalf("rollback to v1: %v", err)
	}
	if rig.runner.runOnceCount != 2 {
		t.Fatalf("rollback must not run pre_start; count = %d, want 2", rig.runner.runOnceCount)
	}
}

func TestDeployRejectsExistingVersion(t *testing.T) {
	rig := newTestRig(t)
	if err := rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/a.tgz", Version: "v1"}); err != nil {
		t.Fatalf("deploy v1: %v", err)
	}
	// Deploy a second version so v1 is on disk but not running.
	if err := rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/b.tgz", Version: "v2"}); err != nil {
		t.Fatalf("deploy v2: %v", err)
	}
	// Re-deploying v1 (URL) must be rejected — versions are immutable.
	err := rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/a.tgz", Version: "v1"})
	var vErr validationError
	if !errors.As(err, &vErr) || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("re-deploy of existing version should be a validation error about immutability, got %v", err)
	}
	// But rollback to v1 (which exists) is allowed.
	if err := rig.ma.Deploy(context.Background(), deployRequest{Version: "v1", rollback: true}); err != nil {
		t.Fatalf("rollback to existing v1 should succeed, got %v", err)
	}
}

func TestFailedDeployCleansUpRelease(t *testing.T) {
	rig := newTestRig(t)
	if err := rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/a.tgz", Version: "v1"}); err != nil {
		t.Fatalf("deploy v1: %v", err)
	}
	// v2 fails its health gate.
	rig.prober.err = errTest
	if err := rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/b.tgz", Version: "v2"}); err == nil {
		t.Fatal("deploy v2 should have failed the health gate")
	}
	// Its freshly-extracted release must be gone.
	if _, err := os.Stat(rig.spec.dirs.release("v2")); !os.IsNotExist(err) {
		t.Fatalf("failed deploy should remove its release dir, stat err = %v", err)
	}
	// So the same version is retriable once healthy.
	rig.prober.err = nil
	if err := rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/b.tgz", Version: "v2"}); err != nil {
		t.Fatalf("re-deploy of a cleaned-up failed version should succeed: %v", err)
	}
}

func TestFailedRollbackKeepsRelease(t *testing.T) {
	rig := newTestRig(t)
	if err := rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/a.tgz", Version: "v1"}); err != nil {
		t.Fatalf("deploy v1: %v", err)
	}
	if err := rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/b.tgz", Version: "v2"}); err != nil {
		t.Fatalf("deploy v2: %v", err)
	}
	// A rollback to v1 that fails its health gate must NOT delete v1's
	// pre-existing release.
	rig.prober.err = errTest
	if err := rig.ma.Deploy(context.Background(), deployRequest{Version: "v1", rollback: true}); err == nil {
		t.Fatal("rollback should have failed the health gate")
	}
	if _, err := os.Stat(rig.spec.dirs.release("v1")); err != nil {
		t.Fatalf("failed rollback must not delete the on-disk release: %v", err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("body read boom") }

func TestStageUploadClassifiesErrors(t *testing.T) {
	var se *stagingError

	// A body-read failure is a client fault — bare error, not stagingError.
	if _, err := stageUpload(errReader{}, t.TempDir(), 100); err == nil || errors.As(err, &se) {
		t.Fatalf("body-read error should be a bare (client) error, got %v", err)
	}
	// A local filesystem failure (tmpDir is actually a file → MkdirAll
	// fails) is a server fault — *stagingError.
	notADir := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := stageUpload(strings.NewReader("data"), notADir, 100); !errors.As(err, &se) {
		t.Fatalf("a local FS failure should be a *stagingError (server), got %v", err)
	}
	// Happy path still works.
	if _, err := stageUpload(strings.NewReader("data"), t.TempDir(), 100); err != nil {
		t.Fatalf("valid upload should stage: %v", err)
	}
}

func TestFailedDeployKeepsReleaseWhenStopUnconfirmed(t *testing.T) {
	rig := newTestRig(t)
	if err := rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/a.tgz", Version: "v1"}); err != nil {
		t.Fatalf("deploy v1: %v", err)
	}
	// v2 fails the health gate, and Stop can't confirm the instance
	// exited (it stays alive) — the release must NOT be deleted beneath it.
	rig.prober.err = errTest
	rig.runner.stopErr = errTest
	rig.runner.stopLeavesAlive = true
	err := rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/b.tgz", Version: "v2"})
	if err == nil {
		t.Fatal("deploy v2 should have failed")
	}
	if !strings.Contains(err.Error(), "may still be running") {
		t.Fatalf("error should note the release was left in place: %v", err)
	}
	if _, statErr := os.Stat(rig.spec.dirs.release("v2")); statErr != nil {
		t.Fatalf("release must not be deleted under a still-running failed instance: %v", statErr)
	}
}

func TestFailedDeployKeepsReleaseWhenStopErrorsEvenIfDead(t *testing.T) {
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/a.tgz", Version: "v1"}))
	// v2 fails the health gate and Stop reports an error even though
	// the handle then reads as dead. Under cgroup kill "Stop errored"
	// is the only signal a caller gets, so the release stays on disk.
	rig.prober.err = errTest
	rig.runner.stopErr = errTest
	err := rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/b.tgz", Version: "v2"})
	if err == nil || !strings.Contains(err.Error(), "left on disk") {
		t.Fatalf("expected the release to be kept: %v", err)
	}
	if _, statErr := os.Stat(rig.spec.dirs.release("v2")); statErr != nil {
		t.Fatalf("release must survive an unconfirmed stop: %v", statErr)
	}
}

func TestDeployStopOldErrorDefersToSweep(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	for i, v := range []string{"v1", "v2"} {
		must(t, rig.ma.Deploy(ctx, deployRequest{URL: "https://x/a", Version: v}))
		mt := time.Now().Add(time.Duration(i-10) * time.Minute)
		_ = os.Chtimes(rig.spec.dirs.release(v), mt, mt)
	}
	// Stopping v2 errors, but the sweep — the runner's own ledger —
	// vouches that only v3 remains, so keep=2 GC proceeds: our memory
	// of a failed stop is not the source of truth, the manager is.
	rig.runner.stopErr = errTest
	rig.runner.stopLeavesAlive = true
	must(t, rig.ma.Deploy(ctx, deployRequest{URL: "https://x/a", Version: "v3"}))
	if rig.runner.lastSweep() != rig.runner.handleAt(2) {
		t.Fatal("the pre-GC sweep must keep the just-promoted instance")
	}
	if _, err := os.Stat(rig.spec.dirs.release("v1")); err == nil {
		t.Fatal("with the sweep vouching, GC runs as usual")
	}
	if rig.ma.currentInstance().version != "v3" {
		t.Fatal("the deploy itself succeeds regardless")
	}
}

func TestDestructOnRemovalSweepsWholeApp(t *testing.T) {
	rig := newTestRig(t)
	rig.ma.started.Store(true) // as App.Start would
	must(t, rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/1", Version: "v1"}))
	must(t, rig.ma.Destruct())
	if rig.runner.stopCount() != 1 {
		t.Fatalf("Destruct must stop the tracked instance, got %d stops", rig.runner.stopCount())
	}
	// Deploy swept twice (pre-start, pre-GC); removal sweeps everything (keep=nil).
	if n := rig.runner.sweepCount(); n != 3 || rig.runner.lastSweep() != nil {
		t.Fatalf("removal must sweep the whole app with keep=nil, sweeps=%d last=%v", n, rig.runner.sweeps)
	}
}

func TestDestructLeavesInstanceRunningOnProcessExit(t *testing.T) {
	rig := newTestRig(t)
	rig.ma.started.Store(true) // as App.Start would
	must(t, rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/1", Version: "v1"}))
	orig := caddyExiting
	caddyExiting = func() bool { return true }
	t.Cleanup(func() { caddyExiting = orig })
	must(t, rig.ma.Destruct())
	if rig.runner.stopCount() != 0 {
		t.Fatal("on process exit the unit must be left running for reattach")
	}
	if !rig.runner.Alive(rig.runner.handleAt(0)) {
		t.Fatal("instance must still be alive after Destruct on exit")
	}
}

func TestStartSpecCarriesUnitIdentity(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.preStart = []string{"true"}
	must(t, rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/1", Version: "v1"}))
	if rig.runner.startCount() != 2 {
		t.Fatalf("expected pre_start + start, got %d", rig.runner.startCount())
	}
	for i := range 2 {
		s := rig.runner.started[i]
		if s.app != "demo" || s.version != "v1" || s.grace != rig.spec.grace {
			t.Fatalf("startSpec %d lacks unit identity: %+v", i, s)
		}
	}
}

func TestEnsureRunningUnreadableReattachIsTransientAndLaunchesNothing(t *testing.T) {
	rig := newTestRig(t)
	rig.store.state = appState{CurrentVersion: "v7", Port: 12345, Handle: handleState{Unit: "u.service"}}
	rig.store.ok = true
	must(t, os.MkdirAll(rig.spec.dirs.release("v7"), 0o755))
	rig.runner.reattachErrs = []error{errTest}
	err := rig.ma.ensureRunning()
	if !transientRecovery(err) || !strings.Contains(err.Error(), "not relaunching") {
		t.Fatalf("expected a transient refusal, got %v", err)
	}
	if rig.runner.startCount() != 0 || rig.ma.activePort.Load() != 0 {
		t.Fatal("must not launch or publish while the recorded unit's state is unknown")
	}
}

func TestRecoverRetriesTransientErrorsUntilReattached(t *testing.T) {
	rig := newTestRig(t)
	rig.store.state = appState{CurrentVersion: "v7", Port: 12345, Handle: handleState{Unit: "u.service"}}
	rig.store.ok = true
	must(t, os.MkdirAll(rig.spec.dirs.release("v7"), 0o755))
	rig.runner.reattachErrs = []error{errTest, errTest}
	rig.runner.reattachOK = true
	done := make(chan struct{})
	go func() { rig.ma.recover(zap.NewNop()); close(done) }()
	advanceUntil(t, rig, recoveryBackoffFloor, "reattach after two transient failures", func() bool {
		return rig.ma.activePort.Load() == 12345
	})
	<-done
	if rig.runner.startCount() != 0 || rig.ma.activePort.Load() != 12345 {
		t.Fatalf("expected a reattach on the third try, starts=%d port=%d", rig.runner.startCount(), rig.ma.activePort.Load())
	}
	if rig.runner.reattachCalls != 3 {
		t.Fatalf("expected 3 reattach attempts, got %d", rig.runner.reattachCalls)
	}
	if rig.runner.sweepCount() != 1 || rig.runner.sweeps[0] != rig.runner.handleAt(0) {
		t.Fatal("recovery must sweep stray units, keeping the adopted one")
	}
}

func TestRecoverGivesUpOnPermanentErrors(t *testing.T) {
	rig := newTestRig(t)
	rig.store.state = appState{CurrentVersion: "v7", Port: 12345, Handle: handleState{PID: 1}}
	rig.store.ok = true // release dir deliberately missing: not something a retry fixes
	done := make(chan struct{})
	go func() { rig.ma.recover(zap.NewNop()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("recover must return on a permanent error, not retry forever")
	}
}

func TestDestructBeforeStartTouchesNothing(t *testing.T) {
	// `hotserve validate` and a config load that fails elsewhere both
	// provision and then clean up without Start: whatever the manager
	// runs belongs to the serving process and must be left alone.
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/1", Version: "v1"}))
	before := rig.runner.sweepCount()
	must(t, rig.ma.Destruct())
	if rig.runner.stopCount() != 0 || rig.runner.sweepCount() != before {
		t.Fatalf("Destruct without Start must not stop or sweep: stops=%d sweeps=%d", rig.runner.stopCount(), rig.runner.sweepCount()-before)
	}
	if !rig.runner.Alive(rig.runner.handleAt(0)) {
		t.Fatal("the instance must still be running")
	}
}

func TestParseEnvFileRejectsInvalidKeys(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "app.env")
	must(t, os.WriteFile(envFile, []byte("GOOD=1\nmy-var=2\n"), 0o600))
	_, err := parseEnvFile(envFile)
	if err == nil || !strings.Contains(err.Error(), `"my-var"`) || !strings.Contains(err.Error(), ":2:") {
		t.Fatalf("an invalid key must be named with its line, got %v", err)
	}
}

func TestEnsureRunningRelaunchSweepsStrays(t *testing.T) {
	rig := newTestRig(t)
	rig.store.state = appState{CurrentVersion: "v7", Port: 12345, Handle: handleState{PID: 1}}
	rig.store.ok = true
	must(t, os.MkdirAll(rig.spec.dirs.release("v7"), 0o755))
	must(t, rig.ma.ensureRunning())
	if rig.runner.sweepCount() != 1 || rig.runner.sweeps[0] != nil {
		t.Fatal("relaunch must sweep everything before starting")
	}
	if rig.runner.startCount() != 1 {
		t.Fatal("relaunched once")
	}
}

func TestEnsureRunningDoesNotRelaunchWhenSweepUnconfirmed(t *testing.T) {
	rig := newTestRig(t)
	rig.store.state = appState{CurrentVersion: "v7", Port: 12345, Handle: handleState{PID: 1}}
	rig.store.ok = true
	must(t, os.MkdirAll(rig.spec.dirs.release("v7"), 0o755))
	rig.runner.sweepErr = errTest
	if err := rig.ma.ensureRunning(); err == nil || !strings.Contains(err.Error(), "not launching") {
		t.Fatalf("expected a refusal to launch, got %v", err)
	}
	if rig.runner.startCount() != 0 || rig.ma.activePort.Load() != 0 {
		t.Fatal("nothing may be launched or published beside a possibly-running unit")
	}
}

func TestDeployAbortsStartWhenSweepUnconfirmed(t *testing.T) {
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/1", Version: "v1"}))
	rig.runner.sweepErr = errTest
	err := rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/2", Version: "v2"})
	if err == nil || !strings.Contains(err.Error(), "not starting") {
		t.Fatalf("expected the deploy to abort before Start, got %v", err)
	}
	if rig.runner.startCount() != 1 {
		t.Fatal("no second instance may be started")
	}
	if rig.runner.lastSweep() != rig.runner.handleAt(0) {
		t.Fatal("the pre-start sweep must keep the serving instance")
	}
	if rig.ma.currentInstance().version != "v1" {
		t.Fatal("v1 keeps serving")
	}
}

func TestDestructOnRemovalSweepsEvenWithoutInstance(t *testing.T) {
	rig := newTestRig(t)
	rig.ma.started.Store(true) // as App.Start would
	must(t, rig.ma.Destruct())
	if rig.runner.stopCount() != 0 || rig.runner.sweepCount() != 1 || rig.runner.sweeps[0] != nil {
		t.Fatalf("removal with nothing tracked must still sweep the app: stops=%d sweeps=%v", rig.runner.stopCount(), rig.runner.sweeps)
	}
}

func TestDeploySweepsBeforeGCAndSkipsGCWhenSweepFails(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	for i, v := range []string{"v1", "v2"} {
		must(t, rig.ma.Deploy(ctx, deployRequest{URL: "https://x/a", Version: v}))
		mt := time.Now().Add(time.Duration(i-10) * time.Minute)
		_ = os.Chtimes(rig.spec.dirs.release(v), mt, mt)
	}
	// Each deploy sweeps twice: before Start (keeping the serving
	// instance) and before GC (keeping the promoted one).
	if rig.runner.sweepCount() != 4 || rig.runner.sweeps[0] != nil || rig.runner.sweeps[2] != rig.runner.handleAt(0) {
		t.Fatalf("sweeps %v", rig.runner.sweeps)
	}
	// keep=2: v3 would GC v1 — unless the pre-GC sweep cannot vouch
	// that nothing else is running.
	rig.runner.sweepErrs = []error{nil, errTest}
	must(t, rig.ma.Deploy(ctx, deployRequest{URL: "https://x/a", Version: "v3"}))
	for _, v := range []string{"v1", "v2", "v3"} {
		if _, err := os.Stat(rig.spec.dirs.release(v)); err != nil {
			t.Fatalf("release %s must survive a deploy whose sweep failed: %v", v, err)
		}
	}
	if rig.runner.lastSweep() != rig.runner.handleAt(2) {
		t.Fatal("the pre-GC sweep must keep the just-promoted instance")
	}
	must(t, rig.ma.Deploy(ctx, deployRequest{URL: "https://x/a", Version: "v4"}))
	if _, err := os.Stat(rig.spec.dirs.release("v1")); err == nil {
		t.Fatal("once the sweep vouches, GC catches up")
	}
}

func TestFailedDeployKeepsReleaseWhenStartUnconfirmed(t *testing.T) {
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/a.tgz", Version: "v1"}))
	rig.runner.setStartErr(&unitUnconfirmedError{unit: "hotserve-demo.v2.deadbeef.service", err: errTest})
	err := rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/b.tgz", Version: "v2"})
	if err == nil || !strings.Contains(err.Error(), "left on disk") {
		t.Fatalf("an unreconciled start must keep the release: %v", err)
	}
	if _, statErr := os.Stat(rig.spec.dirs.release("v2")); statErr != nil {
		t.Fatalf("release must survive an unconfirmed start: %v", statErr)
	}
	rig.runner.setStartErr(nil)
	rig.runner.runOnceErr = &unitUnconfirmedError{unit: "hotserve-demo.v3.deadbeef.prestart.service", err: errTest}
	rig.spec.preStart = []string{"migrate"}
	err = rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/c.tgz", Version: "v3"})
	if err == nil || !strings.Contains(err.Error(), "left on disk") {
		t.Fatalf("an unreconciled pre_start must keep the release: %v", err)
	}
}
