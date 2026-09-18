package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
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

      The repository is a restic backend URL — s3:, b2:, rest:, sftp:,
      azure:, gs:, swift: or rclone: — never a path on this box; init,
      run and restore refuse one.

  hotserve backup run
      Asks the admin API which apps declare state and backs up each
      one in its own sandboxed, short-lived systemd unit. This is what
      the packaged timer runs; it needs root, to create those units.

  hotserve backup status [--check]
      One line per app that declares state: what it declares, how many
      snapshots it has, and how old the newest one is. --check exits
      non-zero when an app has no current backup, for a monitor. Needs
      root: the repository settings are root-only.

  hotserve backup restore <app> [--snapshot <id>] [--delete] [--yes]
      Puts the app's declared state back from the newest snapshot a
      clean run recorded in the repository, or the one given. It says which snapshot and what will happen, and asks
      for the app's name before changing anything. Databases are
      replaced through SQLite's own backup API, in one transaction, so
      the app can keep running; files in the snapshot are put back, and
      files added since are kept unless --delete. The snapshot is
      checked first — each declared path looked up, each database copy
      through its integrity check — and if any of that fails nothing
      is touched. A path the snapshot does not hold is left as it is.
      A directory can be left part-restored by a failure part-way;
      running the command again finishes it. It runs in the app's
      backup unit, so it never overlaps that app's backup. On a rebuilt
      box, restore before the first deploy: the app then starts on its
      data.

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
      then restic gets the copies plus every files entry. This is what
      'run' launches inside the sandbox; running it by hand is
      supported and does exactly the same thing, unsandboxed.
      ('restore-app' is the same for 'restore', and is not run by
      hand.)

