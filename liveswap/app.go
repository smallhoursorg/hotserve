package liveswap

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// appDirs is the on-disk layout for one app:
//
//	<root>/<app>/releases/<version>/   one dir per deployed version
//	<root>/<app>/shared/               persistent data, survives deploys
//	<root>/<app>/tmp/                  download staging
//	<root>/<app>/state.json            current version + process handle
//	<root>/<app>/current -> releases/<version>   convenience symlink
type appDirs struct {
	app      string
	releases string
	shared   string
	tmp      string
	state    string
	current  string
}

func newAppDirs(root, name string) appDirs {
	app := filepath.Join(root, name)
	return appDirs{
		app:      app,
		releases: filepath.Join(app, "releases"),
		shared:   filepath.Join(app, "shared"),
		tmp:      filepath.Join(app, "tmp"),
		state:    filepath.Join(app, "state.json"),
		current:  filepath.Join(app, "current"),
	}
}

func (d appDirs) release(version string) string {
	// versionPathComponent: identity for valid tags, mechanical
	// traversal containment (and the analyzer-modeled sanitizer) for
	// anything else — validVersion remains the actual gate upstream.
	return filepath.Join(d.releases, versionPathComponent(version))
}

func (d appDirs) ensure() error {
	for _, dir := range []string{d.releases, d.shared, d.tmp} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	return nil
}

// appSpec is the fully-defaulted, internal form of one app's config.
// A fresh spec is installed on every config load; the managedApp holds
// it behind a lock and snapshots it at the start of each deploy, so a
// mid-deploy reload never sees a torn config.
type appSpec struct {
	name            string
	command         []string
	preStart        []string
	env             map[string]string
	envFile         string
	trust           []trustSource
	healthPath      string // "" = no HTTP check (health_path off)
	healthInterval  time.Duration
	healthTimeout   time.Duration
	soak            time.Duration
	deadline        time.Duration
	drain           time.Duration
	grace           time.Duration
	watchdogOn      bool
	wdFailures      int
	wdGrace         time.Duration
	wdRestarts      int
	wdWindow        time.Duration
	keep            int
	maxArtifactSize int64
	allowInsecure   bool
	allowlist       []artifactAllowEntry
	dirs            appDirs
}

// deployRequest is the validated webhook payload. The three sources are
// mutually exclusive: a URL to pull (the default), a pushed archive
// already staged on disk (localArchive), or a rollback to an existing
// on-disk release (rollback). URL and AuthHeader come from the JSON
// body; the rest are set server-side by the handler.
type deployRequest struct {
	URL        string `json:"url"`
	Version    string `json:"version"`
	AuthHeader string `json:"auth_header,omitempty"`
	// localArchive is a path to an already-staged pushed tarball (the
	// upload path); empty for a URL pull.
	localArchive string
	// rollback relaunches an existing on-disk release/<Version> without
	// fetching or extracting anything.
	rollback bool
	// by is the label of the trust source that authorized this deploy.
	by string
}

// source names the deploy's artifact source, for the audit log.
func (r deployRequest) source() string {
	switch {
	case r.rollback:
		return "rollback"
	case r.localArchive != "":
		return "push"
	default:
		return "url"
	}
}

