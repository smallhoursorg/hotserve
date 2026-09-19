package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// stagingRestore is the restore's own working dir, inside the app's
// staging dir and the only part of it the restore unit can see: its
// database copies and restic's cache live here. The rest — the backup's
// copies and its cache — is not in the restore's view, because the
// restore writes into the app's data, and a link the app left there
// must not be able to steer those writes into what the backup keeps.
const stagingRestore = "restore"

// StagingRestore is the restore unit's HOME and only writable dir
// besides the app's data.
func StagingRestore(appStaging string) string { return filepath.Join(appStaging, stagingRestore) }

// restoreErrors is what one `restic restore --json` said went wrong.
//
// It exists for one case. restic 0.18 restores a file's owner by
// number, and the restore unit's user namespace maps only root and the
// hotserve user — so where the snapshot records any other number,
// chown answers "invalid argument" rather than "not permitted", which
// is the one refusal restic overlooks when it is not root. That is
// every file on a rebuilt box whose hotserve user came out with a
// different uid than the old box's. The file is restored — data, mode
// and times — and belongs to this box's hotserve user, which is the
// right owner; but restic counts an error per file and exits 1, and
// taken at its word the restore would fail on every attempt, stopping
// before the next declared path.
//
// restic 0.19 has --ownership-by-name, which makes all of this
// unnecessary; Debian 13 has 0.18. With 0.19 on the supported platform,
// pass that flag and delete this.
type restoreErrors struct {
	// Owner counts `lchown <path>: invalid argument`.
	Owner int
	// Other counts everything else, and OtherFirst keeps the first few
	// as restic worded them.
	Other      int
	OtherFirst []string
	// Counted is the N of restic's closing "There were N errors", or
	// -1 when it gave none.
	Counted int
}

// ownerOnly reports whether the owner refusals are all that went wrong,
// by restic's own count as well as this one: an error restic counted
// and this did not recognise is an error.
func (e restoreErrors) ownerOnly() bool {
	return e.Owner > 0 && e.Other == 0 && e.Counted == e.Owner
}

// describe is the part of a failure's message that says what restic
// reported, for an error that has only its exit status otherwise.
func (e restoreErrors) describe() string {
	if e.Other == 0 {
		return ""
	}
	return fmt.Sprintf(" (restic reported %d errors, the first: %s)", e.Other, strings.Join(e.OtherFirst, "; "))
}

// read takes one line of the stderr of `restic restore --json`, as it
// is written — a line per file is a lot of lines for an uploads dir, so
// they are counted, not kept. Every error that is not an owner refusal
// goes to show, in restic's words, as it arrives.
//
// An owner refusal is recognised by both ends of its message: the same
// lchown fails with "no such file or directory" when restic could not
// write the file at all, and that is a real failure. restic's summary
// on stdout is no help in telling them apart — it counts such a file
// as restored.
func (e *restoreErrors) read(line string, show io.Writer) {
	other := func(msg string) {
		e.Other++
		if len(e.OtherFirst) < 3 {
			e.OtherFirst = append(e.OtherFirst, msg)
		}
		say(show, "restic: %s", msg)
	}
	m, ok := parseResticLine(line)
	switch {
	case !ok:
		other(line)
	case m.Type == "error" && m.During == "restore" && strings.HasPrefix(m.Error.Message, "lchown ") && strings.HasSuffix(m.Error.Message, ": invalid argument"):
		e.Owner++
	case m.Type == "error":
		other(m.Error.Message)
	case m.Type == "exit_error":
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(m.Message), "Fatal: There were %d errors", &n); err == nil {
			e.Counted = n
			return
		}
		other(strings.TrimSpace(m.Message))
	default:
		other(line)
	}
}

// readRestoreErrors reads a whole stderr.
func readRestoreErrors(r io.Reader, show io.Writer) restoreErrors {
	errs := restoreErrors{Counted: -1}
	w := &lineWriter{each: func(line string) { errs.read(line, show) }}
	if _, err := io.Copy(w, r); err != nil {
		errs.read("reading restic's errors: "+err.Error(), show)
	}
	w.flush()
	return errs
}

