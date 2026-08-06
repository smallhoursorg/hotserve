package liveswap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(Handler{})
}

// secretHeader carries the shared deploy secret from CI.
const secretHeader = "X-Liveswap-Secret"

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
	// unknown app the provided secret is compared against the global
	// default so the timing and status never leak valid app names to
	// unauthenticated callers.
	configured := h.app.WebhookSecret
	if ma != nil {
		configured = ma.currentSpec().secret
	}
	provided := r.Header.Get(secretHeader)
	if configured == "" || !secretsEqual(provided, configured) {
		h.logger.Warn("webhook auth failed",
			zap.String("app", name), zap.String("remote", r.RemoteAddr))
		return respondJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or missing " + secretHeader})
	}
	if ma == nil {
		return respondJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("unknown app %q", name)})
	}

	switch r.Method {
	case http.MethodGet:
		return respondJSON(w, http.StatusOK, ma.status())
	case http.MethodPost:
		return h.deploy(w, r, ma)
	default:
		w.Header().Set("Allow", "GET, POST")
		return respondJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (h *Handler) deploy(w http.ResponseWriter, r *http.Request, ma *managedApp) error {
	var req deployRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPayloadBytes))
	if err != nil {
		return respondJSON(w, http.StatusBadRequest, map[string]string{"error": "reading body: " + err.Error()})
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON payload: " + err.Error()})
	}
	if req.URL == "" {
		return respondJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "url is required"})
	}
	if !versionRe.MatchString(req.Version) {
		return respondJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": fmt.Sprintf("version must match %s", versionRe)})
	}

	err = ma.Deploy(r.Context(), req)
	status := ma.status()
	switch {
	case err == nil:
		return respondJSON(w, http.StatusOK, status)
	case errors.Is(err, errDeployInProgress):
		return respondJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.As(err, &validationError{}):
		return respondJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
	case errors.Is(err, context.Canceled):
		// The CI client hung up mid-deploy; nobody is reading this.
		return nil
	default:
		return respondJSON(w, http.StatusInternalServerError, map[string]any{
			"error":  err.Error(),
			"status": status, // shows the old version still serving
		})
	}
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
