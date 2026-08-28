package liveswap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"

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

// Handler implements the liveswap webhook endpoint. Mount it in its own
// (HTTPS) site block; the final path segment names the app:
//
//	POST /<app>  deploys: {"url": "...", "version": "...", "auth_header": "..."}
//	GET  /<app>  returns status JSON (phase, current version, last deploy)
//
// Responses are synchronous — the POST returns when the deploy has
// fully succeeded (200) or failed (5xx with the old version still
// serving), so `curl --fail` makes CI red exactly when it should be.
type Handler struct {
	app    *App
	logger *zap.Logger
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
	if !ok {
		h.logger.Warn("webhook auth failed",
			zap.String("app", name), zap.String("remote", r.RemoteAddr))
		return respondJSON(w, http.StatusUnauthorized, map[string]string{
			"error": "invalid or missing deploy token (Authorization: Bearer <jwt>)",
		})
	}
	if ma == nil {
		return respondJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("unknown app %q", name)})
	}

	switch r.Method {
	case http.MethodGet:
		return respondJSON(w, http.StatusOK, ma.status())
	case http.MethodPost:
		return h.deploy(w, r, ma, who)
	default:
		w.Header().Set("Allow", "GET, POST")
		return respondJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
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
	var req deployRequest
	// Read one byte past the cap: exceeding it proves the payload is
	// oversized, which deserves an honest 413 — truncating at the cap
	// would surface as a misleading "invalid JSON" 400.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPayloadBytes+1))
	if err != nil {
		return respondJSON(w, http.StatusBadRequest, map[string]string{"error": "reading body: " + err.Error()})
	}
	if len(body) > maxPayloadBytes {
		return respondJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
			"error": fmt.Sprintf("payload exceeds %d bytes", maxPayloadBytes)})
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON payload: " + err.Error()})
	}
	if req.URL == "" {
		return respondJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "url is required"})
	}
	if !validVersion(req.Version) {
		return respondJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": fmt.Sprintf("version must match %s and not be . or ..", versionRe)})
	}
	// Go's transport would refuse a control character in the header
	// value anyway, but as an opaque 500 at fetch time; catching it
	// here names the field while it is still cheap to fix.
	if strings.ContainsFunc(req.AuthHeader, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return respondJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "auth_header contains control characters"})
	}
	return h.runDeploy(w, r, ma, req, by)
}

// deployPush streams an uploaded gzip tarball to a staging file and
// deploys it — no artifact host needed. Version comes from the query
// string because the body is the artifact.
func (h *Handler) deployPush(w http.ResponseWriter, r *http.Request, ma *managedApp, by string) error {
	version := r.URL.Query().Get("version")
	if !validVersion(version) {
		return respondJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": fmt.Sprintf("version query param must match %s and not be . or ..", versionRe)})
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
			"error": fmt.Sprintf("uploaded artifact exceeds max_artifact_size (%d bytes)", spec.maxArtifactSize)})
	case errors.As(err, &stgErr):
		// A local filesystem failure (mkdir/create/write/close, e.g. a
		// full disk) is a server error, like a failed URL download.
		return respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "staging upload: " + err.Error()})
	case err != nil:
		// Otherwise the body read itself failed — a bad request.
		return respondJSON(w, http.StatusBadRequest, map[string]string{"error": "reading upload: " + err.Error()})
	}
	// Backstop cleanup: fetch removes the archive once it extracts it,
	// but if the pipeline returns before fetch it would leak.
	defer func() { _ = os.Remove(archive) }()
	req := deployRequest{Version: version, localArchive: archive, by: by}
	h.logDeployAuthorized(r, ma, req)
	return h.mapDeployResult(w, ma, ma.deployLocked(r.Context(), req, c))
}

// deployRollback relaunches an already-extracted on-disk release.
func (h *Handler) deployRollback(w http.ResponseWriter, r *http.Request, ma *managedApp, by string) error {
	version := r.URL.Query().Get("rollback")
	if !validVersion(version) {
		return respondJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": fmt.Sprintf("rollback version must match %s and not be . or ..", versionRe)})
	}
	return h.runDeploy(w, r, ma, deployRequest{Version: version, rollback: true}, by)
}

// runDeploy records the authorizing source, runs the pipeline, and maps
// the outcome to a status code.
func (h *Handler) runDeploy(w http.ResponseWriter, r *http.Request, ma *managedApp, req deployRequest, by string) error {
	req.by = by
	h.logDeployAuthorized(r, ma, req)
	return h.mapDeployResult(w, ma, ma.Deploy(r.Context(), req))
}

func (h *Handler) logDeployAuthorized(r *http.Request, ma *managedApp, req deployRequest) {
	h.logger.Info("deploy authorized",
		zap.String("app", ma.name), zap.String("via", req.by),
		zap.String("source", req.source()), zap.String("remote", r.RemoteAddr))
}

// mapDeployResult turns a pipeline outcome into the webhook response.
func (h *Handler) mapDeployResult(w http.ResponseWriter, ma *managedApp, err error) error {
	status := ma.status()
	var vErr validationError
	switch {
	case err == nil:
		return respondJSON(w, http.StatusOK, status)
	case errors.Is(err, errDeployInProgress):
		return respondJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.As(err, &vErr):
		return respondJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
	case errors.Is(err, context.Canceled):
		// The client hung up mid-deploy; nobody is reading this.
		return nil
	default:
		return respondJSON(w, http.StatusInternalServerError, map[string]any{
			"error":  err.Error(),
			"status": status, // shows the old version still serving
		})
	}
}

// isGzipUpload reports whether the request body is a pushed artifact.
func isGzipUpload(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	switch strings.TrimSpace(ct) {
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

func respondJSON(w http.ResponseWriter, code int, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	return json.NewEncoder(w).Encode(v)
}

// Interface guards.
var (
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
)
