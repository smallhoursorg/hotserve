package liveswap

import (
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// The webhook's auth-failure throttle. Token forgery is infeasible (no
// private key on the box), so failed authentications are not a
// guessing oracle; what an unauthenticated caller can do with them is
// make hotserve write a Warn per request, without bound — a
// journal-fill primitive. The throttle bounds that: an address gets
// authFailBudget failures per authFailWindow and is then answered 429
// until its oldest failure ages out, and the process as a whole logs
// at most authFailGlobalBudget failures per window however many
// addresses a flood comes from. The address budget counts failures
// and decides the 429; the process budget decides what is logged —
// every line, the once-per-window "budget spent" lines included, so
// under a spent process budget an address is throttled silently. The
// token is still verified for a
// throttled address — a valid one is admitted and clears the address
// — so nobody sharing an address with a flood (a NAT, a CI egress
// pool, a proxy in front without trusted_proxies) is ever locked out
// of deploying; the verification's cost is comparable to the TLS
// handshake the request already paid and is not what the throttle is
// for. Fixed, not configurable: a legitimate deployer fails a handful
// of times while setting up, never ten times in a minute.
const (
	authFailBudget       = 10
	authFailGlobalBudget = 100
	authFailWindow       = time.Minute
	// authKeysMax bounds the table so a flood from many addresses
	// cannot grow memory. Past it, addresses whose window has drained
	// are swept (at most once per window — the sweep is O(table) and
	// attacker-triggered) and then an arbitrary one is dropped. A
	// dropped address is re-admitted with a fresh budget; the global
	// budget is what holds above the table size.
	authKeysMax = 4096
)

// webhookAuthLimiter is the process-wide instance every webhook
// handler shares: a config reload must not hand a throttled address a
// fresh budget, and two mounts must not double it. It has no goroutine
// and so no lifecycle.
var webhookAuthLimiter = newAuthLimiter(realClock{})

// authLimiter is a sliding window of logged authentication failures
// per client address, plus one for the process. Expiry happens on the
// touches themselves; there is nothing to clean up.
type authLimiter struct {
	budget       int
	globalBudget int
	maxKeys      int
	window       time.Duration
	clock        clock

	mu        sync.Mutex
	keys      map[string]*failWindow
	global    failWindow
	lastSweep time.Time
}

// failWindow is the failures logged inside the window, oldest first,
// and whether the line saying the budget is spent has been written
// for this window (the window drains before it is written again).
type failWindow struct {
	times   []time.Time
	tripped bool
}

func newAuthLimiter(c clock) *authLimiter {
	return &authLimiter{
		budget: authFailBudget, globalBudget: authFailGlobalBudget, maxKeys: authKeysMax,
		window: authFailWindow, clock: c, keys: map[string]*failWindow{},
	}
}

// failVerdict is what the handler does with one failed authentication.
type failVerdict struct {
	// throttled: the address has spent its budget — answer 429 with
	// retryAfter, and write nothing.
	throttled  bool
	retryAfter time.Duration
	// log: this failure gets its Warn line (inside both budgets).
	log bool
	// trippedKey / trippedGlobal: this failure spent the last of the
	// address's / the process's budget — say so, once per window.
	trippedKey    bool
	trippedGlobal bool
}

// fail records a failed authentication for key and says what to do
// with it. One lock scope: concurrent failures from one address
// cannot each see room and all be logged.
func (l *authLimiter) fail(key string) failVerdict {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()

	fw := l.keys[key]
	if fw == nil {
		l.makeRoomLocked(now)
		fw = &failWindow{}
		l.keys[key] = fw
	}
	fw.times = withinWindow(fw.times, now, l.window)
	if len(fw.times) >= l.budget {
		// Over budget: not recorded, so the window is exactly the
		// budgeted failures and retryAfter is when the oldest ages out.
		return failVerdict{throttled: true, retryAfter: fw.times[0].Add(l.window).Sub(now)}
	}
	if len(fw.times) == 0 {
		fw.tripped = false
	}
	fw.times = append(fw.times, now)

	l.global.times = withinWindow(l.global.times, now, l.window)
	if len(l.global.times) >= l.globalBudget {
		return failVerdict{} // 401, silently: the process's budget is spent
	}
	if len(l.global.times) == 0 {
		l.global.tripped = false
	}
	l.global.times = append(l.global.times, now)

	v := failVerdict{log: true}
	if len(fw.times) == l.budget && !fw.tripped {
		fw.tripped = true
		v.trippedKey = true
	}
	if len(l.global.times) == l.globalBudget && !l.global.tripped {
		l.global.tripped = true
		v.trippedGlobal = true
	}
	return v
}

// clear forgets key: an authentication that succeeded.
func (l *authLimiter) clear(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.keys, key)
}

// size is the number of addresses tracked, for the tests.
func (l *authLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.keys)
}

// makeRoomLocked keeps the table under maxKeys before a new address is
// added: a sweep of drained windows, at most once per window, then an
// arbitrary address if the table is still full.
func (l *authLimiter) makeRoomLocked(now time.Time) {
	if len(l.keys) < l.maxKeys {
		return
	}
	if now.Sub(l.lastSweep) >= l.window {
		l.lastSweep = now
		for k, fw := range l.keys {
			if fw.times = withinWindow(fw.times, now, l.window); len(fw.times) == 0 {
				delete(l.keys, k)
			}
		}
	}
	for k := range l.keys {
		if len(l.keys) < l.maxKeys {
			return
		}
		delete(l.keys, k)
	}
}

// withinWindow drops the timestamps that are window or more before
// now, in place; times must be ascending.
func withinWindow(times []time.Time, now time.Time, window time.Duration) []time.Time {
	i := 0
	for i < len(times) && now.Sub(times[i]) >= window {
		i++
	}
	return times[i:]
}

// clientKey is the address a request is throttled under: Caddy's
// client_ip (the peer, or the forwarded address when the peer is a
// configured trusted proxy), falling back to the peer address. IPv6
// is keyed by /64 — one host owns the whole prefix, so per-address
// keys would make the throttle free to bypass. Anything unparseable
// (a unix-socket listener's peer, say) is keyed as the string it is,
// which shares one budget among everyone behind it; sharing costs
// only log lines, never a deploy (see the package comment above).
func clientKey(r *http.Request) string {
	addr := r.RemoteAddr
	if v, ok := caddyhttp.GetVar(r.Context(), caddyhttp.ClientIPVarKey).(string); ok && v != "" {
		addr = v
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		addr = host
	}
	ip, err := netip.ParseAddr(addr)
	if err != nil {
		return addr
	}
	ip = ip.Unmap()
	if ip.Is6() {
		prefix, _ := ip.Prefix(64)
		return prefix.String()
	}
	return ip.String()
}
