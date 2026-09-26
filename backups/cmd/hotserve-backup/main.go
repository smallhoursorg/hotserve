// Command hotserve-backup backs up what hotserve's apps declare.
//
//	hotserve-backup setup <repository>
//	                        the account, the directories, the credential file, the repository; root, at a terminal
//	hotserve-backup run     one backup run; root; what the timer starts
//	hotserve-backup restore <app> [--snapshot <id>] [--to <dir>] [--no-pre-backup] [--yes]
//	                        one snapshot of one app, into place or into a new directory; root
//	hotserve-backup drill   fetch and check the newest snapshot of every app, installing nothing; root
//	hotserve-backup status  whether each app's backup is fresh and a restore of it proven; anyone.
//	                        Exits 0 healthy, 1 not, 3 when it could not tell
//	hotserve-backup validate <Caddyfile>
//	                        whether a run could plan from that Caddyfile, before it goes live; anyone
//
// and subcommands that are only ever the command of a unit one of those
// starts, each with a fixed view and no arguments — nothing an operator
// or an app wrote reaches a command line:
//
//	hotserve-backup plan    print the plan read from /etc/hotserve/Caddyfile
//	hotserve-backup dump    copy the databases /plan.json declares from /shared to /staging
//	hotserve-backup clean   empty /staging
//	hotserve-backup check   check the snapshot fetched into /restore
//	hotserve-backup install check it, and install it into /target if all of it is sound
//	hotserve-backup extract check it, and install what is sound into /target
//
// (The one string of a declaration's that does reach a command line is
// the directory part of a nested path, to `restic ls`: engine.verify.
// validate's argument is a path this process opens; it starts no unit.)
//
// It does not link Caddy, and hotserve does not run it: the two share a
// declaration format (liveswap/backupdecl) and nothing else.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/smallhoursorg/hotserve/backups/dump"
	"github.com/smallhoursorg/hotserve/backups/engine"
	"github.com/smallhoursorg/hotserve/backups/envfile"
	"github.com/smallhoursorg/hotserve/backups/plan"
	"github.com/smallhoursorg/hotserve/backups/record"
	"github.com/smallhoursorg/hotserve/backups/restore"
	"github.com/smallhoursorg/hotserve/backups/status"
	"github.com/smallhoursorg/hotserve/backups/unit"
	"github.com/smallhoursorg/hotserve/liveswap/backupdecl"
)

const caddyfile = "/etc/hotserve/Caddyfile"

const usage = `usage: hotserve-backup setup <repository>
       hotserve-backup run
       hotserve-backup restore <app> [--snapshot <id>] [--to <dir>] [--no-pre-backup] [--yes]
       hotserve-backup drill
       hotserve-backup status
       hotserve-backup validate <Caddyfile>`

// arguments says whether a command takes that many: restore takes its
// own, validate takes one file, setup one repository, and nothing else
// takes any — least of all the commands of units.
func arguments(name string, n int) bool {
	switch name {
	case "restore":
		return true
	case "validate", "setup":
		return n == 1
	}
	return n == 0
}

func main() {
	if len(os.Args) < 2 || !arguments(os.Args[1], len(os.Args)-2) {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err := command(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "hotserve-backup:", err)
		if errors.As(err, new(couldNotTell)) {
			os.Exit(3)
		}
		os.Exit(1)
	}
}

// couldNotTell is status not having been able to look — no setup, a
// record it cannot read — which is not the same news as backups that
// are unhealthy, and does not leave with the same status: 3, not 1.
type couldNotTell struct{ error }

func (e couldNotTell) Unwrap() error { return e.error }

func command(name string, args []string) error {
	// SIGINT, SIGTERM and SIGHUP cancel the context; a run then stops the unit
	// it is waiting on, by name, and confirms it gone before returning.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()
	switch name {
	case "setup":
		return setup(ctx, args[0])
	case "run":
		return run(ctx)
	case "restore":
		return restoreApp(ctx, args)
	case "drill":
		return drill(ctx)
	case "status":
		return showStatus(ctx)
	case "validate":
		return validate(ctx, args[0])
	case "check":
		return settle(ctx, restore.CheckOnly)
	case "install":
		return settle(ctx, restore.AllOrNothing)
	case "extract":
		return settle(ctx, restore.WhatIsSound)
	case "plan":
		return printPlan(ctx)
	case "dump":
		return dumpDatabases(ctx)
	case "clean":
		return clean("/staging")
	}
	return fmt.Errorf("unknown command %q", name)
}

