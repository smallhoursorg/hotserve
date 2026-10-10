package deploytrust

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
// FailBudget failures per FailWindow and is then answered 429
// until its oldest failure ages out, and the process as a whole logs
// at most FailGlobalBudget failures per window however many
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
//
// A failure that is the box's — a trust source it could not consult
// (unavailable, which says why) — is charged like any other and gets
// a line of its own once per window per source (outage), outside
// both budgets: sources are operator config, so that line's bound is
// not the caller's to grow.
const (
	FailBudget       = 10
	FailGlobalBudget = 100
	FailWindow       = time.Minute
	// keysMax bounds the table so a flood from many addresses
	// cannot grow memory. Past it, addresses whose window has drained
	// are swept (at most once per window — the sweep is O(table) and
	// attacker-triggered) and then an arbitrary one is dropped. A
	// dropped address is re-admitted with a fresh budget; the global
	// budget is what holds above the table size.
	keysMax = 4096
)

// shared is the process-wide instance every webhook
// handler shares: a config reload must not hand a throttled address a
// fresh budget, and two mounts must not double it. It has no goroutine
// and so no lifecycle.
var shared = NewLimiter(systemClock{})

// Shared is the process-wide limiter: what liveswap's webhook and the
// box's authenticate on (Limiter.Authenticate), so one address has one
// budget whichever webhook a flood aims at. Tests build their own with
// NewLimiter on a clock they advance.
func Shared() *Limiter { return shared }

// Clock is what the limiter reads time from: the system clock in
// production, a hand-advanced one in tests.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// Limiter is a sliding window of logged authentication failures
// per client address, plus one for the process. Expiry happens on the
// touches themselves; there is nothing to clean up.
type Limiter struct {
	budget       int
	globalBudget int
	maxKeys      int
	window       time.Duration
	clock        Clock

	mu        sync.Mutex
	keys      map[string]*failWindow
	global    failWindow
	lastSweep time.Time
	// outages is when each source that could not be consulted was
	// last logged, by label and by the caller's scope key: both
	// config, so it cannot grow with traffic; labels a reload retired
	// are dropped as their window drains.
	outages map[outageKey]time.Time
}

// failWindow is the failures logged inside the window, oldest first,
// and whether the line saying the budget is spent has been written
// for this window (the window drains before it is written again).
type failWindow struct {
	times   []time.Time
	tripped bool
}

// NewLimiter is a limiter on its own clock, with the production
// budgets (FailBudget, FailGlobalBudget, FailWindow): for tests, which
// advance the clock. Production authenticates on Shared.
func NewLimiter(c Clock) *Limiter {
	return &Limiter{
		budget: FailBudget, globalBudget: FailGlobalBudget, maxKeys: keysMax,
		window: FailWindow, clock: c, keys: map[string]*failWindow{},
		outages: map[outageKey]time.Time{},
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
func (l *Limiter) fail(key string) failVerdict {
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

// outageKey is one source as one caller met it: the caller's scope
// key (liveswap's "app", the box's "box_request") is a constant of the
// caller, so the table stays config-sized.
type outageKey struct{ label, scope string }

// outage records that the source label could not be consulted and
// says whether to log it: once per window per source per caller,
// outside both budgets, so an operator reading one webhook's lines
// sees the outage whichever webhook met it first. It touches no
// address; the request is charged by fail as usual. The table is swept
// of drained entries on each write, which is O(entries) — configured
// ones, not the caller's to grow.
func (l *Limiter) outage(label, scope string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	k := outageKey{label, scope}
	if last, ok := l.outages[k]; ok && now.Sub(last) < l.window {
		return false
	}
	for k, last := range l.outages {
		if now.Sub(last) >= l.window {
			delete(l.outages, k)
		}
	}
	l.outages[k] = now
	return true
}

// clear forgets key: an authentication that succeeded.
func (l *Limiter) clear(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.keys, key)
}

// Size is the number of addresses holding failures in their window:
// what a flood costs in memory, and what the tests read.
func (l *Limiter) Size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.keys)
}

// makeRoomLocked keeps the table under maxKeys before a new address is
// added: a sweep of drained windows, at most once per window, then an
// arbitrary address if the table is still full.
func (l *Limiter) makeRoomLocked(now time.Time) {
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