// RestoreJob puts one app's declared state back from a snapshot. It
// runs where the backup job runs — the same sandbox, as the same user,
// under the same unit name, so a restore and that app's hourly backup
// can never run at once — and it is launched by `hotserve backup
// restore`, which picks the snapshot and asks first.
//
// Everything is checked before anything is replaced: every declared
// path is looked up in the snapshot, and every database copy is taken
// out and passes SQLite's integrity check. A snapshot that fails any of
// that restores nothing. (A declared files path the snapshot does not
// hold at all is not a failure: the backup skips a path the app had not
// created yet, so there is nothing of it to put back, and it is left.)
type RestoreJob struct {
	App      string
	Shared   string
	Staging  string
	Snapshot string
	// Databases and Files are paths relative to Shared, as declared.
	Databases []string
	Files     []string
	// Delete removes files the app added after the snapshot was taken,
	// inside each declared files path. Off by default: a restore that
	// deletes what nobody asked it to is the one that loses data.
	Delete bool

	// Exec runs restic and sqlite3.
	Exec Exec
	Log  io.Writer
}

// Execute restores the snapshot.
//
// A database goes back with SQLite's own backup API (sqlite3
// `.restore`), not by copying the file over the live one: the app can
// keep running, its open connections see the restored data as one
// ordinary commit, and its -wal and -shm files stay consistent with
// the database they belong to. Copying a file under an open database
// is how a database gets corrupted.
func (j RestoreJob) Execute(ctx context.Context) error {
	if len(j.Databases) == 0 && len(j.Files) == 0 {
		return fmt.Errorf("app %s declares no state to restore", j.App)
	}
	// Where each declared path is in the snapshot: a database under the
	// copy the backup took, files where they live.
	var want []expectedNode
	for _, rel := range j.Databases {
		want = append(want, expectedNode{path: filepath.Join(StagingData(j.Staging), filepath.Clean(rel)), database: true})
	}
	for _, rel := range j.Files {
		p, err := sharedPath(j.Shared, rel)
		if err != nil {
			return fmt.Errorf("app %s: %w", j.App, err)
		}
		want = append(want, expectedNode{path: p})
	}
	nodes, err := j.list(ctx, want)
	if err != nil {
		return err
	}

	// 1. Every check, before anything live is touched.
	dir := filepath.Join(StagingRestore(j.Staging), "copies")
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("app %s: clearing an earlier restore's copies: %w", j.App, err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("app %s: %w", j.App, err)
	}
	// The copies are the app's data in plaintext; they go when this
	// does, whichever way it ends.
	defer func() { _ = os.RemoveAll(dir) }()
	copies := make([]string, len(j.Databases))
	for i, rel := range j.Databases {
		n, ok := nodes[want[i].path]
		switch {
		case !ok:
			return fmt.Errorf("snapshot %s holds no copy of %s (it would be at %s) — it was taken before the app declared it, or on a box with a different staging dir; nothing was restored", j.Snapshot, rel, want[i].path)
		case n.Type != "file" || n.Size == 0:
			return fmt.Errorf("snapshot %s holds %s as an empty %s, not a database — nothing was restored", j.Snapshot, rel, n.Type)
		}
		// Numbered, not named after the declaration: the name goes into
		// a sqlite3 dot-command below, which splits on whitespace and
		// reads quotes, so it has to be a path that needs neither.
		copies[i] = filepath.Join(dir, fmt.Sprintf("%d.db", i))
		if strings.ContainsAny(copies[i], " \t\n'\"\\") {
			return fmt.Errorf("app %s: the staging dir %s has a space, quote or backslash in it, which sqlite3's .restore cannot be given — use a plain --staging path", j.App, j.Staging)
		}
		j.logf("+ restic dump %s %s > %s", j.Snapshot, want[i].path, copies[i])
		if err := j.dump(ctx, want[i].path, copies[i]); err != nil {
			return fmt.Errorf("app %s: taking %s out of snapshot %s: %w", j.App, rel, j.Snapshot, err)
		}
		args := []string{copies[i], "PRAGMA integrity_check"}
		j.logf("+ sqlite3 %s", quoteArgs(args))
		out, err := j.Exec.output(ctx, Cmd{Name: "sqlite3", Args: args})
		// sqlite3 rejects a damaged copy in one of two ways: it prints
		// what is wrong and exits 0, or — a page it cannot read at all —
		// it says "malformed" on stderr and exits 1. Both are the check
		// failing; only sqlite3 not running at all is something else.
		if err != nil && exitStatus(err) < 0 {
			return fmt.Errorf("app %s: checking the copy of %s: %w", j.App, rel, err)
		}
		if got := strings.TrimSpace(string(out)); err != nil || got != "ok" {
			reason := strings.SplitN(got, "\n", 2)[0]
			if reason == "" {
				reason = "sqlite3: " + err.Error()
			}
			return fmt.Errorf("app %s: the copy of %s in snapshot %s fails SQLite's integrity check (%s) — nothing was restored; try an earlier snapshot", j.App, rel, j.Snapshot, reason)
		}
	}
	type fileStep struct {
		rel, path string
		dir       bool
		mode      *fs.FileMode
	}
	var files []fileStep
	for i, rel := range j.Files {
		w := want[len(j.Databases)+i]
		n, ok := nodes[w.path]
		switch {
		case !ok:
			// The backup skips a declared path the app had not created
			// yet, so a snapshot without it is ordinary — there is
			// nothing of it to put back.
			j.logf("%s: %s is not in snapshot %s (the app had not created it yet), leaving it as it is", j.App, filepath.Clean(rel), j.Snapshot)
			continue
		case n.Type == "dir":
			files = append(files, fileStep{rel: filepath.Clean(rel), path: w.path, dir: true})
		case n.Type == "file":
			step := fileStep{rel: filepath.Clean(rel), path: w.path}
			if n.Mode != nil {
				m := fs.FileMode(*n.Mode).Perm()
				step.mode = &m
			}
			files = append(files, step)
		default:
			return fmt.Errorf("snapshot %s holds %s as a %s, which this does not restore — nothing was restored", j.Snapshot, rel, n.Type)
		}
	}

	// A snapshot holding nothing this app declares is not one of this
	// app as it is declared here — taken under another liveswap root,
	// most likely, since a snapshot keeps absolute paths. Restoring
	// "nothing" from it and saying "restored" is the one answer that is
	// wrong.
	if len(j.Databases) == 0 && len(files) == 0 {
		return fmt.Errorf("app %s: snapshot %s holds none of the paths this app declares under %s — nothing was restored. `hotserve backup restic -- ls %s` shows what it holds; a snapshot taken under another liveswap root keeps its paths under that root", j.App, j.Snapshot, j.Shared, j.Snapshot)
	}

	// 2. Everything checked out: put it back.
	for i, rel := range j.Databases {
		live, err := sharedPath(j.Shared, rel)
		if err != nil {
			return fmt.Errorf("app %s: %w", j.App, err)
		}
		if err := os.MkdirAll(filepath.Dir(live), 0o750); err != nil {
			return fmt.Errorf("app %s: %w", j.App, err)
		}
		// .timeout: the app may be mid-write, and this waits for it
		// the way the backup's copy does.
		args := []string{"-cmd", ".timeout " + sqliteBusyTimeoutMS, live, ".restore " + copies[i]}
		j.logf("+ sqlite3 %s", quoteArgs(args))
		if err := j.Exec(ctx, Cmd{Name: "sqlite3", Args: args}); err != nil {
			// .restore is one transaction: a failure leaves the database
			// as it was.
			return fmt.Errorf("app %s: restoring %s failed, and it is as it was: %w", j.App, rel, err)
		}
		j.logf("%s: %s restored", j.App, filepath.Clean(rel))
	}
	for _, f := range files {
		if f.dir {
			// The snapshot's copy of this one directory, into the live
			// one: restic puts back what differs and, only when asked,
			// removes what the snapshot does not have.
			// --json for the errors, which are read rather than shown
			// (see restoreErrors); --quiet because nothing else it
			// would print is used.
			args := []string{"restore", "--json", "--quiet", j.Snapshot + ":" + f.path, "--target", f.path}
			if j.Delete {
				args = append(args, "--delete")
			}
			j.logf("+ restic %s", quoteArgs(args))
			errs, err := j.restoreTree(ctx, args)
			switch {
			case err == nil:
			case errs.ownerOnly():
				j.logf("%s: %s: %d files and directories belong to this box's user, not the owner the snapshot records — it records an owner by number, and that number is no user here (a rebuilt box gives the same user another one). Their data, modes and times are restored.", j.App, f.rel, errs.Owner)
			default:
				// Not atomic: restic writes file by file, so a failure
				// part-way leaves some of it restored (and, with
				// --delete, some of it pruned). Running the restore
				// again finishes it.
				return fmt.Errorf("app %s: restoring %s failed part-way, so it may be partly restored — run the restore again to finish it: %w%s", j.App, f.rel, err, errs.describe())
			}
		} else {
			// restic restores directories; a declared single file comes
			// out with dump, beside the live one, and replaces it in one
			// rename so the app never reads half of it.
			tmp := filepath.Join(filepath.Dir(f.path), "."+filepath.Base(f.path)+".hotserve-restore")
			j.logf("+ restic dump %s %s > %s", j.Snapshot, f.path, tmp)
			if err := os.MkdirAll(filepath.Dir(f.path), 0o750); err != nil {
				return fmt.Errorf("app %s: %w", j.App, err)
			}
			// A leftover from a restore that was interrupted — or a
			// link planted there: Remove takes the name away, never
			// what a link points at.
			if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("app %s: restoring %s: %w", j.App, f.rel, err)
			}
			err := j.dump(ctx, f.path, tmp)
			if err == nil && f.mode != nil {
				// dump gives the bytes and not the mode; the listing
				// has it, and a script that was executable stays so —
				// as does a file nobody may read (mode 0).
				err = os.Chmod(tmp, *f.mode)
			}
			if err != nil {
				_ = os.Remove(tmp)
				return fmt.Errorf("app %s: restoring %s: %w", j.App, f.rel, err)
			}
			if err := os.Rename(tmp, f.path); err != nil {
				_ = os.Remove(tmp)
				return fmt.Errorf("app %s: restoring %s: %w", j.App, f.rel, err)
			}
		}
		j.logf("%s: %s restored", j.App, f.rel)
	}
	return nil
}

