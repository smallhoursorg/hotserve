package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/smallhoursorg/hotserve/backups/record"
	"github.com/smallhoursorg/hotserve/backups/restore"
	"github.com/smallhoursorg/hotserve/backups/unit"
	"github.com/smallhoursorg/hotserve/liveswap/backupdecl"
	"golang.org/x/sys/unix"
)

// A restore is three units, and the drill is the same three with the
// last one told to install nothing:
//
//	fetch     restic, as its own account, with the credential and the
//	          network and no capability, writes the snapshot into an
//	          empty directory. It is in no user namespace: there restic's
//	          chown of every file fails in a way it does not overlook.
//	handover  root, with no network, seeing that directory and nothing
//	          else, gives it to the data user — by name, which is what
//	          makes a restore independent of the uid a snapshot was made
//	          under. Two capabilities: to chown, and to read directories
//	          that are another account's and closed [measured: with
//	          CAP_CHOWN alone, "cannot read directory"].
//	install   the data user, in its own namespaces, no network, no
//	          credential: reads the snapshot's own plan.json, checks
//	          every copy, and installs into the app's shared dir, which
//	          the engine has pinned and bound as it does for a backup.
//
// What was fetched is plaintext, and is removed first and last.

// RestoreOptions is what an operator asked of a restore. App and
// Snapshot are checked against their alphabets before any unit starts;
// To reaches a unit as a bind source and nothing else.
type RestoreOptions struct {
	App string
	// Snapshot is a snapshot's hex id, or the start of one, or empty for
	// the newest the repository holds of the app.
	Snapshot string
	// To is a directory that does not exist yet, to restore into instead
	// of into place. Nothing is asked and nothing is backed up first:
	// nothing is overwritten.
	To string
	// NoPreBackup skips the backup of the app an in-place restore makes
	// first. A pre-backup that does not end ok stops the restore.
	NoPreBackup bool
	// Confirm is asked once the snapshot is known, before anything is
	// backed up, fetched or changed. Nil is yes.
	Confirm func(RestoreAsk) bool
}

// RestoreAsk is what a restore is about to do.
type RestoreAsk struct {
	App      string
	Snapshot record.Snapshot
	Into     string // the shared dir, or To
	// PreBackup: what is there is backed up first. False where nothing is
	// there — a rebuilt box — or it was asked to be skipped.
	PreBackup bool
	// LastOK is the last snapshot the record says a run ended ok on,
	// when the one about to be restored is not it: see RestoreReport.
	LastOK *record.Snapshot
}

// RestoreReport is what a restore did.
type RestoreReport struct {
	App      string
	Snapshot record.Snapshot
	Into     string
	// PreBackup is the snapshot made of the app before it was restored
	// over; nil when there was nothing to back up or it was skipped.
	// PreBackupClass and PreBackupDetail say how that backup ended:
	// one that did not end ok holds less than what was there.
	PreBackup       *record.Snapshot
	PreBackupClass  record.Class
	PreBackupDetail string
	// Warning is something the restore got past and a person should know.
	Warning string
	// LastOK is set when the snapshot restored is not the last one the
	// record says a backup run ended ok on. A run that ended incomplete
	// — restic exit 3, files left out of a directory it did read — leaves
	// a snapshot every check at install passes, and a restore of it puts
	// back fewer files than the ok one would: the record is what knows,
	// and only on a box that has one.
	LastOK *record.Snapshot
	// Items are what the snapshot's own plan.json declares, each with
	// whether it was restored.
	Items []record.Item
	// Left is what is in place and not in the snapshot: never removed.
	Left     []string
	LeftMore int
	// Skipped is what is in the snapshot and is neither a file nor a
	// directory: never installed.
	Skipped     []string
	SkippedMore int
}

// ErrDeclined is returned when Confirm said no.
var ErrDeclined = errors.New("the restore was not confirmed; nothing was changed")

// snapshotArgRe is a snapshot as an operator names one: hex, whole or
// the start of one. Never "latest", and nothing restic would read as
// anything but an id.
var snapshotArgRe = regexp.MustCompile(`^[0-9a-f]{8,64}$`)