func config() engine.Config {
	return engine.Config{
		ConfigDir: "/etc/hotserve", EnvFile: "/etc/hotserve-backup/repository.env", OldEnvFile: "/etc/hotserve/backup.env",
		StateDir: "/var/lib/hotserve-backup", RunDir: "/run/hotserve-backup",
		Self: "/usr/bin/hotserve-backup", Restic: "/usr/bin/restic",
		BindsTo: ownService(),
	}
}

// runner connects to the system manager, which takes root.
func runner(ctx context.Context, what string) (*unit.Runner, error) {
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("a %s starts system units, which needs root: sudo hotserve-backup %s", what, what)
	}
	return unit.NewSystemRunner(ctx)
}

// setup needs root and a terminal, and says so before anything else:
// the secrets are typed there, with echo off, never given on a command
// line. The engine checks what else can be known to fail before it
// asks for one.
func setup(ctx context.Context, repository string) error {
	if os.Geteuid() != 0 {
		return errors.New("a setup starts system units, which needs root: sudo hotserve-backup setup <repository>")
	}
	r, err := unit.NewSystemRunner(ctx)
	if err != nil {
		return err
	}
	defer r.Close()
	t, err := openTTY()
	if err != nil {
		return err
	}
	defer t.Close()
	rep, err := engine.Setup(ctx, config(), r, engine.SetupOptions{Repository: repository, Terminal: t})
	if err != nil {
		return err
	}
	t.Say(fmt.Sprintf("credentials: %s (root, 0600)", config().EnvFile))
	if len(rep.Apps) > 0 {
		t.Say("next: sudo hotserve-backup run, then hotserve-backup status. Nothing runs it on a schedule on this branch: until the package's timer exists, run it hourly from a timer or cron entry of your own.")
	} else {
		t.Say("next: declare a backup in an app's Caddyfile block (liveswap/README.md), then sudo hotserve-backup run")
	}
	return nil
}

func run(ctx context.Context) error {
	r, err := runner(ctx, "run")
	if err != nil {
		return err
	}
	defer r.Close()
	st, err := engine.Run(ctx, config(), r)
	if st != nil {
		report(st)
	}
	if err != nil {
		return err
	}
	for _, app := range st.Apps {
		if app == nil || (app.Class != record.OK && app.Class != record.Pending) {
			return errors.New("not every app was backed up")
		}
	}
	return nil
}

// report is one line an app, and never says "backed up" of anything
// that was not: the class, and for an ok run the snapshot and what was
// found in it.
func report(st *record.Status) {
	names := make([]string, 0, len(st.Apps))
	for n := range st.Apps {
		names = append(names, n)
	}
	sort.Strings(names)
	if st.Warning != "" {
		fmt.Println("warning:", st.Warning)
	}
	if len(names) == 0 && st.Error == "" {
		fmt.Println("no app declares a backup; nothing was backed up")
	}
	for _, n := range names {
		app := st.Apps[n]
		if app == nil {
			continue
		}
		line := fmt.Sprintf("%s: %s", n, app.Class)
		if app.Class == record.NotRun && app.LastOK != nil {
			line += fmt.Sprintf(" (last ok %s, snapshot %.8s)", app.LastOK.Time.Format("2006-01-02 15:04 MST"), app.LastOK.ID)
		}
		if app.Class == record.OK {
			var found []string
			for _, it := range app.Items {
				found = append(found, it.Kind+" "+it.Path)
			}
			line += fmt.Sprintf(": snapshot %.8s holds %s", app.Snapshot.ID, strings.Join(found, ", "))
		} else if app.Detail != "" {
			line += ": " + app.Detail
		}
		fmt.Println(line)
		// A run that drilled says what the drill found; one that did not
		// says nothing of it.
		if app.RestoreDrill != nil && !app.RestoreDrill.Time.Before(st.Started) {
			fmt.Printf("%s: restore not proven: %s\n", n, app.RestoreDrill.Detail)
		} else if app.RestoreProven != nil && !app.RestoreProven.Time.Before(st.Started) {
			fmt.Printf("%s: restore proven: snapshot %.8s was fetched and checked whole\n", n, app.RestoreProven.Snapshot.ID)
		}
	}
}

