package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/smallhoursorg/hotserve/backups/record"
	"github.com/smallhoursorg/hotserve/backups/unit"
)

// The repository's own check: its structure, and one fifty-second of
// its data a week, so that every pack is read once a year. The weekly
// drill makes it after its apps, under the run lock.
//
// restic's exit 1 is both "damaged" and "unreachable" [H], and its
// summary counts an error where it could not open the repository at all
// [M73]. So a probe — `restic cat config`, which answers in a second
// where the storage answers at all [M40, M47] — goes first, under a
// clock of its own, and again after a check that failed: damage is a
// check that said it found some between two probes that were answered.
//
// It takes no lock (the owner, 2026-09-29): a check that dies hard
// leaves an exclusive lock, and restic never passes a stale lock by
// itself, so every backup after it would exit 11 until someone ran
// `restic unlock` [M75]. The cost: a prune run off the box at the same
// moment can make packs vanish under it, and the check say damaged.

// checkGroups is how many groups the data is read in: one a week.
const checkGroups = 52

var (
	// probeClock bounds the probe. restic answers it in a second where
	// the storage answers at all, and retries for a quarter of an hour
	// where it does not, or refuses the key [M15, M47].
	probeClock = 30 * time.Second
	// listClock is the backstop on a listing of the repository — its
	// snapshots, or a snapshot's directory (D4; the owner, 2026-09-29):
	// seventy times the slowest measured, on 28.5 GB and 1,506 snapshots
	// over a remote link [M78], and twice restic's own giving up on a
	// wrong key [M15]. A storage that takes connections and never
	// answers held the run lock, and every hourly run with it, for as
	// long as restic kept trying.
	listClock = 30 * time.Minute
	// checkClock is the clock a drill reads, once, for the group a
	// first check reads and the time the check is recorded at.
	checkClock = time.Now
)

// checkGroup is the group of its data a first check on day t reads: its
// ISO week's, in UTC, whatever zone the box's clock is in.
func checkGroup(t time.Time) string {
	_, week := t.UTC().ISOWeek()
	return fmt.Sprintf("%d/%d", (week-1)%checkGroups+1, checkGroups)
}

// nextGroup is the group a check reads: the one after the last a check
// read, or on day t the ISO week's where none was (the owner,
// 2026-09-30). Which group a pack is in is fixed by its id [M74], so
// fifty-two checks read every pack once, whatever weeks were missed.
func nextGroup(last string, t time.Time) string {
	var n, of int
	if _, err := fmt.Sscanf(last, "%d/%d", &n, &of); err != nil || of != checkGroups || n < 1 || n > checkGroups || fmt.Sprintf("%d/%d", n, of) != last {
		return checkGroup(t)
	}
	return fmt.Sprintf("%d/%d", n%checkGroups+1, checkGroups)
}

// checkArgv is the check of one group: no lock, restic's words as JSON,
// and the temporary cache a check killed hard leaves behind removed once
// it is a month old [M79].
func checkArgv(restic, group string) []string {
	return []string{restic, "check", "--no-lock", "--json", "--cleanup-cache", "--read-data-subset=" + group}
}

// checkRepository checks the repository's structure and one group of
// its data, beginning at, and says what it found, or nothing where it
// was interrupted: an interrupt is no verdict.
func (x *run) checkRepository(ctx context.Context, group string, at time.Time) *record.Check {
	c := &record.Check{Time: at, Group: group}
	verdict := func(class record.CheckClass, detail string) *record.Check {
		if ctx.Err() != nil {
			return nil
		}
		c.Class, c.Detail = class, record.Text(detail)
		return c
	}
	if class, detail := x.probe(ctx); class != "" {
		return verdict(class, detail)
	}
	name := x.name("repocheck", "")
	out := filepath.Join(x.dir, "repocheck.json")
	o, err := x.start(ctx, resticUnit(name, "hotserve backup: check the repository, and data group "+c.Group, x.cfg.EnvFile, checkArgv(x.cfg.Restic, c.Group), out))
	if err != nil {
		return verdict(record.CheckFailed, "the check's unit: "+err.Error())
	}
	var said struct {
		NumErrors   *int     `json:"num_errors"`
		BrokenPacks []string `json:"broken_packs"`
	}
	counted := jsonLine(out, "summary", &said) && said.NumErrors != nil
	switch {
	case o.OK() && counted && *said.NumErrors == 0:
		return verdict(record.CheckClean, "")
	case o.OK():
		return verdict(record.CheckFailed, "restic check exited 0, and its summary could not be read, or counted errors: not believed")
	case o.Result != "exit-code" || o.ExitStatus != 1:
		class, detail := checkClass(o)
		return verdict(class, detail)
	}
	// Exit 1: damage found, or the storage lost half way. The probe
	// after says which.
	if class, detail := x.probe(ctx); class != "" {
		return verdict(class, "the check could not finish: "+detail)
	}
	if !counted || *said.NumErrors == 0 {
		return verdict(record.CheckFailed, fmt.Sprintf("restic check failed (exit 1) without counting any damage, the repository answering before and after; `journalctl -u %s` has restic's own words", name))
	}
	found := fmt.Sprintf("restic check found %s", plural(*said.NumErrors, "error"))
	if n := len(said.BrokenPacks); n > 0 {
		found += fmt.Sprintf(", in %s", plural(n, "damaged pack"))
	}
	// Still damage — a check never masks it — and what else looks the
	// same: a prune off the box makes packs vanish under a check that
	// takes no lock. The next check reads this group again, and settles
	// it. Short enough, whatever the counts, that record.Text never cuts
	// it.
	return verdict(record.CheckDamaged, fmt.Sprintf("%s; a prune run off the box during the check looks the same, and the next check reads this group again; restic's own words: `journalctl -u %s`", found, name))
}

