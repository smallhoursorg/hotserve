package box

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/smallhoursorg/hotserve/liveswap/deploytrust"
	"github.com/smallhoursorg/hotserve/liveswap/deploytrust/trusttest"
)

// sha is a commit id the entropy layer masks unless it is listed (the
// one liveswap's TestRedactorSafeList pins).
const sha = "3f9a1c2b4d5e6f708192a3b4c5d6e7f8091a2b3c"

const jwt = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJyZXBvIn0.c2lnbmF0dXJlX2hlcmVfMTIz" // gitleaks:allow

type rig struct {
	h         *Handler
	clock     *trusttest.Clock
	priv      ed25519.PrivateKey
	logs      *observer.ObservedLogs
	dir       string // the exchange tree
	installed string
	nexted    int // requests passed to the next handler
}

func newRig(t testing.TB) *rig {
	t.Helper()
	priv, pub := trusttest.GenerateKey()
	vs, err := deploytrust.New([]deploytrust.TrustConfig{{Kind: "local", PublicKey: trusttest.KeyFile(t, pub), Audience: "box1"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	core, logs := observer.New(zap.InfoLevel)
	r := &rig{clock: trusttest.NewClock(), priv: priv, logs: logs, dir: filepath.Join(tmp, "hotserve-box"), installed: filepath.Join(tmp, "Caddyfile")}
	r.h = &Handler{
		app:       &App{verifiers: vs},
		logger:    zap.New(core),
		limiter:   deploytrust.NewLimiter(r.clock),
		installed: r.installed,
		dir:       r.dir,
	}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r.write(t, r.installed, file(good))
	return r
}

func (r *rig) token(t testing.TB) string { return trusttest.Mint(t, r.priv, "box1", nil) }

func (r *rig) write(t testing.TB, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) writeJSON(t testing.TB, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	r.write(t, path, b)
}

func (r *rig) applied(t *testing.T, commit string) {
	r.writeJSON(t, filepath.Join(r.dir, "applied.json"), applied{SHA: commit, Path: "box1/Caddyfile", SHA256: strings.Repeat("0", 64), Signer: "alice", When: r.clock.Now()})
}

type req struct {
	method, target, token string
	header                http.Header
	body                  io.Reader
}

// do serves one request, failing the test rather than hanging if the
// handler blocks (a FIFO opened without O_NONBLOCK would).
func (r *rig) do(t *testing.T, q req) *httptest.ResponseRecorder {
	t.Helper()
	if q.method == "" {
		q.method = http.MethodGet
	}
	hr := httptest.NewRequest(q.method, q.target, q.body)
	for k, vs := range q.header {
		hr.Header[k] = vs
	}
	if q.token != "" {
		hr.Header.Set("Authorization", "Bearer "+q.token)
	}
	w := httptest.NewRecorder()
	done := make(chan error, 1)
	go func() {
		done <- r.h.ServeHTTP(w, hr, nextFunc(func(http.ResponseWriter, *http.Request) error {
			r.nexted++
			return nil
		}))
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeHTTP: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the handler blocked")
	}
	return w
}

type nextFunc func(http.ResponseWriter, *http.Request) error

func (f nextFunc) ServeHTTP(w http.ResponseWriter, r *http.Request) error { return f(w, r) }

func body(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type %q, body %s", ct, w.Body)
	}
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("not JSON: %v: %s", err, w.Body)
	}
	return m
}

func wantError(t *testing.T, w *httptest.ResponseRecorder, code int, msg string) {
	t.Helper()
	if w.Code != code {
		t.Fatalf("status %d, want %d: %s", w.Code, code, w.Body)
	}
	if got, _ := body(t, w)["error"].(string); !strings.Contains(got, msg) {
		t.Fatalf("error %q, want it to contain %q", got, msg)
	}
}

const unauthorized = "invalid or missing deploy token (Authorization: Bearer <jwt>)"

func TestHandlerPassesOtherPaths(t *testing.T) {
	r := newRig(t)
	for _, target := range []string{"/x", "/x?result=00", "/deploy/app", "/%2F", "/index.html", "/./"} {
		for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut} {
			before := r.nexted
			w := r.do(t, req{method: m, target: target})
			if r.nexted != before+1 || w.Body.Len() != 0 || len(w.Header()) != 0 {
				t.Errorf("%s %s was answered (%d %s), not passed on", m, target, w.Code, w.Body)
			}
		}
	}
	if r.h.limiter.Size() != 0 {
		t.Error("a passed-on request was charged")
	}
}

