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
// held to DESIGN-box.md and read from it: the units, line for line, to
// the table in "The applier unit", and the tmpfiles.d file to the Paths
// table. Neither list is restated here, so the design cannot say one
// thing while the package ships another.

const (
	packagingDir = "../packaging"
	pathUnit     = "hotserve-box-apply.path"
	serviceUnit  = "hotserve-box-apply.service"
)

// forbidden is what neither unit may say, whatever the design's table
// says — the contract that would break if the two moved together: the
// applier is root (no User=, Group=, DynamicUser=, and no capability
// granted beyond its bounding set), the path unit is what starts it
// again (no Restart=), and its one non-zero exit, a full disk, fails
// the unit (no SuccessExitStatus=).
var forbidden = []string{"User", "Group", "DynamicUser", "Restart", "SuccessExitStatus", "AmbientCapabilities"}

func TestTheShippedUnitsSayWhatTheDesignLists(t *testing.T) {
	design := designUnits(t)
	for _, name := range []string{pathUnit, serviceUnit} {
		t.Run(name, func(t *testing.T) {
			want, got := design[name], values(readUnit(t, name))
			for _, s := range slices.Sorted(maps.Keys(got)) {
				if _, ok := want[s]; !ok {
					t.Errorf("[%s] is not in the design's table: %q", s, got[s])
				}
			}
			for _, s := range slices.Sorted(maps.Keys(want)) {
				for _, k := range slices.Sorted(maps.Keys(want[s])) {
					if !slices.Equal(got[s][k], want[s][k]) {
						t.Errorf("[%s] %s: the design says %q, the unit %q", s, k, want[s][k], got[s][k])
					}
				}
				for _, k := range slices.Sorted(maps.Keys(got[s])) {
					if _, ok := want[s][k]; !ok {
						t.Errorf("[%s] %s=%q is not in the design's table", s, k, got[s][k])
					}
				}
			}
			for _, u := range []map[string]map[string][]string{want, got} {
				for s, keys := range u {
					for _, k := range forbidden {
						if v, ok := keys[k]; ok {
							t.Errorf("[%s] %s=%q: never, whatever the table says", s, k, v)
						}
					}
				}
			}
		})
	}
	// The contract with the applier, which the table must keep.
	svc := design[serviceUnit]
	if got := svc["Service"]["ExecStart"]; !slices.Equal(got, []string{"/usr/bin/hotserve box apply"}) {
		t.Errorf("ExecStart %q: the applier is `/usr/bin/hotserve box apply`", got)
	}
	if _, ok := svc["Install"]; ok {
		t.Error("the service has an [Install] section: the path unit is its only start")
	}
}

// Each capability on a line of its own, with a reason in the design's
// table and in the comment directly above it in the unit (the manager
// ORs the lines together): a capability added without a reason, or two
// on one line sharing one, fails here.
func TestEachCapabilityCarriesItsReason(t *testing.T) {
	reasons := map[string]string{}
	for _, r := range designRows(t) {
		if r.unit == serviceUnit && r.key == "CapabilityBoundingSet" {
			reasons[r.value] = r.why
		}
	}
	if len(reasons) == 0 {
		t.Fatal("the design's table grants no capability; the verifier's child could not change uid")
	}
	var caps []string
	for _, l := range readUnit(t, serviceUnit) {
		if l.Key != "CapabilityBoundingSet" {
			continue
		}
		f := strings.Fields(l.Value)
		if len(f) != 1 || !strings.HasPrefix(f[0], "CAP_") {
			t.Errorf("CapabilityBoundingSet=%q: one capability per line, never a reset or a ~", l.Value)
			continue
		}
		if !strings.Contains(l.Comment, f[0]+":") {
			t.Errorf("%s has no reason of its own above it in the unit: %q", f[0], l.Comment)
		}
		if reasons[f[0]] == "" {
			t.Errorf("%s has no reason in the design's table", f[0])
		}
		caps = append(caps, f[0])
	}
	if want := slices.Sorted(maps.Keys(reasons)); !slices.Equal(slices.Sorted(slices.Values(caps)), want) {
		t.Errorf("capabilities %q, the design's %q", caps, want)
	}
}

