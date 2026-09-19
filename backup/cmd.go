package backup

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	caddycmd "github.com/caddyserver/caddy/v2/cmd"
)

// caddy1 is the conventional generic failure exit code in Caddy's
// command funcs (caddy.ExitCodeFailedStartup).
const caddy1 = 1

func init() {
	caddycmd.RegisterCommand(caddycmd.Command{
		Name:  "backup",
		Usage: "init <repository> | run | status | restore <app> | restic -- <args> | app <flags>",
		Short: "Back up what liveswap apps declare as state",
		Long: `Copies each app's declared state (see the 'state' directive) to a
restic repository. The repository and its credentials come from an
environment file, by default /etc/hotserve/backup.env, which systemd
reads as root; nothing about backups is configured in the Caddyfile.

  hotserve backup init <repository>
      Writes the environment file, creates the repository if it is
      new, and checks whether these credentials can delete from it —
      they should not be able to, so that someone who takes the box
      cannot erase its backups. The checks run the way the hourly job
      will: as a unit with its sandbox and user, with only the settings
      being written.

      At a terminal, init asks for what it needs: the storage key for
      an s3: or b2: repository (the secret half without echo), and, for
      a repository that already exists — a rebuilt box — its password.
      A new repository gets a generated password, printed once; keep
      it somewhere other than the box, because nothing can recover it.
      For a script, give the same things in root-only files instead:
      --credentials-file (KEY=VALUE lines) and --password-file. Never
      on the command line, where your shell history and /proc/*/cmdline
      keep them.

      The repository is a restic backend URL — s3:, b2:, rest:, azure:,
      gs: or swift: — never a path on this box; init, run and restore
      refuse one. (Not sftp: or rclone: — what they need is in files,
      and the jobs' sandbox holds only the settings init writes.)

  hotserve backup run [<app>...]
      Asks the admin API which apps declare state and backs up each
      one — or only the apps named — in its own sandboxed, short-lived
      systemd unit. This is what the packaged timer runs; it needs
      root, to create those units.

  hotserve backup status [--check] [--timeout 2m] [<app>...]
      One line per app that declares state (or per app named): what it
      declares, how many snapshots it has, and how old the newest one
      is. For a monitor, --check exits 1 when an app has no current
      backup and 2 when that could not be found out — hotserve not
      answering, or the repository not answering within --timeout.
      Needs root: the repository settings are root-only.

  hotserve backup restore <app> [--snapshot <id>] [--delete] [--yes]
      Puts the app's declared state back from the newest snapshot a
      clean run recorded in the repository, or the one given. It says
      which snapshot and what will happen, and asks for the app's name
      before changing anything. Databases are replaced through SQLite's
      own backup API, in one transaction, so the app can keep running;
      files in the snapshot are put back, and files added since are
      kept unless --delete. The snapshot is checked first — each
      declared path looked up, each database copy through its integrity
      check — and if any of that fails nothing is touched. A path the
      snapshot does not hold is left as it is. A directory can be left
      part-restored by a failure part-way; running the command again
      finishes it. It runs in the app's backup unit, so it never
      overlaps that app's backup. On a rebuilt box, restore before the
      first deploy: the app then starts on its data.

  hotserve backup restic -- <restic arguments...>
      Runs restic against this box's repository, as the user the
      backups belong to, with the settings from the environment file.
      Running restic on its own finds no repository, because that file
      is root-only:
          sudo hotserve backup restic -- snapshots --tag app:blog

  hotserve backup app --name <app> --shared <dir> [--staging <dir>]
                      <kind>:<path> [<kind>:<path>...]
      One app's backup, in the current process. Each entry is written
      the way the Caddyfile declares it, as sqlite:app.db or
      files:uploads, with paths relative to the app's shared dir:
      every sqlite entry is copied with VACUUM INTO into --staging,
      then restic gets the copies plus every files entry. It is what a
      unit runs, as the backups' user, and refuses to run as root.
      'run' launches it as two sandboxed units (--phase): the copy with the
      app's data writable — SQLite needs that to read a database — and
      no network or repository settings, then the upload with the app's
      data read-only. ('restore-app' is the same for 'restore', and is
      not run by hand.)

Every restic and sqlite3 command is printed as it runs, so any step
can be reproduced by hand. Retention is deliberately not applied here:
the key on the box should not be able to delete, so 'restic forget'
belongs with the privileged key, off the box.`,
		Flags: func() *flag.FlagSet {
			fs := flag.NewFlagSet("backup", flag.ExitOnError)
			fs.String("admin", DefaultAdminAddress, "admin API address (run, status, restore)")
			fs.String("staging", DefaultStagingRoot, "staging root for database copies (run, restore)")
			fs.String("env-file", "/etc/hotserve/backup.env", "repository settings, read by systemd as root (every command)")
			fs.String("user", "hotserve", "user the jobs, and every restic, run as (every command)")
			fs.Duration("timeout", 2*time.Minute, "how long the repository may take to answer; after it the report gives up and exits 2. 0 waits for as long as restic retries (status)")
			fs.String("name", "", "app name (app)")
			fs.String("shared", "", "the app's shared dir, absolute (app)")
			fs.String("phase", "", "one step of an app's backup: stage (copy its databases) or upload; both when unset (app; set by run)")
			fs.String("password-file", "", "the password of a repository that already exists, for a rebuilt box (init)")
			fs.String("credentials-file", "", "provider credentials as KEY=VALUE lines, instead of on the command line (init)")
			fs.Bool("force", false, "replace an existing environment file (init)")
			fs.Bool("check", false, "exit 1 when an app has no current backup, 2 when that could not be found out (status)")
			fs.String("snapshot", "", "the snapshot to restore, by id; when unset, the newest one a clean run vouches for (restore)")
			fs.Bool("delete", false, "also delete files added since the snapshot, inside each declared files path (restore)")
			fs.Bool("yes", false, "restore without asking first, for a script (restore)")
			return fs
		}(),
		Func: cmdBackup,
	})
}

