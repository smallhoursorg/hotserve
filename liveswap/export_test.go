package liveswap

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// The exported names are the handler's own functions. These tests hold
// each to its original, so a change that reaches liveswap's webhook
// reaches the box webhook the same way, and nothing reaches one alone.

// forgetTestAddress is the cleanup for a test that charged the
// process-wide limiter: the address, and the process window its
// failure was counted in, so a repeated run (-count) starts as a fresh
// process would.
func forgetTestAddress(key string) {
	l := webhookAuthLimiter
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.keys, key)
	l.global = failWindow{}
}

// charged reports whether the process-wide limiter holds a window for
// the address.
func charged(key string) bool {
	l := webhookAuthLimiter
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.keys[key]
	return ok
}

// oneAuthLine is the single `webhook auth failed` record the observer
// holds, taken.
func oneAuthLine(t *testing.T, logs *observer.ObservedLogs) observer.LoggedEntry {
	t.Helper()
	all := logs.TakeAll()
	if len(all) != 1 || all[0].Message != "webhook auth failed" {
		t.Fatalf("logged %d records, want the one auth-failed line: %+v", len(all), all)
	}
	return all[0]
}

// TestAuthenticateIsTheWebhookPreamble pins Authenticate to the
// preamble on the process-wide limiter: one refusal through it is
// charged there, answered with the flat 401 handler_test pins, and
// logged under the caller's field; an accepted token is who it is,
// with the claim the box binds a bundle to, and clears the address.
func TestAuthenticateIsTheWebhookPreamble(t *testing.T) {
	priv, pub := mustGenTestKey()
	vs := resolveVerifiers([]trustSource{localTrust(pub, "box")}, nil)
	core, logs := observer.New(zap.WarnLevel)
	logger := zap.New(core)
	// An address of this test's own on the process-wide limiter, which
	// no other test touches.
	const addr, key = "203.0.113.9:4242", "203.0.113.9"
	t.Cleanup(func() { forgetTestAddress(key) })
	request := func(token string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.RemoteAddr = addr
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return req
	}

	w := httptest.NewRecorder()
	who, ok, err := Authenticate(w, request("not-a-jwt"), vs, logger, "webhook", "box")
	if err != nil || ok || who.By != "" || w.Code != http.StatusUnauthorized || strings.TrimSpace(w.Body.String()) != flat401 {
		t.Fatalf("refused token: ok = %v, By = %q, err = %v, response = %d %s; want the flat 401", ok, who.By, err, w.Code, w.Body.String())
	}
	if !charged(key) {
		t.Fatalf("a refusal through Authenticate was not charged to the process-wide limiter")
	}
	fields := oneAuthLine(t, logs).ContextMap()
	if fields["webhook"] != "box" || fields["remote"] != key {
		t.Errorf("fields = %v; want the caller's field and the address", fields)
	}
	if refused, _ := fields["refused"].(string); !strings.HasPrefix(refused, "local:test-key: ") {
		t.Errorf("refused = %q, want the source's reason", refused)
	}

	const sha = "4f1c2a9d0e8b7c6a5f4e3d2c1b0a998877665544"
	w = httptest.NewRecorder()
	who, ok, err = Authenticate(w, request(mintTestToken(t, priv, "box", map[string]string{"sub": "ci", "sha": sha})), vs, logger, "webhook", "box")
	if err != nil || !ok || w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Fatalf("accepted token: ok = %v, err = %v, wrote %d %q; want ok and nothing written", ok, err, w.Code, w.Body.String())
	}
	if who.By != "local:test-key sub=ci" {
		t.Errorf("By = %q, want the attribution a deploy record carries", who.By)
	}
	if got, ok := who.Claim("sha"); !ok || got != sha {
		t.Errorf("Claim(sha) = %q, %v; want the token's", got, ok)
	}
	if got, ok := who.Claim("nope"); ok || got != "" {
		t.Errorf("Claim(nope) = %q, %v; want absent", got, ok)
	}
	if charged(key) {
		t.Errorf("an accepted token did not clear the address")
	}
	if logs.Len() != 0 {
		t.Errorf("an accepted token wrote %d journal lines, want none: %+v", logs.Len(), logs.All())
	}
}

