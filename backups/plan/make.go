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

// sentinel is a value no operator sets a variable to.
const sentinel = "hotserve-backup-sentinel-value"

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
// variable whose trial value makes the adapt fail is being used for
// something an arbitrary string is wrong for — a port, a duration —
// and neither a root nor a backup path is that.
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
	var decisive []string
	for _, name := range names {
		if strings.ContainsAny(name, "=\x00") {
			continue // cannot be set, so the server does not have it set either
		}
		trial, err := adapt([]string{name + "=" + sentinel})
		if err != nil {
			continue
		}
		if !reflect.DeepEqual(base, trial) {
			decisive = append(decisive, name)
		}
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
	envRe    = regexp.MustCompile(`\{\$([^}:\s]+)`)
	importRe = regexp.MustCompile(`(?m)^\s*import\s+(\S+)`)
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
			pattern := string(m[1])
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
