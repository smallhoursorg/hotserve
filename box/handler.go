package box

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

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
//	GET  /?result=<id>  what the push with that id came to
//	POST /              a push; refused 501 until the applier ships
//
// Every request but a result poll that carries its push's poll secret
// is authenticated first, on the limiter liveswap's webhook uses, so an
// unauthenticated caller gets the same flat 401 a deploy host gives.
// Every body passes liveswap's response filter.
type Handler struct {
	app     *App
	logger  *zap.Logger
	limiter *deploytrust.Limiter

	// Where the handler reads: installedFile and exchangeDir, set in
	// Provision; a test's temporary files otherwise.
	installed string
	dir       string
	now       func() time.Time
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
	h.installed, h.dir, h.now = installedFile, exchangeDir, time.Now
	return nil
}

// The handler's own messages (DESIGN-box.md, "Message catalogue").
const (
	msgMethod     = "method not allowed"
	msgResultID   = "result must be a 32-hex id"
	msgNoBaseline = "this box has no baseline; hotserve init <dir> <sha> as root on the box sets one"
	msgNoApplier  = "config pushes are not applied by this hotserve version; nothing applied"
)

// pollScheme is the Authorization scheme a result poll carries its
// push's poll secret in: 32 random bytes the workflow chose, standard
// base64 with padding — what `head -c 32 /dev/urandom | base64`
// prints. Authorization, because Caddy's access log redacts it and no
// custom header (liveswap retired X-Liveswap-Secret for that leak);
// the push itself carries only the secret's digest.
const pollScheme = "Box-Poll "

const pollSecretLen = 32

// ServeHTTP answers on `/` and passes every other path on.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if r.URL.Path != "/" {
		return next.ServeHTTP(w, r)
	}
	switch r.Method {
	case http.MethodGet:
		if r.URL.RawQuery == "" {
			return h.status(w, r)
		}
		return h.result(w, r)
	case http.MethodPost:
		return h.push(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		return respond(w, http.StatusMethodNotAllowed, errorBody(msgMethod))
	}
}

// authenticate is the preamble (deploytrust): ok=false means the
// refusal is written. route names the request in the preamble's
// journal lines, beside the address.
func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request, route string) (bool, error) {
	_, refusal := h.limiter.Authenticate(r, h.app.verifiers, h.logger, "box_request", route)
	if refusal != nil {
		return false, refusal.Write(w, func(code int, body any) error { return respond(w, code, body) })
	}
	return true, nil
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
	if ok, err := h.authenticate(w, r, "status"); !ok {
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
		h.logger.Error("box webhook: the installed Caddyfile is not a box's", zap.String("refused", ref.Reason))
		return respond(w, http.StatusInternalServerError, errorBody("the Caddyfile this box runs "+ref.Reason))
	}
	sum := sha256.Sum256(file)
	digest := hex.EncodeToString(sum[:])
	return respond(w, http.StatusOK, statusBody{Commit: base.SHA, BoxWebhook: shape.Host, SHA256: digest},
		base.SHA, shape.Host, digest)
}

// result is `GET /?result=<id>`. The poll secret is tried first and
// never charged: it admits the request only for the id derived from
// it, and only while that id's marker holds the secret's digest and is
// younger than pendingLife. Anything else goes through the preamble —
// a wrong or stale secret is then an unauthenticated request like any
// other — and only once authenticated is the query's shape checked, so
// an unauthenticated caller learns nothing from a 400. The query names
// a file only after the id has passed its grammar, and the poll path
// reaches the disk only with an id it derived itself.
//
// With no result, a marker says the push was admitted and is not yet
// settled: 202 pending, whatever the marker's age. Only root can tell
// a push it still holds from one that was lost, and its next run
// settles a lost one as `failed` (DESIGN-box.md, "Retention").
func (h *Handler) result(w http.ResponseWriter, r *http.Request) error {
	var id string
	q, err := url.ParseQuery(r.URL.RawQuery)
	single := err == nil && len(q) == 1 && len(q["result"]) == 1
	var m *marker // the push's marker, as the poll secret read it
	if single {
		id = q["result"][0]
		if m, err = h.pollSecret(r, id); err != nil {
			return h.boxError(w, "the push's marker", err)
		}
	}
	if m == nil {
		if ok, err := h.authenticate(w, r, "result"); !ok {
			return err
		}
		if !single || !validID(id) {
			return respond(w, http.StatusBadRequest, errorBody(msgResultID))
		}
	}
	res, err := readResult(h.dir, id)
	if err == nil {
		safe := []string{id, res.Phase} // the phase is one of phaseStatus's
		if proof.IsID(res.Commit) {
			safe = append(safe, res.Commit)
		}
		if host, ok := BareHost([]string{res.BoxWebhook}); ok && host == res.BoxWebhook {
			safe = append(safe, host)
		}
		return respond(w, phaseStatus[res.Phase], res, safe...)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return h.boxError(w, "the result", err)
	}
	if m == nil {
		if _, err := readMarker(h.dir, id); errors.Is(err, fs.ErrNotExist) {
			return respond(w, http.StatusNotFound, errorBody("no result and no pending push for "+id+": swept, or never admitted"), id)
		} else if err != nil {
			return h.boxError(w, "the push's marker", err)
		}
	}
	return respond(w, http.StatusAccepted, map[string]string{"phase": "pending"})
}

