package penaltybox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// serveReq runs one request through h.ServeHTTP with a Caddy replacer in
// context (as caddyhttp does for real requests). clientKey is exposed as
// the {test.client} placeholder.
func serveReq(t *testing.T, h *Handler, clientKey string, next caddyhttp.Handler) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	repl := caddy.NewReplacer()
	repl.Set("test.client", clientKey)
	req = req.WithContext(context.WithValue(req.Context(), caddy.ReplacerCtxKey, repl))
	rec := httptest.NewRecorder()
	if err := h.ServeHTTP(rec, req, next); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}
	return rec
}

func levelNext(level string) caddyhttp.Handler {
	return caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		if level != "" {
			w.Header().Set("X-Rate-Limit-Level", level)
		}
		w.WriteHeader(http.StatusOK)
		_, err := w.Write([]byte("ok"))
		return err
	})
}

func TestServeHTTPBoxesAndRejects(t *testing.T) {
	clk := newFakeClock()
	h, _ := newTestHandler(clk, storeConfig{limit: 5, penaltyTTL: 10 * time.Second})
	h.Key = "{test.client}"
	h.Status = http.StatusTooManyRequests

	// Two level-3 responses (6 units) cross limit 5.
	for i := 0; i < 2; i++ {
		rec := serveReq(t, h, "client-a", levelNext("3"))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d should pass, got %d", i, rec.Code)
		}
		if rec.Header().Get("X-Rate-Limit-Level") != "" {
			t.Fatal("hint header must be stripped from counted responses")
		}
	}

	// Now boxed: enforced before the next handler runs.
	nextCalled := false
	rec := serveReq(t, h, "client-a", caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		nextCalled = true
		return nil
	}))
	if nextCalled {
		t.Fatal("boxed request must not reach the next handler")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rec.Code)
	}
	ra, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || ra < 1 || ra > 10 {
		t.Fatalf("Retry-After must be an integer in (0, ttl], got %q", rec.Header().Get("Retry-After"))
	}

	// Mid-box probe gets a smaller, honest Retry-After.
	clk.Advance(7 * time.Second)
	rec = serveReq(t, h, "client-a", levelNext(""))
	if got := rec.Header().Get("Retry-After"); got != "3" {
		t.Fatalf("expected honest remaining Retry-After 3, got %q", got)
	}

	// After expiry, traffic flows again.
	clk.Advance(4 * time.Second)
	rec = serveReq(t, h, "client-a", levelNext("1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 after box expiry, got %d", rec.Code)
	}
}

func TestServeHTTPBoxedResponseStripsPreexistingHint(t *testing.T) {
	clk := newFakeClock()
	h, _ := newTestHandler(clk, storeConfig{limit: 5, penaltyTTL: time.Minute})
	h.Key = "{test.client}"
	h.Status = http.StatusTooManyRequests

	serveReq(t, h, "client-a", levelNext("3"))
	serveReq(t, h, "client-a", levelNext("3")) // boxed now

	// Simulate earlier middleware having already set the hint header
	// before this handler runs: the boxed 429 must still strip it.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	repl := caddy.NewReplacer()
	repl.Set("test.client", "client-a")
	req = req.WithContext(context.WithValue(req.Context(), caddy.ReplacerCtxKey, repl))
	rec := httptest.NewRecorder()
	rec.Header().Set("X-Rate-Limit-Level", "3")
	if err := h.ServeHTTP(rec, req, levelNext("")); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rec.Code)
	}
	if got := rec.Header().Get("X-Rate-Limit-Level"); got != "" {
		t.Errorf("boxed 429 must strip a pre-set hint header, got %q", got)
	}
}

func TestServeHTTPClientIsolation(t *testing.T) {
	clk := newFakeClock()
	h, _ := newTestHandler(clk, storeConfig{limit: 5, penaltyTTL: time.Minute})
	h.Key = "{test.client}"
	h.Status = http.StatusTooManyRequests

	serveReq(t, h, "client-a", levelNext("3"))
	serveReq(t, h, "client-a", levelNext("3"))
	if rec := serveReq(t, h, "client-a", levelNext("1")); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("client-a should be boxed, got %d", rec.Code)
	}
	if rec := serveReq(t, h, "client-b", levelNext("3")); rec.Code != http.StatusOK {
		t.Fatalf("boxing client-a must not affect client-b, got %d", rec.Code)
	}
}

func TestServeHTTPLevel1NeverAllocates(t *testing.T) {
	h, st := newTestHandler(newFakeClock(), storeConfig{})
	h.Key = "{test.client}"

	for i := 0; i < 50; i++ {
		serveReq(t, h, "client-a", levelNext("1"))
		serveReq(t, h, "client-b", levelNext(""))
	}
	if got := st.size(); got != 0 {
		t.Fatalf("level-1-only traffic must never allocate counters, got %d", got)
	}
}