// parseEntries reads the positional `<kind>:<path>` arguments — the
// same vocabulary the Caddyfile uses, so what an operator reads in
// the journal matches what they wrote in the app block. Paths may
// contain a colon, so only the first one separates.
func parseEntries(args []string) ([]StateEntry, error) {
	entries := make([]StateEntry, 0, len(args))
	for _, a := range args {
		kind, path, ok := strings.Cut(a, ":")
		if !ok || path == "" {
			return nil, fmt.Errorf("state entry %q must be written <kind>:<path>, e.g. sqlite:app.db or files:uploads", a)
		}
		if kind != KindSQLite && kind != KindFiles {
			return nil, fmt.Errorf("unknown state kind %q in %q: want %s or %s", kind, a, KindSQLite, KindFiles)
		}
		entries = append(entries, StateEntry{Kind: kind, Path: path})
	}
	return entries, nil
}

func cmdBackup(fl caddycmd.Flags) (int, error) {
	args := fl.Args()
	if len(args) == 0 {
		return caddy1, fmt.Errorf("say what to do: `hotserve backup init <repository>` (once per box), `status` (is it working), `run [app…]` (back up now), `restore <app>`, or `restic -- <restic arguments>` — `hotserve backup --help` describes each")
	}
	switch args[0] {
	case "init":
		return cmdInit(fl, args[1:])
	case "run":
		return cmdRun(fl, args[1:])
	case "app":
		return cmdApp(fl, args[1:])
	case "status":
		return cmdStatus(fl, args[1:])
	case "restic":
		return cmdRestic(fl, args[1:])
	case "restore":
		return cmdRestore(fl, args[1:])
	case "restore-app":
		return cmdRestoreApp(fl, args[1:])
	case "check-settings":
		return cmdCheckSettings()
	default:
		return caddy1, fmt.Errorf("unknown subcommand %q: want init, run, status, restore, restic or app", args[0])
	}
}

