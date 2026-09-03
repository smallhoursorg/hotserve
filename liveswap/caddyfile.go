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
//	    deploy_trust <preset>  { ... }   # who may deploy (repeatable)
//	    allow_insecure_http
//	    artifact_allowlist     <host[:port][/path/][?param&param...]...>
//	    sandbox                <on|off>   # default for every app
//	    app <name> {
//	        command           <cmd> [args...]
//	        pre_start         <cmd> [args...]
//	        env               <KEY> <value>
//	        env_file          <path>
//	        deploy_trust <preset> { ... }  # overrides the global default
//	        artifact_allowlist <host[:port][/path/][?param&param...]...>
//	        sandbox           <on|off>   # overrides the global default
//	        health_path       <path|off>
//	        health_interval   <duration>
//	        health_timeout    <duration>
//	        soak              <duration>
//	        deadline          <duration>
//	        drain             <duration>
//	        grace             <duration>
//	        watchdog          <on|off>
//	        watchdog_failures <n>
//	        watchdog_grace    <duration>
//	        watchdog_restarts <n>
//	        watchdog_window   <duration>
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
		case "deploy_trust":
			tc, err := parseDeployTrust(d)
			if err != nil {
				return err
			}
			a.DeployTrust = append(a.DeployTrust, tc)
			continue // the block consumed its own trailing tokens
		case "allow_insecure_http":
			a.AllowInsecureHTTP = true
		case "artifact_allowlist":
			entries := d.RemainingArgs()
			if len(entries) == 0 {
				return d.ArgErr()
			}
			a.ArtifactAllowlist = append(a.ArtifactAllowlist, entries...)
		case "sandbox":
			if !d.NextArg() {
				return d.ArgErr()
			}
			a.Sandbox = d.Val()
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
		case "deploy_trust":
			tc, err := parseDeployTrust(d)
			if err != nil {
				return err
			}
			cfg.DeployTrust = append(cfg.DeployTrust, tc)
			continue
		case "artifact_allowlist":
			entries := d.RemainingArgs()
			if len(entries) == 0 {
				return d.ArgErr()
			}
			cfg.ArtifactAllowlist = append(cfg.ArtifactAllowlist, entries...)
			continue
		case "sandbox":
			if !d.NextArg() {
				return d.ArgErr()
			}
			cfg.Sandbox = d.Val()
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
		case "watchdog":
			if !d.NextArg() {
				return d.ArgErr()
			}
			cfg.Watchdog = d.Val()
		case "watchdog_failures":
			if err := parseCountArg(d, &cfg.WatchdogFailures); err != nil {
				return err
			}
		case "watchdog_grace":
			if err := parseDurationArg(d, &cfg.WatchdogGrace); err != nil {
				return err
			}
		case "watchdog_restarts":
			if err := parseCountArg(d, &cfg.WatchdogRestarts); err != nil {
				return err
			}
		case "watchdog_window":
			if err := parseDurationArg(d, &cfg.WatchdogWindow); err != nil {
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

func parseCountArg(d *caddyfile.Dispenser, out *int) error {
	name := d.Val()
	if !d.NextArg() {
		return d.ArgErr()
	}
	n, err := strconv.Atoi(d.Val())
	if err != nil {
		return d.Errf("invalid %s %q: %v", name, d.Val(), err)
	}
	*out = n
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

// parseDeployTrust parses one `deploy_trust <preset> { ... }` block,
// at either the global or the per-app nesting level:
//
//	deploy_trust github {          # preset names the token issuer
//	    audience   <aud>           # required for OIDC presets
//	    claim      <name> <value>  # exact-match constraint, repeatable
//	    subject    <sub>           # sugar for `claim sub <sub>`
//	}
//	deploy_trust local {           # non-CI / manual / test fallback
//	    public_key <path>
//	}
//	deploy_trust oidc  { issuer <url>; audience <aud>; ... }
func parseDeployTrust(d *caddyfile.Dispenser) (TrustConfig, error) {
	tc := TrustConfig{}
	if !d.NextArg() {
		return tc, d.Err("deploy_trust needs a preset: github, gitlab, oidc or local")
	}
	tc.Kind = d.Val()
	if d.NextArg() {
		return tc, d.ArgErr() // only the preset name, then a block
	}
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		switch d.Val() {
		case "issuer":
			if !d.NextArg() {
				return tc, d.ArgErr()
			}
			tc.Issuer = d.Val()
		case "audience":
			if !d.NextArg() {
				return tc, d.ArgErr()
			}
			tc.Audience = d.Val()
		case "public_key":
			if !d.NextArg() {
				return tc, d.ArgErr()
			}
			tc.PublicKey = d.Val()
		case "subject":
			if !d.NextArg() {
				return tc, d.ArgErr()
			}
			// Route to a `sub` claim rather than the Subject sugar field:
			// if the value is a placeholder that resolves empty, it then
			// stays a (fail-closed) sub="" constraint instead of silently
			// dropping — dropping would broaden the trust source.
			if tc.Claims == nil {
				tc.Claims = make(map[string]string)
			}
			if _, dup := tc.Claims["sub"]; dup {
				return tc, d.Err("subject and `claim sub` are both set")
			}
			tc.Claims["sub"] = d.Val()
		case "claim":
			if !d.NextArg() {
				return tc, d.ArgErr()
			}
			key := d.Val()
			if !d.NextArg() {
				return tc, d.ArgErr()
			}
			if tc.Claims == nil {
				tc.Claims = make(map[string]string)
			}
			if _, dup := tc.Claims[key]; dup {
				return tc, d.Errf("duplicate claim %q", key)
			}
			tc.Claims[key] = d.Val()
		default:
			return tc, d.Errf("unknown deploy_trust subdirective %q", d.Val())
		}
	}
	return tc, nil
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
