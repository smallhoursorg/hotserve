package plan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// hotserve is the binary that adapts the Caddyfile. Absolute: a
// relative name is looked up on whatever PATH the caller has. A
// variable only so a test can stand a script in for it.
var hotserve = "/usr/bin/hotserve"

// kinds are the values a variable can be given here, in the order they
// are tried: a name of its own, then the other things a Caddyfile asks
// for — a number, a duration, a path. Two of each, because seeing
// whether the plan depends on a variable takes two values it adapts
// with. The first is made from the variable's name, so that when the
// adapter quotes a value it cannot use, the variable is known.
func kinds(name string) []string {
	own := "hsb-placeholder-" + strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, name)
	return []string{own + ".invalid", own + "-2.invalid", "1", "2", "1s", "2s", "/" + own, "/" + own + "-2"}
}

// Inspection is a Caddyfile's plan, and what a person about to make
// that Caddyfile live should hear besides.
type Inspection struct {
	Plan *Plan
	// Undeclared are the apps that declare no backup, sorted: a new
	// app's block with the two lines forgotten is otherwise silent.
	Undeclared []string
	// UndeclaredByEnv are the environment variables that name an app
	// which declares no backup: what it is called on the server is not
	// known here, so it is not in Undeclared under a made-up name.
	UndeclaredByEnv []string
}

