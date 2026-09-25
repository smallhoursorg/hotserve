// Package engine is one backup run: it reads the plan, and for each app
// that declares a backup it looks for the data, dumps the databases,
// uploads, checks that what it uploaded is in the snapshot, and removes
// the plaintext copies — each step a unit of its own, with only what
// that step needs.
//
// The engine runs as root and is root-equivalent (it starts system
// units and makes mounts), so it touches as little as it can: it never
// reads or removes anything an app wrote — it opens an app's
// directories only as O_PATH, to name them (pin.go) — never reads the
// credential file (it passes the path to the manager), and takes
// nothing from a unit but an exit status and a small JSON file, of
// which it keeps only what it recognises, cleaned (record.Text).
package engine

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/smallhoursorg/hotserve/backups/dump"
	"github.com/smallhoursorg/hotserve/backups/plan"
	"github.com/smallhoursorg/hotserve/backups/record"
	"github.com/smallhoursorg/hotserve/backups/unit"
	"github.com/smallhoursorg/hotserve/liveswap/backupdecl"
)

// Config is where things are. Every field is a constant of the
// installation, none of it an operator's or an app's string.
type Config struct {
	ConfigDir string // /etc/hotserve: what the plan unit sees, for the Caddyfile's imports
	EnvFile   string // /etc/hotserve/backup.env: root-only; read by the manager, never here
	StateDir  string // /var/lib/hotserve-backup
	RunDir    string // /run/hotserve-backup
	Self      string // /usr/bin/hotserve-backup
	Restic    string // /usr/bin/restic
	// BindsTo is the engine's own service when it has one, so that the
	// manager ends its units if it dies; empty when run from a shell.
	BindsTo string
}

// Accounts the units run as.
const (
	dataUser   = "hotserve"        // owns the apps' data; the only uid that reads a live database
	backupUser = "hotserve-backup" // holds the credential while restic runs; runs nothing else
)

// retryLock is how long restic waits for a repository something else
// holds — a check takes it exclusively. A flag, because restic 0.18
// does not read it from the environment.
const retryLock = "2h"

// Runner is the part of unit.Runner a run uses.
type Runner interface {
	Run(ctx context.Context, s unit.Spec) (unit.Outcome, error)
	Stop(name string) error
}

// ErrBusy is returned when another run holds the lock.
var ErrBusy = errors.New("another backup run is in progress")

type run struct {
	cfg    Config
	r      Runner
	nonce  string
	dir    string // this run's own directory under RunDir
	status *record.Status
	prev   *record.Status
	mounts int // how many mount points this run has made, for their names
	// left are the apps on the last record that this run's plan no
	// longer has: said once, by this run, and never kept.
	left map[string]bool
}

// preRestoreTag is on a snapshot a restore made of what it was about to
// restore over. Such a snapshot is there to undo that restore, by name:
// it is never what "the newest" means to a restore or to a drill — it
// holds, as often as not, the damage that was being restored over.
const preRestoreTag = "pre-restore"

// Run does one run and writes the record. The error is about the run as
// a whole; how each app fared is in the record.
func Run(ctx context.Context, cfg Config, r Runner) (*record.Status, error) {
	x, end, err := begin(cfg, r)
	if x == nil {
		return nil, err
	}
	defer end()
	if err != nil {
		return x.finish(err)
	}
	return x.finish(x.apps(ctx))
}

// begin is how a run, a restore and a drill each start: the one lock,
// the last record, whatever an earlier one left running or mounted taken
// away, and a directory of its own. With an error and a run, the lock is
// held and the record is still to be written; end removes the directory
// and releases the lock.
func begin(cfg Config, r Runner) (x *run, end func(), err error) {
	if _, err := os.Lstat(cfg.EnvFile); err != nil {
		return nil, nil, fmt.Errorf("backups are not set up: %s: %w", cfg.EnvFile, err)
	}
	// The state dir is where the status record is read from by anyone;
	// everything else is root's alone. Units reach staging through
	// binds the manager makes, not by walking here.
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil { //nolint:gosec // holds status.json, which is for everyone to read
		return nil, nil, err
	}
	// Said again, because a mode given to mkdir is cut by the caller's
	// umask, and sudo hands down a shell's.
	if err := os.Chmod(cfg.StateDir, 0o755); err != nil { //nolint:gosec // as above
		return nil, nil, err
	}
	for _, d := range []string{cfg.RunDir, filepath.Join(cfg.StateDir, "staging"), filepath.Join(cfg.StateDir, "restore")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, nil, err
		}
	}
	unlock, err := lock(filepath.Join(cfg.RunDir, "lock"))
	if err != nil {
		return nil, nil, err
	}
	nonce, err := newNonce()
	if err != nil {
		unlock()
		return nil, nil, err
	}
	prev, err := record.Read(filepath.Join(cfg.StateDir, "status.json"))
	unreadable := ""
	if err != nil {
		// A record that cannot be read costs what it remembered, and is
		// said; it does not cost the box its backups.
		unreadable = record.Text(fmt.Sprintf("the previous record could not be read and was replaced: %v", err))
		prev = &record.Status{Apps: map[string]*record.App{}}
	}
	x = &run{cfg: cfg, r: r, nonce: nonce, dir: filepath.Join(cfg.RunDir, nonce), prev: prev,
		status: &record.Status{Started: time.Now().UTC(), Warning: unreadable, Apps: map[string]*record.App{}, Listed: prev.Listed, Unlisted: prev.Unlisted, LastDrill: prev.LastDrill}}
	if err := x.sweep(); err != nil {
		return x, unlock, err
	}
	if err := os.Mkdir(x.dir, 0o700); err != nil {
		return x, unlock, err
	}
	// Every mount under it has been taken away by then, each by its own
	// defer — but a mount that would not go is the app's own data, bound
	// here, and root must never walk into it deleting: removeRunDir does
	// not recurse.
	return x, func() { removeRunDir(x.dir); unlock() }, nil
}

