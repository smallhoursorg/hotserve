package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// InitOptions is one `hotserve backup init`.
type InitOptions struct {
	Repository string
	EnvFile    string
	Password   string
	// PasswordFile holds the password of a repository that already
	// exists — how a rebuilt box reaches the backups it is recovering
	// from, since sudo clears the environment by default.
	PasswordFile string
	// Extra are provider settings given as KEY=VALUE (access keys and
	// the like). They go in the env file beside the password, because
	// the same root-only file is the one thing systemd hands the jobs.
	Extra []string
	// User is who the per-app jobs run as. init's checks run as the
	// job does, as this user.
	User  string
	Force bool
	// AskPassword, when set, is asked for the password of a repository
	// that turns out to exist already — a rebuilt box, at a terminal —
	// instead of refusing. nil anywhere nobody can answer.
	AskPassword func() (string, error)
}

// asJobHint explains the one thing that surprises an operator whose
// credentials work in their own shell: these checks do not use their
// shell. They run as the hourly job does — as the backup user, in its
// sandbox, with the settings being written and nothing else — so
// anything the repository needs has to be among those settings.
const asJobHint = "These checks ran the way the hourly backup will: as the backup user, inside its sandbox, with only the settings being written. Anything that lives in your own shell (AWS_PROFILE, a proxy, a credentials file under your home) was not used — pass what the repository needs to init as KEY=VALUE, or in --credentials-file."

// DeleteProbeTag marks the snapshot init makes to find out whether
// these credentials can delete. It is left behind when they cannot —
// which is the good case — so the tag says what it is.
const DeleteProbeTag = "hotserve-delete-probe"

