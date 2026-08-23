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
// managed app, bypassing caddy.Context entirely. The app trusts
// appTestPub for its own deploys; the App's global trust (for the
// unknown-app path) is globalTestPub.
func newTestHandler(t *testing.T) (*Handler, *testRig) {
	t.Helper()
	rig := newTestRig(t)
	app := &App{
		managed:         map[string]*managedApp{"demo": rig.ma},
		globalVerifiers: resolveVerifiers([]trustSource{localTrust(globalTestPub, "global")}, nil),
	}
	h := &Handler{app: app, logger: zap.NewNop()}
	return h, rig
}

// do sends a request with token as the Authorization bearer (empty =
// no Authorization header).
func do(t *testing.T, h *Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	reader := strings.NewReader(body)
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
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
	valid := appToken(t)
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

	if w := bearer("Bearer " + valid); w.Code != http.StatusOK {
		t.Errorf("valid Bearer: got %d, want 200", w.Code)
	}
	if w := bearer("bearer " + valid); w.Code != http.StatusOK {
		t.Errorf("scheme is case-insensitive: got %d, want 200", w.Code)
	}
	if w := bearer("Bearer not-a-jwt"); w.Code != http.StatusUnauthorized {
		t.Errorf("garbage Bearer: got %d, want 401", w.Code)
	}
	if w := bearer("Basic " + valid); w.Code != http.StatusUnauthorized {
		t.Errorf("non-Bearer scheme: got %d, want 401", w.Code)
	}
	if w := bearer("Bearer"); w.Code != http.StatusUnauthorized {
		t.Errorf("empty Bearer: got %d, want 401", w.Code)
	}
}

func TestWebhookRejectsBadToken(t *testing.T) {
	h, _ := newTestHandler(t)
	for name, token := range map[string]string{"missing": "", "garbage": "not-a-jwt"} {
		t.Run(name, func(t *testing.T) {
			w := do(t, h, http.MethodPost, "/demo", token, `{"url":"https://x/a.tgz","version":"v1"}`)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("code = %d, want 401", w.Code)
			}
		})
	}
}

func TestWebhookUnknownAppIs404OnlyWhenAuthenticated(t *testing.T) {
	h, _ := newTestHandler(t)
	// Garbage token + unknown app: still 401, no name enumeration.
	w := do(t, h, http.MethodPost, "/ghost", "not-a-jwt", "{}")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated unknown app must 401, got %d", w.Code)
	}
	// Valid global token + unknown app: 404.
	w = do(t, h, http.MethodPost, "/ghost", globalToken(t), "{}")
	if w.Code != http.StatusNotFound {
		t.Fatalf("authenticated unknown app must 404, got %d", w.Code)
	}
}

func TestWebhookDeployHappyPath(t *testing.T) {
	h, rig := newTestHandler(t)
	w := do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/a.tgz","version":"v1"}`)
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
	// The status records which trust source authorized the deploy.
	if status.LastDeploy == nil || status.LastDeploy.By != "local:test-key" {
		t.Fatalf("deployed_by not recorded: %+v", status.LastDeploy)
	}
}

func TestWebhookUsesPerAppTrust(t *testing.T) {
	h, _ := newTestHandler(t)
	// The app trusts appTestPub (audience "demo"); a token valid only
	// under the GLOBAL trust must NOT authenticate against a known app.
	w := do(t, h, http.MethodPost, "/demo", globalToken(t), `{"url":"https://x/a.tgz","version":"v1"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("global token must not open a per-app-trust app, got %d", w.Code)
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
		{"auth_header with CRLF", `{"url":"https://x/a.tgz","version":"v1","auth_header":"Bearer a\r\nX-Evil: 1"}`, http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, h, http.MethodPost, "/demo", appToken(t), tc.body)
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
	w := do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/a.tgz","version":"v1"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409", w.Code)
	}
}

func TestWebhookDeployFailureReturns500WithOldStatus(t *testing.T) {
	h, rig := newTestHandler(t)
	do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/1.tgz","version":"v1"}`)
	rig.prober.err = errTest
	w := do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/2.tgz","version":"v2"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"current_version":"v1"`) {
		t.Fatalf("500 body should show the still-serving version: %s", w.Body.String())
	}
}

