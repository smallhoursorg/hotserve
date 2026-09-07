package liveswap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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
	id     string
	alive  bool
	done   chan struct{}
	socket string // removed when the "unit" stops, as ExecStopPost= does
	mu     sync.Mutex
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
	if h.socket != "" {
		_ = os.Remove(h.socket)
	}
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
	reattachSeen    []handleState
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
	h := &fakeHandle{id: fmt.Sprintf("h%d", len(r.handles)), alive: true, done: make(chan struct{}), socket: spec.socket}
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

func (r *fakeRunner) Reattach(st handleState) (handle, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reattachCalls++
	r.reattachSeen = append(r.reattachSeen, st)
	if len(r.reattachErrs) > 0 {
		err := r.reattachErrs[0]
		r.reattachErrs = r.reattachErrs[1:]
		return nil, false, err
	}
	// Nothing recorded is nothing to reattach to, as with the real
	// runner; a record the caller refused arrives here as zero.
	if !r.reattachOK || st == (handleState{}) {
		return nil, false, nil
	}
	h := &fakeHandle{id: "reattached", alive: true}
	r.handles = append(r.handles, h)
	return h, true, nil
}

func (r *fakeRunner) reattachCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reattachCalls
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
	err  error
	bind bool // bind a real socket during the health gate, as the app would, and pin it as the real prober does

	mu           sync.Mutex
	probeErr     error
	probeResults []error
	probeCalls   int
	listeners    []net.Listener
}

func (p *fakeProber) waitHealthy(_ context.Context, sock *socketRef, alive func() bool, _ healthConfig) error {
	if p.bind {
		ln, err := listenSocket(sock.path)
		if err != nil {
			return err
		}
		p.mu.Lock()
		p.listeners = append(p.listeners, ln)
		p.mu.Unlock()
		if _, err := sock.dial(); err != nil {
			return err
		}
	}
	if !alive() {
		return errors.New("process exited before becoming healthy")
	}
	return p.err
}

func (p *fakeProber) closeListeners() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ln := range p.listeners {
		_ = ln.Close()
	}
	p.listeners = nil
}

func (p *fakeProber) probeOnce(_ context.Context, _ *socketRef, _ string, _ time.Duration) error {
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
	dir := spec.dirs.release(req.version)
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
		dirs:            newAppDirs(shortTempDir(t), "demo"),
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

// recordedNonce is the instance nonce the state.json fixtures record.
const recordedNonce = "0a1b2c3d0a1b2c3d"

// activeSocketIs reports whether the proxy is routed to the recorded
// instance's socket.
func activeSocketIs(rig *testRig) bool {
	s := rig.ma.activeSocket.Load()
	return s != nil && s.path == rig.spec.dirs.socket(recordedNonce)
}

// socketFiles lists the instance sockets present under the app's run
// dir, by nonce.
func socketFiles(t *testing.T, rig *testRig) []string {
	t.Helper()
	entries, err := os.ReadDir(rig.spec.dirs.run)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && nonceRe.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

// pinnedFiles lists the pinned names under the app's proxy dir, by nonce.
func pinnedFiles(t *testing.T, rig *testRig) []string {
	t.Helper()
	return socketNames(t, rig.spec.dirs.proxy)
}

func socketNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if n, ok := strings.CutSuffix(e.Name(), ".sock"); ok {
			out = append(out, n)
		}
	}
	return out
}

// listenSocket binds a real unix socket at path, as a running app
// does; closing the listener leaves the file, as an app's exit does
// (the unit's ExecStopPost removes it — the fake handle mimics that).
func listenSocket(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	return ln, nil
}

// bindSocket is listenSocket for a test's own fixture.
func bindSocket(t *testing.T, path string) {
	t.Helper()
	ln, err := listenSocket(path)
	must(t, err)
	t.Cleanup(func() { _ = ln.Close() })
}

// touchSocket plants a stale socket file the way a dead app leaves one.
func touchSocket(t *testing.T, path string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(path), 0o750))
	must(t, os.WriteFile(path, nil, 0o600))
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
	ma.wdCtx, ma.wdCancel = context.WithCancel(context.Background())
	t.Cleanup(ma.wdCancel)
	t.Cleanup(rig.prober.closeListeners)
	rig.ma = ma
	return rig
}

