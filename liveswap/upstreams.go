package liveswap

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
)

func init() {
	caddy.RegisterModule(Upstreams{})
}

// Upstreams is the reverse_proxy dynamic upstream source:
//
//	reverse_proxy {
//	    dynamic liveswap <app>
//	}
//
// GetUpstreams reads the app's active socket from an atomic — the
// deploy pipeline's promote step swaps that value, which makes the
// cutover instantaneous and config-reload-free while keeping every
// reverse_proxy feature (websockets, h2, streaming, load-balancer
// retries) intact.
type Upstreams struct {
	// App names the liveswap app whose active version receives traffic.
	App string `json:"app,omitempty"`

	ma      *managedApp
	waitCap time.Duration // recoveryWaitCap; a test shortens it
}

// recoveryWaitCap bounds how long a request is held for the app's first
// recovery attempt. Past it the request fails as it would have without
// the hold, so the cap can cost a visitor a 503 but never a deploy. The
// whole window (the HTTP server up before this app has started, the
// manager and sandbox probes, then the reattach) measured about 60ms
// in the install-test container. The cap stays well under the unit's
// TimeoutStopSec (5s) because Caddy's shutdown waits for held requests,
// and a stop during a stuck start must not end in a SIGKILL.
const recoveryWaitCap = 2 * time.Second

// CaddyModule returns the Caddy module information.
func (Upstreams) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.reverse_proxy.upstreams.liveswap",
		New: func() caddy.Module { return new(Upstreams) },
	}
}

// Provision resolves the app reference.
func (u *Upstreams) Provision(ctx caddy.Context) error {
	if u.App == "" {
		return fmt.Errorf("an app name is required: dynamic liveswap <app>")
	}
	appModule, err := ctx.App("liveswap")
	if err != nil {
		return err
	}
	u.ma = appModule.(*App).managedApp(u.App)
	if u.ma == nil {
		return fmt.Errorf("unknown liveswap app %q (define it in the liveswap global options)", u.App)
	}
	return nil
}

// GetUpstreams returns the active version's address, or an error (a
// 503 with a clear log line) when there is nothing to route to.
func (u *Upstreams) GetUpstreams(r *http.Request) ([]*reverseproxy.Upstream, error) {
	sock := u.ma.activeSocket.Load()
	if sock == nil {
		if !u.awaitRecovery(r.Context()) {
			return nil, fmt.Errorf("liveswap app %q is still being recovered after hotserve started", u.App)
		}
		sock = u.ma.activeSocket.Load()
	}
	if sock == nil {
		return nil, fmt.Errorf("liveswap app %q has no running version yet (deploy one via the webhook)", u.App)
	}
	// The pinned name under proxy/, never the app-writable one (socketRef).
	addr, err := sock.dial()
	if err != nil {
		return nil, fmt.Errorf("liveswap app %q: %w", u.App, err)
	}
	// "unix/" + an absolute path is Caddy's unix//abs/path spelling.
	return []*reverseproxy.Upstream{
		{Dial: "unix/" + addr},
	}, nil
}

// awaitRecovery holds a request that arrived before hotserve has
// reattached to the app — Caddy can start serving before the liveswap
// app has started, and recovery runs in the background after that —
// until the first recovery attempt returns, the client gives up, or
// recoveryWaitCap passes. It reports whether recovery had returned.
// Once it has, this returns at once: a nil socket after that (a crash
// the watchdog is relaunching, an app never deployed) fails as fast as
// it always did.
func (u *Upstreams) awaitRecovery(ctx context.Context) bool {
	select {
	case <-u.ma.recovered:
		return true
	default:
	}
	limit := u.waitCap
	if limit == 0 {
		limit = recoveryWaitCap
	}
	t := time.NewTimer(limit)
	defer t.Stop()
	select {
	case <-u.ma.recovered:
		return true
	case <-ctx.Done():
	case <-t.C:
	}
	return false
}

// Interface guards.
var (
	_ caddy.Provisioner           = (*Upstreams)(nil)
	_ reverseproxy.UpstreamSource = (*Upstreams)(nil)
)