// finish writes the record, whatever became of the run. An app the run
// did not reach is recorded as not run — never as whatever the last
// run found, under this run's date — and keeps only when it was last
// ok.
func (x *run) finish(runErr error) (*record.Status, error) {
	statusPath := filepath.Join(x.cfg.StateDir, "status.json")
	if runErr != nil {
		x.status.Error = record.Text(runErr.Error())
		for name, old := range x.prev.Apps {
			if _, reached := x.status.Apps[name]; !reached && old != nil && !x.left[name] {
				x.status.Apps[name] = &record.App{Class: record.NotRun, Detail: "the run ended before it reached this app", Looked: old.Looked, LastOK: old.LastOK, LastSnapshot: old.LastSnapshot,
					RestoreProven: old.RestoreProven, RestoreDrill: old.RestoreDrill}
			}
		}
	}
	x.status.Finished = time.Now().UTC()
	if err := record.Write(statusPath, x.status); err != nil {
		return x.status, errors.Join(runErr, err)
	}
	return x.status, runErr
}

func (x *run) apps(ctx context.Context) error {
	p, err := x.plan(ctx)
	if err != nil {
		return err
	}
	x.status.Root = p.Root
	// An app that has left the plan with snapshots in the repository — a
	// block deleted, an import that stopped matching — is said by the
	// run that finds it gone. Once: this record drops it, as the
	// operator may have meant it to, and nothing after remembers.
	var left []string
	x.left = map[string]bool{}
	for name, old := range x.prev.Apps {
		if _, planned := p.Apps[name]; planned {
			continue
		}
		x.left[name] = true
		if old != nil && old.LastSnapshot != nil {
			left = append(left, fmt.Sprintf("%s no longer declares a backup and is not backed up any more: its last snapshot is %.8s, of %s.", name, old.LastSnapshot.ID, old.LastSnapshot.Time.UTC().Format("2006-01-02 15:04 MST")))
		}
	}
	sort.Strings(left)
	for _, l := range left {
		x.status.Warning = strings.TrimSpace(x.status.Warning + " " + record.Text(l))
	}
	x.forget(ctx, p)
	x.sweepFetched(ctx)
	// Name order, but what failed last run goes last: a failure that is
	// one app's own — and ends the run, as every restic exit 1 does —
	// costs its neighbours one run, not every run.
	names := p.Names()
	sort.SliceStable(names, func(i, j int) bool {
		return !x.failedLast(names[i]) && x.failedLast(names[j])
	})
	var stop *record.App // set once the repository refuses for a reason every app shares
	for _, name := range names {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if stop != nil {
			x.status.Apps[name] = &record.App{Class: record.NotAttempted, Detail: stop.Detail, Looked: backupdecl.SharedDir(p.Root, name)}
		} else {
			app, repositoryWide := x.app(ctx, p.Root, name, p.Apps[name])
			x.status.Apps[name] = app
			if repositoryWide {
				stop = app
			}
		}
		x.carryLastOK(name)
		// A backup nobody has restored is a hypothesis: an app's first
		// good backup has its snapshot fetched and checked, there and
		// then. After that — proven or not — it is the drill's: a drill
		// that keeps failing is not re-fetched, in full, every hour.
		if app := x.status.Apps[name]; app.Class == record.OK && app.RestoreProven == nil && app.RestoreDrill == nil && ctx.Err() == nil {
			if x.firstDrill(ctx, name, app) {
				stop = &record.App{Detail: app.RestoreDrill.Detail}
			}
		}
	}
	// Once the repository has refused for a reason every app shares,
	// asking it again is one more wait for the same answer.
	if stop == nil && ctx.Err() == nil {
		x.list(ctx)
	}
	return ctx.Err()
}

// list asks the repository, once, what it holds of every app, and
// writes beside each snapshot the record names when it was last there:
// a snapshot pruned off the box is otherwise "the last good backup" for
// as long as nothing replaces it. A listing that fails, or that cannot
// be trusted, is a warning and changes nothing — it is never what makes
// a snapshot look gone, nor a good backup bad.
func (x *run) list(ctx context.Context) {
	named := func(app *record.App) (out []*record.Snapshot) {
		if app == nil {
			return nil
		}
		for _, snap := range []*record.Snapshot{app.Snapshot, app.LastOK, app.LastSnapshot} {
			if snap != nil {
				out = append(out, snap)
			}
		}
		if app.RestoreProven != nil {
			out = append(out, &app.RestoreProven.Snapshot)
		}
		return out
	}
	nothing := true
	for _, app := range x.status.Apps {
		nothing = nothing && len(named(app)) == 0
	}
	kept := filepath.Join(x.cfg.StateDir, "listing.err")
	if nothing {
		// Nothing to list, so nothing a listing failed to say: an earlier
		// failure was about snapshots this record no longer names, and
		// kept, it would turn status unhealthy for good.
		x.status.Unlisted = nil
		_ = os.Remove(kept)
		return
	}
	snaps, _, err := x.snapshots(ctx, "listing", "")
	if ctx.Err() != nil {
		return
	}
	// restic leaves a snapshot it cannot load out of the listing, says
	// so on stderr alone, and exits 0 [measured, a cold cache]: an
	// answer that came with anything beside it is not taken for the
	// whole repository.
	//
	// The unit's stderr is in a file, so the journal has none of it, and
	// the record is for everyone to read, so restic's words — which can
	// say where the repository is — do not go there either, but for the
	// id in that one line. They are kept for root, until a listing is
	// answered.
	said, readErr := os.ReadFile(filepath.Join(x.dir, ".listing.err"))
	switch {
	case err != nil:
	case readErr != nil:
		err = fmt.Errorf("what restic said beside the listing could not be read, so the listing is not believed: %w", readErr)
	case len(bytes.TrimSpace(said)) > 0:
		if m := ignoringRe.FindSubmatch(said); m != nil {
			err = fmt.Errorf("restic could not load snapshot %.8s and left it out of the listing", m[1])
		} else {
			err = errors.New("restic had something to say beside the listing")
		}
	}
	if err != nil {
		where := ""
		if len(bytes.TrimSpace(said)) > 0 && os.WriteFile(kept, said, 0o600) == nil && os.Chmod(kept, 0o600) == nil { //nolint:gosec // a constant name under root's own state dir
			where = fmt.Sprintf(" What restic said is in %s, root's to read.", kept)
		}
		if x.status.Unlisted == nil {
			since := time.Now().UTC()
			x.status.Unlisted = &since
		}
		x.status.Warning = strings.TrimSpace(x.status.Warning + " " + record.Text(fmt.Sprintf("the repository could not be listed, so nothing is known of whether it still holds each snapshot named here: %v.%s", err, where)))
		return
	}
	_ = os.Remove(kept) // an answered listing: what was said of an earlier one is history
	x.status.Unlisted = nil
	held := map[string]bool{} // app, a slash — no app's name holds one — and the id
	for _, s := range snaps {
		held[s.App+"/"+s.ID] = true
	}
	now := time.Now().UTC()
	for name, app := range x.status.Apps {
		for _, snap := range named(app) {
			// What this run itself saw written, or fetched, is not called
			// gone by this run's listing: a store that lists a new object
			// late [not measured of any] would turn every good backup into
			// a missing one until the next run. The next listing judges it.
			sawItself := snap.Seen != nil && !snap.Seen.Before(x.status.Started)
			if held[name+"/"+snap.ID] || sawItself {
				snap.Seen = &now
			}
		}
	}
	x.status.Listed = &now
}