func TestDeployFirstVersion(t *testing.T) {
	rig := newTestRig(t)
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if rig.ma.activeSocket.Load() == nil {
		t.Fatal("activeSocket not published after deploy")
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
	must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/1", version: "v1"}))
	sockV1 := rig.ma.activeSocket.Load()
	before := rig.clock.Now()

	must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/2", version: "v2"}))
	if rig.ma.activeSocket.Load() == sockV1 {
		t.Fatal("cutover did not change the active socket")
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
	must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/1", version: "v1"}))
	sockV1 := rig.ma.activeSocket.Load()

	rig.spec.preStart = []string{"./migrate"}
	rig.runner.runOnceErr = errors.New("migration exploded")
	err := rig.ma.Deploy(ctx, deployRequest{url: "https://x/2", version: "v2"})
	if err == nil || rig.ma.activeSocket.Load() != sockV1 {
		t.Fatalf("pre_start failure must abort and keep the old socket routed; err=%v", err)
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
	must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/1", version: "v1"}))
	sockV1 := rig.ma.activeSocket.Load()

	rig.prober.err = errors.New("never became healthy")
	err := rig.ma.Deploy(ctx, deployRequest{url: "https://x/2", version: "v2"})
	if err == nil {
		t.Fatal("expected health-gate failure")
	}
	if rig.ma.activeSocket.Load() != sockV1 {
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
	must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/1", version: "v1"}))
	err := rig.ma.Deploy(ctx, deployRequest{url: "https://x/1", version: "v1"})
	var vErr validationError
	if !errors.As(err, &vErr) {
		t.Fatalf("expected validationError, got %v", err)
	}
}

func TestDeployConcurrentGets409Error(t *testing.T) {
	rig := newTestRig(t)
	rig.ma.deployMu.Lock()
	defer rig.ma.deployMu.Unlock()
	err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"})
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
		must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/a", version: v}))
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
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{PID: 1}}
	rig.store.ok = true
	if err := os.MkdirAll(rig.spec.dirs.release("v7"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := rig.ma.ensureRunning(); err != nil {
		t.Fatalf("ensureRunning: %v", err)
	}
	if rig.ma.activeSocket.Load() == nil {
		t.Fatal("recovered instance not published")
	}
	if got := rig.ma.status().CurrentVersion; got != "v7" {
		t.Fatalf("recovered version = %s, want v7", got)
	}
	if len(rig.runner.started) != 1 {
		t.Fatalf("expected exactly one Start, got %d", len(rig.runner.started))
	}
	// A relaunch creates the dirs it binds, run/ included: the runner
	// resolves every bind source and a missing one refuses the launch.
	for _, d := range []string{rig.spec.dirs.run, rig.spec.dirs.shared} {
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			t.Fatalf("relaunch must create %s: %v", d, err)
		}
	}
}

func TestEnsureRunningReattachesWhenRunnerCan(t *testing.T) {
	rig := newTestRig(t)
	rig.runner.reattachOK = true
	bindSocket(t, rig.spec.dirs.socket(recordedNonce))
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{Unit: "hotserve-demo.v7.0a1b2c3d0a1b2c3d.service"}}
	rig.store.ok = true
	if err := os.MkdirAll(rig.spec.dirs.release("v7"), 0o755); err != nil {
		t.Fatal(err)
	}
	must(t, rig.ma.ensureRunning())
	if !activeSocketIs(rig) {
		t.Fatalf("reattach must keep the recorded socket, got %v", rig.ma.activeSocket.Load())
	}
	if len(rig.runner.started) != 0 {
		t.Fatal("reattach must not start a new process")
	}
}

func TestEnsureRunningNoStateIsNoop(t *testing.T) {
	rig := newTestRig(t)
	must(t, rig.ma.ensureRunning())
	if rig.ma.activeSocket.Load() != nil || len(rig.runner.started) != 0 {
		t.Fatal("nothing should happen without persisted state")
	}
}

func TestEnsureRunningSkipsWhenAlreadyAlive(t *testing.T) {
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
	started := len(rig.runner.started)
	must(t, rig.ma.ensureRunning())
	if len(rig.runner.started) != started {
		t.Fatal("ensureRunning must be a no-op while the instance is alive (reload case)")
	}
}

func TestDestructStopsCurrentInstance(t *testing.T) {
	rig := newTestRig(t)
	markLive(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
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
	spec.env = map[string]string{"OVERRIDE": "inline", "DB": "sqlite:{shared_dir}/app.db", "V": "{version}:{socket}"}

	sock := spec.dirs.socket("0a1b2c3d0a1b2c3d")
	env, err := buildEnv(spec, "v9", sock, spec.dirs.release("v9"))
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
		"V":         "v9:" + sock,
		"SOCKET":    sock,
	} {
		if byKey[k] != want {
			t.Errorf("%s = %q, want %q", k, byKey[k], want)
		}
	}
	for _, gone := range []string{"PORT", "HOST"} {
		if v, ok := byKey[gone]; ok {
			t.Errorf("%s=%q is set: the contract is the socket, nothing listens on a port", gone, v)
		}
	}
}

func TestBuildEnvDoesNotLeakSupervisorSecrets(t *testing.T) {
	t.Setenv("ACME_DNS_API_TOKEN", "hunter2")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "tok")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("LC_ALL", "C.UTF-8")

	env, err := buildEnv(testSpec(t), "v1", "/var/lib/liveswap/demo/run/0a1b2c3d0a1b2c3d.sock", t.TempDir())
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

func TestValidVersionRejectsDotPrefix(t *testing.T) {
	for _, v := range []string{"v1", "1.2.3", "release_2024-01-01", "v1..2", "a.", "_", "-"} {
		if !validVersion(v) {
			t.Errorf("validVersion(%q) = false, want true", v)
		}
	}
	// A leading dot is refused as a whole class: "." and ".." resolve
	// onto the releases dir or the app root, ".extract-1" is deleted by
	// release GC as a staging orphan, and any other dot-name is
	// invisible to it. See versionRe.
	for _, v := range []string{".", "..", "..foo", "...", ".extract-1", ".v1", "", "has/slash", "with space", "..\x00"} {
		if validVersion(v) {
			t.Errorf("validVersion(%q) = true, want false (path-unsafe)", v)
		}
	}
}

func TestExpandArgs(t *testing.T) {
	spec := testSpec(t)
	got := expandArgs([]string{"run", "--rel={release_dir}", "{version}", "--listen={socket}"}, spec, "v2", "/run/x.sock", "/rel/v2")
	if got[1] != "--rel=/rel/v2" || got[2] != "v2" || got[3] != "--listen=/run/x.sock" {
		t.Fatalf("placeholders not expanded: %v", got)
	}
}

// shortTempDir is t.TempDir without the test name in the path: a
// liveswap root the tests bind sockets under, where a long test name
// would push <root>/demo/proxy/<nonce>.sock past sun_path.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ls")
	must(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
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
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"}); err != nil {
		t.Fatalf("deploy v1: %v", err)
	}
	if rig.runner.runOnceCount != 1 {
		t.Fatalf("deploy should run pre_start once, got %d", rig.runner.runOnceCount)
	}

	// Deploy a second version so v1 is no longer the running one.
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/b.tgz", version: "v2"}); err != nil {
		t.Fatalf("deploy v2: %v", err)
	}
	if rig.runner.runOnceCount != 2 {
		t.Fatalf("second deploy should run pre_start, got %d", rig.runner.runOnceCount)
	}
	if err := rig.ma.Deploy(context.Background(), deployRequest{version: "v1", rollback: true}); err != nil {
		t.Fatalf("rollback to v1: %v", err)
	}
	if rig.runner.runOnceCount != 2 {
		t.Fatalf("rollback must not run pre_start; count = %d, want 2", rig.runner.runOnceCount)
	}
}

func TestDeployRejectsExistingVersion(t *testing.T) {
	rig := newTestRig(t)
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"}); err != nil {
		t.Fatalf("deploy v1: %v", err)
	}
	// Deploy a second version so v1 is on disk but not running.
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/b.tgz", version: "v2"}); err != nil {
		t.Fatalf("deploy v2: %v", err)
	}
	// Re-deploying v1 (URL) must be rejected — versions are immutable.
	err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"})
	var vErr validationError
	if !errors.As(err, &vErr) || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("re-deploy of existing version should be a validation error about immutability, got %v", err)
	}
	// But rollback to v1 (which exists) is allowed.
	if err := rig.ma.Deploy(context.Background(), deployRequest{version: "v1", rollback: true}); err != nil {
		t.Fatalf("rollback to existing v1 should succeed, got %v", err)
	}
}

