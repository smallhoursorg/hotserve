// Package liveswap turns Caddy into a zero-downtime deploy orchestrator
// for a single server. CI builds a tarball and POSTs a webhook with its
// URL and version; Caddy downloads it, runs an optional pre-start
// command (migrations), starts the new version as a child process on a
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
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
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
// refcount never touches zero across a reload and Destruct — which
// stops the child process — only runs at real shutdown or when an app
// is removed from the config.
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

	// WebhookSecret is the default shared secret for all apps; each app
	// may override it. Use a placeholder like {env.LIVESWAP_SECRET} to
	// keep it out of the Caddyfile. Every app must end up with a
	// non-empty secret — config load fails otherwise.
	WebhookSecret string `json:"webhook_secret,omitempty"`

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

	logger    *zap.Logger
	managed   map[string]*managedApp
	pooled    []string // pool keys this config instance holds references to
	allowlist []artifactAllowEntry
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

	// WebhookSecret overrides the global default for this app.
	WebhookSecret string `json:"webhook_secret,omitempty"`

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
	// including the current one. Default 5.
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
	a.WebhookSecret = repl.ReplaceKnown(a.WebhookSecret, "")
	for i, e := range a.ArtifactAllowlist {
		a.ArtifactAllowlist[i] = repl.ReplaceKnown(e, "")
	}
	if a.Root == "" {
		a.Root = "/var/lib/liveswap"
	}

	var err error
	if a.allowlist, err = parseAllowlist(a.ArtifactAllowlist); err != nil {
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
	}

	a.managed = make(map[string]*managedApp, len(a.Apps))
	for name, cfg := range a.Apps {
		if cfg == nil {
			cfg = new(AppConfig)
			a.Apps[name] = cfg
		}
		cfg.applyDefaults(repl, a.WebhookSecret)
		spec, err := a.buildSpec(name, cfg)
		if err != nil {
			return err
		}

		val, _ := appPool.LoadOrStore(poolKey(name), newManagedApp(name))
		ma := val.(*managedApp)
		ma.configure(spec, a.logger.Named(name), clients)
		ma.startWatchdog()
		a.managed[name] = ma
		a.pooled = append(a.pooled, poolKey(name))
	}
	return nil
}

func (cfg *AppConfig) applyDefaults(repl *caddy.Replacer, defaultSecret string) {
	cfg.WebhookSecret = repl.ReplaceKnown(cfg.WebhookSecret, "")
	if cfg.WebhookSecret == "" {
		cfg.WebhookSecret = defaultSecret
	}
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
	return &appSpec{
		name:            name,
		command:         cfg.Command,
		preStart:        cfg.PreStart,
		env:             cfg.Env,
		envFile:         cfg.EnvFile,
		secret:          cfg.WebhookSecret,
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
		if len(cfg.Command) == 0 {
			return fmt.Errorf("app %s: command is required", name)
		}
		if cfg.WebhookSecret == "" {
			return fmt.Errorf("app %s: no webhook secret configured (set webhook_secret globally or per app; if it references {env.*}, the variable is empty)", name)
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
	for name, ma := range a.managed {
		go func(name string, ma *managedApp) {
			if err := ma.ensureRunning(); err != nil {
				a.logger.Error("recovery failed", zap.String("app", name), zap.Error(err))
			}
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
// pool calls managedApp.Destruct, which stops the child process.
func (a *App) Cleanup() error {
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

// Interface guards.
var (
	_ caddy.App          = (*App)(nil)
	_ caddy.Provisioner  = (*App)(nil)
	_ caddy.Validator    = (*App)(nil)
	_ caddy.CleanerUpper = (*App)(nil)
)
