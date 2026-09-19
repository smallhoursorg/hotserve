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
	"sync"
	"time"
)

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

	// Staged says the databases were copied by an earlier step — the
	// staging unit, which has the app's data writable and no network —
	// so Execute uploads the copies it finds and opens no database.
	Staged bool

	// Exec runs sqlite3 and restic, both from the distribution.
	Exec Exec
	Log  io.Writer
	Env  func(string) string
}

// Stage takes the consistent copy of each declared database, and does
// nothing else: it needs neither the repository nor the network.
func (j Job) Stage(ctx context.Context) error {
	if len(j.Databases) == 0 {
		return fmt.Errorf("app %s declares no database to copy", j.App)
	}
	return j.stageDatabases(ctx)
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
	if err := checkSettings(env); err != nil {
		return err
	}
	if len(j.Databases) == 0 && len(j.Files) == 0 {
		return fmt.Errorf("app %s declares no state to back up", j.App)
	}
	targets := []string{}
	if len(j.Databases) == 0 {
		// No database declared: a staged copy left by a removed
		// declaration is plaintext app data, and nothing else would
		// remove it. stageDatabases clears this itself when there is
		// something to stage.
		if err := os.RemoveAll(StagingData(j.Staging)); err != nil {
			return fmt.Errorf("app %s: clearing staged copies: %w", j.App, err)
		}
	}
	if len(j.Databases) > 0 {
		stage := j.stageDatabases
		if j.Staged {
			stage = j.requireStaged
		}
		if err := stage(ctx); err != nil {
			return err
		}
		// Only the copies, never the whole staging dir: restic's own
		// cache sits beside them and must not be backed up.
		targets = append(targets, StagingData(j.Staging))
	}
	// What this box's last clean run backed up is the only way to tell a
	// path that is not there YET from one that is not there ANY MORE. It
	// is in the repository, and is asked for only when a declared path is
	// missing: a run whose paths are all there costs no extra call.
	var before map[string]bool
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
		if errors.Is(err, fs.ErrNotExist) && before == nil {
			// Not knowing is not "not created yet": a run that cannot
			// ask fails, rather than quietly dropping a path from every
			// backup from now on.
			if before, err = j.lastCleanPaths(ctx); err != nil {
				return fmt.Errorf("app %s: state files %s is not there, and whether it has been backed up before could not be read from the repository: %w", j.App, rel, err)
			}
			err = fs.ErrNotExist
		}
		switch {
		case errors.Is(err, fs.ErrNotExist) && before[p]:
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
	}

	// Everything declared is still to come: a files-only app whose
	// directories the app has not created yet leaves nothing to hand
	// restic, and restic with no target exits with an argument error.
	// Saying so and succeeding is the same answer the skip above gives.
	if len(targets) == 0 {
		j.logf("%s: nothing to back up yet — every declared path is still to be created", j.App)
		return nil
	}

	snapshot, err := j.backup(ctx, targets)
	if err != nil {
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
	if err := j.verifySnapshot(ctx, snapshot, want); err != nil {
		return fmt.Errorf("app %s: %w", j.App, err)
	}
	// A snapshot existing is not the same as a backup succeeding:
	// restic writes one and still exits non-zero when it could not
	// read some of the sources. So a clean run says so, in the
	// repository (see CleanTag) — only after a clean exit AND a
	// snapshot read back holding what was declared. status measures
	// freshness from these records, and restore chooses by them.
	record := cleanRecordArgs(j.App, snapshot)
	j.logf("+ restic %s", quoteArgs(record))
	if err := j.Exec(ctx, restic(record...)); err != nil {
		return fmt.Errorf("app %s: snapshot %s is in the repository, but recording it as a clean run failed, so it will not count as one: %w", j.App, shortID(snapshot), err)
	}
	return nil
}

// backup runs restic over targets and returns the id of the snapshot it
// wrote — restic's own word for it, from the summary it prints with
// --json, so that the read-back and the clean-run record are about this
// run's snapshot and no other. ("The newest snapshot of this app" is
// not that whenever the clock has stepped back since an earlier run,
// and that run may have been one restic exited part-way through.)
//
// --json turns restic's errors into JSON too, so they are put back into
// words for the journal; --quiet leaves stdout with the summary alone.
func (j Job) backup(ctx context.Context, targets []string) (string, error) {
	args := ResticBackupArgs(j.App, targets)
	j.logf("+ restic %s", quoteArgs(args))
	var (
		mu      sync.Mutex // the two streams are written from two goroutines
		summary resticMessage
	)
	stdout := &lineWriter{each: func(line string) {
		mu.Lock()
		defer mu.Unlock()
		if m, ok := parseResticLine(line); ok && m.Type == "summary" {
			summary = m
		} else if !ok {
			j.logf("restic: %s", line)
		}
	}}
	stderr := &lineWriter{each: func(line string) {
		mu.Lock()
		defer mu.Unlock()
		j.logf("restic: %s", resticSays(line))
	}}
	c := restic(args...)
	c.Stdout, c.Stderr = stdout, stderr
	err := j.Exec(ctx, c)
	stdout.flush()
	stderr.flush()
	if err != nil {
		return "", err
	}
	if summary.SnapshotID == "" {
		return "", errors.New("restic exited cleanly without naming the snapshot it wrote, so there is nothing to read back")
	}
	j.logf("%s: snapshot %s: %d new and %d changed files, %s added to the repository, in %s", j.App, shortID(summary.SnapshotID),
		summary.FilesNew, summary.FilesChanged, humanBytes(summary.DataAdded), time.Duration(summary.TotalDuration*float64(time.Second)).Round(100*time.Millisecond))
	return summary.SnapshotID, nil
}

// shortID is a snapshot id as restic prints it.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// humanBytes is a size for a log line.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// lastCleanPaths is what this box's newest clean run backed up, as the
// absolute paths restic was given — read from the repository: the
// newest clean-run record from this host, then the snapshot it vouches
// for. No clean run yet is no paths, not an error.
//
// From this host only. restore takes a vouched snapshot from any box,
// because someone is there to read which box it names; this decides,
// with nobody watching, whether a run fails — and another box backing
// an app of the same name up to the same repository says nothing about
// what this one has had.
func (j Job) lastCleanPaths(ctx context.Context) (map[string]bool, error) {
	paths := map[string]bool{}
	host, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("finding this box's hostname (restic records it on every snapshot): %w", err)
	}
	out, err := j.Exec.output(ctx, restic("snapshots", "--json", "--latest", "1", "--host", host, "--tag", CleanTag+","+cleanAppTag(j.App)))
	if err != nil {
		return nil, err
	}
	var listed []Snapshot
	if err := json.Unmarshal(out, &listed); err != nil {
		return nil, fmt.Errorf("restic returned a snapshot list this version cannot read: %w", err)
	}
	_, vouched, _ := cleanRecords(listed)
	for id := range vouched[j.App] {
		out, err := j.Exec.output(ctx, restic("snapshots", "--json", id))
		if err != nil {
			return nil, fmt.Errorf("the clean run's snapshot %s: %w", id, err)
		}
		var snaps []Snapshot
		if err := json.Unmarshal(out, &snaps); err != nil {
			return nil, fmt.Errorf("restic returned a snapshot list this version cannot read: %w", err)
		}
		for _, s := range snaps {
			for _, p := range s.Paths {
				paths[p] = true
			}
		}
	}
	return paths, nil
}

