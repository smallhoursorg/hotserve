package plan

import (
	"bytes"
	"context"
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
	// Imports are the imports of the Caddyfile and of what it imports,
	// however deep. A run reads them from inside a view that holds the
	// config directory and nothing else.
	Imports []Import
}

// Import is one import line that names files.
type Import struct {
	// Pattern is what is imported, as an absolute, clean path — a glob,
	// where the line was one — with the adapter's defaults in.
	Pattern string
	// Relative: written relative to the file the line is in.
	Relative bool
	// Files are the files it matches here.
	Files []string
}

// ImportsOutside are the imports a run could not read once the
// Caddyfile is live: the patterns that are not under configDir — or,
// written relatively, under ownDir, the Caddyfile's own directory, which
// on the box is configDir — whose way down, a directory a wildcard
// matches included, leads through a link out of both, and the matched
// files that lead out of both.
//
// The pattern is judged as written, before anything it matches: inside
// a run's view a glob that reaches outside matches nothing, which to
// the adapter is no error, so the apps declared out there would simply
// not be in the plan.
func (i *Inspection) ImportsOutside(configDir, ownDir string) (out []string) {
	resolved := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return filepath.Clean(p)
	}
	roots := []string{resolved(configDir), resolved(ownDir)}
	for _, imp := range i.Imports {
		inside := within(imp.Pattern, configDir) || (imp.Relative && within(imp.Pattern, ownDir))
		// A directory a wildcard matches may be a link out as well, and
		// in a run's view dangle: every one on the way is followed.
		linked := false
		if inside {
			wayDown(literalDir(imp.Pattern), imp.Pattern, func(d, _ string) bool {
				if leadsOut(d, roots...) {
					linked = true
					return false
				}
				st, err := os.Stat(d) //nolint:gosec // a directory the Caddyfile's own import leads through, looked at and never opened
				return err == nil && st.IsDir()
			})
		}
		if !inside || linked {
			out = append(out, imp.Pattern)
			continue
		}
		for _, file := range imp.Files {
			if r := resolved(file); !within(r, roots[0]) && !within(r, roots[1]) {
				out = append(out, file)
			}
		}
	}
	return out
}

// literalDir is the directory a pattern names before its first
// wildcard.
func literalDir(pattern string) string {
	if at := strings.IndexAny(pattern, "*?["); at >= 0 {
		pattern = pattern[:at]
	}
	return filepath.Dir(pattern)
}

// leadsOut says whether dir, followed through its links, is under
// neither root — or cannot be followed because a link on the way
// dangles, which is what a link out of a run's view looks like from
// inside it, where the glob then matches nothing. A directory that is
// simply not there leads nowhere: what is judged is the deepest of it
// and the directories above it that is there, a link that dangles
// included.
func leadsOut(dir string, roots ...string) bool {
	d := filepath.Clean(dir)
	for {
		if _, err := os.Lstat(d); err == nil {
			break
		}
		if d == filepath.Dir(d) {
			return false
		}
		d = filepath.Dir(d)
	}
	r, err := filepath.EvalSymlinks(d)
	if err != nil {
		// A way it may not walk is the closed-directory check's to say.
		return errors.Is(err, fs.ErrNotExist)
	}
	for _, root := range roots {
		if within(r, root) {
			return false
		}
	}
	return true
}

// within says whether p is dir or under it.
func within(p, dir string) bool {
	p, dir = filepath.Clean(p), filepath.Clean(dir)
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, string(filepath.Separator))+string(filepath.Separator))
}

