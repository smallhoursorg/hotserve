package liveswap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(Handler{})
}

// bearerToken extracts the deploy JWT from `Authorization: Bearer
// <jwt>`. Bearer is the only accepted transport: Caddy redacts the
// Authorization header from access logs automatically (the retired
// X-Liveswap-Secret custom header did not, which leaked it).
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	if auth := r.Header.Get("Authorization"); len(auth) > len(prefix) &&
		strings.EqualFold(auth[:len(prefix)], prefix) {
		return auth[len(prefix):]
	}
	return ""
}

// maxPayloadBytes bounds the webhook JSON body.
const maxPayloadBytes = 64 * 1024

// loggedAppName bounds a request-supplied app name for the log: no
// configured name is longer than appNameRe allows, so anything past
// that is noise.
func loggedAppName(name string) string {
	if len(name) <= appNameMaxLen {
		return name
	}
	return name[:appNameMaxLen] + "..."
}

// Handler implements the liveswap webhook endpoint. Mount it in its own
// (HTTPS) site block; the final path segment names the app:
//
//	POST /<app>  deploys: {"url": "...", "version": "...", "auth_header": "..."}
//	GET  /<app>  returns status JSON (phase, current version, last deploy)
//
// Responses are synchronous — the POST returns when the deploy has
// fully succeeded (200) or failed (5xx with the old version, if there
// was one, still serving), so `curl --fail-with-body` makes CI red exactly when it
// should be, with the reason in the body.
type Handler struct {
	app     *App
	logger  *zap.Logger
	limiter *authLimiter
}

// CaddyModule returns the Caddy module information.
func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.liveswap_webhook",
		New: func() caddy.Module { return new(Handler) },
	}
}

// Provision binds the handler to the liveswap app.
func (h *Handler) Provision(ctx caddy.Context) error {
	h.logger = ctx.Logger()
	appModule, err := ctx.App("liveswap")
	if err != nil {
		return err
	}
	h.app = appModule.(*App)
	if len(h.app.Apps) == 0 {
		return fmt.Errorf("liveswap_webhook is configured but no apps are defined in the liveswap global options")
	}
	h.limiter = webhookAuthLimiter
	return nil
}

// ServeHTTP is terminal: it never calls the next handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, _ caddyhttp.Handler) error {
	name := path.Base(path.Clean(r.URL.Path))
	ma := h.app.managedApp(name)

	// Authenticate before revealing whether the app exists. For an
	// unknown app the token is verified against the global trust
	// sources, so timing and status never leak valid app names to
	// unauthenticated callers (with no global trust configured, an
	// unknown app is simply unauthenticable → 401, still no leak).
	verifiers := h.app.globalVerifiers
	if ma != nil {
		verifiers = ma.currentVerifiers()
	}
	who, ok := authorize(r.Context(), verifiers, bearerToken(r))
	key := clientKey(r)
	if !ok {
		// What a failure costs in the journal is the limiter's call
		// (see authLimiter): the count of lines, and — the name being
		// request input, logged truncated — the size of each.
		v := h.limiter.fail(key)
		if v.log {
			h.logger.Warn("webhook auth failed",
				zap.String("app", loggedAppName(name)), zap.String("remote", key))
		}
		if v.trippedKey {
			h.logger.Warn("webhook auth failures from this address throttled: further ones are answered 429 and not logged",
				zap.String("remote", key), zap.Int("failures", authFailBudget), zap.Duration("window", authFailWindow))
		}
		if v.trippedGlobal {
			h.logger.Warn("webhook auth failures throttled process-wide: further ones are not logged",
				zap.Int("failures", authFailGlobalBudget), zap.Duration("window", authFailWindow))
		}
		if v.throttled {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(v.retryAfter.Seconds()))))
			return respondJSON(w, http.StatusTooManyRequests, map[string]string{
				"error": "too many failed deploy authentications from this address; retry later",
			}, nil)
		}
		return respondJSON(w, http.StatusUnauthorized, map[string]string{
			"error": "invalid or missing deploy token (Authorization: Bearer <jwt>)",
		}, nil)
	}
	h.limiter.clear(key)
	if ma == nil {
		return respondJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("unknown app %q", name)}, nil)
	}

	switch r.Method {
	case http.MethodGet:
		s := ma.status()
		return respondJSON(w, http.StatusOK, s, ma.redactorFor(s))
	case http.MethodPost:
		return h.deploy(w, r, ma, who)
	default:
		w.Header().Set("Allow", "GET, POST")
		return respondJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"}, ma.redactorFor(statusSnapshot{}))
	}
}

