package deploytrust

import (
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"

	"github.com/smallhoursorg/hotserve/liveswap/internal/dispenser"
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
//	    audience   <aud>           # this box's; `deploy-token --audience` to match
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
		if err := dispenser.RefuseRepeat(d, seen, "claim"); err != nil {
			return tc, err
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