// cmdRestic is how every restore in the docs reaches the repository:
// the settings live in a root-only file, and the backups belong to
// the jobs' user, so neither `restic` as that user nor `sudo restic`
// works on its own.
func cmdRestic(fl caddycmd.Flags, args []string) (int, error) {
	if err := requireTools("restic"); err != nil {
		return caddy1, err
	}
	if err := Passthrough(context.Background(), fl.String("env-file"), fl.String("user"), args, osExec); err != nil {
		return caddy1, err
	}
	return 0, nil
}

func cmdStatus(fl caddycmd.Flags, names []string) (int, error) {
	// A monitor gives this a time limit and ends it with a signal: the
	// context ends, and the unit restic is running in is stopped with it
	// (stopWithContext) rather than left retrying a repository that is
	// not answering, one more for every poll.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := requireTools("restic"); err != nil {
		return exitCouldNotCheck, err
	}
	apps, err := FetchApps(ctx, fl.String("admin"))
	if err != nil {
		return exitCouldNotCheck, err
	}
	if apps, err = selectApps(apps, names); err != nil {
		return exitCouldNotCheck, err
	}
	envFile := fl.String("env-file")
	if err := requireSettingsFile(envFile); err != nil {
		return exitCouldNotCheck, err
	}
	// Asked of a repository that is not answering, restic retries for
	// many minutes, and a monitor asks again long before that: the report
	// has a time limit of its own, after which its restic is stopped and
	// the answer is "could not find out".
	timeout := fl.Duration("timeout")
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	host, err := os.Hostname()
	if err != nil {
		return exitCouldNotCheck, fmt.Errorf("finding this box's hostname (restic records it on every snapshot): %w", err)
	}
	// This command is root. restic is not: it talks to the network and
	// parses what the storage sends back, so it runs where the jobs'
	// restic runs — a unit, as the jobs' user, in their sandbox, with no
	// app's data in its view — and only its listing comes back here. The
	// settings reach it the way they reach a job: systemd reads the file.
	view := jobView{User: fl.String("user"), EnvFile: envFile, Home: statusHome}
	statuses, err := Status(ctx, apps, inUnit(view, osExec), host)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return exitCouldNotCheck, fmt.Errorf("the repository did not answer within %s (--timeout), and the restic that was asking has been stopped: %w", timeout, err)
		}
		return exitCouldNotCheck, err
	}
	for i := range statuses {
		statuses[i].Running = unitActive(ctx, unitName(statuses[i].App.Name)+".service")
		statuses[i].NothingYet = nothingToBackUpYet(statuses[i].App)
	}
	now := time.Now()
	FormatStatus(os.Stdout, statuses, now)
	if !fl.Bool("check") {
		return 0, nil
	}
	// --check makes this usable from a monitor: the report still
	// prints, and a stale app is the non-zero exit.
	var stale []string
	for _, s := range statuses {
		if s.Stale(now) {
			stale = append(stale, s.App.Name)
		}
	}
	if len(stale) > 0 {
		return caddy1, fmt.Errorf("no clean backup in the last %.1f hours of: %s", StaleAfter.Hours(), strings.Join(stale, ", "))
	}
	return 0, nil
}