// Inspect adapts the Caddyfile and returns its Plan, with the apps that
// declare no backup.
//
// An import glob that matches nothing is no error to the adapter, only a
// warning [measured]; here it is refused. A run adapts the Caddyfile
// inside a view that holds its directory and nothing else, where a glob
// reaching outside matches nothing — and so does one through a
// directory the run's account may not list, or a link out — and the
// apps declared there would simply not be in the plan. The adapter's
// own warning is exact where a reading of the Caddyfile here would not
// be: it comes after its snippets, heredocs and {$NAME}, from any depth
// of import. What it does not see is a glob that matches something and
// misses something else: a wildcard directory with a link out beside
// real ones.
//
// It does not have the environment hotserve adapts it in, and is not
// given it: that environment is where the server's secrets are. But the
// Caddyfile's own {$NAME} is substituted in the text by whoever adapts,
// before any module sees a token, and that matters twice.
//
// A variable with no default, unset, leaves nothing where its value
// would be, and `email {$ACME_EMAIL}` is then a parse error: the most
// ordinary production Caddyfile does not adapt in an empty environment
// [measured; set to an empty string it fails the same way]. So every
// such variable is given a placeholder — a made-up value, of whichever
// kind the adapter accepts there. The structure of what is adapted is
// the server's own; only those values differ, and none of them is
// anywhere a plan looks.
//
// Unless one is: a root or a backup path written with a variable means
// one thing to the server and another here, and the difference would
// show up as an app with "no data yet", for ever. So every variable is
// then given a second value, and if the plan comes out different, Make
// refuses, naming it. One that no second value adapts with is refused
// as well: that is what `import sites/{$ENV:prod}.caddy` looks like
// from here, and the file the server imports may say anything.
//
// What this does not show: the substitution is textual, so a value
// with a newline in it is more lines of Caddyfile, and can declare a
// backup the file does not [measured]. Every value tried here is one
// token; no set of them shows that no value restructures the file, and
// the server's values are not known here. A declaration that exists
// only through the server's environment is not in the plan.
func Inspect(ctx context.Context, caddyfile, configDir string) (*Inspection, error) {
	// adapt returns the plan as the Caddyfile spells it under env, not
	// yet validated — a placeholder is not a valid root, and a plan that
	// turns invalid under one has changed like any other — or what the
	// adapter said.
	adapt := func(env map[string]string) (*Plan, []string, string, error) {
		//nolint:gosec // a constant path outside tests; the Caddyfile path is the caller's, a root-owned constant
		cmd := exec.CommandContext(ctx, hotserve, "adapt", "--adapter", "caddyfile", "--config", caddyfile)
		cmd.Env = []string{} // never nil: nil means "inherit"
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return nil, nil, stderr.String(), fmt.Errorf("%s adapt: %w: %s", hotserve, err, lastLine(stderr.String()))
		}
		if globs := emptyGlobs(stderr.Bytes()); len(globs) > 0 {
			return nil, nil, stderr.String(), &emptyGlobError{globs}
		}
		p, undeclared, err := extractAll(stdout.Bytes())
		return p, undeclared, "", err
	}

	// What is beside the live Caddyfile is what a run's view holds; beside
	// a copy checked anywhere else, it could be a whole tree.
	names, needs, err := envNames(caddyfile, filepath.Clean(filepath.Dir(caddyfile)) == filepath.Clean(configDir))
	if err != nil {
		return nil, err
	}

	// The base: nothing set but what has to be, each of those at the
	// first kind of value the adapter takes there. One variable moves on
	// by one kind a round — the one whose value the adapter quoted, if
	// it quoted one — so this ends.
	env, at := map[string]string{}, map[string]int{}
	for _, n := range needs {
		env[n] = kinds(n)[0]
	}
	var base *Plan
	var undeclared []string // as the base adapt has them
	for {
		var said string
		if base, undeclared, said, err = adapt(env); err == nil {
			break
		}
		// An empty glob no made-up value is in is no variable's doing.
		var empty *emptyGlobError
		if errors.As(err, &empty) && !slices.ContainsFunc(empty.patterns, func(p string) bool {
			return slices.ContainsFunc(needs, func(n string) bool { return strings.Contains(p, env[n]) })
		}) {
			return nil, err
		}
		moved := false
		for pass := 0; pass < 2 && !moved; pass++ {
			for _, n := range needs {
				quoted := strings.Contains(said, env[n])
				if (pass == 0) != quoted || at[n]+2 >= len(kinds(n)) {
					continue
				}
				at[n] += 2 // the next kind; the odd ones are each kind's second value
				env[n], moved = kinds(n)[at[n]], true
				break
			}
		}
		if !moved {
			hint := ""
			if len(needs) > 0 {
				hint = fmt.Sprintf(" (it was tried with made-up values for %s, which have no default: a backup reads the Caddyfile without hotserve's environment; a default the Caddyfile adapts with, `{$NAME:value}`, settles it)", strings.Join(needs, ", "))
			}
			return nil, fmt.Errorf("%w%s", err, hint)
		}
	}
	if err := base.Validate(); err != nil {
		if len(needs) == 0 {
			return nil, err
		}
		// Fall through: a root that is a placeholder is about to be named
		// as depending on its variable, which is the better thing to say.
	}

	var decisive, opaque, byEnv []string
	displaced := map[string]bool{} // undeclared names that some variable's value changes
	for _, name := range names {
		if strings.ContainsAny(name, "=\x00") {
			continue // cannot be set, so the server does not have it set either
		}
		verdict := &opaque
		for _, value := range kinds(name) {
			if value == env[name] {
				continue
			}
			trialEnv := map[string]string{name: value}
			for k, v := range env {
				if k != name {
					trialEnv[k] = v
				}
			}
			trial, trialUndeclared, _, err := adapt(trialEnv)
			if err != nil {
				continue
			}
			// An app that declares no backup and is named through this
			// variable changes nothing a run does, so it is no refusal;
			// but its name here is a default's, or a placeholder. Every
			// trial is held against the base's names as they were: the
			// names a variable displaces are taken out after the last one.
			if !slices.Equal(undeclared, trialUndeclared) {
				byEnv = append(byEnv, name)
				for _, n := range undeclared {
					if !slices.Contains(trialUndeclared, n) {
						displaced[n] = true
					}
				}
			}
			verdict = nil
			if !reflect.DeepEqual(base, trial) {
				verdict = &decisive
			}
			break
		}
		if verdict != nil {
			*verdict = append(*verdict, name)
		}
	}
	if len(decisive) > 0 {
		return nil, fmt.Errorf("the liveswap root or a backup path depends on the environment variable(s) %s through the Caddyfile's {$NAME}; a backup reads the Caddyfile without hotserve's environment, so it would look somewhere else than the server does — write those values literally", strings.Join(decisive, ", "))
	}
	if len(opaque) > 0 {
		return nil, fmt.Errorf("the Caddyfile does not adapt with the environment variable(s) %s set to any value tried, so what the server reads when they are set — an import, perhaps — cannot be known here; a backup reads the Caddyfile without hotserve's environment", strings.Join(opaque, ", "))
	}
	if err := base.Validate(); err != nil {
		return nil, err
	}
	undeclared = slices.DeleteFunc(undeclared, func(n string) bool { return displaced[n] })
	return &Inspection{Plan: base, Undeclared: undeclared, UndeclaredByEnv: byEnv}, nil
}