// The units and the tree agree, across the design's two tables: what
// the path unit watches is a directory tmpfiles.d makes, or the record
// in its base; the service may write the tree and /etc/hotserve; and
// each watched directory counts every name in it — DirectoryNotEmpty=
// skips the names systemd takes for hidden or backup files, and the
// three globs beside it are what count those.
func TestTheUnitsWatchAndWriteTheTree(t *testing.T) {
	dirs := map[string]bool{}
	for _, e := range readTmpfiles(t) {
		dirs[e.path] = true
	}
	path := values(readUnit(t, pathUnit))["Path"]
	globs := path["PathExistsGlob"]
	for _, d := range path["DirectoryNotEmpty"] {
		if !dirs[d] {
			t.Errorf("DirectoryNotEmpty=%s: tmpfiles.d does not make it", d)
		}
		for _, g := range []string{"/*", "/.[!.]*", "/..?*"} {
			if !slices.Contains(globs, d+g) {
				t.Errorf("no PathExistsGlob=%s%s: an entry named so in %s would never start a run", d, g, d)
			}
		}
	}
	for _, g := range globs {
		if !slices.Contains(path["DirectoryNotEmpty"], filepath.Dir(g)) {
			t.Errorf("PathExistsGlob=%s watches a directory that is not one of the watched", g)
		}
	}
	for _, p := range path["PathExists"] {
		if !dirs[filepath.Dir(p)] {
			t.Errorf("PathExists=%s: not in a directory tmpfiles.d makes", p)
		}
	}
	rw := strings.Fields(strings.Join(values(readUnit(t, serviceUnit))["Service"]["ReadWritePaths"], " "))
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

// The reader of the units' table, likewise: a row it cannot read
// stops the test, and the section ends at the next second-level
// heading.
func TestTheUnitTableReader(t *testing.T) {
	head := "## The applier unit\n\n| Unit | Section | Line | Why |\n|---|---|---|---|\n"
	both := "| `.path` | `[Path]` | `PathExists=/x` | a |\n| `.service` | `[Service]` | `Type=oneshot` | |\n"
	for _, tc := range []struct {
		name, doc string
		want      []unitRow
		err       string
	}{
		{"both units, then another section", head + both + "| `.path` | `[Path]` | `PathExists=/y` | b |\n\n## Next\n| `.path` | `[Path]` | `PathExists=/z` | c |\n",
			[]unitRow{{pathUnit, "Path", "PathExists", "/x", "a"}, {serviceUnit, "Service", "Type", "oneshot", ""}, {pathUnit, "Path", "PathExists", "/y", "b"}}, ""},
		{"no section", "## Caps\n", nil, "no section"},
		{"one unit only", head + "| `.path` | `[Path]` | `PathExists=/x` | a |\n", nil, "both units"},
		{"a pipe in a cell", head + both + "| `.path` | `[Path]` | `PathExists=/x` | a | b |\n", nil, "5 cells"},
		{"an unknown unit", head + both + "| `.timer` | `[Timer]` | `OnCalendar=daily` | |\n", nil, "unknown unit"},
		{"a line that is not a key", head + both + "| `.path` | `[Path]` | `PathExists` | |\n", nil, "not a key"},
		{"a section not in brackets", head + both + "| `.path` | `Path` | `PathExists=/x` | |\n", nil, "section"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseUnitTable(tc.doc)
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

// unitRow is one row of the design's table of unit lines.
type unitRow struct{ unit, section, key, value, why string }

func designRows(t *testing.T) []unitRow {
	t.Helper()
	rows, err := parseUnitTable(readDesign(t))
	if err != nil {
		t.Fatalf("DESIGN-box.md: %v", err)
	}
	return rows
}

// designUnits is the design's table as unit → section → key → values,
// in table order.
func designUnits(t *testing.T) map[string]map[string]map[string][]string {
	t.Helper()
	out := map[string]map[string]map[string][]string{}
	for _, r := range designRows(t) {
		if out[r.unit] == nil {
			out[r.unit] = map[string]map[string][]string{}
		}
		if out[r.unit][r.section] == nil {
			out[r.unit][r.section] = map[string][]string{}
		}
		out[r.unit][r.section][r.key] = append(out[r.unit][r.section][r.key], r.value)
	}
	return out
}

// parseUnitTable reads the table in "The applier unit": a row is a line
// that opens with a backquoted unit suffix, then the section in
// brackets, the line, and why. The section ends at the next
// second-level heading.
func parseUnitTable(doc string) ([]unitRow, error) {
	_, section, ok := strings.Cut(doc, "## The applier unit\n")
	if !ok {
		return nil, errors.New("no section \"The applier unit\"")
	}
	section, _, _ = strings.Cut(section, "\n## ")
	var rows []unitRow
	seen := map[string]bool{}
	for _, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(line, "| `.") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) != 6 {
			return nil, fmt.Errorf("%d cells, not the table's 4, in %q", len(cells)-2, line)
		}
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		unit := "hotserve-box-apply" + strings.Trim(cells[1], "`")
		if unit != pathUnit && unit != serviceUnit {
			return nil, fmt.Errorf("an unknown unit %s in %q", cells[1], line)
		}
		sec := strings.Trim(cells[2], "`")
		if !strings.HasPrefix(sec, "[") || !strings.HasSuffix(sec, "]") {
			return nil, fmt.Errorf("a section not written [Section] in %q", line)
		}
		k, v, ok := strings.Cut(strings.Trim(cells[3], "`"), "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("not a key: %q", cells[3])
		}
		rows = append(rows, unitRow{unit, sec[1 : len(sec)-1], k, v, cells[4]})
		seen[unit] = true
	}
	if !seen[pathUnit] || !seen[serviceUnit] {
		return nil, errors.New("the table must have rows for both units")
	}
	return rows, nil
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

func readDesign(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("DESIGN-box.md")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// designTmpfilesRows is DESIGN-box.md's Paths table, the rows whose
// "Creates" column is tmpfiles.d, as path → "mode user:group".
func designTmpfilesRows(t *testing.T) map[string]string {
	t.Helper()
	rows, err := parseTmpfilesRows(readDesign(t))
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
