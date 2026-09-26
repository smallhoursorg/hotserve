// Package envfile writes and reads the credential file the way systemd's
// EnvironmentFile= does. It has one writer, setup, and one reader,
// status as root, to say where a hand-edited line is not read as it
// was written.
//
// The manager's rules are measured, not assumed
// (TestIntegrationSystemdReadsAnEnvFileAsParseDoes), and Parse walks
// the same states env-file.c does: a line ends at a newline or a
// carriage return; whitespace is a space, a tab or either of those and
// nothing else; whitespace around a key is trimmed; the last assignment
// of a key wins; a line whose first character is # or ; is a comment; a
// line with no =, or with a name the manager does not take, is skipped;
// a byte that is not UTF-8 makes the manager refuse the whole file. In
// a value: whitespace at either end is trimmed unless escaped or
// quoted; outside quotes a backslash escapes the byte after it, a line
// end included, which joins the lines; inside double quotes only \, ",
// $ and ` are unescaped and any other byte keeps its backslash; inside
// single quotes nothing; a quote runs on to the line that closes it,
// and one that is never closed takes the rest of the file; after a
// closing quote the value goes on by the unquoted rules, whitespace
// skipped, a quote opening another quoted run; a backslash that ends
// the file is gone.
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

// Parse reads raw as the manager does — the same states, byte by byte,
// as systemd's env-file.c — and says of each line that the manager
// reads other than as it was written, or not at all.
func Parse(raw []byte) (Values, []string) {
	v := Values{}
	var findings []string
	if !utf8.Valid(raw) {
		n := 1 + bytes.Count(raw[:utf8FirstInvalid(raw)], []byte("\n"))
		return v, []string{fmt.Sprintf("line %d holds a byte that is not UTF-8: the manager refuses the whole file, and no unit that needs it starts", n)}
	}
	const (
		preKey = iota
		inKey
		preValue
		inValue
		valueEscape
		singleQuoted
		doubleQuoted
		doubleQuotedEscape
		comment
	)
	// The manager's whitespace and line ends are these bytes and no
	// other: a non-breaking space is part of a name or a value.
	const whitespace, newline = " \t\n\r", "\n\r"
	state := preKey
	var key, value strings.Builder
	keyWS, valueWS := -1, -1 // where trailing whitespace begins, if any
	padded := false          // whitespace skipped before the key
	line, keyLine := 1, 1
	push := func(strip bool) {
		name := key.String()
		if keyWS >= 0 {
			name = name[:keyWS]
		}
		val := value.String()
		if strip && valueWS >= 0 {
			val = val[:valueWS]
		}
		switch {
		case !keyRe.MatchString(name):
			findings = append(findings, fmt.Sprintf("line %d: %q is not a name the manager takes; it skips the line", keyLine, name))
		default:
			if padded || keyWS >= 0 {
				findings = append(findings, fmt.Sprintf("line %d: the key is written with whitespace around it; the manager reads it as %s", keyLine, name))
			}
			if _, again := v[name]; again {
				findings = append(findings, fmt.Sprintf("line %d: %s is set again; the manager takes this one", keyLine, name))
			}
			v[name] = val
		}
		key.Reset()
		value.Reset()
		keyWS, valueWS, padded = -1, -1, false
	}
	noValue := func() {
		findings = append(findings, fmt.Sprintf("line %d: not KEY=value; the manager skips it", keyLine))
		key.Reset()
		keyWS, padded = -1, false
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c == '\n' {
			line++
		}
		switch state {
		case preKey:
			switch {
			case c == '#' || c == ';':
				state = comment
			case strings.IndexByte(newline, c) >= 0:
				padded = false
			case strings.IndexByte(whitespace, c) >= 0:
				padded = true
			default:
				state = inKey
				keyLine = line
				key.WriteByte(c)
			}
		case inKey:
			switch {
			case strings.IndexByte(newline, c) >= 0:
				noValue()
				state = preKey
			case c == '=':
				state = preValue
			default:
				if strings.IndexByte(whitespace, c) < 0 {
					keyWS = -1
				} else if keyWS < 0 {
					keyWS = key.Len()
				}
				key.WriteByte(c)
			}
		case preValue:
			// Where a value begins, and where it goes on after a closing
			// quote: whitespace skipped, a quote opening a quoted run.
			switch {
			case strings.IndexByte(newline, c) >= 0:
				push(false)
				state = preKey
			case c == '\'':
				state = singleQuoted
			case c == '"':
				state = doubleQuoted
			case c == '\\':
				state = valueEscape
			case strings.IndexByte(whitespace, c) < 0:
				state = inValue
				value.WriteByte(c)
			}
		case inValue:
			switch {
			case strings.IndexByte(newline, c) >= 0:
				push(true)
				state = preKey
			case c == '\\':
				state = valueEscape
				valueWS = -1
			default:
				if strings.IndexByte(whitespace, c) < 0 {
					valueWS = -1
				} else if valueWS < 0 {
					valueWS = value.Len()
				}
				value.WriteByte(c)
			}
		case valueEscape:
			// The byte after a backslash, as it is; a line end there
			// joins the lines and is itself gone.
			state = inValue
			if strings.IndexByte(newline, c) < 0 {
				value.WriteByte(c)
				valueWS = -1
			}
		case singleQuoted:
			if c == '\'' {
				state = preValue
			} else {
				value.WriteByte(c)
			}
		case doubleQuoted:
			switch c {
			case '"':
				state = preValue
			case '\\':
				state = doubleQuotedEscape
			default:
				value.WriteByte(c)
			}
		case doubleQuotedEscape:
			// Only \, ", $ and ` are unescaped; any other byte keeps its
			// backslash, as a shell would; a newline is joined.
			state = doubleQuoted
			switch {
			case strings.IndexByte("\"\\`$", c) >= 0:
				value.WriteByte(c)
			case c != '\n':
				value.WriteByte('\\')
				value.WriteByte(c)
			}
		case comment:
			if strings.IndexByte(newline, c) >= 0 {
				state = preKey
				padded = false // the comment's own indent is not the next key's
			}
		}
	}
	// The end of the file ends a value that was under way, a quote
	// still open included; a key with no = is nothing; a backslash
	// waiting for its byte is gone.
	switch state {
	case inKey:
		noValue()
	case singleQuoted, doubleQuoted, doubleQuotedEscape:
		findings = append(findings, fmt.Sprintf("line %d: the quote is never closed; the manager reads everything after it, to the end of the file, as %s's value", keyLine, key.String()))
		push(false)
	case preValue, inValue, valueEscape:
		push(state == inValue)
	}
	return v, findings
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

// Staged is where setup writes the file beside the working one at
// path, for its units to read until the repository has answered: one
// name, since setup holds the run lock and no two write at once.
func Staged(path string) string { return path + ".staged" }

// IsLeftover says whether name, beside base, is a file a setup that
// did not live to the end left behind: the staged file, or what a
// Write on the way to it or to base made — the two shapes a sweep may
// remove, and the only two: what an operator keeps beside the file
// under another name is theirs.
func IsLeftover(name, base string) bool {
	staged := filepath.Base(Staged(base))
	if name == staged {
		return true
	}
	// What Write makes on the way to base, or to the staged file.
	for _, to := range []string{base, staged} {
		if digits, ok := strings.CutPrefix(name, "."+to+"-"); ok && digits != "" && strings.Trim(digits, "0123456789") == "" {
			return true
		}
	}
	return false
}

// Commit renames from over to, and puts the directory on the disk.
func Commit(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(to))
}

// SyncDir puts a directory's entries on the disk: what a rename in it
// needs before anything counts on it after a power cut.
func SyncDir(path string) error {
	dir, err := os.Open(path) //nolint:gosec // a directory of the installation's, from a constant path
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
