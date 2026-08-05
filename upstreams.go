package hotswap

import (
	"fmt"
	"net/http"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
)

func init() {
	caddy.RegisterModule(Upstreams{})
}

// Upstreams is the reverse_proxy dynamic upstream source:
//
//	reverse_proxy {
//	    dynamic hotswap <app>
//	}
//
// GetUpstreams reads the app's active port from an atomic — the deploy
// pipeline's promote step swaps that value, which makes the cutover
// instantaneous and config-reload-free while keeping every
// reverse_proxy feature (websockets, h2, streaming, load-balancer
// retries) intact.
type Upstreams struct {
	// App names the hotswap app whose active version receives traffic.
	App string `json:"app,omitempty"`

	ma *managedApp
}

// CaddyModule returns the Caddy module information.
func (Upstreams) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.reverse_proxy.upstreams.hotswap",
		New: func() caddy.Module { return new(Upstreams) },
	}
}

// Provision resolves the app reference.
func (u *Upstreams) Provision(ctx caddy.Context) error {
	if u.App == "" {
		return fmt.Errorf("an app name is required: dynamic hotswap <app>")
	}
	appModule, err := ctx.App("hotswap")
	if err != nil {
		return err
	}
	u.ma = appModule.(*App).managedApp(u.App)
	if u.ma == nil {
		return fmt.Errorf("unknown hotswap app %q (define it in the hotswap global options)", u.App)
	}
	return nil
}

// GetUpstreams returns the active version's address, or an error (a
// 502 with a clear log line) when nothing has been deployed yet.
func (u *Upstreams) GetUpstreams(_ *http.Request) ([]*reverseproxy.Upstream, error) {
	port := u.ma.activePort.Load()
	if port == 0 {
		return nil, fmt.Errorf("hotswap app %q has no running version yet (deploy one via the webhook)", u.App)
	}
	return []*reverseproxy.Upstream{
		{Dial: "127.0.0.1:" + portString(int(port))},
	}, nil
}

// Interface guards.
var (
	_ caddy.Provisioner           = (*Upstreams)(nil)
	_ reverseproxy.UpstreamSource = (*Upstreams)(nil)
)
