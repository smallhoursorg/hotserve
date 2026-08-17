package liveswap

import (
	"math"
	"strconv"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/dustin/go-humanize"
)

func init() {
	httpcaddyfile.RegisterGlobalOption("liveswap", parseGlobalOption)
	httpcaddyfile.RegisterHandlerDirective("liveswap_webhook", parseWebhookDirective)
	// The webhook is terminal, but give it a sane default position for
	// site blocks that mix it with other directives.
	httpcaddyfile.RegisterDirectiveOrder("liveswap_webhook", httpcaddyfile.Before, "reverse_proxy")
}

func parseGlobalOption(d *caddyfile.Dispenser, _ any) (any, error) {
	a := new(App)
	if err := a.UnmarshalCaddyfile(d); err != nil {
		return nil, err
	}
	return httpcaddyfile.App{
		Name:  "liveswap",
		Value: caddyconfig.JSON(a, nil),
	}, nil
}

func parseWebhookDirective(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var handler Handler
	err := handler.UnmarshalCaddyfile(h.Dispenser)
	return &handler, err
}

// UnmarshalCaddyfile parses the global options block. No defaults are
// applied here — Provision owns every default — so an empty block is
// valid and zero values pass through.
//
//	liveswap {
//	    root                   <path>
//	    webhook_secret         <secret>
//	    allow_insecure_http
//	    artifact_allowlist     <host[/path/]...>
//	    app <name> {
//	        command           <cmd> [args...]
//	        pre_start         <cmd> [args...]
//	        env               <KEY> <value>
//	        env_file          <path>
//	        webhook_secret    <secret>
//	        artifact_allowlist <host[/path/]...>
//	        health_path       <path|off>
//	        health_interval   <duration>
//	        health_timeout    <duration>
//	        soak              <duration>
//	        deadline          <duration>
//	        drain             <duration>
//	        grace             <duration>
//	        keep              <n>
//	        max_artifact_size <size>
//	    }
//	}
func (a *App) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // consume option name
	if d.NextArg() {
		return d.ArgErr() // no positional args; everything is in the block
	}
	for d.NextBlock(0) {
		switch d.Val() {
		case "root":
			if !d.NextArg() {
				return d.ArgErr()
			}
			a.Root = d.Val()
		case "webhook_secret":
			if !d.NextArg() {
				return d.ArgErr()
			}
			a.WebhookSecret = d.Val()
		case "allow_insecure_http":
			a.AllowInsecureHTTP = true
		case "artifact_allowlist":
			entries := d.RemainingArgs()
			if len(entries) == 0 {
				return d.ArgErr()
			}
			a.ArtifactAllowlist = append(a.ArtifactAllowlist, entries...)
		case "allowed_artifact_hosts":
			return d.Errf("allowed_artifact_hosts was replaced by artifact_allowlist; entries may now pin a path prefix (github.com/your-org/), which is the recommended form on multi-tenant hosts")
		case "app":
			if !d.NextArg() {
				return d.ArgErr()
			}
			name := d.Val()
			if a.Apps == nil {
				a.Apps = make(map[string]*AppConfig)
			}
			if _, dup := a.Apps[name]; dup {
				return d.Errf("duplicate app %q", name)
			}
			cfg := new(AppConfig)
			if err := cfg.unmarshalBlock(d); err != nil {
				return err
			}
			a.Apps[name] = cfg
			continue // the app block consumed its own trailing tokens
		default:
			return d.Errf("unknown subdirective %q", d.Val())
		}
		if d.NextArg() {
			return d.ArgErr() // no simple subdirective takes more args
		}
	}
	return nil
}

