// Package hotswap turns Caddy into a zero-downtime deploy orchestrator
// for a single server. CI builds a tarball and POSTs a webhook with its
// URL and version; Caddy downloads it, runs an optional pre-start
// command (migrations), starts the new version as a child process on a
// fresh localhost port, health-gates it, atomically cuts traffic over,
// then gracefully stops the old version. Part of the Hot Source Stack.
//
// Three cooperating modules:
//   - hotswap (this file): the app module holding app definitions and
//     the per-app orchestration state
//   - http.handlers.hotswap_webhook (handler.go): the deploy trigger
//   - http.reverse_proxy.upstreams.hotswap (upstreams.go): routes
//     reverse_proxy traffic to the active version's port
package hotswap

import (
	"fmt"
	"net/http"
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

func poolKey(name string) string { return "hotswap:app:" + name }

var appNameRe = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

// versionRe matches the same tag alphabet the Nomad-era webhook
// allowed; it is also filesystem-safe by construction.
var versionRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// App is the `hotswap` Caddy app module: the deploy orchestrator.
type App struct {
	// Root is the directory holding all app state on disk:
	// <root>/<app>/{releases/<version>/, shared/, tmp/, state.json}.
	// Default "/var/lib/hotswap".
	Root string `json:"root,omitempty"`

	// WebhookSecret is the default shared secret for all apps; each app
	// may override it. Use a placeholder like {env.HOTSWAP_SECRET} to
	// keep it out of the Caddyfile. Every app must end up with a
	// non-empty secret — config load fails otherwise.
	WebhookSecret string `json:"webhook_secret,omitempty"`

	// AllowInsecureHTTP permits artifact downloads over plain http.
	// Default false (https only). Exists for test rigs and LAN setups.
	AllowInsecureHTTP bool `json:"allow_insecure_http,omitempty"`

	// AllowedArtifactHosts, when non-empty, restricts artifact URLs to
	// these hostnames. Default empty = any host.
	AllowedArtifactHosts []string `json:"allowed_artifact_hosts,omitempty"`

	// Apps defines the managed applications, keyed by name
	// ([a-z0-9-]). The name is the webhook path segment and the
	// argument to `dynamic hotswap <name>`.
	Apps map[string]*AppConfig `json:"apps,omitempty"`

	logger  *zap.Logger
	managed map[string]*managedApp
	pooled  []string // pool keys this config instance holds references to
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

	// EnvFile loads KEY=VALUE lines (e.g. /etc/hotswap/blog.env) into
	// the app's environment. Optional; missing file fails the deploy.
	EnvFile string `json:"env_file,omitempty"`

	// WebhookSecret overrides the global default for this app.
	WebhookSecret string `json:"webhook_secret,omitempty"`

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
		ID:  "hotswap",
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
	if a.Root == "" {
		a.Root = "/var/lib/hotswap"
	}

	clients := &fetchClients{
		download: newDownloadClient(),
		health:   &http.Client{},
	}

	a.managed = make(map[string]*managedApp, len(a.Apps))
	for name, cfg := range a.Apps {
		if cfg == nil {
			cfg = new(AppConfig)
			a.Apps[name] = cfg
		}
		cfg.applyDefaults(repl, a.WebhookSecret)
		spec := a.buildSpec(name, cfg)

		val, _ := appPool.LoadOrStore(poolKey(name), newManagedApp(name))
		ma := val.(*managedApp)
		ma.configure(spec, a.logger.Named(name), clients)
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
	if cfg.Keep == 0 {
		cfg.Keep = 5
	}
	if cfg.MaxArtifactSize == 0 {
		cfg.MaxArtifactSize = 100_000_000
	}
}

func (a *App) buildSpec(name string, cfg *AppConfig) *appSpec {
	healthPath := cfg.HealthPath
	if healthPath == "off" {
		healthPath = ""
	}
	var allowedHosts map[string]struct{}
	if len(a.AllowedArtifactHosts) > 0 {
		allowedHosts = make(map[string]struct{}, len(a.AllowedArtifactHosts))
		for _, h := range a.AllowedArtifactHosts {
			allowedHosts[h] = struct{}{}
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
		keep:            cfg.Keep,
		maxArtifactSize: cfg.MaxArtifactSize,
		allowInsecure:   a.AllowInsecureHTTP,
		allowedHosts:    allowedHosts,
		dirs:            newAppDirs(a.Root, name),
	}
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
