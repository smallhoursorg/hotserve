package box

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/smallhoursorg/hotserve/box/proof"
)

// The install transaction (DESIGN-box.md, "The install transaction"):
// the state machine from the buffer comparison on, shared by the
// applier and `init` through origin, and recovery by the States table.
// Every durable write is writeDurable's or removeDurable's, in the order
// the Transitions table gives; the tests cite I1–I9 per write.
//
// The set is the Caddyfile and each env file step 15a put in the record
// (envfiles.go). The Caddyfile is always the record's first file; it is
// in the set — written, and reloaded — only when its bytes change, so a
// set of env files alone reloads nothing. Env files are written first,
// in app-name order, then the Caddyfile, so that a reload of a file
// that names a new env_file finds it in place. Recovery reads every
// file of the set against its own prev and new and acts on the phase.

// origin is who runs a transaction; recovery honours it.
type origin string

const (
	originApplier origin = "applier"
	originInit    origin = "init"
)

// record is txn.json (DESIGN-box.md, "Record and result fields"): the
// one durable marker of a transaction in flight. Present means "no
// terminal result yet" (I5). Prev holds the installed file's bytes as
// step 10 read them, and each Secrets entry its env file's as step 15a
// read them: the only rollback sources, and the one place a secret's
// plaintext is besides its env file (I9). Error is, in a rolling_back
// record, the text its rolled_back result will carry.
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

	// Secrets is the set's env files, in app-name order.
	Secrets []envEntry `json:"secrets,omitempty"`
	// SecretsTree and SecretsInstalled are what applied.json gets:
	// HEAD's secrets tree, and the digest of every env file the box
	// manages once the push applies.
	SecretsTree      string            `json:"secrets_tree,omitempty"`
	SecretsInstalled map[string]string `json:"secrets_installed,omitempty"`
	SecretsAdded     []string          `json:"secrets_added,omitempty"`
	SecretsChanged   []string          `json:"secrets_changed,omitempty"`
	SecretsRemoved   []string          `json:"secrets_removed,omitempty"`
	SecretsOutOfBand []string          `json:"secrets_edited_out_of_band,omitempty"`
}

// envEntry is one env file of the set: its bytes before the push (none,
// and prev_sha256 "", when no file stood there) and the rendered file's
// digest ("" when the push removes it).
type envEntry struct {
	App        string `json:"app"`
	Prev       []byte `json:"prev,omitempty"`
	PrevSHA256 string `json:"prev_sha256"`
	NewSHA256  string `json:"new_sha256"`
}

// maxRecord caps txn.json as read back (Caps): Prev's 1 MiB is 1.4 MiB
// of base64, the diff's 64 KiB at most 384 KiB of JSON escapes, the
// app names a 1 MiB file can hold under 1 MiB, the env files' previous
// bytes — a set holds at most 2 × MaxSecretApps, each at most maxEnvRead
// — 5.5 MiB of base64, and the secrets' names 3.2 MiB: about 11.5 MiB.
// Only root writes the file; the cap is a bound, not a trust decision.
const maxRecord = 16 << 20

// maxSecretNames bounds each of a record's and a result's name lists:
// one push adds, changes or removes at most every secret a bundle can
// carry.
const maxSecretNames = proof.MaxBlobs

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
	// Recovery writes applied.json from the record: its path must be one
	// the next push can be checked against.
	if _, err := proof.SplitPath(r.Path); err != nil {
		return errors.New("path is not a repository path")
	}
	if err := r.validSecrets(); err != nil {
		return err
	}
	switch r.Phase {
	case phaseNoChange:
		// Buffers identical and no env file in the set, by definition:
		// recovery writes applied.json with new_sha256, which must then
		// be the bytes that stand.
		if r.NewSHA256 != r.PrevSHA256 {
			return errors.New("a no_change record whose digests differ")
		}
		if len(r.Secrets) > 0 {
			return errors.New("a no_change record with env files in its set")
		}
		return nil
	case phaseInstalling, phaseSwapped, phaseApplied, phaseRollingBack:
		return nil
	}
	return errors.New("phase is not a record's")
}

