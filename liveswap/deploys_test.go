package liveswap

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// A record is written for every deploy — a failure with its detail, a
// success — as the filter left it, and read back whole; the status
// lists each version's outcome newest first.
func TestDeployRecordsAreWrittenAndListed(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.envFile = filepath.Join(t.TempDir(), "app.env")
	must(t, os.WriteFile(rig.spec.envFile, []byte("SECRET=hunter2hunter2hunter2\n"), 0o600))
	rig.runner.runOnceErr = runOnceExit("exit status 3", "u", "failed")
	rig.spec.preStart = []string{"./migrate"}
	rig.spec.deployLogLines = 40
	rig.ma.journal = &fakeJournal{lines: []string{"migrate: SECRET=hunter2hunter2hunter2 refused"}}
	if err := deployOnceV1(t, rig); err == nil {
		t.Fatal("v1 should have failed in pre_start")
	}
	rec, err := readDeployRecord(rig.spec.dirs.deploys, "v1")
	if err != nil {
		t.Fatalf("no record for the failed v1: %v", err)
	}
	var got deployResult
	must(t, json.Unmarshal(rec, &got))
	if got.Version != "v1" || got.Status != "failed" || got.Phase != "preparing" || got.Detail == nil || got.Detail.Exit != "exit status 3" {
		t.Fatalf("record = %+v", got)
	}
	if s := string(rec); strings.Contains(s, "hunter2") || !strings.Contains(s, "[redacted:SECRET]") || !strings.Contains(s, `"redacted_env":["SECRET"]`) {
		t.Fatalf("the record must be the filtered result: %s", s)
	}

	rig.runner.runOnceErr = nil
	rig.clock.Advance(time.Minute)
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://example.test/v2.tgz", version: "v2", by: "test"}); err != nil {
		t.Fatal(err)
	}
	st := rig.ma.status()
	if len(st.Deploys) != 2 || st.Deploys[0].Version != "v2" || st.Deploys[0].Status != "succeeded" || st.Deploys[1].Version != "v1" || st.Deploys[1].Status != "failed" || st.Deploys[1].Phase != "preparing" {
		t.Fatalf("status.deploys = %+v", st.Deploys)
	}
	if _, err := readDeployRecord(rig.spec.dirs.deploys, "v9"); !errors.Is(err, errNoDeployRecord) {
		t.Fatalf("a version never deployed: %v", err)
	}
}

// Records are pruned with the releases: every version still on disk
// keeps its record, and the newest keep others survive.
func TestDeployRecordsArePrunedWithReleases(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.keep = 2
	fail := func(v string) {
		rig.runner.startErr = errors.New("boom")
		rig.clock.Advance(time.Minute)
		if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://example.test/" + v, version: v, by: "test"}); err == nil {
			t.Fatalf("%s should have failed", v)
		}
		rig.runner.startErr = nil
	}
	ok := func(v string) {
		rig.clock.Advance(time.Minute)
		if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://example.test/" + v, version: v, by: "test"}); err != nil {
			t.Fatalf("%s: %v", v, err)
		}
	}
	ok("v1")
	fail("f1")
	fail("f2")
	fail("f3")
	ok("v2")
	ok("v3") // release GC keeps 2: v3, v2 (v1 pruned)
	var names []string
	for _, d := range rig.ma.status().Deploys {
		names = append(names, d.Version)
	}
	// On disk: v3, v2 → kept. Others by the time their record was
	// written: f3, f2, f1, v1 → the newest 2 (f3, f2) kept; v1's
	// record went with its release.
	if got := strings.Join(names, ","); got != "v3,v2,f3,f2" {
		t.Fatalf("records after pruning = %s", got)
	}
}

func TestPruneDeployRecordsKeepsOnDiskAndNewest(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-time.Hour)
	for i, v := range []string{"a", "b", "c", "d"} {
		must(t, writeDeployRecord(dir, v, []byte(`{"version":"`+v+`"}`)))
		must(t, os.Chtimes(deployRecordPath(dir, v), base.Add(time.Duration(i)*time.Minute), base.Add(time.Duration(i)*time.Minute)))
	}
	pruneDeployRecords(dir, 1, []string{"a"}, zap.NewNop())
	entries, _ := os.ReadDir(dir)
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if got := strings.Join(left, ","); got != "a.json,d.json" {
		t.Fatalf("left = %s", got)
	}
}

// The records live beside proxy/, outside every instance's view.
func TestDeployRecordsAreOutsideTheSandboxView(t *testing.T) {
	spec := testSpec(t)
	sb := spec.sandboxSpecFor(spec.dirs.release("v1"), recordedNonce)
	if sb.inView(spec.dirs.deploys) || sb.inView(deployRecordPath(spec.dirs.deploys, "v1")) {
		t.Fatal("the deploys dir must not be inside the sandbox view")
	}
}

// A filter that replaced a timestamp-shaped value leaves the record in
// the list: the times are carried raw, and the order is the write time.
func TestDeploySummariesSurviveARedactedTimestamp(t *testing.T) {
	dir := t.TempDir()
	must(t, writeDeployRecord(dir, "v1", []byte(`{"version":"v1","status":"succeeded","started_at":"[redacted:STAMP]","finished_at":"[redacted:STAMP]"}`)))
	got := listDeploySummaries(dir)
	if len(got) != 1 || got[0].Version != "v1" || string(got[0].FinishedAt) != `"[redacted:STAMP]"` {
		t.Fatalf("summaries = %+v", got)
	}
}