// TestAuthenticateBudgetAndBound pins the preamble's budget on a
// clocked limiter — ten failures logged, the eleventh 429 with a
// Retry-After, the window draining it, a valid token clearing it —
// and the bound on the caller's field: one line, the length of a
// refusal, whatever the caller put in it; an app name with a control
// byte reads Go-quoted (the one visible change #188 made); no key is
// no field.
func TestAuthenticateBudgetAndBound(t *testing.T) {
	priv, pub := mustGenTestKey()
	vs := resolveVerifiers([]trustSource{localTrust(pub, "box")}, nil)
	core, logs := observer.New(zap.WarnLevel)
	logger := zap.New(core)
	clk := newFakeClock()
	l := newAuthLimiter(clk)
	call := func(token, key, value string) (bool, *httptest.ResponseRecorder) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.RemoteAddr = "203.0.113.10:4242"
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		_, ok, err := authenticate(w, req, vs, logger, l, key, value)
		if err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		return ok, w
	}

	for i := range authFailBudget {
		if ok, w := call("not-a-jwt", "webhook", "box"); ok || w.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: ok = %v, code = %d; want 401", i+1, ok, w.Code)
		}
	}
	if got := logs.TakeAll(); len(got) != authFailBudget+1 {
		t.Fatalf("logged %d records for %d failures; want each, and the line saying the budget is spent", len(got), authFailBudget)
	}
	if ok, w := call("not-a-jwt", "webhook", "box"); ok || w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != strconv.Itoa(int(authFailWindow.Seconds())) {
		t.Fatalf("past the budget: ok = %v, code = %d, Retry-After = %q; want 429 and the window", ok, w.Code, w.Header().Get("Retry-After"))
	}
	if logs.Len() != 0 {
		t.Fatalf("a throttled refusal was logged: %+v", logs.All())
	}
	clk.Advance(authFailWindow)
	if ok, w := call("not-a-jwt", "webhook", "box"); ok || w.Code != http.StatusUnauthorized {
		t.Fatalf("after the window: ok = %v, code = %d; want 401, the budget fresh", ok, w.Code)
	}
	if ok, w := call(mintTestToken(t, priv, "box", nil), "webhook", "box"); !ok || w.Code != http.StatusOK {
		t.Fatalf("a valid token: ok = %v, code = %d; want admitted", ok, w.Code)
	}
	if l.size() != 0 {
		t.Fatalf("a valid token left %d addresses charged, want none", l.size())
	}
	logs.TakeAll()

	// The bound: whatever the caller put in the field, one line of at
	// most a refusal's length.
	long := strings.Repeat("r", 2*maxRefusalLen) + "\n"
	call("not-a-jwt", "ref", long)
	if ref, _ := oneAuthLine(t, logs).ContextMap()["ref"].(string); len(ref) > maxRefusalLen+len("...") || strings.Contains(ref, "\n") || !strings.HasPrefix(ref, `"rrr`) {
		t.Errorf("ref = %d bytes %q; want one quoted line of at most %d bytes", len(ref), ref, maxRefusalLen)
	}

	// This webhook's app name, as ServeHTTP passes it: cut by
	// loggedAppName, and Go-quoted here when it carries a control byte.
	call("not-a-jwt", "app", loggedAppName("demo\n"))
	if app := oneAuthLine(t, logs).ContextMap()["app"]; app != strconv.QuoteToASCII("demo\n") {
		t.Errorf("app = %q, want the name Go-quoted", app)
	}

	// No key is no field: the line is the address and the reason.
	call("not-a-jwt", "", "")
	if fields := oneAuthLine(t, logs).ContextMap(); len(fields) != 2 || fields["remote"] == nil || fields["refused"] == nil {
		t.Errorf("fields = %v; want remote and refused alone", fields)
	}
}

// TestIdentityClaimRendersAsMatchClaimsCompares pins the one rendering
// rule: a number claim by its source digits, a bool as its word, a
// composite claim as no value at all.
func TestIdentityClaimRendersAsMatchClaimsCompares(t *testing.T) {
	priv, pub := mustGenTestKey()
	vs := resolveVerifiers([]trustSource{localTrust(pub, "box")}, nil)
	now := time.Now()
	m := claimMap("", "box", now, now.Add(5*time.Minute), map[string]string{"sub": "ci"})
	m["repository_id"] = 100000000
	m["roles"] = []string{"admin"}
	m["verified"] = true
	who, _, err := authorize(context.Background(), vs, signEdDSA(t, priv, m))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]struct {
		value string
		ok    bool
	}{
		"repository_id": {"100000000", true},
		"verified":      {"true", true},
		"sub":           {"ci", true},
		"roles":         {"", false},
		"absent":        {"", false},
	} {
		if got, ok := who.Claim(name); got != want.value || ok != want.ok {
			t.Errorf("Claim(%s) = %q, %v; want %q, %v", name, got, ok, want.value, want.ok)
		}
	}
}

