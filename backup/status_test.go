package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

var statusNow = time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)

func snapshotsJSON(entries ...string) []byte {
	return []byte("[" + strings.Join(entries, ",") + "]")
}

func snap(id, app string, age time.Duration) string {
	return snapOf(id, app, age, "/var/lib/hotserve-backup/"+app)
}

// snapOf is a backup of app that was given paths.
func snapOf(id, app string, age time.Duration, paths ...string) string {
	quoted, _ := json.Marshal(paths)
	return fmt.Sprintf(`{"id":%q,"short_id":%q,"time":%q,"tags":["hotserve","app:%s"],"paths":%s}`,
		id, id, statusNow.Add(-age).Format(time.RFC3339Nano), app, quoted)
}

// record is a clean-run record, as the job writes it, from host.
func record(app, host string, age time.Duration, of string) string {
	return fmt.Sprintf(`{"short_id":"rec-%s","time":%q,"hostname":%q,"tags":[%q,%q,%q]}`,
		of, statusNow.Add(-age).Format(time.RFC3339Nano), host, CleanTag, cleanAppTag(app), cleanOfTag(of))
}

func capturing(out []byte, err error) (Exec, *[]call) {
	calls := &[]call{}
	return fake(nil, func(_ context.Context, name string, args ...string) ([]byte, error) {
		*calls = append(*calls, call{name, args})
		return out, err
	}), calls
}

func TestStatusMatchesSnapshotsToApps(t *testing.T) {
	apps := []App{
		testApp("blog", StateEntry{Kind: KindSQLite, Path: "app.db"}, StateEntry{Kind: KindFiles, Path: "uploads"}),
		testApp("shop", StateEntry{Kind: KindFiles, Path: "uploads"}),
		testApp("wiki", StateEntry{Kind: KindSQLite, Path: "wiki.db"}),
	}
	capture, calls := capturing(snapshotsJSON(
		snap("aaa", "blog", 3*time.Hour),
		snap("bbb", "blog", 12*time.Minute),
		snap("ccc", "shop", 20*time.Minute),
		// blog's last run finished cleanly, which is what makes its
		// snapshot evidence of a working backup rather than of restic
		// having written one on the way to failing. The record is not a
		// backup itself: blog has two snapshots, not three.
		record("blog", "box-1", 11*time.Minute, "bbb"),
		record("shop", "box-1", 19*time.Minute, "ccc"),
	), nil)

	got, err := Status(context.Background(), apps, capture, "box-1")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(*calls) != 1 {
		t.Errorf("want one restic call for every app, got %d", len(*calls))
	}
	if args := strings.Join((*calls)[0].args, " "); !strings.Contains(args, "--tag hotserve --tag "+CleanTag) {
		t.Errorf("one call lists both the backups and the records: %v", args)
	}
	if got[0].App.Name != "blog" || got[1].App.Name != "shop" || got[2].App.Name != "wiki" {
		t.Fatalf("apps out of order: %+v", got)
	}
	if got[0].Snapshots != 2 || got[0].Latest.ShortID != "bbb" {
		t.Errorf("blog: want 2 snapshots with bbb newest, got %d/%v", got[0].Snapshots, got[0].Latest)
	}
	// An app that declares state but has never been backed up is the
	// case worth catching — a typo'd path looks exactly like this.
	if got[2].Latest != nil || got[2].Snapshots != 0 {
		t.Errorf("wiki should have no snapshots: %+v", got[2])
	}
	if !got[2].Stale(statusNow) {
		t.Error("an app with no backup at all must read as stale")
	}
	if got[0].Stale(statusNow) {
		t.Error("a 12-minute-old backup is current")
	}
	// Each app's freshness is its own record's, not its neighbour's.
	if got[1].Stale(statusNow) || got[1].LastSuccess.Equal(got[0].LastSuccess) {
		t.Errorf("shop's clean run is its own, 19 minutes ago: %+v", got[1])
	}
}

func TestStaleAfterTwoMissedRuns(t *testing.T) {
	for _, tc := range []struct {
		age   time.Duration
		stale bool
	}{
		{30 * time.Minute, false},
		{90 * time.Minute, false},
		{StaleAfter - time.Minute, false},
		{StaleAfter + time.Minute, true},
		{72 * time.Hour, true},
	} {
		// With a clean run recorded at the same moment as the snapshot:
		// freshness is measured from the run, and a snapshot with no
		// clean run behind it is stale whatever its age (below).
		s := AppStatus{
			Latest:      &Snapshot{Time: statusNow.Add(-tc.age)},
			LastSuccess: statusNow.Add(-tc.age),
		}
		if got := s.Stale(statusNow); got != tc.stale {
			t.Errorf("age %v: stale = %v, want %v", tc.age, got, tc.stale)
		}
	}
}

