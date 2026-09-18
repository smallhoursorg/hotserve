package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"strconv"
	"strings"
)

// LaunchOptions is what one app's transient unit needs beyond the app
// itself.
type LaunchOptions struct {
	// Self is the hotserve binary the unit re-executes (this one, so
	// an upgrade never leaves a job running last version's code).
	Self string
	// StagingRoot holds one dir per app; only this app's is bound in.
	StagingRoot string
	// EnvFile carries the repository and its credentials. systemd
	// reads it as root, before dropping to User=, so the file stays
	// 0600 root-owned and the job never has to be able to read it.
	EnvFile string
	// User owns liveswap's data; the job reads it as that user rather
	// than as root.
	User string
	// RepositoryPath is set only for a filesystem repository, which
	// has to be inside the job's view to be written to. Empty for a
	// remote one.
	RepositoryPath string
}

// unitName keeps one unit per app so `systemctl status` and the
// journal are per app, and two apps can never share a cgroup.
func unitName(app string) string { return "hotserve-backup-" + app }

// LaunchArgs is the whole systemd-run invocation for one app's job.
//
// The property set is the one the spikes verified, and it is
// load-bearing as a whole: dropping parts of it (the base view, the
// private namespaces) makes systemd fail to set the user at all
// (217/USER) rather than run with a weaker sandbox. The view holds
// exactly three writable things — this app's staging dir — and one
// read-only thing that matters: this app's shared dir. hotserve's own
// state, every other app, and the credentials file are simply absent.
//
// Memory: measured peaks were ~78 MB of real memory (restic plus
// sqlite3), with the rest of the cgroup figure being reclaimable page
// cache. GOGC/GOMAXPROCS trade a little CPU for a third less heap, and
// MemoryHigh throttles rather than kills.
func LaunchArgs(app App, o LaunchOptions) []string {
	staging := o.StagingRoot + "/" + app.Name
	// An app whose state is only files is read as it lies, so its dir
	// goes in read-only. A database cannot be: SQLite creates the
	// -shm file beside it to read a WAL database at all, so reading
	// one from a read-only mount fails outright ("unable to open
	// database file"). The app's dir is therefore writable exactly
	// when a database must be opened — and the job still only reads,
	// as the same user that already owns the data.
	sharedBind := "--property=BindReadOnlyPaths=" + app.Shared
	if len(app.Databases()) > 0 {
		sharedBind = "--property=BindPaths=" + app.Shared
	}
	args := []string{
		"--wait", "--collect", "--quiet",
		"--unit=" + unitName(app.Name),
		"--property=Type=oneshot",
		"--property=User=" + o.User,
		"--property=Group=" + o.User,
		"--property=EnvironmentFile=" + o.EnvFile,
		"--property=Environment=GOGC=20",
		"--property=Environment=GOMAXPROCS=1",
		// restic's cache lives with this app's staging, not in the
		// user's home: the view has no home, and an uncached run
		// re-reads the whole repository index every hour.
		"--property=Environment=XDG_CACHE_HOME=" + StagingCache(staging),
		"--property=Environment=HOME=" + staging,
		"--property=MemoryAccounting=yes",
		// MemoryHigh throttles: past it the kernel reclaims and the
		// job slows down. There is deliberately no MemoryMax to go
		// with it — that one kills, and restic's working set grows
		// with the size of the repository's index, so a cap that
		// suited a new repository would start OOM-killing the hourly
		// backup a year later, with no way to raise it short of a new
		// release. A backup that runs slowly is better than one that
		// dies, and the 64 MB measured here is a floor to aim at, not
		// a ceiling to enforce.
		"--property=MemoryHigh=64M",
		// Nice on hotserve-backup.service only lowers the launcher: a
		// transient unit is started by the system manager, not forked
		// from this process, so it would otherwise do the CPU-heavy
		// part — sqlite3 and restic — at normal priority beside the
		// apps it is backing up.
		"--property=Nice=10",
		"--property=IOSchedulingClass=idle",
		// A job that hangs must not outlive the run that started it.
		// systemd-run --wait only waits; a launcher that goes away
		// leaves the transient unit going, and the next hour's run
		// would then fail on the unit name being taken — every hour,
		// invisibly. This is what bounds a backup, which is why the
		// launcher itself has no start timeout.
		"--property=RuntimeMaxSec=45min",
		"--property=TemporaryFileSystem=/:ro",
		"--property=BindReadOnlyPaths=/usr /bin /lib -/lib64 /etc/ssl /etc/resolv.conf /etc/hosts /etc/passwd /etc/group /etc/localtime",
		sharedBind,
		"--property=BindPaths=" + staging,
		"--property=PrivateUsers=yes",
		// The same private PID namespace liveswap gives app units
		// (systemd_dbus.go): every app and every job runs as the
		// hotserve user, so without it a compromised restic could
		// read a sibling app's /proc — its command line, its
		// environment — despite the filesystem view being per app.
		"--property=PrivatePIDs=yes",
		"--property=PrivateTmp=yes",
		"--property=PrivateDevices=yes",
		"--property=NoNewPrivileges=yes",
		"--property=CapabilityBoundingSet=",
		"--property=ProtectKernelTunables=yes",
		"--property=ProtectKernelModules=yes",
		"--property=ProtectKernelLogs=yes",
		"--property=ProtectControlGroups=yes",
		"--property=RestrictNamespaces=yes",
		"--property=RestrictSUIDSGID=yes",
		"--property=LockPersonality=yes",
		"--property=MemoryDenyWriteExecute=yes",
		"--property=RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX",
		"--property=SystemCallFilter=@system-service",
	}
	if o.RepositoryPath != "" {
		// A repository on this box is written to, so it goes in
		// writable — and only for the jobs, never for an app.
		args = append(args, "--property=BindPaths="+o.RepositoryPath)
	}
	args = append(args,
		o.Self, "backup", "app",
		"--name="+app.Name,
		"--shared="+app.Shared,
		"--staging="+staging,
	)
	// The declarations go across as they were written, so the argv in
	// the journal reads like the app block it came from.
	for _, e := range app.State {
		args = append(args, e.Kind+":"+e.Path)
	}
	return args
}

