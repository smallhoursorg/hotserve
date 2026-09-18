package backup

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	caddycmd "github.com/caddyserver/caddy/v2/cmd"
)

// caddy1 is the conventional generic failure exit code in Caddy's
// command funcs (caddy.ExitCodeFailedStartup).
const caddy1 = 1

func init() {
	caddycmd.RegisterCommand(caddycmd.Command{
		Name:  "backup",
		Usage: "init <repository> | run | status | restic -- <args> | app <flags>",
		Short: "Back up what liveswap apps declare as state",
		Long: `Copies each app's declared state (see the 'state' directive) to a
restic repository. The repository and its credentials come from an
environment file, by default /etc/hotserve/backup.env, which systemd
reads as root; nothing about backups is configured in the Caddyfile.

  hotserve backup init <repository> [KEY=VALUE...]
      Writes the environment file, creates the repository if it is
      new, and checks whether these credentials can delete from it —
      they should not be able to, so that someone who takes the box
      cannot erase its backups. Provider credentials are passed as
      KEY=VALUE, or in a root-only file with --credentials-file, which
      keeps the storage key out of shell history and out of
      /proc/*/cmdline while init runs. A password is
      generated and printed once; keep it somewhere else too, because
      nothing can recover it. Rebuilding a box means pointing init at
      the repository that already exists, with the password it was
      made with: --password-file <path> (sudo does not carry
      RESTIC_PASSWORD through).

  hotserve backup run
      Asks the admin API which apps declare state and backs up each
      one in its own sandboxed, short-lived systemd unit. This is what
      the packaged timer runs; it needs root, to create those units.

  hotserve backup restic -- <restic arguments...>
      Runs restic against this box's repository, as the user the
      backups belong to, with the settings from the environment file.
      Running restic on its own finds no repository, because that file
      is root-only. This is how the restores in docs/backups.md reach
      the repository:
          sudo hotserve backup restic -- snapshots --tag app:blog
          sudo hotserve backup restic -- restore latest --tag app:blog --target /

  hotserve backup status [--check]
      One line per app that declares state: what it declares, how many
      snapshots it has, and how old the newest one is. --check exits
      non-zero when an app has no current backup, for a monitor.

  hotserve backup app --name <app> --shared <dir> [--staging <dir>]
                      <kind>:<path> [<kind>:<path>...]
      One app's backup, in the current process. Each entry is written
      the way the Caddyfile declares it, as sqlite:app.db or
      files:uploads, with paths relative to the app's shared dir:
      every sqlite entry is copied with VACUUM INTO into --staging,
      then restic gets the copies plus every files entry. This is what
      'run' launches inside the sandbox; running it by hand is
      supported and does exactly the same thing, unsandboxed.

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
	default:
		return caddy1, fmt.Errorf("unknown subcommand %q: want init, run, status, restic or app", args[0])
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
	statuses, err := Status(ctx, apps, withCaptureEnv(captureRunner(), env), fl.String("staging"))
	if err != nil {
		return caddy1, err
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
		return caddy1, fmt.Errorf("say where the backups go, e.g. `hotserve backup init s3:s3.us-west-004.backblazeb2.com/my-bucket` — or a path, for a disk you mount")
	}
	extra := args[1:] // provider credentials as KEY=VALUE
	if path := fl.String("credentials-file"); path != "" {
		fromFile, err := LoadEnvFile(path)
		if err != nil {
			return caddy1, err
		}
		extra = append(fromFile, extra...)
	}
	o := InitOptions{
		Repository:   args[0],
		Extra:        extra,
		EnvFile:      fl.String("env-file"),
		Password:     os.Getenv("RESTIC_PASSWORD"),
		PasswordFile: fl.String("password-file"),
		User:         fl.String("user"),
		Force:        fl.Bool("force"),
	}
	// The existence and probe checks are questions, not failures, so
	// their output is captured rather than shown: restic's "repository
	// does not exist" while init is about to create one reads as an
	// error when it is the expected answer.
	if err := Init(context.Background(), o, execRunner(os.Stderr), captureQuiet(), os.Stdout); err != nil {
		return caddy1, err
	}
	return 0, nil
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
	// systemd hands the jobs this file; read it here too, for the one
	// thing the launcher has to know: whether the repository is a
	// path on this box, which then has to be inside each job's view.
	env, err := LoadEnvFile(envFile)
	if err != nil {
		return caddy1, err
	}
	repoPath, err := LocalRepositoryPath(env)
	if err != nil {
		return caddy1, err
	}
	o := LaunchOptions{
		Self:           self,
		StagingRoot:    fl.String("staging"),
		EnvFile:        envFile,
		User:           fl.String("user"),
		RepositoryPath: repoPath,
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
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Env = append(os.Environ(), runnerEnv(ctx)...)
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
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Env = append(os.Environ(), runnerEnv(ctx)...)
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
// the delete check classifies. Discarding it made every append-only
// repository report as "unknown".
func captureQuiet() Capturer {
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Env = append(os.Environ(), runnerEnv(ctx)...)
		var errOut bytes.Buffer
		cmd.Stderr = &errOut
		out, err := cmd.Output()
		if err != nil {
			return append(out, errOut.Bytes()...), err
		}
		return out, nil
	}
}

// envKey carries extra environment for a Runner through the context,
// so a command that has just learned a password (init) can hand it to
// restic without putting it in this process's own environment, where
// anything it later starts would inherit it.
type envKey struct{}

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

func runnerEnv(ctx context.Context) []string {
	env, _ := ctx.Value(envKey{}).([]string)
	return env
}
