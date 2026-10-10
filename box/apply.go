package box

// `hotserve box apply`: the root applier (DESIGN-box.md, "The trust
// chain" steps 9–16, "The install transaction", "Retention"). One shot,
// started by hotserve-box-apply.path when in/ or work/ is not empty or
// txn.json exists.
//
// The rules every function here keeps:
//
//  1. Nothing is verified from disk: each bundle is read once, into
//     memory, and every check runs on those bytes (step 9).
//  2. Every durable write goes through writeDurable or removeDurable,
//     which carry the test hooks, in the order the Transitions table
//     gives (I8: temporary fsynced, renamed, directory fsynced).
//  3. Nothing is written to disk from a defer, so a test that panics at
//     a hook stands for a crash faithfully.
//  4. Every value a bundle, a marker or the hotserve uid chose that
//     reaches a result, a record or the journal is held to a grammar
//     (ids, hosts, principals, app names) or passes proof.Bound or the
//     diff's redaction and cap.
//  5. Recovery acts on the record's phase, after checking the installed
//     file's digest against what the phase implies; never on digests
//     alone.
//
// The exit status is 0 for every run but the full-disk end, which
// leaves the record for the next run by design (Failure-mode table).

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/smallhoursorg/hotserve/box/proof"
)

// Applier is one run of the applier. Its fields are the seams the tests
// use: the filesystem root the box's paths are joined to, the service
// manager, the clock, the verifier and the hooks.
type Applier struct {
	root     string
	systemd  Systemd
	clock    Clock
	verifier *proof.Verifier
	logger   *zap.Logger
	hooks    hooks
}

// nobody is the uid (and gid) ssh-keygen runs as (step 12).
const nobody = 65534

// newApplier is the applier as the unit runs it.
func newApplier(logger *zap.Logger) *Applier {
	return &Applier{
		root:     "/",
		systemd:  systemctl{unit: "hotserve"},
		clock:    realClock{},
		verifier: &proof.Verifier{RunAs: nobody},
		logger:   logger,
	}
}

// x is a path in the exchange tree.
func (a *Applier) x(name string) string { return filepath.Join(a.root, exchangeDir, name) }

func (a *Applier) caddyfile() string { return filepath.Join(a.root, installedFile) }

// caddyfileTmp is the swap's temporary, `.Caddyfile.box-<id>`: a name
// no sudoers line of bin/push grants (DESIGN-box.md, Paths notes).
func (a *Applier) caddyfileTmp(id string) string {
	return filepath.Join(filepath.Dir(a.caddyfile()), ".Caddyfile.box-"+id)
}

// installedDigest is d: the installed file's digest as it stands. A
// file that is missing or over the cap is neither prev nor new — a
// console's doing — and reads as "" so that it matches nothing.
func (a *Applier) installedDigest() (string, error) {
	b, err := readFile(a.caddyfile(), proof.MaxCaddyfile, true)
	var big *tooLargeError
	if errors.Is(err, fs.ErrNotExist) || errors.As(err, &big) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return digest(b), nil
}

