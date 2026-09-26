// Package envfile writes and reads the credential file the way systemd's
// EnvironmentFile= does. It has one writer, setup, and two readers: setup
// again, to compare the repository with the one before, and status as
// root, to say where a hand-edited line is not read as it was written.
//
// The manager's rules are measured, not assumed
// (TestIntegrationSystemdReadsAnEnvFileAsParseDoes): whitespace around a
// key is trimmed; a value's surrounding quotes are stripped and its
// whitespace trimmed; the last assignment of a key wins; a line whose
// first character is # or ; is a comment; a line with no = is skipped; a
// trailing backslash joins the next line; outside quotes a backslash
// escapes the character after it, inside double quotes only \ and "; a
// quote runs on to the line that closes it, and one that is never
// closed takes the rest of the file. Inside double quotes whitespace at
// either end, a tab and an escaped quote are kept, so the writer quotes
// a value the plain form would not carry.
package envfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Values is what the manager would give a unit: each key's last value.
type Values map[string]string

// Pair is one KEY=value the writer writes.
type Pair struct{ Key, Value string }

var keyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Parse reads raw as the manager does, and says of each line that the
// manager reads other than as it was written, or not at all.
func Parse(raw []byte) (Values, []string) {
	v := Values{}
	var findings []string
	lines := strings.Split(string(raw), "\n")
	for i := 0; i < len(lines); i++ {
		n := i + 1
		line := lines[i]
		// A trailing backslash continues the line on the next.
		for strings.HasSuffix(line, `\`) && !strings.HasSuffix(line, `\\`) && i+1 < len(lines) {
			i++
			line = strings.TrimSuffix(line, `\`) + lines[i]
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || trimmed[0] == '#' || trimmed[0] == ';' {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			findings = append(findings, fmt.Sprintf("line %d: not KEY=value; the manager skips it", n))
			continue
		}
		k := strings.TrimSpace(key)
		if !keyRe.MatchString(k) {
			findings = append(findings, fmt.Sprintf("line %d: %q is not a name the manager takes; it skips the line", n, k))
			continue
		}
		if k != key {
			findings = append(findings, fmt.Sprintf("line %d: the key is written with whitespace around it; the manager reads it as %s", n, k))
		}
		if _, again := v[k]; again {
			findings = append(findings, fmt.Sprintf("line %d: %s is set again; the manager takes this one", n, k))
		}
		value = strings.TrimSpace(value)
		// A quoted value runs on until the quote is closed — on a later
		// line, or never, in which case it takes the rest of the file.
		if len(value) > 0 && (value[0] == '"' || value[0] == '\'') && !closed(value) {
			for i+1 < len(lines) {
				i++
				value += "\n" + lines[i]
				if closed(value) {
					break
				}
			}
			if !closed(value) {
				findings = append(findings, fmt.Sprintf("line %d: the quote is never closed; the manager reads everything after it, to the end of the file, as %s's value", n, k))
			}
		}
		v[k] = unquote(value)
	}
	return v, findings
}

// closed says whether a value that opens with a quote closes it.
func closed(value string) bool {
	q := value[0]
	for i := 1; i < len(value); i++ {
		if value[i] == '\\' && q == '"' {
			i++
			continue
		}
		if value[i] == q {
			return true
		}
	}
	return false
}

// unquote strips a value's surrounding quotes and undoes its escapes.
func unquote(s string) string {
	if s == "" {
		return s
	}
	q := s[0]
	if q == '"' || q == '\'' {
		s = s[1:]
		if q == '\'' {
			before, _, _ := strings.Cut(s, "'")
			return before
		}
		var out strings.Builder
		for i := 0; i < len(s); i++ {
			if s[i] == '\\' && i+1 < len(s) && (s[i+1] == '\\' || s[i+1] == '"') {
				i++
			} else if s[i] == '"' {
				break
			}
			out.WriteByte(s[i])
		}
		return out.String()
	}
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		out.WriteByte(s[i])
	}
	return out.String()
}

// Refuse says why value cannot be written so that the manager reads it
// back unchanged, or nil.
func Refuse(value string) error {
	switch {
	case value == "":
		return errors.New("it is empty")
	case !utf8.ValidString(value):
		return errors.New("it is not UTF-8")
	case strings.ContainsAny(value, "\n"):
		return errors.New("it holds a line break")
	case strings.IndexFunc(value, unicode.IsControl) >= 0:
		return errors.New("it holds a control character")
	}
	return nil
}

// Format is the file: one KEY=value a line, a backslash doubled so that
// the manager reads it as one; a value the plain form would not carry
// — whitespace at either end, which the manager trims, a leading quote,
// which it strips — goes in double quotes, with \ and " escaped
// [measured].
func Format(pairs []Pair) ([]byte, error) {
	var out strings.Builder
	for _, p := range pairs {
		if !keyRe.MatchString(p.Key) {
			return nil, fmt.Errorf("%q is not a name the manager takes", p.Key)
		}
		if err := Refuse(p.Value); err != nil {
			return nil, fmt.Errorf("%s: %w", p.Key, err)
		}
		v := strings.ReplaceAll(p.Value, `\`, `\\`)
		if strings.TrimSpace(p.Value) != p.Value || p.Value[0] == '"' || p.Value[0] == '\'' {
			v = `"` + strings.ReplaceAll(v, `"`, `\"`) + `"`
		}
		fmt.Fprintf(&out, "%s=%s\n", p.Key, v)
	}
	return []byte(out.String()), nil
}

// Write replaces path whole: a temporary file beside it, root-only,
// on the disk before it takes the old one's place, and the directory
// after it. Refused, it leaves the old file as it was.
func Write(path string, pairs []Pair) error {
	raw, err := Format(pairs)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone already when the rename succeeded
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close() //nolint:errcheck,gosec // the first error is the one reported
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close() //nolint:errcheck,gosec // as above
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close() //nolint:errcheck,gosec // as above
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return Commit(tmp.Name(), path)
}

// Commit renames from over to, and puts the directory on the disk.
func Commit(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(to))
	if err != nil {
		return err
	}
	defer dir.Close() //nolint:errcheck // read-only
	return dir.Sync()
}

// Lint is what status says of the file: what the manager reads other
// than as written, and what a run needs that is not set.
func Lint(v Values, findings []string) []string {
	out := append([]string(nil), findings...)
	for _, k := range []string{"RESTIC_REPOSITORY", "RESTIC_PASSWORD"} {
		if _, ok := v[k]; !ok {
			out = append(out, k+" is not set: no unit that needs the repository can run")
		}
	}
	return out
}