// requireStaged checks that the staging step left a copy of every
// declared database: a copy that is not there would otherwise be a
// backup without that database, found out by the read-back only after
// the upload.
func (j Job) requireStaged(context.Context) error {
	for _, rel := range j.Databases {
		staged := filepath.Join(StagingData(j.Staging), filepath.Clean(rel))
		if info, err := os.Lstat(staged); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("app %s: no staged copy of %s at %s — the staging step copies the databases before this one uploads them", j.App, rel, staged)
		}
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
		if err := j.Exec(ctx, Cmd{Name: "sqlite3", Args: args}); err != nil {
			return fmt.Errorf("app %s: copying %s: %w", j.App, rel, err)
		}
	}
	return nil
}

// ResticBackupArgs is the whole restic invocation, as its own
// function so the tests pin the argv (tags included: they are what
// `hotserve backup status` and a per-app restore select on). --json is
// for the summary, which names the snapshot; --quiet leaves stdout with
// nothing else.
func ResticBackupArgs(app string, targets []string) []string {
	args := []string{"backup", "--json", "--quiet", "--tag", "hotserve", "--tag", "app:" + app}
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
	// Mode is nil when the listing has none, so a recorded mode of 0 —
	// a file nobody may read — is told apart from a missing one.
	Mode *uint32 `json:"mode"`
}

// verifySnapshot reads the snapshot just written back out of the
// repository and requires every declared path to be in it as data.
//
// Every check before this one is about the job's own view: the file
// existed, the copy ran, restic exited 0. None of them is the backup:
// all of those can be true while the snapshot holds nothing worth
// restoring — a symlink stored as a link, a database copy that came
// out empty. Reading the snapshot back catches any such case: a
// declared path that is missing, is a link, or (for a database) is
// empty fails the run, so it never earns the clean-run record.
//
// One cheap call: `ls` of each declared path's PARENT. restic's ls lists
// a named directory's direct children, so naming an uploads dir itself
// would return one line per upload — a hundred thousand of them, held
// in this job's memory. In the snapshot, a declared path's parent holds
// only what was backed up from it, so its listing grows with the number
// of declarations and nothing else.
func (j Job) verifySnapshot(ctx context.Context, snapshot string, want []expectedNode) error {
	args := []string{"ls", "--json", snapshot}
	seen := map[string]bool{}
	for _, w := range want {
		if parent := filepath.Dir(w.path); !seen[parent] {
			seen[parent] = true
			args = append(args, parent)
		}
	}
	out, err := j.Exec.output(ctx, restic(args...))
	if err != nil {
		return fmt.Errorf("listing snapshot %s: %w", shortID(snapshot), err)
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
			return fmt.Errorf("snapshot %s does not contain %s, which was backed up — nothing of it could be restored", shortID(snapshot), w.path)
		case n.Type == "symlink":
			return fmt.Errorf("snapshot %s holds %s as a symlink, not its data — nothing behind it could be restored", shortID(snapshot), w.path)
		case w.database && (n.Type != "file" || n.Size == 0):
			return fmt.Errorf("snapshot %s holds the database copy %s as an empty %s — the copy did not take", shortID(snapshot), w.path, n.Type)
		}
	}
	j.logf("%s: snapshot %s read back: %d declared path(s) present", j.App, shortID(snapshot), len(want))
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
