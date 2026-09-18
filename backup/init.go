package backup

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
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
	// User is who the per-app jobs run as. It matters for a
	// repository on this box: init runs as root, so everything restic
	// creates here would be root's, and the jobs could not read it.
	User  string
	Force bool
}

// DeleteProbeTag marks the snapshot init makes to find out whether
// these credentials can delete. It is left behind when they cannot —
// which is the good case — so the tag says what it is.
const DeleteProbeTag = "hotserve-delete-probe"

// Init writes the environment file, makes sure the repository exists,
// and then answers the question that decides whether these backups
// survive someone taking the box: can this key delete?
//
// The check is a real deletion attempt, not a reading of the
// configuration: a snapshot of one temporary file is taken and then
// `restic forget` is asked to remove it. A key that cannot delete
// fails that call, which is the outcome to want — the probe snapshot
// stays in the repository as evidence, costing a few hundred bytes.
// A key that can delete gets a warning naming what it means, because
// the box's credentials being able to erase the backups is the
// difference between a backup and a hostage.
func Init(ctx context.Context, o InitOptions, run Runner, capture Capturer, log io.Writer) error {
	if o.Repository == "" {
		return fmt.Errorf("a repository is required, e.g. `hotserve backup init s3:s3.us-west-004.backblazeb2.com/my-bucket` (or a path, for a disk you mount)")
	}
	if _, err := RepositoryPath(o.Repository); err != nil {
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
	env := EnvFor(o.Repository, password, o.Extra)
	run = withEnv(run, env)
	capture = withCaptureEnv(capture, env)

	// Three cases, and restic is what tells them apart: the repository
	// opens (use it), it does not exist (create it), or it exists and
	// this password cannot open it (say so, rather than leaving the
	// operator with a box that backs up into a repository they cannot
	// read).
	if _, err := capture(ctx, "restic", "cat", "config"); err != nil {
		say(log, "repository not found; creating it")
		if err := run(ctx, "restic", "init"); err != nil {
			return fmt.Errorf("cannot open %s with this password, and cannot create it either: if the repository already exists, the password is wrong — pass --password-file with the one you saved; if it does not, check the URL and the storage credentials (%w)", o.Repository, err)
		}
	} else if generated {
		// It opened with a password this command invented, which
		// means... it cannot have: restic would have refused. Reaching
		// here means the repository is readable without the password
		// we generated, so writing that password down would leave the
		// box unable to read what it just backed up.
		return fmt.Errorf("%s already exists, and a new password was generated for it — pass --password-file with the password you saved when you first set it up, so this box uses the same one", o.Repository)
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
			return fmt.Errorf("%w\n\n%s is unchanged: the box still backs up to the repository it was already using", err, o.EnvFile)
		}
		if generated {
			return fmt.Errorf("%w\n\n%s was not written, so nothing is scheduled. Keep the password printed above and pass --password-file when you run init again", err, o.EnvFile)
		}
		return fmt.Errorf("%w\n\n%s was not written, so nothing is scheduled", err, o.EnvFile)
	}
	// A repository on this box was just created by root; the jobs run
	// as someone else and would find it unreadable ("open …/keys:
	// permission denied"). Handing it over here is the difference
	// between backups working and every hourly run failing.
	path, err := RepositoryPath(o.Repository)
	if err != nil {
		return err
	}
	if path != "" && o.User != "" {
		if err := chownTree(path, o.User); err != nil {
			return err
		}
		say(log, "repository is on this box, so it now belongs to %s (the user the jobs run as)", o.User)
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
// snapshots one temporary file and tries to forget it: success means
// the key deletes, failure means it does not.
//
// The snapshot is forgotten by id, never by tag alone: `restic forget
// --tag x` without a retention policy refuses with "no policy was
// specified, no snapshots will be removed", which would look exactly
// like a storage that denied the delete — a check that always
// reported "you are safe", whatever the credentials could do.
func probeDelete(ctx context.Context, run Runner, capture Capturer) (probeResult, error) {
	dir, err := os.MkdirTemp("", "hotserve-probe")
	if err != nil {
		return probeUnknown, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.WriteFile(filepath.Join(dir, "probe"), []byte("hotserve delete probe\n"), 0o600); err != nil {
		return probeUnknown, err
	}
	if err := run(ctx, "restic", "backup", "--quiet", "--tag", DeleteProbeTag, dir); err != nil {
		return probeUnknown, fmt.Errorf("writing a probe snapshot: %w (the repository is not writable with these credentials)", err)
	}
	id, err := newestSnapshotID(ctx, capture, DeleteProbeTag)
	if err != nil {
		return probeUnknown, err
	}
	// A forget that fails is the good outcome — but only when the
	// storage is what refused it. A network blip, a stale lock or a
	// corrupt repository would otherwise be reported as "your backups
	// cannot be deleted", which is the one thing a security check must
	// never say on no evidence.
	out, err := capture(ctx, "restic", "forget", "--quiet", id)
	if err == nil {
		return probeDeletable, nil
	}
	if deniedBy(string(out) + " " + err.Error()) {
		return probeRefused, nil
	}
	return probeUnknown, nil
}

// probeResult is what the delete check learned.
type probeResult int

const (
	probeDeletable probeResult = iota
	probeRefused
	probeUnknown
)

// deniedBy recognises a storage saying no. The wording comes from the
// backends: S3-compatible stores answer AccessDenied or 403, B2 says
// unauthorized, a filesystem repository gives EACCES.
func deniedBy(text string) bool {
	t := strings.ToLower(text)
	for _, s := range []string{
		"accessdenied", "access denied", "403", "forbidden",
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

func writeEnvFile(path, repo, password string, extra []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# hotserve backups: read by systemd as root, never by an app.\n")
	b.WriteString("# `hotserve backup init` wrote this file; keep the password safe elsewhere too.\n")
	b.WriteString("RESTIC_REPOSITORY=" + repo + "\n")
	b.WriteString("RESTIC_PASSWORD=" + password + "\n")
	// Written in the order they were given, deliberately unsorted:
	// systemd takes the last assignment of a key, and the same order is
	// what `init` just probed the repository with. Sorting could put a
	// different value last here than the one that was tested, so the
	// hourly jobs would use credentials no one checked.
	for _, kv := range extra {
		b.WriteString(kv + "\n")
	}
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
	if _, err := io.WriteString(tmp, b.String()); err != nil {
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

// chownTree gives a whole directory to the user the jobs run as.
// Symlinks are changed, never followed: a repository directory holds
// only restic's own files, and following a link out of it is how a
// chown turns into a way to take ownership of something else.
func chownTree(root, username string) error {
	u, err := user.Lookup(username)
	if err != nil {
		return fmt.Errorf("looking up the %s user (the jobs run as it): %w", username, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return fmt.Errorf("user %s has a non-numeric uid %q", username, u.Uid)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return fmt.Errorf("user %s has a non-numeric gid %q", username, u.Gid)
	}
	return filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := os.Lchown(path, uid, gid); err != nil { //nolint:gosec // G122: Lchown is the symlink-safe half of this walk, which is the point made above
			return fmt.Errorf("giving %s to %s: %w", path, username, err)
		}
		return nil
	})
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
