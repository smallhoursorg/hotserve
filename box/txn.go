package box

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"go.uber.org/zap"

	"github.com/smallhoursorg/hotserve/box/proof"
)

// The install transaction (DESIGN-box.md, "The install transaction"):
// the state machine from the buffer comparison on, shared by the
// applier and `init` through origin, and recovery by the States table.
// Every durable write is writeDurable's or removeDurable's, in the order
// the Transitions table gives; the tests cite I1–I8 per write.

// origin is who runs a transaction; recovery honours it.
type origin string

const (
	originApplier origin = "applier"
	originInit    origin = "init"
)

// record is txn.json (DESIGN-box.md, "Record and result fields"): the
// one durable marker of a transaction in flight. Present means "no
// terminal result yet" (I5). Prev holds the installed file's bytes as
// step 10 read them, the only rollback source; Error is, in a
// rolling_back record, the text its rolled_back result will carry.
type record struct {
	ID         string   `json:"id"`
	Origin     origin   `json:"origin"`
	Phase      string   `json:"phase"`
	Commit     string   `json:"commit"`
	Path       string   `json:"path"`
	Signer     string   `json:"signer"`
	Prev       []byte   `json:"prev"`
	PrevSHA256 string   `json:"prev_sha256"`
	NewSHA256  string   `json:"new_sha256"`
	Diff       string   `json:"diff,omitempty"`
	Apps       []string `json:"apps,omitempty"`
	BoxWebhook string   `json:"box_webhook"`
	OutOfBand  bool     `json:"caddyfile_edited_out_of_band"`
	Error      string   `json:"error,omitempty"`
}

// maxRecord caps txn.json as read back: Prev's 1 MiB is 1.4 MiB of
// base64, the diff's 64 KiB at most 384 KiB of JSON escapes, the path's
// 4 KiB 24 KiB, and the app names a 1 MiB file can hold well under
// 1 MiB of plain ASCII. Only root writes the file; the cap is a bound,
// not a trust decision.
const maxRecord = 4 << 20

// valid holds a record read back to the shape only the applier and
// `init` write. One that is not is never acted on (recoverAll).
func (r *record) valid() error {
	switch {
	case r.Origin != originApplier && r.Origin != originInit:
		return errors.New("origin is not applier or init")
	case !isRequestID(r.ID):
		return errors.New("id is not 32 hex")
	case !proof.IsID(r.Commit):
		return errors.New("commit is not 40 hex")
	case !isDigest(r.PrevSHA256) || !isDigest(r.NewSHA256):
		return errors.New("a digest is not 64 hex")
	case len(r.Prev) > proof.MaxCaddyfile || digest(r.Prev) != r.PrevSHA256:
		return errors.New("prev is not the bytes prev_sha256 names")
	}
	switch r.Phase {
	case phaseNoChange, phaseInstalling, phaseSwapped, phaseApplied, phaseRollingBack:
		return nil
	}
	return errors.New("phase is not a record's")
}

// txn is one transaction in memory.
type txn struct {
	rec record
	// next is the incoming file; recovery never needs it.
	next []byte
	// entry is work/<id>.tar, removed after the terminal result; ""
	// for init, which has none.
	entry string
	// running is whether hotserve was up when the transaction began —
	// step 16's `active` for the applier, init's one question for init —
	// and decides whether the swap, and a live rollback, reload.
	running bool
	// recorded is true once a record of this transaction may be on disk.
	recorded bool
	// outcome is the terminal result, for init's caller to print.
	outcome *result
}

var (
	// errFullDisk is the one run that ends unsettled by design: the
	// previous Caddyfile could not be written back, so the record stays
	// for the next run (Failure-mode table, "record swapped"). The
	// command exits non-zero on it.
	errFullDisk = errors.New(msgDiskFull)
	// errUnsettled ends a run that cannot reason safely — a record it
	// cannot read, the installed file unreadable mid-transaction,
	// is-active unanswered in recovery — with everything left as found
	// and an error-level line. The path unit runs it again until its
	// trigger limit; the console decides.
	errUnsettled = errors.New("the transaction could not be settled")
)