func TestFailedDeployCleansUpRelease(t *testing.T) {
	rig := newTestRig(t)
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"}); err != nil {
		t.Fatalf("deploy v1: %v", err)
	}
	// v2 fails its health gate.
	rig.prober.err = errTest
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/b.tgz", version: "v2"}); err == nil {
		t.Fatal("deploy v2 should have failed the health gate")
	}
	// Its freshly-extracted release must be gone.
	if _, err := os.Stat(rig.spec.dirs.release("v2")); !os.IsNotExist(err) {
		t.Fatalf("failed deploy should remove its release dir, stat err = %v", err)
	}
	// So the same version is retriable once healthy.
	rig.prober.err = nil
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/b.tgz", version: "v2"}); err != nil {
		t.Fatalf("re-deploy of a cleaned-up failed version should succeed: %v", err)
	}
}

func TestFailedRollbackKeepsRelease(t *testing.T) {
	rig := newTestRig(t)
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"}); err != nil {
		t.Fatalf("deploy v1: %v", err)
	}
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/b.tgz", version: "v2"}); err != nil {
		t.Fatalf("deploy v2: %v", err)
	}
	// A rollback to v1 that fails its health gate must NOT delete v1's
	// pre-existing release.
	rig.prober.err = errTest
	if err := rig.ma.Deploy(context.Background(), deployRequest{version: "v1", rollback: true}); err == nil {
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
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"}); err != nil {
		t.Fatalf("deploy v1: %v", err)
	}
	// v2 fails the health gate, and Stop can't confirm the instance
	// exited (it stays alive) — the release must NOT be deleted beneath it.
	rig.prober.err = errTest
	rig.runner.stopErr = errTest
	rig.runner.stopLeavesAlive = true
	err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/b.tgz", version: "v2"})
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
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"}))
	// v2 fails the health gate and Stop reports an error even though
	// the handle then reads as dead. Under cgroup kill "Stop errored"
	// is the only signal a caller gets, so the release stays on disk.
	rig.prober.err = errTest
	rig.runner.stopErr = errTest
	err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/b.tgz", version: "v2"})
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
		must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/a", version: v}))
		mt := time.Now().Add(time.Duration(i-10) * time.Minute)
		_ = os.Chtimes(rig.spec.dirs.release(v), mt, mt)
	}
	// Stopping v2 errors, but the sweep — the runner's own ledger —
	// vouches that only v3 remains, so keep=2 GC proceeds: our memory
	// of a failed stop is not the source of truth, the manager is.
	rig.runner.stopErr = errTest
	rig.runner.stopLeavesAlive = true
	must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/a", version: "v3"}))
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

// Promise: the nonce in the start spec is the one that names the
// socket, so the unit (named from the spec) and the socket (in the
// env) can be checked against each other on reattach.
func TestStartSpecNonceNamesTheSocket(t *testing.T) {
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
	spec := rig.runner.started[0]
	if !nonceRe.MatchString(spec.nonce) {
		t.Fatalf("start spec nonce %q", spec.nonce)
	}
	want := "SOCKET=" + rig.spec.dirs.socket(spec.nonce)
	if spec.env[len(spec.env)-1] != want {
		t.Fatalf("env ends with %q, want %q", spec.env[len(spec.env)-1], want)
	}
	inst := rig.ma.currentInstance()
	if inst.nonce != spec.nonce || inst.socket != rig.spec.dirs.socket(spec.nonce) {
		t.Fatalf("instance %+v does not carry the spec's nonce", inst)
	}
	if st, _, _ := rig.store.load(); st.Nonce != inst.nonce {
		t.Fatalf("state.json records nonce %q, want %q", st.Nonce, inst.nonce)
	}
	if s := rig.ma.status(); s.Socket != inst.socket {
		t.Fatalf("status reports %q, want %q", s.Socket, inst.socket)
	}
}

// Promise: a confirmed sweep is the manager's word that nothing but
// the kept instance runs, so every other socket under run/ is a
// leftover and is removed. Files that are not instance sockets are
// not ours to touch.
func TestSweepPrunesSocketsNotHeldByTheManager(t *testing.T) {
	rig := newTestRig(t)
	rig.prober.bind = true
	must(t, rig.spec.dirs.ensure())
	touchSocket(t, rig.spec.dirs.socket("aaaaaaaaaaaaaaaa"))
	must(t, os.WriteFile(filepath.Join(rig.spec.dirs.run, "junk.txt"), nil, 0o600))
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
	cur := rig.ma.currentInstance()
	if got := socketFiles(t, rig); len(got) != 1 || got[0] != cur.nonce {
		t.Fatalf("sockets after deploy = %v, want only the current instance's %s", got, cur.nonce)
	}
	if _, err := os.Stat(filepath.Join(rig.spec.dirs.run, "junk.txt")); err != nil {
		t.Fatal("a file that is not an instance socket must be left alone")
	}
	// An unconfirmed sweep vouches for nothing: nothing is pruned, and
	// the aborted launch takes its own dir with it.
	touchSocket(t, rig.spec.dirs.socket("bbbbbbbbbbbbbbbb"))
	rig.runner.sweepErr = errTest
	_ = rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"})
	got := socketFiles(t, rig)
	slices.Sort(got)
	if want := []string{"bbbbbbbbbbbbbbbb", cur.nonce}; !slices.Equal(got, slices.Sorted(slices.Values(want))) {
		t.Fatalf("sockets after an unconfirmed sweep = %v, want %v", got, want)
	}
}