func TestHandlerMethodBeforeAuth(t *testing.T) {
	r := newRig(t)
	for _, m := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodHead, http.MethodOptions, "BREW"} {
		w := r.do(t, req{method: m, target: "/"})
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, POST" {
			t.Errorf("%s: %d, Allow %q", m, w.Code, w.Header().Get("Allow"))
		}
		if m != http.MethodHead {
			wantError(t, w, http.StatusMethodNotAllowed, msgMethod)
		}
	}
	if r.h.limiter.Size() != 0 || r.nexted != 0 {
		t.Error("a 405 was charged or passed on")
	}
}

func TestHandlerAuthenticates(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	for _, q := range []req{
		{target: "/"},
		{target: "/?result=" + id},
		{target: "/?result=nonsense"},
		{target: "/?other=1"},
		{method: http.MethodPost, target: "/", body: strings.NewReader("x")},
	} {
		t.Run(q.method+" "+q.target, func(t *testing.T) {
			r := newRig(t)
			r.applied(t, sha)
			for _, token := range []string{"", "not.a.jwt", trusttest.MintExpired(t, r.priv, "box1", nil), trusttest.Mint(t, r.priv, "another-box", nil)} {
				q.token = token
				wantError(t, r.do(t, q), http.StatusUnauthorized, unauthorized)
			}
			if r.h.limiter.Size() != 1 {
				t.Fatalf("failures not charged: %d addresses", r.h.limiter.Size())
			}
			for range deploytrust.FailBudget {
				r.do(t, q)
			}
			w := r.do(t, q)
			wantError(t, w, http.StatusTooManyRequests, "too many failed deploy authentications")
			if ra, err := strconv.Atoi(w.Header().Get("Retry-After")); err != nil || ra < 1 {
				t.Fatalf("Retry-After %q", w.Header().Get("Retry-After"))
			}
		})
	}
}

func TestHandlerJournalNamesTheRoute(t *testing.T) {
	r := newRig(t)
	r.do(t, req{method: http.MethodPost, target: "/"})
	got := r.logs.FilterMessage("webhook auth failed").All()
	if len(got) != 1 || got[0].ContextMap()["box_request"] != "push" || got[0].ContextMap()["app"] != nil {
		t.Fatalf("want one line scoped box_request=push: %v", r.logs.All())
	}
}

func TestHandlerStatus(t *testing.T) {
	r := newRig(t)
	wantError(t, r.do(t, req{target: "/", token: r.token(t)}), http.StatusConflict, msgNoBaseline)

	r.applied(t, sha)
	w := r.do(t, req{target: "/", token: r.token(t)})
	sum := sha256.Sum256(file(good))
	want := map[string]any{"commit": sha, "box_webhook": "deploy.example.com", "sha256": hex.EncodeToString(sum[:])}
	if got := body(t, w); w.Code != http.StatusOK || len(got) != len(want) || got["commit"] != want["commit"] || got["box_webhook"] != want["box_webhook"] || got["sha256"] != want["sha256"] {
		t.Fatalf("%d %v, want %v", w.Code, got, want)
	}

	// applied.json's cap holds a 4 KiB path that JSON's escapes grow
	// sixfold (each of these bytes is written \u0026).
	r.writeJSON(t, filepath.Join(r.dir, "applied.json"), applied{SHA: sha, Path: strings.Repeat("&", 4096), SHA256: strings.Repeat("0", 64), Signer: strings.Repeat("&", 256)})
	if w := r.do(t, req{target: "/", token: r.token(t)}); w.Code != http.StatusOK {
		t.Fatalf("an escaped 4 KiB path: %d %s", w.Code, w.Body)
	}

	// A console edit since the last apply shows in the digest.
	r.write(t, r.installed, append(file(good), "# edited\n"...))
	if got := body(t, r.do(t, req{target: "/", token: r.token(t)})); got["sha256"] == want["sha256"] {
		t.Fatal("the digest is not the installed file's")
	}
}