func (t *txn) result(phase, msg string) result {
	r := t.rec
	return result{
		ID: r.ID, Phase: phase, Commit: r.Commit, Path: r.Path, Signer: r.Signer,
		BoxWebhook: r.BoxWebhook, OutOfBand: r.OutOfBand, Apps: r.Apps, Diff: r.Diff, Error: msg,
	}
}

func (a *Applier) writeRecord(rec *record) error {
	return a.writeDurable("record:"+rec.Phase, a.x("txn.json"), a.x(".txn.json.tmp"), encodeJSON(rec), 0o600)
}

func (a *Applier) readRecord() (*record, error) {
	b, err := readFile(a.x("txn.json"), maxRecord, false)
	if err != nil {
		return nil, err
	}
	var r record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	if err := r.valid(); err != nil {
		return nil, fmt.Errorf("txn.json: %w", err)
	}
	return &r, nil
}

// writeApplied advances the baseline to the record's commit (I4: only
// from a record that says applied or no_change, or init's).
func (a *Applier) writeApplied(rec *record) error {
	b := encodeJSON(applied{SHA: rec.Commit, Path: rec.Path, SHA256: rec.NewSHA256, Signer: rec.Signer, When: a.clock.Now().UTC()})
	return a.writeDurable("applied.json", a.x("applied.json"), a.x(".applied.json.tmp"), b, 0o640)
}

// finish ends a transaction at a terminal phase, in the order every
// Transitions row ends: the result, then the record, then the entry
// (I2, I3, I5). A result that cannot be written is journaled whole by
// writeResult and the record and the entry go anyway (Failure-mode
// table, "terminal result": the disk is settled, only the report is
// owed). init writes no result: its caller prints the outcome.
func (a *Applier) finish(t *txn, phase, msg string) {
	res := t.result(phase, msg)
	t.outcome = &res
	if t.rec.Origin == originApplier {
		_ = a.writeResult(res) // a failure is journaled with every field (I3)
	} else {
		a.logOutcome(res)
	}
	if t.recorded {
		a.removeRecord()
	}
	if t.entry != "" {
		a.removeEntry(t.entry)
	}
}

func (a *Applier) removeRecord() {
	if err := a.removeDurable("record:remove", a.x("txn.json")); err != nil {
		// The next run's recovery finds the record with its terminal
		// result and removes it (Failure-mode table, "terminal result").
		a.logger.Error("box record could not be removed", zap.String("error", proof.Bound(err.Error())))
	}
}

func (a *Applier) removeEntry(path string) {
	if err := a.removeDurable("entry:remove", path); err != nil {
		// work/ stays non-empty, so the path unit runs recovery again,
		// which removes an entry that has its terminal result.
		a.logger.Error("box work entry could not be removed", zap.String("entry", filepath.Base(path)), zap.String("error", proof.Bound(err.Error())))
	}
}