func cmdInit(fl caddycmd.Flags, args []string) (int, error) {
	if len(args) == 0 {
		return caddy1, fmt.Errorf("say where the backups go, e.g. `hotserve backup init s3:s3.us-west-004.backblazeb2.com/my-bucket`")
	}
	extra := args[1:] // provider credentials as KEY=VALUE
	if path := fl.String("credentials-file"); path != "" {
		fromFile, err := CredentialsFile(path)
		if err != nil {
			return caddy1, err
		}
		extra = append(fromFile, extra...)
	}
	username := fl.String("user")
	if err := CheckRepository(args[0]); err != nil {
		return caddy1, err
	}
	// Before anything is asked for: a storage key typed at a prompt and
	// then "exec: restic: not found" is a key typed for nothing.
	if err := requireTools("restic"); err != nil {
		return caddy1, err
	}
	// What the checks need on disk is on tmpfs, in two places: the
	// settings they run with (the password and the storage key) in a
	// root-only directory, and their one writable directory, which
	// systemd makes for them (checkHome).
	//   - The settings' directory is root-only, so nothing the backup
	//     user controls sits in any path root writes through here: a
	//     directory the hotserve user can rename things in is a directory
	//     where root's next write can be redirected.
	//   - On tmpfs, so a crash — or a SIGKILL, which no cleanup survives —
	//     leaves the secrets in memory until the next boot at most, never
	//     on disk.
	// And an interrupt or a stop cancels the context rather than killing
	// this process outright, so the cleanups below do run.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := os.MkdirAll(checkRoot, 0o700); err != nil {
		return caddy1, fmt.Errorf("making %s for the repository checks: %w", checkRoot, err)
	}
	if err := requireRootOnlyDir(checkRoot); err != nil {
		return caddy1, err
	}
	// Whatever an init that was killed left there goes first, and what
	// this one leaves goes last. /run is root's, so the name cannot be
	// swapped under the removal, and RemoveAll follows no link inside.
	_ = os.RemoveAll(checkHome)
	defer func() { _ = os.RemoveAll(checkHome) }()
	o := InitOptions{
		Repository:   args[0],
		Extra:        extra,
		EnvFile:      fl.String("env-file"),
		Password:     os.Getenv("RESTIC_PASSWORD"),
		PasswordFile: fl.String("password-file"),
		User:         username,
		Force:        fl.Bool("force"),
	}
	// At a terminal, init asks for what it was not given — the storage
	// key, and the password of a repository that already exists — so
	// setting up a box is one command, and no secret is typed where the
	// shell or /proc would keep it. Anywhere else it asks nothing.
	if ask := terminalPrompter(ctx, os.Stdin, os.Stderr); ask != nil {
		for _, c := range missingCredentials(o.Repository, o.Extra) {
			v, err := ask(c.label, c.secret)
			if err != nil {
				return caddy1, err
			}
			o.Extra = append(o.Extra, c.key+"="+v)
		}
		if o.PasswordFile == "" && o.Password == "" {
			o.AskPassword = func() (string, error) {
				return ask("This repository already exists. Its password (not shown)", true)
			}
		}
	}
	view := jobView{User: username, Home: checkHome}
	// The existence and probe checks are questions, not failures, so
	// their output is captured rather than shown: restic's "repository
	// does not exist" while init is about to create one reads as an
	// error when it is the expected answer.
	if err := Init(ctx, o, asJob(view, checkRoot, osExec), os.Stdout); err != nil {
		return caddy1, err
	}
	return 0, nil
}

// unitPath is where a unit looks for a command given by name: systemd's
// default PATH for a system unit. The jobs' restic and sqlite3 are found
// there, not on the PATH of whoever ran this command.
var unitPath = []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin"}

// requireTools says, before a command has asked for or started anything,
// that a program its units run is not installed. The package recommends
// restic and sqlite3 rather than depending on them — a box that never
// sets backups up does not need them — so `apt install
// --no-install-recommends` leaves them out, and what a unit then says is
// "exec: restic: not found" at the end of an error about something else.
func requireTools(names ...string) error { return requireToolsIn(unitPath, names...) }

