package box

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The applier's units and its exchange tree, as the package ships them,
// held to DESIGN-box.md: the units to "The applier unit" (restated here
// as tables, a line each, so that a key added to a unit file is a line
// added to the design first), and the tmpfiles.d file to the Paths
// table itself, read from the document.

const packagingDir = "../packaging"

// The path unit, every line of it. DefaultDependencies=no and the four
// lines after it stand for the defaults a path unit would otherwise
// get: with them, Before=paths.target plus After=hotserve.service is
// an ordering cycle at boot (hotserve.service is After=basic.target,
// which is After=paths.target).
var wantPathUnit = map[string]map[string][]string{
	"Unit": {
		"Description":         {"hotserve box applier trigger"},
		"Documentation":       {"https://github.com/smallhoursorg/hotserve/blob/main/box/DESIGN-box.md"},
		"DefaultDependencies": {"no"},
		"Requires":            {"sysinit.target"},
		"After":               {"sysinit.target hotserve.service"},
		"Before":              {"shutdown.target"},
		"Conflicts":           {"shutdown.target"},
	},
	"Path": {
		// in/ for a bundle, and work/ and the record for what a killed
		// run left: recovery runs at boot because of these two.
		"DirectoryNotEmpty":       {"/var/lib/hotserve-box/in", "/var/lib/hotserve-box/work"},
		"PathExists":              {"/var/lib/hotserve-box/txn.json"},
		"TriggerLimitIntervalSec": {"10s"},
		"TriggerLimitBurst":       {"20"},
	},
	"Install": {
		"WantedBy": {"multi-user.target"},
	},
}

// The service, every line of it: no [Install] (the path unit is its one
// start), root (no User=), no Restart=, no SuccessExitStatus= (exit 0 is
// a settled run, and the one non-zero exit, a full disk, fails the
// unit). StartLimitIntervalSec=0 leaves the path unit's trigger limit
// as the one bound (Caps).
var wantService = map[string]map[string][]string{
	"Unit": {
		"Description":           {"hotserve box applier"},
		"Documentation":         {"https://github.com/smallhoursorg/hotserve/blob/main/box/DESIGN-box.md"},
		"After":                 {"hotserve.service"},
		"StartLimitIntervalSec": {"0"},
	},
	"Service": {
		"Type":                    {"oneshot"},
		"ExecStart":               {"/usr/bin/hotserve box apply"},
		"TimeoutStartSec":         {"infinity"},
		"CapabilityBoundingSet":   {"CAP_SETUID", "CAP_SETGID", "CAP_KILL", "CAP_DAC_OVERRIDE", "CAP_FOWNER"},
		"ProtectSystem":           {"strict"},
		"ReadWritePaths":          {"/etc/hotserve /var/lib/hotserve-box"},
		"PrivateTmp":              {"yes"},
		"NoNewPrivileges":         {"yes"},
		"RestrictAddressFamilies": {"AF_UNIX"},
		"SystemCallFilter":        {"@system-service"},
	},
}

func TestTheShippedUnitsSayWhatTheDesignLists(t *testing.T) {
	for name, want := range map[string]map[string]map[string][]string{
		"hotserve-box-apply.path":    wantPathUnit,
		"hotserve-box-apply.service": wantService,
	} {
		t.Run(name, func(t *testing.T) {
			got := values(readUnit(t, name))
			for _, s := range slices.Sorted(maps.Keys(got)) {
				if _, ok := want[s]; !ok {
					t.Errorf("[%s] is not in the design's list: %q", s, got[s])
				}
			}
			for _, s := range slices.Sorted(maps.Keys(want)) {
				for _, k := range slices.Sorted(maps.Keys(want[s])) {
					if !slices.Equal(got[s][k], want[s][k]) {
						t.Errorf("[%s] %s: want %q, have %q", s, k, want[s][k], got[s][k])
					}
				}
				for _, k := range slices.Sorted(maps.Keys(got[s])) {
					if _, ok := want[s][k]; !ok {
						t.Errorf("[%s] %s=%q is not in the design's list", s, k, got[s][k])
					}
				}
			}
		})
	}
}

// Each capability on a line of its own, with the design's reason for it
// in the comment directly above (the manager ORs the lines together):
// a capability added without a reason, or two on one line sharing one,
// fails here.
func TestEachCapabilityCarriesItsReason(t *testing.T) {
	var caps []string
	for _, l := range readUnit(t, "hotserve-box-apply.service") {
		if l.Key != "CapabilityBoundingSet" {
			continue
		}
		f := strings.Fields(l.Value)
		if len(f) != 1 || !strings.HasPrefix(f[0], "CAP_") {
			t.Errorf("CapabilityBoundingSet=%q: one capability per line, never a reset or a ~", l.Value)
			continue
		}
		if !strings.Contains(l.Comment, f[0]+":") {
			t.Errorf("%s has no reason of its own above it: %q", f[0], l.Comment)
		}
		caps = append(caps, f[0])
	}
	if want := wantService["Service"]["CapabilityBoundingSet"]; !slices.Equal(caps, want) {
		t.Errorf("capabilities %q, want %q", caps, want)
	}
}

