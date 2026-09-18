package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Runner executes an external command. The two the job needs are
// sqlite3 and restic, both from the distribution; injectable so the
// tests can assert exactly what would be run without either binary.
type Runner func(ctx context.Context, name string, args ...string) error

// sqliteBusyTimeoutMS is how long the copy waits for an app that is
// mid-write. Ten seconds is far longer than a web request's
// transaction and far shorter than the hour until the next attempt.
const sqliteBusyTimeoutMS = "10000"

// Job is one app's backup: take a consistent copy of each declared
// database into staging, then hand restic the staging dir and the
// declared file paths in one snapshot.
//
// It runs inside a sandbox that can see only this app's shared dir
// (read-only) and this app's staging dir, so every path it is given
// is absolute and nothing is discovered at run time.
type Job struct {
	App     string
	Shared  string
	Staging string
	// Databases and Files are paths relative to Shared, as declared.
	Databases []string
	Files     []string

	Run Runner
	Log io.Writer
	Env func(string) string
}

// Execute stages the databases and runs restic. It deliberately does
// not apply a retention policy: `restic forget` deletes from the
// repository, and the key on the box should not be able to — so
// retention is run off the box, with a privileged key, as
// docs/backups.md describes. There is no command here that prunes.
func (j Job) Execute(ctx context.Context) error {
	env := j.Env
	if env == nil {
		env = lookupEnv
	}
	if err := requireResticEnv(env); err != nil {
		return err
	}
	if len(j.Databases) == 0 && len(j.Files) == 0 {
		return fmt.Errorf("app %s declares no state to back up", j.App)
	}
	targets := []string{}
	if len(j.Databases) == 0 {
		// No database declared any more: the staged copy of one that
		// used to be is plaintext app data, and nothing would ever
		// remove it. stageDatabases clears this itself when there is
		// something to stage.
		if err := os.RemoveAll(StagingData(j.Staging)); err != nil {
			return fmt.Errorf("app %s: clearing staged copies: %w", j.App, err)
		}
	}
	if len(j.Databases) > 0 {
		if err := j.stageDatabases(ctx); err != nil {
			return err
		}
		// Only the copies, never the whole staging dir: restic's own
		// cache sits beside them and must not be backed up.
		targets = append(targets, StagingData(j.Staging))
	}
	for _, rel := range j.Files {
		p, err := sharedPath(j.Shared, rel)
		if err != nil {
			return fmt.Errorf("app %s: %w", j.App, err)
		}
		// A declared path the app has not created yet (a new app's
		// uploads/) is not a failure: restic would exit non-zero on a
		// missing target, and an app is allowed to declare where its
		// data will go before it puts anything there.
		//
		// Lstat, not Stat, so the two cases below can be told apart at
		// all: Stat answers about a symlink's target, and a link whose
		// target is missing would be reported as "not created yet".
		info, err := os.Lstat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			j.logf("%s: %s does not exist yet, skipping", j.App, rel)
			continue
		case err != nil:
			return fmt.Errorf("app %s: %s: %w", j.App, rel, err)
		case info.Mode()&fs.ModeSymlink != 0:
			// restic stores a symlink as a symlink — it has no option to
			// follow one (0.18) — so a declared path that is a link to a
			// data disk would put the link in the snapshot and none of
			// the data, hourly, while every run reported success. That
			// is the one outcome a backup must never produce quietly.
			return fmt.Errorf("app %s: state files %s is a symlink to %s: restic would store the link and none of the data, so this would report success and back up nothing — declare a path the app's data really lives at, or mount the disk at %s",
				j.App, rel, linkTarget(p), p)
		}
		targets = append(targets, p)
	}

	// Everything declared is still to come: a files-only app whose
	// directories the app has not created yet leaves nothing to hand
	// restic, and restic with no target exits with an argument error.
	// Saying so and succeeding is the same answer the skip above gives.
	if len(targets) == 0 {
		j.logf("%s: nothing to back up yet — every declared path is still to be created", j.App)
		return nil
	}

	args := ResticBackupArgs(j.App, targets)
	j.logf("+ restic %s", quoteArgs(args))
	if err := j.Run(ctx, "restic", args...); err != nil {
		return fmt.Errorf("app %s: restic backup: %w", j.App, err)
	}
	// A snapshot existing is not the same as a backup succeeding:
	// restic writes one and still exits non-zero when it could not
	// read some of the sources. Freshness is therefore measured from
	// this marker, written only after a clean exit, rather than from
	// the newest snapshot — otherwise repeated partial backups would
	// keep `status --check` green while every run was failing.
	if err := os.WriteFile(SuccessMarker(j.Staging), []byte(""), 0o600); err != nil {
		return fmt.Errorf("app %s: recording the backup as complete: %w", j.App, err)
	}
	return nil
}

