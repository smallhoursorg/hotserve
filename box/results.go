package box

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"go.uber.org/zap"

	"github.com/smallhoursorg/hotserve/box/proof"
)

// Phases (DESIGN-box.md, "States" and Glossary): a result's phase is
// `verified` or terminal; a record's is one of the five it can be in.
const (
	phaseVerified    = "verified"
	phaseRefused     = "refused"
	phaseNoChange    = "no_change"
	phaseApplied     = "applied"
	phaseFailed      = "failed"
	phaseRolledBack  = "rolled_back"
	phaseUnknown     = "unknown"
	phaseInstalling  = "installing"
	phaseSwapped     = "swapped"
	phaseRollingBack = "rolling_back"
)

// terminal reports whether a result's phase ends the push.
func terminal(phase string) bool {
	switch phase {
	case phaseRefused, phaseNoChange, phaseApplied, phaseFailed, phaseRolledBack, phaseUnknown:
		return true
	}
	return false
}

// The applier's messages, word for word from DESIGN-box.md's Message
// catalogue. The proof's and the walk's refusals carry their own.
const (
	msgNotRunning         = "hotserve is not running; nothing applied"
	msgStillStarting      = "hotserve is still starting after 300 s; nothing applied"
	msgNoSigner           = "the Caddyfile this box runs lists no signer; hotserve init is the way back"
	msgInterrupted        = "interrupted before the Caddyfile changed; nothing changed"
	msgInterruptedStopped = "interrupted; the previous Caddyfile is on disk; hotserve is not running"
	msgNoRecord           = "the box has no record of this push; push again"
	msgReloadFailed       = "the reload failed; the previous Caddyfile is back and running — journalctl -u hotserve -n 50 on the box says why"
	msgRecordAfterReload  = "the record could not be updated after a successful reload (disk full); rolled back to keep the file and the record consistent"
	msgBothReloads        = "the reload failed and so did the reload of the previous file; the previous file is on disk; journalctl -u hotserve"
	msgChanged            = "the Caddyfile changed during the transaction; it is left as found"
	msgDiskFull           = "disk full; the previous Caddyfile could not be restored"
)

// installFailed is `failed` for an install that stopped before the
// Caddyfile changed, the box's own error named.
func installFailed(err error) string {
	return "the install failed before the Caddyfile changed: " + proof.Bound(err.Error()) + "; nothing changed"
}

// baselineUnrecorded is `unknown` for an apply whose applied.json could
// not be written.
func baselineUnrecorded(commit string) string {
	return "applied but the baseline could not be recorded; hotserve box baseline " + commit
}

// result is out/<id>.json (DESIGN-box.md, "Record and result fields"):
// what a push came to, as the handler answers a result poll. Every
// string in it is the applier's own text, a value held to a grammar
// (id, commit, host, principal, app names), or bounded: the path by
// proof.SplitPath's rules, the diff redacted and capped (diff.go), the
// error built from proof.Bound pieces.
type result struct {
	ID         string   `json:"id"`
	Phase      string   `json:"phase"`
	Commit     string   `json:"commit,omitempty"`
	Path       string   `json:"path,omitempty"`
	Signer     string   `json:"signer,omitempty"`
	BoxWebhook string   `json:"box_webhook,omitempty"`
	OutOfBand  bool     `json:"caddyfile_edited_out_of_band"`
	Apps       []string `json:"apps,omitempty"`
	Diff       string   `json:"diff,omitempty"`
	Error      string   `json:"error,omitempty"`
}

// marshal is the result's bytes, never more than the handler reads: the
// caps on its fields keep a result well under maxResult (a 64 KiB diff
// whose every byte JSON escapes is 384 KiB; app names are plain ASCII),
// and should that arithmetic ever be wrong the diff and the app list
// give way, never the verdict.
func (r result) marshal() []byte {
	b := encodeJSON(r)
	if len(b) > maxResult {
		r.Diff, r.Apps = "(diff and apps omitted: the result would be larger than the handler reads)", nil
		b = encodeJSON(r)
	}
	return b
}

func encodeJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // `<` in a diff is a byte, not six
	if err := enc.Encode(v); err != nil {
		// Every type marshalled here is strings, bools and slices of
		// them: an error is a programming mistake.
		panic(fmt.Sprintf("box: encoding %T: %v", v, err))
	}
	return buf.Bytes()
}

// writeResult writes out/<id>.json, 0640 in the setgid out/, so the
// handler's group can read it. When it cannot, the result goes to the
// journal instead, every field of it, at error level (I3). Either way
// the outcome is logged once.
func (a *Applier) writeResult(r result) error {
	path := filepath.Join(a.x("out"), r.ID+".json")
	tmp := filepath.Join(a.x("out"), "."+r.ID+".json.tmp")
	err := a.writeDurable("result:"+r.Phase, path, tmp, r.marshal(), 0o640)
	// Kept from this run's sweep whether or not the write succeeded: one
	// whose rename landed before its fsync failed is the push's answer
	// all the same, and one that never landed leaves nothing to keep.
	if a.wrote == nil {
		a.wrote = map[string]string{}
	}
	if !terminal(a.wrote[r.ID]) { // an ended push stays ended
		a.wrote[r.ID] = r.Phase
	}
	if err != nil {
		a.logger.Error("box result could not be written", append(resultFields(r), zap.String("write_error", proof.Bound(err.Error())))...)
		return err
	}
	a.logOutcome(r)
	return nil
}

// removeResult removes out/<id>.json, if there is one.
func (a *Applier) removeResult(id string) {
	if err := a.removeDurable("result:remove", filepath.Join(a.x("out"), id+".json")); err != nil {
		a.logger.Error("box: could not remove a result", zap.String("id", id), zap.String("error", proof.Bound(err.Error())))
	}
}

// logOutcome is the applier's journal line for a push (DESIGN-box.md,
// "Journal lines"): id, commit, signer, box and phase, and the error
// for a red one.
func (a *Applier) logOutcome(r result) {
	fields := []zap.Field{zap.String("id", r.ID), zap.String("commit", r.Commit), zap.String("signer", r.Signer), zap.String("box", r.BoxWebhook), zap.String("phase", r.Phase)}
	switch r.Phase {
	case phaseVerified, phaseNoChange, phaseApplied:
		a.logger.Info("box apply", fields...)
	default:
		a.logger.Warn("box apply", append(fields, zap.String("error", r.Error))...)
	}
}

func resultFields(r result) []zap.Field {
	return []zap.Field{
		zap.String("id", r.ID), zap.String("phase", r.Phase), zap.String("commit", r.Commit),
		zap.String("path", proof.Bound(r.Path)), zap.String("signer", r.Signer), zap.String("box", r.BoxWebhook),
		zap.Bool("caddyfile_edited_out_of_band", r.OutOfBand), zap.Strings("apps", r.Apps),
		zap.String("diff", r.Diff), zap.String("error", r.Error),
	}
}

// readResult reads out/<id>.json as the handler does (readResultFile).
func (a *Applier) readResult(id string) (*result, error) {
	return readResultFile(filepath.Join(a.root, exchangeDir), id)
}

// marker is stage/<id>.auth (DESIGN-box.md, "Record and result fields"),
// written by the handler's admission after the bundle is in in/: the
// time of admission. The applier reads it
// for two things only — the order it takes bundles in, by `posted`, and
// Retention's age — and never trusts it for anything else: the hotserve
// uid writes stage/, and a marker is that uid's claim.
type marker struct {
	Posted time.Time `json:"posted"`
}

// readMarker reads a marker as the handler will: without following a
// symlink at its name, without blocking on a FIFO, held to a regular
// file, at most maxMarker bytes. A marker that does not read, or whose
// fields are not the shape admission writes, is an error the caller
// treats as "no posted time".
func readMarker(path string) (*marker, error) {
	b, err := readFile(path, maxMarker, false)
	if err != nil {
		return nil, err
	}
	var m marker
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m.Posted.IsZero() {
		return nil, fmt.Errorf("%s: not a marker", path)
	}
	return &m, nil
}