// Init writes the environment file, makes sure the repository exists,
// and then answers the question that decides whether these backups
// survive someone taking the box: can this key delete?
//
// The check is a real deletion attempt, not a reading of the
// configuration: a one-line snapshot is taken, `restic forget` is
// asked to remove it, and then the repository is looked at. With a key
// that cannot delete, the snapshot is still there — the outcome to
// want — and stays as evidence, costing a few hundred bytes.
// A key that can delete gets a warning naming what it means, because
// the box's credentials being able to erase the backups is the
// difference between a backup and a hostage.
func Init(ctx context.Context, o InitOptions, run Runner, capture Capturer, log io.Writer) (err error) {
	if o.Repository == "" {
		return fmt.Errorf("a repository is required, e.g. `hotserve backup init s3:s3.us-west-004.backblazeb2.com/my-bucket`")
	}
	if err := CheckRepository(o.Repository); err != nil {
		return err
	}
	// The extra settings are provider credentials, not a way to
	// redefine what init is for. systemd takes the LAST assignment in
	// an environment file, so a second RESTIC_PASSWORD would leave
	// the jobs using one password while the operator saves the one
	// printed here — and a second RESTIC_REPOSITORY would send the
	// hourly backups somewhere other than the repository just set up
	// and checked.
	for _, kv := range o.Extra {
		key, _, _ := strings.Cut(kv, "=")
		switch key {
		case "RESTIC_REPOSITORY_FILE":
			return fmt.Errorf("RESTIC_REPOSITORY_FILE is not supported: the backup jobs run in a sandbox that cannot see it — pass the repository itself instead")
		case "RESTIC_REPOSITORY":
			return fmt.Errorf("pass the repository as the argument, not as %s: two of them in the environment file would send the backups somewhere this command never checked", key)
		case "RESTIC_PASSWORD", "RESTIC_PASSWORD_FILE", "RESTIC_PASSWORD_COMMAND":
			return fmt.Errorf("%s cannot be set here: the password this command reports has to be the one the jobs use — pass --password-file to reuse an existing repository's password", key)
		}
	}
	_, statErr := os.Stat(o.EnvFile)
	replacing := statErr == nil
	if replacing && !o.Force {
		return fmt.Errorf("%s already exists: it holds the password for the existing repository, and overwriting it loses access to those backups — pass --force to replace it anyway", o.EnvFile)
	}
	for _, kv := range o.Extra {
		if !strings.Contains(kv, "=") {
			return fmt.Errorf("%q is not a KEY=VALUE setting (provider credentials look like AWS_ACCESS_KEY_ID=…)", kv)
		}
	}
	password, generated, err := resolvePassword(o)
	if err != nil {
		return err
	}
	// Everything that goes into the environment file has to survive
	// systemd's reading of it unchanged, or this command checks one
	// repository while the jobs use another.
	if err := envFileSafe("the repository password", password); err != nil {
		return err
	}
	if err := envFileSafe("the repository", o.Repository); err != nil {
		return err
	}
	for _, kv := range o.Extra {
		key, value, _ := strings.Cut(kv, "=")
		if err := envFileSafe(key, value); err != nil {
			return err
		}
	}

	// Nothing is written until the repository answers. A file written
	// first would survive a failed run — a typo'd bucket, a wrong
	// key — and the retry would then refuse to overwrite it, warning
	// about losing backups that were never made.
	baseRun, baseCapture := run, capture
	env := EnvFor(o.Repository, password, o.Extra)
	run = withEnv(baseRun, env)
	capture = withCaptureEnv(baseCapture, env)

	// Create first, then open — never the other way round. Asked to open
	// a repository in a bucket that does not exist, restic 0.18 treats
	// "The specified bucket does not exist" as transient and retries it
	// for many minutes (measured: still retrying when stopped at eight),
	// so a typo in a bucket name would look like init hanging. `restic
	// init` answers at once either way: it creates the repository (and
	// the bucket, where the key may), or it refuses because one is
	// already there. Only then is `cat config` asked to prove the
	// password — against a bucket now known to exist.
	out, initErr := capture(ctx, "restic", "init")
	switch {
	case initErr == nil:
		say(log, "created a new repository")
	case !alreadyInitialized(string(out)):
		return fmt.Errorf("cannot create a repository at %s: %s\n\nCheck the URL, that the bucket exists, and that the storage key may write to it.\n\n%s", o.Repository, firstLine(out, initErr), asJobHint)
	case generated && o.AskPassword == nil:
		// A password this command invented cannot open a repository
		// that already exists; writing it down would leave the box
		// backing up into something it cannot read.
		return fmt.Errorf("%s already exists, and a new password was generated for it — run init at a terminal to be asked for the password you saved when you first set it up, or pass it with --password-file", o.Repository)
	default:
		if generated {
			// A rebuilt box, at a terminal: the repository is there, so
			// the invented password is dropped and the real one asked
			// for. Nothing was written with the invented one — `restic
			// init` refused before creating anything.
			asked, err := o.AskPassword()
			if err != nil {
				return err
			}
			if err := envFileSafe("the repository password", asked); err != nil {
				return err
			}
			password, generated = asked, false
			env = EnvFor(o.Repository, password, o.Extra)
			run = withEnv(baseRun, env)
			capture = withCaptureEnv(baseCapture, env)
		}
		if out, err := capture(ctx, "restic", "cat", "config"); err != nil {
			return fmt.Errorf("%s already exists, and this password cannot open it (%s) — pass --password-file with the password you saved when you first set it up", o.Repository, firstLine(out, err))
		}
	}
	say(log, "repository ready")

	// The password is reported before anything else can fail, because
	// at this point the repository may already have been created with
	// it: a later error that swallowed it would leave a repository
	// nothing can open.
	if generated {
		say(log, "\nrepository password — save this somewhere safe now, nothing else has a copy:\n\n    %s\n\nWithout it the backups cannot be read, and no support can recover them.\n", password)
	}

	// The probe writes a real snapshot, so it is also the proof that
	// these credentials can back up at all. It runs before the
	// environment file is installed: a read-only key that gets this far
	// would otherwise leave the file behind, arming an hourly job that
	// fails every time, and the retry would refuse to overwrite it
	// without --force.
	deletion, err := probeDelete(ctx, run, capture)
	if err != nil {
		// What the operator has to know is what the box is doing NOW.
		// With --force over a working setup that is "still backing up
		// where it was", which is the opposite of "nothing is
		// scheduled" — and this is the moment they are least able to go
		// and check.
		if replacing {
			return fmt.Errorf("%w\n\n%s is unchanged: the box still backs up to the repository it was already using.\n\n%s", err, o.EnvFile, asJobHint)
		}
		if generated {
			return fmt.Errorf("%w\n\n%s was not written, so nothing is scheduled. Keep the password printed above and pass --password-file when you run init again.\n\n%s", err, o.EnvFile, asJobHint)
		}
		return fmt.Errorf("%w\n\n%s was not written, so nothing is scheduled.\n\n%s", err, o.EnvFile, asJobHint)
	}
	if err := writeEnvFile(o.EnvFile, o.Repository, password, o.Extra); err != nil {
		return err
	}
	say(log, "wrote %s (root only)", o.EnvFile)
	switch deletion {
	case probeDeletable:
		say(log, "\nWARNING: these credentials can delete backups.")
		say(log, "Anyone who takes this box can erase every backup it made. Give the box a")
		say(log, "key that can write but not delete, and keep a privileged key elsewhere for")
		say(log, "pruning — docs/backups.md, \"Keep the box unable to delete\".")
	case probeRefused:
		say(log, "delete refused by the storage: the box can add backups but not remove them")
	default:
		say(log, "\nThe delete check could not finish: removing its probe snapshot failed for a")
		say(log, "reason that was not a refusal by the storage (a network error, a stale lock).")
		say(log, "Whether these credentials can erase your backups is unknown — run")
		say(log, "`hotserve backup init` again when the repository is reachable.")
	}
	say(log, "\nBackups run hourly (hotserve-backup.timer). Declare what to keep with")
	say(log, "`state` lines in each app's block, then check with `hotserve backup status`.")
	return nil
}

