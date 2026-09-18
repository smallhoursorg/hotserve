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
}

// unitName keeps one unit per app so `systemctl status` and the
// journal are per app, and two apps can never share a cgroup.
func unitName(app string) string { return "hotserve-backup-" + app }

// LaunchArgs is the whole systemd-run invocation for one app's job.
//
// The property set is load-bearing as a whole (measured on Debian 13):
// dropping parts of it (the base view, the
// private namespaces) makes systemd fail to set the user at all
// (217/USER) rather than run with a weaker sandbox. The view holds
// exactly three writable things — this app's staging dir — and one
// read-only thing that matters: this app's shared dir. hotserve's own
// state, every other app, and the credentials file are simply absent.
//
// Memory: measured peaks were ~78 MB of real memory (restic plus
// sqlite3), with the rest of the cgroup figure being reclaimable page
// cache. GOGC/GOMAXPROCS trade a little CPU for a third less heap.
//
// No limit on how long a job runs or how much memory it may use. The
// first backup of a large uploads dir over a slow uplink runs for
// hours, and a job killed part-way leaves the next run to upload
// everything again (measured: a first backup killed at 240 MB of
// 763 MB re-sent all 763 MB), so a time limit would stop a large first
// backup from ever completing and grow the repository with orphaned
// data every hour. A memory throttle cannot shrink restic's working
// set — that grows with the repository's index — it can only make it
// crawl. What does bound a job is restic's own
// per-request timeout (--stuck-request-timeout, 5 minutes by default),
// which turns a hung connection into a retry or a failure; and runs
// never overlap, because systemd does not start the timer's unit while
// it is still active.
func LaunchArgs(app App, o LaunchOptions) []string {
	staging := o.StagingRoot + "/" + app.Name
	// An app whose state is only files is read as it lies, so its dir
	// goes in read-only. A database cannot be: SQLite creates the
	// -shm file beside it to read a WAL database at all, so reading
	// one from a read-only mount fails outright ("unable to open
	// database file"). The app's dir is therefore writable exactly
	// when a database must be opened — and the job still only reads,
	// as the same user that already owns the data.
	args := append([]string{
		"--wait", "--collect", "--quiet",
		"--unit=" + unitName(app.Name),
	}, sandboxProperties(jobView{
		User:           o.User,
		EnvFile:        o.EnvFile,
		Home:           staging,
		Shared:         app.Shared,
		SharedWritable: sharedWritable(app),
	})...)
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

// sharedWritable is whether an app's job gets its data dir writable:
// only when a database must be opened, because SQLite creates the -shm
// file beside a WAL database to read it at all. One place decides it,
// for the sandbox and for what the log says about the sandbox.
func sharedWritable(app App) bool { return len(app.Databases()) > 0 }

// jobCommand is the command a unit runs: LaunchArgs without the
// systemd-run flags and sandbox properties in front of it — what an
// operator would type to run the same job by hand.
func jobCommand(args []string) []string {
	for i, a := range args {
		if !strings.HasPrefix(a, "--") {
			return args[i:]
		}
	}
	return nil
}

// jobView is what one backup unit can see: whose it is, where its
// settings come from, the one directory it may write, and the data it
// is there to read.
type jobView struct {
	User    string
	EnvFile string
	// Home is the unit's only private writable directory: an app's
	// staging dir for the hourly job, a scratch dir for init's checks.
	// systemd makes it (see homeProperties), so it is under /var/lib or
	// under /run.
	Home string
	// Shared is the app's data, or "" for a unit that reads none.
	Shared         string
	SharedWritable bool
}

// sandboxProperties is the one definition of a backup unit's sandbox,
// shared by the hourly job and by init's checks. Two lists would
// drift, and every way they differed would be a check that passes at
// init and a job that fails at 03:00.
func sandboxProperties(v jobView) []string {
	props := []string{
		"--property=Type=oneshot",
		"--property=User=" + v.User,
		"--property=Group=" + v.User,
		"--property=EnvironmentFile=" + v.EnvFile,
		"--property=Environment=GOGC=20",
		"--property=Environment=GOMAXPROCS=1",
		// restic's cache lives with this unit's own directory, not in
		// the user's home: the view has no home, and an uncached run
		// re-reads the whole repository index every hour.
		"--property=Environment=XDG_CACHE_HOME=" + StagingCache(v.Home),
		"--property=Environment=HOME=" + v.Home,
		"--property=MemoryAccounting=yes",
		// Nice on hotserve-backup.service only lowers the launcher: a
		// transient unit is started by the system manager, not forked
		// from this process, so it would otherwise do the CPU-heavy
		// part — sqlite3 and restic — at normal priority beside the
		// apps it is backing up.
		"--property=Nice=10",
		"--property=IOSchedulingClass=idle",
		"--property=TemporaryFileSystem=/:ro",
		"--property=BindReadOnlyPaths=/usr /bin /lib -/lib64 /etc/ssl /etc/resolv.conf /etc/hosts /etc/passwd /etc/group /etc/localtime",
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
	props = append(props, homeProperties(v.Home)...)
	if v.Shared != "" {
		if v.SharedWritable {
			props = append(props, "--property=BindPaths="+v.Shared)
		} else {
			props = append(props, "--property=BindReadOnlyPaths="+v.Shared)
		}
	}
	return props
}

// The two places systemd makes a unit's directory.
const (
	stateBase   = "/var/lib/"
	runtimeBase = "/run/"
)

// homeProperties has systemd make the unit's directory, give it to the
// unit's user and put it in the unit's view: StateDirectory= under
// /var/lib, RuntimeDirectory= under /run. The launcher is root and what
// is in these directories is written by jobs, so it touches none of it:
// systemd refuses to start a unit whose directory is a link, and changes
// owners without following one (both measured, systemd 257).
//
// A state directory stays — the staged copies are cleared by the next
// run, restic's cache is kept for it. A runtime directory is init's, and
// is kept between its checks (each is a unit of its own, and restic's
// cache should outlive one) until init removes it.
//
// Anywhere else gets no directory at all, and a unit without its HOME
// fails at its first write: checkStagingRoot refuses such a root first.
func homeProperties(home string) []string {
	if rel, ok := strings.CutPrefix(home, stateBase); ok {
		return []string{"--property=StateDirectory=" + rel, "--property=StateDirectoryMode=0750"}
	}
	if rel, ok := strings.CutPrefix(home, runtimeBase); ok {
		return []string{"--property=RuntimeDirectory=" + rel, "--property=RuntimeDirectoryMode=0700", "--property=RuntimeDirectoryPreserve=yes"}
	}
	return nil
}

// checkStagingRoot refuses a staging root systemd would not make the
// jobs' directories in.
func checkStagingRoot(root string) error {
	clean := filepath.Clean(root)
	if rel, ok := strings.CutPrefix(clean, stateBase); !ok || rel == "" || !filepath.IsLocal(rel) {
		return fmt.Errorf("--staging %s must be a directory under %s: systemd makes each job's directory (StateDirectory=), and that is where it makes them", root, strings.TrimSuffix(stateBase, "/"))
	}
	return nil
}

// probeScript runs restic by name inside the unit. The name is looked
// up there, on the unit's PATH — the same lookup the hourly job's
// `exec` makes — and not by systemd-run, which resolves a relative
// command on the *caller's* PATH before the unit exists. A restic in
// root's ~/bin would otherwise be the one init checked the repository
// with, and not the one the timer ever runs.
const probeScript = `exec restic "$@"`

// resticInUnit is the systemd-run argv that runs one restic command in
// a unit with the job's sandbox, its output piped back to the caller.
func resticInUnit(v jobView, args []string) []string {
	argv := append([]string{"--wait", "--collect", "--quiet", "--pipe", "--expand-environment=no"}, sandboxProperties(v)...)
	argv = append(argv, "/bin/sh", "-c", probeScript, "restic")
	return append(argv, args...)
}

// asJob runs every command init gives it the way the hourly job runs:
// as a transient unit with the job's own sandbox, user, PATH, HOME and
// settings file — not an imitation of them. What passes here is then
// what passes at 03:00 with nobody logged in, by construction.
//
// The settings reach restic through EnvironmentFile=, rendered by the
// same function that writes /etc/hotserve/backup.env, so systemd
// parses exactly the bytes the jobs will get. Each call writes them
// to a fresh root-only file beside that one and removes it after.
func asJob(v jobView, envDir string, run Runner, capture Capturer) (Runner, Capturer) {
	launch := func(ctx context.Context, name string, args []string) (context.Context, []string, func(), error) {
		if name != "restic" {
			return ctx, nil, nil, fmt.Errorf("init runs restic as the job, and nothing else (asked for %s)", name)
		}
		settings, _ := ctx.Value(envKey{}).([]string)
		f, err := os.CreateTemp(envDir, ".backup.env-check-*")
		if err != nil {
			return ctx, nil, nil, fmt.Errorf("writing the settings to check: %w", err)
		}
		cleanup := func() { _ = os.Remove(f.Name()) }
		// One err through all three steps: a chmod or a write that
		// failed must not leave the check running against a partial
		// settings file.
		err = f.Chmod(0o600)
		if err == nil {
			_, err = io.WriteString(f, renderEnvFile(settings))
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			cleanup()
			return ctx, nil, nil, fmt.Errorf("writing the settings to check: %w", err)
		}
		view := v
		view.EnvFile = f.Name()
		// The secrets are in the file now; systemd-run itself has no
		// use for them in its own environment.
		return context.WithValue(ctx, envKey{}, nil), resticInUnit(view, args), cleanup, nil
	}
	return func(ctx context.Context, name string, args ...string) error {
			ctx, argv, cleanup, err := launch(ctx, name, args)
			if err != nil {
				return err
			}
			defer cleanup()
			return run(ctx, "systemd-run", argv...)
		}, func(ctx context.Context, name string, args ...string) ([]byte, error) {
			ctx, argv, cleanup, err := launch(ctx, name, args)
			if err != nil {
				return nil, err
			}
			defer cleanup()
			return capture(ctx, "systemd-run", argv...)
		}
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
		args := LaunchArgs(app, o)
		// The command the unit runs, not the forty sandbox properties
		// around it: those are the same every time, and
		// `systemctl show hotserve-backup-<app>` has them while it runs.
		access := "read-only"
		if sharedWritable(app) {
			access = "writable, for SQLite"
		}
		say(log, "%s: backing up in %s, its data %s: %s", app.Name, unitName(app.Name), access, quoteArgs(jobCommand(args)))
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
