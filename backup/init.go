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
	"sort"
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
	for _, kv := range o.Extra {
		if strings.HasPrefix(kv, "RESTIC_REPOSITORY_FILE=") {
			return fmt.Errorf("RESTIC_REPOSITORY_FILE is not supported: the backup jobs run in a sandbox that cannot see it — pass the repository itself instead")
		}
	}
	if _, err := os.Stat(o.EnvFile); err == nil && !o.Force {
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
		fmt.Fprintln(log, "repository not found; creating it")
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
	fmt.Fprintln(log, "repository ready")

	if err := writeEnvFile(o.EnvFile, o.Repository, password, o.Extra); err != nil {
		return err
	}
	fmt.Fprintf(log, "wrote %s (root only)\n", o.EnvFile)
	if generated {
		fmt.Fprintf(log, "\nrepository password — save this somewhere safe now, nothing else has a copy:\n\n    %s\n\nWithout it the backups cannot be read, and no support can recover them.\n\n", password)
	}

	deletion, err := probeDelete(ctx, run, capture)
	if err != nil {
		return err
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
		fmt.Fprintf(log, "repository is on this box, so it now belongs to %s (the user the jobs run as)\n", o.User)
	}
	switch deletion {
	case probeDeletable:
		fmt.Fprintln(log, "\nWARNING: these credentials can delete backups.")
		fmt.Fprintln(log, "Anyone who takes this box can erase every backup it made. Give the box a")
		fmt.Fprintln(log, "key that can write but not delete, and keep a privileged key elsewhere for")
		fmt.Fprintln(log, "pruning — docs/backups.md, \"Keep the box unable to delete\".")
	case probeRefused:
		fmt.Fprintln(log, "delete refused by the storage: the box can add backups but not remove them")
	default:
		fmt.Fprintln(log, "\nThe delete check could not finish: removing its probe snapshot failed for a")
		fmt.Fprintln(log, "reason that was not a refusal by the storage (a network error, a stale lock).")
		fmt.Fprintln(log, "Whether these credentials can erase your backups is unknown — run")
		fmt.Fprintln(log, "`hotserve backup init` again when the repository is reachable.")
	}
	fmt.Fprintln(log, "\nBackups run hourly (hotserve-backup.timer). Declare what to keep with")
	fmt.Fprintln(log, "`state` lines in each app's block, then check with `hotserve backup status`.")
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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# hotserve backups: read by systemd as root, never by an app.\n")
	b.WriteString("# `hotserve backup init` wrote this file; keep the password safe elsewhere too.\n")
	b.WriteString("RESTIC_REPOSITORY=" + repo + "\n")
	b.WriteString("RESTIC_PASSWORD=" + password + "\n")
	sorted := append([]string(nil), extra...)
	sort.Strings(sorted)
	for _, kv := range sorted {
		b.WriteString(kv + "\n")
	}
	// 0600 before anything is written to it: the password must never
	// exist on disk in a world-readable file, not even briefly.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	defer f.Close()
	if _, err := io.WriteString(f, b.String()); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return f.Chmod(0o600)
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
		if err := os.Lchown(path, uid, gid); err != nil {
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