// Promise: an instance's socket is gone once the instance is stopped —
// the unit removes it (ExecStopPost=, which the fake handle mimics),
// and the deploy's sweep would prune it anyway: the old one after a
// cutover, the new one after a failed health gate.
func TestStopRemovesTheInstanceSocket(t *testing.T) {
	rig := newTestRig(t)
	rig.prober.bind = true
	ctx := context.Background()
	must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/1", version: "v1"}))
	v1 := rig.ma.currentInstance()
	if _, err := os.Stat(v1.socket); err != nil {
		t.Fatalf("the bound socket should exist: %v", err)
	}

	rig.prober.err = errors.New("never became healthy")
	if err := rig.ma.Deploy(ctx, deployRequest{url: "https://x/2", version: "v2"}); err == nil {
		t.Fatal("expected the health gate to fail")
	}
	if got := socketFiles(t, rig); len(got) != 1 || got[0] != v1.nonce {
		t.Fatalf("after a failed gate only v1's socket remains, got %v", got)
	}

	rig.prober.err = nil
	must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/3", version: "v3"}))
	v3 := rig.ma.currentInstance()
	if got := socketFiles(t, rig); len(got) != 1 || got[0] != v3.nonce {
		t.Fatalf("after a cutover only v3's socket remains, got %v", got)
	}
	if _, err := os.Stat(v1.socket); !os.IsNotExist(err) {
		t.Fatalf("v1's socket must be removed after it is stopped: %v", err)
	}
}

// Promise: the recorded nonce names the socket the proxy will dial, so
// a record is adopted only when the nonce is the recorded unit's own
// and the socket it names is there. Anything else relaunches rather
// than routing to a socket that answers nobody.
func TestEnsureRunningRefusesARecordWhoseSocketItCannotDial(t *testing.T) {
	for name, tc := range map[string]struct {
		nonce   string
		bind    bool
		symlink bool
	}{
		"nonce is not the unit's":  {nonce: "ffffffffffffffff", bind: true},
		"socket file is gone":      {nonce: recordedNonce},
		"the name is a symlink":    {nonce: recordedNonce, symlink: true},
		"the name is a plain file": {nonce: recordedNonce, bind: false, symlink: false},
		// A nonce that is not one is never a path component: the
		// decoy it would escape to is planted, and must survive.
		"the nonce is a traversal": {nonce: "../../target"},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newTestRig(t)
			rig.runner.reattachOK = true
			socket := rig.spec.dirs.socket(tc.nonce)
			decoy := filepath.Join(rig.spec.dirs.root, "target.sock")
			switch {
			case tc.bind:
				bindSocket(t, socket)
			case tc.symlink:
				victim := filepath.Join(t.TempDir(), "admin.sock")
				bindSocket(t, victim)
				must(t, os.MkdirAll(filepath.Dir(socket), 0o750))
				must(t, os.Symlink(victim, socket))
			case name == "the name is a plain file":
				touchSocket(t, socket)
			case name == "the nonce is a traversal":
				touchSocket(t, decoy)
			}
			rig.store.state = appState{CurrentVersion: "v7", Nonce: tc.nonce, Handle: handleState{Unit: "hotserve-demo.v7.0a1b2c3d0a1b2c3d.service"}}
			rig.store.ok = true
			must(t, os.MkdirAll(rig.spec.dirs.release("v7"), 0o755))
			must(t, rig.ma.ensureRunning())
			if _, err := os.Stat(decoy); name == "the nonce is a traversal" && err != nil {
				t.Fatalf("a traversal in the recorded nonce reached outside the app's dirs: %v", err)
			}
			if len(rig.runner.reattachSeen) != 1 || rig.runner.reattachSeen[0].Unit != "" {
				t.Fatalf("the record must not reach the manager: %+v", rig.runner.reattachSeen)
			}
			if rig.runner.startCount() != 1 {
				t.Fatal("the recorded version is relaunched instead")
			}
			if s := rig.ma.activeSocket.Load(); s == nil || s.path == rig.spec.dirs.socket(tc.nonce) {
				t.Fatalf("the recorded socket must never be routed: %v", s)
			}
		})
	}
	// And a record whose nonce is the unit's, with the socket bound, is
	// adopted as-is.
	rig := newTestRig(t)
	rig.runner.reattachOK = true
	bindSocket(t, rig.spec.dirs.socket(recordedNonce))
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{Unit: "hotserve-demo.v7.0a1b2c3d0a1b2c3d.service"}}
	rig.store.ok = true
	must(t, os.MkdirAll(rig.spec.dirs.release("v7"), 0o755))
	must(t, rig.ma.ensureRunning())
	if rig.runner.startCount() != 0 || !activeSocketIs(rig) {
		t.Fatalf("a record whose socket is the unit's own is adopted, starts=%d", rig.runner.startCount())
	}
	if got := rig.ma.currentInstance(); got.nonce != recordedNonce {
		t.Fatalf("the adopted instance carries the recorded nonce, got %+v", got)
	}
}

