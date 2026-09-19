package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

// A run's read-back lists its snapshot: it says the snapshot names what
// was declared, and never that what the snapshot points at is still in
// the repository, or still readable. A prune that was cut off, a bucket
// lifecycle rule, an object removed in the provider's console — each
// leaves every hourly run green, and is found at the restore. So once a
// week the repository itself is checked (`restic check`): its structure
// whole, every pack a snapshot needs there, and one fifty-second of the
// data read back and verified, a different part each week, so that every
// byte is read once a year for about 2% of the repository's size in
// downloads a week.
//
// What it found is a record in the repository, like a clean run's
// (CleanTag): `status` reads it from there, on this box or a rebuilt one.
const (
	// VerifiedTag marks the record of a check that found nothing wrong,
	// VerifyFailedTag of one that did not pass on a repository that was
	// there to be checked. Neither
	// is tagged `hotserve`, so neither is any app's snapshot.
	VerifiedTag     = "hotserve-verified"
	VerifyFailedTag = "hotserve-verify-failed"

	// verifyParts is how many weekly parts the data is read back in.
	verifyParts = 52

	// VerifyOverdue is when a repository has gone unchecked long enough
	// to say so: two missed weeks, as StaleAfter is two missed hours.
	VerifyOverdue = 15 * 24 * time.Hour

	// verifyHome is the directory the check's restic may write: its
	// cache, kept from one week to the next. Beside the staging root, not
	// in it: an app may be named anything.
	verifyHome = "/var/lib/hotserve-backup-verify"
)

// verifySubset is the part of the data this week's check reads back:
// restic's n/t, n by the ISO week so that a year's checks cover all of it
// (week 53, when there is one, reads the first part again).
func verifySubset(now time.Time) string {
	_, week := now.ISOWeek()
	return fmt.Sprintf("%d/%d", (week-1)%verifyParts+1, verifyParts)
}

// VerifyArgs is the weekly check.
func VerifyArgs(now time.Time) []string {
	return []string{"check", "--read-data-subset=" + verifySubset(now)}
}

// verifyRecordArgs writes the record of a check: an append, which a key
// that cannot delete can make, with the part that was read as its
// content.
func verifyRecordArgs(tag, subset string) []string {
	return []string{"backup", "--quiet", "--tag", tag,
		"--stdin-from-command", "--stdin-filename", "hotserve-verify",
		"--", "echo", subset}
}

// Verify checks the repository and records what it found.
//
// restic's exit status decides, and `check` exits 1 over damage it found
// — and over a repository it could not reach at all, which is restic's 1
// for anything that went wrong (measured: a host that does not resolve).
// A check that did not happen says nothing about the repository, so the
// two are told apart by asking the repository something else, before and
// after: only a repository that opens both times, around a check that
// exited 1, is recorded as not having passed. Everything else — it does
// not open, the lock was not had in time — is recorded as nothing, and
// fails this command, so its unit says so and the next week tries again.
func Verify(ctx context.Context, x Exec, now time.Time, log io.Writer) error {
	opens := func() error { return x(ctx, restic("cat", "config", "--no-lock")) }
	notChecked := func(err error) error {
		return fmt.Errorf("the repository could not be checked (it did not open, or a backup held its lock for longer than %s): nothing is recorded, and the next weekly check tries again: %w", lockWait, err)
	}
	if err := opens(); err != nil {
		return notChecked(err)
	}
	args := VerifyArgs(now)
	say(log, "+ restic %s", quoteArgs(args))
	c := restic(args...)
	c.Stdout, c.Stderr = log, log
	err := x(ctx, c)
	switch {
	case err == nil:
		if rerr := x(ctx, restic(verifyRecordArgs(VerifiedTag, verifySubset(now))...)); rerr != nil {
			return fmt.Errorf("the repository checked out, but recording that in it failed, so `status` will not know: %w", rerr)
		}
		say(log, "repository verified: its structure, and part %s of its data read back", verifySubset(now))
		return nil
	case exitStatus(err) == 1 && opens() == nil:
		// Best effort: a repository damaged enough may not take the
		// record either, and the failure below is said regardless.
		_ = x(ctx, restic(verifyRecordArgs(VerifyFailedTag, verifySubset(now))...))
		return fmt.Errorf("restic check did not pass (above), on a repository that opened before it and after it: something in it is missing or does not read back. Backups still run, but a restore may not find what it needs. `sudo hotserve backup restic -- check` repeats it, and restic's documentation on repairing a repository is where to go next: %w", err)
	default:
		return notChecked(err)
	}
}

// Verification is what the repository says about its last check.
type Verification struct {
	At     time.Time // zero when it has never been checked
	Failed bool
}

// Overdue is whether `status --check` should say so: the last check did
// not pass, or there has been none for two weeks. For a repository never
// yet checked the two weeks run from since — its oldest backup — so a new
// one is given the time its first weekly check takes to come round, and
// one whose checks never happen (the timer masked, every check unable to
// reach it) does not stay green for ever on having no record at all.
func (v Verification) Overdue(now, since time.Time) bool {
	switch {
	case v.Failed:
		return true
	case !v.At.IsZero():
		return now.Sub(v.At) > VerifyOverdue
	default:
		return !since.IsZero() && now.Sub(since) > VerifyOverdue
	}
}

// LastVerification reads the newest check record, of either kind.
func LastVerification(ctx context.Context, x Exec) (Verification, error) {
	out, err := x.output(ctx, restic("snapshots", "--no-lock", "--json", "--tag", VerifiedTag, "--tag", VerifyFailedTag))
	if err != nil {
		return Verification{}, fmt.Errorf("reading the repository's check records: %w", err)
	}
	var listed []Snapshot
	if err := json.Unmarshal(out, &listed); err != nil {
		return Verification{}, fmt.Errorf("restic returned a snapshot list this version cannot read: %w", err)
	}
	var v Verification
	for _, s := range listed {
		if s.Time.After(v.At) {
			v = Verification{At: s.Time, Failed: slices.Contains(s.Tags, VerifyFailedTag)}
		}
	}
	return v, nil
}

// FormatVerification is the report's last line: about the repository,
// where every line above it is about an app.
func FormatVerification(w io.Writer, v Verification, now, since time.Time) {
	switch {
	case v.At.IsZero() && v.Overdue(now, since):
		say(w, "\nrepository: ⚠ never checked, though it has held backups for %s: the weekly check has not run, or has not been able to. What it did:\n    journalctl -u hotserve-backup_verify -n 50\nTo check it now: sudo hotserve backup verify", strings.TrimSuffix(humanAge(now.Sub(since)), " ago"))
	case v.At.IsZero():
		say(w, "\nrepository: not checked yet — a weekly check reads it back (systemctl list-timers hotserve-backup_verify.timer); to check it now: sudo hotserve backup verify")
	case v.Failed:
		say(w, "\nrepository: ⚠ the last check, %s, did not pass: something in it is missing or does not read back. What it found:\n    journalctl -u hotserve-backup_verify -n 50", humanAge(now.Sub(v.At)))
	case v.Overdue(now, since):
		say(w, "\nrepository: ⚠ last checked %s; the weekly check has not finished since. What it did:\n    journalctl -u hotserve-backup_verify -n 50", humanAge(now.Sub(v.At)))
	default:
		say(w, "\nrepository: checked %s, nothing wrong", humanAge(now.Sub(v.At)))
	}
}