// validSecrets is valid for the record's secrets: each entry an app in
// the grammar, once, in order, its prev the bytes its digest names, and
// a change in it; the rest in their grammars and caps.
func (r *record) validSecrets() error {
	if len(r.Secrets) > 2*proof.MaxSecretApps {
		return errors.New("more env files than a set holds")
	}
	for i, e := range r.Secrets {
		switch {
		case !appName.MatchString(e.App) || (i > 0 && e.App <= r.Secrets[i-1].App):
			return errors.New("an env file's app is not in the grammar, or out of order")
		case e.PrevSHA256 == "" && (len(e.Prev) > 0 || e.NewSHA256 == ""):
			return errors.New("an env file that was absent with bytes, or absent before and after")
		case e.PrevSHA256 != "" && (len(e.Prev) > maxEnvRead || digest(e.Prev) != e.PrevSHA256):
			return errors.New("an env file's prev is not the bytes prev_sha256 names")
		case e.NewSHA256 != "" && !isDigest(e.NewSHA256):
			return errors.New("an env file's digest is not 64 hex")
		}
	}
	if r.SecretsTree != "" && !proof.IsID(r.SecretsTree) {
		return errors.New("secrets_tree is not 40 hex")
	}
	if err := validInstalled(r.SecretsInstalled); err != nil {
		return err
	}
	for _, names := range [][]string{r.SecretsAdded, r.SecretsChanged, r.SecretsRemoved} {
		if !validSecretNames(names) {
			return errors.New("a secret is not named <app>/<VAR>")
		}
	}
	if len(r.SecretsOutOfBand) > 2*proof.MaxSecretApps || slices.ContainsFunc(r.SecretsOutOfBand, func(s string) bool { return !appName.MatchString(s) }) {
		return errors.New("secrets_edited_out_of_band is not app names")
	}
	return nil
}

// validInstalled holds an app → digest map of managed env files, as
// the record and applied.json carry it, to the grammar and the cap.
func validInstalled(m map[string]string) error {
	if len(m) > proof.MaxSecretApps {
		return errors.New("more env files listed than apps may have secrets")
	}
	for app, d := range m {
		if !appName.MatchString(app) || !isDigest(d) {
			return errors.New("an env file listed is not an app and a digest")
		}
	}
	return nil
}

// validSecretNames reports whether names is a list root writes: within
// the cap, each `<app>/<VAR>`.
func validSecretNames(names []string) bool {
	return len(names) <= maxSecretNames && !slices.ContainsFunc(names, func(s string) bool { return !secretName(s) })
}

// txn is one transaction in memory.
type txn struct {
	rec record
	// next is the incoming file, and nextEnv each env file the swap
	// writes; recovery never needs them.
	next    []byte
	nextEnv map[string][]byte
	// entry is work/<id>.tar, removed after the terminal result; ""
	// for init, which has none.
	entry string
	// recorded is true once a record of this transaction may be on disk.
	recorded bool
	// outcome is the terminal result, for init's caller to print.
	outcome *result
	// named is a refusal's object ids, for its result's `named`.
	named []string
}

// files reports whether env files are in the set: the catalogue's texts
// then name "the box's files".
func (t *txn) files() bool { return len(t.rec.Secrets) > 0 }

// caddyfileInSet reports whether the push changes the Caddyfile: only
// then is it written, and hotserve reloaded.
func (t *txn) caddyfileInSet() bool { return t.rec.PrevSHA256 != t.rec.NewSHA256 }

// pick is the catalogue's text for the set: the first when it is the
// Caddyfile alone, the second when env files are in it.
func (t *txn) pick(caddyfile, files string) string {
	if t.files() {
		return files
	}
	return caddyfile
}

func (t *txn) installFailed(err error) string {
	if t.files() {
		return installFailedFiles(err)
	}
	return installFailed(err)
}