// showStatus needs no root: the record is for everyone to read, and
// the manager tells anyone what is running.
func showStatus(ctx context.Context) error {
	cfg := config()
	// Only a file that is not there says so. Any other answer — a
	// directory this user may not search — is not "not set up".
	env, err := os.Lstat(cfg.EnvFile)
	if errors.Is(err, fs.ErrNotExist) {
		return couldNotTell{fmt.Errorf("backups are not set up: %s is not there%s", cfg.EnvFile, engine.OldEnvFileNote(cfg))}
	} else if err != nil {
		return couldNotTell{fmt.Errorf("whether backups are set up cannot be told from here: %w", err)}
	}
	st, err := record.Read(filepath.Join(cfg.StateDir, "status.json"))
	if err != nil {
		return couldNotTell{fmt.Errorf("the record of the last run could not be read: %w", err)}
	}
	// As root, the file itself: where a line of it is not read as it
	// was written, the manager's rules being what they are. Said, and
	// nothing more: what the run makes of the file the run says.
	if os.Geteuid() == 0 {
		if raw, err := os.ReadFile(cfg.EnvFile); err == nil {
			v, findings := envfile.Parse(raw)
			for _, l := range envfile.Lint(v, findings) {
				fmt.Printf("warning: %s: %s\n", cfg.EnvFile, l)
			}
			if repo, ok := v["RESTIC_REPOSITORY"]; ok {
				if err := engine.RepositoryUsable(repo, cfg.EnvFile); err != nil {
					fmt.Printf("warning: %s: RESTIC_REPOSITORY: %v\n", cfg.EnvFile, err)
				}
			}
		}
	}
	in := status.Input{Record: st, Now: time.Now(), SetUp: env.ModTime()}
	var active []unit.Active
	active, in.RunningErr = unit.ListActive(ctx, engine.UnitPattern)
	for _, a := range active {
		if role, app, ok := engine.ParseUnitName(a.Name); ok {
			in.Running = append(in.Running, status.Running{Role: role, App: app, State: a.State, Since: a.Since})
		} else {
			in.Unrecognised = append(in.Unrecognised, a.Name)
		}
	}
	lines, healthy := status.Report(in)
	for _, l := range lines {
		fmt.Println(l)
	}
	if !healthy {
		return errors.New("not every app has a fresh backup and a restore proven lately")
	}
	return nil
}

// validate says whether a run could turn this Caddyfile into a plan —
// for whoever is about to make it live, before they do. It starts no
// unit, takes no lock and needs no root; the run's own plan step does
// the same to the live file, as another user, inside a view that holds
// the config directory and nothing else. What that view and that user
// would make of an import it cannot say from here, where the whole
// filesystem is in view: that is `hotserve validate`'s, which has the
// adapter's own word on where each backup block comes from.
func validate(ctx context.Context, file string) error {
	abs, err := filepath.Abs(file)
	if err != nil {
		return err
	}
	if st, err := os.Stat(abs); err != nil {
		return err
	} else if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a file", abs)
	}
	ins, err := plan.Inspect(ctx, abs, config().ConfigDir)
	if err != nil {
		// It quotes the adapter, which quotes the Caddyfile: text fit for
		// a terminal only once it holds no control character.
		return errors.New(record.Clean(err.Error()))
	}
	names := ins.Plan.Names()
	if len(names) == 0 {
		fmt.Println("no app declares a backup: a run would back nothing up")
	} else {
		fmt.Printf("a run would back up %s, under %s\n", record.Clean(strings.Join(names, ", ")), record.Text(ins.Plan.Root))
	}
	if len(ins.UndeclaredByEnv) > 0 {
		fmt.Printf("an app named through the environment variable(s) %s declares no backup: nothing of it would be backed up\n", record.Clean(strings.Join(ins.UndeclaredByEnv, ", ")))
	}
	for _, n := range ins.Undeclared {
		fmt.Printf("%s declares no backup: nothing of it would be backed up\n", record.Text(n))
	}
	return nil
}

func restoreApp(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	o := engine.RestoreOptions{App: args[0]}
	var yes bool
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.Snapshot, "snapshot", "", "")
	fs.StringVar(&o.To, "to", "", "")
	fs.BoolVar(&o.NoPreBackup, "no-pre-backup", false, "")
	fs.BoolVar(&yes, "yes", false, "")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return errors.New(usage)
	}
	// A flag given with nothing in it — --to "$DIR", and DIR unset — is
	// not the same restore without the flag: that one overwrites.
	var empty string
	fs.Visit(func(f *flag.Flag) {
		if f.Value.String() == "" {
			empty = f.Name
		}
	})
	if empty != "" {
		return fmt.Errorf("--%s was given with nothing in it", empty)
	}
	if !yes {
		o.Confirm = confirm(ctx)
	}
	r, err := runner(ctx, "restore")
	if err != nil {
		return err
	}
	defer r.Close()
	rep, err := engine.Restore(ctx, config(), r, o)
	if rep != nil {
		reportRestore(rep)
	}
	return err
}