// removeTemp removes the swap's temporary for id if a crash left it.
// Not a write recovery reasons from: its bytes are never read.
func (a *Applier) removeTemp(id string) {
	if err := os.Remove(a.caddyfileTmp(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		a.logger.Warn("box: could not remove a temporary", zap.String("error", proof.Bound(err.Error())))
	}
}

// lock takes root's blocking lock (DESIGN-box.md, Paths: `lock`), shared
// with init, baseline and edit. A crash releases it with the descriptor.
func (a *Applier) lock() (func(), error) {
	f, err := os.OpenFile(a.x("lock"), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}

// Run is one applier run: recover, then take, then each bundle, then
// Retention. It returns an error only for the full-disk end.
func (a *Applier) Run(ctx context.Context) error {
	unlock, err := a.lock()
	if err != nil {
		a.logger.Error("box apply could not take its lock", zap.String("error", proof.Bound(err.Error())))
		return nil
	}
	defer unlock()
	if err := a.recoverAll(ctx); err != nil {
		return fullDiskOnly(err)
	}
	for _, id := range a.take() {
		if err := a.process(ctx, id); err != nil {
			// The record stays for the next run; what else was taken
			// stays in work/ with it (I2's one exception).
			return fullDiskOnly(err)
		}
	}
	a.sweep()
	return nil
}

func fullDiskOnly(err error) error {
	if errors.Is(err, errFullDisk) {
		return err
	}
	return nil
}

// recoverAll settles what a crash left (step 9): the record by its phase,
// then the applier's own stale temporaries, then every work/ entry with
// no record behind it (States table, "(no record)").
func (a *Applier) recoverAll(ctx context.Context) error {
	rec, err := a.readRecord()
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		// Only root writes the record, whole, by rename: one that does
		// not read is a fault no table reasons about. It is kept, and
		// nothing is touched (States table, "record unreadable").
		a.logger.Error("box: txn.json cannot be read; nothing is applied until the console removes it", zap.String("error", proof.Bound(err.Error())))
		return errUnsettled
	default:
		if err := a.recoverRecord(ctx, rec); err != nil {
			return err
		}
	}
	a.sweepTemporaries()
	entries, err := os.ReadDir(a.x("work"))
	if err != nil {
		a.logger.Error("box: work/ cannot be listed", zap.String("error", proof.Bound(err.Error())))
		return errUnsettled
	}
	for _, e := range entries {
		id, ok := a.bundleEntry("work", e.Name())
		if !ok {
			continue
		}
		path := filepath.Join(a.x("work"), e.Name())
		prior, err := a.readResult(id)
		if err == nil && terminal(prior.Phase) {
			a.removeEntry(path)
			continue
		}
		// Taken, or verified, and no record: nothing changed.
		t := &txn{rec: record{ID: id, Origin: originApplier}, entry: path}
		if err == nil { // a verified result: keep what it said
			t.rec.Commit, t.rec.Path, t.rec.Signer, t.rec.BoxWebhook = prior.Commit, prior.Path, prior.Signer, prior.BoxWebhook
			t.rec.OutOfBand, t.rec.Apps, t.rec.Diff = prior.OutOfBand, prior.Apps, prior.Diff
		}
		a.finish(t, phaseFailed, msgInterrupted)
	}
	return nil
}

// sweepTemporaries removes the temporaries a crash can leave: the swap's
// in /etc/hotserve, and writeDurable's beside txn.json, applied.json and
// the results. Every one is in a directory only root writes.
func (a *Applier) sweepTemporaries() {
	etc := filepath.Dir(a.caddyfile())
	var stale []string
	if names, err := filepath.Glob(filepath.Join(etc, ".Caddyfile.box-*")); err == nil {
		stale = append(stale, names...)
	}
	stale = append(stale, a.x(".txn.json.tmp"), a.x(".applied.json.tmp"))
	if names, err := filepath.Glob(filepath.Join(a.x("out"), ".*.json.tmp")); err == nil {
		stale = append(stale, names...)
	}
	for _, p := range stale {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			a.logger.Warn("box: could not remove a temporary", zap.String("error", proof.Bound(err.Error())))
		}
	}
}

// bundleName is what an entry of in/ or work/ must be called to be a
// bundle: `<id>.tar`.
var bundleName = regexp.MustCompile(`^[0-9a-f]{32}\.tar$`)

// bundleEntry classifies an entry of in/'s or work/'s listing (dir is
// "work"): a regular file named `<id>.tar` is a bundle, and its id is
// returned; anything else has no legal result name and is removed with
// an error-level line and no result (step 9).
func (a *Applier) bundleEntry(dir, name string) (string, bool) {
	path := filepath.Join(a.x(dir), name)
	fi, err := os.Lstat(path)
	if err == nil && fi.Mode().IsRegular() && bundleName.MatchString(name) {
		return strings.TrimSuffix(name, ".tar"), true
	}
	kind := "missing"
	if err == nil {
		kind = fi.Mode().Type().String()
	}
	a.logger.Error("box: removing an entry that is not a bundle; no result", zap.String("entry", proof.Bound(name)), zap.String("type", kind))
	if err := a.removeDurable("entry:remove", path); err != nil {
		a.logger.Error("box: could not remove an entry that is not a bundle", zap.String("entry", proof.Bound(name)), zap.String("error", proof.Bound(err.Error())))
	}
	return "", false
}