// install runs the transaction from the buffer comparison (Transitions
// table, from "checking → no_change" and "checking → verified" on). The
// caller has run steps 10–15 and, for the applier, found hotserve
// active; for init it has set t.running. It returns errFullDisk or
// errUnsettled when the run must stop with the record on disk, nil
// once a terminal result is written (or journaled).
func (a *Applier) install(ctx context.Context, t *txn) error {
	if bytes.Equal(t.rec.Prev, t.next) {
		t.rec.Phase = phaseNoChange
		t.recorded = true // a failed write may still have renamed it into place
		if err := a.writeRecord(&t.rec); err != nil {
			a.finish(t, phaseFailed, installFailed(err))
			return nil
		}
		return a.advance(t, phaseNoChange)
	}
	if t.rec.Origin == originApplier {
		if err := a.writeResult(t.result(phaseVerified, "")); err != nil {
			// Nothing has changed (Transitions table, "checking →
			// verified"): the push ends `failed` if that can be written
			// — over a `verified` whose rename landed before the
			// failure, which nothing else would ever replace — and if
			// not, any such `verified` goes, leaving a marker with no
			// result for Retention to settle. The entry goes either way.
			if a.writeResult(t.result(phaseFailed, installFailed(err))) != nil {
				// Only a file that reads as `verified` is removed: one
				// the applier cannot identify is never deleted.
				if r, rerr := a.readResult(t.rec.ID); rerr == nil && r.Phase == phaseVerified {
					a.removeResult(t.rec.ID)
				}
			}
			a.removeEntry(t.entry)
			return nil
		}
	}
	t.rec.Phase = phaseInstalling
	t.recorded = true
	if err := a.writeRecord(&t.rec); err != nil {
		a.finish(t, phaseFailed, installFailed(err))
		return nil
	}
	// installing → swapped: the installed file must still be the bytes
	// step 10 read; a console edit since is left as found.
	d, err := a.installedDigest()
	if err != nil {
		a.finish(t, phaseFailed, installFailed(err))
		return nil
	}
	if d != t.rec.PrevSHA256 {
		a.changed(t)
		return nil
	}
	if err := a.writeDurable("caddyfile:new", a.caddyfile(), a.caddyfileTmp(t.rec.ID), t.next, 0o644); err != nil {
		// The rename may have happened with only the directory's fsync
		// failing: what is on disk decides, and what cannot be read is
		// left, with the record, for the next run's recovery.
		d, derr := a.installedDigest()
		switch {
		case derr != nil:
			return a.unsettled(t, derr)
		case d == t.rec.NewSHA256:
			a.recordUnwritten(t, err)
			return a.rollback(ctx, t, msgRolledBackUnrecorded, false)
		case d != t.rec.PrevSHA256:
			a.changed(t)
			return nil
		}
		a.removeTemp(t.rec.ID)
		a.finish(t, phaseFailed, installFailed(err))
		return nil
	}
	t.rec.Phase = phaseSwapped
	if err := a.writeRecord(&t.rec); err != nil {
		// Post-swap failure (Failure-mode table, "record swapped").
		a.recordUnwritten(t, err)
		return a.rollback(ctx, t, msgRolledBackUnrecorded, false)
	}
	return a.reloadSwapped(ctx, t)
}

// Rollbacks the catalogue has no words of their own for yet: the nearest
// catalogued text. The PR that owns the catalogue gives them theirs.
const (
	// The swap happened but could not be recorded (disk full): nothing
	// was reloaded, the previous bytes went back.
	msgRolledBackUnrecorded = msgRecordAfterReload
	// A crash fell after the swap; recovery rolled back, the reload's
	// outcome unknown.
	msgRolledBackInterrupted = msgReloadFailed
)

// reloadSwapped is swapped → applied: the reload — or, for init on a box
// that is not running, none (a person is at the console; the file loads
// at the next start) — then the installed file read back, then the
// record, then the baseline.
func (a *Applier) reloadSwapped(ctx context.Context, t *txn) error {
	if t.running {
		if err := a.systemd.Reload(ctx); err != nil {
			a.logger.Warn("box reload failed", zap.String("id", t.rec.ID), zap.String("error", proof.Bound(err.Error())))
			return a.rollback(ctx, t, msgReloadFailed, false)
		}
	}
	// "applied" means these bytes: a console edit since the swap is
	// left as found and the baseline does not advance.
	d, err := a.installedDigest()
	if err != nil {
		return a.unsettled(t, err)
	}
	if d != t.rec.NewSHA256 {
		a.changed(t)
		return nil
	}
	t.rec.Phase = phaseApplied
	if err := a.writeRecord(&t.rec); err != nil {
		// The baseline must never run ahead of the record (I4).
		a.recordUnwritten(t, err)
		return a.rollback(ctx, t, msgRecordAfterReload, false)
	}
	return a.advance(t, phaseApplied)
}

// advance writes applied.json from a record that says no_change or
// applied, then ends the transaction at that phase.
func (a *Applier) advance(t *txn, phase string) error {
	if err := a.writeApplied(&t.rec); err != nil {
		a.logger.Error("box baseline could not be recorded", zap.String("id", t.rec.ID), zap.String("commit", t.rec.Commit), zap.String("error", proof.Bound(err.Error())))
		a.finish(t, phaseUnknown, baselineUnrecorded(t.rec.Commit))
		return nil
	}
	a.finish(t, phase, "")
	return nil
}

