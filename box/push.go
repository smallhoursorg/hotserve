package box

// `POST /` and `GET /?result=<id>` (DESIGN-box.md, "The trust chain"
// steps 1–8, "Admission", "Handler contract"). A push runs in this
// order: the token (1); the request id (400) and the media type (415);
// the body, read once into memory (413); the bundle (3); the token's
// `sha` against HEAD (4); the fast path; only then the admission lock
// and its 409s (2); the pre-verification (5); validate (6); the drop
// (7); the lock released; the wait for root's first result (8).
//
// The rules every function here keeps:
//
//  1. Nothing is written before the admission lock, and nothing but the
//     handler's own temporaries, the bundle and the marker after it.
//  2. A failure that is the box's — a file it cannot read or write, a
//     child it cannot run — is 500 and a journal line, never a refusal:
//     a refusal is a verdict on the push.
//  3. Every value the request chose that reaches a response or the
//     journal is held to a grammar (the id) or passes proof.Bound.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/smallhoursorg/hotserve/box/proof"
)

// headerRequestID carries the id the workflow chose for its push.
const headerRequestID = "X-Box-Request-Id"

// Step 8's bounds (DESIGN-box.md, Caps).
const (
	resultWait     = 30 * time.Second
	resultInterval = time.Second
)

// The handler's push and poll messages (DESIGN-box.md, "Message
// catalogue").
const (
	msgRequestID  = "X-Box-Request-Id: 32 lowercase hex, chosen at random for each push, required"
	msgResultID   = "result must be a 32-hex id"
	msgMediaType  = "the bundle is a gzip tarball"
	msgTooLarge   = "the bundle is larger than 16 MiB"
	msgNoSHAClaim = "box tokens must name the commit (sha claim)"
	msgAdmitting  = "a push is being admitted; retry in a moment"
)

func msgDuplicate(id string) string { return "duplicate request id: poll /?result=" + id }

func msgPending(what string, age time.Duration) string {
	return fmt.Sprintf("a push is pending: %s, %s old", what, age)
}

func msgTokenNames(claim, head string) string {
	return fmt.Sprintf("the token names commit %s; the bundle is %s", proof.Bound(claim), head)
}

func msgNoResult(id string) string {
	return "no result and no marker for " + id + ": swept, or never admitted"
}

func msgCheckFailed(err error) string {
	return "could not check the push: " + proof.Bound(err.Error()) + "; nothing changed"
}

// push is `POST /`.
func (h *Handler) push(w http.ResponseWriter, r *http.Request) error {
	ident, ok, err := h.authenticate(w, r, "push")
	if !ok {
		return err
	}
	ids := r.Header.Values(headerRequestID)
	if len(ids) != 1 || !isRequestID(ids[0]) {
		return respond(w, http.StatusBadRequest, errorBody(msgRequestID))
	}
	p := &pushing{h: h, w: w, r: r, id: ids[0], via: ident.By}
	if !isGzip(r.Header.Get("Content-Type")) {
		return p.respond(http.StatusUnsupportedMediaType, errorBody(msgMediaType))
	}
	if r.ContentLength > maxBody {
		return p.respond(http.StatusRequestEntityTooLarge, errorBody(msgTooLarge))
	}
	body, err := readCapped(r.Body, "the bundle", maxBody)
	var big *tooLargeError
	if errors.As(err, &big) {
		return p.respond(http.StatusRequestEntityTooLarge, errorBody(msgTooLarge))
	}
	if err != nil {
		// The caller's connection, not the box: nothing was admitted.
		return p.respond(http.StatusBadRequest, errorBody("could not read the bundle: "+proof.Bound(err.Error())))
	}

	// 3. The bundle, from memory.
	b, err := proof.ReadBundle(body)
	if err != nil {
		return p.refuse(nil, err)
	}
	p.commit, p.path = b.Commit.ID, b.Path

	// 4. The token names the commit the bundle is for.
	claim, ok := ident.Claim("sha")
	if !ok {
		return p.refuse(b, &refusal{msg: msgNoSHAClaim})
	}
	if claim != b.Commit.ID {
		if proof.IsID(claim) {
			p.claim = claim
		}
		return p.refuse(b, &refusal{msg: msgTokenNames(claim, b.Commit.ID)})
	}

	// The fast path: the box runs this commit and this file already.
	base, err := readApplied(h.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return p.respond(http.StatusConflict, errorBody(msgNoBaseline))
	}
	if err != nil {
		return h.boxError(w, "the baseline", err)
	}
	installed, err := readFile(h.installed, proof.MaxCaddyfile, true)
	if err != nil {
		return h.boxError(w, "the Caddyfile this box runs", err)
	}
	if b.Commit.ID == base.SHA && digest(b.Caddyfile) == digest(installed) {
		res := result{ID: p.id, Phase: phaseNoChange, Commit: b.Commit.ID, Path: b.Path}
		h.logger.Info("box push answered", zap.String("id", p.id), zap.String("commit", p.commit), zap.String("phase", phaseNoChange),
			zap.String("via", p.via), zap.String("remote", r.RemoteAddr))
		return p.respond(http.StatusOK, res)
	}

	// 2, 5, 6, 7: under the admission lock.
	if done, err := p.admit(b, body, installed, base); done {
		return err
	}
	// 8. root's first result, read for at most resultWait.
	return p.answer(h.waitResult(r.Context(), p.id))
}

