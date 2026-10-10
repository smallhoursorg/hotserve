package box

// Admission (DESIGN-box.md, "Admission", steps 2 and 5 to 7): whether a
// push may become a bundle in in/, decided under a non-blocking flock
// on stage/lock so that two concurrent pushes cannot both see nothing
// pending and both enqueue. The lock is on a file and flock conflicts
// between open file descriptions in one process, so it holds across the
// reload that re-instantiates the handler; a crash releases it with the
// descriptor.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/smallhoursorg/hotserve/box/proof"
)

// pendingFor is how long a marker with no terminal result blocks
// admission: the workflow's own poll bound (DESIGN-box.md, Caps).
const pendingFor = 15 * time.Minute

// x is a path in the exchange tree.
func (h *Handler) x(elem ...string) string {
	return filepath.Join(append([]string{h.dir}, elem...)...)
}

// admit is steps 2 and 5 to 7 under the admission lock. done=false
// means the bundle is in in/ and the lock released: the caller waits for
// root's result. done=true means the push was answered here.
func (p *pushing) admit(b *proof.Bundle, body, installed []byte, base *applied) (done bool, err error) {
	h := p.h
	unlock, held, err := h.lockAdmission()
	if err != nil {
		return true, p.fail(fmt.Errorf("the admission lock: %w", err))
	}
	if held {
		return true, p.respond(http.StatusConflict, errorBody(msgAdmitting))
	}
	defer unlock()
	h.sweepOwnTemporaries()

	// 2. One push at a time: the id unknown, nothing in in/, no marker
	// still pending.
	if exists(h.x("stage", p.id+".auth")) || exists(h.x("out", p.id+".json")) {
		return true, p.respond(http.StatusConflict, errorBody(msgDuplicate(p.id)))
	}
	if what, age, pending, err := h.pending(); err != nil {
		return true, p.fail(err)
	} else if pending {
		// The pending id is held out of the filter as the push's own is;
		// any other name in in/ is not an id and gets no exemption.
		safe := []string{p.id, p.commit}
		if isRequestID(what) {
			safe = append(safe, what)
		}
		return true, respond(p.w, http.StatusConflict, errorBody(msgPending(what, age)), safe...)
	}

	// 5. The proof, as root will run it: a signed commit paired with
	// other bytes fails here, so validate only ever sees bytes a listed
	// signer committed.
	if _, err := check(p.r.Context(), h.verifier, b, installed, base); err != nil {
		return true, p.refuse(b, err)
	}
	// 6. Validate, as the serving process would load it.
	if msg, err := h.validator.validate(p.r.Context(), p.id, b.Caddyfile); err != nil {
		return true, p.fail(err)
	} else if msg != "" {
		return true, p.refuse(b, &refusal{msg: msg})
	}
	// 7. The drop.
	if err := h.drop(p.id, body); err != nil {
		return true, p.fail(err)
	}
	h.logger.Info("box push accepted", zap.String("id", p.id), zap.String("commit", p.commit),
		zap.String("via", p.via), zap.String("remote", p.r.RemoteAddr))
	return false, nil
}

// lockAdmission takes stage/lock without blocking: held=true when
// another request has it.
func (h *Handler) lockAdmission() (unlock func(), held bool, err error) {
	f, err := os.OpenFile(h.x("stage", "lock"), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, true, nil
		}
		return nil, false, err
	}
	return func() { _ = f.Close() }, false, nil
}

// Temporaries the handler makes in stage/, each named for its push and
// removed by the handler alone (DESIGN-box.md, Paths: "Removes").
func bundleTemp(id string) string   { return "." + id + ".tar.tmp" }
func markerTemp(id string) string   { return "." + id + ".auth.tmp" }
func stagedConfig(id string) string { return "." + id + ".Caddyfile" }

// sweepOwnTemporaries removes what a crash of an earlier admission left:
// every dot-name in stage/. Called under the lock, so no admission is
// writing one; root's names in stage/ are `<id>.auth`, never dotted.
func (h *Handler) sweepOwnTemporaries() {
	entries, err := os.ReadDir(h.x("stage"))
	if err != nil {
		return // the pending check lists stage/ next and says why
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			if err := os.RemoveAll(h.x("stage", e.Name())); err != nil {
				h.logger.Warn("box webhook could not remove a temporary", zap.String("error", proof.Bound(err.Error())))
			}
		}
	}
}