// rollback is → rolling_back: the record says so if it can be written
// (if not, it keeps the phase it had, which recovery rolls back from
// too), then the rolling_back row.
func (a *Applier) rollback(ctx context.Context, t *txn, why string, recovering bool) error {
	t.rec.Phase, t.rec.Error = phaseRollingBack, why
	t.recorded = true
	if err := a.writeRecord(&t.rec); err != nil {
		a.logger.Error("box record could not say rolling_back", zap.String("id", t.rec.ID), zap.String("error", proof.Bound(err.Error())))
	}
	return a.rollingBack(ctx, t, recovering)
}

// rollingBack is the rolling_back row: the previous bytes on disk (put
// back if the new ones stand), then a reload — asking is-active first
// in recovery, since a crash may have been a reboot.
func (a *Applier) rollingBack(ctx context.Context, t *txn, recovering bool) error {
	d, err := a.installedDigest()
	if err != nil {
		return a.unsettled(t, err)
	}
	switch d {
	case t.rec.PrevSHA256:
	case t.rec.NewSHA256:
		if err := a.restore(t); err != nil {
			return a.fullDisk(t, err)
		}
	default:
		a.changed(t)
		return nil
	}
	// Nothing to reload on a hotserve that is not running: the previous
	// bytes load at its next start. Recovery asks (a crash may have been
	// a reboot); init asked once, when it began, and did not reload a
	// hotserve that was not running then.
	running := t.running
	if recovering {
		state, err := a.running(ctx)
		if err != nil {
			return a.unsettled(t, err)
		}
		running = up(state)
	}
	if !running {
		if t.rec.Error != "" {
			a.logger.Warn("box rollback on a hotserve that is not running", zap.String("id", t.rec.ID), zap.String("why", t.rec.Error))
		}
		a.finish(t, phaseFailed, msgInterruptedStopped)
		return nil
	}
	if err := a.systemd.Reload(ctx); err != nil {
		a.logger.Warn("box reload of the previous Caddyfile failed", zap.String("id", t.rec.ID), zap.String("error", proof.Bound(err.Error())))
		a.finish(t, phaseUnknown, msgBothReloads)
		return nil
	}
	why := t.rec.Error
	if why == "" {
		why = msgReloadFailed
	}
	a.finish(t, phaseRolledBack, why)
	return nil
}

// restore writes the previous bytes back, retrying once (Failure-mode
// table, "previous bytes back"); a write whose rename landed counts.
func (a *Applier) restore(t *txn) error {
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if err = a.writeDurable("caddyfile:prev", a.caddyfile(), a.caddyfileTmp(t.rec.ID), t.rec.Prev, 0o644); err == nil {
			return nil
		}
	}
	if d, derr := a.installedDigest(); derr == nil && d == t.rec.PrevSHA256 {
		return nil
	}
	return err
}

// fullDisk is the one named full-disk end: the record and the entry
// stay, every field goes to the journal at error level, and the run
// stops non-zero. The next run's recovery tries the write-back again.
func (a *Applier) fullDisk(t *txn, err error) error {
	r := t.result(t.rec.Phase, msgDiskFull)
	a.logger.Error(msgDiskFull, append(resultFields(r), zap.String("write_error", proof.Bound(err.Error())))...)
	return errFullDisk
}

// recordUnwritten journals the write that failed after the swap — the
// record, or the swap's own fsync — whose rollback reports only the
// catalogue's words for it.
func (a *Applier) recordUnwritten(t *txn, err error) {
	a.logger.Error("box: a write failed after the swap; rolling back", zap.String("id", t.rec.ID), zap.String("writing", t.rec.Phase), zap.String("error", proof.Bound(err.Error())))
}

// unsettled stops the run with everything as found.
func (a *Applier) unsettled(t *txn, err error) error {
	a.logger.Error("box transaction left for the next run", zap.String("id", t.rec.ID), zap.String("phase", t.rec.Phase), zap.String("error", proof.Bound(err.Error())))
	return errUnsettled
}

// changed is the States table's `any` row: a console edit under the
// transaction. Nothing is written to /etc/hotserve but the removal of
// the applier's own temporary.
func (a *Applier) changed(t *txn) {
	a.logger.Warn("box: the Caddyfile changed during the transaction; it is left as found", zap.String("id", t.rec.ID), zap.String("phase", t.rec.Phase))
	a.removeTemp(t.rec.ID)
	a.finish(t, phaseUnknown, msgChanged)
}

