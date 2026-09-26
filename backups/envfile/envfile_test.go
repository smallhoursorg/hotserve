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
		`LEADQ="abc`,
		`SWALLOWED=yes`,
		``,
	}, "\n")
	want := map[string]string{
		"PLAIN": "value", "SPACEKEY": "spaced", "DQ": "double quoted", "SQ": "single quoted", "DUP": "second",
		"HASHIN": "a#b", "TRAIL": "trail", "BS": `ab\c`, "DQBS": `a\b\c"d`, "DOLLAR": "$HOME x", "CONT": "one two",
		"EMPTY": "", "MIDQ": `ab"cd"ef`, "TAB": "a\tb", "SEMIIN": "a;b", "PCT": "100%s", "UTF": "héllo",
		"MULTI": "one\ntwo", "AFTERQ": "ab", "AFTERC": "v# prod", "DQDOLLAR": "a$b", "DQBT": "a`b", "SQESC": `it\s'`, "ESCSP": "trail ",
		"ODD": `abc\JOINED=yes`, "EVEN": `abc\`, "NOTJOINED": "yes",
		"LEADQ": "abc\nSWALLOWED=yes\n",
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
		"line 36: the quote is never closed; the manager reads everything after it, to the end of the file, as LEADQ's value",
	} {
		if !strings.Contains(joined, f) {
			t.Errorf("findings lack %q:\n%s", f, joined)
		}
	}
	if len(findings) != 5 {
		t.Errorf("findings = %q, want exactly five", findings)
	}
	// A byte that is not UTF-8 is not a line the manager skips: it
	// refuses the whole file, and no unit that needs it starts.
	got, findings = Parse([]byte("OK=1\nBAD=caf\xe9\nAFTER=2\n"))
	if len(got) != 0 || len(findings) != 1 || !strings.Contains(findings[0], "line 2 holds a byte that is not UTF-8: the manager refuses the whole file, and no unit that needs it starts") {
		t.Errorf("a non-UTF-8 file: %v %q", got, findings)
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
// name os.CreateTemp makes from Write's own pattern, and the nonce
// shape — never a name an operator chose.
func TestALeftoverIsWhatWriteOrSetupLeaves(t *testing.T) {
	dir := t.TempDir()
	tmp, err := os.CreateTemp(dir, tempPattern("repository.env"))
	must(t, err)
	must(t, tmp.Close())
	if !IsLeftover(filepath.Base(tmp.Name()), "repository.env") {
		t.Fatalf("what Write makes on the way, %s, is not a leftover", filepath.Base(tmp.Name()))
	}
	if !IsLeftover("repository.env.0123456789ab", "repository.env") {
		t.Fatal("setup's own file is not a leftover")
	}
	nonced, err := os.CreateTemp(dir, tempPattern("repository.env.0123456789ab"))
	must(t, err)
	must(t, nonced.Close())
	if !IsLeftover(filepath.Base(nonced.Name()), "repository.env") {
		t.Fatalf("what Write makes on the way to setup's file, %s, is not a leftover", filepath.Base(nonced.Name()))
	}
	for _, kept := range []string{"repository.env", "repository.env.bak", "repository.env.old", "repository.env.gs", ".repository.env.swp", "repository.env.0123456789ab.bak", ".repository.env.0123456789ab", "notes.txt", "repository.env.0123456789ABCD"} {
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
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