// pending reports the push that blocks admission, if any: any entry in
// in/ (named by its id when it is `<id>.tar`, aged by its modification
// time), or a marker younger than pendingFor with no terminal result
// (aged by its `posted`). A marker that does not read blocks nothing:
// root ages it by its modification time and settles it (Retention). A
// marker dated more than pendingFor past the clock is not pending, as
// Retention counts it older than a day.
func (h *Handler) pending() (what string, age time.Duration, pending bool, err error) {
	now := h.clock.Now()
	entries, err := os.ReadDir(h.x("in"))
	if err != nil {
		return "", 0, false, fmt.Errorf("in/: %w", err)
	}
	if len(entries) > 0 {
		e := entries[0] // one is enough to block, and the first is named
		what := proof.Bound(e.Name())
		if id, ok := strings.CutSuffix(e.Name(), ".tar"); ok && isRequestID(id) {
			what = id
		}
		var age time.Duration
		if fi, err := e.Info(); err == nil {
			age = now.Sub(fi.ModTime())
		}
		return what, roundAge(age), true, nil
	}
	entries, err = os.ReadDir(h.x("stage"))
	if err != nil {
		return "", 0, false, fmt.Errorf("stage/: %w", err)
	}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".auth")
		if !ok || !isRequestID(id) {
			continue
		}
		m, err := readMarker(h.x("stage", e.Name()))
		if err != nil {
			continue
		}
		age := now.Sub(m.Posted)
		if age >= pendingFor || -age > pendingFor {
			continue
		}
		if res, err := readResultFile(h.dir, id); err == nil && terminal(res.Phase) {
			continue
		}
		return id, roundAge(age), true, nil
	}
	return "", 0, false, nil
}

func roundAge(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d.Round(time.Second)
}

// drop is step 7: the bundle into in/ first, then the marker, each by
// a rename of a temporary written whole and fsynced, each directory
// fsynced after its rename (I8 covers this handoff). Once the bundle's
// rename has landed the push is root's, whatever follows: a marker that
// cannot be written is a journal line, and the push's poll finds root's
// result in its place (DESIGN-box.md, "Admission").
func (h *Handler) drop(id string, body []byte) error {
	if err := h.fail("bundle"); err != nil {
		return err
	}
	tmp := h.x("stage", bundleTemp(id))
	if err := writeExclusive(tmp, body, 0o644); err != nil {
		return fmt.Errorf("staging the bundle: %w", err)
	}
	if err := os.Rename(tmp, h.x("in", id+".tar")); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("handing the bundle to root: %w", err)
	}
	if err := syncDir(h.x("in")); err != nil {
		h.logger.Error("box webhook: the bundle is in in/, but in/ could not be synced", zap.String("id", id), zap.String("error", proof.Bound(err.Error())))
	}
	h.crash("bundle")
	if err := h.writeMarker(id); err != nil {
		h.logger.Error("box webhook: the bundle is in in/, but its marker could not be written", zap.String("id", id), zap.String("error", proof.Bound(err.Error())))
		return nil
	}
	h.crash("marker")
	return nil
}

func (h *Handler) writeMarker(id string) error {
	if err := h.fail("marker"); err != nil {
		return err
	}
	tmp := h.x("stage", markerTemp(id))
	if err := writeExclusive(tmp, encodeJSON(marker{Posted: h.clock.Now().UTC()}), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, h.x("stage", id+".auth")); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(h.x("stage"))
}

// writeExclusive writes a new file whole: created (never over anything,
// never through a link), written, given its mode after the write so the
// umask has no say, fsynced and closed. On an error it is removed.
func writeExclusive(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, mode) //nolint:gosec // a fixed name in the handler's own directory
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}

// handlerHooks are the test seams at the drop's two durable writes, as
// the applier's hooks are at its own: fail runs before a write and makes
// it fail; crash runs once it is durable, and a test panics there to
// stand for a kill. Production leaves both nil.
type handlerHooks struct {
	fail  func(point string) error
	crash func(point string)
}

func (h *Handler) fail(point string) error {
	if h.hooks.fail != nil {
		return h.hooks.fail(point)
	}
	return nil
}

func (h *Handler) crash(point string) {
	if h.hooks.crash != nil {
		h.hooks.crash(point)
	}
}

// readResultFile reads out/<id>.json, which only root writes, held to
// the shape root writes: the id it is filed under, and `verified` or a
// terminal phase. The handler (step 8, the poll) and the applier
// (settled) read it the same way.
func readResultFile(dir, id string) (*result, error) {
	path := filepath.Join(dir, "out", id+".json")
	b, err := readFile(path, maxResult, false)
	if err != nil {
		return nil, err
	}
	var r result
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if r.ID != id || (r.Phase != phaseVerified && !terminal(r.Phase)) {
		return nil, fmt.Errorf("%s: not a result", path)
	}
	return &r, nil
}