func (x *run) failedLast(name string) bool {
	old := x.prev.Apps[name]
	return old != nil && old.Class == record.Failed
}

func (x *run) carryLastOK(name string) {
	app, old := x.status.Apps[name], x.prev.Apps[name]
	if old != nil {
		// A backup run does not unprove a restore.
		app.RestoreProven, app.RestoreDrill = old.RestoreProven, old.RestoreDrill
		app.LastOK = old.LastOK
		if app.LastSnapshot == nil { // unless the repository has just said
			app.LastSnapshot = old.LastSnapshot
		}
	}
	if app.Snapshot != nil {
		app.LastSnapshot = app.Snapshot
		if app.Class == record.OK {
			app.LastOK = app.Snapshot
		}
	}
}

// forget empties and removes the staging directory of every app that
// no longer declares a backup: what a killed run copied there would
// otherwise stay for good. Names only are read here — the parent is
// root's — and what is inside is removed by a unit, as always.
func (x *run) forget(ctx context.Context, p *plan.Plan) {
	warn := func(format string, a ...any) {
		x.status.Warning = strings.TrimSpace(x.status.Warning + " " + record.Text(fmt.Sprintf(format, a...)))
	}
	entries, err := os.ReadDir(filepath.Join(x.cfg.StateDir, "staging"))
	if err != nil {
		warn("staging could not be looked through for what apps that no longer declare a backup left there: %v.", err)
		return
	}
	for _, e := range entries {
		name := e.Name()
		if _, declared := p.Apps[name]; declared || !backupdecl.ValidAppName(name) || !e.IsDir() {
			continue
		}
		dir := filepath.Join(x.cfg.StateDir, "staging", name)
		// Plaintext that stays is said, not shrugged at: the run goes
		// on, and the record carries it.
		if err := x.clean(ctx, name, dir); err != nil {
			warn("plaintext copies of %s, which no longer declares a backup, may still be in %s: emptying it failed: %v.", name, dir, err)
		} else if err := os.Remove(dir); err != nil { // rmdir: refuses a directory the unit left anything in
			warn("%s could not be removed: %v.", dir, err)
		}
	}
}

// UnitPattern matches the name of every unit a run, a restore or a
// drill starts, and no other unit on the box.
const UnitPattern = "hotserve_backup_*"

var unitNameRe = regexp.MustCompile(`^hotserve_backup_([a-z]+[0-9]*)(?:_([a-z0-9-]{1,63}))?_[0-9a-f]{12}\.service$`)

// ParseUnitName reads what a unit does, and to which app's data, back
// out of a name that name made. A unit of the whole run has no app.
func ParseUnitName(unit string) (role, app string, ok bool) {
	m := unitNameRe.FindStringSubmatch(unit)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// name is a unit name no app's unit and no operator's can have (the
// underscores), unique to this run.
func (x *run) name(role, app string) string {
	n := "hotserve_backup_" + role
	if app != "" {
		n += "_" + app
	}
	return n + "_" + x.nonce + ".service"
}

// start records the unit's exact name before starting it, so that a run
// killed right here leaves the next one a list of what to stop — by
// name, never by pattern.
func (x *run) start(ctx context.Context, s unit.Spec) (unit.Outcome, error) {
	s.BindsTo = x.cfg.BindsTo
	f, err := os.OpenFile(filepath.Join(x.cfg.RunDir, "units"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return unit.Outcome{}, err
	}
	_, werr := fmt.Fprintln(f, s.Name)
	if cerr := f.Close(); werr != nil || cerr != nil {
		return unit.Outcome{}, errors.Join(werr, cerr)
	}
	return x.r.Run(ctx, s)
}

// sweep stops whatever an earlier run recorded and did not live to
// stop. A unit that is already gone is the usual case.
func (x *run) sweep() error {
	if err := x.sweepUnits(); err != nil {
		return err
	}
	// Then the mounts such a run made, deepest first, and its
	// directory. The lock is held, so whatever is here is nobody's.
	mounts, err := mountsUnder(x.cfg.RunDir)
	if err != nil {
		return err
	}
	for _, m := range mounts {
		if err := unmountDetach(m); err != nil {
			return fmt.Errorf("a mount from an earlier run is still there: %s: %w", m, err)
		}
	}
	entries, err := os.ReadDir(x.cfg.RunDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			removeRunDir(filepath.Join(x.cfg.RunDir, e.Name()))
		}
	}
	return nil
}

// removeRunDir removes a run's directory: its files, and its mount
// points, which are empty directories once unmounted. It never
// descends. A mount point that is still mounted holds an app's data,
// and rmdir refuses it; it is left for the next sweep, which unmounts
// first.
func removeRunDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.Remove(filepath.Join(dir, e.Name())) // unlink or rmdir, never a walk
	}
	_ = os.Remove(dir)
}