// deploy dispatches on the request shape:
//   - a gzip body  → push an uploaded artifact  (POST /<app>?version=v2)
//   - ?rollback=v1 → relaunch an on-disk release (POST /<app>?rollback=v1)
//   - otherwise    → pull from a URL             (JSON {url, version})
func (h *Handler) deploy(w http.ResponseWriter, r *http.Request, ma *managedApp, by string) error {
	switch {
	case isGzipUpload(r):
		return h.deployPush(w, r, ma, by)
	case r.URL.Query().Get("rollback") != "":
		return h.deployRollback(w, r, ma, by)
	default:
		return h.deployURL(w, r, ma, by)
	}
}

// deployURL is the default path: a JSON body naming an artifact URL to
// pull, size-capped like any control payload.
func (h *Handler) deployURL(w http.ResponseWriter, r *http.Request, ma *managedApp, by string) error {
	// Read one byte past the cap: exceeding it proves the payload is
	// oversized, which deserves an honest 413 — truncating at the cap
	// would surface as a misleading "invalid JSON" 400.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPayloadBytes+1))
	if err != nil {
		return respondJSON(w, http.StatusBadRequest, map[string]string{"error": "reading body: " + err.Error()}, ma.redactorFor(statusSnapshot{}))
	}
	if len(body) > maxPayloadBytes {
		return respondJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
			"error": fmt.Sprintf("payload exceeds %d bytes", maxPayloadBytes)}, ma.redactorFor(statusSnapshot{}))
	}
	p, status, msg := parseDeployPayload(body)
	if status != 0 {
		return respondJSON(w, status, map[string]string{"error": msg}, ma.redactorFor(statusSnapshot{}))
	}
	return h.runDeploy(w, r, ma, p.request(), by)
}

// parseDeployPayload is the body half of deployURL: the size-capped
// bytes in, either a payload the pipeline may act on or the 4xx that
// refuses it. Every check the JSON body gets before it becomes a
// deployRequest lives here (the URL allowlist is downstream, in the
// download), and it is kept free of the ResponseWriter so
// FuzzDeployRequest drives the production gate itself. status is 0 on
// success.
func parseDeployPayload(body []byte) (p deployPayload, status int, msg string) {
	if err := json.Unmarshal(body, &p); err != nil {
		return deployPayload{}, http.StatusBadRequest, "invalid JSON payload: " + err.Error()
	}
	if p.URL == "" {
		return deployPayload{}, http.StatusUnprocessableEntity, "url is required"
	}
	if !validVersion(p.Version) {
		return deployPayload{}, http.StatusUnprocessableEntity, fmt.Sprintf("version must match %s", versionRe)
	}
	// Go's transport would refuse a control character in the header
	// value anyway, but as an opaque 500 at fetch time; catching it
	// here names the field while it is still cheap to fix.
	if strings.ContainsFunc(p.AuthHeader, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return deployPayload{}, http.StatusUnprocessableEntity, "auth_header contains control characters"
	}
	return p, 0, ""
}

