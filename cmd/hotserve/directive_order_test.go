package main

import (
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig"
)

// TestBoxWebhookRunsFirst adapts a box site in the binary as shipped,
// with every directive it registers — Souin's cache among them —
// and holds box_webhook ahead of each handler that could answer on /
// before it: the cache, a static response, liveswap_webhook. The box
// module's own test holds the standard directives; this one holds the
// ones only the product links in.
func TestBoxWebhookRunsFirst(t *testing.T) {
	const in = `deploy.example.com {
	cache
	respond /x ok
	liveswap_webhook
	box_webhook
}
`
	cfg, _, err := caddyconfig.GetAdapter("caddyfile").Adapt([]byte(in), map[string]any{"filename": "Caddyfile"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(cfg)
	box := strings.Index(s, `"handler":"box_webhook"`)
	if box < 0 {
		t.Fatalf("no box_webhook: %s", s)
	}
	for _, other := range []string{`"handler":"cache"`, `"handler":"static_response"`, `"handler":"liveswap_webhook"`} {
		if i := strings.Index(s, other); i < 0 || i < box {
			t.Errorf("%s runs before box_webhook (or is missing): %s", other, s)
		}
	}
}
