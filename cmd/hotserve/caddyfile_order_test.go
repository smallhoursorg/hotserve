package main

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig"
)

// TestPenaltyboxCacheOrder pins where Caddy's directive sorting puts
// hint_penaltybox against Souin's cache in this build, which compiles
// both in. Souin registers cache before rewrite and penaltybox
// registers hint_penaltybox before reverse_proxy, so wherever Caddy
// sorts (site level, or inside a handle block) cache runs first and a
// cache hit never reaches the penalty box. Inside route the written
// order holds, which is why penaltybox/README.md and the starter
// packaging/Caddyfile put hint_penaltybox first in a route.
func TestPenaltyboxCacheOrder(t *testing.T) {
	cases := []struct {
		name string
		site string
		want []string // relative order of cache, hint_penaltybox, reverse_proxy
	}{
		{"site level", `
	hint_penaltybox
	cache
	reverse_proxy localhost:8000`,
			[]string{"cache", "hint_penaltybox", "reverse_proxy"}},
		{"inside handle", `
	handle {
		hint_penaltybox
		cache
		reverse_proxy localhost:8000
	}`,
			[]string{"cache", "hint_penaltybox", "reverse_proxy"}},
		{"inside route", `
	route {
		hint_penaltybox
		cache
		reverse_proxy localhost:8000
	}`,
			[]string{"hint_penaltybox", "cache", "reverse_proxy"}},
		// The starter Caddyfile's example site, uncommented.
		{"starter example", `
	route {
		hint_penaltybox
		cache
		reverse_proxy {
			dynamic liveswap myapp
		}
	}
	handle_errors 502 503 {
		respond "myapp is not running" {http.error.status_code}
	}`,
			[]string{"hint_penaltybox", "cache", "reverse_proxy"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			order := adaptedHandlerOrder(t, "example.com {"+c.site+"\n}\n")
			got := slices.DeleteFunc(order, func(h string) bool {
				return h != "cache" && h != "hint_penaltybox" && h != "reverse_proxy"
			})
			if !slices.Equal(got, c.want) {
				t.Errorf("handler order %v, want %v", got, c.want)
			}
		})
	}
}

// adaptedRoute and adaptedHandler are the slice of Caddy's HTTP route
// JSON that adaptedHandlerOrder walks.
type adaptedRoute struct {
	Handle []adaptedHandler `json:"handle"`
}

type adaptedHandler struct {
	Handler string         `json:"handler"`
	Routes  []adaptedRoute `json:"routes"`
}

// adaptedHandlerOrder adapts a Caddyfile and lists its HTTP handlers in
// the order Caddy chains them for a request every matcher matches:
// routes in list order, a subroute's routes in place. It ignores
// matchers, route groups, terminal flags and error routes, so it suits
// one-site Caddyfiles only. (penaltybox's caddyfile_test.go has the
// same walker; that module cannot import this one's tests.)
func adaptedHandlerOrder(t *testing.T, input string) []string {
	t.Helper()
	adapter := caddyconfig.GetAdapter("caddyfile")
	if adapter == nil {
		t.Fatal("caddyfile adapter not registered")
	}
	out, _, err := adapter.Adapt([]byte(input), nil)
	if err != nil {
		t.Fatalf("adapt: %v", err)
	}
	var cfg struct {
		Apps struct {
			HTTP struct {
				Servers map[string]struct {
					Routes []adaptedRoute `json:"routes"`
				} `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		t.Fatalf("decode adapted config: %v", err)
	}
	if n := len(cfg.Apps.HTTP.Servers); n != 1 {
		t.Fatalf("adapted config has %d servers, want 1:\n%s", n, out)
	}
	var order []string
	for _, srv := range cfg.Apps.HTTP.Servers {
		order = appendHandlerOrder(order, srv.Routes)
	}
	return order
}

func appendHandlerOrder(order []string, routes []adaptedRoute) []string {
	for _, r := range routes {
		for _, h := range r.Handle {
			if h.Handler == "subroute" {
				order = appendHandlerOrder(order, h.Routes)
				continue
			}
			order = append(order, h.Handler)
		}
	}
	return order
}