// pushing is one push's state, so that its answers and journal lines
// carry what it has established.
type pushing struct {
	h            *Handler
	w            http.ResponseWriter
	r            *http.Request
	id, via      string
	commit, path string
	// claim is the token's sha claim when it names a commit other than
	// HEAD: the refusal quotes it.
	claim string
}

// respond answers with the push's own fields held out of the filter's
// heuristics: the id, the commit and a mismatched claim, each checked
// against its grammar.
func (p *pushing) respond(code int, body any) error {
	return respond(p.w, code, body, p.id, p.commit, p.claim)
}

// refuse answers a verdict on the push: 422 with a `refused` result the
// handler wrote no file for, and the refusal line. An error that is not
// a verdict is the box's own (rule 2).
func (p *pushing) refuse(b *proof.Bundle, err error) error {
	msg, ok := refusalText(err)
	if !ok {
		return p.fail(err)
	}
	res := result{ID: p.id, Phase: phaseRefused, Error: msg}
	if b != nil {
		res.Commit, res.Path = b.Commit.ID, b.Path
	}
	p.h.logger.Warn("box push refused", zap.String("id", p.id), zap.String("commit", p.commit),
		zap.String("refused", msg), zap.String("via", p.via), zap.String("remote", p.r.RemoteAddr))
	return p.respond(http.StatusUnprocessableEntity, res)
}

// fail answers a push the box could not check: 500, nothing changed.
func (p *pushing) fail(err error) error {
	p.h.logger.Error("box push failed", zap.String("id", p.id), zap.String("commit", p.commit),
		zap.String("error", proof.Bound(err.Error())), zap.String("via", p.via), zap.String("remote", p.r.RemoteAddr))
	return p.respond(http.StatusInternalServerError, errorBody(msgCheckFailed(err)))
}

// refusalText is the catalogue text of a verdict: the proof's, the
// walk's (through check), the handler's own.
func refusalText(err error) (string, bool) {
	var pr *proof.Refusal
	if errors.As(err, &pr) {
		return pr.Msg, true
	}
	var r *refusal
	if errors.As(err, &r) {
		return r.msg, true
	}
	return "", false
}

// answer is step 8's answer, from the result waitResult read.
func (p *pushing) answer(res *result, err error) error {
	if err != nil {
		p.h.logStateError("the result for "+p.id, err)
		return p.respond(http.StatusInternalServerError, errorBody("could not read the result for "+p.id+": "+proof.Bound(err.Error())))
	}
	if res == nil {
		return p.respond(http.StatusGatewayTimeout, map[string]string{"id": p.id})
	}
	return respondResult(p.w, res)
}

// waitResult reads out/<id>.json every resultInterval until it exists
// or resultWait has passed (nil, nil), or the caller has gone. The
// handler never waits past the first result ("Why 202 and a poll").
func (h *Handler) waitResult(ctx context.Context, id string) (*result, error) {
	deadline := h.clock.Now().Add(resultWait)
	for {
		res, err := readResultFile(h.dir, id)
		if !errors.Is(err, fs.ErrNotExist) {
			return res, err
		}
		if !h.clock.Now().Before(deadline) {
			return nil, nil
		}
		if err := h.clock.Sleep(ctx, resultInterval); err != nil {
			return nil, nil // the caller has gone: nobody reads the 504
		}
	}
}

// result is `GET /?result=<id>`.
func (h *Handler) result(w http.ResponseWriter, r *http.Request) error {
	if _, ok, err := h.authenticate(w, r, "result"); !ok {
		return err
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(q) != 1 || len(q["result"]) != 1 || !isRequestID(q["result"][0]) {
		return respond(w, http.StatusBadRequest, errorBody(msgResultID))
	}
	id := q["result"][0]
	res, err := readResultFile(h.dir, id)
	switch {
	case err == nil:
		return respondResult(w, res)
	case !errors.Is(err, fs.ErrNotExist):
		h.logStateError("the result for "+id, err)
		return respond(w, http.StatusInternalServerError, errorBody("could not read the result for "+id+": "+proof.Bound(err.Error())), id)
	}
	// No result yet: a marker, of whatever age, is a push admitted.
	if exists(h.x("stage", id+".auth")) {
		return respond(w, http.StatusAccepted, map[string]string{"phase": "admitted"})
	}
	return respond(w, http.StatusNotFound, errorBody(msgNoResult(id)), id)
}

// respondResult answers with a result and step 8's status for its phase.
func respondResult(w http.ResponseWriter, res *result) error {
	return respond(w, phaseStatus(res.Phase), res, res.ID, res.Commit, res.BoxWebhook)
}

// phaseStatus is step 8's status for a phase readResultFile accepted:
// 202 for `verified`, 200 for the green ends, 422 for the red ones — a
// red outcome is never carried by a 2xx.
func phaseStatus(phase string) int {
	switch phase {
	case phaseVerified:
		return http.StatusAccepted
	case phaseNoChange, phaseApplied:
		return http.StatusOK
	default:
		return http.StatusUnprocessableEntity
	}
}

// isGzip reports whether a Content-Type names a gzip body, as liveswap's
// webhook reads one: parameters dropped, case ignored.
func isGzip(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	switch strings.ToLower(mt) {
	case "application/gzip", "application/x-gzip":
		return true
	}
	return false
}