// TestNewTrustIsProvisionsTrustWiring pins NewTrust to the wiring
// Provision calls (trustVerifiers): the placeholder resolution, the
// validation (the same refusal, word for word), the https-only JWKS
// client unless allowInsecure, verifiers that accept what the source
// accepts — and to the two things it does on its own: the caller's
// config is left as parsed, and no source is a config error.
func TestNewTrustIsProvisionsTrustWiring(t *testing.T) {
	ctx := context.Background()
	priv, pub := mustGenTestKey()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "deploy.pub")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}

	// A local source whose audience and subject are placeholders:
	// resolved, as Provision resolves them, so a token for the values
	// is accepted and one for the placeholders' own text is not — and
	// resolved on a copy, so the config still says {env.NAME}.
	t.Setenv("HOTSERVE_TEST_AUDIENCE", "box")
	t.Setenv("HOTSERVE_TEST_SUBJECT", "ci")
	cfgs := []TrustConfig{{Kind: "local", PublicKey: keyPath, Audience: "{env.HOTSERVE_TEST_AUDIENCE}", Claims: map[string]string{"sub": "{env.HOTSERVE_TEST_SUBJECT}"}}}
	vs, err := NewTrust(cfgs, false)
	if err != nil || len(vs) != 1 || vs[0].label() != "local:"+keyPath {
		t.Fatalf("NewTrust(local) = %v, %v; want one verifier labelled local:%s", vs, err, keyPath)
	}
	if who, _, err := authorize(ctx, vs, mintTestToken(t, priv, "box", map[string]string{"sub": "ci"})); err != nil || who.By != "local:"+keyPath+" sub=ci" {
		t.Errorf("a token for the resolved values: By = %q, %v; want accepted", who.By, err)
	}
	if _, _, err := authorize(ctx, vs, mintTestToken(t, priv, "{env.HOTSERVE_TEST_AUDIENCE}", map[string]string{"sub": "ci"})); err == nil {
		t.Errorf("a token for the placeholder's own text was accepted: the audience was not resolved")
	}
	if _, _, err := authorize(ctx, vs, mintTestToken(t, priv, "box", map[string]string{"sub": "{env.HOTSERVE_TEST_SUBJECT}"})); err == nil {
		t.Errorf("a token presenting the placeholder's own text was accepted: the claim was not resolved")
	}
	if cfgs[0].Audience != "{env.HOTSERVE_TEST_AUDIENCE}" || cfgs[0].Claims["sub"] != "{env.HOTSERVE_TEST_SUBJECT}" {
		t.Errorf("NewTrust resolved the caller's config in place: %+v", cfgs[0])
	}

	// No source is refused at config load, not as a 401 per token.
	if _, err := NewTrust(nil, false); err == nil || !strings.Contains(err.Error(), "no source") {
		t.Errorf("NewTrust(nil) = %v; want a refusal naming the missing source", err)
	}

	// A source buildTrust refuses is refused in buildTrust's words.
	bad := []TrustConfig{{Kind: "github", Audience: "hotserve"}}
	_, got := NewTrust(bad, false)
	_, want := buildTrust(bad, nil)
	if got == nil || want == nil || got.Error() != want.Error() {
		t.Errorf("NewTrust(bad) = %v; buildTrust = %v; want the same refusal", got, want)
	}

	// An OIDC source: the JWKS client is https-only unless
	// allowInsecure. The mock issuer speaks plain http, so only the
	// insecure client reaches it; the other names it as unreachable.
	// The warm each one starts is a single bounded discovery fetch
	// that the mock answers, or the https-only client refuses, at
	// once; nothing of it outlives the test.
	iss := newMockIssuer(t)
	cfg := func() []TrustConfig {
		return []TrustConfig{{Kind: "oidc", Issuer: iss.url, Audience: "hotserve", Subject: "ci"}}
	}
	tok := iss.mint(t, iss.priv, "hotserve", map[string]string{"sub": "ci"}, time.Now().Add(5*time.Minute))
	insecure, err := NewTrust(cfg(), true)
	if err != nil {
		t.Fatal(err)
	}
	if who, _, err := authorize(ctx, insecure, tok); err != nil || who.By != "oidc:"+iss.url+" sub=ci" {
		t.Errorf("allow_insecure_http: By = %q, %v; want the token accepted over plain http", who.By, err)
	}
	secure, err := NewTrust(cfg(), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, down, err := authorize(ctx, secure, tok); err == nil || len(down) != 1 || !strings.Contains(err.Error(), "refusing non-https") {
		t.Errorf("https only: err = %v, down = %d; want the issuer refused as unreachable over http", err, len(down))
	}
}