// Promise: an app removed from the config leaves no socket behind.
func TestDestructRemovedAppLeavesNoSocket(t *testing.T) {
	rig := newTestRig(t)
	rig.prober.bind = true
	markLive(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
	touchSocket(t, rig.spec.dirs.socket("aaaaaaaaaaaaaaaa"))
	touchSocket(t, filepath.Join(rig.spec.dirs.proxy, "bbbbbbbbbbbbbbbb.sock"))
	must(t, rig.ma.Destruct())
	if got := socketFiles(t, rig); len(got) != 0 {
		t.Fatalf("sockets after removal = %v, want none", got)
	}
	if got := pinnedFiles(t, rig); len(got) != 0 {
		t.Fatalf("pinned names after removal = %v, want none", got)
	}
}

func TestDestructOnRemovalSweepsWholeApp(t *testing.T) {
	rig := newTestRig(t)
	markLive(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
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
	markLive(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
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
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
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
	bindSocket(t, rig.spec.dirs.socket(recordedNonce))
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{Unit: "hotserve-demo.v7.0a1b2c3d0a1b2c3d.service"}}
	rig.store.ok = true
	must(t, os.MkdirAll(rig.spec.dirs.release("v7"), 0o755))
	rig.runner.reattachErrs = []error{errTest}
	err := rig.ma.ensureRunning()
	if !transientRecovery(err) || !strings.Contains(err.Error(), "not relaunching") {
		t.Fatalf("expected a transient refusal, got %v", err)
	}
	if rig.runner.startCount() != 0 || rig.ma.activeSocket.Load() != nil {
		t.Fatal("must not launch or publish while the recorded unit's state is unknown")
	}
	// The pin made before asking the manager survives a transient
	// answer: the unit may be serving, and the app may since have
	// replaced its own name, so the retry must adopt through the pin.
	if got := pinnedFiles(t, rig); len(got) != 1 || got[0] != recordedNonce {
		t.Fatalf("a transient reattach must keep the pin, got %v", got)
	}
	must(t, os.Remove(rig.spec.dirs.socket(recordedNonce)))
	rig.runner.reattachOK = true
	must(t, rig.ma.ensureRunning())
	if rig.runner.startCount() != 0 || !activeSocketIs(rig) {
		t.Fatalf("the retry must adopt through the surviving pin, starts=%d", rig.runner.startCount())
	}
}

// Recovery holds the app's ownerLock for its whole run, so a
// start-time prune of this app's dirs (a reload adding it back races
// the sweep of apps no longer configured) can never interleave with
// adopting them. Pinned from the other side: recovery waits for the
// lock — its own app's, another app's held lock is nothing to it.
func TestEnsureRunningWaitsForTheOwnershipLock(t *testing.T) {
	rig := newTestRig(t)
	rig.runner.reattachOK = true
	bindSocket(t, rig.spec.dirs.socket(recordedNonce))
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{Unit: "hotserve-demo.v7.0a1b2c3d0a1b2c3d.service"}}
	rig.store.ok = true
	must(t, os.MkdirAll(rig.spec.dirs.release("v7"), 0o755))

	otherApp := ownerLock("somebody-else")
	otherApp.Lock()
	defer otherApp.Unlock()
	mu := ownerLock("demo")
	mu.Lock()
	done := make(chan error, 1)
	go func() { done <- rig.ma.ensureRunning() }()
	time.Sleep(50 * time.Millisecond)
	if rig.ma.activeSocket.Load() != nil {
		mu.Unlock()
		t.Fatal("recovery adopted while the app's ownership lock was held")
	}
	mu.Unlock()
	must(t, <-done)
	if !activeSocketIs(rig) {
		t.Fatal("recovery adopts once the lock is free")
	}
}

// A definitive "not running" from the manager, by contrast, retires
// the pin made for the record: the relaunch gets its own.
func TestEnsureRunningNotAttachedRetiresThePin(t *testing.T) {
	rig := newTestRig(t)
	bindSocket(t, rig.spec.dirs.socket(recordedNonce))
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{Unit: "hotserve-demo.v7.0a1b2c3d0a1b2c3d.service"}}
	rig.store.ok = true
	must(t, os.MkdirAll(rig.spec.dirs.release("v7"), 0o755))
	must(t, rig.ma.ensureRunning()) // reattachOK is false: definitively not running
	if rig.runner.startCount() != 1 {
		t.Fatalf("the recorded version is relaunched, starts=%d", rig.runner.startCount())
	}
	for _, n := range pinnedFiles(t, rig) {
		if n == recordedNonce {
			t.Fatal("the pin made for a unit the manager does not hold must be retired")
		}
	}
}

func TestRecoverRetriesTransientErrorsUntilReattached(t *testing.T) {
	rig := newTestRig(t)
	bindSocket(t, rig.spec.dirs.socket(recordedNonce))
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{Unit: "hotserve-demo.v7.0a1b2c3d0a1b2c3d.service"}}
	rig.store.ok = true
	must(t, os.MkdirAll(rig.spec.dirs.release("v7"), 0o755))
	rig.runner.reattachErrs = []error{errTest, errTest}
	rig.runner.reattachOK = true
	done := make(chan struct{})
	go func() { rig.ma.recover(context.Background(), zap.NewNop()); close(done) }()
	advanceUntil(t, rig, recoveryBackoffFloor, "reattach after two transient failures", func() bool {
		return activeSocketIs(rig)
	})
	<-done
	if rig.runner.startCount() != 0 || !activeSocketIs(rig) {
		t.Fatalf("expected a reattach on the third try, starts=%d socket=%v", rig.runner.startCount(), rig.ma.activeSocket.Load())
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
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{PID: 1}}
	rig.store.ok = true // release dir deliberately missing: not something a retry fixes
	done := make(chan struct{})
	go func() { rig.ma.recover(context.Background(), zap.NewNop()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("recover must return on a permanent error, not retry forever")
	}
}

func TestEnsureRunningReattachSweepFailureIsTransient(t *testing.T) {
	rig := newTestRig(t)
	bindSocket(t, rig.spec.dirs.socket(recordedNonce))
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{Unit: "hotserve-demo.v7.0a1b2c3d0a1b2c3d.service"}}
	rig.store.ok = true
	must(t, os.MkdirAll(rig.spec.dirs.release("v7"), 0o755))
	rig.runner.reattachOK = true
	rig.runner.sweepErr = errTest
	err := rig.ma.ensureRunning()
	if !transientRecovery(err) {
		t.Fatalf("an unsettled ledger after reattach must be reported as transient, got %v", err)
	}
	if !activeSocketIs(rig) {
		t.Fatal("the reattached instance still serves meanwhile")
	}
	// The retry path (instance alive) sweeps again and succeeds.
	rig.runner.sweepErr = nil
	must(t, rig.ma.ensureRunning())
	if rig.runner.sweepCount() != 2 || rig.runner.lastSweep() != rig.runner.handleAt(0) {
		t.Fatalf("retry must sweep keeping the live instance, sweeps=%v", rig.runner.sweeps)
	}
}

