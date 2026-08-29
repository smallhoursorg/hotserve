// Command hotserve is the Hot Sauce server from smallhours: Caddy with
// zero-downtime app deploys (liveswap), rate-limit hint enforcement
// (penaltybox) and HTTP caching (Souin + Otter storage) compiled in.
// It is distributed as its own binary and OS packages — see the repo
// README — and behaves exactly like a custom Caddy build: same CLI,
// same Caddyfile, same admin API.
package main

import (
	"fmt"
	"os"

	caddycmd "github.com/caddyserver/caddy/v2/cmd"

	// Standard Caddy modules — this is what makes the binary equivalent
	// to a stock caddy distribution (http, tls automation, caddyfile
	// adapter, ...); upstream cmd/caddy/main.go does exactly this.
	_ "github.com/caddyserver/caddy/v2/modules/standard"

	// hotserve modules.
	"github.com/smallhoursorg/hotserve/liveswap"
	_ "github.com/smallhoursorg/hotserve/penaltybox"

	// HTTP cache (Souin) with in-memory Otter storage.
	_ "github.com/darkweak/souin/plugins/caddy"
	_ "github.com/darkweak/storages/otter/caddy"
)

func main() {
	// Before anything else: deployed apps share this process's UID, and
	// only a non-dumpable process keeps its /proc closed to them. Not
	// fatal — a server that cannot start protects nothing — but loud.
	if err := liveswap.HardenProcess(); err != nil {
		fmt.Fprintf(os.Stderr, "hotserve: cannot mark the process non-dumpable (%v): processes running as this user can read its environment via /proc\n", err)
	}
	caddycmd.Main()
}