func (cfg *AppConfig) unmarshalBlock(d *caddyfile.Dispenser) error {
	for d.NextBlock(1) {
		switch d.Val() {
		case "command":
			args := d.RemainingArgs()
			if len(args) == 0 {
				return d.ArgErr()
			}
			cfg.Command = args
			continue
		case "pre_start":
			args := d.RemainingArgs()
			if len(args) == 0 {
				return d.ArgErr()
			}
			cfg.PreStart = args
			continue
		case "env":
			if !d.NextArg() {
				return d.ArgErr()
			}
			key := d.Val()
			if !d.NextArg() {
				return d.ArgErr()
			}
			if cfg.Env == nil {
				cfg.Env = make(map[string]string)
			}
			if _, dup := cfg.Env[key]; dup {
				return d.Errf("duplicate env %q", key)
			}
			cfg.Env[key] = d.Val()
		case "env_file":
			if !d.NextArg() {
				return d.ArgErr()
			}
			cfg.EnvFile = d.Val()
		case "webhook_secret":
			if !d.NextArg() {
				return d.ArgErr()
			}
			cfg.WebhookSecret = d.Val()
		case "artifact_allowlist":
			entries := d.RemainingArgs()
			if len(entries) == 0 {
				return d.ArgErr()
			}
			cfg.ArtifactAllowlist = append(cfg.ArtifactAllowlist, entries...)
			continue
		case "health_path":
			if !d.NextArg() {
				return d.ArgErr()
			}
			cfg.HealthPath = d.Val()
		case "health_interval":
			if err := parseDurationArg(d, &cfg.HealthInterval); err != nil {
				return err
			}
		case "health_timeout":
			if err := parseDurationArg(d, &cfg.HealthTimeout); err != nil {
				return err
			}
		case "soak":
			if err := parseDurationArg(d, &cfg.Soak); err != nil {
				return err
			}
		case "deadline":
			if err := parseDurationArg(d, &cfg.Deadline); err != nil {
				return err
			}
		case "drain":
			if err := parseDurationArg(d, &cfg.Drain); err != nil {
				return err
			}
		case "grace":
			if err := parseDurationArg(d, &cfg.Grace); err != nil {
				return err
			}
		case "keep":
			if !d.NextArg() {
				return d.ArgErr()
			}
			n, err := strconv.Atoi(d.Val())
			if err != nil {
				return d.Errf("invalid keep %q: %v", d.Val(), err)
			}
			cfg.Keep = n
		case "max_artifact_size":
			if !d.NextArg() {
				return d.ArgErr()
			}
			size, err := humanize.ParseBytes(d.Val())
			if err != nil {
				return d.Errf("invalid max_artifact_size %q: %v", d.Val(), err)
			}
			if size > math.MaxInt64 {
				return d.Errf("max_artifact_size %q overflows", d.Val())
			}
			cfg.MaxArtifactSize = int64(size)
		default:
			return d.Errf("unknown app subdirective %q", d.Val())
		}
		if d.NextArg() {
			return d.ArgErr() // no subdirective takes more args than consumed
		}
	}
	return nil
}

func parseDurationArg(d *caddyfile.Dispenser, out *caddy.Duration) error {
	name := d.Val()
	if !d.NextArg() {
		return d.ArgErr()
	}
	dur, err := caddy.ParseDuration(d.Val())
	if err != nil {
		return d.Errf("invalid %s %q: %v", name, d.Val(), err)
	}
	*out = caddy.Duration(dur)
	return nil
}

// UnmarshalCaddyfile parses the webhook directive, which takes no
// arguments and no block:
//
//	liveswap_webhook
func (h *Handler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // consume directive name
	if d.NextArg() {
		return d.ArgErr()
	}
	if d.NextBlock(0) {
		return d.Err("liveswap_webhook takes no block")
	}
	return nil
}

// UnmarshalCaddyfile parses the dynamic upstream source:
//
//	dynamic liveswap <app>
func (u *Upstreams) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // consume module name
	if !d.NextArg() {
		return d.ArgErr()
	}
	u.App = d.Val()
	if d.NextArg() {
		return d.ArgErr()
	}
	if d.NextBlock(0) {
		return d.Err("dynamic liveswap takes no block")
	}
	return nil
}

// Interface guards.
var (
	_ caddyfile.Unmarshaler = (*App)(nil)
	_ caddyfile.Unmarshaler = (*Handler)(nil)
	_ caddyfile.Unmarshaler = (*Upstreams)(nil)
)