Every restic and sqlite3 command is printed as it runs, so any step
can be reproduced by hand. Retention is deliberately not applied here:
the key on the box should not be able to delete, so 'restic forget'
belongs with the privileged key, off the box.`,
		Flags: func() *flag.FlagSet {
			fs := flag.NewFlagSet("backup", flag.ExitOnError)
			fs.String("admin", DefaultAdminAddress, "admin API address (run)")
			fs.String("staging", DefaultStagingRoot, "staging root for database copies")
			fs.String("env-file", "/etc/hotserve/backup.env", "repository credentials, read by systemd as root (run)")
			fs.String("user", "hotserve", "user the per-app jobs run as (run)")
			fs.String("name", "", "app name (app)")
			fs.String("shared", "", "the app's shared dir, absolute (app)")
			fs.String("password-file", "", "the password of a repository that already exists, for a rebuilt box (init)")
			fs.String("credentials-file", "", "provider credentials as KEY=VALUE lines, instead of on the command line (init)")
			fs.Bool("force", false, "replace an existing environment file (init)")
			fs.Bool("check", false, "exit non-zero when an app has no current backup (status)")
			fs.String("snapshot", "", "the snapshot to restore, by id; the newest when unset (restore)")
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
		return caddy1, fmt.Errorf("say what to do: `hotserve backup run` (all apps, sandboxed) or `hotserve backup app --name <app> …` (one app, here)")
	}
	switch args[0] {
	case "init":
		return cmdInit(fl, args[1:])
	case "run":
		return cmdRun(fl)
	case "app":
		return cmdApp(fl, args[1:])
	case "status":
		return cmdStatus(fl)
	case "restic":
		return cmdRestic(fl, args[1:])
	case "restore":
		return cmdRestore(fl, args[1:])
	case "restore-app":
		return cmdRestoreApp(fl, args[1:])
	default:
		return caddy1, fmt.Errorf("unknown subcommand %q: want init, run, status, restore, restic or app", args[0])
	}
}

// cmdRestic is how every restore in the docs reaches the repository:
// the settings live in a root-only file, and the backups belong to
// the jobs' user, so neither `restic` as that user nor `sudo restic`
// works on its own.
func cmdRestic(fl caddycmd.Flags, args []string) (int, error) {
	if err := Passthrough(context.Background(), fl.String("env-file"), fl.String("user"), args, os.Stdout, os.Stderr); err != nil {
		return caddy1, err
	}
	return 0, nil
}

func cmdStatus(fl caddycmd.Flags) (int, error) {
	ctx := context.Background()
	apps, err := FetchApps(ctx, fl.String("admin"))
	if err != nil {
		return caddy1, err
	}
	// systemd gives the jobs these settings; a person running this by
	// hand has nothing exported, so read the same file they do.
	env, err := LoadEnvFile(fl.String("env-file"))
	if err != nil {
		return caddy1, err
	}
	host, err := os.Hostname()
	if err != nil {
		return caddy1, fmt.Errorf("finding this box's hostname (restic records it on every snapshot): %w", err)
	}
	statuses, err := Status(ctx, apps, withCaptureEnv(captureRunner(), env), fl.String("staging"), host)
	if err != nil {
		return caddy1, err
	}
	for i := range statuses {
		statuses[i].Running = unitActive(ctx, unitName(statuses[i].App.Name)+".service")
	}
	now := time.Now()
	FormatStatus(os.Stdout, statuses, now)
	if !fl.Bool("check") {
		return 0, nil
	}
	// --check makes this usable from a monitor: the report still
	// prints, and a stale app is the non-zero exit.
	for _, s := range statuses {
		if s.Stale(now) {
			return caddy1, fmt.Errorf("%s has no backup newer than %s", s.App.Name, StaleAfter)
		}
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
	// Everything the checks need on disk lives under one root-only
	// directory on tmpfs: the settings they run with (the password and
	// the storage key) and their one writable directory.
	//   - Root-only, so nothing the backup user controls sits in any path
	//     root writes through here: a directory the hotserve user can
	//     rename things in is a directory where root's next write can
	//     be redirected.
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
	home, err := os.MkdirTemp(checkRoot, "init-")
	if err != nil {
		return caddy1, fmt.Errorf("making a directory for the repository checks: %w", err)
	}
	defer func() { _ = os.RemoveAll(home) }()
	if err := ensureStagingDir(home, username); err != nil {
		return caddy1, err
	}
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
	view := jobView{User: username, Home: home}
	// The existence and probe checks are questions, not failures, so
	// their output is captured rather than shown: restic's "repository
	// does not exist" while init is about to create one reads as an
	// error when it is the expected answer.
	run, capture := asJob(view, checkRoot, execRunner(os.Stderr), captureQuiet())
	if err := Init(ctx, o, run, capture, os.Stdout); err != nil {
		return caddy1, err
	}
	return 0, nil
}

// unitActive reports whether a systemd unit is running. An error reads
// as "not running", rather than failing the report over a detail.
func unitActive(ctx context.Context, unit string) bool {
	return exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", unit).Run() == nil //nolint:gosec // a fixed program; the unit name is built here from an app name the config validated
}

// checkRoot holds init's checks while they run: the settings each one
// is started with, and the directory it may write. /run is tmpfs.
const checkRoot = "/run/hotserve-backup"

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

func cmdRun(fl caddycmd.Flags) (int, error) {
	ctx := context.Background()
	self, err := os.Executable()
	if err != nil {
		return caddy1, fmt.Errorf("finding this binary (the jobs re-exec it): %w", err)
	}
	apps, err := FetchApps(ctx, fl.String("admin"))
	if err != nil {
		return caddy1, err
	}
	envFile := fl.String("env-file")
	// systemd hands the jobs this file; it is read here only to refuse
	// a repository that is not a backend URL.
	env, err := LoadEnvFile(envFile)
	if err != nil {
		return caddy1, err
	}
	if err := checkSettingsRepository(env); err != nil {
		return caddy1, err
	}
	o := LaunchOptions{
		Self:        self,
		StagingRoot: fl.String("staging"),
		EnvFile:     envFile,
		User:        fl.String("user"),
	}
	if err := RunAll(ctx, apps, o, execRunner(os.Stderr), os.Stdout); err != nil {
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
		Run:       execRunner(os.Stderr),
		Capture:   captureRunner(),
		Log:       os.Stdout,
	}
	if err := job.Execute(context.Background()); err != nil {
		return caddy1, err
	}
	fmt.Printf("%s: backed up\n", name)
	return 0, nil
}

// execRunner runs a real command, letting its output through: restic
// and sqlite3 explain their own failures better than a wrapper can.
func execRunner(stderr *os.File) Runner {
	return func(ctx context.Context, name string, args ...string) error {
		cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // restic, sqlite3 and systemd-run with an argv built here from the running config, never from a request
		cmd.Env = commandEnv(ctx)
		cmd.Stdout = stderr
		cmd.Stderr = stderr
		if err := cmd.Run(); err != nil {
			if _, lookErr := exec.LookPath(name); lookErr != nil {
				return fmt.Errorf("%s is not installed: %w", name, lookErr)
			}
			return err
		}
		return nil
	}
}

// captureRunner reads a command's stdout instead of passing it
// through, for the reporting path. Its stderr still goes to the
// terminal, so restic's own explanation of a failure is not swallowed.
func captureRunner() Capturer {
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // restic, sqlite3 and systemd-run with an argv built here from the running config, never from a request
		cmd.Env = commandEnv(ctx)
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		if err != nil {
			if _, lookErr := exec.LookPath(name); lookErr != nil {
				return nil, fmt.Errorf("%s is not installed: %w", name, lookErr)
			}
			return nil, err
		}
		return out, nil
	}
}

// captureQuiet keeps a command's output to itself — its failure is an
// answer, not a fault — but keeps stderr rather than dropping it:
// restic explains a refusal there, and that wording is the evidence
// the delete check classifies. Without it, every append-only
// repository would report as "unknown".
func captureQuiet() Capturer {
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // restic, sqlite3 and systemd-run with an argv built here from the running config, never from a request
		cmd.Env = commandEnv(ctx)
		var errOut bytes.Buffer
		cmd.Stderr = &errOut
		if sink := stderrSink(ctx); sink != nil {
			cmd.Stderr = io.MultiWriter(&errOut, sink)
		}
		out, err := cmd.Output()
		if err != nil {
			return append(out, errOut.Bytes()...), err
		}
		return out, nil
	}
}

// stderrKey carries a buffer that a Capturer copies the command's
// stderr into, whatever its exit status.
//
// A Capturer's return value is the command's stdout — which callers
// parse, as JSON for `restic snapshots --json` — plus stderr only when
// it fails. That is not enough for the one place stderr IS the answer:
// restic exits 0 when the storage refuses a delete, and the refusal is
// on stderr alone (measured: stdout empty, the 403 on stderr). Merging
// stderr into every capture would break the JSON; asking for it here,
// where it is evidence, does not.
type stderrKey struct{}

func withStderr(ctx context.Context, w *bytes.Buffer) context.Context {
	return context.WithValue(ctx, stderrKey{}, w)
}

func stderrSink(ctx context.Context) io.Writer {
	if w, ok := ctx.Value(stderrKey{}).(*bytes.Buffer); ok && w != nil {
		return w
	}
	return nil
}

// envKey carries extra environment for a Runner through the context,
// so a command that has just learned a password (init) can hand it to
// restic without putting it in this process's own environment, where
// anything it later starts would inherit it.
type envKey struct{}

// commandEnv is what a command this package starts runs with: this
// process's environment plus the settings. An operator running
// `status` or a restore by hand keeps their proxy, their locale, their
// ssh-agent. (init's checks do not come through here with the
// settings: they run as units with the job's own environment — see
// asJob.)
func commandEnv(ctx context.Context) []string {
	settings, _ := ctx.Value(envKey{}).([]string)
	return append(os.Environ(), settings...)
}

func withEnv(run Runner, env []string) Runner {
	return func(ctx context.Context, name string, args ...string) error {
		return run(context.WithValue(ctx, envKey{}, env), name, args...)
	}
}

func withCaptureEnv(capture Capturer, env []string) Capturer {
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return capture(context.WithValue(ctx, envKey{}, env), name, args...)
	}
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
	envFile := fl.String("env-file")
	env, err := LoadEnvFile(envFile)
	if err != nil {
		return caddy1, err
	}
	if err := checkSettingsRepository(env); err != nil {
		return caddy1, err
	}
	o := LaunchOptions{
		Self:        self,
		StagingRoot: fl.String("staging"),
		EnvFile:     envFile,
		User:        fl.String("user"),
	}
	unit := unitName(name)
	// Checked here to say so plainly; systemd refuses a second unit of
	// the same name anyway, so a backup that starts after this check
	// still cannot run beside the restore.
	if unitActive(ctx, unit+".service") {
		return caddy1, fmt.Errorf("%s is backing up right now; restore once it has finished (journalctl -u %s -f)", name, unit)
	}
	staging := filepath.Join(o.StagingRoot, name)
	if err := ensureStagingDir(staging, o.User); err != nil {
		return caddy1, err
	}
	// The restore unit's own dir, the only part of staging in its view.
	if err := ensureStagingDir(StagingRestore(staging), o.User); err != nil {
		return caddy1, err
	}
	// A rebuilt box restores before the first deploy, when liveswap has
	// not made the app's directories yet.
	if err := ensureShared(ctx, app.Shared, o.User, execRunner(os.Stderr)); err != nil {
		return caddy1, err
	}
	view := jobView{User: o.User, EnvFile: envFile, Home: StagingRestore(staging)}
	// This app's backups, and every clean-run record (an OR of the two
	// --tag flags).
	out, err := captureRunner()(ctx, "systemd-run", resticInUnit(view, []string{"snapshots", "--json", "--tag", "hotserve,app:" + name, "--tag", CleanTag})...)
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
	launch := RestoreArgs(app, o, snap.ID, del)
	say(os.Stdout, "%s: restoring in %s: %s", name, unit, quoteArgs(jobCommand(launch)))
	// An interrupt ends systemd-run — the client, not the unit, which
	// PID 1 owns and would otherwise go on writing the app's data with
	// nobody watching — so the unit is stopped here, at once, and the
	// message says what that leaves.
	err = execRunner(os.Stderr)(ctx, "systemd-run", launch...)
	if ctx.Err() != nil {
		_ = exec.Command("systemctl", "stop", unit+".service").Run() //nolint:gosec // a fixed program; the unit name is built here from an app name the config validated
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
func ensureShared(ctx context.Context, shared, username string, run Runner) error {
	if _, err := os.Stat(shared); err == nil {
		return nil
	}
	return run(ctx, "systemd-run", "--wait", "--collect", "--quiet",
		"--property=User="+username, "--property=Group="+username, "--property=UMask=0027",
		"/bin/mkdir", "-p", shared)
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
		Run:       execRunner(os.Stderr),
		Capture:   captureRunner(),
		Dump:      resticDump,
		Log:       os.Stdout,
	}
	if err := job.Execute(context.Background()); err != nil {
		return caddy1, err
	}
	return 0, nil
}

// resticDump writes one file out of a snapshot. O_EXCL: dst is a name
// the caller has just cleared, and a link that appeared there since is
// not followed.
func resticDump(ctx context.Context, snapshot, path, dst string) error {
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640) //nolint:gosec // a path built here, inside the unit's own view
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "restic", "dump", snapshot, path) //nolint:gosec // a fixed program; the snapshot and path come from the listing just read
	cmd.Env = commandEnv(ctx)
	cmd.Stdout = f
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