// alreadyInitialized recognises restic refusing to init over a
// repository that exists. The wording is restic 0.18.0's own, captured
// from a real run:
//
//	s3: …failed: repository master key and config already initialized
//
// Anything else is a real failure to create, and is reported as one —
// without asking `cat config` afterwards, which is the call that hangs
// on a bucket that does not exist.
func alreadyInitialized(out string) bool {
	return strings.Contains(out, "master key and config already initialized")
}

// firstLine is the part of restic's output worth quoting in an error:
// its first non-empty line, or the exit status when it said nothing.
func firstLine(out []byte, err error) string {
	for line := range strings.SplitSeq(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	if err != nil {
		return err.Error()
	}
	return "no output"
}

// resolvePassword finds the password for this repository, and says
// whether it made one up. A rebuilt box has to reuse the password of
// the repository it is recovering from, and `sudo` clears the
// environment by default — so a file is the way it arrives, not
// RESTIC_PASSWORD.
func resolvePassword(o InitOptions) (string, bool, error) {
	if o.PasswordFile != "" {
		body, err := os.ReadFile(o.PasswordFile)
		if err != nil {
			return "", false, fmt.Errorf("reading the repository password from %s: %w", o.PasswordFile, err)
		}
		pw := strings.TrimRight(string(body), "\r\n")
		if pw == "" {
			return "", false, fmt.Errorf("%s is empty: it should hold the repository's password, on one line", o.PasswordFile)
		}
		return pw, false, nil
	}
	if o.Password != "" {
		return o.Password, false, nil
	}
	pw, err := newPassword()
	return pw, true, err
}

// probeDelete reports whether these credentials can remove data. It
// writes one tiny snapshot, asks restic to forget it, and then looks:
// is the snapshot still there?
//
// The outcome is read from the repository, never from forget's exit
// status. restic exits 0 when the storage refuses — measured against
// an append-only server, restic 0.18 printed
//
//	Remove(<snapshot/…>) failed: unexpected HTTP response (403): 403 Forbidden
//
// and still reported success. Judged by exit status, the key the docs
// tell operators to create would be reported as able to delete, and
// the "refused" answer would never be given at all.
//
// The snapshot is forgotten by id, never by tag alone: `restic forget
// --tag x` without a retention policy refuses with "no policy was
// specified, no snapshots will be removed", which would look exactly
// like a storage that denied the delete.
//
// The snapshot's content comes from a command restic runs itself, so
// no file is written for it: this process runs as root, and anything
// it wrote where the checks can see is somewhere the backup user can
// reach first.
func probeDelete(ctx context.Context, run Runner, capture Capturer) (probeResult, error) {
	if err := run(ctx, "restic", "backup", "--quiet", "--tag", DeleteProbeTag,
		"--stdin-from-command", "--stdin-filename", DeleteProbeTag,
		"--", "echo", "hotserve delete probe"); err != nil {
		return probeUnknown, fmt.Errorf("writing a probe snapshot: %w (the repository is not writable with these credentials)", err)
	}
	id, err := newestSnapshotID(ctx, capture, DeleteProbeTag)
	if err != nil {
		return probeUnknown, err
	}
	// The refusal, when there is one, is on stderr with exit status 0,
	// so stderr is asked for explicitly: it is the evidence.
	var stderr bytes.Buffer
	out, forgetErr := capture(withStderr(ctx, &stderr), "restic", "forget", "--quiet", id)
	still, err := snapshotListed(ctx, capture, DeleteProbeTag, id)
	switch {
	case err != nil:
		// Cannot tell what happened: that is the answer, not a guess.
		return probeUnknown, nil
	case !still:
		return probeDeletable, nil
	}
	// Still there. That is the good outcome — but only when the storage
	// is what refused. A network blip, a stale lock or a corrupt
	// repository would otherwise be reported as "your backups cannot be
	// deleted", which is the one thing a security check must never say
	// on no evidence.
	evidence := string(out) + " " + stderr.String()
	if forgetErr != nil {
		evidence += " " + forgetErr.Error()
	}
	if deniedBy(evidence) {
		return probeRefused, nil
	}
	return probeUnknown, nil
}

// snapshotListed reports whether the snapshot with this short id is
// still in the repository.
func snapshotListed(ctx context.Context, capture Capturer, tag, id string) (bool, error) {
	out, err := capture(ctx, "restic", "snapshots", "--json", "--tag", tag)
	if err != nil {
		return false, err
	}
	var snaps []Snapshot
	if err := json.Unmarshal(out, &snaps); err != nil {
		return false, err
	}
	for _, s := range snaps {
		if s.ShortID == id {
			return true, nil
		}
	}
	return false, nil
}

// probeResult is what the delete check learned.
type probeResult int

const (
	probeDeletable probeResult = iota
	probeRefused
	probeUnknown
)

// deniedBy recognises a storage saying no. The wording comes from the
// backends: S3-compatible stores answer AccessDenied or 403 Forbidden,
// B2 says unauthorized, an sftp server says permission denied.
//
// Words only, never a bare status number. A match here becomes "your
// backups cannot be deleted", so a false one is the worst answer this
// check can give — and "403" turns up in plenty of text that is not a
// refusal: "already locked by PID 403", a duration, a pack id. An HTTP
// 403 from a real store carries "Forbidden" or "AccessDenied" beside
// it; one that does not is reported as unknown, which is honest.
func deniedBy(text string) bool {
	t := strings.ToLower(text)
	for _, s := range []string{
		"accessdenied", "access denied", "forbidden",
		"unauthorized", "not authorized", "permission denied",
		"operation not permitted", "read-only",
	} {
		if strings.Contains(t, s) {
			return true
		}
	}
	return false
}

// newestSnapshotID finds the snapshot just written, so it can be
// deleted by id.
func newestSnapshotID(ctx context.Context, capture Capturer, tag string) (string, error) {
	out, err := capture(ctx, "restic", "snapshots", "--json", "--tag", tag)
	if err != nil {
		return "", fmt.Errorf("listing the probe snapshot: %w", err)
	}
	var snaps []Snapshot
	if err := json.Unmarshal(out, &snaps); err != nil {
		return "", fmt.Errorf("restic returned a snapshot list this version cannot read: %w", err)
	}
	if len(snaps) == 0 {
		return "", fmt.Errorf("the probe snapshot was written but does not appear in the repository — these credentials may be writing somewhere they cannot read back")
	}
	newest := snaps[0]
	for _, s := range snaps[1:] {
		if s.Time.After(newest.Time) {
			newest = s
		}
	}
	return newest.ShortID, nil
}

// renderEnvFile is the one way settings are written for systemd: the
// file the jobs get, and the one init's checks run with. Two writers
// could disagree about a byte, and systemd would then hand the checks
// one repository and the jobs another.
//
// Written in the order given, deliberately unsorted: systemd takes the
// last assignment of a key, so re-ordering could put a different value
// last in the file than the one that was tested.
func renderEnvFile(env []string) string {
	var b strings.Builder
	b.WriteString("# hotserve backups: read by systemd as root, never by an app.\n")
	b.WriteString("# `hotserve backup init` wrote this file; keep the password safe elsewhere too.\n")
	for _, kv := range env {
		b.WriteString(kv + "\n")
	}
	return b.String()
}

func writeEnvFile(path, repo, password string, extra []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	body := renderEnvFile(EnvFor(repo, password, extra))
	// Written to a new 0600 file and renamed over the old one. Opening
	// the existing file instead would keep whatever mode it had while
	// the password went into it — and `--force` replaces a file that
	// may have been made readable by someone else — so the secret
	// never exists at a mode this command did not choose. The rename
	// is also atomic: a reader sees the old settings or the new ones,
	// never half a file.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".backup.env-*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name()) // no-op once the rename has happened
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if _, err := io.WriteString(tmp, body); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// envFileSafe refuses a value systemd would read differently from the
// way it was given. EnvironmentFile= strips surrounding quotes,
// processes backslashes and ends a value at a newline, so a password
// of 'secret' would be used literally here and as secret by the jobs,
// and one holding a newline could append a second RESTIC_REPOSITORY
// line of the attacker's choosing. Refusing is better than guessing:
// the alternative is a box that initializes happily and backs up
// somewhere else every hour.
func envFileSafe(key, value string) error {
	if strings.ContainsAny(value, "\n\r\x00") {
		return fmt.Errorf("%s must not contain a newline: systemd ends the value there, and the rest would become another setting", key)
	}
	if strings.ContainsAny(value, `"'\`) {
		return fmt.Errorf(`%s must not contain quotes or backslashes: systemd reads them as syntax, so the jobs would use a different value than this command just checked`, key)
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%s must not start or end with whitespace: systemd trims it, so the jobs would use a different value", key)
	}
	return nil
}

// EnvFor is the environment restic needs for this repository, in the
// order the env file writes it.
func EnvFor(repo, password string, extra []string) []string {
	return append([]string{"RESTIC_REPOSITORY=" + repo, "RESTIC_PASSWORD=" + password}, extra...)
}

// newPassword is what protects every backup, so it is long enough
// that no wordlist reaches it: 256 bits, printable.
func newPassword() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating a repository password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