// mountsUnder lists the mount points beneath dir, deepest first.
var mountsUnder = func(dir string) ([]string, error) {
	raw, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if f := strings.Fields(line); len(f) > 4 && strings.HasPrefix(unescapeMount(f[4]), dir+"/") {
			out = append(out, unescapeMount(f[4]))
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out, nil
}

// unescapeMount undoes mountinfo's octal escapes (space, tab, newline,
// backslash).
func unescapeMount(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}

func (x *run) sweepUnits() error {
	list := filepath.Join(x.cfg.RunDir, "units")
	raw, err := os.ReadFile(list) //nolint:gosec // root's own file under /run
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, name := range strings.Fields(string(raw)) {
		if err := x.r.Stop(name); err != nil {
			return fmt.Errorf("a unit from an earlier run is still there and could not be stopped: %w", err)
		}
	}
	return os.Remove(list)
}

func (x *run) plan(ctx context.Context) (*plan.Plan, error) {
	out := filepath.Join(x.dir, "plan.json")
	o, err := x.start(ctx, unit.Spec{
		Name: x.name("plan", ""), Description: "hotserve backup: read what each app declares",
		Argv: []string{x.cfg.Self, "plan"},
		// Not the data user: the config dir holds the apps' env files,
		// readable by its group. In its own namespaces, since it shares
		// an account with the unit that will hold the credential.
		User: backupUser, SameUIDNamespaces: true,
		Binds:      []unit.Bind{{Source: x.cfg.ConfigDir}},
		StdoutFile: out,
	})
	if err != nil {
		return nil, fmt.Errorf("reading the plan: %w", err)
	}
	if !o.OK() {
		return nil, fmt.Errorf("the Caddyfile could not be turned into a plan (exit %d); `journalctl -u %s` has the reason", o.ExitStatus, x.name("plan", ""))
	}
	raw, err := os.ReadFile(out) //nolint:gosec // written by the manager into root's own run dir
	if err != nil {
		return nil, err
	}
	return plan.Decode(raw)
}

// app backs one app up. repositoryWide reports a failure that every
// other app would meet too — and meet slowly.
//
// tags go on the snapshot beside the app's own; a restore names what it
// backs up first with preRestoreTag.
func (x *run) app(ctx context.Context, root, name string, decl *backupdecl.Config, tags ...string) (app *record.App, repositoryWide bool) {
	shared := backupdecl.SharedDir(root, name)
	app = &record.App{Looked: shared}
	fail := func(class record.Class, format string, a ...any) (*record.App, bool) {
		app.Class, app.Detail = class, fmt.Sprintf(format, a...)
		return app, false
	}

	// Plaintext copies do not outlive the run that made them, and do
	// not wait for the next run that gets this far either: staging is
	// emptied before anything else is looked at — an earlier run may
	// have died, and this one may be about to find the data gone — and
	// again at the end.
	staging, err := x.staging(name)
	if err != nil {
		return fail(record.Failed, "%v", err)
	}
	if err := x.clean(ctx, name, staging); err != nil {
		return fail(record.Failed, "emptying staging before the run: %v", err)
	}

	// Whether the data is there is decided here, on the real
	// filesystem, never from inside a unit, where a path that was not
	// bound looks exactly like a path that does not exist. And only "no
	// such file" means absent: anything else is an error.
	//
	// It is decided by opening it (pin.go): what the units are shown is
	// the directory that was found here, not whatever its name leads to
	// by the time the manager binds it.
	rootPin, err := pinRoot(root)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fail(record.Failed, "looking at the liveswap root: %v", err)
	}
	var sharedPin pin
	if err == nil {
		defer rootPin.close()
		sharedPin, err = rootPin.beneath(name + "/shared")
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// "Not deployed yet" is the one absence a run exits 0 on, so it
		// is said only of an app nothing has ever been backed up of. The
		// record remembers a snapshot; and where it has none for the app
		// — a rebuilt box, a new disk, a purge, or an app that really is
		// new — the repository is asked, every run that finds it so: it
		// remembers what the box does not, and an answer of "none" is
		// not kept, so it cannot go stale.
		old := x.prev.Apps[name]
		last := (*record.Snapshot)(nil)
		if old != nil {
			last = old.LastSnapshot
		}
		if last == nil {
			var wide bool
			if last, wide, err = x.lastInRepository(ctx, name); err != nil {
				app.Class, app.Detail = record.Failed, fmt.Sprintf("%s does not exist, and the repository could not be asked whether this app was ever backed up: %v", shared, err)
				return app, wide
			}
			app.LastSnapshot = last
		}
		if last != nil {
			return fail(record.DataMissing, "%s does not exist, and this app was last backed up on %s (snapshot %s): its data is gone, or the disk it lives on is not mounted", shared, last.Time.Format(time.RFC3339), short(last.ID))
		}
		return fail(record.Pending, "%s does not exist yet: the app has not been deployed", shared)
	case errors.Is(err, errLink):
		return fail(record.Failed, "%s is reached through a symbolic link, which a backup does not follow; liveswap itself takes a bind mount there, not a link", shared)
	case err != nil:
		return fail(record.Failed, "looking at %s: %v", shared, err)
	}
	defer sharedPin.close()
	if !sharedPin.isDir() {
		return fail(record.Failed, "%s is not a directory", shared)
	}
	// The Caddyfile's author is not root, and the upload unit reads any
	// file it is shown: a root written to make <root>/<app>/shared land
	// on something that is not an app's data is refused by whose it is.
	if uid, _, err := dataOwner(); err != nil || sharedPin.owner() != uid {
		return fail(record.Failed, "%s does not belong to the %s user, so it is not an app's data dir (owner uid %d)", shared, dataUser, sharedPin.owner())
	}

	defer func() {
		if err := x.clean(context.WithoutCancel(ctx), name, staging); err != nil {
			if app.Class == record.OK {
				app.Class = record.Incomplete
			}
			app.Detail = strings.TrimSpace(fmt.Sprintf("%s (plaintext copies are still in %s: they could not be removed: %v)", app.Detail, staging, err))
		}
	}()

	declFile := filepath.Join(x.dir, name+".plan.json")
	raw, _ := json.Marshal(decl)
	if err := os.WriteFile(declFile, raw, 0o644); err != nil { //nolint:gosec // read by units running as other users; it is the app's own declaration
		return fail(record.Failed, "%v", err)
	}
	if err := os.Chmod(declFile, 0o644); err != nil { //nolint:gosec // as above; the umask may have cut it
		return fail(record.Failed, "%v", err)
	}

	sharedSource, unmountShared, err := x.bound(sharedPin)
	if err != nil {
		return fail(record.Failed, "%s: %v", shared, err)
	}
	defer unmountShared()

	excludeFile := filepath.Join(x.dir, name+".exclude")
	if err := os.WriteFile(excludeFile, []byte(excludes(name, decl)), 0o644); err != nil { //nolint:gosec // read by the upload unit; it holds the app's own declared paths
		return fail(record.Failed, "%v", err)
	}
	if err := os.Chmod(excludeFile, 0o644); err != nil { //nolint:gosec // as above; the umask may have cut it
		return fail(record.Failed, "%v", err)
	}

	dumped := x.dump(ctx, name, sharedSource, staging, declFile, decl, app)
	if ctx.Err() != nil {
		return fail(record.Failed, "interrupted before anything was uploaded")
	}
	binds, masked, present, unpin := x.view(name, sharedPin, staging, declFile, decl, app)
	defer unpin()
	if dumped+present == 0 {
		return fail(record.Failed, "nothing that is declared could be read: %s", firstDetail(app.Items))
	}

	binds = append(binds, unit.Bind{Source: excludeFile, Dest: excludePath})
	id, class, detail, wide := x.upload(ctx, name, binds, masked, tags)
	app.Class, app.Detail = class, detail
	if id == "" {
		return app, wide
	}
	// restic has just said it wrote it: the repository holds it now.
	made := time.Now().UTC()
	app.Snapshot = &record.Snapshot{ID: id, Time: made, Seen: &made}
	if err := x.verify(ctx, name, id, app); err != nil {
		app.Class, app.Detail = record.Incomplete, fmt.Sprintf("snapshot %s was made, but what is in it could not be checked: %v", short(id), err)
		return app, false
	}
	for _, it := range app.Items {
		if !it.OK && app.Class == record.OK {
			app.Class, app.Detail = record.Incomplete, fmt.Sprintf("snapshot %s lacks %s %q: %s", short(id), it.Kind, it.Path, it.Detail)
		}
	}
	return app, false
}