func TestHandlerStatusBoxErrors(t *testing.T) {
	big := append(file(good), bytes.Repeat([]byte("#"), 1<<20)...)
	for name, c := range map[string]struct {
		setup func(t *testing.T, r *rig)
		want  string
	}{
		"installed missing": {func(t *testing.T, r *rig) { must(t, os.Remove(r.installed)) }, "could not read the Caddyfile this box runs: open "},
		"installed too big": {func(t *testing.T, r *rig) { r.write(t, r.installed, big) }, "larger than 1048576 bytes"},
		"installed a FIFO":  {func(t *testing.T, r *rig) { must(t, os.Remove(r.installed)); mkfifo(t, r.installed) }, "not a regular file"},
		"installed a dir":   {func(t *testing.T, r *rig) { must(t, os.Remove(r.installed)); must(t, os.Mkdir(r.installed, 0o755)) }, "not a regular file"},
		"installed not a box's": {func(t *testing.T, r *rig) { r.write(t, r.installed, []byte("{\n}\nx.com {\n}\n")) },
			"the Caddyfile this box runs has no box block"},
		"installed refused with input": {func(t *testing.T, r *rig) {
			r.write(t, r.installed, file(strings.Replace(good, "deploy.example.com {", strings.Repeat("a", 2000)+".com {", 1)))
		}, "has a box_webhook site whose address is not one bare hostname"},
		"installed refused, the address filtered": {func(t *testing.T, r *rig) {
			r.write(t, r.installed, file(strings.Replace(good, "deploy.example.com {", "https://"+jwt+" {", 1)))
		}, "[redacted:jwt]"},
		"applied not JSON":      {func(t *testing.T, r *rig) { r.write(t, filepath.Join(r.dir, "applied.json"), []byte("{")) }, "could not read the baseline"},
		"applied sha not an id": {func(t *testing.T, r *rig) { r.applied(t, "HEAD") }, "sha is not a 40-hex commit id"},
		"applied too big": {func(t *testing.T, r *rig) {
			r.write(t, filepath.Join(r.dir, "applied.json"), bytes.Repeat([]byte(" "), maxApplied+1))
		}, "larger than"},
		"applied a symlink": {func(t *testing.T, r *rig) {
			r.applied(t, sha)
			must(t, os.Rename(filepath.Join(r.dir, "applied.json"), filepath.Join(r.dir, "real.json")))
			must(t, os.Symlink("real.json", filepath.Join(r.dir, "applied.json")))
		}, "could not read the baseline"},
		"applied a FIFO": {func(t *testing.T, r *rig) {
			must(t, os.Remove(filepath.Join(r.dir, "applied.json")))
			mkfifo(t, filepath.Join(r.dir, "applied.json"))
		}, "not a regular file"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.applied(t, sha)
			c.setup(t, r)
			w := r.do(t, req{target: "/", token: r.token(t)})
			wantError(t, w, http.StatusInternalServerError, c.want)
			if strings.Contains(w.Body.String(), jwt) {
				t.Errorf("a token-shaped address in the answer: %s", w.Body)
			}
			if len(w.Body.String()) > 600 {
				t.Errorf("an unbounded 500: %d bytes", w.Body.Len())
			}
			if r.logs.FilterLevelExact(zap.ErrorLevel).Len() != 1 {
				t.Errorf("want one error line: %v", r.logs.All())
			}
		})
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func mkfifo(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Fatal(err)
	}
}

// failingReader fails the test if the handler reads the body.
type failingReader struct{ t *testing.T }

func (f failingReader) Read([]byte) (int, error) {
	f.t.Error("the body was read")
	return 0, io.EOF
}

func TestHandlerPushUntilTheApplier(t *testing.T) {
	r := newRig(t)
	w := r.do(t, req{method: http.MethodPost, target: "/", token: r.token(t), body: failingReader{t}})
	wantError(t, w, http.StatusNotImplemented, msgNoApplier)
	wantError(t, r.do(t, req{method: http.MethodPost, target: "/", body: failingReader{t}}), http.StatusUnauthorized, unauthorized)
}

// TestHandlerResultUntilTheApplier: a result poll comes with the
// applier, which writes what it reads; until then any query on `GET /`
// is authenticated, then refused, and no poll secret is read — a
// Box-Poll Authorization is a request without a bearer.
func TestHandlerResultUntilTheApplier(t *testing.T) {
	r := newRig(t)
	for _, target := range []string{"/?result=0123456789abcdef0123456789abcdef", "/?result=", "/?x=1", "/?%ZZ", "/?"} {
		wantError(t, r.do(t, req{target: target, token: r.token(t)}), http.StatusNotImplemented, msgNoApplier)
	}
	poll := http.Header{"Authorization": {"Box-Poll " + strings.Repeat("A", 43) + "="}}
	wantError(t, r.do(t, req{target: "/?result=0123456789abcdef0123456789abcdef", header: poll}), http.StatusUnauthorized, unauthorized)
	if r.h.limiter.Size() != 1 {
		t.Fatal("a refused poll was not charged")
	}
}