// deployPush streams an uploaded gzip tarball to a staging file and
// deploys it — no artifact host needed. Version comes from the query
// string because the body is the artifact.
func (h *Handler) deployPush(w http.ResponseWriter, r *http.Request, ma *managedApp, by string) error {
	version := r.URL.Query().Get("version")
	if !validVersion(version) {
		return respondJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": fmt.Sprintf("version query param must match %s", versionRe)}, ma.redactorFor(statusSnapshot{}))
	}
	// Acquire the per-app deploy lock BEFORE staging the upload: a
	// concurrent push then gets an immediate 409 instead of streaming a
	// full tarball to disk only to lose the lock, and total staged bytes
	// stay bounded to a single upload.
	if !ma.deployMu.TryLock() {
		return h.mapDeployResult(w, ma, errDeployInProgress)
	}
	defer ma.deployMu.Unlock()

	// One snapshot for both staging and the pipeline, so a reload during
	// the upload can't stage under one spec and extract under another.
	c := ma.snapshot()
	spec := c.spec
	archive, err := stageUpload(r.Body, spec.dirs.tmp, spec.maxArtifactSize)
	var stgErr *stagingError
	switch {
	case errors.Is(err, errArtifactTooLarge):
		return respondJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
			"error": fmt.Sprintf("uploaded artifact exceeds max_artifact_size (%d bytes)", spec.maxArtifactSize)}, ma.redactorFor(statusSnapshot{}))
	case errors.As(err, &stgErr):
		// A local filesystem failure (mkdir/create/write/close, e.g. a
		// full disk) is a server error, like a failed URL download.
		return respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "staging upload: " + err.Error()}, ma.redactorFor(statusSnapshot{}))
	case err != nil:
		// Otherwise the body read itself failed — a bad request.
		return respondJSON(w, http.StatusBadRequest, map[string]string{"error": "reading upload: " + err.Error()}, ma.redactorFor(statusSnapshot{}))
	}
	// Backstop cleanup: fetch removes the archive once it extracts it,
	// but if the pipeline returns before fetch it would leak.
	defer func() { _ = os.Remove(archive) }()
	req := deployRequest{version: version, localArchive: archive, by: by}
	h.logDeployAuthorized(r, ma, req)
	return h.finishDeploy(w, r, ma, func(onPhase func(string)) error {
		req.onPhase, c.onPhase = onPhase, onPhase
		return ma.deployLocked(r.Context(), req, c)
	})
}

// deployRollback relaunches an already-extracted on-disk release.
func (h *Handler) deployRollback(w http.ResponseWriter, r *http.Request, ma *managedApp, by string) error {
	version := r.URL.Query().Get("rollback")
	if !validVersion(version) {
		return respondJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": fmt.Sprintf("rollback version must match %s", versionRe)}, ma.redactorFor(statusSnapshot{}))
	}
	return h.runDeploy(w, r, ma, deployRequest{version: version, rollback: true}, by)
}

// runDeploy records the authorizing source, runs the pipeline, and maps
// the outcome to a status code — or, when the client asked for it,
// streams the phases as they happen and ends with the same outcome.
func (h *Handler) runDeploy(w http.ResponseWriter, r *http.Request, ma *managedApp, req deployRequest, by string) error {
	req.by = by
	h.logDeployAuthorized(r, ma, req)
	if !wantsStream(r) {
		return h.mapDeployResult(w, ma, ma.Deploy(r.Context(), req))
	}
	st := newDeployStream(w, ma)
	req.onPhase = st.phase
	return st.finish(ma, ma.Deploy(r.Context(), req))
}

// finishDeploy is the push path's runDeploy: the pipeline already ran
// under the lock the handler took, and its outcome is written the way
// the request asked for.
func (h *Handler) finishDeploy(w http.ResponseWriter, r *http.Request, ma *managedApp, run func(onPhase func(string)) error) error {
	if !wantsStream(r) {
		return h.mapDeployResult(w, ma, run(nil))
	}
	st := newDeployStream(w, ma)
	return st.finish(ma, run(st.phase))
}

func (h *Handler) logDeployAuthorized(r *http.Request, ma *managedApp, req deployRequest) {
	h.logger.Info("deploy authorized",
		zap.String("app", ma.name), zap.String("via", req.by),
		zap.String("source", req.source()), zap.String("remote", r.RemoteAddr))
}