// deployResult records the outcome of the most recent deploy attempt
// for the status endpoint and webhook responses.
type deployResult struct {
	Version    string    `json:"version"`
	Status     string    `json:"status"` // "succeeded" | "failed"
	Error      string    `json:"error,omitempty"`
	Phase      string    `json:"phase,omitempty"`       // phase reached when it failed
	By         string    `json:"deployed_by,omitempty"` // the trust source that authorized it
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

// instance is one running version of an app.
type instance struct {
	version string
	port    int
	handle  handle
}

// Sentinel errors the webhook handler maps to status codes.
var (
	errDeployInProgress = errors.New("a deploy is already in progress for this app")
)

// validationError is a bad request (422), as opposed to a deploy that
// failed on its own merits (500).
type validationError struct{ msg string }

func (e validationError) Error() string { return e.msg }

// managedApp owns everything about one app that must survive config
// reloads: the running instance, the active port the proxy reads, and
// the single-deploy-at-a-time lock. It lives in the package-level
// UsagePool (see liveswap.go), never inside a config's module instance.
type managedApp struct {
	name string

	// specMu guards spec and every collaborator below it: they are all
	// (re)wired on config load while deploys may be reading them, so
	// readers take the snapshot accessors, never the fields.
	specMu    sync.RWMutex
	spec      *appSpec
	verifiers []verifier // resolved deploy-auth trust sources; rewired every reload
	runner    runner
	prober    prober
	fetch     fetcher
	clock     clock
	store     stateStore
	logger    *zap.Logger

	// deployMu serializes deploys per app. TryLock (not a queue): a
	// concurrent webhook gets an immediate 409 and CI can retry.
	deployMu sync.Mutex

	// mu guards current, phase, lastDeploy and leaked.
	mu         sync.Mutex
	current    *instance
	phase      string
	lastDeploy *deployResult
	// leaked is the set of release versions whose processes could not
	// be confirmed stopped (a Stop error: workers that survived their
	// group's sweep). Every release GC leaves them alone, however old
	// they get. Persisted in state.json and restored on start, because
	// such a worker outlives a Caddy restart too. An entry clears when
	// its release dir is gone — the operator's signal that the strays
	// were dealt with. Surfaced as leaked_releases in status.
	leaked map[string]struct{}

	// activePort is what GetUpstreams reads on every request; storing
	// it is the cutover. 0 = nothing serving yet.
	activePort atomic.Int64

	// Watchdog plumbing. The goroutine is pool-scoped like everything
	// else here: started once (first Provision), never touched by
	// reloads, torn down in Destruct BEFORE the child is stopped so a
	// mid-restart watchdog can never orphan a fresh process.
	wdStarted bool // under specMu
	wdCancel  context.CancelFunc
	wdWG      sync.WaitGroup
	wdNotify  chan struct{} // buffered(1); poked whenever current changes
	wd        watchdogState
}

func newManagedApp(name string) *managedApp {
	return &managedApp{name: name, phase: "idle", wdNotify: make(chan struct{}, 1), leaked: map[string]struct{}{}}
}

// configure installs the latest spec and (re)wires collaborators. On
// the first provision the runner/prober/etc. are created; on reloads
// the spec, logger and state path are refreshed while the runner — and
// with it any running child process — is left untouched: a changed
// definition takes effect on the next deploy, never by restarting a
// running app.
func (ma *managedApp) configure(spec *appSpec, logger *zap.Logger, clients *fetchClients) {
	ma.specMu.Lock()
	defer ma.specMu.Unlock()
	changed := ma.spec != nil && !specEqual(ma.spec, spec)
	ma.spec = spec
	// Deploy auth is not tied to the running process, so — unlike the
	// runner — it is rewired on every reload and takes effect at once.
	ma.verifiers = resolveVerifiers(spec.trust, clients.jwks)
	ma.logger = logger
	if ma.runner == nil {
		ma.runner = newExecRunner(logger)
		ma.prober = &httpProber{client: clients.health, clock: realClock{}}
		ma.fetch = &releaseFetcher{client: clients.download}
		ma.clock = realClock{}
	} else if er, ok := ma.runner.(*execRunner); ok {
		er.setLogger(logger)
	}
	ma.store = &fileStateStore{path: spec.dirs.state}
	if changed {
		logger.Info("app definition changed; it will apply on the next deploy")
	}
	// Wake the watchdog so a reload's spec (watchdog off, new
	// intervals) applies promptly even while the instance is healthy
	// or the loop is parked. The loop treats a poke with an unchanged
	// instance as a re-read, not a new instance — no grace re-arm.
	ma.pokeWatchdog()
}

// specEqual is a shallow inequality check good enough for the "config
// changed" notice; false negatives only cost a log line.
func specEqual(a, b *appSpec) bool {
	return fmt.Sprintf("%+v", a) == fmt.Sprintf("%+v", b)
}

// currentVerifiers snapshots the app's deploy-auth trust sources.
func (ma *managedApp) currentVerifiers() []verifier {
	ma.specMu.RLock()
	defer ma.specMu.RUnlock()
	return ma.verifiers
}

// snapshot returns a consistent view of the spec and collaborators for
// one deploy/recovery run.
type collaborators struct {
	spec   *appSpec
	runner runner
	prober prober
	fetch  fetcher
	clock  clock
	store  stateStore
	logger *zap.Logger
}

func (ma *managedApp) snapshot() collaborators {
	ma.specMu.RLock()
	defer ma.specMu.RUnlock()
	return collaborators{
		spec:   ma.spec,
		runner: ma.runner,
		prober: ma.prober,
		fetch:  ma.fetch,
		clock:  ma.clock,
		store:  ma.store,
		logger: ma.logger,
	}
}

func (ma *managedApp) setPhase(c collaborators, phase string) {
	ma.mu.Lock()
	ma.phase = phase
	ma.mu.Unlock()
	c.logger.Info("deploy phase", zap.String("phase", phase))
}

// Deploy runs the full blue/green pipeline. Any failure before the
// promote step leaves the old version serving untouched — that is the
// rollback story. There is deliberately no post-promote auto-revert:
// rolling back is re-POSTing the previous version.
func (ma *managedApp) Deploy(ctx context.Context, req deployRequest) error {
	if !ma.deployMu.TryLock() {
		return errDeployInProgress
	}
	defer ma.deployMu.Unlock()
	return ma.deployLocked(ctx, req, ma.snapshot())
}

// deployLocked runs the blue/green pipeline. The caller MUST already
// hold deployMu: URL/rollback go through Deploy, while the push handler
// acquires the lock BEFORE staging the upload, so a concurrent push gets
// an immediate 409 rather than streaming a whole tarball to disk only to
// lose the lock (which would also make aggregate staging unbounded).
//
// It runs against the single collaborators snapshot `c` the caller took,
// so a config reload mid-flight can never split one deploy across two app
// definitions (e.g. staging a push under the old size cap/root and
// extracting it under the new).
func (ma *managedApp) deployLocked(ctx context.Context, req deployRequest, c collaborators) (err error) {
	spec := c.spec
	// App.Start's recovery goroutine may lose deployMu to an early
	// webhook; restore the durable leaked set here too, so this deploy
	// can never persist an empty set over it or GC a protected release.
	// An unreadable record is a hard stop: proceeding would overwrite
	// it at promotion and GC blind. Prune before the fetch can recreate
	// a release dir the operator just removed to clear its marker.
	if err := ma.restoreLeaked(c); err != nil {
		return fmt.Errorf("cannot read app state: %w", err)
	}
	ma.pruneLeaked(c)
	started := c.clock.Now()
	logger := c.logger.With(zap.String("version", req.Version))
	logger.Info("deploy started", zap.String("artifact_host", hostOf(req.URL)))

	defer func() {
		result := deployResult{
			Version:    req.Version,
			Status:     "succeeded",
			By:         req.by,
			StartedAt:  started,
			FinishedAt: c.clock.Now(),
		}
		if err != nil {
			result.Status = "failed"
			result.Error = err.Error()
			ma.mu.Lock()
			result.Phase = ma.phase
			ma.mu.Unlock()
			logger.Error("deploy failed", zap.Error(err))
		} else {
			logger.Info("deploy succeeded")
		}
		ma.mu.Lock()
		ma.lastDeploy = &result
		ma.phase = "idle"
		ma.mu.Unlock()
	}()

	old := ma.currentInstance()
	if old != nil && old.version == req.Version && c.runner.Alive(old.handle) {
		return validationError{fmt.Sprintf("version %s is already running; bump the version to redeploy", req.Version)}
	}
	if err := spec.dirs.ensure(); err != nil {
		return err
	}
	// Versions are immutable: a deploy (URL or push) never overwrites an
	// existing on-disk release, so a version you can roll back to can't be
	// silently replaced with different content. Rollback is exempt — it
	// relaunches an existing release by design. Deploys are serialized per
	// app (deployMu), so this check-then-create has no race.
	if !req.rollback {
		switch _, statErr := os.Stat(spec.dirs.release(req.Version)); {
		case statErr == nil:
			return validationError{fmt.Sprintf("version %s already exists — versions are immutable; deploy a new version, or roll back to relaunch this one", req.Version)}
		case !os.IsNotExist(statErr):
			// A real I/O/permission error must not be read as "absent"
			// and fall through to fetch, whose RemoveAll would then
			// overwrite the existing release.
			return fmt.Errorf("checking release %s: %w", req.Version, statErr)
		}
	}

	releaseDir, err := c.fetch.fetch(ctx, spec, req, func(phase string) { ma.setPhase(c, phase) })
	if err != nil {
		return err
	}
	// If this freshly-extracted release never promotes (pre_start, start
	// or health-gate failure), remove it: a failed attempt must not
	// permanently reserve its immutable version or leave disk litter, so
	// the same version stays retriable. Rollback is excluded — it
	// relaunches a pre-existing release it must never delete.
	// newHandle is predeclared so the cleanup defer can confirm the failed
	// instance is really gone before deleting its release.
	var newHandle handle
	promoted := false
	stopUnconfirmed := false // Stop could not vouch that the whole process group is gone
	if !req.rollback {
		defer func() {
			if err == nil || promoted {
				return
			}
			// Never delete a release out from under a failed instance that
			// may still be running — Alive covers the leader, and a Stop
			// error covers workers that outlived it — that would pull
			// files from beneath a live, leaked process.
			if newHandle != nil && (stopUnconfirmed || c.runner.Alive(newHandle)) {
				ma.markLeaked(c, req.Version)
				err = errors.Join(err, fmt.Errorf("release %s left on disk: the failed instance may still be running", req.Version))
				return
			}
			// Surface a cleanup failure: otherwise the release lingers and
			// the next attempt at this version 422s (immutable) with no
			// explanation of why.
			if rmErr := os.RemoveAll(releaseDir); rmErr != nil {
				err = errors.Join(err, fmt.Errorf("cleanup of failed release %s: %w", req.Version, rmErr))
			}
		}()
	}

	port, err := freePort()
	if err != nil {
		return err
	}
	env, err := buildEnv(spec, req.Version, port, releaseDir)
	if err != nil {
		return err
	}

	// pre_start is deploy-time preparation; its outputs (migrations,
	// generated config, warmed caches) persist, so a rollback — which
	// relaunches an already-prepared on-disk release, like crash
	// recovery — must NOT re-run it. Re-running an old forward migration
	// would be wrong, and a flaky preflight check must never block an
	// emergency rollback.
	if len(spec.preStart) > 0 && !req.rollback {
		ma.setPhase(c, "preparing")
		preCtx, cancel := context.WithTimeout(ctx, spec.deadline)
		err := c.runner.RunOnce(preCtx, startSpec{
			command: expandArgs(spec.preStart, spec, req.Version, port, releaseDir),
			dir:     releaseDir,
			env:     env,
		})
		cancel()
		if err != nil {
			return fmt.Errorf("pre_start failed: %w", err)
		}
	}

	ma.setPhase(c, "starting")
	newHandle, err = c.runner.Start(startSpec{
		command: expandArgs(spec.command, spec, req.Version, port, releaseDir),
		dir:     releaseDir,
		env:     env,
		grace:   spec.grace,
	})
	if err != nil {
		return fmt.Errorf("start failed: %w", err)
	}

	ma.setPhase(c, "soaking")
	baseURL := "http://127.0.0.1:" + portString(port)
	alive := func() bool { return c.runner.Alive(newHandle) }
	if err := c.prober.waitHealthy(ctx, baseURL, alive, healthConfig{
		path:     spec.healthPath,
		interval: spec.healthInterval,
		timeout:  spec.healthTimeout,
		soak:     spec.soak,
		deadline: spec.deadline,
	}); err != nil {
		deployErr := fmt.Errorf("health gate: %w", err)
		// If Stop can't confirm the instance is gone — its signal failed,
		// or workers survived the sweep of its process group even though
		// the leader is dead — surface that, and the cleanup defer leaves
		// the release in place rather than delete it beneath them.
		if stopErr := c.runner.Stop(newHandle, spec.grace); stopErr != nil {
			stopUnconfirmed = true
			deployErr = errors.Join(deployErr, fmt.Errorf("failed instance could not be stopped: %w", stopErr))
		}
		return deployErr
	}

	// The point of no return. From here on the request context is
	// ignored: a CI client hanging up must not abort stop-old or GC.
	ma.setPhase(c, "promoting")
	newInst := &instance{version: req.Version, port: port, handle: newHandle}
	if err := ma.publishInstance(c, newInst); err != nil { // ← the cutover
		logger.Error("state persistence failed; deploys still work but a Caddy restart will not know about this version", zap.Error(err))
	}
	promoted = true // past the cutover: never clean up the now-live release

	// A successful deploy is the fast path out of any watchdog wait:
	// it clears the failure count, restart budget and backoff, then
	// wakes the loop to adopt the new instance.
	ma.wd.reset()
	ma.pokeWatchdog()

	if old != nil {
		if c.runner.Alive(old.handle) {
			ma.setPhase(c, "draining")
			c.clock.Sleep(spec.drain)
		}
		// Stop even a dead leader: its workers may still be draining
		// under the runner's crash sweep, and Stop blocks until that
		// sweep is done — so the release GC below never deletes a
		// release dir from under processes still running out of it.
		ma.setPhase(c, "stopping_old")
		if err := c.runner.Stop(old.handle, spec.grace); err != nil {
			// Some of the old instance may still be running out of its
			// release dir. Protect that release from this and every
			// later GC — a later deploy's GC has no other way to know.
			ma.markLeaked(c, old.version)
			logger.Warn("stopping old version failed; its release is protected from GC",
				zap.String("version", old.version), zap.Error(err))
		}
	}

	gcReleases(spec.dirs.releases, spec.keep, logger, append(ma.leakedReleases(), req.Version)...)
	ma.pruneLeaked(c)
	return nil
}

// markLeaked records that version's processes could not be confirmed
// stopped, so its release dir must never be deleted by GC — and
// persists that immediately, since the stray processes outlive us.
func (ma *managedApp) markLeaked(c collaborators, version string) {
	ma.mu.Lock()
	ma.leaked[version] = struct{}{}
	ma.mu.Unlock()
	ma.persistLeaked(c)
}

// persistLeaked writes the leaked set into state.json without
// disturbing the rest of the record (there may be none yet: a first
// deploy can fail and leak before anything was ever published).
func (ma *managedApp) persistLeaked(c collaborators) {
	st, _, err := c.store.load()
	if err != nil {
		c.logger.Warn("persisting leaked releases: cannot read state", zap.Error(err))
		return
	}
	st.LeakedReleases = ma.leakedReleases()
	if err := c.store.save(st); err != nil {
		c.logger.Warn("persisting leaked releases", zap.Error(err))
	}
}

// restoreLeaked merges the persisted leaked set into memory. Called
// before anything can run GC; a union, so nothing recorded in this
// process is ever dropped by an older file. A load error is returned,
// not swallowed: a caller that cannot read the record must not go on
// to overwrite it or GC without it.
func (ma *managedApp) restoreLeaked(c collaborators) error {
	st, ok, err := c.store.load()
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	ma.mu.Lock()
	defer ma.mu.Unlock()
	for _, v := range st.LeakedReleases {
		ma.leaked[v] = struct{}{}
	}
	return nil
}

// pruneLeaked drops entries whose release dir no longer exists: the
// defined way to clear one is to kill the stray processes and remove
// the release dir by hand. Runs right after restore — before a deploy
// could recreate that very dir and make a stale marker permanent —
// and again after GC (which never touches them).
func (ma *managedApp) pruneLeaked(c collaborators) {
	changed := false
	ma.mu.Lock()
	for v := range ma.leaked {
		if _, err := os.Stat(c.spec.dirs.release(v)); errors.Is(err, fs.ErrNotExist) {
			delete(ma.leaked, v)
			changed = true
		}
	}
	ma.mu.Unlock()
	if changed {
		ma.persistLeaked(c)
	}
}

// leakedReleases returns the protected versions, sorted, for GC and status.
func (ma *managedApp) leakedReleases() []string {
	ma.mu.Lock()
	defer ma.mu.Unlock()
	out := make([]string, 0, len(ma.leaked))
	for v := range ma.leaked {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// ensureRunning is crash/restart recovery, called from App.Start. If
// the instance already exists in memory (config reload — the pool kept
// it), this is a no-op. Otherwise it tries to reattach via the runner
// and falls back to relaunching the version recorded in state.json.
// The health gate is a deploy gate, not a boot gate: on a cold start
// there is nothing better to serve, so the port is published as soon
// as the process is up.
func (ma *managedApp) ensureRunning() error {
	if !ma.deployMu.TryLock() {
		return nil // a deploy is running; it owns the lifecycle
	}
	defer ma.deployMu.Unlock()

	c := ma.snapshot()
	spec := c.spec
	if err := ma.restoreLeaked(c); err != nil { // before any path that could lead to a GC
		return fmt.Errorf("cannot read app state: %w", err)
	}
	ma.pruneLeaked(c)
	if inst := ma.currentInstance(); inst != nil {
		if c.runner.Alive(inst.handle) {
			return nil
		}
		// Dead in memory (a reload with the watchdog off): the handle
		// is about to be replaced, and it is the only thing that can
		// replay its crash sweep's verdict. Collect it first.
		if err := c.runner.Stop(inst.handle, spec.grace); err != nil {
			ma.markLeaked(c, inst.version)
			c.logger.Warn("recovery: dead instance could not be confirmed stopped; its release is protected from GC",
				zap.String("version", inst.version), zap.Error(err))
		}
	}
	st, ok, err := c.store.load()
	if err != nil {
		return err
	}
	if !ok || st.CurrentVersion == "" {
		return nil // nothing was ever deployed
	}
	releaseDir := spec.dirs.release(st.CurrentVersion)
	if _, err := os.Stat(releaseDir); err != nil {
		return fmt.Errorf("state names version %s but its release dir is missing: %w", st.CurrentVersion, err)
	}

	if h, ok := c.runner.Reattach(st.Handle); ok {
		inst := &instance{version: st.CurrentVersion, port: st.Port, handle: h}
		ma.mu.Lock()
		ma.current = inst
		ma.mu.Unlock()
		ma.activePort.Store(int64(st.Port))
		c.logger.Info("reattached to running instance", zap.String("version", st.CurrentVersion))
		ma.pokeWatchdog()
		return nil
	}

	inst, err := ma.launchVersion(c, st.CurrentVersion)
	if err != nil {
		return fmt.Errorf("relaunching %s: %w", st.CurrentVersion, err)
	}
	if err := ma.publishInstance(c, inst); err != nil {
		c.logger.Warn("persisting recovered state", zap.Error(err))
	}
	c.logger.Info("relaunched current version after restart",
		zap.String("version", st.CurrentVersion), zap.Int("port", inst.port))
	ma.pokeWatchdog()
	return nil
}

// launchVersion starts an already-on-disk version on a fresh port and
// returns the new instance without publishing it. It relaunches what
// was recorded as running — the version comes from the caller's record
// (state.json, or the live instance the watchdog is replacing), never
// from a re-read of config-level launch policy — so recovery and
// watchdog restarts reproduce the instance that existed, and a changed
// app definition still takes effect only on the next deploy.
func (ma *managedApp) launchVersion(c collaborators, version string) (*instance, error) {
	spec := c.spec
	releaseDir := spec.dirs.release(version)
	if _, err := os.Stat(releaseDir); err != nil {
		return nil, fmt.Errorf("release dir for version %s is missing: %w", version, err)
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	env, err := buildEnv(spec, version, port, releaseDir)
	if err != nil {
		return nil, err
	}
	h, err := c.runner.Start(startSpec{
		command: expandArgs(spec.command, spec, version, port, releaseDir),
		dir:     releaseDir,
		env:     env,
		grace:   spec.grace,
	})
	if err != nil {
		return nil, err
	}
	return &instance{version: version, port: port, handle: h}, nil
}

// publishInstance installs inst as current, cuts traffic over to it
// and persists it for the next restart. It is the ONLY place current
// and activePort are installed — the watchdog's stale-instance guards
// depend on the swap-before-route ordering here, so promote, recovery
// and watchdog restarts must all go through it. A persist failure is
// returned, not logged: each caller has its own severity and message.
func (ma *managedApp) publishInstance(c collaborators, inst *instance) error {
	ma.mu.Lock()
	ma.current = inst
	ma.mu.Unlock()
	ma.activePort.Store(int64(inst.port))
	return ma.persistState(c, inst)
}

// unrouteIf clears activePort only while inst is still current, under
// the same lock that guards the current swap. Check-then-store without
// the lock races promote: a deploy could install and route a healthy
// instance between the check and the store, and the store would then
// unroute it permanently.
func (ma *managedApp) unrouteIf(inst *instance) {
	ma.mu.Lock()
	defer ma.mu.Unlock()
	if ma.current == inst {
		ma.activePort.Store(0)
	}
}

func (ma *managedApp) persistState(c collaborators, inst *instance) error {
	if err := c.store.save(appState{
		CurrentVersion: inst.version,
		Port:           inst.port,
		Handle:         inst.handle.state(),
		UpdatedAt:      c.clock.Now(),
		LeakedReleases: ma.leakedReleases(),
	}); err != nil {
		return err
	}
	// The `current` symlink is for humans poking around the server;
	// state.json remains the source of truth.
	tmp := c.spec.dirs.current + ".tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(c.spec.dirs.release(inst.version), tmp); err == nil {
		_ = os.Rename(tmp, c.spec.dirs.current)
	}
	return nil
}

func (ma *managedApp) currentInstance() *instance {
	ma.mu.Lock()
	defer ma.mu.Unlock()
	return ma.current
}

// statusSnapshot backs the webhook's GET endpoint.
type statusSnapshot struct {
	App            string            `json:"app"`
	Phase          string            `json:"phase"`
	CurrentVersion string            `json:"current_version,omitempty"`
	Port           int               `json:"port,omitempty"`
	PID            int               `json:"pid,omitempty"`
	Running        bool              `json:"running"`
	LastDeploy     *deployResult     `json:"last_deploy,omitempty"`
	Watchdog       *watchdogSnapshot `json:"watchdog,omitempty"`
	// AvailableVersions lists the on-disk releases, newest-first — the
	// versions `?rollback=<version>` can relaunch. Always serialized (a
	// healthy app with no releases reports []), so an empty set is
	// distinguishable from a server that doesn't report the field.
	AvailableVersions []string `json:"available_versions"`
	// LeakedReleases lists releases whose processes could not be
	// confirmed stopped. They are exempt from GC — across restarts too,
	// via state.json — until the operator kills the strays and removes
	// the release dir, at which point the entry clears itself.
	LeakedReleases []string `json:"leaked_releases,omitempty"`
}

func (ma *managedApp) status() statusSnapshot {
	c := ma.snapshot()
	var wd *watchdogSnapshot
	available := []string{} // always an array in the JSON, never null
	if c.spec != nil {
		if c.clock != nil {
			wd = ma.wd.statusSnapshot(c.clock.Now(), c.spec.wdWindow)
		}
		// Read releases outside ma.mu — it is disk I/O, and status is polled.
		if rels := listReleases(c.spec.dirs.releases); rels != nil {
			available = rels
		}
	}
	ma.mu.Lock()
	defer ma.mu.Unlock()
	s := statusSnapshot{App: ma.name, Phase: ma.phase, LastDeploy: ma.lastDeploy, Watchdog: wd, AvailableVersions: available}
	for v := range ma.leaked {
		s.LeakedReleases = append(s.LeakedReleases, v)
	}
	sort.Strings(s.LeakedReleases)
	if ma.current != nil {
		s.CurrentVersion = ma.current.version
		s.Port = ma.current.port
		s.PID = ma.current.handle.state().PID
		s.Running = c.runner.Alive(ma.current.handle)
	}
	return s
}

// Destruct is called by the UsagePool when the last config referencing
// this app is unloaded — i.e. real shutdown or the app being removed
// from the Caddyfile, never a plain reload. The watchdog is stopped
// FIRST and waited for: only then is it impossible for a restart in
// flight to spawn a fresh process after the one below is stopped.
func (ma *managedApp) Destruct() error {
	ma.stopWatchdog()
	c := ma.snapshot()
	inst := ma.currentInstance()
	if inst == nil {
		return nil
	}
	c.logger.Info("shutting down app", zap.String("version", inst.version))
	err := c.runner.Stop(inst.handle, c.spec.grace)
	if err != nil {
		// Whatever survived outlives this Caddy; the next one must
		// still know not to GC its release.
		ma.markLeaked(c, inst.version)
	}
	return err
}

// startWatchdog launches the supervision goroutine once per pooled
// app; reloads find wdStarted already set and leave it alone. The loop
// re-snapshots the spec every cycle, so config changes (including
// `watchdog off`) apply on its next iteration without a restart.
func (ma *managedApp) startWatchdog() {
	ma.specMu.Lock()
	defer ma.specMu.Unlock()
	if ma.wdStarted {
		return
	}
	ma.wdStarted = true
	ctx, cancel := context.WithCancel(context.Background())
	ma.wdCancel = cancel
	ma.wdWG.Add(1)
	go func() {
		defer ma.wdWG.Done()
		ma.watchdogLoop(ctx)
	}()
}

// stopWatchdog cancels the supervision goroutine and blocks until it
// has fully exited. Safe to call when the watchdog never started.
func (ma *managedApp) stopWatchdog() {
	ma.specMu.Lock()
	cancel := ma.wdCancel
	ma.specMu.Unlock()
	if cancel != nil {
		cancel()
	}
	ma.wdWG.Wait()
}

// pokeWatchdog wakes the supervision loop after current changed
// (promote, recovery). Non-blocking: the channel is buffered and a
// pending poke already means "re-examine the world".
func (ma *managedApp) pokeWatchdog() {
	select {
	case ma.wdNotify <- struct{}{}:
	default:
	}
}

// envAllowlist is the only part of Caddy's own environment that apps
// inherit. Everything else is withheld: the supervisor's env holds
// secrets (ACME DNS tokens, and whatever the operator sets), and
// handing those to every app defeats the isolation story — env-dumping
// supply-chain payloads read process.env before they read files.
// Operators pass anything extra explicitly via env_file or env.
var envAllowlist = []string{"PATH", "HOME", "LANG", "TZ"}

func inheritedEnv() []string {
	var env []string
	for _, key := range envAllowlist {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "LC_") {
			env = append(env, kv)
		}
	}
	return env
}

// buildEnv assembles the child environment. Precedence, lowest to
// highest: the allowlisted slice of Caddy's environment (PATH, HOME —
// needed by node etc.), env_file, inline env, then the injected
// PORT/HOST contract.
func buildEnv(spec *appSpec, version string, port int, releaseDir string) ([]string, error) {
	env := inheritedEnv()
	if spec.envFile != "" {
		fileVars, err := parseEnvFile(spec.envFile)
		if err != nil {
			return nil, fmt.Errorf("env_file: %w", err)
		}
		env = append(env, fileVars...)
	}
	for k, v := range spec.env {
		env = append(env, k+"="+expandPlaceholders(v, spec, version, port, releaseDir))
	}
	env = append(env,
		"PORT="+portString(port),
		"HOST=127.0.0.1",
	)
	return env, nil
}

// parseEnvFile reads simple KEY=VALUE lines: blank lines and #comments
// skipped, an optional `export ` prefix tolerated, and single or
// double quotes around the value stripped. Deliberately not a shell.
func parseEnvFile(path string) ([]string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is the operator's env_file config value, not request input
	if err != nil {
		return nil, err
	}
	var vars []string
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, found := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !found || key == "" {
			return nil, fmt.Errorf("%s:%d: not KEY=VALUE", path, i+1)
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		vars = append(vars, key+"="+value)
	}
	return vars, nil
}

// expandPlaceholders substitutes the deploy-time placeholders that are
// only knowable mid-deploy. These intentionally use the same brace
// style as Caddy placeholders but are resolved here, not by a
// caddy.Replacer — Provision's ReplaceKnown leaves them alone.
func expandPlaceholders(s string, spec *appSpec, version string, port int, releaseDir string) string {
	return strings.NewReplacer(
		"{version}", version,
		"{port}", portString(port),
		"{release_dir}", releaseDir,
		"{shared_dir}", spec.dirs.shared,
	).Replace(s)
}

func expandArgs(args []string, spec *appSpec, version string, port int, releaseDir string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = expandPlaceholders(a, spec, version, port, releaseDir)
	}
	return out
}

// hostOf returns the host[:port] of a raw URL for logging. It parses
// with net/url so any userinfo credentials (user:pass@host, e.g. a
// tokenised artifact URL from CI) are excluded — url.URL keeps those in
// .User, never in .Host — rather than slicing the raw string, which
// would log the secret. Empty string if the URL does not parse.
func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Host
}

// fetchClients bundles the shared HTTP clients handed to each
// managedApp at configure time.
type fetchClients struct {
	download *http.Client
	health   *http.Client
	jwks     *http.Client // fetches OIDC issuers' public keys for deploy auth
}
