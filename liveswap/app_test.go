package liveswap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
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
	stopErr         error // Stop returns this
	stopLeavesAlive bool  // Stop does not actually kill the handle
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

// A crashed old leader still gets a Stop on promote: with the exec
// runner, Stop blocks until the reaper's sweep of the old process group
// is done, so the release GC that follows never deletes a release dir
// from under workers that are still draining. Only the drain sleep is
// skipped — there is nothing left serving to drain.
func TestDeployStopsDeadOldLeaderBeforeGC(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.watchdogOn = false // keep the watchdog's own restart out of the count
	ctx := context.Background()
	must(t, rig.ma.Deploy(ctx, deployRequest{URL: "https://x/1", Version: "v1"}))
	oldHandle := rig.runner.handles[0]
	oldHandle.kill() // leader crashed; its group may still be draining
	before := rig.clock.Now()

	must(t, rig.ma.Deploy(ctx, deployRequest{URL: "https://x/2", Version: "v2"}))
	if rig.runner.stopCount() != 1 || rig.runner.stopped[0] != oldHandle {
		t.Fatalf("dead old leader must still be stopped exactly once before release GC: %d stops", rig.runner.stopCount())
	}
	if got := rig.ma.status().CurrentVersion; got != "v2" {
		t.Fatalf("current version = %s, want v2", got)
	}
	if rig.clock.Now().Sub(before) >= rig.spec.drain {
		t.Fatal("drain period must be skipped for a dead leader")
	}
}

