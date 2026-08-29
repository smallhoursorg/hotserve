package liveswap

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2"
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

	// mu guards current, phase and lastDeploy.
	mu         sync.Mutex
	current    *instance
	phase      string
	lastDeploy *deployResult

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
	return &managedApp{name: name, phase: "idle", wdNotify: make(chan struct{}, 1)}
}

// configure installs the latest spec and (re)wires collaborators. On
// the first provision the runner/prober/etc. are created; on reloads
// the spec, logger and state path are refreshed while the runner — and
// with it any running unit — is left untouched: a changed
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
		ma.runner = newSystemdRunner(userManager, logger)
		ma.prober = &httpProber{client: clients.health, clock: realClock{}}
		ma.fetch = &releaseFetcher{client: clients.download}
		ma.clock = realClock{}
	} else if sr, ok := ma.runner.(*systemdRunner); ok {
		sr.setLogger(logger)
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
	stopUnconfirmed := false // Stop of the failed instance returned an error
	if !req.rollback {
		defer func() {
			if err == nil || promoted {
				return
			}
			// Never delete a release out from under an instance that may
			// still be running (a Stop that errored, a start or pre_start
			// the runner could not reconcile, a handle still alive) —
			// that would pull files from beneath a live process. The
			// next deploy's sweep settles it against the runner.
			if unitUnconfirmed(err) || (newHandle != nil && (stopUnconfirmed || c.runner.Alive(newHandle))) {
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
			app:     spec.name,
			version: req.Version,
			command: expandArgs(spec.preStart, spec, req.Version, port, releaseDir),
			dir:     releaseDir,
			env:     env,
			grace:   spec.grace,
		})
		cancel()
		if err != nil {
			return fmt.Errorf("pre_start failed: %w", err)
		}
	}

	ma.setPhase(c, "starting")
	// No Start without a confirmed sweep (keeping the version that is
	// serving): the new instance must not come up beside a unit the
	// manager still holds for this app.
	if !ma.sweep(c, oldHandle(old)) {
		return errors.New("cannot confirm no other instance is running; not starting")
	}
	newHandle, err = c.runner.Start(startSpec{
		app:     spec.name,
		version: req.Version,
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
		// If Stop can't confirm the instance is gone, surface that — and
		// the cleanup defer then leaves the release in place rather than
		// delete it beneath a possibly live process.
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

	if old != nil && c.runner.Alive(old.handle) {
		ma.setPhase(c, "draining")
		c.clock.Sleep(spec.drain)
		ma.setPhase(c, "stopping_old")
		if err := c.runner.Stop(old.handle, spec.grace); err != nil {
			logger.Warn("stopping old version failed; the sweep below retries it", zap.String("version", old.version), zap.Error(err))
		}
	}

	// Nothing is deleted while anything but the new instance may be
	// running out of a release dir: the sweep settles that against
	// the runner's own ledger (an unconfirmed stop above, a unit an
	// earlier hotserve left behind), and any doubt skips GC — the next
	// successful deploy catches up.
	if !ma.sweep(c, newHandle) {
		logger.Warn("skipping release GC for this deploy")
		return nil
	}
	gcReleases(spec.dirs.releases, spec.keep, req.Version, logger)
	return nil
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
	if inst := ma.currentInstance(); inst != nil && c.runner.Alive(inst.handle) {
		return nil
	}
	st, ok, err := c.store.load()
	if err != nil {
		return err
	}
	if !ok || st.CurrentVersion == "" {
		// Nothing recorded — but a deploy whose state write failed may
		// have left a unit behind; the manager's ledger decides, not
		// the absence of a file.
		ma.sweep(c, nil)
		return nil
	}
	releaseDir := spec.dirs.release(st.CurrentVersion)
	if _, err := os.Stat(releaseDir); err != nil {
		return fmt.Errorf("state names version %s but its release dir is missing: %w", st.CurrentVersion, err)
	}

	// An unreadable unit state is not "not running": launching beside
	// a unit that may still be up would duplicate the app. Retry a
	// few times (Provision just proved the manager reachable, so this
	// is a blip) and otherwise give up loudly rather than guess.
	var (
		h        handle
		attached bool
	)
	for attempt := 1; ; attempt++ {
		h, attached, err = c.runner.Reattach(st.Handle)
		if err == nil {
			break
		}
		if attempt >= reattachAttempts {
			return fmt.Errorf("cannot tell whether %s is still running; not relaunching: %w", st.CurrentVersion, err)
		}
		c.logger.Warn("reattach: unit state unreadable; retrying", zap.Int("attempt", attempt), zap.Error(err))
		c.clock.Sleep(reattachRetryDelay)
	}
	if attached {
		inst := &instance{version: st.CurrentVersion, port: st.Port, handle: h}
		ma.mu.Lock()
		ma.current = inst
		ma.mu.Unlock()
		ma.activePort.Store(int64(st.Port))
		c.logger.Info("reattached to running instance", zap.String("version", st.CurrentVersion))
		ma.pokeWatchdog()
		ma.sweep(c, h)
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

// oldHandle is the handle of a possibly-nil instance, as a nil-safe
// Sweep keep argument.
func oldHandle(inst *instance) handle {
	if inst == nil {
		return nil
	}
	return inst.handle
}

const (
	reattachAttempts   = 5
	reattachRetryDelay = 2 * time.Second
)

// sweep stops every unit of this app other than keep — the runner's
// ledger, not ours, decides what is running. Recovery calls it so a
// unit hotserve lost track of (a state.json write that failed, an
// earlier stop that could not be confirmed) does not outlive the next
// start; deploys call it before GC. Returns false if something may
// still be running, in which case callers must not delete anything.
func (ma *managedApp) sweep(c collaborators, keep handle) bool {
	if err := c.runner.Sweep(ma.name, keep); err != nil {
		c.logger.Error("sweeping stray instances", zap.Error(err))
		return false
	}
	return true
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
	// No Start without a confirmed sweep: whatever the manager still
	// runs for this app (a unit an earlier hotserve lost track of, a
	// stop that could not be confirmed) is settled first, or nothing
	// is launched beside it.
	if !ma.sweep(c, nil) {
		return nil, errors.New("cannot confirm no other instance is running; not launching")
	}
	h, err := c.runner.Start(startSpec{
		app:     spec.name,
		version: version,
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
	App            string `json:"app"`
	Phase          string `json:"phase"`
	CurrentVersion string `json:"current_version,omitempty"`
	Port           int    `json:"port,omitempty"`
	PID            int    `json:"pid,omitempty"`
	// Unit is the systemd unit running the instance — what to pass to
	// journalctl for the app's own output.
	Unit       string            `json:"unit,omitempty"`
	Running    bool              `json:"running"`
	LastDeploy *deployResult     `json:"last_deploy,omitempty"`
	Watchdog   *watchdogSnapshot `json:"watchdog,omitempty"`
	// AvailableVersions lists the on-disk releases, newest-first — the
	// versions `?rollback=<version>` can relaunch. Always serialized (a
	// healthy app with no releases reports []), so an empty set is
	// distinguishable from a server that doesn't report the field.
	AvailableVersions []string `json:"available_versions"`
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
	if ma.current != nil {
		s.CurrentVersion = ma.current.version
		s.Port = ma.current.port
		hs := ma.current.handle.state()
		s.PID = hs.PID
		s.Unit = hs.Unit
		s.Running = c.runner.Alive(ma.current.handle)
	}
	return s
}

// caddyExiting reports whether the whole process is shutting down (as
// opposed to a config unloading an app); a variable so tests can flip it.
var caddyExiting = caddy.Exiting

// Destruct is called by the UsagePool when the last config referencing
// this app is unloaded — i.e. real shutdown or the app being removed
// from the Caddyfile, never a plain reload. The watchdog is stopped
// FIRST and waited for: only then is it impossible for a restart in
// flight to spawn a fresh process after the one below is stopped.
//
// On process exit the instance is deliberately left running: it is a
// systemd unit that does not depend on hotserve, state.json names it,
// and the next hotserve start reattaches to it — that is how apps
// survive hotserve restarts and upgrades. Only removing the app from
// the config stops it.
func (ma *managedApp) Destruct() error {
	ma.stopWatchdog()
	c := ma.snapshot()
	inst := ma.currentInstance()
	if caddyExiting() {
		if inst != nil {
			c.logger.Info("hotserve exiting; app keeps running for reattach", zap.String("version", inst.version), zap.String("unit", inst.handle.state().Unit))
		}
		return nil
	}
	// Stop what we track (if anything), then everything else the
	// manager holds for this app: a removed app must leave no unit
	// behind, tracked or not — an ambiguous start may have left one
	// that never became current.
	var stopErr error
	if inst != nil {
		c.logger.Info("app removed from config; stopping it", zap.String("version", inst.version))
		stopErr = c.runner.Stop(inst.handle, c.spec.grace)
	}
	return errors.Join(stopErr, c.runner.Sweep(ma.name, nil))
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