// restic writes a snapshot even when it exits non-zero (unreadable
// sources, exit 3), so "a snapshot exists" is not "the backup
// worked". Freshness comes from the record a clean run writes, or a
// box whose every run fails part-way stays green for ever.
func TestStaleMeasuresFromTheLastCleanRun(t *testing.T) {
	// Snapshots every hour, but nothing has finished cleanly in a day.
	failing := AppStatus{
		Latest:      &Snapshot{Time: statusNow.Add(-10 * time.Minute)},
		LastSuccess: statusNow.Add(-24 * time.Hour),
	}
	if !failing.Stale(statusNow) {
		t.Error("recent snapshots from failing runs must not read as current")
	}
	healthy := AppStatus{
		Latest:      &Snapshot{Time: statusNow.Add(-10 * time.Minute)},
		LastSuccess: statusNow.Add(-10 * time.Minute),
	}
	if healthy.Stale(statusNow) {
		t.Error("a clean run ten minutes ago is current")
	}
}

// The failure the records exist to catch: restic writes a snapshot and
// still exits non-zero, so a box whose every run fails part-way gets a
// fresh snapshot every hour. Falling back to the newest snapshot when
// no clean run has ever been recorded would call that healthy for ever.
func TestStaleWithSnapshotsButNoCleanRunEverRecorded(t *testing.T) {
	failing := AppStatus{Latest: &Snapshot{Time: statusNow.Add(-5 * time.Minute)}}
	if !failing.Stale(statusNow) {
		t.Error("snapshots from runs that never finished cleanly are not a backup")
	}
	var out strings.Builder
	FormatStatus(&out, []AppStatus{{
		App:    testApp("blog", StateEntry{Kind: KindFiles, Path: "uploads"}),
		Latest: &Snapshot{Time: statusNow.Add(-5 * time.Minute), ShortID: "aaa"},
	}}, statusNow)
	if !strings.Contains(out.String(), "no clean run on this box") {
		t.Errorf("the report has to say why a recent snapshot is not current:\n%s", out.String())
	}
}

// A first backup of a large uploads dir takes hours. The report has to
// say it is under way, or "never ⚠" reads as something broken — while
// staying honest that there is no backup yet (--check still fails:
// there is nothing to restore until it finishes).
func TestFormatStatusSaysWhenABackupIsRunning(t *testing.T) {
	var out strings.Builder
	first := AppStatus{App: testApp("blog", StateEntry{Kind: KindFiles, Path: "uploads"}), Running: true}
	FormatStatus(&out, []AppStatus{first}, statusNow)
	for _, want := range []string{"first backup running now", "journalctl -u hotserve-backup-blog -f"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("want %q in the report:\n%s", want, out.String())
		}
	}
	if !first.Stale(statusNow) {
		t.Error("a first backup still running is not a backup yet")
	}

	out.Reset()
	later := AppStatus{
		App:         testApp("blog", StateEntry{Kind: KindFiles, Path: "uploads"}),
		Latest:      &Snapshot{Time: statusNow.Add(-50 * time.Minute), ShortID: "aaa"},
		LastSuccess: statusNow.Add(-50 * time.Minute),
		Running:     true,
	}
	FormatStatus(&out, []AppStatus{later}, statusNow)
	if !strings.Contains(out.String(), "(backing up now)") {
		t.Errorf("an hourly run in progress should say so:\n%s", out.String())
	}
}

// The snapshots are the backup: an app with none in this repository is
// stale whatever else is known about it.
func TestStaleWhenTheRepositoryHoldsNoSnapshotForTheApp(t *testing.T) {
	switched := AppStatus{LastSuccess: statusNow.Add(-10 * time.Minute)}
	if !switched.Stale(statusNow) {
		t.Error("no snapshot in this repository is stale, whatever else is known")
	}
}

// Freshness is this box's clean runs into THIS repository. A record
// from another box writing to the same repository does not count; and
// after `init --force` onto another repository that already holds this
// app's snapshots, the listing is that repository's, so the old
// repository's runs cannot vouch for it.
func TestStatusCountsOnlyThisBoxsRecordsInThisRepository(t *testing.T) {
	apps := []App{testApp("blog", StateEntry{Kind: KindFiles, Path: "uploads"})}
	capture, _ := capturing(snapshotsJSON(
		snap("aaa", "blog", 10*time.Minute),
		record("blog", "box-2", 9*time.Minute, "aaa"),
	), nil)
	got, err := Status(context.Background(), apps, capture, "box-1")
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].LastSuccess.IsZero() || !got[0].Stale(statusNow) {
		t.Errorf("another box's clean run is not this box's: %+v", got[0])
	}
	if len(got[0].NeverThere) != 0 {
		t.Errorf("nor does it say what this box has found: %v", got[0].NeverThere)
	}
	// The switched-to repository: snapshots of blog, no record of a
	// clean run from here.
	capture, _ = capturing(snapshotsJSON(snap("old", "blog", 5*time.Minute)), nil)
	got, err = Status(context.Background(), apps, capture, "box-1")
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].Stale(statusNow) {
		t.Error("a repository with no clean run recorded from this box must not read as current")
	}
}