// mapDeployResult turns a pipeline outcome into the webhook response.
func (h *Handler) mapDeployResult(w http.ResponseWriter, ma *managedApp, err error) error {
	code, body, rd := deployOutcome(ma, err)
	if body == nil {
		// The client hung up mid-deploy; nobody is reading this.
		return nil
	}
	return respondJSON(w, code, body, rd)
}

// deployOutcome is the status code and body a pipeline outcome maps
// to, with the app's filter for it; a nil body is a client that hung
// up, which nothing is written for.
func deployOutcome(ma *managedApp, err error) (int, any, *redactor) {
	status := ma.status()
	rd := ma.redactorFor(status)
	var vErr validationError
	switch {
	case err == nil:
		return http.StatusOK, status, rd
	case errors.Is(err, errDeployInProgress):
		return http.StatusConflict, map[string]string{"error": err.Error()}, rd
	case errors.As(err, &vErr):
		return http.StatusUnprocessableEntity, map[string]string{"error": err.Error()}, rd
	case errors.Is(err, context.Canceled):
		return 0, nil, rd
	default:
		return http.StatusInternalServerError, map[string]any{
			"error":  err.Error(),
			"status": status, // shows the old version still serving
		}, rd
	}
}

// ndjson is the media type a client sends in Accept to have the deploy
// streamed: one JSON object per line as the pipeline moves, the last
// being the body the single response carries, plus its status code.
const ndjson = "application/x-ndjson"

// wantsStream reports whether the request asked for the stream: the
// media type, exactly, among the items of every Accept header sent (a
// list, with or without parameters), not refused by a q of zero, and
// nothing that merely resembles it.
func wantsStream(r *http.Request) bool {
	for _, v := range r.Header.Values("Accept") {
		for _, item := range strings.Split(v, ",") {
			mt, params, err := mime.ParseMediaType(strings.TrimSpace(item))
			if err != nil || mt != ndjson {
				continue
			}
			if q, ok := params["q"]; ok {
				// A qvalue is 0 to 1; anything else — NaN, Inf, 2 —
				// is not one, and does not switch the protocol.
				if f, err := strconv.ParseFloat(q, 64); err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 || f > 1 {
					continue
				}
			}
			return true
		}
	}
	return false
}

// deployStream writes a deploy as JSON lines. The stream begins with
// the first phase: from then on the status line is 200 whatever the
// outcome — it went out before the outcome existed — and the last line
// carries "http_status", the code the single response would have had,
// which a client that streams reads instead. An outcome reached before
// any phase (a version refused, a deploy already running) has nothing
// streamed yet and is the single response with its real code. Every
// line passes the app's filter.
type deployStream struct {
	w     http.ResponseWriter
	ma    *managedApp
	begun bool
}

func newDeployStream(w http.ResponseWriter, ma *managedApp) *deployStream {
	return &deployStream{w: w, ma: ma}
}

// write sends one filtered JSON object as a line and flushes it. A
// write that fails is a client gone; the deploy carries on (its
// context says when to stop) and the last line's write reports it.
func (s *deployStream) write(filtered string) error {
	if !s.begun {
		s.begun = true
		s.w.Header().Set("Content-Type", ndjson)
		s.w.Header().Set("Cache-Control", "no-store")
		s.w.WriteHeader(http.StatusOK)
	}
	if _, err := s.w.Write(append([]byte(filtered), '\n')); err != nil {
		return err
	}
	_ = http.NewResponseController(s.w).Flush()
	return nil
}

// phase is the pipeline's listener: one line per phase entered. Like
// the last line, its fixed fields go on after the filter: a filter
// that withholds the whole body (an unreadable env_file) then leaves
// its diagnostic in the line and the event and phase beside it.
func (s *deployStream) phase(phase string) {
	raw, err := json.Marshal(map[string]any{"at": time.Now().UTC()})
	if err != nil {
		return
	}
	filtered := s.ma.redactorFor(statusSnapshot{}).redactJSON(raw)
	_ = s.write(fmt.Sprintf(`%s,"event":"phase","phase":%q}`, filtered[:len(filtered)-1], phase))
}

