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

// trials are the values a variable is tried with. The first is one no
// operator sets anything to; the rest are for a variable that is a
// number, a duration, a path or a host, where the first does not adapt.
var trials = []string{"hotserve-backup-trial-value", "1", "1s", "/hotserve-backup-trial", "trial.invalid"}

// Make adapts the Caddyfile and returns its Plan.
//
// It adapts with an empty environment, which is not the environment
// hotserve adapts it in. The Caddyfile's own {$NAME} is substituted in
// the text by whoever adapts, before any module sees a token, so a
// root or a backup path written with one means one thing to the server
// and another here — and the difference would show up as an app with
// "no data yet", forever. So every name the Caddyfile and its imports
// mention is tried: adapted again with that one variable set, and if
// the plan comes out different, Make refuses, naming the variable. A
// variable is cleared by a trial value that adapts and leaves the plan
// as it was. One that no trial value adapts with is refused as well:
// that is what `import sites/{$ENV:prod}.caddy` looks like from here,
// and the file the server imports may say anything about the root.
func Make(ctx context.Context, caddyfile string) (*Plan, error) {
	// adapt returns the plan as the Caddyfile spells it under env, not
	// yet validated: a trial value is not a valid root, and a plan that
	// turns invalid under a trial has changed like any other.
	adapt := func(env []string) (*Plan, error) {
		//nolint:gosec // a constant path outside tests; the Caddyfile path is the caller's, a root-owned constant
		cmd := exec.CommandContext(ctx, hotserve, "adapt", "--adapter", "caddyfile", "--config", caddyfile)
		cmd.Env = append([]string{}, env...) // never nil: nil means "inherit"
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("%s adapt: %w: %s", hotserve, err, lastLine(stderr.String()))
		}
		return extract(stdout.Bytes())
	}
	base, err := adapt(nil)
	if err != nil {
		return nil, err
	}
	if err := base.Validate(); err != nil {
		return nil, err
	}
	names, err := envNames(caddyfile)
	if err != nil {
		return nil, err
	}
	var decisive, opaque []string
	for _, name := range names {
		if strings.ContainsAny(name, "=\x00") {
			continue // cannot be set, so the server does not have it set either
		}
		cleared := false
		for _, value := range trials {
			trial, err := adapt([]string{name + "=" + value})
			if err != nil {
				continue
			}
			cleared = reflect.DeepEqual(base, trial)
			if !cleared {
				decisive = append(decisive, name)
			}
			break
		}
		if !cleared && (len(decisive) == 0 || decisive[len(decisive)-1] != name) {
			opaque = append(opaque, name)
		}
	}
	if len(opaque) > 0 {
		return nil, fmt.Errorf("the Caddyfile does not adapt with the environment variable(s) %s set to any value tried, so what the server reads when they are set — an import, perhaps — cannot be known here; a backup reads the Caddyfile without hotserve's environment", strings.Join(opaque, ", "))
	}
	if len(decisive) > 0 {
		return nil, fmt.Errorf("the liveswap root or a backup path depends on the environment variable(s) %s through the Caddyfile's {$NAME}; a backup reads the Caddyfile without hotserve's environment, so it would look somewhere else than the server does — write those values literally", strings.Join(decisive, ", "))
	}
	return base, nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

var (
	// A name runs to the first ":" (where a default starts) or "}".
	envRe = regexp.MustCompile(`\{\$([^}:]+)`)
	// The argument of an import: quoted, backquoted or bare.
	importRe = regexp.MustCompile("(?m)^\\s*import\\s+(?:\"([^\"]+)\"|`([^`]+)`|(\\S+))")
	// {$NAME} and {$NAME:default}, as the adapter substitutes them with
	// nothing set.
	substRe = regexp.MustCompile(`\{\$[^}:]+(?::([^}]*))?\}`)
)

// envNames returns every {$NAME} in the Caddyfile and the files it
// imports, however deep. An import that names no file — a snippet, a
// glob matching nothing — contributes nothing.
func envNames(caddyfile string) ([]string, error) {
	seen := map[string]bool{}
	names := map[string]bool{}
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
			names[string(m[1])] = true
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
		return nil, fmt.Errorf("reading the Caddyfile for {$NAME}: %w", err)
	}
	out := make([]string, 0, len(names))
	for n := range names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}