// The units and the tree agree: what the path unit watches is in a
// directory tmpfiles.d makes (or is that directory's own child, for the
// record), and the service may write the tree and /etc/hotserve.
func TestTheUnitsWatchAndWriteTheTree(t *testing.T) {
	dirs := map[string]bool{}
	for _, e := range readTmpfiles(t) {
		dirs[e.path] = true
	}
	path := values(readUnit(t, "hotserve-box-apply.path"))["Path"]
	watched := append(slices.Clone(path["DirectoryNotEmpty"]), path["PathExists"]...)
	for _, p := range path["DirectoryNotEmpty"] {
		if !dirs[p] {
			t.Errorf("DirectoryNotEmpty=%s: tmpfiles.d does not make it", p)
		}
	}
	for _, p := range watched {
		if !dirs[filepath.Dir(p)] && !dirs[p] {
			t.Errorf("%s: not in a directory tmpfiles.d makes", p)
		}
	}
	rw := strings.Fields(strings.Join(values(readUnit(t, "hotserve-box-apply.service"))["Service"]["ReadWritePaths"], " "))
	for _, need := range []string{"/etc/hotserve", "/var/lib/hotserve-box"} {
		if !slices.Contains(rw, need) {
			t.Errorf("ReadWritePaths %q lacks %s", rw, need)
		}
	}
	if !dirs["/var/lib/hotserve-box"] {
		t.Error("tmpfiles.d does not make the base directory the service writes")
	}
}

// The tmpfiles.d file makes exactly the Paths table's rows whose
// "Creates" column is tmpfiles.d, each with the table's mode and owner,
// as a directory with no age.
func TestTmpfilesIsThePathsTable(t *testing.T) {
	want := designTmpfilesRows(t)
	got := map[string]string{}
	for _, e := range readTmpfiles(t) {
		if e.typ != "d" || e.age != "-" {
			t.Errorf("%s: type %q age %q, want a directory (d) with no age (-)", e.path, e.typ, e.age)
		}
		if _, dup := got[e.path]; dup {
			t.Errorf("%s: twice", e.path)
		}
		got[e.path] = e.mode + " " + e.user + ":" + e.group
	}
	if !reflect.DeepEqual(got, want) {
		for _, p := range slices.Sorted(maps.Keys(want)) {
			if got[p] != want[p] {
				t.Errorf("%s: tmpfiles.d says %q, the Paths table %q", p, got[p], want[p])
			}
		}
		for _, p := range slices.Sorted(maps.Keys(got)) {
			if _, ok := want[p]; !ok {
				t.Errorf("%s: tmpfiles.d makes it, and the Paths table does not say tmpfiles.d creates it", p)
			}
		}
	}
}

// The reader of the Paths table, on tables that would fool a careless
// one: a cell with a pipe in it is a row of another shape, and stops
// the test rather than being read short.
func TestThePathsTableReader(t *testing.T) {
	head := "## Paths, owners, and who may touch what\n\n| Path | Mode | Owner | Creates | Writes | Reads | Removes |\n|---|---|---|---|---|---|---|\n"
	for _, tc := range []struct {
		name, doc string
		want      map[string]string
		err       string
	}{
		{"the base and a child", head +
			"| `/var/lib/hotserve-box/` | 2750 | root:hotserve | tmpfiles.d | — | — | — |\n" +
			"| `…/in/` | 0770 | root:hotserve | tmpfiles.d | handler | applier | applier |\n" +
			"| `…/txn.json` | 0600 | root:hotserve | applier | applier | applier | applier |\n\n## Next\n" +
			"| `…/out/` | 2750 | root:hotserve | tmpfiles.d | applier | handler | — |\n",
			map[string]string{"/var/lib/hotserve-box": "2750 root:hotserve", "/var/lib/hotserve-box/in": "0770 root:hotserve"}, ""},
		{"no section", "## Caps\n", nil, "no section"},
		{"no rows", head + "\n", nil, "no row"},
		{"a pipe in a cell", head + "| `…/in/` | 0770 | root:hotserve | tmpfiles.d | a | b | c | d |\n", nil, "8 cells"},
		{"a child before the base", head + "| `…/in/` | 0770 | root:hotserve | tmpfiles.d | — | — | — |\n", nil, "before the base"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTmpfilesRows(tc.doc)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("error %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("%v, want %v", got, tc.want)
			}
		})
	}
}

