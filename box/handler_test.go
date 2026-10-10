package box

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
		now:       r.clock.Now,
		stateLog:  &stateLog{},
	}
	for _, d := range []string{"out", "stage"} {
		if err := os.MkdirAll(filepath.Join(r.dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
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

func (r *rig) marker(t *testing.T, id, digest string, posted time.Time) {
	r.writeJSON(t, filepath.Join(r.dir, "stage", id+".auth"), marker{SHA256: digest, Posted: posted})
}

func (r *rig) result(t *testing.T, res result) {
	r.writeJSON(t, filepath.Join(r.dir, "out", res.ID+".json"), res)
}

// secret is a poll secret made of one repeated byte: its header, its
// id and its digest.
func secret(seed byte) (header, id, digest string) {
	return digestOf(bytes.Repeat([]byte{seed}, pollSecretLen), base64.StdEncoding)
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

func pollHeader(h string) http.Header { return http.Header{"Authorization": {pollScheme + h}} }

// body decodes a JSON response, failing on anything else.
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
	_, id, _ := secret(1)
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
	_, _, digest := secret(1)
	w := r.do(t, req{method: http.MethodPost, target: "/", token: r.token(t), body: failingReader{t}, header: http.Header{"X-Box-Poll-Digest": {digest}}})
	wantError(t, w, http.StatusNotImplemented, msgNoApplier)
	wantError(t, r.do(t, req{method: http.MethodPost, target: "/", body: failingReader{t}}), http.StatusUnauthorized, unauthorized)
}

func TestHandlerResultPhases(t *testing.T) {
	for phase, code := range map[string]int{
		"verified": 202, "no_change": 200, "applied": 200,
		"refused": 422, "failed": 422, "rolled_back": 422, "unknown": 422,
	} {
		t.Run(phase, func(t *testing.T) {
			r := newRig(t)
			h, id, digest := secret(7)
			r.marker(t, id, digest, r.clock.Now())
			r.result(t, result{ID: id, Commit: sha, Path: "box1/Caddyfile", Signer: "alice@example.com", Phase: phase, BoxWebhook: "deploy.example.com", Apps: []string{"example"}})
			w := r.do(t, req{target: "/?result=" + id, header: pollHeader(h)})
			got := body(t, w)
			if w.Code != code || got["id"] != id || got["commit"] != sha || got["phase"] != phase || got["signer"] != "alice@example.com" || got["box_webhook"] != "deploy.example.com" {
				t.Fatalf("%d %v, want %d with the id and commit standing", w.Code, got, code)
			}
			if _, ok := got["caddyfile_edited_out_of_band"]; !ok {
				t.Error("the out-of-band flag is not in the answer")
			}
			// The same answer to the bearer.
			if w2 := r.do(t, req{target: "/?result=" + id, token: r.token(t)}); w2.Code != code || w2.Body.String() != w.Body.String() {
				t.Fatalf("by token: %d %s", w2.Code, w2.Body)
			}
			if r.h.limiter.Size() != 0 {
				t.Fatal("a poll was charged")
			}
		})
	}
}

func TestHandlerResultPollSecret(t *testing.T) {
	h, id, digest := secret(7)
	other, otherID, otherDigest := secret(8)
	// A secret whose standard base64 holds '+' and '/', sent in the URL
	// alphabet; and one a byte short, in the right alphabet.
	urlRaw := bytes.Repeat([]byte{0xfb}, pollSecretLen)
	urlHeader, urlID, urlDigest := digestOf(urlRaw, base64.URLEncoding)
	shortHeader, shortID, shortDigest := digestOf(bytes.Repeat([]byte{7}, pollSecretLen-1), base64.StdEncoding)
	fresh := func(t *testing.T, r *rig) { r.marker(t, id, digest, r.clock.Now()) }
	// unreadable: refused as any unauthenticated request is, and the
	// box's failure in the journal.
	const admitted, refused, unreadable = 202, 401, -1
	for name, c := range map[string]struct {
		setup  func(t *testing.T, r *rig)
		target string
		header http.Header
		code   int
	}{
		"fresh": {fresh, "/?result=" + id, pollHeader(h), admitted},
		// The secret has no age of its own: it is honoured for as long
		// as its marker is kept, whatever the clocks say.
		"a day old":            {func(t *testing.T, r *rig) { r.marker(t, id, digest, r.clock.Now().Add(-24*time.Hour)) }, "/?result=" + id, pollHeader(h), admitted},
		"posted in the future": {func(t *testing.T, r *rig) { r.marker(t, id, digest, r.clock.Now().Add(time.Hour)) }, "/?result=" + id, pollHeader(h), admitted},
		"no posted time":       {func(t *testing.T, r *rig) { r.marker(t, id, digest, time.Time{}) }, "/?result=" + id, pollHeader(h), admitted},
		"no marker":            {func(*testing.T, *rig) {}, "/?result=" + id, pollHeader(h), refused},
		"another push's secret": {func(t *testing.T, r *rig) {
			fresh(t, r)
			r.marker(t, otherID, otherDigest, r.clock.Now())
		}, "/?result=" + id, pollHeader(other), refused},
		// The marker's digest begins with the id but is not the
		// secret's: the comparison is of the whole digest.
		"digest shares only the id": {func(t *testing.T, r *rig) { r.marker(t, id, id+strings.Repeat("0", 32), r.clock.Now()) }, "/?result=" + id, pollHeader(h), refused},
		"two headers":               {fresh, "/?result=" + id, http.Header{"Authorization": {pollScheme + h, pollScheme + h}}, refused},
		"scheme in another case":    {fresh, "/?result=" + id, http.Header{"Authorization": {"box-POLL " + h}}, admitted},
		"the secret as a bearer":    {fresh, "/?result=" + id, http.Header{"Authorization": {"Bearer " + h}}, refused},
		"the secret bare":           {fresh, "/?result=" + id, http.Header{"Authorization": {h}}, refused},
		"the old header":            {fresh, "/?result=" + id, http.Header{"X-Box-Poll-Secret": {h}}, refused},
		"two spaces":                {fresh, "/?result=" + id, http.Header{"Authorization": {"Box-Poll  " + h[:43]}}, refused},
		"unpadded":                  {fresh, "/?result=" + id, pollHeader(strings.TrimRight(h, "=")), refused},
		"url alphabet":              {func(t *testing.T, r *rig) { r.marker(t, urlID, urlDigest, r.clock.Now()) }, "/?result=" + urlID, pollHeader(urlHeader), refused},
		"31 bytes":                  {func(t *testing.T, r *rig) { r.marker(t, shortID, shortDigest, r.clock.Now()) }, "/?result=" + shortID, pollHeader(shortHeader), refused},
		"upper-case id":             {fresh, "/?result=" + strings.ToUpper(id), pollHeader(h), refused},
		"another param":             {fresh, "/?result=" + id + "&x=1", pollHeader(h), refused},
		"result twice":              {fresh, "/?result=" + id + "&result=" + id, pollHeader(h), refused},
		// A marker the box cannot read authenticates nothing, as an
		// issuer it cannot reach authenticates no token: the flat 401,
		// charged, and the failure journaled (once a window).
		"stage not a directory": {func(t *testing.T, r *rig) {
			must(t, os.RemoveAll(filepath.Join(r.dir, "stage")))
			r.write(t, filepath.Join(r.dir, "stage"), nil)
		}, "/?result=" + id, pollHeader(h), unreadable},
		"marker symlink": {func(t *testing.T, r *rig) { symlinkMarker(t, r, id, digest) }, "/?result=" + id, pollHeader(h), unreadable},
		"marker a FIFO":  {func(t *testing.T, r *rig) { mkfifo(t, filepath.Join(r.dir, "stage", id+".auth")) }, "/?result=" + id, pollHeader(h), unreadable},
		"marker too big": {func(t *testing.T, r *rig) {
			r.write(t, filepath.Join(r.dir, "stage", id+".auth"), bytes.Repeat([]byte(" "), maxMarker+1))
		}, "/?result=" + id, pollHeader(h), unreadable},
		"marker for another id": {func(t *testing.T, r *rig) { r.marker(t, id, otherDigest, r.clock.Now()) }, "/?result=" + id, pollHeader(h), unreadable},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			c.setup(t, r)
			w := r.do(t, req{target: c.target, header: c.header})
			errorLines := r.logs.FilterLevelExact(zap.ErrorLevel).Len()
			switch c.code {
			case admitted:
				if w.Code != http.StatusAccepted || body(t, w)["phase"] != "admitted" || r.h.limiter.Size() != 0 || errorLines != 0 {
					t.Fatalf("not admitted: %d %s, %d charged, %d error lines", w.Code, w.Body, r.h.limiter.Size(), errorLines)
				}
			case refused:
				// An unauthenticated request like any other, and nothing
				// in the journal but the preamble's own line.
				wantError(t, w, http.StatusUnauthorized, unauthorized)
				if r.h.limiter.Size() != 1 || errorLines != 0 {
					t.Fatalf("%d charged, %d error lines: %v", r.h.limiter.Size(), errorLines, r.logs.All())
				}
			case unreadable:
				wantError(t, w, http.StatusUnauthorized, unauthorized)
				lines := r.logs.FilterMessage("box webhook could not read its state").All()
				if r.h.limiter.Size() != 1 || errorLines != 1 || len(lines) != 1 || lines[0].ContextMap()["reading"] != "the push's marker" {
					t.Fatalf("%d charged, %d error lines: %v", r.h.limiter.Size(), errorLines, r.logs.All())
				}
			}
		})
	}
}

// TestHandlerStateErrorJournalWindow: a state file the handler cannot
// read is journaled once a minute per file, whatever repeats the
// request — a poll may for a day, before the preamble's budgets — and
// every request is still answered.
func TestHandlerStateErrorJournalWindow(t *testing.T) {
	r := newRig(t)
	h, id, digest := secret(7)
	r.marker(t, id, digest, r.clock.Now())
	mkfifo(t, filepath.Join(r.dir, "out", id+".json"))
	lines := func() int { return r.logs.FilterMessage("box webhook could not read its state").Len() }
	for range 5 {
		wantError(t, r.do(t, req{target: "/?result=" + id, header: pollHeader(h)}), http.StatusInternalServerError, "could not read the result")
	}
	if lines() != 1 {
		t.Fatalf("5 polls of one unreadable result: %d lines, want 1", lines())
	}
	// Another file is its own line.
	wantError(t, r.do(t, req{target: "/", token: r.token(t)}), http.StatusConflict, msgNoBaseline)
	must(t, os.Mkdir(filepath.Join(r.dir, "applied.json"), 0o755))
	wantError(t, r.do(t, req{target: "/", token: r.token(t)}), http.StatusInternalServerError, "could not read the baseline")
	if lines() != 2 {
		t.Fatalf("a second file: %d lines, want 2", lines())
	}
	r.clock.Advance(stateLogWindow - time.Second)
	r.do(t, req{target: "/?result=" + id, header: pollHeader(h)})
	if lines() != 2 {
		t.Fatalf("inside the window: %d lines, want 2", lines())
	}
	r.clock.Advance(time.Second)
	r.do(t, req{target: "/?result=" + id, header: pollHeader(h)})
	if lines() != 3 {
		t.Fatalf("past the window: %d lines, want 3", lines())
	}
	if r.h.limiter.Size() != 0 {
		t.Fatal("a poll was charged")
	}
}

// digestOf is raw as a header in enc, with the id and digest the box
// would derive from it.
func digestOf(raw []byte, enc *base64.Encoding) (header, id, digest string) {
	sum := sha256.Sum256(raw)
	d := hex.EncodeToString(sum[:])
	return enc.EncodeToString(raw), d[:32], d
}

func symlinkMarker(t *testing.T, r *rig, id, digest string) {
	r.marker(t, "real", digest, r.clock.Now())
	must(t, os.Symlink("real.auth", filepath.Join(r.dir, "stage", id+".auth")))
}

func TestHandlerResultByToken(t *testing.T) {
	_, id, digest := secret(7)
	for name, c := range map[string]struct {
		setup  func(t *testing.T, r *rig)
		target string
		code   int
		want   string
	}{
		"admitted": {func(t *testing.T, r *rig) { r.marker(t, id, digest, r.clock.Now()) }, "/?result=" + id, 202, ""},
		// Admitted and not settled, whatever the marker's age: only
		// root can tell a push it holds from one it lost.
		"marker past fifteen minutes": {func(t *testing.T, r *rig) { r.marker(t, id, digest, r.clock.Now().Add(-time.Hour)) }, "/?result=" + id, 202, ""},
		"marker dated ahead":          {func(t *testing.T, r *rig) { r.marker(t, id, digest, r.clock.Now().Add(time.Minute)) }, "/?result=" + id, 202, ""},
		"swept":                       {func(*testing.T, *rig) {}, "/?result=" + id, 404, fmt.Sprintf(msgNoResult, id)},
		// stage/ broken for every id: reported, as the box's error, to
		// an authenticated request.
		"stage not a directory": {func(t *testing.T, r *rig) {
			must(t, os.RemoveAll(filepath.Join(r.dir, "stage")))
			r.write(t, filepath.Join(r.dir, "stage"), nil)
		}, "/?result=" + id, 500, "could not read the push's marker"},
		"31 hex":             {func(*testing.T, *rig) {}, "/?result=" + id[:31], 400, msgResultID},
		"33 hex":             {func(*testing.T, *rig) {}, "/?result=" + id + "0", 400, msgResultID},
		"upper case":         {func(*testing.T, *rig) {}, "/?result=" + strings.ToUpper(id), 400, msgResultID},
		"a path":             {func(*testing.T, *rig) {}, "/?result=..%2F..%2Fapplied", 400, msgResultID},
		"empty":              {func(*testing.T, *rig) {}, "/?result=", 400, msgResultID},
		"bare key":           {func(*testing.T, *rig) {}, "/?result", 400, msgResultID},
		"twice":              {func(*testing.T, *rig) {}, "/?result=" + id + "&result=" + id, 400, msgResultID},
		"another param":      {func(*testing.T, *rig) {}, "/?result=" + id + "&x=1", 400, msgResultID},
		"only another param": {func(*testing.T, *rig) {}, "/?x=1", 400, msgResultID},
		"malformed":          {func(*testing.T, *rig) {}, "/?result=%ZZ", 400, msgResultID},
		"result a symlink": {func(t *testing.T, r *rig) {
			r.result(t, result{ID: id, Phase: "applied"})
			must(t, os.Rename(filepath.Join(r.dir, "out", id+".json"), filepath.Join(r.dir, "out", "real.json")))
			must(t, os.Symlink("real.json", filepath.Join(r.dir, "out", id+".json")))
		}, "/?result=" + id, 500, "could not read the result"},
		"result a FIFO": {func(t *testing.T, r *rig) { mkfifo(t, filepath.Join(r.dir, "out", id+".json")) }, "/?result=" + id, 500, "not a regular file"},
		"result a dir":  {func(t *testing.T, r *rig) { must(t, os.Mkdir(filepath.Join(r.dir, "out", id+".json"), 0o755)) }, "/?result=" + id, 500, "not a regular file"},
		"result too big": {func(t *testing.T, r *rig) {
			r.result(t, result{ID: id, Phase: "applied", Diff: strings.Repeat("x", maxResult)})
		}, "/?result=" + id, 500, "larger than"},
		"result for another id": {func(t *testing.T, r *rig) {
			r.writeJSON(t, filepath.Join(r.dir, "out", id+".json"), result{ID: strings.Repeat("0", 32), Phase: "applied"})
		}, "/?result=" + id, 500, "names another id"},
		"unknown phase": {func(t *testing.T, r *rig) {
			r.result(t, result{ID: id, Phase: "installing\n" + strings.Repeat("x", 1000)})
		}, "/?result=" + id, 500, `phase "installing\nxxx`},
		"marker malformed": {func(t *testing.T, r *rig) {
			r.write(t, filepath.Join(r.dir, "stage", id+".auth"), []byte(`{"sha256":"x"}`))
		}, "/?result=" + id, 500, "could not read the push's marker"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			c.setup(t, r)
			w := r.do(t, req{target: c.target, token: r.token(t)})
			if c.code == 202 {
				if got := body(t, w); w.Code != 202 || len(got) != 1 || got["phase"] != "admitted" {
					t.Fatalf("%d %s", w.Code, w.Body)
				}
				return
			}
			wantError(t, w, c.code, c.want)
			if len(w.Body.String()) > 600 {
				t.Errorf("unbounded: %d bytes", w.Body.Len())
			}
		})
	}
}

