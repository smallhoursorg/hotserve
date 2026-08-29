// Package liveswap turns Caddy into a zero-downtime deploy orchestrator
// for a single server. CI builds a tarball and POSTs a webhook with its
// URL and version; Caddy downloads it, runs an optional pre-start
// command (migrations), starts the new version as a systemd unit on a
// fresh localhost port, health-gates it, atomically cuts traffic over,
// then gracefully stops the old version. Part of hotserve, from
// smallhours.
//
// Three cooperating modules:
//   - liveswap (this file): the app module holding app definitions and
//     the per-app orchestration state
//   - http.handlers.liveswap_webhook (handler.go): the deploy trigger
//   - http.reverse_proxy.upstreams.liveswap (upstreams.go): routes
//     reverse_proxy traffic to the active version's port
package liveswap

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(App{})
}

// appPool holds every managedApp, keyed by app name, OUTSIDE any config
// instance. This is what makes app processes survive config reloads:
// Caddy provisions the new config (which takes a pool reference) before
// it cleans up the old one (which releases its reference), so the
// refcount never touches zero across a reload and Destruct only runs
// at real shutdown (where it leaves the unit running for reattach) or
// when an app is removed from the config (where it stops it).
var appPool = caddy.NewUsagePool()

func poolKey(name string) string { return "liveswap:app:" + name }

var appNameRe = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

// versionRe matches the same tag alphabet the Nomad-era webhook
// allowed. The alphabet has no path separator, but "." and ".." still
// match it, so validVersion — not the regex alone — is the check to
// use before a version reaches a filesystem path.
var versionRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// versionPathComponent renders a version tag safe to use as a single
// path component, mechanically: rooting the string at "/" and cleaning
// resolves any "..", and the leading separator is then stripped. For
// every tag validVersion accepts this is the identity function (the
// tag alphabet contains no separators), so it changes no behavior —
// it is belt-and-braces beneath validVersion, which remains the real
// gate. It also uses the exact idiom static analysis models as a
// path-traversal sanitizer (CodeQL's FilepathCleanSanitizer), so the
// version->filesystem flows stop lighting up go/path-injection at
// every join downstream. (Tested for both properties.)
func versionPathComponent(v string) string {
	return strings.TrimPrefix(filepath.Clean("/"+v), "/")
}

// validVersion reports whether v is an acceptable version tag. It must
// match the tag alphabet and must not be "." or ".." — both match the
// alphabet but resolve releases/<version> to the releases dir itself
// or its parent (the app root), turning a deploy's release-replace
// (os.RemoveAll of releaseDir in download.go) into deletion of every
// release or of the app's persistent shared/ data.
func validVersion(v string) bool {
	return versionRe.MatchString(v) && v != "." && v != ".."
}

// App is the `liveswap` Caddy app module: the deploy orchestrator.
type App struct {
	// Root is the directory holding all app state on disk:
	// <root>/<app>/{releases/<version>/, shared/, tmp/, state.json}.
	// Default "/var/lib/liveswap".
	Root string `json:"root,omitempty"`

	// DeployTrust is the default set of deploy-auth trust sources for
	// all apps; each app may override it. A deploy request carries an
	// `Authorization: Bearer <JWT>` and is authorized if any source
	// verifies it (OIDC against an issuer's public JWKS, or a local
	// public key). No shared secret ever lives on the box. Every app
	// must resolve to at least one source — config load fails otherwise.
	DeployTrust []TrustConfig `json:"deploy_trust,omitempty"`

	// AllowInsecureHTTP permits artifact downloads over plain http.
	// Default false (https only). Exists for test rigs and LAN setups.
	AllowInsecureHTTP bool `json:"allow_insecure_http,omitempty"`

	// ArtifactAllowlist declares where artifacts may be fetched from —
	// required: there is no "any origin" mode. Entries are host
	// ("artifacts.corp", single-tenant) or host+path prefix
	// ("github.com/your-org/"): on multi-tenant platforms the tenant
	// lives in the path, so a host-only rule would admit anyone's
	// artifacts. Governs the first hop only (GitHub asset URLs
	// redirect to S3); every hop must still be https unless
	// AllowInsecureHTTP. Apps may override.
	ArtifactAllowlist []string `json:"artifact_allowlist,omitempty"`

	// Apps defines the managed applications, keyed by name
	// ([a-z0-9-]). The name is the webhook path segment and the
	// argument to `dynamic liveswap <name>`.
	Apps map[string]*AppConfig `json:"apps,omitempty"`

	logger          *zap.Logger
	managed         map[string]*managedApp
	specs           map[string]*appSpec // built in Provision, installed at Start
	clients         *fetchClients
	started         bool     // Start ran: this config counts as live until Cleanup
	pooled          []string // pool keys this config instance holds references to
	allowlist       []artifactAllowEntry
	globalTrust     []trustSource // resolved global DeployTrust, for the unknown-app path
	globalVerifiers []verifier
}