// Restore puts one snapshot of one app into place, or into a new
// directory. Every check comes before any change.
func Restore(ctx context.Context, cfg Config, r Runner, o RestoreOptions) (rep *RestoreReport, retErr error) {
	if !backupdecl.ValidAppName(o.App) {
		return nil, fmt.Errorf("%q is not an app name: lower-case letters, digits and hyphens", o.App)
	}
	if o.Snapshot != "" && !snapshotArgRe.MatchString(o.Snapshot) {
		return nil, fmt.Errorf("%q is not a snapshot: name one by its hex id, or the first eight or more characters of it (`restic snapshots --tag app:%s` lists them)", o.Snapshot, o.App)
	}
	if o.To != "" {
		if !filepath.IsAbs(o.To) || filepath.Clean(o.To) != o.To {
			return nil, fmt.Errorf("--to %q: give the directory as a clean absolute path", o.To)
		}
		// Not the engine's own directories: under the state dir a restore
		// would empty what it had just put there as a fetch's leftovers.
		// By the name, and by where the name leads — a link in its parent
		// — and again once made, by the directory that was opened.
		if err := notOwn(o.To, cfg); err != nil {
			return nil, err
		}
		// Looked at here too, before any unit: the same walk toDir makes
		// again right before it makes the directory.
		if parent, err := filepath.EvalSymlinks(filepath.Dir(o.To)); err == nil {
			if err := notOwn(filepath.Join(parent, filepath.Base(o.To)), cfg); err != nil {
				return nil, err
			}
			fd, err := rootsOwn(parent)
			if err != nil {
				return nil, fmt.Errorf("--to: %w", err)
			}
			unix.Close(fd) //nolint:errcheck,gosec // a path descriptor
		}
		if _, err := os.Lstat(o.To); err == nil {
			return nil, fmt.Errorf("--to %s exists: a restore to a directory makes the directory, so that nothing is overwritten", o.To)
		}
	}
	x, end, err := begin(ctx, cfg, r)
	if x == nil {
		return nil, err
	}
	defer end()
	if err != nil {
		return nil, err
	}
	p, err := x.plan(ctx)
	if err != nil {
		return nil, err
	}
	decl, declared := p.Apps[o.App]
	if !declared {
		return nil, fmt.Errorf("%s declares no backup in the Caddyfile, or is not an app there: a restore takes the liveswap root and the app from it", o.App)
	}
	x.sweepFetched(ctx)

	snaps, _, err := x.history(ctx, o.App)
	if err != nil {
		return nil, fmt.Errorf("asking the repository for %s's snapshots: %w", o.App, err)
	}
	snap, err := pick(o.App, snaps, o.Snapshot)
	if err != nil {
		return nil, err
	}
	rep = &RestoreReport{App: o.App, Snapshot: snap, Into: o.To}
	if old := x.prev.Apps[o.App]; old != nil && old.LastOK != nil && old.LastOK.ID != snap.ID {
		rep.LastOK = old.LastOK
	}
	defer func() {
		if rep != nil {
			rep.Warning = x.status.Warning
		}
	}()
	if o.To == "" {
		rep.Into = backupdecl.SharedDir(p.Root, o.App)
		if o.Confirm != nil {
			// Whether there is anything there to back up is looked at
			// before the question promises it.
			ask := RestoreAsk{App: o.App, Snapshot: snap, Into: rep.Into, PreBackup: !o.NoPreBackup, LastOK: rep.LastOK}
			if rootPin, err := pinRoot(p.Root); err == nil {
				if sharedPin, err := rootPin.beneath(o.App + "/shared"); err == nil {
					sharedPin.close()
				} else if errors.Is(err, fs.ErrNotExist) {
					ask.PreBackup = false
				}
				rootPin.close()
			}
			if !o.Confirm(ask) {
				return nil, ErrDeclined
			}
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	var target string
	role := "extract"
	if o.To == "" {
		role = "install"
		var release func()
		var made bool
		if target, release, made, err = x.inPlace(ctx, p.Root, o, decl, rep); err != nil {
			if made {
				x.unmakeShared(context.WithoutCancel(ctx), p.Root, o.App)
			}
			return rep, err
		}
		defer release()
		if made {
			// A shared dir this restore made and then put nothing in is
			// not left: an hourly run would take it for the app's data,
			// empty, where before it knew the data was missing. Whether
			// anything went in is rmdir's to say — it refuses a directory
			// with anything in it — not a report's.
			defer func() {
				if retErr != nil {
					x.unmakeShared(context.WithoutCancel(ctx), p.Root, o.App)
				}
			}()
		}
	} else {
		var release func()
		if target, release, err = x.toDir(o.To); err != nil {
			return nil, fmt.Errorf("--to: %w", err)
		}
		defer release()
	}

	answer, err := x.bring(ctx, o.App, snap.ID, role, target, 0)
	if answer == nil {
		if o.To != "" {
			_ = os.Remove(o.To) // rmdir: only if nothing landed in it
		}
		// With the report all the same: it names the backup made first,
		// which is what undoes whatever this did.
		return rep, err
	}
	if err != nil {
		// Restored, and something after it went wrong: both are said.
		defer func(after error) { retErr = errors.Join(retErr, after) }(err)
	}
	for _, it := range answer.Items {
		rep.Items = append(rep.Items, record.Item{Kind: it.Kind, Path: it.Path, OK: it.Class == restore.OK, Detail: itemDetail(it)})
	}
	rep.Left, rep.LeftMore, rep.SkippedMore = answer.Left, answer.LeftMore, answer.SkippedMore
	for _, s := range answer.Skipped {
		rep.Skipped = append(rep.Skipped, fmt.Sprintf("%s (%s)", s.Path, s.Kind))
	}
	return rep, unsound(answer, role)
}

// inPlace readies a restore over the app's own shared dir: the dir
// itself — made, where a rebuilt box has none — pinned and bound, and
// the app backed up first.
//
// made says the shared dir was made here, for a rebuilt box.
func (x *run) inPlace(ctx context.Context, root string, o RestoreOptions, decl *backupdecl.Config, rep *RestoreReport) (target string, release func(), made bool, err error) {
	shared := backupdecl.SharedDir(root, o.App)
	rootPin, err := pinRoot(root)
	if err != nil {
		return "", nil, false, fmt.Errorf("looking at the liveswap root: %w", err)
	}
	var undo []func()
	release = func() {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}
	undo = append(undo, rootPin.close)
	sharedPin, err := rootPin.beneath(o.App + "/shared")
	existed := err == nil
	if errors.Is(err, fs.ErrNotExist) {
		// A rebuilt box: nothing to back up first, and nothing to
		// overwrite. The dir is made by the data user, in a unit that
		// sees the liveswap root and nothing else.
		if err = x.makeShared(ctx, rootPin, o.App); err == nil {
			made = true
			sharedPin, err = rootPin.beneath(o.App + "/shared")
		}
	}
	switch {
	case errors.Is(err, errLink):
		release()
		return "", nil, made, fmt.Errorf("%s is reached through a symbolic link, which a restore does not follow; liveswap itself takes a bind mount there, not a link", shared)
	case err != nil:
		release()
		return "", nil, made, fmt.Errorf("looking at %s: %w", shared, err)
	}
	undo = append(undo, sharedPin.close)
	if uid, _, err := dataOwner(); err != nil || !sharedPin.isDir() || sharedPin.owner() != uid {
		release()
		return "", nil, made, fmt.Errorf("%s is not a directory of the %s user's, so it is not an app's data dir (owner uid %d)", shared, dataUser, sharedPin.owner())
	}

	if existed && !o.NoPreBackup {
		app, _ := x.app(ctx, root, o.App, decl, preRestoreTag)
		x.status.Apps[o.App] = app
		x.carryLastOK(o.App)
		// A backup of what is about to be restored over is never "the
		// last one a backup run ended ok on": that is what a later
		// restore's question compares against, and this holds, as often
		// as not, the damage. It is the last snapshot, so the data going
		// missing is still never "not deployed yet".
		if old := x.prev.Apps[o.App]; old != nil {
			app.LastOK = old.LastOK
		} else {
			app.LastOK = nil
		}
		rep.PreBackup, rep.PreBackupClass, rep.PreBackupDetail = app.Snapshot, app.Class, app.Detail // whether or not it ended ok: it is in the repository
		// The record: this app under this restore's own time, every other
		// app as the last run left it — never this app's result under
		// the last run's date.
		st := *x.status
		st.Root, st.Finished = root, time.Now().UTC()
		st.Apps = map[string]*record.App{}
		for name, old := range x.prev.Apps {
			st.Apps[name] = old
		}
		st.Apps[o.App] = app
		if err := record.Write(filepath.Join(x.cfg.StateDir, "status.json"), &st); err != nil {
			release()
			return "", nil, made, err
		}
		if app.Class != record.OK {
			release()
			return "", nil, made, fmt.Errorf("the backup a restore makes first did not end ok (%s: %s), so nothing was restored over; --no-pre-backup restores without one, and what is in place now is then not kept anywhere", app.Class, app.Detail)
		}
	}
	if ctx.Err() != nil {
		release()
		return "", nil, made, ctx.Err()
	}
	target, unmount, err := x.bound(sharedPin)
	if err != nil {
		release()
		return "", nil, made, fmt.Errorf("%s: %w", shared, err)
	}
	undo = append(undo, unmount)
	return target, release, made, nil
}

// unmakeShared takes away what makeShared made, where nothing was put
// in it — rmdir, which refuses a directory with anything in it, as the
// data user in the same view. What it cannot take away it leaves.
func (x *run) unmakeShared(ctx context.Context, root, app string) {
	rootPin, err := pinRoot(root)
	if err != nil {
		return
	}
	defer rootPin.close()
	source, unmount, err := x.bound(rootPin)
	if err != nil {
		return
	}
	defer unmount()
	_, _ = x.start(ctx, unit.Spec{
		Name: x.name("unmake", app), Description: "hotserve backup: take away " + app + "'s empty shared dir",
		Argv: []string{"/usr/bin/rmdir", "/liveswap/" + app + "/shared", "/liveswap/" + app},
		User: dataUser, SameUIDNamespaces: true,
		Binds: []unit.Bind{{Source: source, Dest: "/liveswap", Writable: true}},
	})
}

// rootsOwn walks the resolved directory dir from /, one component at a
// time without following a link — none should be left, and one that has
// appeared since is the swap this refuses — and requires each to belong
// to root and to be writable by nobody else. It returns a descriptor
// for dir, which is then the only way it is reached.
func rootsOwn(dir string) (int, error) {
	fd, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	sofar := "/"
	check := func(fd int, at string) error {
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return &os.PathError{Op: "stat", Path: at, Err: err}
		}
		switch {
		case st.Mode&unix.S_IFMT != unix.S_IFDIR:
			return fmt.Errorf("%s is not a directory", at)
		case st.Uid != 0:
			return fmt.Errorf("%s belongs to uid %d, not root. A restore is made where the name leads, and every directory on the way has to be root's own and writable by nobody else, or someone else could have put a link there first: /root, /srv, /var/backups, or a root-owned directory of your own", at, st.Uid)
		case st.Mode&0o022 != 0:
			return fmt.Errorf("%s can be written by others (mode %04o). A restore is made where the name leads, and every directory on the way has to be root's own and writable by nobody else, or someone else could have put a link there first: not /tmp — /root, /srv, /var/backups, or a root-owned directory of your own", at, st.Mode&0o7777)
		}
		return nil
	}
	if err := check(fd, sofar); err != nil {
		unix.Close(fd) //nolint:errcheck,gosec // a path descriptor
		return -1, err
	}
	rel := strings.TrimPrefix(dir, "/")
	if rel == "" {
		return fd, nil
	}
	for _, part := range strings.Split(rel, "/") {
		next, err := unix.Openat(fd, part, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd) //nolint:errcheck,gosec // a path descriptor
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return -1, fmt.Errorf("%s is a link now, and was not when it was looked at: it is not held still, and the restore will not follow it", filepath.Join(sofar, part))
		}
		if err != nil {
			return -1, &os.PathError{Op: "open", Path: filepath.Join(sofar, part), Err: err}
		}
		sofar = filepath.Join(sofar, part)
		if err := check(next, sofar); err != nil {
			unix.Close(next) //nolint:errcheck,gosec // a path descriptor
			return -1, err
		}
		fd = next
	}
	return fd, nil
}