// TestHandlerResultIsFiltered: a result is re-encoded from its known
// fields and filtered — bundle-derived text loses what the shape and
// entropy layers catch, a field root never meant to send is dropped,
// and the protocol's own ids stand.
func TestHandlerResultIsFiltered(t *testing.T) {
	r := newRig(t)
	h, id, digest := secret(9)
	r.marker(t, id, digest, r.clock.Now())
	const generated = "Zx81QmT0vLp4RkWc9NsJ2hYeAqUoBi7G" // gitleaks:allow
	raw := map[string]any{
		"id": id, "commit": sha, "phase": "refused", "box_webhook": "deploy.example.com",
		"error": "hotserve validate: token " + jwt + " rejected",
		"diff":  "-\tsecret " + generated + "\n+\tsecret none\n",
		"prev":  "the previous file's bytes",
	}
	r.writeJSON(t, filepath.Join(r.dir, "out", id+".json"), raw)
	w := r.do(t, req{target: "/?result=" + id, header: pollHeader(h)})
	got := w.Body.String()
	for _, leak := range []string{jwt, generated, "prev", "the previous file's bytes"} {
		if strings.Contains(got, leak) {
			t.Errorf("%q in the answer: %s", leak, got)
		}
	}
	for _, stands := range []string{`"id":"` + id, `"commit":"` + sha, "[redacted:jwt]"} {
		if !strings.Contains(got, stands) {
			t.Errorf("%q not in the answer: %s", stands, got)
		}
	}
}
