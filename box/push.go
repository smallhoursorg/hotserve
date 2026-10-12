package box

// `POST /` and `GET /?result=<id>` (DESIGN-box.md, "The trust chain"
// steps 1–8, "Admission", "Handler contract"). A push runs in this
// order: the token (1); the request id (400) and whether the box
// already holds it (409); the media type (415); the process's push slot
// (409); the body, read once into memory under the upload deadline
// (413); the bundle (3); the token's `sha` against HEAD (4); the fast
// path; the admission lock and its 409s (2), the fast path asked again;
// the pre-verification (5); validate (6); the drop (7); the lock and the
// slot released; the wait for root's first result (8).
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
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/smallhoursorg/hotserve/box/proof"
)

// headerRequestID carries the id the workflow chose for its push.
const headerRequestID = "X-Box-Request-Id"

// Step 8's bounds, and the upload's (DESIGN-box.md, Caps).
const (
	resultWait     = 30 * time.Second
	resultInterval = time.Second
	// uploadTimeout bounds the body's arrival while the push slot is
	// held: a stalled upload would otherwise hold it for good.
	uploadTimeout = 120 * time.Second
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
	if len(ids) != 1 {
		return respond(w, http.StatusBadRequest, errorBody(msgRequestID))
	}
	id, ok := parseRequestID(ids[0])
	if !ok {
		return respond(w, http.StatusBadRequest, errorBody(msgRequestID))
	}
	p := &pushing{h: h, w: w, r: r, id: id, via: ident.By}
	// An id the box already holds is a retry after a lost response: it
	// polls, whatever its push came to (DESIGN-box.md, "Admission"). Asked
	// before the body is read, and again under the lock.
	if known, err := h.known(p.id); err != nil {
		return p.fail(err)
	} else if known {
		return p.respond(http.StatusConflict, errorBody(msgDuplicate(p.id)))
	}
	if !isGzip(r.Header.Get("Content-Type")) {
		return p.respond(http.StatusUnsupportedMediaType, errorBody(msgMediaType))
	}
	if r.ContentLength > maxBody {
		return p.respond(http.StatusRequestEntityTooLarge, errorBody(msgTooLarge))
	}
	// One push at a time in this process, from its body to its drop: a
	// body is up to 16 MiB in memory, and nothing else bounds how many
	// authenticated requests could hold one (DESIGN-box.md, Caps). Not a
	// queue: a second push is told to retry, its body unread.
	release, ok := h.takeSlot()
	if !ok {
		return p.respond(http.StatusConflict, errorBody(msgAdmitting))
	}
	defer release()
	// The deadline replaces, for this body, any read_body timeout the
	// operator set (DESIGN-box.md, Caps). HTTP/1.1, HTTP/2 and HTTP/3
	// all take one; a writer in the chain that hides the method is said
	// in the journal, since the slot then has no bound but the
	// connection's.
	rc := http.NewResponseController(w)
	upload := h.upload
	if upload == 0 {
		upload = uploadTimeout
	}
	if err := rc.SetReadDeadline(time.Now().Add(upload)); err != nil {
		h.logger.Warn("box webhook: the upload has no deadline", zap.String("id", p.id), zap.String("error", proof.Bound(err.Error())))
	}
	// Never cleared here: once a body ends, net/http clears the deadline
	// itself as it starts its background read (connReader.
	// startBackgroundRead), so admission and the wait may outlast it; and
	// after a body that did not arrive whole it must stay, so the
	// server's drain of what is left of it ends too.
	body, err := readCapped(r.Body, "the bundle", maxBody)
	var big *tooLargeError
	if errors.As(err, &big) {
		return p.respond(http.StatusRequestEntityTooLarge, errorBody(msgTooLarge))
	}
	if err != nil {
		// The caller's connection, not the box: nothing was admitted. A
		// line all the same, since the slot was held while it stalled.
		h.logger.Warn("box push upload failed", zap.String("id", p.id), zap.String("error", proof.Bound(err.Error())),
			zap.String("via", p.via), zap.String("remote", r.RemoteAddr))
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
	base, installed, done, err := p.state()
	if done {
		return err
	}
	if answered, err := p.noChange(b, base, installed); answered {
		return err
	}

	// 2, 5, 6, 7: under the admission lock.
	if done, err := p.admit(b, body); done {
		return err
	}
	// 8. root's first result, read for at most resultWait — with the slot
	// given back: the body is in in/, and the next push may begin.
	release()
	return p.answer(h.waitResult(r.Context(), p.id))
}

// state is readState for a push: done=true means the push was answered
// — 409 before `init`, 500 when either file cannot be read.
func (p *pushing) state() (base *applied, installed []byte, done bool, err error) {
	base, installed, what, err := p.h.readState()
	switch {
	case err == nil:
		return base, installed, false, nil
	case what == readingBaseline && errors.Is(err, fs.ErrNotExist):
		return nil, nil, true, p.respond(http.StatusConflict, errorBody(msgNoBaseline))
	default:
		return nil, nil, true, p.h.boxError(p.w, what, err)
	}
}

// noChange is the fast path (step 4): when the box runs this commit and
// this file already, in a shape root would accept (unchanged), 200
// `no_change`, nothing written. Asked before the lock and again under
// it, of what was read there: a push another one applied meanwhile is
// answered so, not pre-verified against itself.
func (p *pushing) noChange(b *proof.Bundle, base *applied, installed []byte) (bool, error) {
	if !unchanged(b, installed, base, p.h.envDigest) {
		return false, nil
	}
	p.h.logger.Info("box push answered", zap.String("id", p.id), zap.String("commit", p.commit), zap.String("phase", phaseNoChange),
		zap.String("via", p.via), zap.String("remote", p.r.RemoteAddr))
	return true, p.respond(http.StatusOK, result{ID: p.id, Phase: phaseNoChange, Commit: b.Commit.ID, Path: b.Path})
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
	msg, ids, ok := refusalText(err)
	if !ok {
		return p.fail(err)
	}
	res := result{ID: p.id, Phase: phaseRefused, Error: msg, Named: ids}
	if b != nil {
		res.Commit, res.Path = b.Commit.ID, b.Path
	}
	p.h.logger.Warn("box push refused", zap.String("id", p.id), zap.String("commit", p.commit),
		zap.String("refused", msg), zap.String("via", p.via), zap.String("remote", p.r.RemoteAddr))
	return respond(p.w, http.StatusUnprocessableEntity, res, append([]string{p.id, p.commit, p.claim}, ids...)...)
}

// fail answers a push the box could not check: 500, nothing changed.
func (p *pushing) fail(err error) error {
	p.h.logger.Error("box push failed", zap.String("id", p.id), zap.String("commit", p.commit),
		zap.String("error", proof.Bound(err.Error())), zap.String("via", p.via), zap.String("remote", p.r.RemoteAddr))
	return p.respond(http.StatusInternalServerError, errorBody(msgCheckFailed(err)))
}

// refusalText is the catalogue text of a verdict — the proof's, the
// walk's (through check), the handler's own — and the object ids it
// names.
func refusalText(err error) (string, []string, bool) {
	var pr *proof.Refusal
	if errors.As(err, &pr) {
		return pr.Msg, pr.IDs, true
	}
	var r *refusal
	if errors.As(err, &r) {
		return r.msg, r.ids, true
	}
	return "", nil, false
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

// pushSlot is the process's one push in memory, shared by every
// box_webhook instance a reload provisions.
var pushSlot = make(chan struct{}, 1)

// takeSlot takes the push slot without waiting; release gives it back,
// once however often it is called.
func (h *Handler) takeSlot() (release func(), ok bool) {
	slot := h.slot // Provision's pushSlot; a test's own
	select {
	case slot <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-slot }) }, true
	default:
		return nil, false
	}
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
	if err != nil || len(q) != 1 || len(q["result"]) != 1 {
		return respond(w, http.StatusBadRequest, errorBody(msgResultID))
	}
	id, ok := parseRequestID(q["result"][0])
	if !ok {
		return respond(w, http.StatusBadRequest, errorBody(msgResultID))
	}
	res, err := readResultFile(h.dir, id)
	switch {
	case err == nil:
		return respondResult(w, res)
	case !errors.Is(err, fs.ErrNotExist):
		h.logStateError("the result for "+id, err)
		return respond(w, http.StatusInternalServerError, errorBody("could not read the result for "+id+": "+proof.Bound(err.Error())), id)
	}
	// No result yet: a marker, of whatever age, or a bundle still in in/
	// (its marker never written), is a push admitted.
	for _, path := range h.admittedPaths(id) {
		if found, err := present(path); err != nil {
			h.logStateError("the push "+id, err)
			return respond(w, http.StatusInternalServerError, errorBody("could not read the push "+id+": "+proof.Bound(err.Error())), id)
		} else if found {
			return respond(w, http.StatusAccepted, map[string]string{"phase": "admitted"})
		}
	}
	return respond(w, http.StatusNotFound, errorBody(msgNoResult(id)), id)
}

// respondResult answers with a result and step 8's status for its phase.
// The secrets' names are held out of the filter with the ids: in the
// added, changed and removed lists each is `<app>/<VAR>`, out of band an
// app name, each in its grammar (readResultFile) and never a value — and
// a variable's name can look as random as a key.
func respondResult(w http.ResponseWriter, res *result) error {
	safe := append([]string{res.ID, res.Commit, res.BoxWebhook}, res.Named...)
	for _, names := range [][]string{res.SecretsAdded, res.SecretsChanged, res.SecretsRemoved, res.SecretsOutOfBand} {
		safe = append(safe, names...)
	}
	return respond(w, phaseStatus(res.Phase), res, safe...)
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
