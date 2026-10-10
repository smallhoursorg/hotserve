package box

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"

	"github.com/smallhoursorg/hotserve/box/proof"
	"github.com/smallhoursorg/hotserve/liveswap"
	"github.com/smallhoursorg/hotserve/liveswap/deploytrust"
)

func init() {
	caddy.RegisterModule(Handler{})
}

// Handler is `box_webhook` (DESIGN-box.md, "Handler contract"). It
// answers on exactly the path `/` of its site and passes every other
// path to the next handler:
//
//	GET  /              the baseline: {commit, box_webhook, sha256}
//	GET  /?result=<id>  a push's result (push.go)
//	POST /              a push (push.go, admission.go, validate.go)
//
// Another method on `/` is 405 before anything else. Every GET and
// POST on `/` is authenticated first, on the limiter liveswap's
// webhook uses, so an unauthenticated caller gets the same flat 401 a
// deploy host gives. Every body passes liveswap's response filter.
type Handler struct {
	app     *App
	logger  *zap.Logger
	limiter *deploytrust.Limiter

	// Where the handler reads: installedFile and exchangeDir, set in
	// Provision; a test's temporary files otherwise.
	installed string
	dir       string

	// A push's seams: the proof's verifier (ssh-keygen as the hotserve
	// uid, step 5), step 6's children, the clock step 8 waits on, and
	// the drop's test hooks.
	verifier  *proof.Verifier
	validator validator
	clock     Clock
	hooks     handlerHooks
}

// CaddyModule returns the Caddy module information.
func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.box_webhook",
		New: func() caddy.Module { return new(Handler) },
	}
}

// Provision binds the handler to the box app. AppIfConfigured, not
// App: App would instantiate an empty `box` app, and the webhook would
// then fail every request rather than the config load.
func (h *Handler) Provision(ctx caddy.Context) error {
	h.logger = ctx.Logger()
	app, err := ctx.AppIfConfigured("box")
	if err != nil {
		return fmt.Errorf("box_webhook needs the box global option, which says who may push and who may sign: %w", err)
	}
	h.app = app.(*App)
	h.limiter = deploytrust.Shared()
	h.installed, h.dir = installedFile, exchangeDir
	h.verifier = &proof.Verifier{} // RunAs 0: as the hotserve uid, in the service's PrivateTmp
	h.validator = newChildValidator(h.x("stage"))
	h.clock = realClock{}
	return nil
}

// The handler's own messages (DESIGN-box.md, "Message catalogue").
const (
	msgMethod     = "method not allowed"
	msgNoBaseline = "this box has no baseline; hotserve init <dir> <sha> as root on the box sets one"
)

// ServeHTTP answers on `/` and passes every other path on.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if r.URL.Path != "/" {
		return next.ServeHTTP(w, r)
	}
	// A query is present when the target has a `?`, an empty one
	// included (ForceQuery): `GET /?` is a result poll with no id, not
	// the baseline.
	queried := r.URL.RawQuery != "" || r.URL.ForceQuery
	switch {
	case r.Method == http.MethodGet && !queried:
		return h.status(w, r)
	case r.Method == http.MethodGet:
		return h.result(w, r)
	case r.Method == http.MethodPost:
		return h.push(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		return respond(w, http.StatusMethodNotAllowed, errorBody(msgMethod))
	}
}

// authenticate is the preamble (deploytrust): ok=false means the
// refusal is written. route names the request in the preamble's
// journal lines, beside the address.
func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request, route string) (deploytrust.Identity, bool, error) {
	ident, refusal := h.limiter.Authenticate(r, h.app.verifiers, h.logger, "box_request", route)
	if refusal != nil {
		return ident, false, refusal.Write(w, func(code int, body any) error { return respond(w, code, body) })
	}
	return ident, true, nil
}

type statusBody struct {
	Commit     string `json:"commit"`
	BoxWebhook string `json:"box_webhook"`
	SHA256     string `json:"sha256"`
}

// status is `GET /`: the commit the box runs, from applied.json, and
// the host and digest of the file it runs, read now — what a push's
// no_change fast path compares (step 4) — so a console edit since the
// last apply shows.
func (h *Handler) status(w http.ResponseWriter, r *http.Request) error {
	if _, ok, err := h.authenticate(w, r, "status"); !ok {
		return err
	}
	base, err := readApplied(h.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return respond(w, http.StatusConflict, errorBody(msgNoBaseline))
	}
	if err != nil {
		return h.boxError(w, "the baseline", err)
	}
	file, err := readFile(h.installed, proof.MaxCaddyfile, true)
	if err != nil {
		return h.boxError(w, "the Caddyfile this box runs", err)
	}
	shape, err := Walk(file)
	if err != nil {
		var ref *Refusal
		if !errors.As(err, &ref) {
			return h.boxError(w, "the Caddyfile this box runs", err)
		}
		h.logStateError("the Caddyfile this box runs", err)
		return respond(w, http.StatusInternalServerError, errorBody("the Caddyfile this box runs "+ref.Reason))
	}
	sum := sha256.Sum256(file)
	digest := hex.EncodeToString(sum[:])
	return respond(w, http.StatusOK, statusBody{Commit: base.SHA, BoxWebhook: shape.Host, SHA256: digest},
		base.SHA, shape.Host, digest)
}

// boxError answers a failure that is the box's, never the caller's:
// 500 with the error, and the journal line, both bounded.
func (h *Handler) boxError(w http.ResponseWriter, what string, err error) error {
	h.logStateError(what, err)
	return respond(w, http.StatusInternalServerError, errorBody("could not read "+what+": "+proof.Bound(err.Error())))
}

// logStateError journals a state file the handler cannot read, bounded.
// Only an authenticated request reaches one: the preamble decides who
// can make the line, not how many — a token holder can repeat `GET /`
// against a box whose own files fail, and each request is a line, as
// each is an answer.
func (h *Handler) logStateError(what string, err error) {
	h.logger.Error("box webhook could not read its state", zap.String("reading", what), zap.String("error", proof.Bound(err.Error())))
}

func errorBody(msg string) map[string]string { return map[string]string{"error": msg} }

// respond writes body through liveswap's response filter: the shape
// and entropy layers, with safe — the protocol's own commit, digest and
// host, each one the caller checked against its grammar — held out of
// them, since a 40-hex commit and a 64-hex digest are exactly what the
// entropy layer exists to catch.
func respond(w http.ResponseWriter, code int, body any, safe ...string) error {
	return liveswap.RespondJSON(w, code, body, liveswap.NewRedactor(nil, safe))
}

var (
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
)
