package liveswap

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

// newTestHandler wires a Handler to an App backed by one fake-driven
// managed app, bypassing caddy.Context entirely.
func newTestHandler(t *testing.T) (*Handler, *testRig) {
	t.Helper()
	rig := newTestRig(t)
	app := &App{
		WebhookSecret: "global-secret",
		managed:       map[string]*managedApp{"demo": rig.ma},
	}
	h := &Handler{app: app, logger: zap.NewNop()}
	return h, rig
}

func do(t *testing.T, h *Handler, method, path, secret, body string) *httptest.ResponseRecorder {
	t.Helper()
	reader := strings.NewReader(body)
	req := httptest.NewRequest(method, path, reader)
	if secret != "" {
		req.Header.Set(secretHeader, secret)
	}
	w := httptest.NewRecorder()
	var next caddyhttp.Handler = caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error { return nil })
	if err := h.ServeHTTP(w, req, next); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}
	return w
}

func TestWebhookBearerAuth(t *testing.T) {
	h, _ := newTestHandler(t)
	bearer := func(auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/demo", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		var next caddyhttp.Handler = caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error { return nil })
		if err := h.ServeHTTP(w, req, next); err != nil {
			t.Fatalf("ServeHTTP returned error: %v", err)
		}
		return w
	}

	if w := bearer("Bearer s3cret"); w.Code != http.StatusOK {
		t.Errorf("valid Bearer: got %d, want 200", w.Code)
	}
	if w := bearer("bearer s3cret"); w.Code != http.StatusOK {
		t.Errorf("scheme is case-insensitive: got %d, want 200", w.Code)
	}
	if w := bearer("Bearer wrong"); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong Bearer: got %d, want 401", w.Code)
	}
	if w := bearer("Basic s3cret"); w.Code != http.StatusUnauthorized {
		t.Errorf("non-Bearer scheme: got %d, want 401", w.Code)
	}
	if w := bearer("Bearer"); w.Code != http.StatusUnauthorized {
		t.Errorf("empty Bearer: got %d, want 401", w.Code)
	}
}

func TestWebhookCustomHeaderTakesPrecedenceOverBearer(t *testing.T) {
	h, _ := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/demo", nil)
	req.Header.Set(secretHeader, "wrong")
	req.Header.Set("Authorization", "Bearer s3cret")
	w := httptest.NewRecorder()
	var next caddyhttp.Handler = caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error { return nil })
	if err := h.ServeHTTP(w, req, next); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}
	// A present custom header is authoritative — a wrong value must not
	// fall through to a different credential.
	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong custom header with valid Bearer: got %d, want 401", w.Code)
	}
}

func TestWebhookRejectsBadSecret(t *testing.T) {
	h, _ := newTestHandler(t)
	for name, secret := range map[string]string{"missing": "", "wrong": "nope"} {
		t.Run(name, func(t *testing.T) {
			w := do(t, h, http.MethodPost, "/demo", secret, `{"url":"https://x/a.tgz","version":"v1"}`)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("code = %d, want 401", w.Code)
			}
		})
	}
}

func TestWebhookUnknownAppIs404OnlyWhenAuthenticated(t *testing.T) {
	h, _ := newTestHandler(t)
	// Wrong secret + unknown app: still 401, no name enumeration.
	w := do(t, h, http.MethodPost, "/ghost", "nope", "{}")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated unknown app must 401, got %d", w.Code)
	}
	// Correct global secret + unknown app: 404.
	w = do(t, h, http.MethodPost, "/ghost", "global-secret", "{}")
	if w.Code != http.StatusNotFound {
		t.Fatalf("authenticated unknown app must 404, got %d", w.Code)
	}
}

