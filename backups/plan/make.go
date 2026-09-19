package plan

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
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

// Make adapts the Caddyfile and returns its Plan.
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
func Make(ctx context.Context, caddyfile string) (*Plan, error) {
	// adapt returns the plan as the Caddyfile spells it under env, not
	// yet validated — a placeholder is not a valid root, and a plan that
	// turns invalid under one has changed like any other — or what the
	// adapter said.
	adapt := func(env map[string]string) (*Plan, string, error) {
		//nolint:gosec // a constant path outside tests; the Caddyfile path is the caller's, a root-owned constant
		cmd := exec.CommandContext(ctx, hotserve, "adapt", "--adapter", "caddyfile", "--config", caddyfile)
		cmd.Env = []string{} // never nil: nil means "inherit"
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return nil, stderr.String(), fmt.Errorf("%s adapt: %w: %s", hotserve, err, lastLine(stderr.String()))
		}
		p, err := extract(stdout.Bytes())
		return p, "", err
	}

	names, needs, err := envNames(caddyfile)
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
	for {
		var said string
		if base, said, err = adapt(env); err == nil {
			break
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

	var decisive, opaque []string
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
			trial, _, err := adapt(trialEnv)
			if err != nil {
				continue
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
	return base, base.Validate()
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

var (
	// A name runs to the first ":" (where a default starts) or "}".
	envRe = regexp.MustCompile(`\{\$([^}:]+)(:[^}]*)?\}`)
	// The argument of an import: quoted, backquoted or bare.
	importRe = regexp.MustCompile("(?m)^\\s*import\\s+(?:\"([^\"]+)\"|`([^`]+)`|(\\S+))")
	// {$NAME} and {$NAME:default}, as the adapter substitutes them with
	// nothing set.
	substRe = regexp.MustCompile(`\{\$[^}:]+(?::([^}]*))?\}`)
)

// envNames returns every {$NAME} in the Caddyfile and the files it
// imports, however deep, and those among them written somewhere with
// no default. An import that names no file — a snippet, a glob matching
// nothing — contributes nothing.
func envNames(caddyfile string) (all, noDefault []string, err error) {
	seen := map[string]bool{}
	names := map[string]bool{} // true: written somewhere with no default
	var scan func(file string) error
	scan = func(file string) error {
		if seen[file] {
			return nil
		}
		seen[file] = true
		raw, err := os.ReadFile(file) //nolint:gosec // the Caddyfile and what it imports: read here exactly as the adapter reads them, inside a unit whose view holds nothing else
		if err != nil {
			return err
		}
		for _, m := range envRe.FindAllSubmatch(raw, -1) {
			names[string(m[1])] = names[string(m[1])] || len(m[2]) == 0
		}
		for _, m := range importRe.FindAllSubmatch(raw, -1) {
			// As the base adapt sees it: defaults in, unset names empty.
			pattern := substRe.ReplaceAllString(string(m[1])+string(m[2])+string(m[3]), "$1")
			if !filepath.IsAbs(pattern) {
				pattern = filepath.Join(filepath.Dir(file), pattern)
			}
			matches, _ := filepath.Glob(pattern)
			for _, match := range matches {
				if st, err := os.Stat(match); err != nil || !st.Mode().IsRegular() { //nolint:gosec // as above
					continue
				}
				if err := scan(match); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := scan(caddyfile); err != nil {
		return nil, nil, fmt.Errorf("reading the Caddyfile for {$NAME}: %w", err)
	}
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