// AppConfig defines one managed application.
type AppConfig struct {
	// Command starts the app, argv-style, with the release directory as
	// working directory. The app must listen on 127.0.0.1 at the
	// injected PORT. Placeholders: {version}, {port}, {release_dir},
	// {shared_dir}. Required.
	Command []string `json:"command,omitempty"`

	// PreStart runs to completion in the release directory before the
	// new version starts (migrations, asset warming). A non-zero exit
	// aborts the deploy while the old version keeps serving. Optional.
	PreStart []string `json:"pre_start,omitempty"`

	// Env sets extra environment variables. Values may use the same
	// placeholders as Command. Applied on top of env_file.
	Env map[string]string `json:"env,omitempty"`

	// EnvFile loads KEY=VALUE lines (e.g. /etc/liveswap/blog.env) into
	// the app's environment. Optional; missing file fails the deploy.
	EnvFile string `json:"env_file,omitempty"`

	// DeployTrust overrides the global deploy-auth trust sources for
	// this app (replaces, not appends).
	DeployTrust []TrustConfig `json:"deploy_trust,omitempty"`

	// ArtifactAllowlist overrides the global allowlist for this app
	// (same entry syntax; replaces, not appends).
	ArtifactAllowlist []string `json:"artifact_allowlist,omitempty"`

	// HealthPath is GET-probed on the new instance; 2xx = healthy.
	// "off" disables HTTP probing (the gate becomes "process stays
	// alive through the soak"). Default "/health".
	HealthPath string `json:"health_path,omitempty"`

	// HealthInterval is the time between probes. Default 5s.
	HealthInterval caddy.Duration `json:"health_interval,omitempty"`

	// HealthTimeout is the per-probe timeout. Default 2s.
	HealthTimeout caddy.Duration `json:"health_timeout,omitempty"`

	// Soak is how long the new instance must be continuously healthy
	// before traffic cuts over. Default 15s.
	Soak caddy.Duration `json:"soak,omitempty"`

	// Deadline bounds the whole health gate (and pre_start); a version
	// that cannot get healthy within it is stopped and the deploy
	// fails. Default 5m.
	Deadline caddy.Duration `json:"deadline,omitempty"`

	// Drain is the pause between cutover and SIGTERM to the old
	// version, letting in-flight requests finish. Default 5s.
	Drain caddy.Duration `json:"drain,omitempty"`

	// Grace is how long the old version gets between SIGTERM and
	// SIGKILL. Default 10s.
	Grace caddy.Duration `json:"grace,omitempty"`

	// Watchdog enables continuous supervision of the running instance:
	// a crash, or WatchdogFailures consecutive failed health probes,
	// restarts the current version (with backoff, within the
	// WatchdogRestarts/WatchdogWindow budget). "off" disables it.
	// Default "on".
	Watchdog string `json:"watchdog,omitempty"`

	// WatchdogFailures is how many consecutive health probes must fail
	// before the watchdog restarts the app; a single passing probe
	// resets the count. Default 3.
	WatchdogFailures int `json:"watchdog_failures,omitempty"`

	// WatchdogGrace is how long after every (re)start probe failures
	// are ignored, giving the app time to boot; it re-arms on each
	// watchdog restart. A process exit always counts, grace or not.
	// Default 30s.
	WatchdogGrace caddy.Duration `json:"watchdog_grace,omitempty"`

	// WatchdogRestarts is how many watchdog restarts (crash and health
	// triggered alike) may happen within WatchdogWindow. The budget is
	// a rate limiter, not a give-up point: when the window is full the
	// watchdog throttles until the oldest restart slides out, then
	// keeps trying. A successful deploy resets the budget. Default 5.
	WatchdogRestarts int `json:"watchdog_restarts,omitempty"`

	// WatchdogWindow is the sliding window the restart budget is
	// counted over. Default 10m.
	WatchdogWindow caddy.Duration `json:"watchdog_window,omitempty"`

	// Keep is how many release directories to retain on disk,
	// including the current one. Default 5. The running version is
	// always retained even if it is older than the newest `keep` (e.g.
	// after rolling back to an old release), so on-disk releases can
	// briefly be `keep+1`.
	Keep int `json:"keep,omitempty"`

	// MaxArtifactSize caps the artifact download, in bytes (the
	// Caddyfile accepts human forms like 100MB). Decompressed content
	// is additionally capped at 10x this. Default 100MB.
	MaxArtifactSize int64 `json:"max_artifact_size,omitempty"`
}