// Make is the plan alone: what a run needs of an Inspection.
func Make(ctx context.Context, caddyfile string) (*Plan, error) {
	i, err := Inspect(ctx, caddyfile, filepath.Dir(caddyfile))
	if err != nil {
		return nil, err
	}
	return i.Plan, nil
}

// emptyGlobError is the adapter's word that an import glob matches no
// file.
type emptyGlobError struct{ patterns []string }

func (e *emptyGlobError) Error() string {
	return fmt.Sprintf("the Caddyfile imports %s, which matches no file: a backup run reads the Caddyfile inside a view that holds its own directory and nothing else, where an import from outside it matches nothing and what is declared there would not be backed up — so every import glob has to match a file, and what is imported has to be under the Caddyfile's directory, readable by others", strings.Join(e.patterns, ", "))
}

// emptyGlobs are the import patterns the adapter says match no file, as
// the adapter logs them: after its own substitutions, and a relative
// one relative to the file it is in.
func emptyGlobs(stderr []byte) (patterns []string) {
	for _, line := range bytes.Split(stderr, []byte("\n")) {
		var w struct {
			Msg     string `json:"msg"`
			Pattern string `json:"pattern"`
		}
		if json.Unmarshal(line, &w) == nil && w.Msg == "No files matching import glob pattern" {
			patterns = append(patterns, w.Pattern)
		}
	}
	return patterns
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// A name runs to the first ":" (where a default starts) or "}", and has
// no white space in it: text that only looks like one — `{$` and a `}`
// lines apart, in a page's JavaScript — would cost a trial adapt each.
var envRe = regexp.MustCompile(`\{\$([^}:\s]+)(:[^}]*)?\}`)

// envNames returns every {$NAME} in the Caddyfile and, with beside, in
// the files beside it and below — the directory a run's view holds, so
// every file the adapter can read there, imported or not — and those
// among them written somewhere with no default. A copy checked anywhere
// else is read alone. Nothing here reads a Caddyfile's
// syntax: a name from a file nothing imports costs one trial adapt that
// changes nothing, and one that is missed has no default given, which
// the adapter says loudly. A file here that may not be read (another
// app's env file) is passed over, and so is one of over a megabyte: a
// file the Caddyfile imports that could not be read fails the adapt.
func envNames(caddyfile string, beside bool) (all, noDefault []string, err error) {
	names := map[string]bool{} // true: written somewhere with no default
	read := func(file string) error {
		raw, err := os.ReadFile(file) //nolint:gosec // the Caddyfile and the files beside it, read for {$NAME} alone
		if err != nil {
			return err
		}
		for _, m := range envRe.FindAllSubmatch(raw, -1) {
			names[string(m[1])] = names[string(m[1])] || len(m[2]) == 0
		}
		return nil
	}
	if err := read(caddyfile); err != nil {
		return nil, nil, fmt.Errorf("reading the Caddyfile for {$NAME}: %w", err)
	}
	if !beside {
		return sorted(names)
	}
	_ = filepath.WalkDir(filepath.Dir(caddyfile), func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil // one it may not list or read is passed over, as above
		}
		if info, err := d.Info(); err == nil && info.Size() <= 1<<20 {
			_ = read(p)
		}
		return nil
	})
	return sorted(names)
}

// sorted is envNames' names, and those with no default, each sorted.
func sorted(names map[string]bool) (all, noDefault []string, err error) {
	for n, bare := range names {
		all = append(all, n)
		if bare {
			noDefault = append(noDefault, n)
		}
	}
	sort.Strings(all)
	sort.Strings(noDefault)
	return all, noDefault, nil
}