// TestExportedFilterIsTheResponseFilter pins RespondJSON to respondJSON
// and NewRedactor to the filter's own layers: a body through a nil
// filter loses a token-shaped string to the shape layer; a filter
// primed with an environment loses that environment's values and
// names the key; an id a body names survives the entropy layer only
// on the safe list, and never when it equals a known value.
func TestExportedFilterIsTheResponseFilter(t *testing.T) {
	const jwt = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJyZXBvIn0.c2lnbmF0dXJlX2hlcmVfMTIz" // gitleaks:allow
	body := map[string]string{"error": "validate: token " + jwt + " rejected"}
	w, w2 := httptest.NewRecorder(), httptest.NewRecorder()
	if err := RespondJSON(w, http.StatusUnprocessableEntity, body, nil); err != nil {
		t.Fatal(err)
	}
	if err := respondJSON(w2, http.StatusUnprocessableEntity, body, nil); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusUnprocessableEntity || w.Header().Get("Content-Type") != "application/json" || w.Body.String() != w2.Body.String() {
		t.Fatalf("RespondJSON wrote %d %q; respondJSON wrote %q", w.Code, w.Body.String(), w2.Body.String())
	}
	if strings.Contains(w.Body.String(), jwt) || !strings.Contains(w.Body.String(), "[redacted:jwt]") {
		t.Errorf("a nil filter let a token through: %s", w.Body.String())
	}

	const text = "dial postgres://app:hunter2hunter2@db/app: refused, password hunter2hunter2"
	r := NewRedactor([]string{"DATABASE_URL=postgres://app:hunter2hunter2@db/app", "PATH=/usr/bin"}, nil)
	out := r.Redact(text)
	if strings.Contains(out, "hunter2hunter2") || !strings.Contains(out, "[redacted:DATABASE_URL]") {
		t.Errorf("Redact = %q; want the value and its password replaced", out)
	}
	if want, _ := r.redact(text); out != want {
		t.Errorf("Redact = %q; the filter's own text form = %q", out, want)
	}
	w = httptest.NewRecorder()
	if err := RespondJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "password hunter2hunter2"}, r); err != nil {
		t.Fatal(err)
	}
	if got := w.Body.String(); strings.Contains(got, "hunter2hunter2") || !strings.Contains(got, "redacted_env") || !strings.Contains(got, "DATABASE_URL") {
		t.Errorf("a primed filter's body = %s; want the value replaced and the key named", got)
	}
	var none *Redactor
	if out := none.Redact("token " + jwt); strings.Contains(out, jwt) {
		t.Errorf("a nil Redactor is the two layers: %q", out)
	}
	if out := NewRedactor(nil, nil).Redact("token " + jwt); strings.Contains(out, jwt) {
		t.Errorf("an empty filter is the two layers: %q", out)
	}

	// The safe list: the sha a box body names survives the entropy
	// layer only when the caller lists it, as liveswap lists an app's
	// versions, and never when it equals a known value (rule 1).
	const sha = "3f9a1c2b4d5e6f708192a3b4c5d6e7f8091a2b3c" // the SHA TestRedactorSafeList pins as masked
	for name, tc := range map[string]struct {
		r        *Redactor
		survives bool
	}{
		"unlisted":         {NewRedactor(nil, nil), false},
		"listed":           {NewRedactor(nil, []string{sha}), true},
		"listed but known": {NewRedactor([]string{"TOKEN=" + sha}, []string{sha}), false},
	} {
		w := httptest.NewRecorder()
		if err := RespondJSON(w, http.StatusOK, map[string]string{"commit": sha}, tc.r); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(w.Body.String(), sha) != tc.survives {
			t.Errorf("%s: body = %s; want the sha to survive = %v", name, w.Body.String(), tc.survives)
		}
	}
}
