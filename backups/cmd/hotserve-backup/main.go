// Command hotserve-backup backs up what hotserve's apps declare.
//
//	hotserve-backup run     one backup run; root; what the timer starts
//
// and three subcommands that are only ever the command of a unit a run
// starts, each with a fixed view and no arguments — nothing an operator
// or an app wrote reaches a command line:
//
//	hotserve-backup plan    print the plan read from /etc/hotserve/Caddyfile
//	hotserve-backup dump    copy the databases /plan.json declares from /shared to /staging
//	hotserve-backup clean   empty /staging
//
// It does not link Caddy, and hotserve does not run it: the two share a
// declaration format (liveswap/backupdecl) and nothing else.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/smallhoursorg/hotserve/backups/dump"
	"github.com/smallhoursorg/hotserve/backups/engine"
	"github.com/smallhoursorg/hotserve/backups/plan"
	"github.com/smallhoursorg/hotserve/backups/record"
	"github.com/smallhoursorg/hotserve/backups/unit"
	"github.com/smallhoursorg/hotserve/liveswap/backupdecl"
)

const caddyfile = "/etc/hotserve/Caddyfile"

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: hotserve-backup run")
		os.Exit(2)
	}
	if err := command(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "hotserve-backup:", err)
		os.Exit(1)
	}
}

func command(name string) error {
	// SIGINT and SIGTERM cancel the context; a run then stops the unit
	// it is waiting on, by name, and confirms it gone before returning.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()
	switch name {
	case "run":
		return run(ctx)
	case "plan":
		return printPlan(ctx)
	case "dump":
		return dumpDatabases(ctx)
	case "clean":
		return clean("/staging")
	}
	return fmt.Errorf("unknown command %q", name)
}

func run(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("a run starts system units, which needs root: sudo hotserve-backup run")
	}
	r, err := unit.NewSystemRunner(ctx)
	if err != nil {
		return err
	}
	defer r.Close()
	st, err := engine.Run(ctx, engine.Config{
		Caddyfile: caddyfile, ConfigDir: "/etc/hotserve", EnvFile: "/etc/hotserve/backup.env",
		StateDir: "/var/lib/hotserve-backup", RunDir: "/run/hotserve-backup",
		Self: "/usr/bin/hotserve-backup", Restic: "/usr/bin/restic",
		BindsTo: ownService(),
	}, r)
	if st != nil {
		report(st)
	}
	if err != nil {
		return err
	}
	for _, app := range st.Apps {
		if app.Class != record.OK && app.Class != record.Pending {
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
	if len(names) == 0 && st.Error == "" {
		fmt.Println("no app declares a backup; nothing was backed up")
	}
	for _, n := range names {
		app := st.Apps[n]
		line := fmt.Sprintf("%s: %s", n, app.Class)
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
	}
}

var serviceRe = regexp.MustCompile(`/([^/]+\.service)$`)

// ownService is the service this process runs in, when it runs in one:
// the units a run starts are bound to it, so the manager ends them if
// the run is killed. From a shell there is none, and the signal handler
// and the next run's sweep do that work.
func ownService() string {
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	if m := serviceRe.FindStringSubmatch(strings.TrimSpace(string(raw))); m != nil && !strings.HasPrefix(m[1], "user@") {
		return m[1]
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
// without following anything out of it.
func clean(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		errs = append(errs, os.RemoveAll(filepath.Join(dir, e.Name())))
	}
	return errors.Join(errs...)
}