// answerWithin is how long a restore waits to be answered. It holds the
// run lock while it waits, and a backup run that comes due then does
// nothing: an abandoned question must not be what ends a box's backups.
const answerWithin = 10 * time.Minute

// confirm asks on standard input, and takes the app's own name, typed,
// and nothing else for a yes: not "y", which a hand types on its own.
// With nobody there to answer it says how to go without being asked,
// and that is a no; so is an interrupt, and so is silence.
func confirm(ctx context.Context) func(engine.RestoreAsk) bool {
	return func(a engine.RestoreAsk) bool {
		fmt.Printf("%s: restore snapshot %.8s, made %s, into %s?\n", a.App, a.Snapshot.ID, a.Snapshot.Time.Format("2006-01-02 15:04 MST"), a.Into)
		if a.LastOK != nil {
			fmt.Println(notLastOK(a.App, a.LastOK))
		}
		if a.PreBackup {
			fmt.Println("What is there is backed up first. Its databases are then replaced and its files overwritten; what the snapshot does not hold is left.")
		} else {
			fmt.Println("Its databases are replaced and its files overwritten, with no backup first; what the snapshot does not hold is left.")
		}
		fmt.Printf("Type the app's name, %s, to go on: ", a.App)
		type reply struct {
			line string
			err  error
		}
		said := make(chan reply, 1)
		go func() {
			line, err := bufio.NewReader(os.Stdin).ReadString('\n')
			said <- reply{line, err}
		}()
		select {
		case r := <-said:
			if r.err != nil && r.line == "" {
				fmt.Println("\nnobody answered: pass --yes to restore without being asked")
				return false
			}
			return strings.TrimSpace(r.line) == a.App
		case <-ctx.Done():
			fmt.Println()
			return false
		case <-time.After(answerWithin):
			fmt.Printf("\nno answer in %s\n", answerWithin)
			return false
		}
	}
}

// notLastOK says that the snapshot is not the last one a run ended ok
// on, and which that is: a run that ended incomplete leaves a snapshot
// with files left out that no check at restore can see.
func notLastOK(app string, ok *record.Snapshot) string {
	return fmt.Sprintf("%s: this is not the last snapshot a backup run ended ok on — that is %.8s, made %s. A run that ended incomplete leaves files out of a directory it read, which a restore cannot tell; --snapshot %.8s restores that one", app, ok.ID, ok.Time.Format("2006-01-02 15:04 MST"), ok.ID)
}

// reportRestore says what was restored, what was left and what was not,
// and never "restored" of an item that was not.
func reportRestore(rep *engine.RestoreReport) {
	if rep.Warning != "" {
		fmt.Println("warning:", rep.Warning)
	}
	if rep.LastOK != nil {
		fmt.Println(notLastOK(rep.App, rep.LastOK))
	}
	if rep.PreBackup != nil {
		if rep.PreBackupClass == record.OK {
			fmt.Printf("%s: backed up first: snapshot %.8s (restore --snapshot %.8s puts back what was there)\n", rep.App, rep.PreBackup.ID, rep.PreBackup.ID)
		} else {
			fmt.Printf("%s: backed up first, and that backup ended %s: %s — snapshot %.8s holds what it could, not all of what was there\n", rep.App, rep.PreBackupClass, rep.PreBackupDetail, rep.PreBackup.ID)
		}
	}
	var restored, not []string
	for _, it := range rep.Items {
		if it.OK {
			restored = append(restored, it.Kind+" "+it.Path)
		} else {
			not = append(not, fmt.Sprintf("%s %s (%s)", it.Kind, it.Path, it.Detail))
		}
	}
	from := fmt.Sprintf("snapshot %.8s of %s", rep.Snapshot.ID, rep.Snapshot.Time.Format("2006-01-02 15:04 MST"))
	switch {
	case len(restored) > 0:
		// The items are the ones the snapshot's own plan.json declares.
		fmt.Printf("%s: restored from %s into %s: %s\n", rep.App, from, rep.Into, strings.Join(restored, ", "))
	case len(rep.Items) > 0:
		fmt.Printf("%s: nothing was restored from %s\n", rep.App, from)
	default:
		// No answer came back: the error that follows says what is known.
	}
	if len(not) > 0 {
		fmt.Printf("%s: not restored: %s\n", rep.App, strings.Join(not, "; "))
	}
	list := func(what string, names []string, more int) {
		if len(names) == 0 {
			return
		}
		line := what + ": " + strings.Join(names, ", ")
		if more > 0 {
			line += fmt.Sprintf(", and %d more", more)
		}
		fmt.Println(line)
	}
	list("left in place, not in the snapshot", rep.Left, rep.LeftMore)
	list("in the snapshot and not installed, being neither files, directories nor links (a FIFO, socket or device)", rep.Skipped, rep.SkippedMore)
}