// A declared path no clean run has found is either not created yet or
// a typo; the job cannot tell which, so the report names it — without
// calling the app stale, which a new app's empty uploads dir is not.
func TestStatusNamesADeclaredPathThatHasNeverBeenThere(t *testing.T) {
	app := testApp("blog", StateEntry{Kind: KindFiles, Path: "uploads/"}, StateEntry{Kind: KindFiles, Path: "upload"})
	// What this box's newest clean run was given is in the snapshot it
	// vouches for — and only that one counts: an older snapshot that
	// held `upload`, and another box's clean run, say nothing about now.
	capture, _ := capturing(snapshotsJSON(
		snapOf("old", "blog", 3*time.Hour, app.Shared+"/uploads", app.Shared+"/upload"),
		snapOf("aaa", "blog", time.Hour, app.Shared+"/uploads"),
		record("blog", "box-1", 59*time.Minute, "aaa"),
		snapOf("bbb", "blog", 30*time.Minute, app.Shared+"/uploads", app.Shared+"/upload"),
		record("blog", "box-2", 29*time.Minute, "bbb"),
	), nil)
	got, err := Status(context.Background(), []App{app}, capture, "box-1")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got[0].NeverThere, []string{"upload"}) {
		t.Fatalf("want only the path no run has found, got %v", got[0].NeverThere)
	}
	if got[0].Stale(statusNow) {
		t.Error("a path not created yet must not make the app stale: --check would page someone for an empty uploads dir")
	}
	var b strings.Builder
	FormatStatus(&b, got, statusNow)
	if !strings.Contains(b.String(), "upload has not existed at any backup yet") || !strings.Contains(b.String(), "`state files upload` has the path wrong") {
		t.Errorf("the report must name the path and the typo it may be:\n%s", b.String())
	}
	// Before any clean run on this box there is nothing to compare
	// with, which is not news: say nothing.
	capture, _ = capturing(snapshotsJSON(snapOf("aaa", "blog", time.Hour, app.Shared+"/uploads")), nil)
	got, _ = Status(context.Background(), []App{app}, capture, "box-1")
	if len(got[0].NeverThere) != 0 {
		t.Errorf("without a clean run there is nothing to compare with, got %v", got[0].NeverThere)
	}
}

// The report has to show the difference, or an operator reads the
// snapshot's age and thinks all is well.
func TestFormatStatusSaysWhenSnapshotsOutrunCleanRuns(t *testing.T) {
	var out strings.Builder
	FormatStatus(&out, []AppStatus{{
		App:         testApp("blog", StateEntry{Kind: KindFiles, Path: "uploads"}),
		Latest:      &Snapshot{ShortID: "bbb", Time: statusNow.Add(-10 * time.Minute)},
		Snapshots:   40,
		LastSuccess: statusNow.Add(-26 * time.Hour),
	}}, statusNow)
	if !strings.Contains(out.String(), "last clean run") {
		t.Errorf("the report must show that the runs since have failed:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "⚠") {
		t.Errorf("and mark the app as needing attention:\n%s", out.String())
	}
}

func TestFormatStatusReport(t *testing.T) {
	statuses := []AppStatus{
		{
			App:       testApp("blog", StateEntry{Kind: KindSQLite, Path: "app.db"}, StateEntry{Kind: KindFiles, Path: "uploads"}),
			Latest:    &Snapshot{ShortID: "bbb", Time: statusNow.Add(-12 * time.Minute)},
			Snapshots: 24,
		},
		{App: testApp("wiki", StateEntry{Kind: KindSQLite, Path: "wiki.db"})},
	}
	var out strings.Builder
	FormatStatus(&out, statuses, statusNow)
	text := out.String()
	for _, want := range []string{
		"blog", "1 database, 1 path", "24", "12 min ago", "bbb",
		// Never backed up is not "look at the last run": there was none.
		"wiki", "never",
		"sudo hotserve backup run wiki",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("report missing %q:\n%s", want, text)
		}
	}
}

// A box where nothing declares state should say what to do, not print
// an empty table.
func TestFormatStatusWithNothingDeclared(t *testing.T) {
	var out strings.Builder
	FormatStatus(&out, nil, statusNow)
	if !strings.Contains(out.String(), "state sqlite") {
		t.Errorf("want the report to say how to declare state:\n%s", out.String())
	}
}

func TestStatusSurfacesResticFailures(t *testing.T) {
	capture, _ := capturing(nil, errors.New("Fatal: unable to open repository"))
	_, err := Status(context.Background(), []App{testApp("blog")}, capture, "box-1")
	if err == nil || !strings.Contains(err.Error(), "reading snapshots") {
		t.Fatalf("want the restic failure surfaced, got %v", err)
	}
}

func TestHumanAge(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "just now"},
		{12 * time.Minute, "12 min ago"},
		{3 * time.Hour, "3 hours ago"},
		{50 * time.Hour, "2 days ago"},
	} {
		if got := humanAge(tc.d); got != tc.want {
			t.Errorf("humanAge(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