func requireToolsIn(dirs []string, names ...string) error {
	var missing []string
	for _, name := range names {
		found := false
		for _, dir := range dirs {
			if info, err := os.Stat(filepath.Join(dir, name)); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	list, verb := strings.Join(missing, " and "), "is"
	if len(missing) > 1 {
		verb = "are"
	}
	return fmt.Errorf("%s %s not installed: a backup's units run %s from the distribution — `sudo apt install %s` (the hotserve package recommends them, and an install without recommended packages leaves them out)", list, verb, list, strings.Join(missing, " "))
}

// requireToolsFor is requireTools for these apps: restic, and sqlite3 if
// one of them declares a database.
func requireToolsFor(apps []App) error {
	if len(apps) == 0 {
		return nil
	}
	tools := []string{"restic"}
	for _, a := range apps {
		if len(a.Databases()) > 0 {
			tools = append(tools, "sqlite3")
			break
		}
	}
	return requireTools(tools...)
}

// exitCouldNotCheck is `status` failing to find out — hotserve not
// answering, the repository not reachable — as opposed to finding out
// that a backup is stale (1): a monitor pages for the one and retries
// the other.
const exitCouldNotCheck = 2

// nothingToBackUpYet is whether an app has, right now, none of what it
// declares: no shared dir (never deployed), or no declared path in it.
// The hourly run passes over such an app, and says so.
func nothingToBackUpYet(app App) bool {
	for _, e := range app.State {
		p, err := sharedPath(app.Shared, e.Path)
		if err != nil {
			return false
		}
		if _, err := os.Lstat(p); err == nil {
			return false
		}
	}
	return true
}

// unitActive reports whether a systemd unit is running. An error reads
// as "not running", rather than failing the report over a detail.
func unitActive(ctx context.Context, unit string) bool {
	// The state is read, not the exit status: is-active exits 0 only
	// for "active", and a job is never that (see unitStateIsRunning).
	out, _ := exec.CommandContext(ctx, "systemctl", "is-active", unit).Output() //nolint:gosec // a fixed program; the unit name is built here from an app name the config validated
	return unitStateIsRunning(strings.TrimSpace(string(out)))
}

// unitStateIsRunning reports whether a unit in this state has a process
// running. The jobs are Type=oneshot, and systemd calls a oneshot
// "activating" for as long as its command runs — it becomes "active"
// only with RemainAfterExit=, which a job does not have — so
// "activating" is the state a backup or a restore spends its life in.
func unitStateIsRunning(state string) bool {
	switch state {
	case "active", "activating", "reloading", "deactivating":
		return true
	}
	return false
}

// checkRoot holds the settings each of init's checks is started with,
// while they run. /run is tmpfs.
const checkRoot = "/run/hotserve-backup"

// statusHome is the directory status's restic may write: its cache,
// kept from one report to the next, since a monitor asks every few
// minutes. Beside the staging root, not in it: an app may be named
// anything.
const statusHome = "/var/lib/hotserve-backup-status"

// checkHome is the directory init's checks may write: restic's cache,
// kept from one check to the next. systemd makes it for them, as it
// makes a job's (homeProperties).
const checkHome = "/run/hotserve-backup-check"

// requireRootOnlyDir refuses to use a directory for secrets unless it
// is a real directory, owned by root, that nobody else can enter —
// whatever state an earlier run, or someone else, left it in.
func requireRootOnlyDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("checking %s: %w", dir, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || st.Uid != 0 || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s must be a directory only root can enter (it holds the repository password while init runs); it is %s, owner uid %d — remove it and run init again", dir, info.Mode(), ownerOf(st))
	}
	return nil
}

func ownerOf(st *syscall.Stat_t) int64 {
	if st == nil {
		return -1
	}
	return int64(st.Uid)
}

func cmdRun(fl caddycmd.Flags, names []string) (int, error) {
	// `systemctl stop hotserve-backup.service`, or Ctrl-C, ends the
	// context, and the run stops the job it is waiting on (RunAll). The
	// jobs are units of their own, outside this one's cgroup: nothing
	// else would.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	self, err := os.Executable()
	if err != nil {
		return caddy1, fmt.Errorf("finding this binary (the jobs re-exec it): %w", err)
	}
	apps, err := FetchApps(ctx, fl.String("admin"))
	if err != nil {
		return caddy1, err
	}
	if apps, err = selectApps(apps, names); err != nil {
		return caddy1, err
	}
	if err := requireToolsFor(apps); err != nil {
		return caddy1, err
	}
	o := LaunchOptions{
		Self:        self,
		StagingRoot: fl.String("staging"),
		EnvFile:     fl.String("env-file"),
		User:        fl.String("user"),
	}
	if err := checkStagingRoot(o.StagingRoot); err != nil {
		return caddy1, err
	}
	if len(apps) > 0 {
		if err := checkSettingsInUnit(ctx, o, osExec); err != nil {
			return caddy1, err
		}
	}
	if err := RunAll(ctx, apps, o, osExec, os.Stdout); err != nil {
		return caddy1, err
	}
	return 0, nil
}

// checkSettingsInUnit refuses settings this package will not run on —
// a repository that is a path on this box, above all — before anything
// is launched on them, and without this process, which is root, opening
// the file: systemd reads it for a unit, and the unit says what is wrong
// (settingsCheckArgs). What the unit prints reaches the operator.
func checkSettingsInUnit(ctx context.Context, o LaunchOptions, x Exec) error {
	if err := requireSettingsFile(o.EnvFile); err != nil {
		return err
	}
	unit := oneOffUnit("settings")
	argv := append([]string{"--unit=" + unit}, settingsCheckArgs(o.Self, o.User, o.EnvFile)...)
	if err := stopWithContext(ctx, x, unit, Cmd{Name: "systemd-run", Args: argv}); err != nil {
		return fmt.Errorf("the settings in %s cannot be used (above): %w", o.EnvFile, err)
	}
	return nil
}

// cmdCheckSettings is checkSettingsInUnit's other half, inside the unit:
// the settings are this process's environment, as systemd read them.
func cmdCheckSettings() (int, error) {
	if err := checkSettings(lookupEnv); err != nil {
		return caddy1, err
	}
	return 0, nil
}

func cmdApp(fl caddycmd.Flags, entryArgs []string) (int, error) {
	name := fl.String("name")
	shared := fl.String("shared")
	staging := fl.String("staging")
	if name == "" || shared == "" {
		return caddy1, fmt.Errorf("--name and --shared are required (this subcommand is normally launched by `hotserve backup run`)")
	}
	// In its unit this is the backups' user. By hand as root it would
	// leave root's copies in the app's staging dir, which the hourly copy
	// — not root — then cannot clear, every hour (systemd hands a
	// StateDirectory= to the unit's user by its top directory alone:
	// measured), and it would run restic as root.
	if os.Geteuid() == 0 {
		return caddy1, fmt.Errorf("`backup app` is one app's job, and does not run as root: what it left in the staging dir would be root's, and the hourly job could not clear it. `sudo hotserve backup run` runs every app's job as the backups' user, each in its sandbox")
	}
	entries, err := parseEntries(entryArgs)
	if err != nil {
		return caddy1, err
	}
	if len(entries) == 0 {
		return caddy1, fmt.Errorf("app %s: nothing to back up — pass what its Caddyfile declares, e.g. sqlite:app.db files:uploads", name)
	}
	// `run` passes the app's own dir. A hand-run needs one too: two
	// apps staging into one dir would wipe each other's copies, since
	// every run clears the staging data before it writes. Compared
	// after Clean, so a trailing slash does not decide it.
	if clean := filepath.Clean(staging); filepath.Base(clean) != name {
		staging = filepath.Join(clean, name)
	}
	app := App{Name: name, Shared: shared, State: entries}
	job := Job{
		App:       name,
		Shared:    shared,
		Staging:   staging,
		Databases: app.Databases(),
		Files:     app.Files(),
		Exec:      osExec,
		Log:       os.Stdout,
	}
	// Said by the job, which is given the app's data if there is any,
	// and not by the launcher, which cannot see every place it may be.
	// The copy has no network, so it can only say the dir is not there;
	// the upload asks the repository whether that is news (missingData).
	if _, err := os.Stat(shared); errors.Is(err, os.ErrNotExist) {
		if fl.String("phase") != phaseStage {
			if err := job.missingData(context.Background()); !errors.Is(err, errNoData) {
				return caddy1, err
			}
		}
		fmt.Printf("%s: nothing to back up yet — %s is not there: the app has not been deployed\n", name, shared)
		return noData(name), nil
	}
	switch phase := fl.String("phase"); phase {
	case phaseStage:
		if err := job.Stage(context.Background()); err != nil {
			return caddy1, err
		}
		fmt.Printf("%s: databases copied\n", name)
		return 0, nil
	case phaseUpload:
		job.Staged = true
	case "":
	default:
		return caddy1, fmt.Errorf("unknown --phase %q: want %s or %s", phase, phaseStage, phaseUpload)
	}
	switch err := job.Execute(context.Background()); {
	case errors.Is(err, errNoData):
		return noData(name), nil // why is said by the job, in its own words
	case err != nil:
		return caddy1, err
	}
	fmt.Printf("%s: backed up\n", name)
	return 0, nil
}

// noData is the job's exit for an app with nothing to back up yet, with
// the line that goes before it. systemd logs the status as this unit
// failing, straight after the job's own line saying why there was
// nothing to copy: read together, without this, they look like a reason
// and then a second problem.
func noData(app string) int {
	fmt.Printf("%s: exiting with status %d, which tells the run there was nothing to back up. systemd reports that below as this unit failing; it is not a failure — the run says \"skipping\" and carries on, and this stops once the app has data\n", app, exitNoData)
	return exitNoData
}

// cmdRestore puts one app's declared state back from a snapshot: it
// finds the snapshot, says what will happen, asks, and then runs the
// restore where the backup runs — in that app's unit, as its user,
// in its sandbox.
func cmdRestore(fl caddycmd.Flags, args []string) (int, error) {
	if len(args) != 1 {
		return caddy1, fmt.Errorf("say which app to restore: `hotserve backup restore <app>` (the newest snapshot) or `hotserve backup restore <app> --snapshot <id>`")
	}
	name := args[0]
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	self, err := os.Executable()
	if err != nil {
		return caddy1, fmt.Errorf("finding this binary (the restore re-execs it): %w", err)
	}
	apps, err := FetchApps(ctx, fl.String("admin"))
	if err != nil {
		return caddy1, err
	}
	app, err := findApp(apps, name)
	if err != nil {
		return caddy1, err
	}
	if err := requireToolsFor([]App{app}); err != nil {
		return caddy1, err
	}
	envFile := fl.String("env-file")
	o := LaunchOptions{
		Self:        self,
		StagingRoot: fl.String("staging"),
		EnvFile:     envFile,
		User:        fl.String("user"),
	}
	if err := checkStagingRoot(o.StagingRoot); err != nil {
		return caddy1, err
	}
	if err := checkSettingsInUnit(ctx, o, osExec); err != nil {
		return caddy1, err
	}
	unit := unitName(name)
	// Checked here to say so plainly; systemd refuses a second unit of
	// the same name anyway, so a backup that starts after this check
	// still cannot run beside the restore.
	if unitActive(ctx, unit+".service") {
		return caddy1, fmt.Errorf("%s is backing up right now; restore once it has finished (journalctl -u %s -f)", name, unit)
	}
	staging := filepath.Join(o.StagingRoot, name)
	view := jobView{User: o.User, EnvFile: envFile, Home: StagingRestore(staging)}
	// This app's backups, and every clean-run record (an OR of the two
	// --tag flags).
	out, err := inUnit(view, osExec).output(ctx, restic("snapshots", "--json", "--tag", "hotserve,app:"+name, "--tag", CleanTag))
	if err != nil {
		return caddy1, fmt.Errorf("listing the snapshots of %s: %w", name, err)
	}
	var listed []Snapshot
	if err := json.Unmarshal(out, &listed); err != nil {
		return caddy1, fmt.Errorf("restic returned a snapshot list this version cannot read: %w", err)
	}
	snaps, cleanIDs, _ := cleanRecords(listed)
	snap, note, err := pickSnapshot(name, snaps, fl.String("snapshot"), cleanIDs[name])
	if err != nil {
		return caddy1, err
	}
	del := fl.Bool("delete")
	DescribeRestore(os.Stdout, app, snap, del, time.Now())
	if note != "" {
		say(os.Stdout, "%s", note)
	}
	if !fl.Bool("yes") {
		if err := confirmRestore(name, terminalPrompter(ctx, os.Stdin, os.Stderr)); err != nil {
			return caddy1, err
		}
	}
	// A rebuilt box restores before the first deploy, when liveswap has
	// not made the app's directories yet. Made only now, with a snapshot
	// chosen and the operator's yes: an empty shared/ left by a restore
	// that went no further would turn that app's hourly "no data yet"
	// into an hourly failure over a database that is not there.
	if err := ensureShared(ctx, app.Shared, o.User, osExec); err != nil {
		return caddy1, err
	}
	launch := RestoreArgs(app, o, snap.ID, del)
	say(os.Stdout, "%s: restoring in %s: %s", name, unit, quoteArgs(jobCommand(launch)))
	// An interrupt ends systemd-run — the client, not the unit, which
	// PID 1 owns and would otherwise go on writing the app's data with
	// nobody watching — so the unit is stopped with it, at once
	// (stopWithContext), and the message says what that leaves.
	err = stopWithContext(ctx, osExec, unit, Cmd{Name: "systemd-run", Args: launch})
	if ctx.Err() != nil {
		return caddy1, fmt.Errorf("interrupted: the restore of %s was stopped, so it may be partly done — run it again to finish it", name)
	}
	if err != nil {
		return caddy1, fmt.Errorf("restoring %s failed (%w) — the lines above say which paths were restored and which step failed", name, err)
	}
	say(os.Stdout, "%s: restored from snapshot %s", name, snap.ShortID)
	return 0, nil
}

func findApp(apps []App, name string) (App, error) {
	var names []string
	for _, a := range apps {
		if a.Name == name {
			return a, nil
		}
		names = append(names, a.Name)
	}
	if len(names) == 0 {
		return App{}, fmt.Errorf("no app declares state, so there is nothing to restore %s into — add its block, with its `state` lines, to the Caddyfile and reload first", name)
	}
	return App{}, fmt.Errorf("no app %s declares state here (apps that do: %s) — a restore puts back what the app's block declares, so the block comes first", name, strings.Join(names, ", "))
}

// ensureShared makes an app's shared dir when it does not exist yet, as
// the user that owns it and with liveswap's own mode: made by root, it
// would be root's, and the app could not write its own data. Made as
// that user, a link someone left in the path is followed with only
// that user's rights.
func ensureShared(ctx context.Context, shared, username string, x Exec) error {
	if _, err := os.Stat(shared); err == nil {
		return nil
	}
	return x(ctx, Cmd{Name: "systemd-run", Args: []string{"--wait", "--collect", "--quiet", noExpansion,
		"--property=User=" + username, "--property=Group=" + username, "--property=UMask=0027",
		"/bin/mkdir", "-p", shared}})
}

// cmdRestoreApp is the restore itself, inside the unit cmdRestore
// launches.
func cmdRestoreApp(fl caddycmd.Flags, entryArgs []string) (int, error) {
	name, shared, staging, snapshot := fl.String("name"), fl.String("shared"), fl.String("staging"), fl.String("snapshot")
	if name == "" || shared == "" || snapshot == "" {
		return caddy1, fmt.Errorf("--name, --shared and --snapshot are required (this subcommand is launched by `hotserve backup restore`)")
	}
	entries, err := parseEntries(entryArgs)
	if err != nil {
		return caddy1, err
	}
	if clean := filepath.Clean(staging); filepath.Base(clean) != name {
		staging = filepath.Join(clean, name)
	}
	app := App{Name: name, Shared: shared, State: entries}
	job := RestoreJob{
		App:       name,
		Shared:    shared,
		Staging:   staging,
		Snapshot:  snapshot,
		Databases: app.Databases(),
		Files:     app.Files(),
		Delete:    fl.Bool("delete"),
		Exec:      osExec,
		Log:       os.Stdout,
	}
	if err := job.Execute(context.Background()); err != nil {
		return caddy1, err
	}
	return 0, nil
}
