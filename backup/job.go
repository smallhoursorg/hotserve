package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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
	// Capture reads the snapshot back after it is written; see
	// verifySnapshot.
	Capture Capturer
	Log     io.Writer
	Env     func(string) string
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
	// Which declared paths a clean run has backed up before: the only
	// way to tell a path that is not there YET from one that is not
	// there ANY MORE.
	seen := readSeen(SeenPaths(j.Staging))
	var present []string
	for _, declared := range j.Files {
		p, err := sharedPath(j.Shared, declared)
		if err != nil {
			return fmt.Errorf("app %s: %w", j.App, err)
		}
		// Keyed as cleaned, so `uploads/` and `uploads` are one path.
		rel := filepath.Clean(declared)
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
		case errors.Is(err, fs.ErrNotExist) && seen[rel]:
			// It was backed up before, so "not created yet" is not what
			// happened. Skipping it would let every other declared path
			// keep the run green while this one is never backed up
			// again — with only a journal line to say so.
			return fmt.Errorf("app %s: state files %s was backed up before and is not there now — deleted or moved; put it back, or remove the `state files %s` line if the app no longer keeps it", j.App, rel, rel)
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
			return fmt.Errorf("app %s: state files %s is a symlink to %s: restic would store the link and none of the data, so this would report success and back up nothing — declare the path the app's data really lives at; to keep an app's data on another disk, bind-mount that disk at the app's shared dir (liveswap/README.md, \"Sandbox\")",
				j.App, rel, linkTarget(p))
		}
		targets = append(targets, p)
		present = append(present, rel)
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
	// What must be in the snapshot, by declaration: every database as a
	// copy with something in it, every files path as the real thing.
	var want []expectedNode
	for _, rel := range j.Databases {
		want = append(want, expectedNode{path: filepath.Join(StagingData(j.Staging), rel), database: true})
	}
	for _, p := range targets {
		if p != StagingData(j.Staging) {
			want = append(want, expectedNode{path: p})
		}
	}
	if err := j.verifySnapshot(ctx, want); err != nil {
		return fmt.Errorf("app %s: %w", j.App, err)
	}
	// A snapshot existing is not the same as a backup succeeding:
	// restic writes one and still exits non-zero when it could not
	// read some of the sources. Freshness is therefore measured from
	// this marker, written only after a clean exit AND a snapshot that
	// was read back holding what was declared — rather than from the
	// newest snapshot, which repeated partial backups would keep fresh
	// while every run was failing.
	if err := writeSeen(SeenPaths(j.Staging), present); err != nil {
		return fmt.Errorf("app %s: recording which declared paths were backed up: %w", j.App, err)
	}
	if err := replaceFile(SuccessMarker(j.Staging), ""); err != nil {
		return fmt.Errorf("app %s: recording the backup as complete: %w", j.App, err)
	}
	return nil
}

// SeenPaths lists the declared files paths the last clean run backed
// up, one per line, relative to the app's shared dir.
func SeenPaths(appStaging string) string {
	return filepath.Join(appStaging, ".declared-present")
}

// readSeen treats a missing or unreadable list as empty: the first run,
// or the first after an upgrade, cannot know what was there before, so
// it gives every path the benefit of "not created yet" — as all runs
// did before this list existed.
func readSeen(path string) map[string]bool {
	seen := map[string]bool{}
	body, err := os.ReadFile(path) //nolint:gosec // the job's own staging dir, a path built here
	if err != nil {
		return seen
	}
	for line := range strings.SplitSeq(string(body), "\n") {
		if line != "" {
			seen[line] = true
		}
	}
	return seen
}

// writeSeen replaces the list with exactly what this run backed up. A
// declaration that was removed drops out with it, and one that went
// missing never gets here: the run fails first.
func writeSeen(path string, present []string) error {
	var b strings.Builder
	for _, rel := range present {
		b.WriteString(rel + "\n")
	}
	return replaceFile(path, b.String())
}