// probe asks the repository for its config, under probeClock: nothing
// where it answered, else the verdict and why.
func (x *run) probe(ctx context.Context) (record.CheckClass, string) {
	// Its answer, the repository's config decrypted, into root's run
	// directory: never the journal.
	o, err := x.startWithin(ctx, probeClock, resticUnit(x.name("probe", ""), "hotserve backup: whether the repository answers, and the password opens it", x.cfg.EnvFile,
		[]string{x.cfg.Restic, "cat", "config", "--no-lock"}, filepath.Join(x.dir, "probe.json")))
	switch {
	case errors.Is(err, errDidNotAnswer):
		return record.CheckUnreachable, err.Error() + ": the storage could not be reached, or refused the key"
	case err != nil:
		return record.CheckFailed, "the probe's unit: " + err.Error()
	case o.OK():
		return "", ""
	}
	return checkClass(o)
}

// checkClass is how a probe or a check ended, as a verdict.
func checkClass(o unit.Outcome) (record.CheckClass, string) {
	detail, _ := resticFailure(o)
	if o.Result != "exit-code" {
		return record.CheckFailed, detail
	}
	switch o.ExitStatus {
	case 1:
		return record.CheckUnreachable, detail
	case 10:
		return record.CheckNoRepository, detail
	case 11:
		// Not resticFailure's words: nothing here waited for a lock.
		return record.CheckFailed, "restic said the repository is locked (exit 11), which a check that takes no lock does not ask"
	case 12:
		return record.CheckWrongPassword, detail
	}
	return record.CheckFailed, detail
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

// readClock is the backstop a read of the repository is given: listClock
// where nobody is there to stop it — a run, a drill, which hold the run
// lock unwatched — and none in a restore (the owner, 2026-09-30), which
// someone started and can stop, and whose listings, a rebuilt box's
// from an empty cache, are the longest a box makes.
func (x *run) readClock() time.Duration {
	if x.attended {
		return 0
	}
	return listClock
}

// didNotAnswer is whether err is a unit given up at its clock — stopped,
// or not confirmed gone — rather than an interrupt: the repository not
// answering, which it will not do for the next app either.
func didNotAnswer(ctx context.Context, err error) bool {
	return errors.Is(err, errDidNotAnswer) || ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded)
}

// startWithin is start under a clock of its own, or none where within is
// 0. At the clock the unit is stopped, and the error says it did not
// answer; an interrupt is the caller's context's, and is returned as it
// came.
func (x *run) startWithin(ctx context.Context, within time.Duration, s unit.Spec) (unit.Outcome, error) {
	if within <= 0 {
		return x.start(ctx, s)
	}
	clock, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	o, err := x.start(clock, s)
	if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, unit.ErrNotConfirmedGone) {
		return o, fmt.Errorf("%w within %s; the unit was stopped", errDidNotAnswer, within)
	}
	return o, err
}

// vouchArgv writes the record that a run ended ok on snapshot id of app,
// made at: a snapshot of its own [M76], holding the id — one of nothing
// is restic's exit 3 — tagged with it, in a (host, paths) group of its
// own, which no app's history lists; and at the snapshot's own time, to
// the second — restic reads --time in the zone it runs in, so the unit
// is given UTC — so that a forget policy that keeps a snapshot for a
// period keeps the record that falls in the same one.
func vouchArgv(restic, app, id string, at time.Time) []string {
	return []string{restic, "backup", "--quiet", "--json", "--retry-lock", vouchRetryLock, "--host", "hotserve",
		"--time", at.UTC().Format(time.DateTime),
		"--tag", cleanTag, "--tag", vouchesTag + id,
		"--stdin-from-command", "--stdin-filename", "hotserve-clean-" + app, "--", "/usr/bin/echo", id}
}

// vouchRetryLock is how long a record waits for a lock something else
// holds: less than its clock (listClock), so that a lock is said as one,
// not as a storage that did not answer.
const vouchRetryLock = "20m"

// The tags of a clean-run record.
const (
	cleanTag   = "hotserve-clean"
	vouchesTag = "vouches:"
)
