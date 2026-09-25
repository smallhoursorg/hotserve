package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/caddyserver/caddy/v2/caddyconfig"
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
	err := checkBackups(cmd, args[1:])
	if err == nil {
		return false
	}
	if cmd == "run" {
		_, _ = fmt.Fprintf(stderr, "ERROR: %v (served all the same; `hotserve validate` and `hotserve reload` refuse this Caddyfile until it is mended)\n", err)
		return false
	}
	_, _ = fmt.Fprintf(stderr, "Error: %v\n", err)
	return true
}

// commandFlags parses a command's flags as Caddy's own command does —
// the same flag library, the same flags (caddy/cmd/commands.go) — so
// that every form Caddy takes (`-cFILE`, `-fc FILE`, `--config=FILE`)
// names the same file here. An error is Caddy's to report.
func commandFlags(cmd string, args []string) (file, adapter string, ok bool) {
	fs := pflag.NewFlagSet(cmd, pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringP("config", "c", "", "")
	fs.StringP("adapter", "a", "", "")
	switch cmd {
	case "validate":
		fs.StringSlice("envfile", nil, "")
	case "reload":
		fs.String("address", "", "")
		fs.BoolP("force", "f", false, "")
	case "run":
		fs.StringSlice("envfile", nil, "")
		fs.BoolP("environ", "e", false, "")
		fs.BoolP("resume", "r", false, "")
		fs.BoolP("watch", "w", false, "")
		fs.String("pidfile", "", "")
		fs.String("pingback", "", "")
	}
	if err := fs.Parse(args); err != nil {
		return "", "", false
	}
	file, _ = fs.GetString("config")
	adapter, _ = fs.GetString("adapter")
	return file, adapter, true
}

// isCaddyfile is Caddy's own rule for when a config is adapted as a
// Caddyfile (caddy/cmd/main.go): the adapter says so, or, with none, the
// name begins "caddyfile" or ends ".caddyfile", any case, and is not
// ".json". Anything else Caddy reads as JSON.
func isCaddyfile(file, adapter string) bool {
	if adapter == "caddyfile" {
		return true
	}
	base := strings.ToLower(filepath.Base(file))
	return adapter == "" && filepath.Ext(base) != ".json" &&
		(strings.HasPrefix(base, "caddyfile") || strings.HasSuffix(base, ".caddyfile"))
}

// checkBackups adapts the Caddyfile the command's flags name, as Caddy
// would, and holds every file a backup declaration came from to the
// Caddyfile's directory. An adapt that fails says nothing here: Caddy's
// own command says it next, in its own words. So does a config that is
// not a Caddyfile, or not a file.
//
// What this does not do: load `--envfile`. Caddy loads it before it
// adapts, and a {$NAME} it sets can change what the Caddyfile imports;
// here the Caddyfile is adapted in the environment the command was
// started in. The packaged service passes none, and hotserve-backup
// refuses an import that depends on a variable.
func checkBackups(cmd string, args []string) error {
	file, adapter, ok := commandFlags(cmd, args)
	if !ok {
		return nil
	}
	if file == "" && adapter == "" {
		file = "Caddyfile" // Caddy's own default, where there is one
	}
	if !isCaddyfile(file, adapter) {
		return nil
	}
	// Stdin or a pipe is Caddy's to read, once; and a pipe nobody writes
	// to would never answer.
	if st, err := os.Stat(file); err != nil || !st.Mode().IsRegular() { //nolint:gosec // the config the command was given, looked at before it is read
		return nil
	}
	body, err := os.ReadFile(file) //nolint:gosec // the config the command was given, which it reads next
	if err != nil {
		return nil
	}
	liveswap.ClearBackupSources()
	if _, _, err := caddyconfig.GetAdapter("caddyfile").Adapt(body, map[string]any{"filename": file}); err != nil {
		return nil
	}
	sources := liveswap.BackupSources()
	if len(sources) == 0 {
		return nil
	}
	main, err := filepath.Abs(file)
	if err != nil {
		return err
	}
	dir := filepath.Dir(main)
	// What a backup run's view holds is the directory itself, and so what
	// a file leads to is judged against where the directory leads.
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
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
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("a backup run would not see what this Caddyfile declares: %s — a backup run reads the Caddyfile inside a view that holds %s and nothing else, as an account that owns nothing there, so keep every file a backup is declared in under %s, readable by others (0644, directories 0755)", strings.Join(problems, "; "), dir, dir)
}

// under says whether p is below dir.
func under(p, dir string) bool {
	return strings.HasPrefix(p, strings.TrimSuffix(dir, string(filepath.Separator))+string(filepath.Separator))
}