// ImportsClosedToOthers are the imported files under configDir that
// "other" may not read, and the directories on the way to them —
// configDir itself and every one the pattern leads through, wildcards
// included, matched a segment at a time — that "other" may not list
// and walk: the plan unit
// runs as an account that owns nothing there and is in no group that
// does. A closed file fails every run; a closed directory makes its glob
// match nothing, and the run plan without what is declared in it.
// Imports elsewhere — a copy being checked — say nothing of the box.
func (i *Inspection) ImportsClosedToOthers(configDir string) (closed []string) {
	configDir = filepath.Clean(configDir)
	seen := map[string]bool{}
	look := func(p string, need os.FileMode) {
		if seen[p] {
			return
		}
		seen[p] = true
		if st, err := os.Stat(p); err == nil && st.Mode().Perm()&need != need { //nolint:gosec // the Caddyfile's own imports, looked at for their mode and never opened
			closed = append(closed, p)
		}
	}
	inside := func(p string) bool { return within(p, configDir) }
	dirs := func(from string) {
		for d := from; inside(d); d = filepath.Dir(d) {
			look(d, 0o005)
		}
	}
	for _, imp := range i.Imports {
		// configDir, and every directory the pattern leads through, one
		// segment at a time, each looked at before anything under it
		// is: a directory whoever is asking may not enter either has
		// nothing under it that a glob of the whole way down would
		// match, and only the way itself says it is there.
		if inside(imp.Pattern) {
			wayDown(configDir, imp.Pattern, func(d, _ string) bool {
				if st, err := os.Stat(d); err == nil && st.IsDir() { //nolint:gosec // as above
					look(d, 0o005)
					return true
				}
				return false
			})
		}
		for _, file := range imp.Files {
			if inside(file) {
				look(file, 0o004)
				dirs(filepath.Dir(file))
			}
			// A link among them is read where it leads, by the way there.
			if target, err := filepath.EvalSymlinks(file); err == nil && target != file && inside(target) {
				look(target, 0o004)
				dirs(filepath.Dir(target))
			}
		}
	}
	sort.Strings(closed)
	return closed
}

// wayDown walks the directories pattern leads through from from, a
// segment at a time: visit is given each, from itself on, with the
// segment about to be matched in it, and the walk goes on below a
// directory only where visit says to. A wildcard segment is matched
// against the entries of the directory, read by whoever is asking —
// one it may not list matches nothing — and never joined back into a
// pattern: a directory may be called "a[1]".
func wayDown(from, pattern string, visit func(dir, seg string) bool) {
	sep := string(filepath.Separator)
	var segs []string
	for _, seg := range strings.Split(strings.TrimPrefix(filepath.Clean(pattern), filepath.Clean(from)), sep) {
		if seg != "" {
			segs = append(segs, seg)
		}
	}
	level := []string{from}
	for k, seg := range segs {
		var next []string
		for _, d := range level {
			if !visit(d, seg) || k == len(segs)-1 {
				continue
			}
			if !strings.ContainsAny(seg, "*?[") {
				next = append(next, filepath.Join(d, seg))
				continue
			}
			entries, _ := os.ReadDir(d)
			for _, e := range entries {
				if ok, _ := filepath.Match(seg, e.Name()); ok {
					next = append(next, filepath.Join(d, e.Name()))
				}
			}
		}
		level = next
	}
}

// ImportsClosedHere are the directories under configDir, on an import's
// way, that whoever is asking may not walk — or, where a wildcard is
// matched in one, list. It is the run's own check, made as the run's
// account: the adapter takes a glob that cannot look for nothing to
// match, and the run would plan without what is declared past it. A
// path it walks by name needs only the walk.
func (i *Inspection) ImportsClosedHere(configDir string) (closed []string) {
	seen := map[string]bool{}
	for _, imp := range i.Imports {
		if !within(imp.Pattern, configDir) {
			continue
		}
		wayDown(configDir, imp.Pattern, func(d, seg string) bool {
			var err error
			if strings.ContainsAny(seg, "*?[") {
				_, err = os.ReadDir(d)
			} else {
				_, err = os.Lstat(filepath.Join(d, seg)) //nolint:gosec // the way to an import of the Caddyfile's own, looked at and never opened
			}
			if errors.Is(err, fs.ErrPermission) {
				if !seen[d] {
					seen[d] = true
					closed = append(closed, d)
				}
				return false
			}
			st, err := os.Stat(d) //nolint:gosec // a directory the Caddyfile's own import leads through, looked at and never opened
			return err == nil && st.IsDir()
		})
	}
	sort.Strings(closed)
	return closed
}

