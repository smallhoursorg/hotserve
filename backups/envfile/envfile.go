// Package envfile writes and reads the credential file the way systemd's
// EnvironmentFile= does. It has one writer, setup, and two readers: setup
// again, to compare the repository with the one before, and status as
// root, to say where a hand-edited line is not read as it was written.
//
// The manager's rules are measured, not assumed
// (TestIntegrationSystemdReadsAnEnvFileAsParseDoes): whitespace around a
// key is trimmed; the last assignment of a key wins; a line whose
// first character is # or ; is a comment; a line with no =, or with a
// name the manager does not take, is skipped; a trailing backslash
// joins the next line; a byte that is not UTF-8 makes the manager
// refuse the whole file. In a value: whitespace at either end is
// trimmed unless escaped or quoted; outside quotes a backslash escapes
// the character after it; inside double quotes only \, ", $ and `;
// inside single quotes nothing; a quote runs on to the line that closes
// it, and one that is never closed takes the rest of the file; what
// follows a closing quote, whitespace skipped, is appended as it is.
package envfile

import (
	"bytes"
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
	if !utf8.Valid(raw) {
		n := 1 + bytes.Count(raw[:utf8FirstInvalid(raw)], []byte("\n"))
		return v, []string{fmt.Sprintf("line %d holds a byte that is not UTF-8: the manager refuses the whole file, and no unit that needs it starts", n)}
	}
	lines := strings.Split(string(raw), "\n")
	for i := 0; i < len(lines); i++ {
		n := i + 1
		line := lines[i]
		// Outside quotes a trailing backslash continues the line on the
		// next — one that is not itself escaped, so an odd run of them
		// [measured]. Inside quotes the line ends where the quote does
		// (below): in double quotes a backslash-newline is dropped, in
		// single quotes it is kept as it is [measured].
		if _, value, ok := strings.Cut(line, "="); !ok || !opensQuote(value) {
			for oddTrailingBackslashes(line) && i+1 < len(lines) {
				i++
				line = strings.TrimSuffix(line, `\`) + lines[i]
			}
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
		value = strings.TrimLeftFunc(value, unicode.IsSpace)
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
		v[k] = readValue(value)
	}
	return v, findings
}

// opensQuote says whether a value, its leading whitespace aside, begins
// with a quote.
func opensQuote(value string) bool {
	v := strings.TrimLeftFunc(value, unicode.IsSpace)
	return v != "" && (v[0] == '"' || v[0] == '\'')
}

// oddTrailingBackslashes says whether the line ends in an unescaped
// backslash: an odd run of them.
func oddTrailingBackslashes(line string) bool {
	n := 0
	for i := len(line) - 1; i >= 0 && line[i] == '\\'; i-- {
		n++
	}
	return n%2 == 1
}

// utf8FirstInvalid is the offset of the first byte that is not UTF-8.
func utf8FirstInvalid(raw []byte) int {
	for i := 0; i < len(raw); {
		r, size := utf8.DecodeRune(raw[i:])
		if r == utf8.RuneError && size == 1 {
			return i
		}
		i += size
	}
	return len(raw)
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

// readValue is a value as the manager reads it: a quote that opens it
// stripped and its escapes undone, what follows the closing quote
// appended, and whitespace at the end trimmed where it is neither
// quoted nor escaped. A quote anywhere but first is a character.
func readValue(s string) string {
	var out strings.Builder
	kept := 0 // how much of out is quoted or escaped, and so not trimmed
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case i == 0 && (c == '"' || c == '\''):
			// Up to the closing quote, or the end.
			j := i + 1
			for ; j < len(s) && s[j] != c; j++ {
				if c == '"' && s[j] == '\\' && j+1 < len(s) {
					if s[j+1] == '\n' {
						j++ // a line joined: the backslash and the newline go
						continue
					}
					if strings.IndexByte("\\\"$`", s[j+1]) >= 0 {
						j++
					}
				}
				out.WriteByte(s[j])
			}
			kept = out.Len()
			// After the closing quote, whitespace is skipped and the rest
			// is appended as it is.
			for i = j + 1; i < len(s) && (s[i] == ' ' || s[i] == '\t'); i++ {
			}
			if i < len(s) {
				out.WriteString(s[i:])
			}
			i = len(s)
		case c == '\\' && i+1 < len(s):
			i++
			out.WriteByte(s[i])
			kept = out.Len()
		default:
			out.WriteByte(c)
		}
	}
	res := out.String()
	return res[:kept] + strings.TrimRightFunc(res[kept:], unicode.IsSpace)
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
	tmp, err := os.CreateTemp(filepath.Dir(path), tempPattern(filepath.Base(path)))
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

// tempPattern is the name Write makes a file under on the way to base:
// a dotfile beside it, with what os.CreateTemp appends.
func tempPattern(base string) string { return "." + base + "-*" }

// IsLeftover says whether name, beside base, is a file a Write that
// did not live to its rename left behind, or one setup wrote beside
// the working file under base.<12 hex> for its units to read — the two
// shapes a sweep may remove, and the only two: what an operator keeps
// beside the file under another name is theirs.
func IsLeftover(name, base string) bool {
	if rest, ok := strings.CutPrefix(name, base+"."); ok {
		return nonce(rest)
	}
	// What Write makes on the way to base, or to base.<nonce>.
	rest, ok := strings.CutPrefix(name, "."+base)
	if !ok {
		return false
	}
	if n, ok := strings.CutPrefix(rest, "."); ok {
		if n, rest, ok = strings.Cut(n, "-"); !ok || !nonce(n) {
			return false
		}
		rest = "-" + rest
	}
	digits, ok := strings.CutPrefix(rest, "-")
	return ok && digits != "" && strings.Trim(digits, "0123456789") == ""
}

// nonce is twelve hex characters: what setup names its file with.
func nonce(s string) bool {
	if len(s) != 12 {
		return false
	}
	return strings.Trim(s, "0123456789abcdef") == ""
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
		if val, ok := v[k]; !ok {
			out = append(out, k+" is not set: no unit that needs the repository can run")
		} else if val == "" {
			out = append(out, k+" is set to nothing: no unit that needs the repository can run")
		}
	}
	return out
}