func TestWebhookGetStatus(t *testing.T) {
	h, _ := newTestHandler(t)
	do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/1.tgz","version":"v1"}`)
	w := do(t, h, http.MethodGet, "/demo", appToken(t), "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"current_version":"v1"`) {
		t.Fatalf("status GET wrong: %d %s", w.Code, w.Body.String())
	}
}

func TestWebhookMethodNotAllowed(t *testing.T) {
	h, _ := newTestHandler(t)
	w := do(t, h, http.MethodDelete, "/demo", appToken(t), "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code = %d, want 405", w.Code)
	}
}

// The app name is the LAST path segment, so mounting under a prefix
// like handle /deploy/* works.
func TestWebhookAppNameFromLastSegment(t *testing.T) {
	h, _ := newTestHandler(t)
	w := do(t, h, http.MethodGet, "/deploy/demo", appToken(t), "")
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
	w := do(t, h, http.MethodPost, "/demo", appToken(t), big)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "payload exceeds") {
		t.Fatalf("body should name the limit: %s", w.Body.String())
	}
}

// send runs one request through the handler.
func send(t *testing.T, h *Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	var next caddyhttp.Handler = caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error { return nil })
	if err := h.ServeHTTP(w, req, next); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}
	return w
}

func gzipDeploy(t *testing.T, target, token, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/gzip")
	return req
}

func TestWebhookPushRoutesAndDeploys(t *testing.T) {
	h, rig := newTestHandler(t)
	w := send(t, h, gzipDeploy(t, "/demo?version=v9", appToken(t), "tarball-bytes"))
	if w.Code != http.StatusOK {
		t.Fatalf("push: got %d %s", w.Code, w.Body.String())
	}
	got := rig.fetch.lastReq
	if got.source() != "push" || got.localArchive == "" || got.Version != "v9" {
		t.Fatalf("push not routed correctly: %+v", got)
	}
}

func TestWebhookPushRequiresValidVersion(t *testing.T) {
	h, _ := newTestHandler(t)
	// missing version
	if w := send(t, h, gzipDeploy(t, "/demo", appToken(t), "x")); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing version: got %d", w.Code)
	}
	// traversal version
	if w := send(t, h, gzipDeploy(t, "/demo?version=../etc", appToken(t), "x")); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("evil version: got %d", w.Code)
	}
}

func TestWebhookPushOversizedIs413(t *testing.T) {
	h, rig := newTestHandler(t)
	rig.spec.maxArtifactSize = 8 // tiny cap for the test
	w := send(t, h, gzipDeploy(t, "/demo?version=v1", appToken(t), "way past eight bytes"))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized push: got %d, want 413", w.Code)
	}
}

func TestWebhookRollbackRoutes(t *testing.T) {
	h, rig := newTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/demo?rollback=v3", nil)
	req.Header.Set("Authorization", "Bearer "+appToken(t))
	w := send(t, h, req)
	if w.Code != http.StatusOK {
		t.Fatalf("rollback: got %d %s", w.Code, w.Body.String())
	}
	got := rig.fetch.lastReq
	if got.source() != "rollback" || !got.rollback || got.Version != "v3" {
		t.Fatalf("rollback not routed correctly: %+v", got)
	}
}

func TestWebhookRollbackRequiresValidVersion(t *testing.T) {
	h, _ := newTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/demo?rollback=..", nil)
	req.Header.Set("Authorization", "Bearer "+appToken(t))
	if w := send(t, h, req); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("evil rollback version: got %d", w.Code)
	}
}
