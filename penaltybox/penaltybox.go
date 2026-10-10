// Package penaltybox is the Caddy counterpart to the Fastly Edge Rate
// Limiting and HAProxy stick-table consumption recipes for the CMS
// rate-limit hint header (X-Rate-Limit-Level: 1|2|3, absence = 1).
//
// Response phase: each origin response's hint level is added, weighted,
// to a per-client sliding-window budget, and the header is stripped
// before reaching the client. Crossing the budget puts the client in a
// penalty box. Request phase: boxed clients get 429 + Retry-After
// before the request reaches the origin.
package penaltybox

import (
	"fmt"
	"net/http"
	"net/netip"
	"net/textproto"
	"strconv"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(Handler{})
}

// Handler implements the hint penalty box middleware.
type Handler struct {
	// Header is the origin response header carrying the hint level.
	// Default "X-Rate-Limit-Level" (the CMS wire contract).
	Header string `json:"header,omitempty"`

	// Key identifies the client. Default "{client_ip}", which respects
	// the server's trusted_proxies configuration — XFF trust is the
	// server config's job, not this module's. A key whose whole value
	// resolves to a single IPv6 address is counted under its /64 (see
	// maskKey); any other value is used verbatim.
	Key string `json:"key,omitempty"`

	// MinLevel is the lowest hint level that counts toward the budget.
	// Default 2, so level-1 (normal) traffic costs nothing to track.
	MinLevel int `json:"min_level,omitempty"`

	// Window is the sliding window over which weighted units accumulate.
	// Default 60s. Free-form; Fastly's 1s/10s/60s are the documented
	// convention.
	Window caddy.Duration `json:"window,omitempty"`

	// Limit is the weighted-unit budget per window; exceeding it (strictly)
	// boxes the client. Default 30.
	Limit int `json:"limit,omitempty"`

	// PenaltyTTL is how long a boxed client stays boxed. Fixed — traffic
	// during the box does not extend it. Default 5m.
	PenaltyTTL caddy.Duration `json:"penalty_ttl,omitempty"`

	// Strip removes the hint header before the response reaches the
	// client. Default true.
	Strip *bool `json:"strip,omitempty"`

	// Status is the response code for boxed clients. Default 429.
	Status int `json:"status,omitempty"`

	// MaxKeys caps tracked clients; beyond it, oldest-idle entries are
	// evicted. Default 100000.
	MaxKeys int `json:"max_keys,omitempty"`

	// Tiers gives a level its own budget, separate from the default
	// window/limit/penalty_ttl. Keyed by hint level ("2", "3"). Within a
	// tier each response costs 1 (not `level` units — weighting only
	// matters when levels share a budget), so `limit` is a plain
	// response count. A counted level without its own tier uses the
	// nearest configured tier below it, else the default budget (which
	// keeps the original weighted semantics). Omitted tier fields
	// inherit the top-level window/limit/penalty_ttl.
	Tiers map[int]TierConfig `json:"tiers,omitempty"`

	store       boxStore
	headerCanon string
	stripOn     bool
	logger      *zap.Logger
}

// TierConfig is a per-level budget (see Handler.Tiers).
type TierConfig struct {
	// Window is the sliding window for this tier.
	Window caddy.Duration `json:"window,omitempty"`

	// Limit is the number of responses at this tier's level allowed per
	// window; exceeding it (strictly) boxes the client.
	Limit int `json:"limit,omitempty"`

	// PenaltyTTL is how long this tier's box lasts.
	PenaltyTTL caddy.Duration `json:"penalty_ttl,omitempty"`
}

// CaddyModule returns the Caddy module information.
func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.hint_penaltybox",
		New: func() caddy.Module { return new(Handler) },
	}
}

// Provision sets up defaults and the counter store.
func (h *Handler) Provision(ctx caddy.Context) error {
	if h.Header == "" {
		h.Header = "X-Rate-Limit-Level"
	}
	if h.Key == "" {
		// The canonical form of the {client_ip} Caddyfile shorthand
		// (per httpcaddyfile/shorthands.go): it must be spelled out here
		// because shorthand rewriting only happens during Caddyfile
		// adaptation, not at provision time.
		h.Key = "{http.vars.client_ip}"
	}
	if h.MinLevel == 0 {
		h.MinLevel = 2
	}
	if h.Window == 0 {
		h.Window = caddy.Duration(60 * time.Second)
	}
	if h.Limit == 0 {
		h.Limit = 30
	}
	if h.PenaltyTTL == 0 {
		h.PenaltyTTL = caddy.Duration(5 * time.Minute)
	}
	if h.Status == 0 {
		h.Status = http.StatusTooManyRequests
	}
	if h.MaxKeys == 0 {
		h.MaxKeys = 100_000
	}
	// Tier fields inherit the top-level values when omitted.
	for level, t := range h.Tiers {
		if t.Window == 0 {
			t.Window = h.Window
		}
		if t.Limit == 0 {
			t.Limit = h.Limit
		}
		if t.PenaltyTTL == 0 {
			t.PenaltyTTL = h.PenaltyTTL
		}
		h.Tiers[level] = t
	}
	h.stripOn = h.Strip == nil || *h.Strip
	h.headerCanon = textproto.CanonicalMIMEHeaderKey(h.Header)
	h.logger = ctx.Logger()

	tiers := make(map[int]tierSpec, len(h.Tiers))
	for level, t := range h.Tiers {
		tiers[level] = tierSpec{
			window:     time.Duration(t.Window),
			limit:      t.Limit,
			penaltyTTL: time.Duration(t.PenaltyTTL),
		}
	}
	st := newStore(storeConfig{
		window:     time.Duration(h.Window),
		limit:      h.Limit,
		penaltyTTL: time.Duration(h.PenaltyTTL),
		maxKeys:    h.MaxKeys,
		tiers:      tiers,
	}, realClock{})
	st.startSweeper()
	h.store = st
	return nil
}