// Inspect adapts the Caddyfile and returns its Plan, with the apps that
// declare no backup and the files it imports.
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
func Inspect(ctx context.Context, caddyfile string) (*Inspection, error) {
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
		p, undeclared, err := extractAll(stdout.Bytes())
		return p, undeclared, "", err
	}

	names, needs, imported, imports, byArgument, byHeredoc, err := scanCaddyfile(caddyfile)
	if err != nil {
		return nil, err
	}
	// A heredoc is one token to the adapter, and an import follows it as
	// a path like any other [measured]; to this reader it is quoted text.
	if len(byHeredoc) > 0 {
		return nil, fmt.Errorf("the Caddyfile imports by a heredoc (%s), which this reader does not follow, so which files the server reads cannot be known here; a backup has to know every file the Caddyfile is made of — write those import paths literally, on the import's line", strings.Join(byHeredoc, "; "))
	}
	// The adapter fills a snippet's argument in from wherever the snippet
	// is used, and this reader does not: one of those uses can name
	// files outside the directory, which a run's view does not hold —
	// its glob would match nothing there, and the run plan without them.
	if len(byArgument) > 0 {
		return nil, fmt.Errorf("the Caddyfile imports by a snippet's argument (%s), so which files the server reads cannot be known here; a backup has to know every file the Caddyfile is made of — write those import paths literally", strings.Join(byArgument, "; "))
	}
	// No value tried here says what the server imports with the value it
	// has: `import sites/{$ENV:prod}/*.caddy` adapts with anything — a
	// glob that matches nothing is not an error — and the files the
	// server reads may say anything about the root.
	if len(imported) > 0 {
		return nil, fmt.Errorf("the Caddyfile imports by the environment variable(s) %s, so which files the server reads cannot be known here; a backup reads the Caddyfile without hotserve's environment — write those import paths literally", strings.Join(imported, ", "))
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
	return &Inspection{Plan: base, Undeclared: undeclared, UndeclaredByEnv: byEnv, Imports: imports}, nil
}

// Make is the plan alone — what a run needs of an Inspection — and
// refuses a Caddyfile that imports from outside its own directory: the
// plan unit's view holds that directory and nothing else.
func Make(ctx context.Context, caddyfile string) (*Plan, error) {
	i, err := Inspect(ctx, caddyfile)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(caddyfile)
	if out := i.ImportsOutside(dir, dir); len(out) > 0 {
		return nil, OutsideError(out, dir)
	}
	if closed := i.ImportsClosedHere(dir); len(closed) > 0 {
		return nil, fmt.Errorf("the Caddyfile imports through %s, which this account may not enter or list: a wildcard there matches nothing, and what is declared past it would not be backed up — make it listable by others (0755)", strings.Join(closed, ", "))
	}
	return i.Plan, nil
}

// OutsideError is the refusal of imports from outside dir.
func OutsideError(imports []string, dir string) error {
	return fmt.Errorf("the Caddyfile imports %s from outside %s: a backup run reads the Caddyfile inside a view that holds that directory and nothing else, so what is declared out there would not be backed up — keep what is imported under %s", strings.Join(imports, ", "), dir, dir)
}