// bound mounts what is pinned in the run's own directory and returns
// the path to give the manager as a bind source.
func (x *run) bound(p pin) (source string, unmount func(), err error) {
	x.mounts++
	source = filepath.Join(x.dir, fmt.Sprintf("mount-%d", x.mounts))
	unmount, err = p.mountAt(source)
	return source, unmount, err
}

// staging is the app's own directory for plaintext copies: at a fixed
// path made of a name that matched the app alphabet, under a parent
// only root can write, owned by the data user. Root makes it and never
// looks inside.
func (x *run) staging(app string) (string, error) {
	dir := filepath.Join(x.cfg.StateDir, "staging", app)
	uid, gid, err := dataOwner()
	if err != nil {
		return "", err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%s is not a directory", dir)
	}
	if err := os.Lchown(dir, uid, gid); err != nil {
		return "", err
	}
	return dir, nil
}

func (x *run) clean(ctx context.Context, app, staging string) error {
	return x.cleanAs(ctx, "clean", app, staging)
}

// cleanAs is the one unit that empties a directory of plaintext as the
// data user, under the role it plays: "clean" for a dump's staging,
// "unstage" for what a restore fetched.
func (x *run) cleanAs(ctx context.Context, role, app, staging string) error {
	o, err := x.start(ctx, unit.Spec{
		Name: x.name(role, app), Description: "hotserve backup: remove " + app + "'s plaintext copies",
		Argv: []string{x.cfg.Self, "clean"},
		User: dataUser, SameUIDNamespaces: true,
		Binds: []unit.Bind{{Source: staging, Dest: "/staging", Writable: true}},
	})
	if err != nil {
		return err
	}
	if !o.OK() {
		return fmt.Errorf("exit %d", o.ExitStatus)
	}
	return nil
}

// dump runs the dump unit and records each database; it returns how
// many copies are in staging. The unit parses what the app chose, so it
// is the app's own kind of sandbox: the data user in its own
// namespaces, no network, no credential, one app.
func (x *run) dump(ctx context.Context, app, shared, staging, declFile string, decl *backupdecl.Config, rec *record.App) int {
	if len(decl.SQLite) == 0 {
		return 0
	}
	failAll := func(format string, a ...any) int {
		for _, p := range decl.SQLite {
			rec.Items = append(rec.Items, record.Item{Kind: "sqlite", Path: p, Detail: fmt.Sprintf(format, a...)})
		}
		return 0
	}
	out := filepath.Join(x.dir, app+".dump.json")
	o, err := x.start(ctx, unit.Spec{
		Name: x.name("dump", app), Description: "hotserve backup: copy " + app + "'s databases",
		Argv: []string{x.cfg.Self, "dump"},
		User: dataUser, SameUIDNamespaces: true,
		Binds: []unit.Bind{
			// Writable: SQLite reads a WAL database only where it can
			// create the -shm beside it.
			{Source: shared, Dest: "/shared", Writable: true},
			{Source: staging, Dest: "/staging", Writable: true},
			{Source: declFile, Dest: "/plan.json"},
		},
		StdoutFile: out,
	})
	if err != nil {
		return failAll("the dump unit: %v", err)
	}
	raw, rerr := os.ReadFile(out) //nolint:gosec // written by the manager into root's own run dir
	var results []dump.Result
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if rerr != nil || dec.Decode(&results) != nil || len(results) != len(decl.SQLite) {
		return failAll("the dump unit said nothing usable (exit %d)", o.ExitStatus)
	}
	// All of it is checked before any of it is recorded: the unit
	// handled the app's bytes, and what it says is believed only where
	// it is an answer to what was asked, in words this side knows.
	for i, p := range decl.SQLite {
		if results[i].Path != p {
			return failAll("the dump unit answered about %q where %q was asked", record.Text(results[i].Path), p)
		}
		if !dump.Known(results[i].Class) {
			return failAll("the dump unit answered %q, which is not an answer", record.Text(string(results[i].Class)))
		}
	}
	n := 0
	for i, p := range decl.SQLite {
		res := results[i]
		it := record.Item{Kind: "sqlite", Path: p, OK: res.Class == dump.OK}
		if it.OK {
			n++
		} else {
			it.Detail = record.Text(string(res.Class) + ": " + res.Detail)
		}
		rec.Items = append(rec.Items, it)
	}
	return n
}

