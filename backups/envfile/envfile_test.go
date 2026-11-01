package envfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What the manager makes of a line is measured
// (TestIntegrationSystemdReadsAnEnvFileAsParseDoes); what is pinned
// here is that Parse agrees with those measurements, and says where a
// line is not read as it was written.
func TestParseReadsAsTheManagerDoes(t *testing.T) {
	raw := strings.Join([]string{
		`PLAIN=value`,
		` SPACEKEY = spaced `,
		`DQ="double quoted"`,
		`SQ='single quoted'`,
		`DUP=first`,
		`DUP=second`,
		`# COMMENT=no`,
		`; SEMI=no`,
		`HASHIN=a#b`,
		`TRAIL=trail   `,
		`BS=a\b\\c`,
		`DQBS="a\b\\c\"d"`,
		`DOLLAR=$HOME x`,
		`CONT=one \`,
		`two`,
		`EMPTY=`,
		`NOEQ`,
		`MIDQ=ab"cd"ef`,
		`TAB=a	b`,
		`SEMIIN=a;b`,
		`PCT=100%s`,
		`UTF=héllo`,
		`MULTI="one`,
		`two"`,
		`AFTERQ="a" b`,
		`AFTERC="v" # prod`,
		`DQDOLLAR="a\$b"`,
		"DQBT=\"a\\`b\"",
		`SQESC='it\'s'`,
		`ESCSP=trail\ `,
		`export EXP=1`,
		`ODD=abc\\\`,
		`JOINED=yes`,
		`EVEN=abc\\`,
		`NOTJOINED=yes`,
		`SQCONT='a\`,
		`b'`,
		`DQCONT="a\`,
		`b"`,
		`AFTERQC="v"\`,
		`NEXT=1`,
		`AFTERQS="v" x\`,
		`NEXT2=2`,
		"NBSPV=bucket\u00a0",
		"\u00a0NBSPK=v",
		`REQ="a" "b"`,
		`SQIDIOM='it'"'"'s'`,
		"CRCONT=one \\\r",
		`CRB=2`,
		"CRMID=x\ry",
		"CRQ=\"a\rb\"",
		"  # an indented comment",
		`AFTERIC=1`,
		`LEADQ="abc`,
		`SWALLOWED=yes`,
		`EOFBS=abc\`,
	}, "\n")
	want := map[string]string{
		"PLAIN": "value", "SPACEKEY": "spaced", "DQ": "double quoted", "SQ": "single quoted", "DUP": "second",
		"HASHIN": "a#b", "TRAIL": "trail", "BS": `ab\c`, "DQBS": `a\b\c"d`, "DOLLAR": "$HOME x", "CONT": "one two",
		"EMPTY": "", "MIDQ": `ab"cd"ef`, "TAB": "a\tb", "SEMIIN": "a;b", "PCT": "100%s", "UTF": "héllo",
		"MULTI": "one\ntwo", "AFTERQ": "ab", "AFTERC": "v# prod", "DQDOLLAR": "a$b", "DQBT": "a`b", "SQESC": `it\s'`, "ESCSP": "trail ",
		"ODD": `abc\JOINED=yes`, "EVEN": `abc\`, "NOTJOINED": "yes", "SQCONT": "a\\\nb", "DQCONT": "ab", "AFTERQC": "vNEXT=1", "AFTERQS": "vxNEXT2=2",
		"NBSPV": "bucket\u00a0", "REQ": "ab", "SQIDIOM": "it's", "CRCONT": "one ", "CRB": "2", "CRMID": "x", "CRQ": "a\rb",
		"AFTERIC": "1", "LEADQ": "abc\nSWALLOWED=yes\nEOFBS=abc",
	}
	got, findings := Parse([]byte(raw))
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s = %q was read, and the manager does not read it", k, got[k])
		}
	}
	joined := strings.Join(findings, "\n")
	for _, f := range []string{
		"line 2: the key is written with whitespace around it; the manager reads it as SPACEKEY",
		"line 6: DUP is set again; the manager takes this one",
		"line 17: not KEY=value; the manager skips it",
		`line 31: "export EXP" is not a name the manager takes; it skips the line`,
		`line 45: "\u00a0NBSPK" is not a name the manager takes; it skips the line`,
		"line 50: not KEY=value; the manager skips it",
		"line 54: the quote is never closed; the manager reads everything after it, to the end of the file, as LEADQ's value",
	} {
		if !strings.Contains(joined, f) {
			t.Errorf("findings lack %q:\n%s", f, joined)
		}
	}
	if len(findings) != 7 {
		t.Errorf("findings = %q, want exactly seven", findings)
	}
	// A backslash as the file's last byte is dropped, and what it
	// follows is kept as it is.
	got, findings = Parse([]byte("A=x\nB=abc \\"))
	if got["B"] != "abc " || len(findings) != 0 {
		t.Errorf("a backslash at the end of the file: %q %q", got, findings)
	}
	// The manager refuses the whole file for a NUL anywhere, and for a
	// byte that is not UTF-8 in a key or a value, quoted or not; not for
	// one in a comment or a line with no = [M91].
	for raw, want := range map[string]string{
		"OK=1\nBAD=caf\xe9\nAFTER=2\n":     "line 2: the value of BAD holds a byte that is not UTF-8: the manager refuses the whole file, and no unit that needs it starts",
		"OK=1\nBAD=\"caf\xe9\"\nAFTER=2\n": "line 2: the value of BAD holds a byte that is not UTF-8: the manager refuses the whole file, and no unit that needs it starts",
		"OK=1\ncaf\xe9=1\nAFTER=2\n":       "line 2: a key holds a byte that is not UTF-8: the manager refuses the whole file, and no unit that needs it starts",
		// Named by the key, from its line, in a value over several.
		"OK=1\nNOTE=\"first\nsecond\ncaf\xe9\"\nAFTER=2\n": "line 2: the value of NOTE holds a byte that is not UTF-8: the manager refuses the whole file, and no unit that needs it starts",
		"OK=1\nNOTE=a \\\ncaf\xe9\nAFTER=2\n":              "line 2: the value of NOTE holds a byte that is not UTF-8: the manager refuses the whole file, and no unit that needs it starts",
		// A name the manager does not take is quoted, its control bytes
		// escaped, never sent to a terminal as they are.
		"OK=1\n\x1b]0;x\x07X=caf\xe9\nAFTER=2\n": "line 2: the value of \"\\x1b]0;x\\aX\" holds a byte that is not UTF-8: the manager refuses the whole file, and no unit that needs it starts",
		// A sequence split by a joined line, valid once joined; the byte
		// after it is not.
		"OK=1\nB=\"\xc3\\\n\xa9 \xff\"\nAFTER=2\n": "line 2: the value of B holds a byte that is not UTF-8: the manager refuses the whole file, and no unit that needs it starts",
		"OK=1\n# a\x00b\nAFTER=2\n":                "line 2 holds a NUL byte: the manager refuses the whole file, and no unit that needs it starts",
		"OK=1\nBAD=a\x00b\nAFTER=2\n":              "line 2 holds a NUL byte: the manager refuses the whole file, and no unit that needs it starts",
		"OK=1\n# caf\xe9\nAFTER=2\n":               "",
		"OK=1\ncaf\xe9\nAFTER=2\n":                 "",
	} {
		got, findings = Parse([]byte(raw))
		if RefusedWhole(findings) != want || want != "" && len(got) != 0 {
			t.Errorf("%q: %v %q, want refused %q", raw, got, findings, want)
		}
		if want == "" && (got["OK"] != "1" || got["AFTER"] != "2") {
			t.Errorf("%q: %v, want OK and AFTER read", raw, got)
		}
	}
}

func TestNothingIsFoundWithAFileTheWriterWrote(t *testing.T) {
	// Awkward values are written quoted, as the manager reads them
	// [measured]: a leading quote, whitespace at either end.
	pairs := []Pair{{"RESTIC_REPOSITORY", "s3:https://h/b"}, {"RESTIC_PASSWORD", `p#a$s;s"w'o=rd`}, {"K", `back\slash`},
		{"QLEAD", `"quoted"`}, {"SQLEAD", `'q`}, {"QPAD", "  both  "}, {"QTRAIL", `trail\ `}}
	raw, err := Format(pairs)
	if err != nil {
		t.Fatal(err)
	}
	got, findings := Parse(raw)
	if len(findings) != 0 {
		t.Fatalf("findings on a file the writer wrote: %q", findings)
	}
	for _, p := range pairs {
		if got[p.Key] != p.Value {
			t.Errorf("%s: wrote %q, read back %q", p.Key, p.Value, got[p.Key])
		}
	}
	// A backslash is escaped on the way out, so the manager reads it as one.
	if !strings.Contains(string(raw), "K=back\\\\slash\n") {
		t.Fatalf("a backslash is not doubled: %s", raw)
	}
	if !strings.Contains(string(raw), "QLEAD=\"\\\"quoted\\\"\"\n") || !strings.Contains(string(raw), "QPAD=\"  both  \"\n") {
		t.Fatalf("awkward values are not quoted: %s", raw)
	}
	if strings.Contains(string(raw), "RESTIC_REPOSITORY=\"") {
		t.Fatalf("a plain value was quoted: %s", raw)
	}
}

func TestWhatCannotBeWrittenFaithfullyIsRefused(t *testing.T) {
	for _, tc := range []struct{ value, why string }{
		{"a\nb", "a line break"},
		{"a\rb", "a control character"},
		{"a\x01b", "a control character"},
		{"\ttab", "a control character"},
		{"", "empty"},
		{"\xff", "not UTF-8"},
	} {
		err := Refuse(tc.value)
		if err == nil || !strings.Contains(err.Error(), tc.why) {
			t.Errorf("Refuse(%q) = %v, want %q", tc.value, err, tc.why)
		}
	}
	for _, ok := range []string{"a b", "a#b", "a$b", "a=b", `a"b`, "mid'quote", `a\b`, "é", "100%", " lead", "trail ", `"quoted"`, `'quoted'`} {
		if err := Refuse(ok); err != nil {
			t.Errorf("Refuse(%q) = %v, want nil", ok, err)
		}
	}
	if _, err := Format([]Pair{{"bad key", "v"}}); err == nil || !strings.Contains(err.Error(), "bad key") {
		t.Errorf("a key with a space: %v", err)
	}
	if _, err := Format([]Pair{{"K", "a\nb"}}); err == nil {
		t.Error("Format wrote a value Refuse refuses")
	}
}

func TestWriteIsWholeAndRootOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "repository.env")
	must(t, os.WriteFile(path, []byte("OLD=1\n"), 0o600))
	// A write that is refused leaves the old file untouched, and no
	// temp file beside it.
	if err := Write(path, []Pair{{"K", "a\nb"}}); err == nil {
		t.Fatal("a value with a line break was written")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "OLD=1\n" {
		t.Fatalf("the old file was changed: %q", raw)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("left beside it: %v", entries)
	}
	must(t, Write(path, []Pair{{"K", "v"}, {"L", "w"}}))
	raw, _ = os.ReadFile(path)
	if string(raw) != "K=v\nL=w\n" {
		t.Fatalf("written: %q", raw)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o, want 0600", st.Mode().Perm())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("left beside it: %v", entries)
	}
}