// replaceFile writes a new file beside path and renames it over path.
// The job's staging dir is the job user's own, so a rename replaces
// the old file whoever owns it — which a plain write does not: after
// `hotserve backup app` has been run by hand as root, a write would
// fail on the root-owned file every hour from then on, and the app
// would never count as backed up again.
func replaceFile(path, body string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := io.WriteString(tmp, body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
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

// expectedNode is one thing the snapshot has to hold.
type expectedNode struct {
	path     string
	database bool
}

// lsNode is the part of `restic ls --json` this reads.
type lsNode struct {
	StructType string `json:"struct_type"`
	Path       string `json:"path"`
	Type       string `json:"type"`
	Size       uint64 `json:"size"`
}

// verifySnapshot reads the snapshot just written back out of the
// repository and requires every declared path to be in it as data.
//
// Every check before this one is about the job's own view: the file
// existed, the copy ran, restic exited 0. None of them is the backup.
// This package has shipped, and then found, several ways for all of
// those to be true while the snapshot held nothing worth restoring — a
// symlink stored as a link, a database copy that came out empty. They
// were each found by someone imagining them. Reading the snapshot back
// catches the next one without anyone having to: a declared path that
// is missing, is a link, or (for a database) is empty fails the run,
// so it never earns the success marker.
//
// Two cheap calls: the newest snapshot for this app from this box,
// then `ls` of each declared path's PARENT. restic's ls lists a named
// directory's direct children, so naming an uploads dir itself would
// return one line per upload — a hundred thousand of them, held in
// memory inside a job throttled at 64 MB. In the snapshot, a declared
// path's parent holds only what was backed up from it, so its listing
// grows with the number of declarations and nothing else.
func (j Job) verifySnapshot(ctx context.Context, want []expectedNode) error {
	if j.Capture == nil {
		return errors.New("cannot read the snapshot back: no way to capture restic's output was given")
	}
	// This box's snapshots only: restic records the hostname, and
	// another box backing up to the same repository with its clock
	// running ahead would otherwise be "newest" and be checked instead.
	host, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("finding this box's hostname (restic records it on every snapshot): %w", err)
	}
	out, err := j.Capture(ctx, "restic", "snapshots", "--json", "--latest", "1", "--host", host, "--tag", "hotserve,app:"+j.App)
	if err != nil {
		return fmt.Errorf("reading back the snapshot just written: %w", err)
	}
	var snaps []Snapshot
	if err := json.Unmarshal(out, &snaps); err != nil {
		return fmt.Errorf("restic returned a snapshot list this version cannot read: %w", err)
	}
	if len(snaps) == 0 {
		return errors.New("restic reported a backup, but no snapshot for this app is in the repository")
	}
	newest := snaps[0]
	for _, s := range snaps[1:] {
		if s.Time.After(newest.Time) {
			newest = s
		}
	}
	args := []string{"ls", "--json", newest.ShortID}
	seen := map[string]bool{}
	for _, w := range want {
		if parent := filepath.Dir(w.path); !seen[parent] {
			seen[parent] = true
			args = append(args, parent)
		}
	}
	out, err = j.Capture(ctx, "restic", args...)
	if err != nil {
		return fmt.Errorf("listing snapshot %s: %w", newest.ShortID, err)
	}
	nodes := map[string]lsNode{}
	for line := range strings.SplitSeq(string(out), "\n") {
		var n lsNode
		if json.Unmarshal([]byte(line), &n) == nil && n.StructType == "node" {
			nodes[n.Path] = n
		}
	}
	for _, w := range want {
		n, ok := nodes[w.path]
		switch {
		case !ok:
			return fmt.Errorf("snapshot %s does not contain %s, which was backed up — nothing of it could be restored", newest.ShortID, w.path)
		case n.Type == "symlink":
			return fmt.Errorf("snapshot %s holds %s as a symlink, not its data — nothing behind it could be restored", newest.ShortID, w.path)
		case w.database && (n.Type != "file" || n.Size == 0):
			return fmt.Errorf("snapshot %s holds the database copy %s as an empty %s — the copy did not take", newest.ShortID, w.path, n.Type)
		}
	}
	j.logf("%s: snapshot %s read back: %d declared path(s) present", j.App, newest.ShortID, len(want))
	return nil
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