// env is the set's entry for app.
func (t *txn) env(app string) envEntry {
	for _, e := range t.rec.Secrets {
		if e.App == app {
			return e
		}
	}
	return envEntry{} // unreachable: callers name an app of the set
}

var (
	// errFullDisk is the one run that ends unsettled by design: the
	// previous bytes of a file of the set could not be written back, so
	// the record stays for the next run (Failure-mode table, "record
	// swapped"). The command exits non-zero on it.
	errFullDisk = errors.New(msgDiskFull)
	// errUnsettled ends a run that cannot reason safely — a record it
	// cannot read, a file of the set unreadable mid-transaction,
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
		Named:        t.named,
		SecretsAdded: r.SecretsAdded, SecretsChanged: r.SecretsChanged, SecretsRemoved: r.SecretsRemoved,
		SecretsOutOfBand: r.SecretsOutOfBand,
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
// from a record that says applied or no_change, the applier's or init's),
// with the secrets the box manages from then on.
func (a *Applier) writeApplied(rec *record) error {
	return a.putApplied(applied{
		SHA: rec.Commit, Path: rec.Path, SHA256: rec.NewSHA256, Signer: rec.Signer, When: a.clock.Now().UTC(),
		SecretsTree: rec.SecretsTree, Secrets: rec.SecretsInstalled,
	})
}

// putApplied writes applied.json whole (I8): writeApplied's write, and
// `hotserve box baseline`'s.
func (a *Applier) putApplied(v applied) error {
	return a.writeDurable("applied.json", a.x("applied.json"), a.x(".applied.json.tmp"), encodeJSON(v), 0o640)
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

// fileAt is one file of the set as it stands: at its prev, at its new —
// both, for a file whose bytes the push keeps — or at neither. app is
// "" for the Caddyfile.
type fileAt struct {
	app        string
	prev, next bool
}

func atPrev(f fileAt) bool { return f.prev }
func atNew(f fileAt) bool  { return f.next }
func known(f fileAt) bool  { return f.prev || f.next }

// states is d for every file of the set (States table): the Caddyfile
// first, then each env file the record lists. An error is a read's own,
// for the caller to stop on as found.
func (a *Applier) states(t *txn) ([]fileAt, error) {
	d, err := a.installedDigest()
	if err != nil {
		return nil, err
	}
	out := []fileAt{{prev: d == t.rec.PrevSHA256, next: d == t.rec.NewSHA256}}
	for _, e := range t.rec.Secrets {
		d, err := a.envDigest(e.App)
		if err != nil {
			return nil, err
		}
		out = append(out, fileAt{app: e.App, prev: d == e.PrevSHA256, next: d == e.NewSHA256})
	}
	return out, nil
}

// firstNot is the first file of fs that is not as want says, if any:
// ok is true when every one is.
func firstNot(fs []fileAt, want func(fileAt) bool) (app string, ok bool) {
	for _, f := range fs {
		if !want(f) {
			return f.app, false
		}
	}
	return "", true
}

// fileDigest is d for one file of the set, by its app ("" for the
// Caddyfile), with its prev and new.
func (a *Applier) fileDigest(t *txn, app string) (d, prev, next string, err error) {
	if app == "" {
		d, err = a.installedDigest()
		return d, t.rec.PrevSHA256, t.rec.NewSHA256, err
	}
	e := t.env(app)
	d, err = a.envDigest(app)
	return d, e.PrevSHA256, e.NewSHA256, err
}

// install runs the transaction from the buffer comparison (Transitions
// table, from "checking → no_change" and "checking → verified" on). The
// caller has run steps 10–15a (init: the walk) and found hotserve
// active (init: up). It returns errFullDisk or errUnsettled when the run
// must stop with the record on disk, nil once a terminal result is
// written (or journaled).
func (a *Applier) install(ctx context.Context, t *txn) error {
	if bytes.Equal(t.rec.Prev, t.next) && !t.files() {
		t.rec.Phase = phaseNoChange
		t.recorded = true // a failed write may still have renamed it into place
		if err := a.writeRecord(&t.rec); err != nil {
			a.finish(t, phaseFailed, installFailed(err))
			return nil
		}
		return a.advance(t, phaseNoChange) // which re-reads the files first
	}
	if t.rec.Origin == originApplier {
		if err := a.writeResult(t.result(phaseVerified, "")); err != nil {
			// Nothing has changed (Transitions table, "checking →
			// verified"): the push ends `failed` if that can be written
			// — over a `verified` whose rename landed before the
			// failure, which nothing else would ever replace — and if
			// not, any such `verified` goes, leaving a marker with no
			// result for Retention to settle. The entry goes either way.
			if a.writeResult(t.result(phaseFailed, t.installFailed(err))) != nil {
				a.removeVerified(t.rec.ID)
			}
			a.removeEntry(t.entry)
			return nil
		}
	}
	t.rec.Phase = phaseInstalling
	t.recorded = true
	if err := a.writeRecord(&t.rec); err != nil {
		a.finish(t, phaseFailed, t.installFailed(err))
		return nil
	}
	// installing → swapped: every file of the set must still be the
	// bytes step 10 and 15a read; a console edit since is left as found.
	fs, err := a.states(t)
	if err != nil {
		a.finish(t, phaseFailed, t.installFailed(err))
		return nil
	}
	if odd, ok := firstNot(fs, atPrev); !ok {
		a.changed(t, odd)
		return nil
	}
	// The env files, in app-name order, then the Caddyfile. A write that
	// fails once a file has changed is the post-swap failure.
	wrote := false
	for _, e := range t.rec.Secrets {
		if err := a.writeEnv(t, e); err != nil {
			if wrote {
				a.recordUnwritten(t, err)
				return a.rollback(ctx, t, msgRolledBackUnrecordedFiles, false)
			}
			return a.firstWriteFailed(ctx, t, e.App, err)
		}
		wrote = true
	}
	if t.caddyfileInSet() {
		if err := a.writeDurable("caddyfile:new", a.caddyfile(), a.caddyfileTmp(t.rec.ID), t.next, 0o644); err != nil {
			if wrote {
				a.recordUnwritten(t, err)
				return a.rollback(ctx, t, msgRolledBackUnrecordedFiles, false)
			}
			return a.firstWriteFailed(ctx, t, "", err)
		}
	}
	t.rec.Phase = phaseSwapped
	if err := a.writeRecord(&t.rec); err != nil {
		// Post-swap failure (Failure-mode table, "record swapped").
		a.recordUnwritten(t, err)
		return a.rollback(ctx, t, t.pick(msgRolledBackUnrecorded, msgRolledBackUnrecordedFiles), false)
	}
	return a.reloadSwapped(ctx, t)
}

// firstWriteFailed settles the swap's first write that failed, before
// any file of the set is known to have changed: the rename may have
// happened with only the directory's fsync failing, so what is on disk
// decides, and what cannot be read is left, with the record, for the
// next run's recovery. A file whose bytes the push keeps (an env file
// rewritten for its mode) reads as unchanged either way.
func (a *Applier) firstWriteFailed(ctx context.Context, t *txn, app string, err error) error {
	d, prev, next, derr := a.fileDigest(t, app)
	switch {
	case derr != nil:
		return a.unsettled(t, derr)
	case d == prev:
		a.removeTemp(t.rec.ID)
		a.finish(t, phaseFailed, t.installFailed(err))
		return nil
	case d == next:
		a.recordUnwritten(t, err)
		return a.rollback(ctx, t, t.pick(msgRolledBackUnrecorded, msgRolledBackUnrecordedFiles), false)
	}
	a.changed(t, app)
	return nil
}

// reloadSwapped is swapped → applied: the reload, if the Caddyfile is in
// the set, then every file of the set read back, then the record, then
// the baseline.
func (a *Applier) reloadSwapped(ctx context.Context, t *txn) error {
	if t.caddyfileInSet() {
		if err := a.systemd.Reload(ctx); err != nil {
			a.logger.Warn("box reload failed", zap.String("id", t.rec.ID), zap.String("error", proof.Bound(err.Error())))
			return a.rollback(ctx, t, msgReloadFailed, false)
		}
	}
	// "applied" means these bytes: a console edit since the swap is
	// left as found and the baseline does not advance.
	fs, err := a.states(t)
	if err != nil {
		return a.unsettled(t, err)
	}
	if odd, ok := firstNot(fs, atNew); !ok {
		a.changed(t, odd)
		return nil
	}
	t.rec.Phase = phaseApplied
	if err := a.writeRecord(&t.rec); err != nil {
		// The baseline must never run ahead of the record (I4). With no
		// reload asked, the catalogue's text for a write after the
		// first file changed says what happened.
		a.recordUnwritten(t, err)
		why := msgRecordAfterReload
		if !t.caddyfileInSet() {
			why = msgRolledBackUnrecordedFiles
		}
		return a.rollback(ctx, t, why, false)
	}
	return a.advance(t, phaseApplied)
}

// advance writes applied.json from a record that says no_change or
// applied, then ends the transaction at that phase. Every write of the
// baseline comes through here, so here is where it is held to I1 and
// I4: the baseline names the record's new bytes only if they still
// stand — every file of the set re-read now, after whatever wait came
// before, and for the applier every env file applied.json will list —
// and a console edit since is left as found, `unknown`, the baseline not
// advanced. A file that cannot be read stops the run as found (I2); the
// record stays for recovery, which reads it again. init's set is the
// Caddyfile alone, and the env files it lists are kept from the file it
// replaces: it checks none of them.
func (a *Applier) advance(t *txn, phase string) error {
	fs, err := a.states(t)
	if err != nil {
		return a.unsettled(t, err)
	}
	if odd, ok := firstNot(fs, atNew); !ok {
		a.changed(t, odd)
		return nil
	}
	if t.rec.Origin == originApplier {
		inSet := map[string]bool{}
		for _, e := range t.rec.Secrets {
			inSet[e.App] = true
		}
		for _, app := range slices.Sorted(maps.Keys(t.rec.SecretsInstalled)) {
			if inSet[app] {
				continue
			}
			d, err := a.envDigest(app)
			if err != nil {
				return a.unsettled(t, err)
			}
			if d != t.rec.SecretsInstalled[app] {
				a.changed(t, app)
				return nil
			}
		}
	}
	if err := a.writeApplied(&t.rec); err != nil {
		a.logger.Error("box baseline could not be recorded", zap.String("id", t.rec.ID), zap.String("commit", t.rec.Commit), zap.String("error", proof.Bound(err.Error())))
		a.finish(t, phaseUnknown, baselineUnrecorded(t.rec.Origin, phase, t.rec.Commit))
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

// rollingBack is the rolling_back row: the previous bytes of every file
// of the set on disk (put back where the new ones stand), then — if the
// Caddyfile was in the set — a reload, asking is-active first in
// recovery, since a crash may have been a reboot. A set of env files
// alone reloads nothing: they are back, and the push rolled back.
func (a *Applier) rollingBack(ctx context.Context, t *txn, recovering bool) error {
	fs, err := a.states(t)
	if err != nil {
		return a.unsettled(t, err)
	}
	if odd, ok := firstNot(fs, known); !ok {
		a.changed(t, odd)
		return nil
	}
	for _, f := range fs {
		if f.prev {
			continue
		}
		if f.app == "" {
			if err := a.restore(t); err != nil {
				return a.fullDisk(t, err, "")
			}
			continue
		}
		if err := a.restoreEnv(t, t.env(f.app)); err != nil {
			return a.fullDisk(t, err, f.app)
		}
	}
	why := t.rec.Error
	if !t.caddyfileInSet() {
		if why == "" {
			why = msgRolledBackInterruptedFiles
		}
		a.finish(t, phaseRolledBack, why)
		return nil
	}
	// Nothing to reload on a hotserve that is not running: the previous
	// bytes load at its next start. Recovery asks (a crash may have been
	// a reboot); a live run found hotserve up before it swapped.
	if recovering {
		state, err := a.running(ctx)
		if err != nil {
			return a.unsettled(t, err)
		}
		if !up(state) {
			if t.rec.Error != "" {
				a.logger.Warn("box rollback on a hotserve that is not running", zap.String("id", t.rec.ID), zap.String("why", t.rec.Error))
			}
			a.finish(t, phaseFailed, msgInterruptedStopped)
			return nil
		}
	}
	if err := a.systemd.Reload(ctx); err != nil {
		a.logger.Warn("box reload of the previous Caddyfile failed", zap.String("id", t.rec.ID), zap.String("error", proof.Bound(err.Error())))
		a.finish(t, phaseUnknown, bothFailed(t.rec.Error))
		return nil
	}
	if why == "" {
		why = msgReloadFailed
	}
	a.finish(t, phaseRolledBack, why)
	return nil
}

// bothFailed is `unknown` for a rollback whose reload of the previous
// file failed, by the reason it rolled back: "both reloads failed" only
// when the record says the first one did — not after a reload that
// succeeded (the record `applied` unwritable), none (the swap
// unrecorded), one a crash cut short, or a reason the record does not
// give.
func bothFailed(why string) string {
	if why == msgReloadFailed {
		return msgBothReloads
	}
	return msgPrevReloadFailed
}

// restore writes the previous Caddyfile back, retrying once
// (Failure-mode table, "previous bytes back"); a write whose rename
// landed counts.
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
// app names the env file that could not be restored ("" for the
// Caddyfile).
func (a *Applier) fullDisk(t *txn, err error, app string) error {
	msg := msgDiskFull
	if app != "" {
		msg = fill(msgEnvDiskFull, app)
	}
	r := t.result(t.rec.Phase, msg)
	a.logger.Error(msg, append(resultFields(r), zap.String("write_error", proof.Bound(err.Error())))...)
	return errFullDisk
}

// recordUnwritten journals the write that failed after the swap — the
// record, a later file, or the swap's own fsync — whose rollback reports
// only the catalogue's words for it.
func (a *Applier) recordUnwritten(t *txn, err error) {
	a.logger.Error("box: a write failed after the swap; rolling back", zap.String("id", t.rec.ID), zap.String("writing", t.rec.Phase), zap.String("error", proof.Bound(err.Error())))
}

// unsettled stops the run with everything as found.
func (a *Applier) unsettled(t *txn, err error) error {
	a.logger.Error("box transaction stopped with everything as found", zap.String("id", t.rec.ID), zap.String("phase", t.rec.Phase), zap.String("error", proof.Bound(err.Error())))
	return errUnsettled
}

// changed is the States table's `any` row: a console edit under the
// transaction, of the file app names ("" for the Caddyfile). Nothing is
// written to /etc/hotserve but the removal of the applier's own
// temporaries.
func (a *Applier) changed(t *txn, app string) {
	msg := msgChanged
	if app != "" {
		msg = fill(msgEnvChanged, app)
	}
	a.logger.Warn("box: "+msg, zap.String("id", t.rec.ID), zap.String("phase", t.rec.Phase))
	a.removeTemp(t.rec.ID)
	a.finish(t, phaseUnknown, msg)
}

// recoverRecord settles a record a crash left (DESIGN-box.md, States
// table): by its phase, after reading every file of the set against what
// the phase implies. A record whose result is already terminal finished
// all but its removals.
func (a *Applier) recoverRecord(ctx context.Context, rec *record) error {
	t := &txn{rec: *rec, recorded: true}
	if rec.Origin == originApplier {
		t.entry = filepath.Join(a.x("work"), rec.ID+".tar")
		if a.settled(rec.ID) {
			a.removeRecord()
			a.removeEntry(t.entry)
			return nil
		}
	}
	a.removeTemp(rec.ID)
	fs, err := a.states(t)
	if err != nil {
		return a.unsettled(t, err)
	}
	_, allPrev := firstNot(fs, atPrev)
	_, allNew := firstNot(fs, atNew)
	_, allKnown := firstNot(fs, known)
	want := known
	switch rec.Phase {
	case phaseNoChange:
		if allPrev {
			return a.advance(t, phaseNoChange)
		}
		want = atPrev
	case phaseInstalling:
		switch {
		case allPrev:
			a.finish(t, phaseFailed, t.pick(msgInterrupted, msgInterruptedFiles))
			return nil
		case allKnown: // the crash fell in or after the swap: as swapped
			return a.rollback(ctx, t, t.pick(msgRolledBackInterrupted, msgRolledBackInterruptedFiles), true)
		}
	case phaseSwapped:
		// The reload's outcome is unknown, whoever began it: roll back.
		if allKnown {
			return a.rollback(ctx, t, t.pick(msgRolledBackInterrupted, msgRolledBackInterruptedFiles), true)
		}
	case phaseApplied:
		if allNew {
			return a.advance(t, phaseApplied)
		}
		want = atNew
	case phaseRollingBack:
		if allKnown {
			return a.rollingBack(ctx, t, true)
		}
	}
	odd, _ := firstNot(fs, want)
	a.changed(t, odd)
	return nil
}

// initRun is what runInit reports for init's printout: the outcome, and
// the baseline the transaction began over (nil if there was none).
type initRun struct {
	*result
	was *applied
}

// notUpError is init's refusal on a hotserve that is not up once the
// wait is over. Nothing is written, and the message is the applier's for
// the same state (DESIGN-box.md, "Console commands").
type notUpError struct{ msg string }

func (e *notUpError) Error() string { return e.msg }

// runInit is `hotserve init`'s way into the transaction (DESIGN-box.md,
// "`hotserve init`"): under root's lock, after recovery, step 16's
// question once — anything but up refuses before anything is written,
// since the reload is the only check init makes of the file — then the
// same state machine with origin init and no result files. The caller
// has walked the file; rec carries the id, commit, path, host and apps.
// init installs no secrets: its set is the Caddyfile alone, and the
// secrets applied.json lists are kept from the file it replaces, so the
// first push decides the env files by the full chain.
func (a *Applier) runInit(ctx context.Context, rec record, file []byte) (*initRun, error) {
	// The record must be one recovery can read back (record.valid), and
	// the id names the swap's temporary.
	if !isRequestID(rec.ID) || !proof.IsID(rec.Commit) {
		return nil, errors.New("init: the id must be 32 hex and the commit 40 hex")
	}
	if _, err := proof.SplitPath(rec.Path); err != nil {
		return nil, fmt.Errorf("init: %w", err)
	}
	if !utf8.ValidString(rec.Path) {
		// JSON would write it back changed, and no bundle's path could
		// ever equal the one applied.json then holds.
		return nil, errors.New("init: the path is not UTF-8")
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
	state, err := a.running(ctx)
	if err != nil {
		return nil, err
	}
	if !up(state) {
		if transient(state) { // activating still, after the wait
			return nil, &notUpError{msg: msgStillStarting}
		}
		return nil, &notUpError{msg: msgNotRunning}
	}
	installed, err := readFile(a.caddyfile(), proof.MaxCaddyfile, true)
	if err != nil {
		return nil, err
	}
	if a.initPrev != nil {
		a.initPrev(installed)
	}
	run := &initRun{}
	// One that does not read is replaced like a missing one: init is
	// the way back, and only root writes it (Triage: occurrence 1).
	if was, err := readApplied(a.x("")); err == nil {
		run.was = was
		rec.SecretsTree, rec.SecretsInstalled = was.SecretsTree, was.Secrets
	}
	rec.Origin, rec.Signer = originInit, "init"
	// No diff: nobody reads one for init (it writes no result).
	rec.Prev, rec.PrevSHA256, rec.NewSHA256 = installed, digest(installed), digest(file)
	t := &txn{rec: rec, next: file}
	if err := a.install(ctx, t); err != nil {
		return nil, err
	}
	run.result = t.outcome
	return run, nil
}
