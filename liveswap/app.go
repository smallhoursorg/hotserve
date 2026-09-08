package liveswap

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
//	<root>/<app>/run/<nonce>/app.sock  one socket per instance, bound by the app; only run/<nonce>/ is in that instance's view
//	<root>/<app>/proxy/<nonce>.sock    the same inode, hard-linked by hotserve; what it dials (not in any view)
//	<root>/<app>/state.json            current version + process handle
//	<root>/<app>/current -> releases/<version>   convenience symlink
type appDirs struct {
	root     string // the liveswap root every app lives under
	app      string
	releases string
	shared   string
	tmp      string
	run      string
	proxy    string
	state    string
	current  string
}

func newAppDirs(root, name string) appDirs {
	app := filepath.Join(root, name)
	return appDirs{
		root:     root,
		app:      app,
		releases: filepath.Join(app, "releases"),
		shared:   filepath.Join(app, "shared"),
		tmp:      filepath.Join(app, "tmp"),
		run:      filepath.Join(app, "run"),
		proxy:    filepath.Join(app, "proxy"),
		state:    filepath.Join(app, "state.json"),
		current:  filepath.Join(app, "current"),
	}
}

// runDir is the one directory an instance may bind its socket in: its
// own, so that no other instance of the app — the one it replaces, or
// the one replacing it — can touch the name before hotserve pins it.
func (d appDirs) runDir(nonce string) string {
	return filepath.Join(d.run, nonce)
}

// socket is the path the instance identified by nonce listens on.
func (d appDirs) socket(nonce string) string {
	return filepath.Join(d.runDir(nonce), "app.sock")
}

// socketRef is how hotserve dials that instance: the socket, pinned
// under proxy/.
func (d appDirs) socketRef(nonce string) *socketRef {
	return newSocketRef(d.socket(nonce), filepath.Join(d.proxy, nonce+".sock"), d.runDir(nonce))
}

func (d appDirs) release(version string) string {
	// versionPathComponent: identity for valid tags, mechanical
	// traversal containment (and the analyzer-modeled sanitizer) for
	// anything else — validVersion remains the actual gate upstream.
	return filepath.Join(d.releases, versionPathComponent(version))
}