// view is what the upload unit sees: each declared files path at
// /backup/<app>/files/<path>, the copies at /backup/<app>/sqlite, and
// the declaration itself, so that a snapshot says what it is a snapshot
// of. The paths inside are the same on every box, whatever its liveswap
// root. A declared database inside a files path is masked there, with
// its sidecars: the live file is never what gets uploaded.
func (x *run) view(app string, shared pin, staging, declFile string, decl *backupdecl.Config, rec *record.App) (binds []unit.Bind, masked []string, present int, unpin func()) {
	base := "/backup/" + app
	binds = []unit.Bind{{Source: staging, Dest: base + "/sqlite"}, {Source: declFile, Dest: base + "/plan.json"}}
	var undo []func()
	unpin = func() {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}
	for _, p := range decl.Files {
		it := record.Item{Kind: "files", Path: p}
		// Pinned, not named: see pin.go. A link anywhere in a declared
		// path is refused — the upload unit reads any file it is shown,
		// so what it is shown is never for a link the app made to say.
		item, err := shared.beneath(p)
		if err != nil {
			switch {
			case errors.Is(err, fs.ErrNotExist):
				it.Detail = "missing: no such file or directory under the shared dir"
			case errors.Is(err, errLink):
				it.Detail = "a symbolic link is in the way, and a backup does not follow one: declare the real path"
			default:
				it.Detail = err.Error()
			}
			rec.Items = append(rec.Items, it)
			continue
		}
		undo = append(undo, item.close)
		if kind := item.kind(); kind != "" {
			it.Detail = "it is " + kind + ", not a file or a directory, and a backup keeps only those"
			rec.Items = append(rec.Items, it)
			continue
		}
		source, unmount, err := x.bound(item)
		if err != nil {
			it.Detail = err.Error()
			rec.Items = append(rec.Items, it)
			continue
		}
		undo = append(undo, unmount)
		it.OK = true // until the snapshot says otherwise
		rec.Items = append(rec.Items, it)
		present++
		binds = append(binds, unit.Bind{Source: source, Dest: path.Join(base, "files", p)})
		for _, db := range decl.SQLite {
			if p == "." || db == p || strings.HasPrefix(db, p+"/") {
				for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
					masked = append(masked, path.Join(base, "files", db)+suffix)
				}
			}
		}
	}
	return binds, masked, present, unpin
}

// dataOwner and backupOwner are the two accounts' ids; variables so a
// test can run where the accounts do not exist.
var (
	dataOwner   = func() (uid, gid int, err error) { return ids(dataUser) }
	backupOwner = func() (uid, gid int, err error) { return ids(backupUser) }
)

func ids(name string) (uid, gid int, err error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, err
	}
	if uid, err = strconv.Atoi(u.Uid); err != nil {
		return 0, 0, fmt.Errorf("%s's uid %q is not a number", name, u.Uid)
	}
	if gid, err = strconv.Atoi(u.Gid); err != nil {
		return 0, 0, fmt.Errorf("%s's gid %q is not a number", name, u.Gid)
	}
	return uid, gid, nil
}

// excludePath is where the upload unit finds its exclude file: outside
// /backup/<app>, so it is not itself backed up.
const excludePath = "/backup-exclude"

// excludes is the restic exclude file for one app: every declared
// database that sits inside a declared files path, and its sidecars, by
// their exact paths in the unit's view. It stands behind the masks,
// which are made when the unit starts and cover only what exists then:
// a database the app creates, or puts back under its name, while restic
// walks is not masked, and is excluded all the same. (And what is
// excluded is not in the snapshot at all, not even as the mask's empty
// file.)
//
// restic reads each line as a pattern and expands $VAR in it
// [measured], so every pattern character is escaped and every dollar
// doubled: a database called app*.db excludes itself and not appX.db.
func excludes(app string, decl *backupdecl.Config) string {
	var b strings.Builder
	for _, p := range decl.Files {
		for _, db := range decl.SQLite {
			if p != "." && db != p && !strings.HasPrefix(db, p+"/") {
				continue
			}
			for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
				b.WriteString(excludeEscaper.Replace(path.Join("/backup", app, "files", db)+suffix) + "\n")
			}
		}
	}
	return b.String()
}