// notOwn refuses a path under the engine's own directories.
func notOwn(dir string, cfg Config) error {
	owns := []string{cfg.StateDir, cfg.RunDir}
	for _, own := range []string{cfg.StateDir, cfg.RunDir} {
		if real, err := filepath.EvalSymlinks(own); err == nil && real != own {
			owns = append(owns, real) // /var/lib an alias of a data disk, say
		}
	}
	for _, own := range owns {
		if dir == own || strings.HasPrefix(dir, own+"/") {
			return fmt.Errorf("--to %s is under %s, which is the backup engine's own", dir, own)
		}
	}
	return nil
}

// toDir makes the directory a restore --to goes into, and binds it for
// the unit. Its parent may be anyone's — /tmp — so from the moment it is
// made it is held by descriptor: what is chmod'ed, chowned and bound is
// the directory that was made, whatever its name leads to by then.
func (x *run) toDir(dir string) (source string, release func(), err error) {
	uid, gid, err := dataOwner()
	if err != nil {
		return "", nil, err
	}
	// The parent may be anyone's — /tmp — and a name in it is theirs to
	// swap for a link between a look and a mkdir that walks it as root.
	// So the parent is held by descriptor: what the operator's name led
	// to when it was resolved, and nothing else, is where the directory
	// is made, relative to that descriptor.
	parent, base := filepath.Split(dir)
	parent = filepath.Clean(parent)
	named, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", nil, &os.PathError{Op: "resolve", Path: parent, Err: err}
	}
	// And root's own, every step of the way: a name the operator typed
	// can only mean what root meant by it if nobody else could have put
	// a link at any step — before the command was typed, which no look
	// at the time can tell. So each directory on the way, from /, has to
	// belong to root and be writable by nobody else: not /tmp, which
	// everyone may add to, and nothing an app's user owns.
	pfd, err := rootsOwn(named)
	if err != nil {
		return "", nil, err
	}
	defer unix.Close(pfd) //nolint:errcheck // a path descriptor
	if err := notOwn(filepath.Join(named, base), x.cfg); err != nil {
		return "", nil, err
	}
	if err := unix.Mkdirat(pfd, base, 0o700); err != nil {
		return "", nil, &os.PathError{Op: "mkdir", Path: dir, Err: err}
	}
	// Until this has worked, what was made is taken away again: an empty
	// --to left by a bind that failed would refuse every try after.
	undo := func() { _ = unix.Unlinkat(pfd, base, unix.AT_REMOVEDIR) }
	fd, err := unix.Openat(pfd, base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		undo()
		return "", nil, &os.PathError{Op: "open", Path: dir, Err: err}
	}
	made := pin{fd}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		made.close()
		undo()
		return "", nil, err
	}
	if int(st.Uid) != os.Geteuid() {
		made.close()
		return "", nil, fmt.Errorf("%s is not the directory that was just made (owner uid %d)", dir, st.Uid)
	}
	if err := errors.Join(unix.Fchmod(fd, 0o700), unix.Fchown(fd, uid, gid)); err != nil {
		made.close()
		undo()
		return "", nil, err
	}
	source, unmount, err := x.bound(made)
	if err != nil {
		made.close()
		undo()
		return "", nil, err
	}
	return source, func() { unmount(); made.close() }, nil
}