// A release whose old-instance stop could not be confirmed stays
// protected from GC across LATER deploys, not just the one that saw
// the failure: with keep=2, v1's failed stop during the v2 promote must
// still shield v1 when the v3 promote (whose own stop-old succeeds)
// runs GC — the point at which v1 would otherwise fall out of keep.
func TestLeakedReleaseSurvivesLaterGC(t *testing.T) {
	ctx := context.Background()
	backdate := func(t *testing.T, rig *testRig, v string, i int) {
		t.Helper()
		mt := time.Now().Add(time.Duration(i-10) * time.Minute)
		must(t, os.Chtimes(rig.spec.dirs.release(v), mt, mt))
	}
	run := func(t *testing.T, leakV1 bool) *testRig {
		rig := newTestRig(t)
		must(t, rig.ma.Deploy(ctx, deployRequest{URL: "https://x/v1", Version: "v1"}))
		backdate(t, rig, "v1", 0)
		if leakV1 {
			rig.runner.stopErr = errTest // v1's stop during the v2 promote fails
		}
		must(t, rig.ma.Deploy(ctx, deployRequest{URL: "https://x/v2", Version: "v2"}))
		backdate(t, rig, "v2", 1)
		rig.runner.stopErr = nil // v2's stop during the v3 promote is fine
		must(t, rig.ma.Deploy(ctx, deployRequest{URL: "https://x/v3", Version: "v3"}))
		return rig
	}
	// Control: v1 is GC'd on the third deploy when nothing leaked.
	ctl := run(t, false)
	if _, err := os.Stat(ctl.spec.dirs.release("v1")); !os.IsNotExist(err) {
		t.Fatalf("test premise: v1 should have been GC'd on the third deploy (stat err=%v)", err)
	}
	if got := ctl.ma.status().LeakedReleases; len(got) != 0 {
		t.Fatalf("nothing leaked, status reports %v", got)
	}

	rig := run(t, true)
	if _, err := os.Stat(rig.spec.dirs.release("v1")); err != nil {
		t.Fatalf("a leaked release must survive a later deploy's GC: %v", err)
	}
	if got := rig.ma.status(); got.CurrentVersion != "v3" || !reflect.DeepEqual(got.LeakedReleases, []string{"v1"}) {
		t.Fatalf("deploys must still succeed and status must name the leak: %+v", got)
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
	gcReleases(rig.spec.dirs.releases, rig.spec.keep, zap.NewNop(), "v3")
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

// Same guarantee when the leader is dead but Stop still reports an
// error: with the exec runner that means workers survived the sweep of
// its process group, and they are still running out of the release.
func TestFailedDeployKeepsReleaseWhenGroupStopUnconfirmed(t *testing.T) {
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/a.tgz", Version: "v1"}))
	rig.prober.err = errTest
	rig.runner.stopErr = errTest // leader is killed by the fake, but the sweep verdict is an error
	err := rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/b.tgz", Version: "v2"})
	if err == nil {
		t.Fatal("deploy v2 should have failed")
	}
	if rig.runner.Alive(rig.runner.handles[1]) {
		t.Fatal("test premise: the failed leader should be dead")
	}
	if !strings.Contains(err.Error(), "could not be stopped") || !strings.Contains(err.Error(), "left on disk") {
		t.Fatalf("error should carry the stop failure and note the release was kept: %v", err)
	}
	if _, statErr := os.Stat(rig.spec.dirs.release("v2")); statErr != nil {
		t.Fatalf("release must not be deleted while the failed instance's group may still be running: %v", statErr)
	}
	// ...and not by any later deploy's GC either: with keep=2, two more
	// successful deploys push v2 (and v1) out of the keep window.
	rig.prober.err, rig.runner.stopErr = nil, nil
	for i, v := range []string{"v1", "v2"} {
		mt := time.Now().Add(time.Duration(i-10) * time.Minute)
		must(t, os.Chtimes(rig.spec.dirs.release(v), mt, mt))
	}
	must(t, rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/c.tgz", Version: "v3"}))
	mt := time.Now().Add(-8 * time.Minute)
	must(t, os.Chtimes(rig.spec.dirs.release("v3"), mt, mt))
	must(t, rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/d.tgz", Version: "v4"}))
	if _, statErr := os.Stat(rig.spec.dirs.release("v1")); !os.IsNotExist(statErr) {
		t.Fatalf("test premise: v1 (cleanly stopped) should have been GC'd (stat err=%v)", statErr)
	}
	if _, statErr := os.Stat(rig.spec.dirs.release("v2")); statErr != nil {
		t.Fatalf("leaked release v2 must survive later GC runs: %v", statErr)
	}
	if got := rig.ma.status().LeakedReleases; !reflect.DeepEqual(got, []string{"v2"}) {
		t.Fatalf("status should name the leaked release, got %v", got)
	}
	if st, _, _ := rig.store.load(); !reflect.DeepEqual(st.LeakedReleases, []string{"v2"}) {
		t.Fatalf("leaked set must be persisted, state has %v", st.LeakedReleases)
	}
}

// The stray processes behind a leaked release outlive a Caddy restart
// (Pdeathsig reaches only the direct leader, and only on Linux), so the
// protection must come back from state.json before any GC can run.
func TestLeakedReleasesSurviveRestart(t *testing.T) {
	ctx := context.Background()
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(ctx, deployRequest{URL: "https://x/v1", Version: "v1"}))
	rig.prober.err, rig.runner.stopErr = errTest, errTest
	if err := rig.ma.Deploy(ctx, deployRequest{URL: "https://x/v2", Version: "v2"}); err == nil {
		t.Fatal("v2 should have failed")
	}
	for i, v := range []string{"v1", "v2"} {
		mt := time.Now().Add(time.Duration(i-10) * time.Minute)
		must(t, os.Chtimes(rig.spec.dirs.release(v), mt, mt))
	}

	// "Restart": a fresh managedApp over the same state file and dirs.
	fresh := newTestRig(t)
	fresh.spec, fresh.store = rig.spec, rig.store
	fresh.ma.spec, fresh.ma.store = rig.spec, rig.store
	must(t, fresh.ma.ensureRunning())
	if got := fresh.ma.status().LeakedReleases; !reflect.DeepEqual(got, []string{"v2"}) {
		t.Fatalf("leaked set not restored after restart: %v", got)
	}
	must(t, fresh.ma.Deploy(ctx, deployRequest{URL: "https://x/v3", Version: "v3"}))
	mt := time.Now().Add(-8 * time.Minute)
	must(t, os.Chtimes(fresh.spec.dirs.release("v3"), mt, mt))
	must(t, fresh.ma.Deploy(ctx, deployRequest{URL: "https://x/v4", Version: "v4"}))
	if _, err := os.Stat(fresh.spec.dirs.release("v1")); !os.IsNotExist(err) {
		t.Fatalf("test premise: v1 should have been GC'd (stat err=%v)", err)
	}
	if _, err := os.Stat(fresh.spec.dirs.release("v2")); err != nil {
		t.Fatalf("leaked release must survive GC after a restart: %v", err)
	}

	// The defined way to clear it: deal with the strays and remove the
	// release dir; the next GC pass drops the entry, in memory and on disk.
	must(t, os.RemoveAll(fresh.spec.dirs.release("v2")))
	must(t, fresh.ma.Deploy(ctx, deployRequest{URL: "https://x/v5", Version: "v5"}))
	if got := fresh.ma.status().LeakedReleases; len(got) != 0 {
		t.Fatalf("removed release should drop out of the leaked set, got %v", got)
	}
	if st, _, _ := fresh.store.load(); len(st.LeakedReleases) != 0 {
		t.Fatalf("cleared entry must be persisted as cleared, state has %v", st.LeakedReleases)
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
	if !strings.Contains(err.Error(), "left on disk") {
		t.Fatalf("error should note the release was left in place: %v", err)
	}
	if _, statErr := os.Stat(rig.spec.dirs.release("v2")); statErr != nil {
		t.Fatalf("release must not be deleted under a still-running failed instance: %v", statErr)
	}
}