// What a sweep may remove is exactly what Write and setup leave: the
// staged file, under the one name Staged gives it, and the name
// os.CreateTemp makes from Write's own pattern on the way to either —
// never a name an operator chose.
func TestALeftoverIsWhatWriteOrSetupLeaves(t *testing.T) {
	dir := t.TempDir()
	tmp, err := os.CreateTemp(dir, tempPattern("repository.env"))
	must(t, err)
	must(t, tmp.Close())
	if !IsLeftover(filepath.Base(tmp.Name()), "repository.env") {
		t.Fatalf("what Write makes on the way, %s, is not a leftover", filepath.Base(tmp.Name()))
	}
	if Staged("/etc/hotserve-backup/repository.env") != "/etc/hotserve-backup/repository.env.staged" {
		t.Fatalf("the staged name: %s", Staged("/etc/hotserve-backup/repository.env"))
	}
	if !IsLeftover(filepath.Base(Staged("repository.env")), "repository.env") {
		t.Fatal("setup's own staged file is not a leftover")
	}
	staged, err := os.CreateTemp(dir, tempPattern(filepath.Base(Staged("repository.env"))))
	must(t, err)
	must(t, staged.Close())
	if !IsLeftover(filepath.Base(staged.Name()), "repository.env") {
		t.Fatalf("what Write makes on the way to the staged file, %s, is not a leftover", filepath.Base(staged.Name()))
	}
	for _, kept := range []string{"repository.env", "repository.env.bak", "repository.env.old", "repository.env.gs", ".repository.env.swp", "repository.env.staged.bak", ".repository.env.staged", "repository.env.0123456789ab", ".repository.env.0123456789ab-4207310592", "notes.txt", "repository.env.STAGED"} {
		if IsLeftover(kept, "repository.env") {
			t.Errorf("%s would be swept", kept)
		}
	}
}