// makeShared makes <root>/<app>/shared as the data user. The app's name
// is on the command line: it matched the app alphabet.
func (x *run) makeShared(ctx context.Context, rootPin pin, app string) error {
	source, unmount, err := x.bound(rootPin)
	if err != nil {
		return err
	}
	defer unmount()
	o, err := x.start(ctx, unit.Spec{
		Name: x.name("mkshared", app), Description: "hotserve backup: make " + app + "'s shared dir",
		Argv: []string{"/usr/bin/install", "-d", "-m", "0750", "/liveswap/" + app, "/liveswap/" + app + "/shared"},
		User: dataUser, SameUIDNamespaces: true,
		Binds: []unit.Bind{{Source: source, Dest: "/liveswap", Writable: true}},
	})
	if err != nil {
		return err
	}
	if !o.OK() {
		return fmt.Errorf("the shared dir could not be made (exit %d)", o.ExitStatus)
	}
	return nil
}

// pick is the snapshot a restore was asked for, among the app's own.
//
// Asked for none, it is the newest a backup run made — never one a
// restore made of what it was restoring over, which a second try at a
// failed restore would otherwise put back. That one is restored by name.
func pick(app string, snaps []listed, asked string) (record.Snapshot, error) {
	if len(snaps) == 0 {
		return record.Snapshot{}, fmt.Errorf("the repository holds no snapshot of %s", app)
	}
	if asked == "" {
		if last := newest(snaps); last != nil {
			return *last, nil
		}
		return record.Snapshot{}, fmt.Errorf("the repository holds no snapshot of %s that a backup run made, only what restores backed up first: name one with --snapshot", app)
	}
	var found []record.Snapshot
	for _, s := range snaps {
		if strings.HasPrefix(s.ID, asked) {
			found = append(found, s.Snapshot)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return record.Snapshot{}, fmt.Errorf("%s is not a snapshot of %s: the repository holds %d of it, the newest %s", asked, app, len(snaps), short(snaps[len(snaps)-1].ID))
	}
	return record.Snapshot{}, fmt.Errorf("%s is the start of %d snapshots of %s; give more of it", asked, len(found), app)
}

// bring fetches a snapshot, hands it over, and has it checked — and, as
// role says, installed into target. What was fetched is removed first
// and last, whatever happens in between, by a unit the caller's
// interrupt does not reach.
//
// size is the snapshot's restore size where the caller has asked
// already, or 0 to ask here.
func (x *run) bring(ctx context.Context, app, id, role, target string, size uint64) (*restore.Answer, error) {
	fetched, err := x.fetchedDir(app)
	if err != nil {
		return nil, err
	}
	if err := x.unstage(ctx, app, fetched); err != nil {
		return nil, fmt.Errorf("emptying %s before the restore: %w", fetched, err)
	}
	var answer *restore.Answer
	err = func() error {
		if size == 0 {
			if size, err = x.snapshotSize(ctx, app, id); err != nil {
				return err
			}
		}
		if err := room(size, fetched, target, x.cfg.StateDir); err != nil {
			return err
		}
		if err := x.fetch(ctx, app, id, fetched); err != nil {
			return err
		}
		if err := x.handover(ctx, app, fetched); err != nil {
			return err
		}
		// An interrupt that came while a unit was ending is seen here,
		// before the unit that changes things starts.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		answer, err = x.settle(ctx, app, role, fetched, target)
		if err != nil && role != "check" && !errors.As(err, new(refusedError)) {
			// The unit changes things; one that ended with no usable
			// answer — interrupted, killed, out of memory — may have
			// begun. That is said, whatever the cause.
			return fmt.Errorf("the install ended with no usable answer, so what was being restored into may be partly restored; the same restore, run again, goes over it: %w", err)
		}
		return err
	}()
	if cerr := x.unstage(context.WithoutCancel(ctx), app, fetched); cerr != nil {
		err = errors.Join(err, fmt.Errorf("plaintext of %s is still in %s: it could not be removed: %w", app, fetched, cerr))
	}
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	return answer, err
}

// fetchedDir is where a snapshot of app lands: a fixed path made of a
// name that matched the app alphabet, under a parent only root can
// reach. Root makes it and never looks inside.
func (x *run) fetchedDir(app string) (string, error) {
	dir := filepath.Join(x.cfg.StateDir, "restore", app)
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	if st, err := os.Lstat(dir); err != nil {
		return "", err
	} else if !st.IsDir() {
		return "", fmt.Errorf("%s is not a directory", dir)
	}
	return dir, nil
}

// unstage empties dir. What is in it is the backup account's, the data
// user's, or — after a restore that was killed — some of each, so
// whatever is there is first given to the data user, whose unit then
// removes it. Names only are read here.
func (x *run) unstage(ctx context.Context, app, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		if err := x.handover(ctx, app, dir); err != nil {
			return err
		}
	}
	uid, gid, err := dataOwner()
	if err != nil {
		return err
	}
	if err := os.Lchown(dir, uid, gid); err != nil {
		return err
	}
	return x.cleanAs(ctx, "unstage", app, dir)
}