var excludeEscaper = strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`, `$`, `$$`)

var snapshotRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// upload runs restic. It returns the id of the snapshot this run made —
// from this run's own summary, never "the latest" — or none.
func (x *run) upload(ctx context.Context, app string, binds []unit.Bind, masked []string, tags []string) (id string, class record.Class, detail string, repositoryWide bool) {
	out := filepath.Join(x.dir, app+".summary.json")
	argv := []string{x.cfg.Restic, "backup", "--quiet", "--json", "--retry-lock", retryLock,
		"--exclude-file", excludePath,
		"--host", "hotserve", "--tag", "hotserve", "--tag", "app:" + app}
	for _, tag := range tags {
		argv = append(argv, "--tag", tag)
	}
	o, err := x.start(ctx, unit.Spec{
		Name: x.name("upload", app), Description: "hotserve backup: upload " + app,
		Argv: append(argv, "/backup/"+app),
		// An account of its own, so that the hotserve uid — the server,
		// every app — can neither read this process's environment nor
		// signal it; and one capability, to read files that account
		// does not own, in a view that holds this app and nothing else.
		User: backupUser, Capabilities: []unit.Capability{unit.CapDACReadSearch},
		Network: true, EnvironmentFile: x.cfg.EnvFile,
		Environment:    []string{"RESTIC_CACHE_DIR=/var/cache/hotserve-backup", "HOME=/nonexistent"},
		CacheDirectory: "hotserve-backup",
		Binds:          binds, Masked: masked, StdoutFile: out,
	})
	if err != nil {
		return "", record.Failed, fmt.Sprintf("the upload unit: %v", err), false
	}
	id = summaryID(out)
	switch {
	case o.OK() && id != "":
		return id, record.OK, "", false
	case o.OK():
		// A snapshot existing proves nothing, and neither does exit 0
		// without this run's own id.
		return "", record.Failed, "restic exited 0 but its summary names no snapshot", false
	case o.ExitStatus == 3 && id != "":
		return id, record.Incomplete, "restic could not read everything it was given (exit 3); snapshot " + short(id) + " holds the rest", false
	}
	detail, repositoryWide = resticFailure(o)
	return "", record.Failed, detail + "; `journalctl -u " + x.name("upload", app) + "` has restic's own words", repositoryWide
}

// resticFailure puts restic 0.18's exit statuses into words [measured].
// 1 is "anything else", and is never called "no repository": a wrong
// storage key ends, after a quarter of an hour of retrying, in exit 1
// and a message about a missing repository.
func resticFailure(o unit.Outcome) (detail string, repositoryWide bool) {
	if o.Result != "exit-code" {
		return "restic was ended by " + o.Result, false
	}
	// 200 and up are systemd's own, for a unit it could not set up —
	// 217 the user, 226 the namespace: restic never ran.
	if o.ExitStatus >= 200 && o.ExitStatus <= 243 {
		return fmt.Sprintf("systemd could not set the unit up (status %d), so restic never ran", o.ExitStatus), false
	}
	switch o.ExitStatus {
	case 10:
		return "there is no repository at the configured location (exit 10)", true
	case 11:
		return "the repository stayed locked by something else for " + retryLock + " (exit 11)", true
	case 12:
		return "the repository password is wrong (exit 12)", true
	case 1:
		return "restic failed (exit 1): the storage could not be reached, or refused the key, or something else went wrong", true
	}
	return fmt.Sprintf("restic failed (exit %d)", o.ExitStatus), false
}

// summaryID reads the snapshot id out of restic's --json output, of
// which --quiet leaves one line, the summary.
func summaryID(file string) string {
	raw, err := os.ReadFile(file) //nolint:gosec // written by the manager into root's own run dir
	if err != nil {
		return ""
	}
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var m struct {
			MessageType string `json:"message_type"`
			SnapshotID  string `json:"snapshot_id"`
		}
		if json.Unmarshal(line, &m) == nil && m.MessageType == "summary" && snapshotRe.MatchString(m.SnapshotID) {
			return m.SnapshotID
		}
	}
	return ""
}

// verify looks in the snapshot for every declared item that was given
// to restic. restic leaves out, silently and with exit 0, a file that
// vanishes while it runs [measured], so this is what stands between a
// declared path that disappeared and a run that says ok. `ls <id>
// <dir>` lists a directory's direct children, so each item is looked
// for in a listing of its parent.
func (x *run) verify(ctx context.Context, app, id string, rec *record.App) error {
	base := "/backup/" + app
	want := map[string][]int{} // parent dir -> the items expected in it
	for i, it := range rec.Items {
		if !it.OK {
			continue
		}
		full := itemPath(base, it)
		want[path.Dir(full)] = append(want[path.Dir(full)], i)
	}
	parents := make([]string, 0, len(want))
	for p := range want {
		parents = append(parents, p)
	}
	sort.Strings(parents)
	out := filepath.Join(x.dir, app+".ls.json")
	// One listing of every parent. Given a directory, restic 0.18 lists
	// it and its direct children and no deeper [measured: 2 nodes for a
	// parent, 409 for the same snapshot with no directory given, 404
	// with --recursive], so the listing is a handful of lines however
	// large the app — TestIntegrationResticLsOfADirectoryIsNotRecursive
	// is what says so if a later restic changes its mind. --no-lock: a listing must not fail
	// because a check holds the repository. The parents are the one
	// thing on any command line here that comes from a declaration — the
	// directory part of a declared path, after /backup/<app>/ — and
	// restic takes each as a path and nothing else: they follow "--",
	// start with "/", and like every argument are never expanded.
	o, err := x.start(ctx, unit.Spec{
		Name: x.name("verify", app), Description: "hotserve backup: check " + app + "'s snapshot",
		Argv: append([]string{x.cfg.Restic, "ls", "--json", "--no-lock", "--", id}, parents...),
		User: backupUser, Network: true, EnvironmentFile: x.cfg.EnvFile,
		Environment:    []string{"RESTIC_CACHE_DIR=/var/cache/hotserve-backup", "HOME=/nonexistent"},
		CacheDirectory: "hotserve-backup", StdoutFile: out,
	})
	if err != nil {
		return err
	}
	if !o.OK() {
		return fmt.Errorf("restic ls exited %d", o.ExitStatus)
	}
	wanted := map[string]bool{}
	for _, items := range want {
		for _, i := range items {
			wanted[itemPath(base, rec.Items[i])] = true
		}
	}
	nodes, err := lsNodes(out, wanted)
	if err != nil {
		return err
	}
	for _, parent := range parents {
		for _, i := range want[parent] {
			it := &rec.Items[i]
			node, ok := nodes[itemPath(base, *it)]
			switch {
			case !ok:
				it.OK, it.Detail = false, "it is not in the snapshot: it disappeared while the backup ran"
			case it.Kind == "sqlite" && (node.Type != "file" || node.Size == 0):
				it.OK, it.Detail = false, fmt.Sprintf("in the snapshot it is a %s of %d bytes, not a database copy", record.Text(node.Type), node.Size)
			case it.Kind == "files" && node.Type != "file" && node.Type != "dir":
				// It was a file or a directory when it was pinned, and
				// the app's to replace since.
				it.OK, it.Detail = false, fmt.Sprintf("in the snapshot it is a %s, not a file or a directory", record.Text(node.Type))
			}
		}
	}
	return nil
}

