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
	return filepath.Join(d.releases, version)
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
	secret          string
	healthPath      string // "" = no HTTP check (health_path off)
	healthInterval  time.Duration
	healthTimeout   time.Duration
	soak            time.Duration
	deadline        time.Duration
	drain           time.Duration
	grace           time.Duration
	keep            int
	maxArtifactSize int64
	allowInsecure   bool
	allowedHosts    map[string]struct{}
	dirs            appDirs
}

// deployRequest is the validated webhook payload.
type deployRequest struct {
	URL        string `json:"url"`
	Version    string `json:"version"`
	AuthHeader string `json:"auth_header,omitempty"`
}

// deployResult records the outcome of the most recent deploy attempt
// for the status endpoint and webhook responses.
type deployResult struct {
	Version    string    `json:"version"`
	Status     string    `json:"status"` // "succeeded" | "failed"
	Error      string    `json:"error,omitempty"`
	Phase      string    `json:"phase,omitempty"` // phase reached when it failed
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
	specMu sync.RWMutex
	spec   *appSpec
	runner runner
	prober prober
	fetch  fetcher
	clock  clock
	store  stateStore
	logger *zap.Logger

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
}

func newManagedApp(name string) *managedApp {
	return &managedApp{name: name, phase: "idle"}
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
}

// specEqual is a shallow inequality check good enough for the "config
// changed" notice; false negatives only cost a log line.
func specEqual(a, b *appSpec) bool {
	return fmt.Sprintf("%+v", a) == fmt.Sprintf("%+v", b)
}

func (ma *managedApp) currentSpec() *appSpec {
	ma.specMu.RLock()
	defer ma.specMu.RUnlock()
	return ma.spec
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
func (ma *managedApp) Deploy(ctx context.Context, req deployRequest) (err error) {
	if !ma.deployMu.TryLock() {
		return errDeployInProgress
	}
	defer ma.deployMu.Unlock()

	c := ma.snapshot()
	spec := c.spec
	started := c.clock.Now()
	logger := c.logger.With(zap.String("version", req.Version))
	logger.Info("deploy started", zap.String("artifact_host", hostOf(req.URL)))

	defer func() {
		result := deployResult{
			Version:    req.Version,
			Status:     "succeeded",
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

	releaseDir, err := c.fetch.fetch(ctx, spec, req, func(phase string) { ma.setPhase(c, phase) })
	if err != nil {
		return err
	}

	port, err := freePort()
	if err != nil {
		return err
	}
	env, err := buildEnv(spec, req.Version, port, releaseDir)
	if err != nil {
		return err
	}

	if len(spec.preStart) > 0 {
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
	newHandle, err := c.runner.Start(startSpec{
		command: expandArgs(spec.command, spec, req.Version, port, releaseDir),
		dir:     releaseDir,
		env:     env,
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
		_ = c.runner.Stop(newHandle, spec.grace)
		return fmt.Errorf("health gate: %w", err)
	}

	// The point of no return. From here on the request context is
	// ignored: a CI client hanging up must not abort stop-old or GC.
	ma.setPhase(c, "promoting")
	newInst := &instance{version: req.Version, port: port, handle: newHandle}
	ma.mu.Lock()
	ma.current = newInst
	ma.mu.Unlock()
	ma.activePort.Store(int64(port)) // ← the cutover

	if err := ma.persistState(c, newInst); err != nil {
		logger.Error("state persistence failed; deploys still work but a Caddy restart will not know about this version", zap.Error(err))
	}

	if old != nil && c.runner.Alive(old.handle) {
		ma.setPhase(c, "draining")
		c.clock.Sleep(spec.drain)
		ma.setPhase(c, "stopping_old")
		if err := c.runner.Stop(old.handle, spec.grace); err != nil {
			logger.Warn("stopping old version", zap.Error(err))
		}
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
		return nil
	}

	port, err := freePort()
	if err != nil {
		return err
	}
	env, err := buildEnv(spec, st.CurrentVersion, port, releaseDir)
	if err != nil {
		return err
	}
	h, err := c.runner.Start(startSpec{
		command: expandArgs(spec.command, spec, st.CurrentVersion, port, releaseDir),
		dir:     releaseDir,
		env:     env,
	})
	if err != nil {
		return fmt.Errorf("relaunching %s: %w", st.CurrentVersion, err)
	}
	inst := &instance{version: st.CurrentVersion, port: port, handle: h}
	ma.mu.Lock()
	ma.current = inst
	ma.mu.Unlock()
	ma.activePort.Store(int64(port))
	if err := ma.persistState(c, inst); err != nil {
		c.logger.Warn("persisting recovered state", zap.Error(err))
	}
	c.logger.Info("relaunched current version after restart",
		zap.String("version", st.CurrentVersion), zap.Int("port", port))
	return nil
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
	App            string        `json:"app"`
	Phase          string        `json:"phase"`
	CurrentVersion string        `json:"current_version,omitempty"`
	Port           int           `json:"port,omitempty"`
	PID            int           `json:"pid,omitempty"`
	Running        bool          `json:"running"`
	LastDeploy     *deployResult `json:"last_deploy,omitempty"`
}

func (ma *managedApp) status() statusSnapshot {
	c := ma.snapshot()
	ma.mu.Lock()
	defer ma.mu.Unlock()
	s := statusSnapshot{App: ma.name, Phase: ma.phase, LastDeploy: ma.lastDeploy}
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
// from the Caddyfile, never a plain reload. It stops the child.
func (ma *managedApp) Destruct() error {
	c := ma.snapshot()
	inst := ma.currentInstance()
	if inst == nil {
		return nil
	}
	c.logger.Info("shutting down app", zap.String("version", inst.version))
	return c.runner.Stop(inst.handle, c.spec.grace)
}

// envAllowlist is the only part of Caddy's own environment that apps
// inherit. Everything else is withheld: the supervisor's env holds
// deploy credentials (LIVESWAP_SECRET, ACME DNS tokens), and handing
// those to every app defeats the isolation story — env-dumping
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
}
