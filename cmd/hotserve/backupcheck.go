package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	caddycmd "github.com/caddyserver/caddy/v2/cmd"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/smallhoursorg/hotserve/liveswap"
)

// gate holds a Caddyfile that declares a backup to what a backup run
// can read of it, before Caddy's own command runs. A run (hotserve-backup)
// adapts the Caddyfile inside a view that holds the Caddyfile's own
// directory and nothing else, as an account that owns nothing there: a
// backup declared in a file outside that directory, or in one that
// account may not read, is simply not in its plan, and the app is not
// backed up while every run says ok.
//
// validate and reload refuse such a Caddyfile — a refused reload keeps
// the running config [measured] — and run serves it, saying why: a
// start that refused would take every site down, after a reboot or an
// unattended upgrade, over something the backup run says loudly. It
// returns whether the command is to go no further.
func gate(args []string, stderr io.Writer) (stop bool) {
	if len(args) == 0 {
		return false
	}
	cmd := args[0]
	if cmd != "validate" && cmd != "reload" && cmd != "run" {
		return false
	}
	warning, err := checkBackups(cmd, args[1:])
	switch {
	case err != nil && cmd == "run":
		_, _ = fmt.Fprintf(stderr, "ERROR: %v (served all the same; `hotserve reload` refuses this until it is mended)\n", err)
	case err != nil:
		_, _ = fmt.Fprintf(stderr, "Error: %v\n", err)
		stop = true
	}
	if warning != "" {
		_, _ = fmt.Fprintf(stderr, "WARNING: %s\n", warning)
	}
	return stop
}

// backupCaddyfile is the Caddyfile hotserve-backup reads, and
// backupBinary hotserve-backup itself (backups/cmd/hotserve-backup):
// both fixed by its package, which ships beside this one. Variables only
// so a test can stand others in.
var (
	backupCaddyfile = "/etc/hotserve/Caddyfile"
	backupBinary    = "/usr/bin/hotserve-backup"
)

// commandFlags parses a command's arguments with the flags Caddy's own
// command declares — its own definition, whatever Caddy is linked in,
// never a copy that could drift — so that every form Caddy takes
// (`-cFILE`, `-fc FILE`, `--config=FILE`) names the same file here. An
// error is Caddy's to report.
func commandFlags(name string, args []string) (*pflag.FlagSet, bool) {
	c, ok := caddycmd.Commands()[name]
	if !ok || c.CobraFunc == nil {
		return nil, false
	}
	cmd := &cobra.Command{Use: name}
	c.CobraFunc(cmd)
	fs := cmd.Flags()
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return nil, false
	}
	return fs, true
}