// dump writes one file out of the snapshot to dst — `restic dump`, whose
// output is the file's bytes. O_EXCL: dst is a name the caller has just
// cleared, so it is created, never opened through a link that appeared
// there since.
func (j RestoreJob) dump(ctx context.Context, path, dst string) error {
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640) //nolint:gosec // a path built here, inside the unit's own view
	if err != nil {
		return err
	}
	c := restic("dump", j.Snapshot, path)
	c.Stdout = f
	err = j.Exec(ctx, c)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// restoreTree runs one `restic restore --json` of a directory, reading
// its errors off stderr as they are written; err is its exit status.
// Its stdout is the summary, and is dropped: restic counts a file it
// could not write as restored, so the summary is evidence of nothing.
func (j RestoreJob) restoreTree(ctx context.Context, args []string) (restoreErrors, error) {
	errs := restoreErrors{Counted: -1}
	show := j.Log
	if show == nil {
		show = io.Discard
	}
	stderr := &lineWriter{each: func(line string) { errs.read(line, show) }}
	c := restic(args...)
	c.Stdout, c.Stderr = io.Discard, stderr
	err := j.Exec(ctx, c)
	stderr.flush()
	return errs, err
}

// list looks every declared path up in the snapshot: the listing the
// backup's read-back reads (listNodes), so the two cannot come to read a
// snapshot differently.
func (j RestoreJob) list(ctx context.Context, want []expectedNode) (map[string]lsNode, error) {
	j.logf("+ restic %s", quoteArgs(listArgs(j.Snapshot, want)))
	nodes, err := listNodes(ctx, j.Exec, j.Snapshot, want)
	if err != nil {
		return nil, fmt.Errorf("app %s: listing snapshot %s: %w", j.App, j.Snapshot, err)
	}
	return nodes, nil
}

