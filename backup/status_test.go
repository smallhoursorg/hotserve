package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var statusNow = time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)

func snapshotsJSON(entries ...string) []byte {
	return []byte("[" + strings.Join(entries, ",") + "]")
}

func snap(id, app string, age time.Duration) string {
	return fmt.Sprintf(`{"short_id":%q,"time":%q,"tags":["hotserve","app:%s"],"paths":["/var/lib/hotserve-backup/%s"]}`,
		id, statusNow.Add(-age).Format(time.RFC3339Nano), app, app)
}

func capturing(out []byte, err error) (Capturer, *[]call) {
	calls := &[]call{}
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		*calls = append(*calls, call{name, args})
		return out, err
	}, calls
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
	), nil)

	got, err := Status(context.Background(), apps, capture, t.TempDir())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(*calls) != 1 {
		t.Errorf("want one restic call for every app, got %d", len(*calls))
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
		s := AppStatus{Latest: &Snapshot{Time: statusNow.Add(-tc.age)}}
		if got := s.Stale(statusNow); got != tc.stale {
			t.Errorf("age %v: stale = %v, want %v", tc.age, got, tc.stale)
		}
	}
}

// restic writes a snapshot even when it exits non-zero (unreadable
// sources, exit 3), so "a snapshot exists" is not "the backup
// worked". Freshness comes from the marker a clean run writes, or a
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

// The marker lives on the box; the snapshots are the backup. After
// `backup init --force` onto a fresh repository the old marker is
// still there and still recent, and the report must not call a
// repository that holds nothing for this app current — that is the
// state where an operator believes they have backups and has none.
func TestStaleWhenTheRepositoryHoldsNoSnapshotForTheApp(t *testing.T) {
	switched := AppStatus{LastSuccess: statusNow.Add(-10 * time.Minute)}
	if !switched.Stale(statusNow) {
		t.Error("no snapshot in this repository is stale, whatever the local marker says")
	}
}

// The marker is read from the app's staging dir, which is where the
// job writes it.
func TestStatusReadsTheSuccessMarker(t *testing.T) {
	root := t.TempDir()
	staging := filepath.Join(root, "blog")
	if err := os.MkdirAll(staging, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SuccessMarker(staging), nil, 0o640); err != nil {
		t.Fatal(err)
	}
	capture, _ := capturing(snapshotsJSON(snap("aaa", "blog", time.Hour)), nil)
	got, err := Status(context.Background(), []App{testApp("blog", StateEntry{Kind: KindFiles, Path: "uploads"})}, capture, root)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if got[0].LastSuccess.IsZero() {
		t.Fatal("the marker should have been read")
	}
	if got[0].Stale(time.Now()) {
		t.Error("a marker written just now is not stale")
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
		"wiki", "never",
		"journalctl -u hotserve-backup-wiki",
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
	_, err := Status(context.Background(), []App{testApp("blog")}, capture, t.TempDir())
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