// CaddyModule returns the Caddy module information.
func (App) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "liveswap",
		New: func() caddy.Module { return new(App) },
	}
}

// Provision applies defaults, resolves {env.*} placeholders in the
// static config, and binds each app definition to its long-lived
// managedApp in the pool.
func (a *App) Provision(ctx caddy.Context) error {
	a.logger = ctx.Logger()
	repl := caddy.NewReplacer()

	// ReplaceKnown resolves {env.*} now but leaves the deploy-time
	// placeholders ({version}, {port}, ...) untouched for app.go.
	a.Root = repl.ReplaceKnown(a.Root, "")
	for i, e := range a.ArtifactAllowlist {
		a.ArtifactAllowlist[i] = repl.ReplaceKnown(e, "")
	}
	resolveTrustPlaceholders(repl, a.DeployTrust)
	if a.Root == "" {
		a.Root = "/var/lib/liveswap"
	}

	var err error
	if a.allowlist, err = parseAllowlist(a.ArtifactAllowlist); err != nil {
		return err
	}
	// Global trust sources back the unknown-app path: a request for an
	// app that does not exist is still authenticated (against the
	// global sources) before its 404, so app names never leak to
	// unauthenticated callers.
	if a.globalTrust, err = buildTrust(a.DeployTrust, nil); err != nil {
		return err
	}

	clients := &fetchClients{
		download: newDownloadClient(a.AllowInsecureHTTP),
		// The health client never follows redirects: the probe is a
		// control signal from supervisor to app, and an app answering
		// 3xx must read as "not 2xx", not steer the supervisor's
		// request elsewhere.
		health: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		// The JWKS client fetches OIDC issuers' public keys over https
		// only (unless allow_insecure_http), with a bounded timeout.
		jwks: newJWKSClient(a.AllowInsecureHTTP),
	}
	a.globalVerifiers = resolveVerifiers(a.globalTrust, clients.jwks)

	// Build every spec first, then validate, and only then commit to the
	// process-global pool. Caddy calls Validate *after* Provision, but a
	// managedApp is shared across config loads — so if we mutated it here
	// and Validate then rejected the config, the rejected policy (its
	// deploy-auth verifiers included) would already be live on the pooled
	// object. Validate-before-commit keeps a bad reload from taking effect.
	a.managed = make(map[string]*managedApp, len(a.Apps))
	specs := make(map[string]*appSpec, len(a.Apps))
	for name, cfg := range a.Apps {
		if cfg == nil {
			cfg = new(AppConfig)
			a.Apps[name] = cfg
		}
		cfg.applyDefaults(repl)
		spec, err := a.buildSpec(name, cfg)
		if err != nil {
			return err
		}
		specs[name] = spec
	}
	if err := a.Validate(); err != nil {
		return err
	}
	// Take pool references now (so a reload never drops the refcount to
	// zero) but install nothing on the pooled apps until Start: Caddy
	// keeps the old config if any app's Start fails, and `validate`
	// never starts at all — neither may leave a rejected spec live on
	// an app that is still serving under the previous config.
	a.specs = specs
	a.clients = clients
	for name := range specs {
		val, _ := appPool.LoadOrStore(poolKey(name), newManagedApp(name))
		a.managed[name] = val.(*managedApp)
		a.pooled = append(a.pooled, poolKey(name))
	}
	// Warm OIDC discovery in the background so the first verification of a
	// known app is not slower (by JWKS-fetch latency) than an unknown one.
	sets := [][]verifier{a.globalVerifiers}
	for _, spec := range specs {
		sets = append(sets, resolveVerifiers(spec.trust, clients.jwks))
	}
	warmVerifiers(sets...)
	return nil
}