func (d appDirs) ensure() error {
	for _, dir := range []string{d.releases, d.shared, d.tmp, d.run, d.proxy} {
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
	name               string
	command            []string
	preStart           []string
	env                map[string]string
	envFile            string
	trust              []trustSource
	healthPath         string // "" = no HTTP check (health_path off)
	healthInterval     time.Duration
	healthTimeout      time.Duration
	soak               time.Duration
	deadline           time.Duration
	drain              time.Duration
	grace              time.Duration
	watchdogOn         bool
	wdFailures         int
	wdGrace            time.Duration
	wdRestarts         int
	wdWindow           time.Duration
	keep               int
	maxArtifactSize    int64
	maxArtifactEntries int
	allowInsecure      bool
	allowlist          []artifactAllowEntry
	dirs               appDirs
}

// archiveLimits is what one artifact of this app may cost to extract.
func (s *appSpec) archiveLimits() archiveLimits {
	return archiveLimits{
		maxBytes:   s.maxArtifactSize * decompressionRatioCap,
		maxEntries: s.maxArtifactEntries,
	}
}

// capWarnFraction is how close to a cap an artifact may come before a
// deploy warns: a cap an app grows into with no notice is an outage on
// the deploy that crosses it, so the operator hears several deploys
// early and raises the directive on their own schedule.
const capWarnFraction = 0.75

// warnNearCaps logs what the artifact cost and, past capWarnFraction
// of either cap, a Warn naming the directive to raise.
func (s *appSpec) warnNearCaps(logger *zap.Logger, st archiveStats) {
	lim := s.archiveLimits()
	logger.Info("artifact extracted",
		zap.Int("entries", st.entries), zap.Int("max_entries", lim.maxEntries),
		zap.Int64("bytes", st.bytes), zap.Int64("max_bytes", lim.maxBytes))
	if float64(st.entries) >= capWarnFraction*float64(lim.maxEntries) {
		logger.Warn("artifact approaching max_artifact_entries; raise it before a deploy fails on it",
			zap.Int("entries", st.entries), zap.Int("max_entries", lim.maxEntries))
	}
	if float64(st.bytes) >= capWarnFraction*float64(lim.maxBytes) {
		logger.Warn("artifact approaching the decompressed byte cap (max_artifact_size x 10); raise max_artifact_size before a deploy fails on it",
			zap.Int64("bytes", st.bytes), zap.Int64("max_bytes", lim.maxBytes))
	}
}

// deployPayload is the JSON body of a URL deploy — the only request
// body the webhook ever decodes into a struct. It is deliberately a
// separate type from deployRequest: the request carries server-side
// fields (the staged upload path, the rollback flag, the authorizing
// source) that must never be reachable from a body, and keeping them
// out of the decoded type makes that structural rather than a matter
// of field visibility — for readers and for static analysis alike,
// which otherwise taints every field of a decoded struct.
// TestDeployPayloadIsTheOnlyWireType pins the split.
type deployPayload struct {
	URL        string `json:"url"`
	Version    string `json:"version"`
	AuthHeader string `json:"auth_header"`
}

// request is the one place a payload becomes a request: exactly the
// three wire fields cross over, everything else stays zero.
func (p deployPayload) request() deployRequest {
	return deployRequest{url: p.URL, version: p.Version, authHeader: p.AuthHeader}
}

// deployRequest is the validated deploy request handed to the
// pipeline. It is never decoded from the wire (see deployPayload). The
// three sources are mutually exclusive: a URL to pull (the default), a
// pushed archive already staged on disk (localArchive), or a rollback
// to an existing on-disk release (rollback).
type deployRequest struct {
	url        string
	version    string
	authHeader string
	// localArchive is the path of an already-staged pushed tarball (an
	// os.CreateTemp name under the app's own tmp dir, chosen by the
	// handler — never request input); empty for a URL pull.
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
	// What the artifact cost against max_artifact_entries and the
	// decompressed byte cap — the files, directories and links
	// extraction created, and its bytes — so an app growing toward
	// either is visible from the status endpoint before a deploy fails
	// on it. Zero for a rollback (nothing is extracted) and for a
	// deploy that failed before extraction.
	ArtifactEntries int   `json:"artifact_entries"`
	ArtifactBytes   int64 `json:"artifact_bytes"`
}

// instance is one running version of an app.
type instance struct {
	version string
	nonce   string     // names the unit and the socket (newNonce)
	socket  string     // the unix socket the app listens on
	sock    *socketRef // how hotserve dials it: pinned by inode under proxy/, never by the app's name
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
// reloads: the running instance, the active socket the proxy reads, and
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

	// activeSocket is what GetUpstreams reads on every request; storing
	// it is the cutover. nil = nothing serving yet.
	activeSocket atomic.Pointer[socketRef]

	// Watchdog plumbing. The goroutine is pool-scoped like everything
	// else here: started once (first Provision), never touched by
	// reloads, torn down in Destruct BEFORE the child is stopped so a
	// mid-restart watchdog can never orphan a fresh process.
	wdStarted bool // under specMu
	wdCtx     context.Context
	wdCancel  context.CancelFunc

	// configuredBy is the config that installed the current spec, and
	// prev what it replaced — so a candidate config Caddy rejects
	// after our Start (another app's Start failed) can be rolled back
	// by its Cleanup, leaving the still-serving config's definition in
	// place. Under specMu.
	configuredBy any
	prev         *appConfigState
	wdWG         sync.WaitGroup
	wdNotify     chan struct{} // buffered(1); poked whenever current changes
	wd           watchdogState
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
// appConfigState is what a config installs on a pooled app and what
// a rollback restores.
type appConfigState struct {
	spec      *appSpec
	verifiers []verifier
	logger    *zap.Logger
	store     stateStore
}

// owner is the config installing this definition (see rollbackConfig).
// manager is the connection a runner built here talks to, passed in
// rather than reached for globally so a test can install its own; it
// is read only when this managedApp has no runner yet, because a
// pooled app keeps the runner — and so the connection — it was first
// started with across every later reload.
func (ma *managedApp) configure(owner any, spec *appSpec, logger *zap.Logger, clients *fetchClients, manager systemdConn) {
	ma.specMu.Lock()
	defer ma.specMu.Unlock()
	changed := ma.spec != nil && !specEqual(ma.spec, spec)
	if ma.spec != nil {
		ma.prev = &appConfigState{spec: ma.spec, verifiers: ma.verifiers, logger: ma.logger, store: ma.store}
	}
	ma.configuredBy = owner
	ma.spec = spec
	// Deploy auth is not tied to the running process, so — unlike the
	// runner — it is rewired on every reload and takes effect at once.
	ma.verifiers = resolveVerifiers(spec.trust, clients.jwks)
	ma.logger = logger
	if ma.runner == nil {
		ma.runner = newSystemdRunner(manager, logger)
		ma.prober = &httpProber{clock: realClock{}}
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
	logger := c.logger.With(zap.String("version", req.version))
	logger.Info("deploy started", zap.String("artifact_host", hostOf(req.url)))

	var stats archiveStats
	defer func() {
		result := deployResult{
			Version:         req.version,
			Status:          "succeeded",
			By:              req.by,
			StartedAt:       started,
			FinishedAt:      c.clock.Now(),
			ArtifactEntries: stats.entries,
			ArtifactBytes:   stats.bytes,
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
	if old != nil && old.version == req.version && c.runner.Alive(old.handle) {
		return validationError{fmt.Sprintf("version %s is already running; bump the version to redeploy", req.version)}
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
		switch _, statErr := os.Stat(spec.dirs.release(req.version)); {
		case statErr == nil:
			return validationError{fmt.Sprintf("version %s already exists — versions are immutable; deploy a new version, or roll back to relaunch this one", req.version)}
		case !os.IsNotExist(statErr):
			// A real I/O/permission error must not be read as "absent"
			// and fall through to fetch, whose RemoveAll would then
			// overwrite the existing release.
			return fmt.Errorf("checking release %s: %w", req.version, statErr)
		}
	}

	releaseDir, stats, err := c.fetch.fetch(ctx, spec, req, func(phase string) { ma.setPhase(c, phase) })
	if err != nil {
		return err
	}
	if !req.rollback { // a rollback extracted nothing; there is nothing to measure
		spec.warnNearCaps(logger, stats)
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
				err = errors.Join(err, fmt.Errorf("release %s left on disk: the failed instance may still be running", req.version))
				return
			}
			// Surface a cleanup failure: otherwise the release lingers and
			// the next attempt at this version 422s (immutable) with no
			// explanation of why.
			if rmErr := os.RemoveAll(releaseDir); rmErr != nil {
				err = errors.Join(err, fmt.Errorf("cleanup of failed release %s: %w", req.version, rmErr))
			}
		}()
	}

	l, err := spec.prepareLaunch(req.version)
	if err != nil {
		return err
	}
	// A launch that never becomes the instance leaves nothing behind:
	// its run dir, and any pin the health gate made, go with it. Once
	// promoted, l.sock is the instance's own and lives on. And like
	// the release above, it is preserved while a unit may still be
	// live — an ambiguous start or pre_start, a stop the runner could
	// not confirm — since removing the dir from under a starting
	// process would leave it running and unreachable; the next
	// confirmed sweep stops that unit and prunes it.
	defer func() {
		if promoted || unitUnconfirmed(err) || (newHandle != nil && (stopUnconfirmed || c.runner.Alive(newHandle))) {
			return
		}
		l.sock.retire()
	}()

	// pre_start is deploy-time preparation; its outputs (migrations,
	// generated config, warmed caches) persist, so a rollback — which
	// relaunches an already-prepared on-disk release, like crash
	// recovery — must NOT re-run it. Re-running an old forward migration
	// would be wrong, and a flaky preflight check must never block an
	// emergency rollback.
	if len(spec.preStart) > 0 && !req.rollback {
		// A previous deploy's pre_start whose outcome could not be
		// observed may still be running; settle the ledger before
		// starting another migration beside it.
		if !ma.sweep(c, old, l.nonce) {
			return fmt.Errorf("%w; not running pre_start", errSweepUnconfirmed)
		}
		ma.setPhase(c, "preparing")
		preCtx, cancel := context.WithTimeout(ctx, spec.deadline)
		// Under the same sandbox as the app it precedes: a migration
		// that writes where the app cannot read fails here, not at 3am.
		err := c.runner.RunOnce(preCtx, l.startSpec(spec, spec.preStart))
		cancel()
		if err != nil {
			return fmt.Errorf("pre_start failed: %w", err)
		}
	}

	ma.setPhase(c, "starting")
	// No Start without a confirmed sweep (keeping the version that is
	// serving): the new instance must not come up beside a unit the
	// manager still holds for this app.
	if !ma.sweep(c, old, l.nonce) {
		return fmt.Errorf("%w; not starting", errSweepUnconfirmed)
	}
	newHandle, err = c.runner.Start(l.startSpec(spec, spec.command))
	if err != nil {
		return fmt.Errorf("start failed: %w", err)
	}

	ma.setPhase(c, "soaking")
	alive := func() bool { return c.runner.Alive(newHandle) }
	if err := c.prober.waitHealthy(ctx, l.sock, alive, healthConfig{
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
	newInst := l.instance(newHandle)
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
	if old != nil {
		old.sock.retire()
	}

	// Nothing is deleted while anything but the new instance may be
	// running out of a release dir: the sweep settles that against
	// the runner's own ledger (an unconfirmed stop above, a unit an
	// earlier hotserve left behind), and any doubt skips GC — the next
	// successful deploy catches up.
	if !ma.sweep(c, newInst, "") {
		logger.Warn("skipping release GC for this deploy")
		return nil
	}
	gcReleases(spec.dirs.releases, spec.keep, req.version, logger)
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
		// A deploy owns the lifecycle right now; it may still fail
		// before publishing anything, so recovery must look again.
		return &transientRecoveryError{errors.New("a deploy is in progress")}
	}
	defer ma.deployMu.Unlock()
	// Recovery pins and adopts, or relaunches into, this app's socket
	// dirs; the start-time sweep of unconfigured apps prunes such dirs.
	// Holding the app's ownerLock across recovery is what keeps the two
	// from interleaving on an app a reload has just added back.
	owner := ownerLock(ma.name)
	owner.Lock()
	defer owner.Unlock()

	c := ma.snapshot()
	spec := c.spec
	if inst := ma.currentInstance(); inst != nil && c.runner.Alive(inst.handle) {
		// Reload, or a retry after an earlier sweep could not confirm:
		// the instance is fine; the ledger must still be settled.
		if !ma.sweep(c, inst, "") {
			return &transientRecoveryError{fmt.Errorf("instance %s running; %w", inst.version, errSweepUnconfirmed)}
		}
		return nil
	}
	st, ok, err := c.store.load()
	if err != nil {
		return &permanentRecoveryError{err} // corrupt state: never silently reset
	}
	if !ok || st.CurrentVersion == "" {
		// Nothing recorded — but a deploy whose state write failed may
		// have left a unit behind; the manager's ledger decides, not
		// the absence of a file.
		if !ma.sweep(c, nil, "") {
			return &transientRecoveryError{errSweepUnconfirmed}
		}
		return nil
	}
	releaseDir := spec.dirs.release(st.CurrentVersion)
	if _, err := os.Stat(releaseDir); err != nil {
		return &permanentRecoveryError{fmt.Errorf("state names version %s but its release dir is missing: %w", st.CurrentVersion, err)}
	}

	// state.json is ours, but it is a file: a recorded unit name that
	// is not one of this app's units is never handed to the manager
	// (it could be a sibling's). Treat it as "nothing recorded".
	if u := st.Handle.Unit; u != "" && !unitBelongsTo(u, ma.name) {
		c.logger.Error("state.json names a unit that is not this app's; ignoring it", zap.String("unit", u))
		st.Handle.Unit = ""
	}
	// The recorded nonce names the socket the proxy will dial, so the
	// record is adopted only when it is the unit's own nonce and the
	// socket it names can be pinned — present, and a socket rather
	// than something planted under the name. Anything else — a unit
	// that is not ours, a nonce that disagrees with it, a socket that
	// is gone or forged — is "nothing recorded": the relaunch's sweep
	// stops whatever the manager still holds, rather than routing to
	// a socket that answers nobody, or to somebody else's.
	// The nonce becomes a path component under run/ and proxy/, so a
	// recorded value that is not a nonce is never used for anything —
	// not to dial, not to remove — before it is even compared.
	if st.Handle.Unit != "" && !nonceRe.MatchString(st.Nonce) {
		c.logger.Error("state.json record ignored: its nonce is not one",
			zap.String("unit", st.Handle.Unit), zap.String("nonce", st.Nonce))
		st.Handle.Unit = ""
	}
	if err := spec.dirs.ensure(); err != nil {
		return &transientRecoveryError{err}
	}
	var (
		socket string
		sock   *socketRef
	)
	if st.Handle.Unit != "" {
		socket, sock = spec.dirs.socket(st.Nonce), spec.dirs.socketRef(st.Nonce)
		reason := ""
		if un, ok := unitNonce(st.Handle.Unit); !ok || un != st.Nonce {
			reason = "its nonce is not the recorded unit's"
		} else if _, err := sock.dial(); err != nil {
			reason = "its socket cannot be dialled: " + err.Error()
		}
		if reason != "" {
			c.logger.Error("state.json record ignored: "+reason,
				zap.String("unit", st.Handle.Unit), zap.String("nonce", st.Nonce), zap.String("socket", socket))
			st.Handle.Unit = ""
		}
	}
	if st.Handle.Unit == "" {
		st.Handle = handleState{} // nothing to reattach to
	}
	// An unreadable unit state is not "not running": launching beside
	// a unit that may still be up would duplicate the app. Report it
	// as transient; recover() retries with backoff until the manager
	// answers, rather than guessing.
	h, attached, err := c.runner.Reattach(st.Handle)
	if err != nil {
		// The pin stays for the retry: the unit may well be serving,
		// and an app that has since replaced its own name (allowed)
		// could not be pinned again from it.
		return &transientRecoveryError{fmt.Errorf("cannot tell whether %s is still running; not relaunching: %w", st.CurrentVersion, err)}
	}
	if !attached && sock != nil {
		sock.retire() // pinned above but definitively not adopted; the relaunch gets its own
	}
	if attached {
		inst := &instance{version: st.CurrentVersion, nonce: st.Nonce, socket: socket, sock: sock, handle: h}
		ma.mu.Lock()
		ma.current = inst
		ma.mu.Unlock()
		ma.activeSocket.Store(inst.sock)
		c.logger.Info("reattached to running instance", zap.String("version", st.CurrentVersion))
		ma.pokeWatchdog()
		if !ma.sweep(c, inst, "") {
			// Serving, but the ledger is unsettled: recover() retries
			// and the retry path above sweeps again.
			return &transientRecoveryError{fmt.Errorf("reattached %s; %w", st.CurrentVersion, errSweepUnconfirmed)}
		}
		return nil
	}

	inst, err := ma.launchVersion(c, st.CurrentVersion)
	if err != nil {
		// A binary that cannot be found will not appear by retrying;
		// everything else here (sweep, manager, unit reconcile) can.
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			return &permanentRecoveryError{fmt.Errorf("relaunching %s: %w", st.CurrentVersion, err)}
		}
		return &transientRecoveryError{fmt.Errorf("relaunching %s: %w", st.CurrentVersion, err)}
	}
	if err := ma.publishInstance(c, inst); err != nil {
		c.logger.Warn("persisting recovered state", zap.Error(err))
	}
	c.logger.Info("relaunched current version after restart",
		zap.String("version", st.CurrentVersion), zap.String("socket", inst.socket))
	ma.pokeWatchdog()
	return nil
}

// Recovery errors come in two kinds. transient: the manager could not
// be asked, a deploy held the lock, a unit could not be reconciled —
// retrying can fix it. permanent: the answer itself is bad (corrupt
// state, missing release dir, command not found) and no retry will
// change it. Anything not marked permanent is retried: an app down
// for an unclassified reason costs a log line a minute, an app left
// down for a transient one costs an outage.
type transientRecoveryError struct{ err error }

func (e *transientRecoveryError) Error() string { return e.err.Error() }
func (e *transientRecoveryError) Unwrap() error { return e.err }

type permanentRecoveryError struct{ err error }

func (e *permanentRecoveryError) Error() string { return e.err.Error() }
func (e *permanentRecoveryError) Unwrap() error { return e.err }

// transientRecovery reports whether err is worth retrying.
func transientRecovery(err error) bool {
	var p *permanentRecoveryError
	return err != nil && !errors.As(err, &p)
}

// errSweepUnconfirmed is the launch refusal when a pre-start sweep
// cannot confirm the app has no other unit running.
var errSweepUnconfirmed = errors.New("cannot confirm no other instance is running")

const (
	recoveryBackoffFloor = 2 * time.Second
	recoveryBackoffCap   = time.Minute
)

// recover runs ensureRunning until it succeeds or fails for a reason a
// retry cannot fix, backing off on transient manager trouble. A boot
// where the user manager is briefly unresponsive must not leave a
// healthy app unrouted until an operator reloads. ctx belongs to the
// config that started it and ends at that config's Cleanup.
func (ma *managedApp) recover(ctx context.Context, logger *zap.Logger) {
	delay := recoveryBackoffFloor
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return // destructed before this attempt
		}
		err := ma.ensureRunning()
		if err == nil {
			return
		}
		if !transientRecovery(err) {
			logger.Error("recovery failed", zap.Error(err))
			return
		}
		// Loud once, then a heartbeat: a manager that is down for an
		// hour should not fill the journal with sixty errors.
		level := zap.WarnLevel
		if attempt == 1 {
			level = zap.ErrorLevel
		}
		logger.Log(level, "recovery deferred; retrying", zap.Int("attempt", attempt), zap.Duration("in", delay), zap.Error(err))
		select {
		case <-ctx.Done():
			return
		case <-ma.snapshot().clock.After(delay):
		}
		if delay *= 2; delay > recoveryBackoffCap {
			delay = recoveryBackoffCap
		}
	}
}

// sweep stops every unit of this app other than keep — the runner's
// ledger, not ours, decides what is running. Recovery calls it so a
// unit hotserve lost track of (a state.json write that failed, an
// earlier stop that could not be confirmed) does not outlive the next
// start; deploys call it before GC. Returns false if something may
// still be running, in which case callers must not delete anything.
// A confirmed sweep is also the manager's word that no instance but
// keep is running, so every other instance's socket dirs are
// leftovers — except the one being launched right after, named by
// launching, whose run dir is already prepared.
func (ma *managedApp) sweep(c collaborators, keep *instance, launching string) bool {
	var keepHandle handle
	keepNonces := []string{launching}
	if keep != nil {
		keepHandle = keep.handle
		keepNonces = append(keepNonces, keep.nonce)
	}
	if err := c.runner.Sweep(ma.name, keepHandle); err != nil {
		c.logger.Error("sweeping stray instances", zap.Error(err))
		return false
	}
	pruneSockets(c.spec.dirs, keepNonces, c.logger)
	return true
}

// launchVersion starts an already-on-disk version on a fresh socket
// and returns the new instance without publishing it. The version comes
// from the caller's record (state.json, or the live instance the
// watchdog is replacing); the command and the environment are rendered
// from the CURRENT spec, so an edited app definition does reach a
// relaunch — the same exposure a reload has always had here (a changed
// command relaunches on the next crash). The sandbox is the same one a
// deploy gets: the base view plus this app's own dirs, which no config
// change moves. Recording the whole launch disposition is the fuller
// answer — see #35.
func (ma *managedApp) launchVersion(c collaborators, version string) (*instance, error) {
	spec := c.spec
	if _, err := os.Stat(spec.dirs.release(version)); err != nil {
		return nil, fmt.Errorf("release dir for version %s is missing: %w", version, err)
	}
	l, err := spec.prepareLaunch(version)
	if err != nil {
		return nil, err
	}
	// No Start without a confirmed sweep: whatever the manager still
	// runs for this app (a unit an earlier hotserve lost track of, a
	// stop that could not be confirmed) is settled first, or nothing
	// is launched beside it.
	if !ma.sweep(c, nil, l.nonce) {
		l.sock.retire()
		return nil, fmt.Errorf("%w; not launching", errSweepUnconfirmed)
	}
	h, err := c.runner.Start(l.startSpec(spec, spec.command))
	if err != nil {
		if !unitUnconfirmed(err) { // an ambiguous start may be live: the next confirmed sweep prunes
			l.sock.retire()
		}
		return nil, err
	}
	return l.instance(h), nil
}

// launch is one instance's disposition — its nonce, socket, release
// dir and environment — prepared once and used for the pre_start unit
// and the app unit alike, by deploys and relaunches alike, so the two
// paths cannot drift.
type launch struct {
	version, nonce, socket, releaseDir string
	sock                               *socketRef
	env                                []string
}

// prepareLaunch draws the instance's nonce and renders its environment
// from the current spec, creating the dirs the unit binds first: the
// runner resolves every bind source, and run/ holds nothing between
// instances.
func (spec *appSpec) prepareLaunch(version string) (launch, error) {
	if err := spec.dirs.ensure(); err != nil {
		return launch{}, err
	}
	nonce, err := newNonce()
	if err != nil {
		return launch{}, err
	}
	// Exclusive: the dir is this launch's reservation of the nonce, and
	// an existing one is some other instance's, never adopted as ours.
	if err := os.Mkdir(spec.dirs.runDir(nonce), 0o750); err != nil {
		return launch{}, err
	}
	l := launch{version: version, nonce: nonce, socket: spec.dirs.socket(nonce), releaseDir: spec.dirs.release(version)}
	l.sock = spec.dirs.socketRef(nonce)
	if l.env, err = buildEnv(spec, version, l.socket, l.releaseDir); err != nil {
		l.sock.retire()
		return launch{}, err
	}
	return l, nil
}

func (l launch) startSpec(spec *appSpec, command []string) startSpec {
	return startSpec{
		app:     spec.name,
		version: l.version,
		nonce:   l.nonce,
		socket:  l.socket,
		command: expandArgs(command, spec, l.version, l.socket, l.releaseDir),
		dir:     l.releaseDir,
		env:     l.env,
		grace:   spec.grace,
		sandbox: spec.sandboxSpecFor(l.releaseDir, l.nonce),
	}
}

func (l launch) instance(h handle) *instance {
	return &instance{version: l.version, nonce: l.nonce, socket: l.socket, sock: l.sock, handle: h}
}

// publishInstance installs inst as current, cuts traffic over to it
// and persists it for the next restart. It is the ONLY place current
// and activeSocket are installed — the watchdog's stale-instance guards
// depend on the swap-before-route ordering here, so promote, recovery
// and watchdog restarts must all go through it. The replaced
// instance's pinned socket is the caller's to retire, once that
// instance is stopped. A persist failure is returned, not logged: each
// caller has its own severity and message.
func (ma *managedApp) publishInstance(c collaborators, inst *instance) error {
	ma.mu.Lock()
	ma.current = inst
	ma.mu.Unlock()
	ma.activeSocket.Store(inst.sock)
	return ma.persistState(c, inst)
}

// unrouteIf clears activeSocket only while inst is still current, under
// the same lock that guards the current swap. Check-then-store without
// the lock races promote: a deploy could install and route a healthy
// instance between the check and the store, and the store would then
// unroute it permanently. The instance is dead, so its pinned socket
// is retired with it.
func (ma *managedApp) unrouteIf(inst *instance) {
	ma.mu.Lock()
	defer ma.mu.Unlock()
	if ma.current == inst {
		ma.activeSocket.Store(nil)
		inst.sock.retire()
	}
}

func (ma *managedApp) persistState(c collaborators, inst *instance) error {
	if err := c.store.save(appState{
		CurrentVersion: inst.version,
		Nonce:          inst.nonce,
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
	// Socket is the unix socket the instance listens on, under its
	// run/ dir. It is what the app bound, not what hotserve dials:
	// that is a hard link to the same inode under proxy/, outside
	// the app's reach. Reachable by the hotserve user (run/ is 0750).
	Socket string `json:"socket,omitempty"`
	PID    int    `json:"pid,omitempty"`
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
		s.Socket = ma.current.socket
		hs := ma.current.handle.state()
		s.PID = hs.PID
		s.Unit = hs.Unit
		s.Running = c.runner.Alive(ma.current.handle)
	}
	return s
}

// rollbackConfig restores the definition owner replaced, if owner's is
// still the one installed (a later config's stays). Used when a
// candidate is cleaned up while another config still holds the app.
func (ma *managedApp) rollbackConfig(owner any) bool {
	ma.specMu.Lock()
	defer ma.specMu.Unlock()
	if ma.configuredBy != owner || ma.prev == nil {
		return false
	}
	p := ma.prev
	ma.spec, ma.verifiers, ma.logger, ma.store = p.spec, p.verifiers, p.logger, p.store
	ma.prev = nil
	ma.configuredBy = nil
	if sr, ok := ma.runner.(*systemdRunner); ok {
		sr.setLogger(p.logger)
	}
	// The watchdog re-snapshots the spec on a poke; without one, a
	// candidate that had turned it off could leave it parked on the
	// restored (watchdog on) definition.
	ma.pokeWatchdog()
	return true
}

// Known limitation, accepted: Caddy offers apps no "config accepted"
// hook, so between this app's Start and the whole config's activation
// the watchdog and recovery act on the candidate definition. Rollback
// restores the definition; it cannot undo an action taken in that
// window (a restart with the candidate's settings). Closing it would
// mean suspending supervision during every reload, which is worse
// than the milliseconds-wide window it removes.

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
	if c.runner == nil {
		return nil // pooled but never configured (a Start that failed before ours ran)
	}
	if caddyExiting() || liveStartedApps.Load() == 0 {
		// Process exit, `validate`, or a candidate config that never
		// became the serving one (another app's Start failed): the
		// units belong to whoever is or will be serving them. Leave
		// them; the next start's sweep settles any that are truly
		// orphaned.
		if inst != nil {
			c.logger.Info("app keeps running for reattach", zap.String("version", inst.version), zap.String("unit", inst.handle.state().Unit))
		}
		return nil
	}
	// Removed by a reload that is now serving: no unit may outlive
	// the definition. (The removing config's Cleanup has already
	// joined its recovery, so nothing can launch after this sweep.)
	// Stop what we track (if anything), then everything else the
	// manager holds for this app: a removed app must leave no unit
	// behind, tracked or not — an ambiguous start may have left one
	// that never became current.
	var stopErr error
	if inst != nil {
		c.logger.Info("app removed from config; stopping it", zap.String("version", inst.version))
		stopErr = c.runner.Stop(inst.handle, c.spec.grace)
		inst.sock.retire()
	}
	sweepErr := c.runner.Sweep(ma.name, nil)
	if sweepErr == nil {
		pruneSockets(c.spec.dirs, nil, c.logger)
	}
	return errors.Join(stopErr, sweepErr)
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
	ma.wdCtx = ctx
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
var envAllowlist = []string{"PATH", "LANG", "TZ"}

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
// highest: the allowlisted slice of Caddy's environment (PATH, LANG,
// TZ, LC_*), the sandbox HOME, env_file, inline env, then the injected
// SOCKET contract. HOME is never inherited: hotserve's own state
// dir does not exist inside the unit at all, so the app's HOME is its
// shared dir — the one writable, persistent place in the view. Not a
// nicety: every runtime that touches $HOME would ENOENT, which is the
// shape a deny-by-default view fails in. Applied before env_file and
// env, so an operator can still point it elsewhere.
func buildEnv(spec *appSpec, version, socket, releaseDir string) ([]string, error) {
	env := append(inheritedEnv(), "HOME="+spec.dirs.shared)
	if spec.envFile != "" {
		fileVars, err := parseEnvFile(spec.envFile)
		if err != nil {
			return nil, fmt.Errorf("env_file: %w", err)
		}
		env = append(env, fileVars...)
	}
	for k, v := range spec.env {
		env = append(env, k+"="+expandPlaceholders(v, spec, version, socket, releaseDir))
	}
	env = append(env, "SOCKET="+socket)
	return env, nil
}

// envKeyRe is what systemd accepts in Environment=; the exec runner
// let anything through to execve, so it is validated explicitly now —
// at config load for inline env, at deploy time for env_file.
var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validEnvKey(key string) bool { return envKeyRe.MatchString(key) }

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
		if !validEnvKey(key) {
			return nil, fmt.Errorf("%s:%d: %q is not a valid environment variable name (systemd requires %s)", path, i+1, key, envKeyRe)
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
func expandPlaceholders(s string, spec *appSpec, version, socket, releaseDir string) string {
	return strings.NewReplacer(
		"{version}", version,
		"{socket}", socket,
		"{release_dir}", releaseDir,
		"{shared_dir}", spec.dirs.shared,
	).Replace(s)
}

func expandArgs(args []string, spec *appSpec, version, socket, releaseDir string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = expandPlaceholders(a, spec, version, socket, releaseDir)
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
	jwks     *http.Client // fetches OIDC issuers' public keys for deploy auth
}