// recoverRecord settles a record a crash left (DESIGN-box.md, States
// table): by its phase, after checking the installed file's digest d
// against what the phase implies. A record whose result is already
// terminal finished all but its removals.
func (a *Applier) recoverRecord(ctx context.Context, rec *record) error {
	t := &txn{rec: *rec, recorded: true}
	if rec.Origin == originApplier {
		t.entry = filepath.Join(a.x("work"), rec.ID+".tar")
		if r, err := a.readResult(rec.ID); err == nil && terminal(r.Phase) {
			a.removeRecord()
			a.removeEntry(t.entry)
			return nil
		}
	}
	a.removeTemp(rec.ID)
	d, err := a.installedDigest()
	if err != nil {
		return a.unsettled(t, err)
	}
	prev, next := rec.PrevSHA256, rec.NewSHA256
	switch rec.Phase {
	case phaseNoChange:
		if d == prev {
			return a.advance(t, phaseNoChange)
		}
	case phaseInstalling:
		switch d {
		case prev:
			a.finish(t, phaseFailed, msgInterrupted)
			return nil
		case next: // the crash fell after the swap
			return a.recoverSwapped(ctx, t, d)
		}
	case phaseSwapped:
		if d == next || d == prev {
			return a.recoverSwapped(ctx, t, d)
		}
	case phaseApplied:
		if d == next {
			return a.advance(t, phaseApplied)
		}
	case phaseRollingBack:
		if d == prev || d == next {
			return a.rollingBack(ctx, t, true)
		}
	}
	a.changed(t)
	return nil
}

// recoverSwapped is the swapped row: roll back, except init on a box
// that is not running, whose swap counts as applied.
func (a *Applier) recoverSwapped(ctx context.Context, t *txn, d string) error {
	if t.rec.Origin == originInit && d == t.rec.NewSHA256 {
		state, err := a.running(ctx)
		if err != nil {
			return a.unsettled(t, err)
		}
		if !up(state) {
			t.rec.Phase = phaseApplied
			if err := a.writeRecord(&t.rec); err != nil {
				return a.rollback(ctx, t, msgRecordAfterReload, true)
			}
			return a.advance(t, phaseApplied)
		}
	}
	return a.rollback(ctx, t, msgRolledBackInterrupted, true)
}

// runInit is `hotserve init`'s way into the transaction (PR 4): under
// root's lock, after recovery, the same state machine with origin init —
// no `is-active` refusal, no result files, the swap counting as applied
// when hotserve is not running. The caller has walked and validated the
// file; plan.rec carries the id, commit, path, host and apps.
func (a *Applier) runInit(ctx context.Context, rec record, file []byte) (*result, error) {
	// The record must be one recovery can read back (record.valid), and
	// the id names the swap's temporary.
	if !isRequestID(rec.ID) || !proof.IsID(rec.Commit) {
		return nil, errors.New("init: the id must be 32 hex and the commit 40 hex")
	}
	if _, err := proof.SplitPath(rec.Path); err != nil {
		return nil, fmt.Errorf("init: %w", err)
	}
	if len(file) > proof.MaxCaddyfile {
		return nil, fmt.Errorf("init: the Caddyfile is larger than %d bytes", proof.MaxCaddyfile)
	}
	unlock, err := a.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := a.recoverAll(ctx); err != nil {
		return nil, err
	}
	installed, err := readFile(a.caddyfile(), proof.MaxCaddyfile, true)
	if err != nil {
		return nil, err
	}
	state, err := a.running(ctx)
	if err != nil {
		return nil, err
	}
	rec.Origin, rec.Signer = originInit, "init"
	rec.Prev, rec.PrevSHA256, rec.NewSHA256 = installed, digest(installed), digest(file)
	rec.Diff = caddyfileDiff(installed, file)
	t := &txn{rec: rec, next: file, running: up(state)}
	if err := a.install(ctx, t); err != nil {
		return nil, err
	}
	return t.outcome, nil
}