func TestWebhookDeployHappyPath(t *testing.T) {
	h, rig := newTestHandler(t)
	w := do(t, h, http.MethodPost, "/demo", "s3cret", `{"url":"https://x/a.tgz","version":"v1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	var status statusSnapshot
	must(t, json.Unmarshal(w.Body.Bytes(), &status))
	if status.CurrentVersion != "v1" || !status.Running {
		t.Fatalf("response status wrong: %+v", status)
	}
	if rig.ma.activePort.Load() == 0 {
		t.Fatal("deploy did not publish a port")
	}
}

func TestWebhookUsesPerAppSecret(t *testing.T) {
	h, _ := newTestHandler(t)
	// The app's own secret is "s3cret" (testSpec); the global secret
	// must NOT authenticate against a known app.
	w := do(t, h, http.MethodPost, "/demo", "global-secret", `{"url":"https://x/a.tgz","version":"v1"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("global secret must not open a per-secret app, got %d", w.Code)
	}
}

func TestWebhookValidatesPayload(t *testing.T) {
	h, _ := newTestHandler(t)
	cases := []struct {
		name string
		body string
		want int
	}{
		{"bad json", "{nope", http.StatusBadRequest},
		{"missing url", `{"version":"v1"}`, http.StatusUnprocessableEntity},
		{"missing version", `{"url":"https://x/a.tgz"}`, http.StatusUnprocessableEntity},
		{"evil version", `{"url":"https://x/a.tgz","version":"../../etc"}`, http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, h, http.MethodPost, "/demo", "s3cret", tc.body)
			if w.Code != tc.want {
				t.Fatalf("code = %d, want %d (body %s)", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

func TestWebhookConflictWhileDeploying(t *testing.T) {
	h, rig := newTestHandler(t)
	rig.ma.deployMu.Lock()
	defer rig.ma.deployMu.Unlock()
	w := do(t, h, http.MethodPost, "/demo", "s3cret", `{"url":"https://x/a.tgz","version":"v1"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409", w.Code)
	}
}

func TestWebhookDeployFailureReturns500WithOldStatus(t *testing.T) {
	h, rig := newTestHandler(t)
	do(t, h, http.MethodPost, "/demo", "s3cret", `{"url":"https://x/1.tgz","version":"v1"}`)
	rig.prober.err = errTest
	w := do(t, h, http.MethodPost, "/demo", "s3cret", `{"url":"https://x/2.tgz","version":"v2"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"current_version":"v1"`) {
		t.Fatalf("500 body should show the still-serving version: %s", w.Body.String())
	}
}

func TestWebhookGetStatus(t *testing.T) {
	h, _ := newTestHandler(t)
	do(t, h, http.MethodPost, "/demo", "s3cret", `{"url":"https://x/1.tgz","version":"v1"}`)
	w := do(t, h, http.MethodGet, "/demo", "s3cret", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"current_version":"v1"`) {
		t.Fatalf("status GET wrong: %d %s", w.Code, w.Body.String())
	}
}

func TestWebhookMethodNotAllowed(t *testing.T) {
	h, _ := newTestHandler(t)
	w := do(t, h, http.MethodDelete, "/demo", "s3cret", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code = %d, want 405", w.Code)
	}
}

// The app name is the LAST path segment, so mounting under a prefix
// like handle /deploy/* works.
func TestWebhookAppNameFromLastSegment(t *testing.T) {
	h, _ := newTestHandler(t)
	w := do(t, h, http.MethodGet, "/deploy/demo", "s3cret", "")
	if w.Code != http.StatusOK {
		t.Fatalf("prefixed path should resolve the app, got %d", w.Code)
	}
}

var errTest = errTestType{}

type errTestType struct{}

func (errTestType) Error() string { return "test error" }

// An oversized payload must get an honest 413, not a misleading
// "invalid JSON" 400 from silent truncation at the cap.
func TestWebhookOversizedPayloadIs413(t *testing.T) {
	h, _ := newTestHandler(t)
	big := `{"url":"https://x/a.tgz","version":"v1","pad":"` +
		strings.Repeat("a", maxPayloadBytes) + `"}`
	w := do(t, h, http.MethodPost, "/demo", "s3cret", big)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "payload exceeds") {
		t.Fatalf("body should name the limit: %s", w.Body.String())
	}
}