func (j RestoreJob) logf(format string, a ...any) {
	if j.Log != nil {
		say(j.Log, format, a...)
	}
}

// RestoreArgs is the systemd-run invocation for one app's restore: the
// backup job's own unit — name, user and sandbox — with the app's data
// writable, since this is the one job that writes it. --pipe, because
// someone is at the terminal waiting on it.
func RestoreArgs(app App, o LaunchOptions, snapshot string, del bool) []string {
	staging := o.StagingRoot + "/" + app.Name
	args := append([]string{
		"--wait", "--collect", "--quiet", "--pipe", noExpansion,
		"--unit=" + unitName(app.Name),
	}, sandboxProperties(jobView{
		User:    o.User,
		EnvFile: o.EnvFile,
		// Not the staging dir: only the restore's own part of it.
		Home:           StagingRestore(staging),
		Shared:         app.Shared,
		SharedWritable: true,
	})...)
	args = append(args,
		o.Self, "backup", "restore-app",
		"--name="+app.Name,
		"--shared="+app.Shared,
		"--staging="+staging,
		"--snapshot="+snapshot,
	)
	if del {
		args = append(args, "--delete")
	}
	for _, e := range app.State {
		args = append(args, e.Kind+":"+e.Path)
	}
	return args
}

// pickSnapshot finds the snapshot a restore uses among this app's: the
// one asked for — by the short or the full id — or the newest. Only
// this app's snapshots are candidates, so an id copied from another
// app's listing is refused rather than restored into this one.
//
// Unasked, it is the newest snapshot a clean-run record vouches for
// (clean: the ids recorded for this app, see CleanTag) — from any box,
// since a rebuilt box has a new hostname and the box whose snapshots it
// needs is the one that is gone. restic writes a snapshot even when the
// run fails part-way, so an unvouched one can be missing files: restored,
// they would be silently absent, and with --delete, deleted. A newer
// unvouched snapshot is named in the note, for --snapshot, rather than
// chosen; with no vouched snapshot at all, one has to be chosen by hand.
// One chosen by hand is restored — and said to be unvouched when it is,
// before the operator confirms.
func pickSnapshot(app string, snaps []Snapshot, want string, clean map[string]bool) (Snapshot, string, error) {
	if len(snaps) == 0 {
		return Snapshot{}, "", fmt.Errorf("the repository has no snapshot of %s", app)
	}
	if want == "" || want == "latest" {
		var latest, latestClean *Snapshot
		for i, s := range snaps {
			if latest == nil || s.Time.After(latest.Time) {
				latest = &snaps[i]
			}
			if clean[s.ID] && (latestClean == nil || s.Time.After(latestClean.Time)) {
				latestClean = &snaps[i]
			}
		}
		switch {
		case latestClean == nil:
			return Snapshot{}, "", fmt.Errorf("no snapshot of %s is recorded as coming from a run that finished cleanly, so any of them may be missing files — choose one yourself with --snapshot <id> (`sudo hotserve backup restic -- snapshots --tag app:%s` lists them; the newest is %s)", app, app, latest.ShortID)
		case latestClean.ID != latest.ID:
			return *latestClean, fmt.Sprintf("Snapshot %s is newer, but the run that took it did not finish cleanly, so it may be missing files; this uses the newest clean one. --snapshot %s restores that one instead.", latest.ShortID, latest.ShortID), nil
		}
		return *latestClean, "", nil
	}
	var found []Snapshot
	for _, s := range snaps {
		if s.ShortID == want || s.ID == want || (len(want) >= 8 && strings.HasPrefix(s.ID, want)) {
			found = append(found, s)
		}
	}
	switch len(found) {
	case 0:
		return Snapshot{}, "", fmt.Errorf("no snapshot %s of %s — `sudo hotserve backup restic -- snapshots --tag app:%s` lists them", want, app, app)
	case 1:
		if !clean[found[0].ID] {
			return found[0], fmt.Sprintf("No clean run vouches for snapshot %s: the run that took it may not have finished, so it may be missing files. What it is missing is not put back — and with --delete, is removed from the live data.", found[0].ShortID), nil
		}
		return found[0], "", nil
	default:
		return Snapshot{}, "", fmt.Errorf("%s matches %d snapshots of %s; give more of its id", want, len(found), app)
	}
}

