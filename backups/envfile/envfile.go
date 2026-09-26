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
	text := string(raw)
	for pos := 0; pos < len(text); {
		n := 1 + strings.Count(text[:pos], "\n")
		eol := strings.IndexByte(text[pos:], '\n')
		if eol < 0 {
			eol = len(text) - pos
		}
		line := text[pos : pos+eol]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || trimmed[0] == '#' || trimmed[0] == ';' {
			pos += eol + 1
			continue
		}
		key, _, ok := strings.Cut(line, "=")
		if !ok {
			findings = append(findings, fmt.Sprintf("line %d: not KEY=value; the manager skips it", n))
			pos += eol + 1
			continue
		}
		k := strings.TrimSpace(key)
		if !keyRe.MatchString(k) {
			findings = append(findings, fmt.Sprintf("line %d: %q is not a name the manager takes; it skips the line", n, k))
			pos += eol + 1
			continue
		}
		if k != key {
			findings = append(findings, fmt.Sprintf("line %d: the key is written with whitespace around it; the manager reads it as %s", n, k))
		}
		if _, again := v[k]; again {
			findings = append(findings, fmt.Sprintf("line %d: %s is set again; the manager takes this one", n, k))
		}
		value, used, unclosed := readValue(text[pos+len(key)+1:])
		if unclosed {
			findings = append(findings, fmt.Sprintf("line %d: the quote is never closed; the manager reads everything after it, to the end of the file, as %s's value", n, k))
		}
		v[k] = value
		pos += len(key) + 1 + used
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

// readValue reads one value from the start of s as the manager does,
// and says how much of s it took (the line end included) and whether a
// quote was left open to the end. Outside quotes: leading whitespace
// is skipped; a backslash escapes the character after it, a newline
// included, which joins the next line; an unescaped newline ends the
// value; trailing whitespace that is neither escaped nor quoted is
// trimmed. A quote where the value begins runs to its closing quote —
// a newline inside kept — with \, ", $ and ` escapable in double
// quotes, nothing in single; what follows the closing quote, its
// leading whitespace skipped, is read by the outside rules.
func readValue(s string) (value string, used int, unclosed bool) {
	var out strings.Builder
	kept := 0 // how much of out is quoted or escaped, and so not trimmed
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	if i < len(s) && (s[i] == '"' || s[i] == '\'') {
		q := s[i]
		i++
		closed := false
		for i < len(s) {
			c := s[i]
			if c == q {
				closed = true
				i++
				break
			}
			if q == '"' && c == '\\' && i+1 < len(s) {
				if s[i+1] == '\n' {
					i += 2 // a line joined: the backslash and the newline go
					continue
				}
				if strings.IndexByte("\\\"$`", s[i+1]) >= 0 {
					i++
					c = s[i]
				}
			}
			out.WriteByte(c)
			i++
		}
		kept = out.Len()
		if !closed {
			return out.String(), len(s), true
		}
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
	}
	for i < len(s) {
		c := s[i]
		switch {
		case c == '\n':
			i++
			res := out.String()
			return res[:kept] + strings.TrimRightFunc(res[kept:], unicode.IsSpace), i, false
		case c == '\\' && i+1 < len(s):
			i++
			if s[i] != '\n' { // a backslash-newline joins the lines and is itself gone
				out.WriteByte(s[i])
				kept = out.Len()
			}
			i++
		default:
			out.WriteByte(c)
			i++
		}
	}
	res := out.String()
	return res[:kept] + strings.TrimRightFunc(res[kept:], unicode.IsSpace), i, false
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
