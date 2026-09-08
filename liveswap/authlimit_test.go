package liveswap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func TestAuthLimiterWindow(t *testing.T) {
	clk := newFakeClock()
	l := newAuthLimiter(clk)
	l.budget, l.window = 3, time.Minute

	// Failures at t=0, 10s, 20s: the third spends the budget.
	for i := range 3 {
		v := l.fail("a")
		if !v.log || v.throttled || v.trippedKey != (i == 2) {
			t.Fatalf("failure %d: %+v", i+1, v)
		}
		if i < 2 {
			clk.Advance(10 * time.Second)
		}
	}
	v := l.fail("a")
	if v.log || !v.throttled || v.retryAfter != 40*time.Second || v.trippedKey {
		t.Fatalf("past the budget: %+v", v)
	}
	// Another address is untouched.
	if v := l.fail("b"); !v.log || v.throttled {
		t.Fatalf("a different address was affected: %+v", v)
	}
	// Over-budget failures are not recorded, so the window is exactly
	// the budgeted ones and retryAfter counts down from the oldest.
	clk.Advance(30 * time.Second) // t=50
	if v := l.fail("a"); v.retryAfter != 10*time.Second {
		t.Fatalf("retryAfter = %v, want the remainder of the window", v.retryAfter)
	}
	// The window slides: once the oldest ages out one more is logged,
	// and the budget is spent again — but "throttled" is said once per
	// window, not once per refilled slot.
	clk.Advance(11 * time.Second) // t=61: the t=0 failure has aged out
	if v := l.fail("a"); !v.log || v.throttled || v.trippedKey {
		t.Fatalf("after the window slid: %+v", v)
	}
	if v := l.fail("a"); !v.throttled {
		t.Fatalf("the refilled slot should have been the last: %+v", v)
	}
	// Once the window drains entirely the next spend says so again.
	clk.Advance(2 * time.Minute)
	for range 3 {
		v = l.fail("a")
	}
	if !v.trippedKey {
		t.Fatalf("a drained window should trip again: %+v", v)
	}
	// A success clears the address entirely.
	l.clear("a")
	l.clear("b")
	if l.size() != 0 {
		t.Fatalf("clear left %d addresses", l.size())
	}
}

// The process-wide budget holds however many addresses a flood comes
// from: past it, failures are still counted per address (so a
// throttled one still gets 429) but nothing is logged.
func TestAuthLimiterGlobalBudget(t *testing.T) {
	clk := newFakeClock()
	l := newAuthLimiter(clk)
	l.budget, l.globalBudget, l.window = 3, 5, time.Minute

	logged := 0
	var tripped int
	for i := range 20 {
		v := l.fail("addr-" + strconv.Itoa(i))
		if v.log {
			logged++
		}
		if v.trippedGlobal {
			tripped++
			if i != 4 {
				t.Fatalf("global budget tripped on failure %d, want the 5th", i+1)
			}
		}
	}
	if logged != 5 || tripped != 1 {
		t.Fatalf("logged %d (want 5), tripped %d (want 1)", logged, tripped)
	}
	// Per-address accounting still runs while the global budget is
	// spent: an address that exhausts its own is throttled, silently.
	for range 3 {
		l.fail("loud")
	}
	if v := l.fail("loud"); !v.throttled || v.log {
		t.Fatalf("per-address throttle under a spent global budget: %+v", v)
	}
	// The global window slides too.
	clk.Advance(time.Minute + time.Second)
	if v := l.fail("later"); !v.log {
		t.Fatalf("global budget did not refill: %+v", v)
	}
}

// The table is bounded however many addresses a flood comes from:
// drained addresses go first, then arbitrary ones, and a fresh address
// is always admitted.
func TestAuthLimiterBoundsTrackedAddresses(t *testing.T) {
	clk := newFakeClock()
	l := newAuthLimiter(clk)
	l.maxKeys = 8
	for i := range 80 {
		l.fail("addr-" + strconv.Itoa(i))
		if l.size() > l.maxKeys {
			t.Fatalf("tracking %d addresses, max %d", l.size(), l.maxKeys)
		}
	}
	// With every tracked address drained, room is made by the sweep
	// alone and the survivor is the fresh one.
	clk.Advance(l.window + time.Second)
	l.fail("fresh")
	if l.size() != 1 {
		t.Fatalf("drained addresses survived the sweep: %d tracked", l.size())
	}
}

func TestClientKey(t *testing.T) {
	req := func(remote string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/demo", nil)
		r.RemoteAddr = remote
		return r
	}
	for remote, want := range map[string]string{
		"203.0.113.9:4242":           "203.0.113.9",
		"[2001:db8:1:2:3:4:5:6]:443": "2001:db8:1:2::/64", // one /64, one client
		"[2001:db8:1:2:9:9:9:9]:80":  "2001:db8:1:2::/64",
		"[::ffff:203.0.113.9]:1":     "203.0.113.9", // mapped v4 is v4
		"not-an-address":             "not-an-address",
	} {
		if got := clientKey(req(remote)); got != want {
			t.Errorf("clientKey(%q) = %q, want %q", remote, got, want)
		}
	}
	// Caddy's client_ip (the forwarded address behind a trusted proxy)
	// wins over the peer.
	r := req("10.0.0.1:1")
	vars := map[string]any{caddyhttp.ClientIPVarKey: "198.51.100.7"}
	r = r.WithContext(context.WithValue(r.Context(), caddyhttp.VarsCtxKey, vars))
	if got := clientKey(r); got != "198.51.100.7" {
		t.Errorf("client_ip var not honoured: %q", got)
	}
}