// liveStartedApps counts liveswap configs that have Started and not
// yet been Cleaned up. It is how Destruct tells "an app was removed by
// a reload that is now serving" (another started config is live: stop
// the units) from "this config never became the serving one" — a
// candidate rejected by another app's Start, `validate`, or process
// exit — where the units belong to whoever is or will be serving them.
var liveStartedApps atomic.Int32

func (cfg *AppConfig) applyDefaults(repl *caddy.Replacer) {
	resolveTrustPlaceholders(repl, cfg.DeployTrust)
	cfg.EnvFile = repl.ReplaceKnown(cfg.EnvFile, "")
	for i, e := range cfg.ArtifactAllowlist {
		cfg.ArtifactAllowlist[i] = repl.ReplaceKnown(e, "")
	}
	for i, arg := range cfg.Command {
		cfg.Command[i] = repl.ReplaceKnown(arg, "")
	}
	for i, arg := range cfg.PreStart {
		cfg.PreStart[i] = repl.ReplaceKnown(arg, "")
	}
	for k, v := range cfg.Env {
		cfg.Env[k] = repl.ReplaceKnown(v, "")
	}
	if cfg.HealthPath == "" {
		cfg.HealthPath = "/health"
	}
	if cfg.HealthInterval == 0 {
		cfg.HealthInterval = caddy.Duration(5 * time.Second)
	}
	if cfg.HealthTimeout == 0 {
		cfg.HealthTimeout = caddy.Duration(2 * time.Second)
	}
	if cfg.Soak == 0 {
		cfg.Soak = caddy.Duration(15 * time.Second)
	}
	if cfg.Deadline == 0 {
		cfg.Deadline = caddy.Duration(5 * time.Minute)
	}
	if cfg.Drain == 0 {
		cfg.Drain = caddy.Duration(5 * time.Second)
	}
	if cfg.Grace == 0 {
		cfg.Grace = caddy.Duration(10 * time.Second)
	}
	if cfg.Watchdog == "" {
		cfg.Watchdog = "on"
	}
	if cfg.WatchdogFailures == 0 {
		cfg.WatchdogFailures = 3
	}
	if cfg.WatchdogGrace == 0 {
		cfg.WatchdogGrace = caddy.Duration(30 * time.Second)
	}
	if cfg.WatchdogRestarts == 0 {
		cfg.WatchdogRestarts = 5
	}
	if cfg.WatchdogWindow == 0 {
		cfg.WatchdogWindow = caddy.Duration(10 * time.Minute)
	}
	if cfg.Keep == 0 {
		cfg.Keep = 5
	}
	if cfg.MaxArtifactSize == 0 {
		cfg.MaxArtifactSize = 100_000_000
	}
}

