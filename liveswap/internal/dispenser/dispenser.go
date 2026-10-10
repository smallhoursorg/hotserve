// Package dispenser holds the one Caddyfile rule liveswap's parsers
// and deploytrust's apply alike, in one place so the two cannot
// drift: a repeated subdirective is refused.
package dispenser

import (
	"slices"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

// RefuseRepeat refuses the subdirective at the dispenser's cursor when
// it already appeared in this block, and records it otherwise. A
// repeated one would silently override the earlier line — a stale
// env_file left under a new one keeps loading; the second of two
// command lines wins with no diagnostic. additive names the
// subdirectives that add an entry per line instead and may repeat;
// such a line refuses a repeated key itself (env its KEY, deploy_trust
// its claim name).
func RefuseRepeat(d *caddyfile.Dispenser, seen map[string]bool, additive ...string) error {
	key := d.Val()
	if slices.Contains(additive, key) {
		return nil
	}
	if seen[key] {
		return d.Errf("duplicate %s: the earlier line would be silently overridden", key)
	}
	seen[key] = true
	return nil
}
