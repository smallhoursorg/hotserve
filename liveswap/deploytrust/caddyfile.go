package deploytrust

import (
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

// Parse reads one `deploy_trust <preset> { ... }` block, at whatever
// nesting level the caller's grammar puts it — liveswap's global
// options and its per-app blocks, the box's `box` block:
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
func Parse(d *caddyfile.Dispenser) (TrustConfig, error) {
	tc := TrustConfig{}
	if !d.NextArg() {
		return tc, d.Err("deploy_trust needs a preset: github, gitlab, oidc or local")
	}
	tc.Kind = d.Val()
	if d.NextArg() {
		return tc, d.ArgErr() // only the preset name, then a block
	}
	seen := map[string]bool{}
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		// A repeated subdirective is refused, as liveswap refuses one
		// anywhere in its config: the earlier line would be silently
		// overridden. The rule and its words are liveswap's refuseRepeat
		// (liveswap/caddyfile.go), which this package cannot import;
		// kept the same by hand. claim adds an entry per line and may
		// repeat; a repeated claim name is refused below.
		if key := d.Val(); key != "claim" {
			if seen[key] {
				return tc, d.Errf("duplicate %s: the earlier line would be silently overridden", key)
			}
			seen[key] = true
		}
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