// finish writes the outcome as the last line: the single response's
// body with "event":"done" and "http_status" added — or, when no
// phase was ever streamed, as the single response itself.
func (s *deployStream) finish(ma *managedApp, err error) error {
	code, body, rd := deployOutcome(ma, err)
	if body == nil {
		return nil
	}
	if !s.begun {
		return respondJSON(s.w, code, body, rd)
	}
	// The single response's bytes, filtered, with two fields appended
	// after the filter: not a re-marshalled map, so the fields keep the
	// order the single response has (a client reading "the last phase"
	// by position relies on it — the examples' deploy.sh does); and
	// after, so the outcome's markers survive a filter that withholds
	// the whole body (an unreadable env_file). redactJSON always
	// returns one JSON object, so the closing brace is where it ends.
	raw, mErr := json.Marshal(body)
	if mErr != nil {
		return mErr
	}
	filtered := rd.redactJSON(raw)
	return s.write(fmt.Sprintf(`%s,"event":"done","http_status":%d}`, filtered[:len(filtered)-1], code))
}

// isGzipUpload reports whether the request body is a pushed artifact.
func isGzipUpload(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	// Media types are case-insensitive (RFC 7231), so normalize before
	// matching — otherwise `Application/Gzip` would fall through to the
	// JSON URL path and fail as invalid JSON.
	switch strings.ToLower(strings.TrimSpace(ct)) {
	case "application/gzip", "application/x-gzip", "application/octet-stream":
		return true
	}
	return false
}

// errArtifactTooLarge signals a pushed upload that exceeded the cap.
var errArtifactTooLarge = errors.New("artifact too large")

// stagingError marks an upload failure that is a local filesystem/server
// fault (mkdir, create, disk write, close), as opposed to a bad request
// body — so the handler can answer 5xx vs 400.
type stagingError struct{ err error }

func (e *stagingError) Error() string { return e.err.Error() }
func (e *stagingError) Unwrap() error { return e.err }

// writeTracker records whether a write to the underlying file failed, so
// io.Copy's combined error can be attributed to the disk (server) rather
// than the request body (client).
type writeTracker struct {
	w      io.Writer
	failed bool
}

func (t *writeTracker) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if err != nil {
		t.failed = true
	}
	return n, err
}

// stageUpload streams the request body to a temp file, capped at
// maxBytes. Returns the path; the caller owns cleanup. Local filesystem
// failures come back wrapped in *stagingError (→ 5xx); a body-read
// failure comes back bare (→ 400); an over-cap body → errArtifactTooLarge.
func stageUpload(body io.Reader, tmpDir string, maxBytes int64) (string, error) {
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		return "", &stagingError{err}
	}
	f, err := os.CreateTemp(tmpDir, "push-*.tar.gz")
	if err != nil {
		return "", &stagingError{err}
	}
	path := f.Name()
	// One byte past the cap proves it is oversized.
	wt := &writeTracker{w: f}
	n, copyErr := io.Copy(wt, io.LimitReader(body, maxBytes+1))
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(path)
		if wt.failed {
			return "", &stagingError{copyErr} // disk write failed
		}
		return "", copyErr // request body read failed
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return "", &stagingError{closeErr}
	}
	if n > maxBytes {
		_ = os.Remove(path)
		return "", errArtifactTooLarge
	}
	return path, nil
}

// respondJSON writes every body the webhook sends, through the
// response filter (redact.go). r is the app's filter — every site
// with the app in scope passes ma.redactorFor, the 405 included —
// and nil only for the bodies written before an app is known (401,
// 429, 404), where the shape and entropy layers still apply. When a known secret was found
// the object gains a `redacted_env` field naming the keys.
func respondJSON(w http.ResponseWriter, code int, v any, r *redactor) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, err = w.Write(append([]byte(r.redactJSON(raw)), '\n'))
	return err
}

// Interface guards.
var (
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
)
