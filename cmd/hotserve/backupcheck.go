package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/caddyserver/caddy/v2/caddyconfig"

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
	err := checkBackups(args[1:])
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

// checkBackups adapts the Caddyfile the command's flags name, as Caddy
// would, and holds every file a backup declaration came from to the
// Caddyfile's directory. An adapt that fails says nothing here: Caddy's
// own command says it next, in its own words. So does a config that is
// not a Caddyfile, or not a file.
func checkBackups(flags []string) error {
	file, adapter := "", ""
	for i, flag := range flags {
		name, value, inline := strings.Cut(flag, "=")
		if name != "--config" && name != "-c" && name != "--adapter" && name != "-a" {
			continue
		}
		if !inline {
			if i+1 == len(flags) {
				return nil
			}
			value = flags[i+1]
		}
		if name == "--config" || name == "-c" {
			file = value
		} else {
			adapter = value
		}
	}
	if file == "" {
		file = "Caddyfile" // Caddy's own default, where there is one
		if _, err := os.Stat(file); err != nil {
			return nil
		}
	}
	if adapter != "" && adapter != "caddyfile" || adapter == "" && strings.HasSuffix(file, ".json") {
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
	var problems []string
	seen := map[string]bool{}
	say := func(format string, a ...any) {
		if p := fmt.Sprintf(format, a...); !seen[p] {
			seen[p] = true
			problems = append(problems, p)
		}
	}
	check := func(what, file string) {
		// Only the main Caddyfile is ever relative, as it was given here.
		if abs, err := filepath.Abs(file); err == nil {
			file = abs
		}
		if !strings.HasPrefix(file, dir+string(filepath.Separator)) {
			say("%s in %s, outside %s", what, file, dir)
			return
		}
		st, err := os.Stat(file)
		switch {
		case err != nil:
			say("%s in %s, which cannot be looked at: %v", what, file, err)
			return
		case st.Mode().Perm()&0o004 == 0:
			say("%s in %s, which others may not read", what, file)
		}
		for d := filepath.Dir(file); d != filepath.Dir(dir); d = filepath.Dir(d) {
			if st, err := os.Stat(d); err == nil && st.Mode().Perm()&0o001 == 0 {
				say("%s under %s, which others may not enter", what, d)
			}
		}
	}
	check("the Caddyfile is", main)
	for _, s := range sources {
		if s.App == "" {
			check("the liveswap root is set", s.File)
		} else {
			check(s.App+"'s backup is declared", s.File)
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("a backup run would not see what this Caddyfile declares: %s — a backup run reads the Caddyfile inside a view that holds %s and nothing else, as an account that owns nothing there, so keep every file a backup is declared in under %s, readable by others (0644, directories 0755)", strings.Join(problems, "; "), dir, dir)
}