// Validate enforces semantic invariants (runs for JSON and Caddyfile
// config alike, after Provision has applied defaults).
func (h *Handler) Validate() error {
	if h.Header == "" {
		return fmt.Errorf("header must not be empty")
	}
	if h.Window <= 0 {
		return fmt.Errorf("window must be positive, got %v", time.Duration(h.Window))
	}
	if h.PenaltyTTL <= 0 {
		return fmt.Errorf("penalty_ttl must be positive, got %v", time.Duration(h.PenaltyTTL))
	}
	if h.Limit <= 0 {
		return fmt.Errorf("limit must be positive, got %d", h.Limit)
	}
	if h.MinLevel < 1 || h.MinLevel > 3 {
		return fmt.Errorf("min_level must be 1, 2, or 3, got %d", h.MinLevel)
	}
	if h.Status < 400 || h.Status > 599 {
		return fmt.Errorf("status must be a 4xx or 5xx code, got %d", h.Status)
	}
	if h.MaxKeys <= 0 {
		return fmt.Errorf("max_keys must be positive, got %d", h.MaxKeys)
	}
	for level, t := range h.Tiers {
		if level < 1 || level > maxLevel {
			return fmt.Errorf("tier level must be 1, 2, or 3, got %d", level)
		}
		if level < h.MinLevel {
			return fmt.Errorf("tier %d is below min_level %d and would never count", level, h.MinLevel)
		}
		if t.Window <= 0 {
			return fmt.Errorf("tier %d: window must be positive, got %v", level, time.Duration(t.Window))
		}
		if t.Limit <= 0 {
			return fmt.Errorf("tier %d: limit must be positive, got %d", level, t.Limit)
		}
		if t.PenaltyTTL <= 0 {
			return fmt.Errorf("tier %d: penalty_ttl must be positive, got %v", level, time.Duration(t.PenaltyTTL))
		}
	}
	return nil
}

// Cleanup stops the store's background sweeper (called on config unload).
func (h *Handler) Cleanup() error {
	if h.store != nil {
		h.store.stop()
	}
	return nil
}

// ServeHTTP enforces the box on the request phase and counts hint levels
// on the response phase.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
	key := repl.ReplaceAll(h.Key, "")
	if key == "" {
		// No resolvable key — fail open rather than box the world.
		return next.ServeHTTP(w, r)
	}
	key = maskKey(key)

	if remaining, boxed := h.store.boxedRemaining(key); boxed {
		// Strip here too: earlier middleware may already have set the
		// hint header, and the contract is that it never reaches the
		// client on any response path — boxed responses included.
		if h.stripOn {
			delete(w.Header(), h.headerCanon)
		}
		// Written directly rather than via caddyhttp.Error so that
		// Retry-After survives any handle_errors routes.
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(remaining)))
		w.WriteHeader(h.Status)
		return nil
	}

	rw := &hintInterceptor{ResponseWriter: w, handler: h, key: key}
	err := next.ServeHTTP(rw, r)
	rw.finalize()
	return err
}

// retryAfterSeconds is the ceiling of the remaining box TTL in seconds
// (minimum 1) — a client probing mid-box gets an honest number, not the
// configured TTL.
func retryAfterSeconds(remaining time.Duration) int {
	secs := int((remaining + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return secs
}

// nat64WellKnown is the RFC 6052 well-known NAT64 prefix: an address in
// it is an IPv4 client seen through a translator, with the IPv4 address
// in the last four bytes.
var nat64WellKnown = netip.MustParsePrefix("64:ff9b::/96")

// maskKey is the store key for a resolved key value. It looks only at
// the whole resolved value, whatever placeholder produced it
// ({client_ip}, or a header such as CF-Connecting-IP): if that value
// parses as exactly one IP address it is masked, and anything else — a
// composite such as "{client_ip}|{host}", a host:port such as {remote}
// gives — is returned unchanged, since only the operator knows its
// shape.
//
// An IPv6 address becomes its /64 prefix ("2001:db8:1:2::/64"): one
// host is routinely handed a whole /64, so per-address budgets would let
// an IPv6 client walk past the box by changing its low 64 bits. A
// link-local address's zone goes with the host bits. IPv4 is keyed per
// address, and so are the IPv6 forms that carry one IPv4 client:
// IPv4-mapped (::ffff:a.b.c.d) and the well-known NAT64 prefix
// (64:ff9b::a.b.c.d) both key as the embedded IPv4 address, so a client
// gets the same key native or translated, and a translator's clients
// are not all folded into one /64. A network-specific NAT64 prefix and
// Teredo cannot be recognised and do fold their IPv4 clients together.
//
// The liveswap deploy throttle (deploytrust's clientKey) applies the
// same /64 rule; this is a copy, not an import, so penaltybox stays an
// independent module.
func maskKey(key string) string {
	ip, err := netip.ParseAddr(key)
	if err != nil {
		return key
	}
	if ip.Is4() {
		// ParseAddr accepts only canonical dotted-quad IPv4, so key
		// already equals ip.String() — skip the allocation.
		return key
	}
	ip = ip.Unmap()
	if ip.Is4() {
		return ip.String()
	}
	if nat64WellKnown.Contains(ip.WithZone("")) {
		b := ip.As16()
		return netip.AddrFrom4([4]byte(b[12:])).String()
	}
	prefix, _ := ip.Prefix(64) // cannot fail: a valid IPv6 address, 64 ≤ 128
	return prefix.String()
}

// Interface guards.
var (
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddy.Validator             = (*Handler)(nil)
	_ caddy.CleanerUpper          = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
)