// sweepFetched empties what a restore or a drill that was killed left
// of any app, and says so in the record when it cannot.
func (x *run) sweepFetched(ctx context.Context) {
	base := filepath.Join(x.cfg.StateDir, "restore")
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range entries {
		dir := filepath.Join(base, e.Name())
		inside, err := os.ReadDir(dir)
		if !e.IsDir() || !backupdecl.ValidAppName(e.Name()) || err != nil || len(inside) == 0 {
			continue
		}
		if err := x.unstage(ctx, e.Name(), dir); err != nil {
			x.status.Warning = strings.TrimSpace(x.status.Warning + " " + record.Text(fmt.Sprintf("plaintext a restore of %s left may still be in %s: emptying it failed: %v.", e.Name(), dir, err)))
		}
	}
}

// errNoRoom is a fetch that was not begun because it could not fit.
var errNoRoom = errors.New("no room")

// freeUnder is how many bytes an unprivileged writer can still put on
// the filesystem dir is on; a variable so a test can say.
var freeUnder = func(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, &os.PathError{Op: "statfs", Path: dir, Err: err}
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// snapshotSize asks the repository how large the snapshot is once
// restored: restic's own figure [measured: equal to what the restored
// tree takes, a file with two names counted once]. Asked about an id the
// repository does not hold, restic says zero bytes of zero snapshots and
// exits 0 [measured]: only one snapshot, counted, is an answer.
func (x *run) snapshotSize(ctx context.Context, app, id string) (uint64, error) {
	out := filepath.Join(x.dir, app+".size.json")
	o, err := x.start(ctx, unit.Spec{
		Name: x.name("size", app), Description: "hotserve backup: ask how large a snapshot of " + app + " is",
		Argv: []string{x.cfg.Restic, "stats", "--quiet", "--json", "--no-lock", "--mode", "restore-size", id},
		User: backupUser, Network: true, EnvironmentFile: x.cfg.EnvFile,
		Environment:    []string{"RESTIC_CACHE_DIR=/var/cache/hotserve-backup", "HOME=/nonexistent"},
		CacheDirectory: "hotserve-backup", StdoutFile: out,
	})
	if err != nil {
		return 0, fmt.Errorf("the size unit: %w", err)
	}
	if !o.OK() {
		detail, wide := resticFailure(o)
		err := fmt.Errorf("asking how large snapshot %s is: %s", short(id), detail)
		if wide {
			return 0, repositoryWideError{err}
		}
		return 0, err
	}
	raw, err := os.ReadFile(out) //nolint:gosec // written by the manager into root's own run dir
	if err != nil {
		return 0, err
	}
	var said struct {
		TotalSize *uint64 `json:"total_size"`
		Snapshots int     `json:"snapshots_count"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &said); err != nil || said.TotalSize == nil || said.Snapshots != 1 {
		return 0, fmt.Errorf("restic did not say how large snapshot %s is (it counted %d snapshots), so whether a fetch of it fits is not known", short(id), said.Snapshots)
	}
	return *said.TotalSize, nil
}

// sameFilesystem reports whether two paths are on one filesystem; a
// variable so a test can say.
var sameFilesystem = func(a, b string) (bool, error) {
	var sa, sb unix.Stat_t
	if err := unix.Stat(a, &sa); err != nil {
		return false, &os.PathError{Op: "stat", Path: a, Err: err}
	}
	if err := unix.Stat(b, &sb); err != nil {
		return false, &os.PathError{Op: "stat", Path: b, Err: err}
	}
	if sa.Dev == sb.Dev {
		return true, nil
	}
	// One pool under two device numbers — btrfs subvolumes — shares its
	// room all the same; the filesystem id says so where st_dev does not.
	var fa, fb unix.Statfs_t
	if unix.Statfs(a, &fa) != nil || unix.Statfs(b, &fb) != nil {
		return false, nil
	}
	return fa.Fsid == fb.Fsid && fa.Type == fb.Type, nil
}

// room refuses a fetch that could not fit where it lands — and, where
// something is then installed, an install that could not fit beside it.
// A fetch is the whole app in plaintext under the state dir, kept until
// the restore or the drill is over; an install writes the app a second
// time, and on the usual box that is the same disk, the one the live
// apps write to. So on one filesystem a restore needs twice the
// snapshot free, on two filesystems the snapshot on each; a drill,
// which installs nothing, the fetch alone. Against what is free now,
// and no more: what an install overwrites is freed as it goes, so twice
// is the worst case, not the usual one — but it is the one that fills
// the disk half way through.
func room(size uint64, fetched, target, stateDir string) error {
	free, err := freeUnder(fetched)
	if err != nil {
		return err
	}
	if size > free {
		return fmt.Errorf("%w for the fetch: it needs %s, and %s has %s free. A fetch is the whole app, in plaintext, until the restore or the drill is over", errNoRoom, mib(size), stateDir, mib(free))
	}
	if target == "" {
		return nil
	}
	same, err := sameFilesystem(fetched, target)
	if err != nil {
		return err
	}
	if same {
		if 2*size > free {
			return fmt.Errorf("%w for the restore: it needs %s — twice %s, since the fetch and the install land on one filesystem, and the fetch stays until the install is done — and %s has %s free. Restore --to a directory on another disk, or make room", errNoRoom, mib(2*size), mib(size), stateDir, mib(free))
		}
		return nil
	}
	freeThere, err := freeUnder(target)
	if err != nil {
		return err
	}
	if size > freeThere {
		return fmt.Errorf("%w for the install: it needs %s where it goes, which has %s free", errNoRoom, mib(size), mib(freeThere))
	}
	return nil
}

// mib is a size in words, rounded up: what is needed is never understated.
func mib(n uint64) string { return fmt.Sprintf("%d MiB", (n+1<<20-1)>>20) }

func (x *run) fetch(ctx context.Context, app, id, fetched string) error {
	uid, gid, err := backupOwner()
	if err != nil {
		return err
	}
	if err := errors.Join(os.Chmod(fetched, 0o700), os.Lchown(fetched, uid, gid)); err != nil { //nolint:gosec // a directory, and the backup account's alone
		return err
	}
	out := filepath.Join(x.dir, app+".fetch.json")
	// <id>:<path>, never --include: asked for a path the snapshot does
	// not hold, the first exits 1 and the second exits 0 having restored
	// nothing [measured].
	o, err := x.start(ctx, unit.Spec{
		Name: x.name("fetch", app), Description: "hotserve backup: fetch a snapshot of " + app,
		Argv: []string{x.cfg.Restic, "restore", "--quiet", "--json", "--retry-lock", retryLock, id + ":/backup/" + app, "--target", "/restore"},
		User: backupUser, Network: true, EnvironmentFile: x.cfg.EnvFile,
		Environment:    []string{"RESTIC_CACHE_DIR=/var/cache/hotserve-backup", "HOME=/nonexistent"},
		CacheDirectory: "hotserve-backup",
		Binds:          []unit.Bind{{Source: fetched, Dest: "/restore", Writable: true}},
		StdoutFile:     out,
	})
	if err != nil {
		return fmt.Errorf("the fetch unit: %w", err)
	}
	if !o.OK() {
		detail, wide := resticFailure(o)
		if o.Result == "exit-code" && o.ExitStatus == 1 {
			// Of a restore, 1 is also every error that is this fetch's own
			// — no room on the disk, above all: a fetch is the whole app,
			// under the state dir — and the repository has just answered.
			detail, wide = fmt.Sprintf("restic could not restore all of it (exit 1): %s may be out of room — a fetch needs as much there as the app's data takes — or the storage stopped answering", x.cfg.StateDir), false
		}
		err := fmt.Errorf("fetching snapshot %s: %s; `journalctl -u %s` has restic's own words", short(id), detail, x.name("fetch", app))
		if wide {
			return repositoryWideError{err}
		}
		return err
	}
	return fetchedAll(out, id)
}

// refusedError is a unit's own word that it did nothing, in an answer
// it could give — as against one that gave none.
type refusedError struct{ error }

func (e refusedError) Unwrap() error { return e.error }

// repositoryWideError is a failure every other app would meet too, and
// meet as slowly.
type repositoryWideError struct{ error }

func (e repositoryWideError) Unwrap() error { return e.error }

// fetchedAll reads restic's closing summary. Exit 0 is not a restore:
// the summary has to be there, and say that something was restored, and
// that it was everything. restic leaves a count that is zero out of the
// summary altogether [measured].
func fetchedAll(file, id string) error {
	raw, err := os.ReadFile(file) //nolint:gosec // written by the manager into root's own run dir
	if err != nil {
		return err
	}
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var m struct {
			MessageType   string `json:"message_type"`
			TotalFiles    int64  `json:"total_files"`
			FilesRestored int64  `json:"files_restored"`
		}
		if json.Unmarshal(line, &m) != nil || m.MessageType != "summary" {
			continue
		}
		if m.FilesRestored == 0 {
			return fmt.Errorf("restic exited 0 and restored nothing of snapshot %s", short(id))
		}
		if m.FilesRestored != m.TotalFiles {
			return fmt.Errorf("restic exited 0 having restored %d of the %d entries of snapshot %s", m.FilesRestored, m.TotalFiles, short(id))
		}
		return nil
	}
	return fmt.Errorf("restic exited 0 and its output holds no summary, so what it restored of snapshot %s is not known", short(id))
}

func (x *run) handover(ctx context.Context, app, fetched string) error {
	o, err := x.start(ctx, unit.Spec{
		Name: x.name("handover", app), Description: "hotserve backup: hand what was fetched of " + app + " to the " + dataUser + " user",
		// -h: a link in the snapshot is chowned itself, never what it
		// points at. "user:" is that user and its own group.
		Argv:   []string{"/usr/bin/chown", "-hR", dataUser + ":", "/restore"},
		AsRoot: true, Capabilities: []unit.Capability{unit.CapChown, unit.CapDACReadSearch},
		Binds: []unit.Bind{{Source: fetched, Dest: "/restore", Writable: true}},
	})
	if err != nil {
		return fmt.Errorf("the hand-over unit: %w", err)
	}
	if !o.OK() {
		return fmt.Errorf("what was fetched could not be given to the %s user (exit %d)", dataUser, o.ExitStatus)
	}
	return nil
}

// settle runs the unit that reads the snapshot. It handled bytes that
// came through the repository, so what it says is believed only where it
// is an answer about a valid declaration, item for item, in words this
// side knows.
func (x *run) settle(ctx context.Context, app, role, fetched, target string) (*restore.Answer, error) {
	// /restore is writable: what was fetched is this run's own scratch,
	// and a copy the app stored closed to its owner is opened to be read
	// (a backup reads with a capability, a restore as the data user).
	binds := []unit.Bind{{Source: fetched, Dest: "/restore", Writable: true}}
	if target != "" {
		binds = append(binds, unit.Bind{Source: target, Dest: "/target", Writable: true})
	}
	out := filepath.Join(x.dir, app+"."+role+".json")
	o, err := x.start(ctx, unit.Spec{
		Name: x.name(role, app), Description: "hotserve backup: " + role + " a snapshot of " + app,
		Argv: []string{x.cfg.Self, role},
		User: dataUser, SameUIDNamespaces: true,
		Binds: binds, StdoutFile: out,
	})
	if err != nil {
		return nil, fmt.Errorf("the %s unit: %w", role, err)
	}
	raw, rerr := os.ReadFile(out) //nolint:gosec // written by the manager into root's own run dir
	answer := new(restore.Answer)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if rerr != nil || dec.Decode(answer) != nil {
		return nil, fmt.Errorf("the %s unit said nothing usable (exit %d); `journalctl -u %s` may say why", role, o.ExitStatus, x.name(role, app))
	}
	if answer.Error != "" {
		// The unit's own word that it did nothing: a plan.json it could
		// not take. Not a failure of the install, and never "partly".
		return nil, refusedError{errors.New(record.Text(answer.Error))}
	}
	if answer.Plan == nil || answer.Plan.Validate() != nil {
		return nil, fmt.Errorf("the %s unit answered about a plan that is not a declaration", role)
	}
	want := make([]string, 0, len(answer.Plan.SQLite)+len(answer.Plan.Files))
	for _, p := range answer.Plan.SQLite {
		want = append(want, "sqlite "+p)
	}
	for _, p := range answer.Plan.Files {
		want = append(want, "files "+p)
	}
	if len(answer.Items) != len(want) {
		return nil, fmt.Errorf("the %s unit answered about %d items of a plan that declares %d", role, len(answer.Items), len(want))
	}
	for i, it := range answer.Items {
		if it.Kind+" "+it.Path != want[i] {
			return nil, fmt.Errorf("the %s unit answered about %q where %q was asked", role, record.Text(it.Kind+" "+it.Path), want[i])
		}
		if !restore.Known(it.Class) {
			return nil, fmt.Errorf("the %s unit answered %q, which is not an answer", role, record.Text(string(it.Class)))
		}
	}
	if role == "check" && (answer.Changed || len(answer.Left) > 0 || answer.LeftMore > 0) {
		return nil, fmt.Errorf("the check unit says it changed something, or lists what is in place, and it was shown no place")
	}
	for i := range answer.Left {
		answer.Left[i] = record.Text(answer.Left[i])
	}
	for i := range answer.Skipped {
		answer.Skipped[i].Path, answer.Skipped[i].Kind = record.Text(answer.Skipped[i].Path), record.Text(answer.Skipped[i].Kind)
	}
	return answer, nil
}

func itemDetail(it restore.Item) string {
	if it.Class == restore.OK {
		return ""
	}
	return record.Text(string(it.Class) + ": " + it.Detail)
}

// unsound is the error for an answer in which not every item is ok,
// and says what that left behind — "nothing was changed" only where the
// unit never began to install.
func unsound(answer *restore.Answer, role string) error {
	var bad []string
	sound := true
	for _, it := range answer.Items {
		sound = sound && it.Class == restore.OK
		if it.Class != restore.OK && it.Class != restore.HeldBack {
			bad = append(bad, fmt.Sprintf("%s %q: %s", it.Kind, it.Path, itemDetail(it)))
		}
	}
	if sound {
		return nil
	}
	if len(bad) == 0 {
		bad = []string{"the unit held every item back and said of none why"}
	}
	sort.Strings(bad)
	what := "nothing was changed"
	switch {
	case role == "check":
		what = "a restore of this snapshot would be refused"
	case role == "extract":
		what = "what is sound was restored, and nothing else"
	case answer.Changed:
		what = "the restore had begun, so the app's data is partly restored: what is listed as restored is in place, and the same restore, run again, goes over it"
	}
	return fmt.Errorf("%s; %s", strings.Join(bad, "; "), what)
}

// drillApp fetches, hands over and checks one snapshot of one app,
// installing nothing — no unit of it sees the app's live data — and
// writes what it proved, or why it proved nothing, into rec. What was
// proven before stays: a drill that fails does not unprove the last one
// that did not.
//
// size is the snapshot's restore size where known, else 0.
func (x *run) drillApp(ctx context.Context, app string, snap record.Snapshot, size uint64, rec *record.App) (repositoryWide bool) {
	answer, err := x.bring(ctx, app, snap.ID, "check", "", size)
	if err == nil {
		err = unsound(answer, "check")
	}
	if ctx.Err() != nil {
		return false // interrupted: nothing was found out, so nothing is written
	}
	if err != nil {
		rec.RestoreDrill = &record.Drill{Snapshot: snap, Time: time.Now().UTC(), Detail: record.Text(err.Error())}
		return errors.As(err, new(repositoryWideError))
	}
	// Fetched whole, a moment ago: the repository holds it.
	proven := time.Now().UTC()
	snap.Seen = &proven
	// Under every name the record has for it: a last good backup that a
	// listing once missed is not still "gone" beside the proof that it
	// is there.
	for _, named := range []*record.Snapshot{rec.Snapshot, rec.LastOK, rec.LastSnapshot} {
		if named != nil && named.ID == snap.ID {
			named.Seen = &proven
		}
	}
	rec.RestoreProven, rec.RestoreDrill = &record.Drill{Snapshot: snap, Time: proven}, nil
	return false
}

// firstDrillLimit is the largest snapshot a run fetches back to prove
// its own first backup: above it, the whole app back in plaintext on a
// timer — egress, the app's size in free disk, the lock held — is an
// operator's deliberate act, `hotserve-backup drill`, not the hour's.
const firstDrillLimit = 1 << 30

// firstDrill is a run's drill of an app's first good backup. It says
// how large that is before fetching it, and leaves one above
// firstDrillLimit to the drill, recorded as not proven and why.
func (x *run) firstDrill(ctx context.Context, app string, rec *record.App) (repositoryWide bool) {
	size, err := x.snapshotSize(ctx, app, rec.Snapshot.ID)
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		rec.RestoreDrill = &record.Drill{Snapshot: *rec.Snapshot, Time: time.Now().UTC(), Detail: record.Text(err.Error())}
		return errors.As(err, new(repositoryWideError))
	}
	if size > firstDrillLimit {
		rec.RestoreDrill = &record.Drill{Snapshot: *rec.Snapshot, Time: time.Now().UTC(),
			Detail: fmt.Sprintf("not drilled by the run: the snapshot is %s restored, above the %s a run fetches back on its own; `hotserve-backup drill` proves it", mib(size), mib(firstDrillLimit))}
		return false
	}
	return x.drillApp(ctx, app, *rec.Snapshot, size, rec)
}

// Drill proves, for every app the repository holds a snapshot of, that
// the newest can be fetched, handed over and read whole, and writes what
// it found into the record beside what the last backup run found.
func Drill(ctx context.Context, cfg Config, r Runner) (*record.Status, error) {
	x, end, err := begin(ctx, cfg, r)
	if x == nil {
		return nil, err
	}
	defer end()
	if err != nil {
		return nil, err
	}
	st := x.prev
	st.LastDrill = &record.Drill{Time: time.Now().UTC()}
	p, err := x.plan(ctx)
	if err != nil {
		// Said in the record, so that a drill failing here week after
		// week does not pass for "proven" ageing quietly.
		st.LastDrill.Detail = record.Text(err.Error())
		return st, errors.Join(err, record.Write(filepath.Join(cfg.StateDir, "status.json"), st))
	}
	x.sweepFetched(ctx)
	// An app that has left the plan has nothing left to prove.
	for name, rec := range st.Apps {
		if _, declared := p.Apps[name]; !declared && rec != nil {
			rec.RestoreDrill = nil
		}
	}
	var refused string // set once the repository refuses for a reason every app shares
	for _, name := range p.Names() {
		if ctx.Err() != nil {
			break
		}
		rec := st.Apps[name]
		if rec == nil {
			rec = &record.App{Class: record.NotRun, Detail: "no backup run has reached this app since this record was begun", Looked: backupdecl.SharedDir(p.Root, name)}
		}
		if refused != "" {
			rec.RestoreDrill = &record.Drill{Time: time.Now().UTC(), Detail: refused}
			st.Apps[name] = rec
			continue
		}
		snaps, wide, err := x.history(ctx, name)
		switch {
		case err != nil:
			rec.RestoreDrill = &record.Drill{Time: time.Now().UTC(), Detail: record.Text("the repository could not be asked for this app's snapshots: " + err.Error())}
			if wide {
				refused = rec.RestoreDrill.Detail
			}
		case newest(snaps) == nil:
			// Nothing to prove. What an earlier drill could not ask is
			// answered now, and is not left standing in the record.
			if st.Apps[name] != nil {
				rec.RestoreDrill = nil
			}
			continue
		default:
			if x.drillApp(ctx, name, *newest(snaps), 0, rec) {
				refused = rec.RestoreDrill.Detail
			}
		}
		st.Apps[name] = rec
	}
	st.Warning = strings.TrimSpace(st.Warning + " " + x.status.Warning)
	// What was finished is written, an interrupt or not: each verdict is
	// an app's own, and the app the interrupt landed on has none.
	if err := record.Write(filepath.Join(cfg.StateDir, "status.json"), st); err != nil {
		return st, err
	}
	return st, ctx.Err()
}