func (a *App) buildSpec(name string, cfg *AppConfig) (*appSpec, error) {
	healthPath := cfg.HealthPath
	if healthPath == "off" {
		healthPath = ""
	}
	allowlist := a.allowlist
	if len(cfg.ArtifactAllowlist) > 0 {
		var err error
		if allowlist, err = parseAllowlist(cfg.ArtifactAllowlist); err != nil {
			return nil, fmt.Errorf("app %s: %w", name, err)
		}
	}
	trust, err := buildTrust(a.DeployTrust, cfg.DeployTrust)
	if err != nil {
		return nil, fmt.Errorf("app %s: %w", name, err)
	}
	return &appSpec{
		name:            name,
		command:         cfg.Command,
		preStart:        cfg.PreStart,
		env:             cfg.Env,
		envFile:         cfg.EnvFile,
		trust:           trust,
		healthPath:      healthPath,
		healthInterval:  time.Duration(cfg.HealthInterval),
		healthTimeout:   time.Duration(cfg.HealthTimeout),
		soak:            time.Duration(cfg.Soak),
		deadline:        time.Duration(cfg.Deadline),
		drain:           time.Duration(cfg.Drain),
		grace:           time.Duration(cfg.Grace),
		watchdogOn:      cfg.Watchdog != "off",
		wdFailures:      cfg.WatchdogFailures,
		wdGrace:         time.Duration(cfg.WatchdogGrace),
		wdRestarts:      cfg.WatchdogRestarts,
		wdWindow:        time.Duration(cfg.WatchdogWindow),
		keep:            cfg.Keep,
		maxArtifactSize: cfg.MaxArtifactSize,
		allowInsecure:   a.AllowInsecureHTTP,
		allowlist:       allowlist,
		dirs:            newAppDirs(a.Root, name),
	}, nil
}

// Validate enforces semantic invariants (runs after Provision has
// applied defaults, for JSON and Caddyfile config alike).
func (a *App) Validate() error {
	if !strings.HasPrefix(a.Root, "/") {
		return fmt.Errorf("root must be an absolute path, got %q", a.Root)
	}
	for name, cfg := range a.Apps {
		if !appNameRe.MatchString(name) {
			return fmt.Errorf("app name %q must match %s", name, appNameRe)
		}
		for k := range cfg.Env {
			if !validEnvKey(k) {
				return fmt.Errorf("app %s: env key %q is not a valid environment variable name (must match %s)", name, k, envKeyRe)
			}
		}
		if len(cfg.Command) == 0 {
			return fmt.Errorf("app %s: command is required", name)
		}
		if len(a.DeployTrust) == 0 && len(cfg.DeployTrust) == 0 {
			return fmt.Errorf("app %s: no deploy_trust configured — declare who may deploy, e.g. a `deploy_trust github { audience ...; claim repository your-org/%s }` block or a `deploy_trust local { public_key ... }` fallback (globally or per app)", name, name)
		}
		// Closed by default, deliberately without an "any origin"
		// escape hatch: a deploy webhook that fetches from anywhere is
		// an SSRF primitive, and on multi-tenant hosts anything short
		// of a tenant pin is theater.
		if len(a.ArtifactAllowlist) == 0 && len(cfg.ArtifactAllowlist) == 0 {
			return fmt.Errorf("app %s: artifact_allowlist is required — declare where artifacts may be fetched from, e.g. `artifact_allowlist github.com/your-org/` (host-only entries suit single-tenant hosts)", name)
		}
		if cfg.HealthPath != "off" && !strings.HasPrefix(cfg.HealthPath, "/") {
			return fmt.Errorf("app %s: health_path must start with / (or be \"off\"), got %q", name, cfg.HealthPath)
		}
		for field, d := range map[string]caddy.Duration{
			"health_interval": cfg.HealthInterval,
			"health_timeout":  cfg.HealthTimeout,
			"deadline":        cfg.Deadline,
			"grace":           cfg.Grace,
		} {
			if d <= 0 {
				return fmt.Errorf("app %s: %s must be positive, got %v", name, field, time.Duration(d))
			}
		}
		if cfg.Soak < 0 || cfg.Drain < 0 {
			return fmt.Errorf("app %s: soak and drain must not be negative", name)
		}
		if cfg.Watchdog != "on" && cfg.Watchdog != "off" {
			return fmt.Errorf("app %s: watchdog must be \"on\" or \"off\", got %q", name, cfg.Watchdog)
		}
		if cfg.WatchdogFailures < 1 {
			return fmt.Errorf("app %s: watchdog_failures must be at least 1, got %d", name, cfg.WatchdogFailures)
		}
		if cfg.WatchdogRestarts < 1 {
			return fmt.Errorf("app %s: watchdog_restarts must be at least 1, got %d", name, cfg.WatchdogRestarts)
		}
		if cfg.WatchdogGrace < 0 {
			return fmt.Errorf("app %s: watchdog_grace must not be negative, got %v", name, time.Duration(cfg.WatchdogGrace))
		}
		if cfg.WatchdogWindow <= 0 {
			return fmt.Errorf("app %s: watchdog_window must be positive, got %v", name, time.Duration(cfg.WatchdogWindow))
		}
		if cfg.Keep < 1 {
			return fmt.Errorf("app %s: keep must be at least 1, got %d", name, cfg.Keep)
		}
		if cfg.MaxArtifactSize < 1 {
			return fmt.Errorf("app %s: max_artifact_size must be positive, got %d", name, cfg.MaxArtifactSize)
		}
	}
	return nil
}