// ensureStagingDir creates the dir the job stages database copies in,
// owned by the user the job runs as. This command runs as root (it has
// to, to create the units), so a directory it makes is root's: without
// the chown the job's very first VACUUM INTO fails with a permission
// error, in every app, forever.
func ensureStagingDir(dir, username string) error {
	// Both halves up front: the job cannot create them itself, since
	// only what is bound into its view exists, and a bind of a
	// missing path fails the unit.
	//
	// Every step is symlink-safe, because the contents of these
	// directories are written by the job — an unprivileged process
	// that this one, running as root, then chowns. A job that swapped
	// `data` for a symlink to /etc would otherwise have the next run
	// hand /etc to the hotserve user: a link followed by root is a
	// root escalation, so a path that is not a real directory is
	// refused rather than repaired.
	for _, d := range []string{dir, StagingData(dir), StagingCache(dir)} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return fmt.Errorf("staging dir %s: %w", d, err)
		}
		info, err := os.Lstat(d)
		if err != nil {
			return fmt.Errorf("staging dir %s: %w", d, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("staging path %s is not a directory (%s) — refusing to use it: the backup job writes here, so anything else is something it put there", d, info.Mode().Type())
		}
	}
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
	for _, d := range []string{dir, StagingData(dir), StagingCache(dir)} {
		// Lchown, not Chown: Chown follows a symlink, and following
		// one here is the escalation described above.
		if err := os.Lchown(d, uid, gid); err != nil {
			return fmt.Errorf("giving %s to %s (the job writes its database copies there): %w", d, username, err)
		}
	}
	return nil
}

// RunAll backs up each app in turn — never concurrently: peak memory
// is then one job's, not the sum, which is what keeps a box with
// several apps from paying for all of them at once.
//
// One app's failure does not stop the others: a broken database must
// not cost every other app its backup. The failures are collected and
// reported together.
func RunAll(ctx context.Context, apps []App, o LaunchOptions, run Runner, log io.Writer) error {
	if len(apps) == 0 {
		say(log, "no app declares state; nothing to back up")
		return nil
	}
	var failed []string
	for _, app := range apps {
		// An app can declare its state before it has ever been
		// deployed — liveswap creates shared/ at the first launch, and
		// the declaration is config, not a promise that the files
		// exist. Nothing to copy yet is not a failure; it would
		// otherwise paint the timer red every hour until the first
		// deploy, and a bind of a missing path fails the unit anyway.
		if _, err := os.Stat(app.Shared); errors.Is(err, fs.ErrNotExist) {
			say(log, "%s: no data yet (never deployed), skipping", app.Name)
			continue
		}
		staging := o.StagingRoot + "/" + app.Name
		// A per-app problem here (a stale file where the dir should
		// be, an owner changed by hand) is this app's failure, not
		// the run's: the other apps still get their backup.
		if err := ensureStagingDir(staging, o.User); err != nil {
			say(log, "%s: FAILED (%v)", app.Name, err)
			failed = append(failed, app.Name)
			continue
		}
		args := LaunchArgs(app, o)
		say(log, "+ systemd-run %s", quoteArgs(args))
		if err := run(ctx, "systemd-run", args...); err != nil {
			say(log, "%s: FAILED (%v) — journalctl -u %s", app.Name, err, unitName(app.Name))
			failed = append(failed, app.Name)
			continue
		}
		say(log, "%s: ok", app.Name)
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d of %d apps failed to back up: %s", len(failed), len(apps), strings.Join(failed, ", "))
	}
	return nil
}