func TestServeHTTPEmptyKeyFailsOpen(t *testing.T) {
	h, _ := newTestHandler(newFakeClock(), storeConfig{})
	h.Key = "{test.missing}" // resolves to empty

	rec := serveReq(t, h, "unused", levelNext("3"))
	if rec.Code != http.StatusOK {
		t.Fatalf("unresolvable key must fail open, got %d", rec.Code)
	}
}

func TestMaskKey(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"ipv4 per address", "192.0.2.1", "192.0.2.1"},
		{"ipv4 other address", "192.0.2.2", "192.0.2.2"},
		{"ipv6 to its /64", "2001:db8:1:2:3:4:5:6", "2001:db8:1:2::/64"},
		{"ipv6 other host same /64", "2001:db8:1:2:ffff:ffff:ffff:ffff", "2001:db8:1:2::/64"},
		{"ipv6 uppercase canonicalised", "2001:DB8:1:2::1", "2001:db8:1:2::/64"},
		{"ipv6 loopback", "::1", "::/64"},
		{"ipv4-mapped ipv6 unmapped, per address", "::ffff:192.0.2.1", "192.0.2.1"},
		{"ipv4-mapped hex form", "::ffff:c000:201", "192.0.2.1"},
		{"ipv4-mapped with zone", "::ffff:192.0.2.1%eth0", "192.0.2.1"},
		{"nat64 well-known prefix keys the embedded ipv4", "64:ff9b::192.0.2.1", "192.0.2.1"},
		{"nat64 well-known hex form", "64:ff9b::c000:201", "192.0.2.1"},
		{"nat64 well-known uppercase with zone", "64:FF9B::192.0.2.1%eth0", "192.0.2.1"},
		{"nat64 /64 but outside the /96", "64:ff9b::1:c000:201", "64:ff9b::/64"},
		{"nat64 local-use prefix is not recognised", "64:ff9b:1::c000:201", "64:ff9b:1::/64"},
		{"teredo is not recognised", "2001:0:4136:e378:8000:63bf:3fff:fdd2", "2001:0:4136:e378::/64"},
		{"zoned link-local drops the zone", "fe80::1%eth0", "fe80::/64"},
		{"zoned link-local other zone, same key", "fe80::2%eth1", "fe80::/64"},
		{"empty", "", ""},
		{"header value", "Bearer abc", "Bearer abc"},
		{"composite key stays per address", "2001:db8:1:2::1|example.com", "2001:db8:1:2::1|example.com"},
		{"host:port is not an address", "192.0.2.1:8080", "192.0.2.1:8080"},
		{"bracketed ipv6 is not an address", "[2001:db8::1]", "[2001:db8::1]"},
		{"prefix string is not an address", "2001:db8:1:2::/64", "2001:db8:1:2::/64"},
		{"leading zeros rejected", "192.000.2.1", "192.000.2.1"},
		{"padded", " 2001:db8::1", " 2001:db8::1"},
		{"garbage", "not-an-ip", "not-an-ip"},
		{"nul byte", "\x00", "\x00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := maskKey(c.in); got != c.want {
				t.Errorf("maskKey(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// An IPv6 client owns its whole /64, so the budget is the prefix's:
// a second address in the same /64 is boxed by the first one's
// offences. A neighbouring /64, and IPv4 addresses, keep their own.
func TestServeHTTPIPv6SharesPrefixBudget(t *testing.T) {
	clk := newFakeClock()
	h, _ := newTestHandler(clk, storeConfig{limit: 5, penaltyTTL: time.Minute})
	h.Key = "{test.client}"
	h.Status = http.StatusTooManyRequests

	box := func(client string) {
		t.Helper()
		serveReq(t, h, client, levelNext("3"))
		serveReq(t, h, client, levelNext("3")) // 6 units > limit 5
	}
	expect := func(client string, want int) {
		t.Helper()
		if rec := serveReq(t, h, client, levelNext("1")); rec.Code != want {
			t.Fatalf("%s: got %d, want %d", client, rec.Code, want)
		}
	}

	// Budget spread across a /64 still adds up: one level-3 response
	// from each of two addresses is the same client's 6 units.
	serveReq(t, h, "2001:db8:1:2::a", levelNext("3"))
	serveReq(t, h, "2001:db8:1:2::b", levelNext("3"))
	expect("2001:db8:1:2::a", http.StatusTooManyRequests)
	expect("2001:db8:1:2:dead:beef:0:1", http.StatusTooManyRequests)
	expect("2001:db8:1:3::a", http.StatusOK)

	box("192.0.2.1")
	expect("192.0.2.1", http.StatusTooManyRequests)
	expect("::ffff:192.0.2.1", http.StatusTooManyRequests)   // the same IPv4 client, mapped
	expect("64:ff9b::192.0.2.1", http.StatusTooManyRequests) // ... and through a NAT64 translator
	expect("192.0.2.2", http.StatusOK)
	expect("::ffff:192.0.2.2", http.StatusOK)
	expect("64:ff9b::192.0.2.2", http.StatusOK)
}

func TestValidate(t *testing.T) {
	valid := Handler{
		Header:     "X-Rate-Limit-Level",
		MinLevel:   2,
		Window:     caddy.Duration(time.Minute),
		Limit:      30,
		PenaltyTTL: caddy.Duration(5 * time.Minute),
		Status:     429,
		MaxKeys:    1000,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	mutations := []struct {
		name   string
		mutate func(*Handler)
	}{
		{"empty header", func(h *Handler) { h.Header = "" }},
		{"negative window", func(h *Handler) { h.Window = caddy.Duration(-time.Second) }},
		{"negative ttl", func(h *Handler) { h.PenaltyTTL = caddy.Duration(-time.Second) }},
		{"zero limit", func(h *Handler) { h.Limit = -1 }},
		{"min_level 0", func(h *Handler) { h.MinLevel = 0 }},
		{"min_level 4", func(h *Handler) { h.MinLevel = 4 }},
		{"status 200", func(h *Handler) { h.Status = 200 }},
		{"status 600", func(h *Handler) { h.Status = 600 }},
		{"negative max_keys", func(h *Handler) { h.MaxKeys = -5 }},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			bad := valid
			m.mutate(&bad)
			if err := bad.Validate(); err == nil {
				t.Error("expected validation error")
			}
		})
	}
}

// A max_keys below the shard count loads (a refusal would make Caddy
// refuse a config that works today) and tracks one client per shard.
func TestProvisionSmallMaxKeysLoads(t *testing.T) {
	ctx, cancel := caddy.NewContext(caddy.Context{Context: t.Context()})
	defer cancel()

	h := Handler{MaxKeys: 50}
	if err := h.Provision(ctx); err != nil {
		t.Fatalf("max_keys 50 must provision: %v", err)
	}
	defer func() {
		if err := h.Cleanup(); err != nil {
			t.Errorf("Cleanup: %v", err)
		}
	}()
	if err := h.Validate(); err != nil {
		t.Fatalf("max_keys 50 must validate: %v", err)
	}
	if got := h.store.(*store).maxPerShard; got != 1 {
		t.Errorf("max_keys 50: %d keys per shard, want 1", got)
	}
}

func TestWarnSmallMaxKeys(t *testing.T) {
	cases := []struct {
		maxKeys   int
		effective int // 0 = no warning
	}{
		{1, numShards},
		{50, numShards},
		{numShards - 1, numShards},
		{numShards, 0},
		{1000, 0},
		{100_000, 0},
		{0, 0},  // Validate refuses it; no warning on top
		{-5, 0}, // likewise
	}
	for _, tc := range cases {
		core, logs := observer.New(zap.WarnLevel)
		warnSmallMaxKeys(zap.New(core), tc.maxKeys)
		got := logs.All()
		if tc.effective == 0 {
			if len(got) != 0 {
				t.Errorf("max_keys %d: want no warning, got %v", tc.maxKeys, got)
			}
			continue
		}
		if len(got) != 1 {
			t.Fatalf("max_keys %d: want one warning, got %v", tc.maxKeys, got)
		}
		fields := got[0].ContextMap()
		if got[0].Level != zap.WarnLevel || fields["max_keys"] != int64(tc.maxKeys) ||
			fields["effective_max_keys"] != int64(tc.effective) || fields["shards"] != int64(numShards) {
			t.Errorf("max_keys %d: warning = %q %v, want level warn, max_keys %d, effective_max_keys %d, shards %d",
				tc.maxKeys, got[0].Message, fields, tc.maxKeys, tc.effective, numShards)
		}
	}
	warnSmallMaxKeys(nil, 50) // a nil logger is skipped, not dereferenced
}

// BenchmarkUnboxedRequestPhase measures the hot path: an unboxed key's
// request-phase check. Informational per the design doc ("benchmark, not
// a hard gate") — expect 0 allocs/op.
func BenchmarkUnboxedRequestPhase(b *testing.B) {
	s := testStore(realClock{}, storeConfig{})
	s.add("203.0.113.7", 2) // existing but unboxed entry
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.boxedRemaining("203.0.113.7")
	}
}