// The unit reader refuses what it does not read the way the manager
// does, and ties a comment to the line directly under it only.
func TestTheUnitReader(t *testing.T) {
	for _, tc := range []struct{ name, unit, err string }{
		{"a continued line", "[Service]\nExecStart=/bin/a \\\n  b\n", "continued"},
		{"a key outside a section", "Type=oneshot\n[Service]\n", "outside a section"},
		{"a line with no =", "[Service]\noneshot\n", "not a key"},
		{"an empty key", "[Service]\n=x\n", "not a key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseUnit(strings.NewReader(tc.unit)); err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("error %v, want %q", err, tc.err)
			}
		})
	}
	lines, err := parseUnit(strings.NewReader("[Service]\n# a: kept\nA=1\n# b: dropped by the blank line\n\nB=2\n; c: kept too\nC=3\nD=4\n"))
	if err != nil {
		t.Fatal(err)
	}
	comments := map[string]string{}
	for _, l := range lines {
		comments[l.Key] = l.Comment
	}
	if want := map[string]string{"A": "a: kept", "B": "", "C": "c: kept too", "D": ""}; !reflect.DeepEqual(comments, want) {
		t.Fatalf("comments %q, want %q", comments, want)
	}
}

// unitLine is one Key=Value line of a unit file, with its section and
// the comment lines directly above it.
type unitLine struct {
	Section, Key, Value, Comment string
}

func readUnit(t *testing.T, name string) []unitLine {
	t.Helper()
	f, err := os.Open(filepath.Join(packagingDir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // read only
	lines, err := parseUnit(f)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return lines
}

// parseUnit reads a unit file the way the manager does for what these
// files say: [Section], Key=Value, comments (# or ;) and blank lines.
// No continuation lines: none is written, and a reader that skipped
// one would read its tail as a key of its own.
func parseUnit(r io.Reader) ([]unitLine, error) {
	var lines []unitLine
	var section string
	var comment []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "":
			comment = nil
		case strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
			comment = append(comment, strings.TrimSpace(line[1:]))
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			section, comment = line[1:len(line)-1], nil
		case strings.HasSuffix(line, "\\"):
			return nil, fmt.Errorf("a continued line, which this reader does not follow: %q", line)
		default:
			k, v, ok := strings.Cut(line, "=")
			if !ok || strings.TrimSpace(k) == "" {
				return nil, fmt.Errorf("not a key: %q", line)
			}
			if section == "" {
				return nil, fmt.Errorf("a key outside a section: %q", line)
			}
			lines = append(lines, unitLine{section, strings.TrimSpace(k), strings.TrimSpace(v), strings.Join(comment, "\n")})
			comment = nil
		}
	}
	return lines, sc.Err()
}

// values is section → key → each line's value, in file order.
func values(lines []unitLine) map[string]map[string][]string {
	out := map[string]map[string][]string{}
	for _, l := range lines {
		if out[l.Section] == nil {
			out[l.Section] = map[string][]string{}
		}
		out[l.Section][l.Key] = append(out[l.Section][l.Key], l.Value)
	}
	return out
}

type tmpfilesEntry struct{ typ, path, mode, user, group, age string }

func readTmpfiles(t *testing.T) []tmpfilesEntry {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(packagingDir, "hotserve-box.tmpfiles"))
	if err != nil {
		t.Fatal(err)
	}
	var out []tmpfilesEntry
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 6 {
			t.Fatalf("tmpfiles.d line %q: want type, path, mode, user, group and age", line)
		}
		out = append(out, tmpfilesEntry{f[0], f[1], f[2], f[3], f[4], f[5]})
	}
	if len(out) == 0 {
		t.Fatal("the tmpfiles.d file makes nothing")
	}
	return out
}

// designTmpfilesRows is DESIGN-box.md's Paths table, the rows whose
// "Creates" column is tmpfiles.d, as path → "mode user:group".
func designTmpfilesRows(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile("DESIGN-box.md")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := parseTmpfilesRows(string(raw))
	if err != nil {
		t.Fatalf("DESIGN-box.md: %v", err)
	}
	return rows
}

// parseTmpfilesRows reads the Paths table out of the document: its
// section ends at the next second-level heading; a row is a line that
// opens with a backquoted path; "…/" in a path stands for the base
// directory, the first row under /var/lib.
func parseTmpfilesRows(doc string) (map[string]string, error) {
	_, section, ok := strings.Cut(doc, "## Paths, owners, and who may touch what\n")
	if !ok {
		return nil, errors.New("no section \"Paths, owners, and who may touch what\"")
	}
	section, _, _ = strings.Cut(section, "\n## ")
	rows := map[string]string{}
	base := ""
	for _, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) != 9 {
			return nil, fmt.Errorf("%d cells, not the table's 7, in %q", len(cells)-2, line)
		}
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		path := strings.TrimSuffix(strings.Trim(cells[1], "`"), "/")
		if rest, ok := strings.CutPrefix(path, "…/"); ok {
			if base == "" {
				return nil, fmt.Errorf("%q comes before the base directory it is under", cells[1])
			}
			path = base + "/" + rest
		} else if base == "" && strings.HasPrefix(path, "/var/lib/") {
			base = path
		}
		if cells[4] == "tmpfiles.d" {
			rows[path] = cells[2] + " " + cells[3]
		}
	}
	if len(rows) == 0 {
		return nil, errors.New("no row the table says tmpfiles.d creates")
	}
	return rows, nil
}