// lastInRepository asks the repository for the newest snapshot of app,
// or nil when it holds none. It is what a run falls back on where the
// record holds no snapshot of an app.
func (x *run) lastInRepository(ctx context.Context, app string) (last *record.Snapshot, repositoryWide bool, err error) {
	snaps, wide, err := x.history(ctx, app)
	if err != nil || len(snaps) == 0 {
		return nil, wide, err
	}
	return &snaps[len(snaps)-1].Snapshot, false, nil
}

// A listed snapshot is one the repository holds of an app.
type listed struct {
	record.Snapshot
	// App is whose it is, by its tag; empty for a snapshot with none.
	App string
	// PreRestore: made by a restore, of what it then restored over.
	PreRestore bool
}

// newest is the newest snapshot a backup run made, or nil.
func newest(snaps []listed) *record.Snapshot {
	for i := len(snaps) - 1; i >= 0; i-- {
		if !snaps[i].PreRestore {
			return &snaps[i].Snapshot
		}
	}
	return nil
}

// history asks the repository for every snapshot of app, oldest first.
// It is what a restore on a rebuilt box starts from: the repository
// remembers what the box does not.
func (x *run) history(ctx context.Context, app string) (snaps []listed, repositoryWide bool, err error) {
	return x.snapshots(ctx, "history", app)
}

// snapshots asks the repository for the snapshots of app — of every
// app, where app is empty — oldest first. --no-lock: it reads, and a
// check that holds the repository exclusively must not fail it.
func (x *run) snapshots(ctx context.Context, role, app string) (snaps []listed, repositoryWide bool, err error) {
	out := filepath.Join(x.dir, app+"."+role+".json")
	argv, about := []string{x.cfg.Restic, "snapshots", "--json", "--no-lock", "--host", "hotserve"}, "every app"
	if app != "" {
		argv, about = append(argv, "--tag", "app:"+app), app
	}
	// An app's history fails loudly or not at all, and its words belong
	// in the journal. The listing of every app is the one answer that
	// is only believed when restic had nothing to say beside it.
	stderr := ""
	if app == "" {
		stderr = filepath.Join(x.dir, "."+role+".err")
	}
	o, err := x.start(ctx, unit.Spec{
		Name: x.name(role, app), Description: "hotserve backup: ask the repository about " + about,
		Argv: argv, StderrFile: stderr,
		User: backupUser, Network: true, EnvironmentFile: x.cfg.EnvFile,
		Environment:    []string{"RESTIC_CACHE_DIR=/var/cache/hotserve-backup", "HOME=/nonexistent"},
		CacheDirectory: "hotserve-backup", StdoutFile: out,
	})
	if err != nil {
		return nil, false, err
	}
	if !o.OK() {
		detail, wide := resticFailure(o)
		return nil, wide, errors.New(detail)
	}
	raw, err := os.ReadFile(out) //nolint:gosec // written by the manager into root's own run dir
	if err != nil {
		return nil, false, err
	}
	var said []struct {
		ID   string    `json:"id"`
		Time time.Time `json:"time"`
		Tags []string  `json:"tags"`
	}
	if err := json.Unmarshal(raw, &said); err != nil {
		return nil, false, fmt.Errorf("what restic said of its snapshots could not be read: %w", err)
	}
	asked := time.Now().UTC()
	for _, s := range said {
		if !snapshotRe.MatchString(s.ID) {
			return nil, false, fmt.Errorf("restic named a snapshot %q", record.Text(s.ID))
		}
		// The repository has just said it holds it.
		one := listed{Snapshot: record.Snapshot{ID: s.ID, Time: s.Time, Seen: &asked}}
		for _, tag := range s.Tags {
			one.PreRestore = one.PreRestore || tag == preRestoreTag
			if name, ok := strings.CutPrefix(tag, "app:"); ok {
				one.App = name
			}
		}
		snaps = append(snaps, one)
	}
	sort.SliceStable(snaps, func(i, j int) bool { return snaps[i].Time.Before(snaps[j].Time) })
	return snaps, false, nil
}

// ignoringRe is what restic says of a snapshot file it cannot load.
var ignoringRe = regexp.MustCompile(`(?m)^Ignoring "([0-9a-f]{64})"`)

// itemPath is where a declared item sits in the snapshot.
func itemPath(base string, it record.Item) string {
	if it.Kind == "files" {
		return path.Join(base, "files", it.Path)
	}
	return path.Join(base, "sqlite", it.Path)
}

type lsNode struct {
	Type string `json:"type"`
	Size int64  `json:"size"`
}

// lsNodes reads a listing a line at a time and keeps the nodes at the
// wanted paths, and nothing else: what it costs in memory does not
// depend on how long the listing is.
func lsNodes(file string, wanted map[string]bool) (map[string]lsNode, error) {
	f, err := os.Open(file) //nolint:gosec // written by the manager into root's own run dir
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	nodes := map[string]lsNode{}
	lines := bufio.NewScanner(f)
	lines.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for lines.Scan() {
		var n struct {
			StructType string `json:"struct_type"`
			Path       string `json:"path"`
			lsNode
		}
		if json.Unmarshal(lines.Bytes(), &n) == nil && n.StructType == "node" && wanted[n.Path] {
			nodes[n.Path] = n.lsNode
		}
	}
	return nodes, lines.Err()
}

func firstDetail(items []record.Item) string {
	for _, it := range items {
		if !it.OK {
			return fmt.Sprintf("%s %q: %s", it.Kind, it.Path, it.Detail)
		}
	}
	return "nothing is declared"
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func newNonce() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// lock takes the one run lock without waiting, and says who has it
// when it is taken.
func lock(file string) (unlock func(), err error) {
	f, err := os.OpenFile(file, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // a constant path under root's run dir
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder, _ := os.ReadFile(file) //nolint:gosec // as above
		f.Close()                      //nolint:errcheck,gosec // nothing was written
		return nil, fmt.Errorf("%w: %s", ErrBusy, strings.TrimSpace(string(holder)))
	}
	if err := f.Truncate(0); err == nil {
		fmt.Fprintf(f, "pid %d, since %s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339)) //nolint:errcheck // a courtesy to whoever finds the lock held
	}
	return func() { f.Close() }, nil //nolint:errcheck,gosec // closing is what releases it
}