func TestLintNamesWhatARunNeeds(t *testing.T) {
	got, findings := Parse([]byte("RESTIC_REPOSITORY=s3:https://h/b\n"))
	lines := Lint(got, findings)
	if len(lines) != 1 || !strings.Contains(lines[0], "RESTIC_PASSWORD is not set") {
		t.Fatalf("lint = %q", lines)
	}
	got, findings = Parse([]byte(" RESTIC_PASSWORD = x\nRESTIC_PASSWORD=y\n"))
	lines = Lint(got, findings)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"RESTIC_REPOSITORY is not set", "whitespace around it", "set again"} {
		if !strings.Contains(joined, want) {
			t.Errorf("lint lacks %q:\n%s", want, joined)
		}
	}
	// Set to nothing is as good as not set: restic opens nothing with it.
	got, findings = Parse([]byte("RESTIC_REPOSITORY=\nRESTIC_PASSWORD=\"\"\n"))
	lines = Lint(got, findings)
	if len(lines) != 2 || !strings.Contains(lines[0], "RESTIC_REPOSITORY is set to nothing") || !strings.Contains(lines[1], "RESTIC_PASSWORD is set to nothing") {
		t.Errorf("lint of empty values = %q", lines)
	}
	// A file the manager refuses whole: that alone, not keys "not set"
	// that are there.
	got, findings = Parse([]byte("RESTIC_REPOSITORY=s3:https://h/b\n# a\x00b\nRESTIC_PASSWORD=x\n"))
	lines = Lint(got, findings)
	if len(lines) != 1 || !strings.Contains(lines[0], "line 2 holds a NUL byte: the manager refuses the whole file") {
		t.Errorf("lint of a file the manager refuses whole = %q", lines)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