func TestCleanupJoinsRecoveryBeforeReleasing(t *testing.T) {
	rig := newTestRig(t)
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{Unit: "hotserve-demo.v7.0a1b2c3d0a1b2c3d.service"}}
	rig.store.ok = true
	must(t, os.MkdirAll(rig.spec.dirs.release("v7"), 0o755))
	rig.runner.reattachErrs = []error{errTest, errTest, errTest, errTest, errTest, errTest}
	// What App.Start does for its apps: recovery owned by the config.
	a := &App{logger: zap.NewNop(), managed: map[string]*managedApp{"demo": rig.ma}}
	ctx, cancel := context.WithCancel(context.Background())
	a.recoverCancel = cancel
	a.recoverWG = new(sync.WaitGroup)
	a.recoverWG.Add(1)
	exited := make(chan struct{})
	go func() { defer a.recoverWG.Done(); rig.ma.recover(ctx, zap.NewNop()); close(exited) }()
	waitUntil(t, "first attempt", func() bool { return rig.runner.reattachCount() >= 1 })
	// The config is cleaned up while recovery is parked in backoff:
	// Cleanup must end it before anything else happens.
	must(t, a.Cleanup())
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery must have exited by the time Cleanup returns")
	}
	if rig.runner.startCount() != 0 {
		t.Fatal("nothing may be launched by a cancelled recovery")
	}
}

// markLive stands in for a started liveswap config being live for the
// rest of the test (what App.Start/Cleanup maintain).
func markLive(t *testing.T) {
	t.Helper()
	liveStartedApps.Add(1)
	t.Cleanup(func() { liveStartedApps.Add(-1) })
}

func TestDestructOnUnconfiguredAppIsANoop(t *testing.T) {
	// A pooled app whose config's Start failed before configuring it
	// (the manager probe, on a reload adding the app) has no runner.
	markLive(t)
	ma := newManagedApp("fresh")
	must(t, ma.Destruct())
}

func TestRollbackConfigRestoresTheServingDefinition(t *testing.T) {
	rig := newTestRig(t)
	clients := &fetchClients{}
	specA, specB := testSpec(t), testSpec(t)
	specB.grace = 99 * time.Second
	ownerA, ownerB := new(int), new(int)
	rig.ma.configure(ownerA, specA, zap.NewNop(), clients, userManager)
	rig.ma.configure(ownerB, specB, zap.NewNop(), clients, userManager)
	// A successful reload: A is cleaned up after B configured — not
	// the last writer, nothing happens.
	if rig.ma.rollbackConfig(ownerA) || rig.ma.snapshot().spec != specB {
		t.Fatal("a replaced config must not roll back the newer one")
	}
	// A rejected candidate: B is cleaned up while A still serves.
	if !rig.ma.rollbackConfig(ownerB) || rig.ma.snapshot().spec != specA {
		t.Fatal("a rejected candidate must restore the serving definition")
	}
	if rig.ma.rollbackConfig(ownerB) {
		t.Fatal("rollback is one-shot")
	}
}

func TestRollbackConfigWakesTheWatchdog(t *testing.T) {
	rig := newTestRig(t)
	clients := &fetchClients{}
	on, off := testSpec(t), testSpec(t)
	off.watchdogOn = false
	ownerA, ownerB := new(int), new(int)
	rig.ma.configure(ownerA, on, zap.NewNop(), clients, userManager)
	rig.ma.configure(ownerB, off, zap.NewNop(), clients, userManager)
	// Drain the pokes configure sent, then roll back: the rollback
	// itself must poke, or a parked loop never re-reads watchdog=on.
	for len(rig.ma.wdNotify) > 0 {
		<-rig.ma.wdNotify
	}
	if !rig.ma.rollbackConfig(ownerB) {
		t.Fatal("rollback expected")
	}
	select {
	case <-rig.ma.wdNotify:
	default:
		t.Fatal("rollback must wake the watchdog so it re-snapshots the restored definition")
	}
}

func TestDeploySweepsBeforePreStart(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.preStart = []string{"migrate"}
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
	// pre-pre_start (keep old=nil), pre-Start (keep old=nil), pre-GC (keep new).
	if n := rig.runner.sweepCount(); n != 3 {
		t.Fatalf("expected 3 sweeps, got %d", n)
	}
	rig.runner.sweepErr = errTest
	err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"})
	if err == nil || !strings.Contains(err.Error(), "not running pre_start") {
		t.Fatalf("an unconfirmed ledger must block the migration too, got %v", err)
	}
	if rig.runner.runOnceCount != 1 {
		t.Fatalf("no second pre_start may run, got %d", rig.runner.runOnceCount)
	}
}

func TestDestructBeforeStartTouchesNothing(t *testing.T) {
	// `hotserve validate`, and a candidate config that never became
	// the serving one (another app's Start failed): no started config
	// is live, so whatever the manager runs belongs to whoever is or
	// will be serving it and must be left alone.
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
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

func TestEnsureRunningRefusesAForeignUnitName(t *testing.T) {
	rig := newTestRig(t)
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{Unit: "hotserve-other.v1.0a1b2c3d0a1b2c3d.service"}}
	rig.store.ok = true
	must(t, os.MkdirAll(rig.spec.dirs.release("v7"), 0o755))
	must(t, rig.ma.ensureRunning())
	if len(rig.runner.reattachSeen) != 1 || rig.runner.reattachSeen[0].Unit != "" {
		t.Fatalf("a unit that is not ours must never reach the manager: %+v", rig.runner.reattachSeen)
	}
	if rig.runner.startCount() != 1 {
		t.Fatal("the recorded version is relaunched instead")
	}
}