// stageDatabases copies each declared database into the staging dir,
// keeping the layout it has under shared/ (so `data/sessions.db`
// stays under `data/`), which is what makes a restore a copy back
// rather than a puzzle.
func (j Job) stageDatabases(ctx context.Context) error {
	// Everything restic is about to be handed comes from this run.
	// Without the clear-out, a database whose `state` line was
	// removed (or renamed) leaves its last copy here for ever, and
	// every hourly snapshot keeps carrying it — growing staler, and
	// landing beside the live database in a restore.
	if err := os.RemoveAll(StagingData(j.Staging)); err != nil {
		return fmt.Errorf("app %s: clearing staged copies: %w", j.App, err)
	}
	if err := os.MkdirAll(StagingData(j.Staging), 0o750); err != nil {
		return fmt.Errorf("app %s: staging dir: %w", j.App, err)
	}
	for _, rel := range j.Databases {
		src, err := sharedPath(j.Shared, rel)
		if err != nil {
			return fmt.Errorf("app %s: %w", j.App, err)
		}
		// Unlike a directory an app fills later, a declared database
		// that is not there is almost always a typo'd path — and a
		// backup that quietly holds no database is discovered at the
		// worst possible moment.
		if _, err := os.Stat(src); errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("app %s: no database at %s (declared as `state sqlite %s`); check the path, or remove the line if the app no longer keeps one", j.App, src, rel)
		}
		dst := filepath.Join(StagingData(j.Staging), filepath.Clean(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return fmt.Errorf("app %s: staging dir for %s: %w", j.App, rel, err)
		}
		// (The whole dir was cleared above, so VACUUM INTO — which
		// refuses to overwrite an existing file — has somewhere to
		// write.)
		// .timeout before the copy: sqlite3 defaults to failing the
		// moment the database is busy, and an app mid-transaction —
		// certain in rollback-journal mode, likely in WAL — would
		// otherwise turn one unlucky second into a failed backup.
		args := []string{"-cmd", ".timeout " + sqliteBusyTimeoutMS, sqliteURI(src), vacuumInto(dst)}
		j.logf("+ sqlite3 %s", quoteArgs(args))
		if err := j.Run(ctx, "sqlite3", args...); err != nil {
			return fmt.Errorf("app %s: copying %s: %w", j.App, rel, err)
		}
	}
	return nil
}

// ResticBackupArgs is the whole restic invocation, as its own
// function so the tests pin the argv (tags included: they are what
// `hotserve backup status` and a per-app restore select on).
func ResticBackupArgs(app string, targets []string) []string {
	args := []string{"backup", "--quiet", "--tag", "hotserve", "--tag", "app:" + app}
	return append(args, targets...)
}

// linkTarget names what a symlink points at, for the error that
// refuses one. Unreadable is not worth its own message: the refusal is
// about the link, and the target is there to save a `ls -l`.
func linkTarget(path string) string {
	target, err := os.Readlink(path)
	if err != nil {
		return "somewhere else"
	}
	return target
}

func (j Job) logf(format string, a ...any) {
	if j.Log == nil {
		return
	}
	say(j.Log, format, a...)
}

// quoteArgs renders an argv for the log the way a person would retype
// it; only for logging, never for execution (nothing is shelled out
// through a shell).
func quoteArgs(args []string) string {
	out := make([]byte, 0, 64)
	for i, a := range args {
		if i > 0 {
			out = append(out, ' ')
		}
		if needsQuote(a) {
			out = append(out, '\'')
			for _, r := range a {
				if r == '\'' {
					out = append(out, `'\''`...)
					continue
				}
				out = append(out, string(r)...)
			}
			out = append(out, '\'')
			continue
		}
		out = append(out, a...)
	}
	return string(out)
}

func needsQuote(s string) bool {
	if s == "" {
		return true
	}
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			continue
		}
		switch r {
		case '-', '_', '.', '/', ':', '=', '+', ',', '@':
			continue
		}
		return true
	}
	return false
}