// insideQuotes says of each byte of a Caddyfile whether it is inside a
// quoted token — "…", `…`, or a heredoc, each of which may run over
// lines — or a comment: text, where a line that begins with a
// directive's name is not that directive. It follows the adapter's
// lexer in what opens one: only the start of a token.
func insideQuotes(raw []byte) []bool {
	in := make([]bool, len(raw))
	tokenStart := true
	for i := 0; i < len(raw); {
		c := raw[i]
		switch {
		case !tokenStart:
			tokenStart = c == ' ' || c == '\t' || c == '\n' || c == '\r'
			i++
		case c == '#':
			for ; i < len(raw) && raw[i] != '\n'; i++ {
				in[i] = true
			}
		case c == '"' || c == '`':
			in[i] = true
			for i++; i < len(raw) && raw[i] != c; i++ {
				in[i] = true
				if c == '"' && raw[i] == '\\' && i+1 < len(raw) {
					i++
					in[i] = true
				}
			}
			if i < len(raw) {
				in[i] = true
				i++
			}
			tokenStart = false
		case c == '<' && bytes.HasPrefix(raw[i:], []byte("<<")):
			// A heredoc opens where "<<" and its marker run to the end of
			// the line, a CR before it skipped; "<<" and a space is an
			// ordinary token. It closes the moment its text ends with the
			// marker, wherever on a line that is — `HTML 200` — and the
			// rest of that line is tokens [Caddy's lexer; measured].
			j := i + 2
			for j < len(raw) && raw[j] != ' ' && raw[j] != '\t' && raw[j] != '\n' {
				j++
			}
			tokenStart = false
			if j == len(raw) || raw[j] != '\n' {
				i = j
				continue
			}
			marker := bytes.TrimSuffix(raw[i+2:j], []byte("\r"))
			i = j + 1
			end := len(raw)
			if k := bytes.Index(raw[i:], marker); len(marker) > 0 && k >= 0 {
				end = i + k + len(marker)
			}
			for ; i < end; i++ {
				in[i] = true
			}
		default:
			tokenStart = c == ' ' || c == '\t' || c == '\n' || c == '\r'
			i++
		}
	}
	return in
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

var (
	// A name runs to the first ":" (where a default starts) or "}".
	envRe = regexp.MustCompile(`\{\$([^}:]+)(:[^}]*)?\}`)
	// The argument of an import: quoted (a backslash escapes), backquoted
	// or bare — on the import's own line, or on the next after a
	// backslash that continues it. Never simply on the next: \s would
	// take the first word of the line after a bare "import".
	importRe = regexp.MustCompile("(?m)^[ \\t]*import(?:[ \\t]|\\\\\\r?\\n)+(?:\"((?:[^\"\\\\]|\\\\.)+)\"|`([^`]+)`|([^\\s\\\\]\\S*))")
	// A snippet's argument: {args[0]}, {args[:]}, and the older {args.0}.
	snippetArgRe = regexp.MustCompile(`\{args[\[.]`)
	// {$NAME} and {$NAME:default}, as the adapter substitutes them with
	// nothing set.
	substRe = regexp.MustCompile(`\{\$[^}:]+(?::([^}]*))?\}`)
)

// scanCaddyfile returns every {$NAME} in the Caddyfile and the files
// it imports, however deep, and those among them written somewhere with
// no default. An import that names no file — a snippet, a glob matching
// nothing — contributes nothing. inImport are the names used in an
// import's argument, and imports every import line read; byArgument the
// import lines whose path is a snippet's argument, as written, and
// byHeredoc those whose path is a heredoc, up to its marker.
func scanCaddyfile(caddyfile string) (all, noDefault, inImport []string, imports []Import, byArgument, byHeredoc []string, err error) {
	seen := map[string]bool{}
	names := map[string]bool{} // true: written somewhere with no default
	importVars := map[string]bool{}
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
		text := insideQuotes(raw)
		for _, at := range importRe.FindAllSubmatchIndex(raw, -1) {
			// The line's first word, where that word is not inside a quoted
			// token or a comment that began on a line before.
			if text[at[0]+bytes.Index(raw[at[0]:at[1]], []byte("import"))] {
				continue
			}
			var m [4][]byte
			for g := 1; g <= 3; g++ {
				if at[2*g] >= 0 {
					m[g] = raw[at[2*g]:at[2*g+1]]
				}
			}
			arg := strings.ReplaceAll(string(m[1]), `\"`, `"`) + string(m[2]) + string(m[3])
			if len(m[3]) > 0 && bytes.HasPrefix(m[3], []byte("<<")) {
				byHeredoc = append(byHeredoc, "import "+arg)
				continue
			}
			if snippetArgRe.MatchString(arg) {
				byArgument = append(byArgument, "import "+arg)
				continue
			}
			for _, v := range envRe.FindAllStringSubmatch(arg, -1) {
				importVars[v[1]] = true
			}
			// As the base adapt sees it: defaults in, unset names empty.
			pattern := substRe.ReplaceAllString(arg, "$1")
			imp := Import{Relative: !filepath.IsAbs(pattern)}
			if imp.Relative {
				pattern = filepath.Join(filepath.Dir(file), pattern)
			}
			imp.Pattern = filepath.Clean(pattern)
			matches, _ := filepath.Glob(pattern)
			for _, match := range matches {
				if st, err := os.Stat(match); err != nil || !st.Mode().IsRegular() { //nolint:gosec // as above
					continue
				}
				imp.Files = append(imp.Files, match)
				if err := scan(match); err != nil {
					return err
				}
			}
			imports = append(imports, imp)
		}
		return nil
	}
	if err := scan(caddyfile); err != nil {
		return nil, nil, nil, nil, nil, nil, fmt.Errorf("reading the Caddyfile for {$NAME}: %w", err)
	}

	for n := range importVars {
		inImport = append(inImport, n)
	}
	sort.Strings(inImport)
	for n, bare := range names {
		all = append(all, n)
		if bare {
			noDefault = append(noDefault, n)
		}
	}
	sort.Strings(all)
	sort.Strings(noDefault)
	return all, noDefault, inImport, imports, byArgument, byHeredoc, nil
}