func TestEnsureRunningNoStateStillSweeps(t *testing.T) {
	rig := newTestRig(t)
	must(t, rig.ma.ensureRunning())
	if rig.runner.startCount() != 0 {
		t.Fatal("nothing recorded ⇒ nothing launched")
	}
	if n := rig.runner.sweepCount(); n != 1 || rig.runner.sweeps[0] != nil {
		t.Fatalf("with no state the manager must still be swept with keep=nil, sweeps=%d", n)
	}
	rig.runner.sweepErr = errTest
	if err := rig.ma.ensureRunning(); !transientRecovery(err) {
		t.Fatalf("an unconfirmed no-state sweep must be retried, got %v", err)
	}
}

func TestEnsureRunningDeployInProgressIsTransient(t *testing.T) {
	rig := newTestRig(t)
	rig.ma.deployMu.Lock()
	defer rig.ma.deployMu.Unlock()
	if err := rig.ma.ensureRunning(); !transientRecovery(err) {
		t.Fatalf("a deploy holding the lock must make recovery look again later, got %v", err)
	}
}

func TestRecoveryErrorClassification(t *testing.T) {
	if transientRecovery(nil) {
		t.Fatal("nil is not an error")
	}
	if !transientRecovery(errTest) || !transientRecovery(&unitUnconfirmedError{unit: "u", err: errTest}) {
		t.Fatal("unclassified and unconfirmed errors are retried")
	}
	if transientRecovery(&permanentRecoveryError{errTest}) {
		t.Fatal("permanent errors are not retried")
	}
}

func TestEnsureRunningRelaunchSweepsStrays(t *testing.T) {
	rig := newTestRig(t)
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{PID: 1}}
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
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{PID: 1}}
	rig.store.ok = true
	must(t, os.MkdirAll(rig.spec.dirs.release("v7"), 0o755))
	rig.runner.sweepErr = errTest
	if err := rig.ma.ensureRunning(); err == nil || !strings.Contains(err.Error(), "not launching") {
		t.Fatalf("expected a refusal to launch, got %v", err)
	}
	if rig.runner.startCount() != 0 || rig.ma.activeSocket.Load() != nil {
		t.Fatal("nothing may be launched or published beside a possibly-running unit")
	}
	// Recovery retries this every minute: the launch prepared and then
	// abandoned must leave no run dir behind, or the retries leak one
	// apiece until a sweep confirms.
	if got := socketFiles(t, rig); len(got) != 0 {
		t.Fatalf("an abandoned launch left run dirs behind: %v", got)
	}
}

// A start the runner cannot confirm may have reached the manager: the
// unit may be coming up, and its socket dir must stay for it to bind
// in — the next confirmed sweep settles it. Same for a stop that
// could not be confirmed. Only a launch known to be dead is retired.
func TestAmbiguousStartKeepsTheLaunchSocketDir(t *testing.T) {
	rig := newTestRig(t)
	rig.runner.setStartErr(&unitUnconfirmedError{unit: "u", err: errTest})
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}); err == nil {
		t.Fatal("expected the ambiguous start to fail the deploy")
	}
	if got := socketFiles(t, rig); len(got) != 1 {
		t.Fatalf("an ambiguous start must keep the launch's run dir, got %v", got)
	}
	// A definitively failed start, by contrast, retires its launch —
	// and this deploy's confirmed pre-Start sweep is the "next
	// confirmed sweep" that prunes the ambiguous one's dir.
	rig.runner.setStartErr(errTest)
	_ = rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"})
	if got := socketFiles(t, rig); len(got) != 0 {
		t.Fatalf("a failed start retires its launch and the confirmed sweep prunes the ambiguous one, got %v", got)
	}
	// The relaunch path makes the same distinction.
	rig2 := newTestRig(t)
	rig2.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{PID: 1}}
	rig2.store.ok = true
	must(t, os.MkdirAll(rig2.spec.dirs.release("v7"), 0o755))
	rig2.runner.setStartErr(&unitUnconfirmedError{unit: "u", err: errTest})
	if err := rig2.ma.ensureRunning(); err == nil {
		t.Fatal("expected the ambiguous relaunch to be reported")
	}
	if got := socketFiles(t, rig2); len(got) != 1 {
		t.Fatalf("an ambiguous relaunch must keep its run dir, got %v", got)
	}
}

// The socket ledger after every way a deploy can end: what is on disk
// under run/ and proxy/ is exactly the instances that are live — or
// may be, when the runner could not say — and nothing else. Each row
// injects one failure into a second deploy over a serving v1; the
// expectation names which of the two launches' dirs may remain.
func TestSocketLedgerAfterEveryDeployOutcome(t *testing.T) {
	type outcome struct {
		inject func(rig *testRig)
		keepV1 bool // v1 still serves (its dirs stay)
		keepV2 bool // v2's dirs stay: it serves, or it may be live and cannot be touched
	}
	for name, tc := range map[string]outcome{
		"success":                 {inject: func(*testRig) {}, keepV1: false, keepV2: true},
		"env_file missing":        {inject: func(r *testRig) { r.spec.envFile = filepath.Join(t.TempDir(), "none.env") }, keepV1: true},
		"pre_start fails":         {inject: func(r *testRig) { r.spec.preStart = []string{"./migrate"}; r.runner.runOnceErr = errTest }, keepV1: true},
		"sweep unconfirmed":       {inject: func(r *testRig) { r.runner.sweepErr = errTest }, keepV1: true},
		"start fails":             {inject: func(r *testRig) { r.runner.setStartErr(errTest) }, keepV1: true},
		"start ambiguous":         {inject: func(r *testRig) { r.runner.setStartErr(&unitUnconfirmedError{unit: "u", err: errTest}) }, keepV1: true, keepV2: true},
		"health gate fails":       {inject: func(r *testRig) { r.prober.err = errTest }, keepV1: true},
		"new instance stop hangs": {inject: func(r *testRig) { r.prober.err = errTest; r.runner.stopErr = errTest; r.runner.stopLeavesAlive = true }, keepV1: true, keepV2: true},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newTestRig(t)
			rig.prober.bind = true
			ctx := context.Background()
			must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/1", version: "v1"}))
			v1 := rig.ma.currentInstance().nonce
			tc.inject(rig)
			err := rig.ma.Deploy(ctx, deployRequest{url: "https://x/2", version: "v2"})
			if (err == nil) != (name == "success") {
				t.Fatalf("deploy v2: err = %v", err)
			}
			for _, dir := range []struct {
				name string
				got  []string
			}{{"run", socketFiles(t, rig)}, {"proxy", pinnedFiles(t, rig)}} {
				var want []string
				if tc.keepV1 {
					want = append(want, v1)
				}
				others := 0
				for _, n := range dir.got {
					if n != v1 {
						others++
					}
				}
				if dir.name == "proxy" && name == "start ambiguous" {
					continue // never pinned: the start never returned a handle to probe
				}
				if (tc.keepV1 && !slices.Contains(dir.got, v1)) || (!tc.keepV1 && slices.Contains(dir.got, v1)) {
					t.Errorf("%s: v1's entry present = %v, want %v (got %v)", dir.name, slices.Contains(dir.got, v1), tc.keepV1, dir.got)
				}
				if wantOthers := map[bool]int{true: 1, false: 0}[tc.keepV2]; others != wantOthers {
					t.Errorf("%s: %d entries besides v1's, want %d (got %v, v1 %s, want-v1 %v)", dir.name, others, wantOthers, dir.got, v1, want)
				}
			}
		})
	}
}