// pollSecret is the poll secret's check, and the marker it admits on:
// exactly one Authorization header, in the Box-Poll scheme (its case
// ignored, as an auth scheme's is), exactly 32 bytes in standard padded
// base64, whose sha256 begins with id and equals, in constant time,
// the digest the marker holds; the marker younger than pendingLife (a
// clock stepped back makes it younger, as it does for admission).
//
// nil, nil admits nothing and writes nothing to the journal: this runs
// before the preamble, where a line per request would be outside the
// limiter's budgets — a broken stage/ makes every id unreadable, and
// is reported only to authenticated requests. An error says a marker
// stands at the secret's id and cannot be read: the box's error,
// answered as one, and reachable only with a secret a push was
// admitted under, so only by its holder — the workflow, which has no
// other way to learn why its polls fail.
func (h *Handler) pollSecret(r *http.Request, id string) (*marker, error) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 || len(values[0]) != len(pollScheme)+base64.StdEncoding.EncodedLen(pollSecretLen) ||
		!strings.EqualFold(values[0][:len(pollScheme)], pollScheme) {
		return nil, nil
	}
	secret, err := base64.StdEncoding.Strict().DecodeString(values[0][len(pollScheme):])
	if err != nil || len(secret) != pollSecretLen {
		return nil, nil
	}
	sum := sha256.Sum256(secret)
	if hex.EncodeToString(sum[:pollSecretLen/2]) != id {
		return nil, nil
	}
	m, err := readMarker(h.dir, id)
	if err != nil {
		if _, serr := os.Lstat(markerPath(h.dir, id)); serr == nil {
			return nil, err
		}
		return nil, nil
	}
	want, err := hex.DecodeString(m.SHA256) // validDigest: cannot fail
	if err != nil || subtle.ConstantTimeCompare(want, sum[:]) != 1 || h.now().Sub(m.Posted) >= pendingLife {
		return nil, nil
	}
	return m, nil
}

// push is `POST /`: authenticated, then refused, until the applier
// ships (DESIGN-box.md, status). The body is never read.
func (h *Handler) push(w http.ResponseWriter, r *http.Request) error {
	if ok, err := h.authenticate(w, r, "push"); !ok {
		return err
	}
	return respond(w, http.StatusNotImplemented, errorBody(msgNoApplier))
}

// boxError answers a failure that is the box's, never the caller's:
// 500, the journal at error level, both bounded.
func (h *Handler) boxError(w http.ResponseWriter, what string, err error) error {
	msg := proof.Bound(err.Error())
	h.logger.Error("box webhook could not read its state", zap.String("reading", what), zap.String("error", msg))
	return respond(w, http.StatusInternalServerError, errorBody("could not read "+what+": "+msg))
}

func errorBody(msg string) map[string]string { return map[string]string{"error": msg} }

// respond writes body through liveswap's response filter: the shape
// and entropy layers, with safe — the protocol's own ids, digests and
// host, each one the caller checked against its grammar — held out of
// them, since a 32-hex id, a 40-hex commit and a 64-hex digest are
// exactly what the entropy layer exists to catch.
func respond(w http.ResponseWriter, code int, body any, safe ...string) error {
	return liveswap.RespondJSON(w, code, body, liveswap.NewRedactor(nil, safe))
}

var (
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
)