// Start recovers apps after a Caddy restart: each app that has a
// recorded current version but no live process is relaunched. Runs in
// the background so a slow app cannot stall config load; the health
// gate is a deploy gate, not a boot gate.
func (a *App) Start() error {
	// Apps run as transient units under this user's systemd manager.
	// Prove it answers before anything runs, with an error that says
	// what to fix; there is deliberately no fallback runner. Checked
	// here rather than in Provision so `hotserve validate` — which
	// provisions and cleans up without starting — works as any user.
	if len(a.managed) > 0 {
		if err := probeUserManager(); err != nil {
			return err
		}
	}
	for name, ma := range a.managed {
		ma.configure(a, a.specs[name], a.logger.Named(name), a.clients)
		ma.startWatchdog()
	}
	a.started = true
	liveStartedApps.Add(1)
	// Units of apps no loaded config names have no managedApp to sweep
	// them (removed or renamed while hotserve was down): settle them
	// against the manager's own listing. Background, like recovery;
	// the sweep judges each app against the pool right before acting,
	// so neither a reload racing it nor a candidate config that later
	// fails to activate can lose an app someone still holds.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), unknownAppSweepTimeout)
		defer cancel()
		if err := sweepUnknownApps(ctx, userManager, a.logger); err != nil {
			a.logger.Error("sweeping units of apps no longer configured", zap.Error(err))
		}
	}()
	for name, ma := range a.managed {
		// Tracked so Destruct-on-removal can wait for an in-flight
		// recovery before its final sweep (exit does not wait: the
		// process is going, and the next start's sweep settles it).
		ma.recoveryWG.Add(1)
		go func(name string, ma *managedApp) {
			defer ma.recoveryWG.Done()
			ma.recover(a.logger.Named(name))
		}(name, ma)
	}
	return nil
}

// Stop intentionally does NOT stop app processes: on a config reload
// the old config is stopped after the new one starts, and the whole
// point of the pool is that apps keep serving across reloads. Real
// shutdown reaches the apps via Cleanup → pool release → Destruct.
func (a *App) Stop() error { return nil }

// Cleanup releases this config's pool references. When the last
// reference goes (shutdown, or an app removed from the config), the
// pool calls managedApp.Destruct: an app removed from the config is
// stopped; on process exit the units stay up for the next start.
func (a *App) Cleanup() error {
	if a.started {
		a.started = false
		liveStartedApps.Add(-1)
		// A candidate Caddy rejected after our Start (another app's
		// Start failed) is cleaned up while the config it would have
		// replaced still holds the apps: give them back the serving
		// definition. A config replaced by a successful reload is not
		// the last writer, so this is a no-op for it.
		for name, ma := range a.managed {
			if refs, ok := appPool.References(poolKey(name)); ok && refs > 1 {
				if ma.rollbackConfig(a) {
					a.logger.Warn("config rejected after start; restored the serving definition", zap.String("app", name))
				}
			}
		}
	}
	var firstErr error
	for _, key := range a.pooled {
		if _, err := appPool.Delete(key); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// managedApp returns the pool-backed state for a configured app, or
// nil if the name is not in this config.
func (a *App) managedApp(name string) *managedApp {
	return a.managed[name]
}

// unknownAppSweepTimeout bounds the start-time sweep of units whose
// apps are no longer configured.
const unknownAppSweepTimeout = 2 * time.Minute

// Interface guards.
var (
	_ caddy.App          = (*App)(nil)
	_ caddy.Provisioner  = (*App)(nil)
	_ caddy.Validator    = (*App)(nil)
	_ caddy.CleanerUpper = (*App)(nil)
)