// Promise: an address the proxy was ever handed never resolves to a
// live socket again once its instance is retired — the property the
// hard-linked, nonce-named pin exists for (a descriptor number would
// be recycled). Across several cutovers, every retired address is
// gone and only the current one remains.
func TestRetiredAddressesNeverResolveAgain(t *testing.T) {
	rig := newUpstreamsRig(t)
	u := &Upstreams{App: "demo", ma: rig.ma}
	var retired []string
	for i := 1; i <= 6; i++ {
		must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a", version: fmt.Sprintf("v%d", i)}))
		ups, err := u.GetUpstreams(httptest.NewRequest("GET", "/", nil))
		must(t, err)
		addr := strings.TrimPrefix(ups[0].Dial, "unix/")
		if slices.Contains(retired, addr) {
			t.Fatalf("deploy %d was handed a retired address %s", i, addr)
		}
		for _, old := range retired {
			if _, err := os.Lstat(old); !os.IsNotExist(err) {
				t.Fatalf("retired address %s still resolves after deploy %d: %v", old, i, err)
			}
		}
		if st, err := os.Lstat(addr); err != nil || st.Mode()&os.ModeSocket == 0 {
			t.Fatalf("current address %s is not a socket: %v", addr, err)
		}
		retired = append(retired, addr)
	}
}

// Promise: a launch whose preparation fails — a missing env_file, the
// case recovery retries forever — reserves nothing. The nonce dir is
// created exclusively, so an existing one is never adopted either.
func TestPrepareLaunchFailureReservesNothing(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.envFile = filepath.Join(t.TempDir(), "missing.env")
	if _, err := rig.spec.prepareLaunch("v1"); err == nil {
		t.Fatal("a missing env_file must fail preparation")
	}
	if got := socketFiles(t, rig); len(got) != 0 {
		t.Fatalf("a failed preparation left run dirs behind: %v", got)
	}
	// And a deploy that aborts after preparation (unconfirmed sweep)
	// leaves none either.
	rig.spec.envFile = ""
	rig.runner.sweepErr = errTest
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}); err == nil {
		t.Fatal("expected the deploy to abort on the unconfirmed sweep")
	}
	if got := socketFiles(t, rig); len(got) != 0 {
		t.Fatalf("an aborted deploy left run dirs behind: %v", got)
	}
}

func TestDeployAbortsStartWhenSweepUnconfirmed(t *testing.T) {
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
	rig.runner.sweepErr = errTest
	err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"})
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
	markLive(t)
	must(t, rig.ma.Destruct())
	if rig.runner.stopCount() != 0 || rig.runner.sweepCount() != 1 || rig.runner.sweeps[0] != nil {
		t.Fatalf("removal with nothing tracked must still sweep the app: stops=%d sweeps=%v", rig.runner.stopCount(), rig.runner.sweeps)
	}
}

func TestDeploySweepsBeforeGCAndSkipsGCWhenSweepFails(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	for i, v := range []string{"v1", "v2"} {
		must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/a", version: v}))
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
	must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/a", version: "v3"}))
	for _, v := range []string{"v1", "v2", "v3"} {
		if _, err := os.Stat(rig.spec.dirs.release(v)); err != nil {
			t.Fatalf("release %s must survive a deploy whose sweep failed: %v", v, err)
		}
	}
	if rig.runner.lastSweep() != rig.runner.handleAt(2) {
		t.Fatal("the pre-GC sweep must keep the just-promoted instance")
	}
	must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/a", version: "v4"}))
	if _, err := os.Stat(rig.spec.dirs.release("v1")); err == nil {
		t.Fatal("once the sweep vouches, GC catches up")
	}
}

func TestFailedDeployKeepsReleaseWhenStartUnconfirmed(t *testing.T) {
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"}))
	rig.runner.setStartErr(&unitUnconfirmedError{unit: "hotserve-demo.v2.deadbeefdeadbeef.service", err: errTest})
	err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/b.tgz", version: "v2"})
	if err == nil || !strings.Contains(err.Error(), "left on disk") {
		t.Fatalf("an unreconciled start must keep the release: %v", err)
	}
	if _, statErr := os.Stat(rig.spec.dirs.release("v2")); statErr != nil {
		t.Fatalf("release must survive an unconfirmed start: %v", statErr)
	}
	rig.runner.setStartErr(nil)
	rig.runner.runOnceErr = &unitUnconfirmedError{unit: "hotserve-demo.v3.deadbeefdeadbeef.prestart.service", err: errTest}
	rig.spec.preStart = []string{"migrate"}
	err = rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/c.tgz", version: "v3"})
	if err == nil || !strings.Contains(err.Error(), "left on disk") {
		t.Fatalf("an unreconciled pre_start must keep the release: %v", err)
	}
}