// take renames every entry of in/ into work/, whatever it is (step 9),
// removes what is not a bundle, and returns the bundles' ids in the
// order of their markers' `posted` time, unmarked last. The order
// decides only which bundle is tried first.
func (a *Applier) take() []string {
	in, work := a.x("in"), a.x("work")
	entries, err := os.ReadDir(in)
	if err != nil {
		a.logger.Error("box: in/ cannot be listed", zap.String("error", proof.Bound(err.Error())))
		return nil
	}
	var ids []string
	for _, e := range entries {
		name := e.Name()
		if err := a.takeOne(in, work, name); err != nil {
			// It must leave in/ all the same (I2): removed where it
			// stands, with a failed result if it was named as a bundle.
			a.logger.Error("box: could not take an entry", zap.String("entry", proof.Bound(name)), zap.String("error", proof.Bound(err.Error())))
			if rerr := a.removeDurable("entry:remove", filepath.Join(in, name)); rerr != nil {
				a.logger.Error("box: could not remove an entry from in/", zap.String("entry", proof.Bound(name)), zap.String("error", proof.Bound(rerr.Error())))
			}
			if bundleName.MatchString(name) {
				id := strings.TrimSuffix(name, ".tar")
				a.finish(&txn{rec: record{ID: id, Origin: originApplier}}, phaseFailed, installFailed(err))
			}
			continue
		}
		if id, ok := a.bundleEntry("work", name); ok {
			ids = append(ids, id)
		}
	}
	posted := make(map[string]time.Time, len(ids))
	for _, id := range ids {
		if m, err := readMarker(filepath.Join(a.x("stage"), id+".auth")); err == nil {
			posted[id] = m.Posted
		}
	}
	sort.SliceStable(ids, func(i, j int) bool {
		pi, iok := posted[ids[i]]
		pj, jok := posted[ids[j]]
		if iok != jok {
			return iok
		}
		return iok && pi.Before(pj)
	})
	return ids
}

// takeOne is one rename from in/ into work/, durable in both directories.
func (a *Applier) takeOne(in, work, name string) error {
	if err := a.fail("take"); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(in, name), filepath.Join(work, name)); err != nil {
		return err
	}
	if err := syncDir(work); err != nil {
		return err
	}
	if err := syncDir(in); err != nil {
		return err
	}
	a.crash("take")
	return nil
}

// process is one bundle through steps 9 to 16 and the transaction. It
// returns errFullDisk or errUnsettled when the run must stop, nil once
// the bundle has its terminal result.
func (a *Applier) process(ctx context.Context, id string) error {
	t := &txn{rec: record{ID: id, Origin: originApplier}, entry: filepath.Join(a.x("work"), id+".tar")}
	// 9. Read once: no symlink followed, no FIFO waited on, a regular
	// file, the body cap plus one byte. Nothing below touches the file.
	data, err := readFile(t.entry, maxBody, false)
	if a.hooks.read != nil {
		a.hooks.read(id)
	}
	var big *tooLargeError
	switch {
	case errors.As(err, &big):
		a.finish(t, phaseRefused, "bundle: larger than 16 MiB")
		return nil
	case err != nil:
		a.finish(t, phaseFailed, installFailed(err))
		return nil
	}
	b, err := proof.ReadBundle(data)
	if err != nil {
		return a.checked(t, asRefusal(err))
	}
	t.rec.Commit, t.rec.Path = b.Commit.ID, b.Path
	base, err := readApplied(a.x(""))
	if errors.Is(err, fs.ErrNotExist) {
		err = errors.New(msgNoBaseline)
	}
	if err != nil {
		a.finish(t, phaseFailed, installFailed(err))
		return nil
	}
	installed, err := readFile(a.caddyfile(), proof.MaxCaddyfile, true)
	if err != nil {
		a.finish(t, phaseFailed, installFailed(err))
		return nil
	}
	// 10 to 15.
	vd, err := check(ctx, a.verifier, b, installed, base)
	if err != nil {
		return a.checked(t, err)
	}
	t.rec.Signer, t.rec.BoxWebhook, t.rec.Apps, t.rec.OutOfBand = vd.signer, vd.host, vd.apps, vd.outOfBand
	t.rec.Prev, t.rec.PrevSHA256, t.rec.NewSHA256 = installed, vd.prevSum, vd.newSum
	// 16. Active? `activating` is waited out; anything else refuses.
	state, err := a.running(ctx)
	if err != nil {
		a.finish(t, phaseFailed, installFailed(err))
		return nil
	}
	switch state {
	case "active":
	case "activating":
		a.finish(t, phaseRefused, msgStillStarting)
		return nil
	default:
		a.finish(t, phaseRefused, msgNotRunning)
		return nil
	}
	t.rec.Diff = caddyfileDiff(installed, b.Caddyfile)
	t.next = b.Caddyfile
	return a.install(ctx, t)
}

// checked ends a bundle that did not pass a check: refused for a verdict
// on the push, failed for the box's own error (checking → failed).
func (a *Applier) checked(t *txn, err error) error {
	var ref *refusal
	if errors.As(err, &ref) {
		a.finish(t, phaseRefused, ref.msg)
		return nil
	}
	a.finish(t, phaseFailed, installFailed(err))
	return nil
}
