// Command hotserve is the Hot Sauce server from smallhours: Caddy with
// zero-downtime app deploys (liveswap), rate-limit hint enforcement
// (penaltybox) and HTTP caching (Souin + Otter storage) compiled in.
// It is distributed as its own binary and OS packages — see the repo
// README — and behaves exactly like a custom Caddy build: same CLI,
// same Caddyfile, same admin API.
package main

import (
	caddycmd "github.com/caddyserver/caddy/v2/cmd"

	// Standard Caddy modules — this is what makes the binary equivalent
	// to a stock caddy distribution (http, tls automation, caddyfile
	// adapter, ...); upstream cmd/caddy/main.go does exactly this.
	_ "github.com/caddyserver/caddy/v2/modules/standard"

	// hotserve modules. Importing liveswap also makes this process
	// non-dumpable before main runs (see liveswap/harden).
	_ "github.com/smallhoursorg/hotserve/liveswap"
	_ "github.com/smallhoursorg/hotserve/penaltybox"

	// HTTP cache (Souin) with in-memory Otter storage.
	_ "github.com/darkweak/souin/plugins/caddy"
	_ "github.com/darkweak/storages/otter/caddy"
)

func main() {
	caddycmd.Main()
}