// checkBackups loads the config the command's flags name with Caddy's
// own LoadConfig — its default Caddyfile, its rule for what is one, its
// adapt — and holds every file a backup declaration came from to the
// Caddyfile's directory. A config that does not load says nothing here:
// Caddy's own command says it next, in its own words. So does a config
// that is not a Caddyfile, or not a file.
//
// What this does not do: load `--envfile`. Caddy loads it before it
// adapts, and a {$NAME} it sets can change what the Caddyfile imports;
// here the Caddyfile is adapted in the environment the command was
// started in. The packaged service passes none, and hotserve-backup
// refuses an import that depends on a variable.
func checkBackups(cmd string, args []string) (warning string, err error) {
	fs, ok := commandFlags(cmd, args)
	if !ok {
		return "", nil
	}
	file, _ := fs.GetString("config")
	adapter, _ := fs.GetString("adapter")
	// Stdin or a pipe is Caddy's to read, once; and a pipe nobody writes
	// to would never answer.
	probe := file
	if probe == "" {
		probe = "Caddyfile" // what LoadConfig reads with none
	}
	if st, err := os.Stat(probe); file == "-" || err == nil && !st.Mode().IsRegular() { //nolint:gosec // the config the command was given, looked at before Caddy reads it
		return "", nil
	}
	liveswap.ClearBackupSources()
	_, file, adapter, err = caddycmd.LoadConfig(file, adapter)
	if err != nil || adapter != "caddyfile" {
		return "", nil
	}
	sources := liveswap.BackupSources()
	if len(sources) == 0 {
		return "", nil
	}
	main, err := filepath.Abs(file)
	if err != nil {
		return "", err
	}
	// hotserve-backup reads one Caddyfile. A server run from another,
	// declaring a backup, runs what no backup run reads: an app only in
	// it is never backed up, and status says all is well. Refused where
	// hotserve-backup is installed (a start is not refused: gate), said
	// where it is not; a candidate being validated is not asked.
	var elsewhere string
	if cmd != "validate" && !sameFile(main, backupCaddyfile) {
		elsewhere = fmt.Sprintf("this %s runs %s, which declares backups, but hotserve-backup reads %s: backups follow that file, not this one, and an app only in this one would not be backed up", cmd, main, backupCaddyfile)
		if _, err := os.Stat(backupBinary); err != nil {
			warning = elsewhere + " (were hotserve-backup installed, this would be refused)"
			elsewhere = ""
		} else {
			elsewhere += fmt.Sprintf(" — to see what a backup run would make of this file, `hotserve-backup validate %s`; to make it the one backups read, put it at %s (bin/push does)", main, backupCaddyfile)
		}
	}
	dir := filepath.Dir(main)
	// What a backup run's view holds is the directory itself, and so what
	// a file leads to is judged against where the directory leads.
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return warning, err
	}
	var problems []string
	seen := map[string]bool{}
	say := func(format string, a ...any) {
		if p := fmt.Sprintf(format, a...); !seen[p] {
			seen[p] = true
			problems = append(problems, p)
		}
	}
	// check says what a backup run could not read of file: that it is not
	// under the directory, as written or where it leads through a link,
	// or that others may not read it or enter a directory on its way.
	check := func(what, file string) {
		// Only the main Caddyfile is ever relative, as it was given here.
		if abs, err := filepath.Abs(file); err == nil {
			file = abs
		}
		if !under(file, dir) {
			say("%s is read from %s, outside %s", what, file, dir)
			return
		}
		real, err := filepath.EvalSymlinks(file)
		switch {
		case err != nil:
			say("%s is read from %s, which cannot be followed: %v", what, file, err)
			return
		case !under(real, realDir):
			say("%s is read from %s, which leads to %s, outside %s", what, file, real, dir)
			return
		}
		if st, err := os.Stat(real); err == nil && st.Mode().Perm()&0o004 == 0 {
			say("%s is read from %s, which others may not read", what, file)
		}
		for d := filepath.Dir(real); ; d = filepath.Dir(d) {
			if st, err := os.Stat(d); err == nil && st.Mode().Perm()&0o001 == 0 {
				say("%s is read from under %s, which others may not enter", what, d)
			}
			if !under(d, realDir) {
				break // realDir itself, the last
			}
		}
	}
	if st, err := os.Stat(main); err == nil && st.Mode().Perm()&0o004 == 0 {
		say("the Caddyfile, %s, may not be read by others", main)
	}
	for _, s := range sources {
		if s.App == "" {
			check("the liveswap root", s.File)
		} else {
			check(s.App+"'s backup", s.File)
		}
	}
	var errs []string
	if elsewhere != "" {
		errs = append(errs, elsewhere)
	}
	if len(problems) > 0 {
		errs = append(errs, fmt.Sprintf("a backup run would not see what this Caddyfile declares: %s — a backup run reads the Caddyfile inside a view that holds %s and nothing else, as an account that owns nothing there, so keep every file a backup is declared in under %s, readable by others (0644, directories 0755)", strings.Join(problems, "; "), dir, dir))
	}
	if len(errs) == 0 {
		return warning, nil
	}
	return warning, errors.New(strings.Join(errs, "; and "))
}

// sameFile says whether a and b are one file: the same path, or the same
// file where each leads.
func sameFile(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	sa, errA := os.Stat(a)
	sb, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(sa, sb)
}

// under says whether p is below dir.
func under(p, dir string) bool {
	return strings.HasPrefix(p, strings.TrimSuffix(dir, string(filepath.Separator))+string(filepath.Separator))
}