// DescribeRestore is what the operator confirms: which snapshot, from
// when, and what happens to each declared path.
func DescribeRestore(w io.Writer, app App, s Snapshot, del bool, now time.Time) {
	say(w, "Restoring %s from snapshot %s, taken %s (%s) on %s:", app.Name, s.ShortID,
		s.Time.Local().Format("2006-01-02 15:04 MST"), humanAge(now.Sub(s.Time)), s.Hostname)
	for _, rel := range app.Databases() {
		say(w, "  %-20s replaced by the snapshot's copy; the app's writes wait while it goes in", filepath.Clean(rel))
	}
	for _, rel := range app.Files() {
		if del {
			say(w, "  %-20s made exactly as it was: files added since are DELETED", filepath.Clean(rel))
		} else {
			say(w, "  %-20s files in the snapshot put back; files added since are kept", filepath.Clean(rel))
		}
	}
	if len(app.Files()) > 0 {
		say(w, "(A path the snapshot does not hold — the app had not created it yet — is left as it is.)")
	}
}

// confirmRestore asks for the app's name to be typed, the way a
// destructive command should: a y/N is answered by reflex.
func confirmRestore(app string, ask Prompter) error {
	if ask == nil {
		return errors.New("a restore replaces live data, so it asks first; run it at a terminal, or pass --yes to a script that means it")
	}
	got, err := ask("Type the app's name to restore it", false)
	if err != nil {
		return err
	}
	if got != app {
		return fmt.Errorf("%q is not %s; nothing was restored", got, app)
	}
	return nil
}