func drill(ctx context.Context) error {
	r, err := runner(ctx, "drill")
	if err != nil {
		return err
	}
	defer r.Close()
	st, err := engine.Drill(ctx, config(), r)
	unproven := false
	if st != nil {
		names := make([]string, 0, len(st.Apps))
		for n := range st.Apps {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			app := st.Apps[n]
			switch {
			case app == nil:
			case app.RestoreDrill != nil:
				unproven = true
				line := fmt.Sprintf("%s: restore not proven", n)
				if app.RestoreDrill.Snapshot.ID != "" {
					line += fmt.Sprintf(": snapshot %.8s", app.RestoreDrill.Snapshot.ID)
				}
				line += ": " + app.RestoreDrill.Detail
				if app.RestoreProven != nil {
					line += fmt.Sprintf(" (last proven: snapshot %.8s, on %s)", app.RestoreProven.Snapshot.ID, app.RestoreProven.Time.Format("2006-01-02 15:04 MST"))
				}
				fmt.Println(line)
			case app.RestoreProven != nil:
				fmt.Printf("%s: restore proven: snapshot %.8s, on %s\n", n, app.RestoreProven.Snapshot.ID, app.RestoreProven.Time.Format("2006-01-02 15:04 MST"))
			default:
				fmt.Printf("%s: nothing to prove: the repository holds no snapshot of it\n", n)
			}
		}
	}
	if err != nil {
		return err
	}
	if unproven {
		return errors.New("not every app's restore was proven")
	}
	return nil
}

// settle is the unit that reads a fetched snapshot. Each item is
// reported on its own, so the unit's exit status says only whether it
// could report at all.
func settle(ctx context.Context, mode restore.Mode) error {
	return json.NewEncoder(os.Stdout).Encode(restore.Run(ctx, "/restore", "/target", mode))
}

var serviceRe = regexp.MustCompile(`^[A-Za-z0-9:_.@-]{1,200}\.service$`)

// ownService is the unit this run is, when it is one: the units a run
// starts are bound to it, so the manager ends them if the run is killed.
// It is what the unit says it is (Environment=HOTSERVE_BACKUP_UNIT=%n in
// the unit file), not a guess from the cgroup: run from a shell inside
// tmux, or by cron, the cgroup names a service that is somebody else's,
// and binding to it would end a backup when sshd restarts — or fail
// every unit, where the system manager has no such service. From a
// shell there is none, and the signal handler and the next run's sweep
// do that work.
func ownService() string {
	if name := os.Getenv("HOTSERVE_BACKUP_UNIT"); serviceRe.MatchString(name) {
		return name
	}
	return ""
}

func printPlan(ctx context.Context) error {
	p, err := plan.Make(ctx, caddyfile)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(p)
}

func dumpDatabases(ctx context.Context) error {
	raw, err := os.ReadFile("/plan.json")
	if err != nil {
		return err
	}
	var decl backupdecl.Config
	if err := json.Unmarshal(raw, &decl); err != nil {
		return err
	}
	if err := decl.Validate(); err != nil {
		return err
	}
	// Each database is reported on its own, so the unit's exit status
	// says only whether it could report at all.
	return json.NewEncoder(os.Stdout).Encode(dump.Databases(ctx, "/shared", "/staging", decl.SQLite))
}

// clean empties dir without removing it (it is a mount point), and
// without following anything out of it. What is in it may be a tree out
// of a snapshot, whose modes are the app's: a directory closed to
// writing — or to its owner altogether — is opened first, or what is in
// it could never be removed, and plaintext would stay for good.
func clean(dir string) error {
	// Best effort: what the opening could not reach, the removing says.
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() && p != dir { // by lstat: a link to a directory is not one
			_ = os.Chmod(p, 0o700) //nolint:gosec // a directory about to be removed
		}
		return nil
	})
	entries, err := os.ReadDir(dir)
	errs := []error{err}
	for _, e := range entries {
		errs = append(errs, os.RemoveAll(filepath.Join(dir, e.Name())))
	}
	return errors.Join(errs...)
}
