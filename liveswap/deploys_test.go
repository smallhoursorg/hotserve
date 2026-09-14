package liveswap

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
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
	must(t, os.WriteFile(filepath.Join(dir, ".record-123.tmp"), []byte("{"), 0o600)) // a write that never got its rename
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

// A releases directory that cannot be listed is no reason to prune:
// every record stays, rather than an on-disk release losing its.
func TestDeployRecordsAreNotPrunedWhenReleasesAreUnreadable(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.keep = 1
	for _, v := range []string{"a", "b", "c"} {
		must(t, writeDeployRecord(rig.spec.dirs.deploys, v, []byte(`{"version":"`+v+`","status":"failed"}`)))
	}
	must(t, os.RemoveAll(rig.spec.dirs.releases))
	must(t, os.WriteFile(rig.spec.dirs.releases, []byte("not a dir"), 0o600)) // ReadDir fails
	rig.ma.recordDeploy(rig.ma.snapshot(), deployResult{Version: "d", Status: "failed"})
	if got := len(listDeploySummaries(rig.spec.dirs.deploys)); got != 4 {
		t.Fatalf("records after an unreadable releases dir = %d, want all 4 kept", got)
	}
}

// A refusal before any phase — re-posting the running version, or a
// version already on disk — writes no record: the record of the deploy
// that put the version there stays as it was.
func TestDeployRecordSurvivesARefusedRepost(t *testing.T) {
	rig := newTestRig(t)
	if err := deployOnceV1(t, rig); err != nil {
		t.Fatal(err)
	}
	before, err := readDeployRecord(rig.spec.dirs.deploys, "v1")
	must(t, err)
	err = deployOnceV1(t, rig)
	var ve validationError
	if !errors.As(err, &ve) {
		t.Fatalf("re-posting the running version should be refused: %v", err)
	}
	after, err := readDeployRecord(rig.spec.dirs.deploys, "v1")
	must(t, err)
	if string(after) != string(before) || !strings.Contains(string(after), `"status":"succeeded"`) {
		t.Fatalf("the refusal replaced v1's record:\n%s\n%s", before, after)
	}
	if d := rig.ma.status().Deploys; len(d) != 1 || d[0].Status != "succeeded" {
		t.Fatalf("deploys = %+v", d)
	}
}

// A failure before the first phase that is not a refusal — the box's
// own, here a releases path that is not a directory — is the version's
// history and is recorded like any other.
func TestDeployRecordKeepsAPrePhaseFailure(t *testing.T) {
	rig := newTestRig(t)
	must(t, os.MkdirAll(rig.spec.dirs.app, 0o750))
	must(t, os.RemoveAll(rig.spec.dirs.releases))
	must(t, os.WriteFile(rig.spec.dirs.releases, []byte("not a dir"), 0o600))
	err := deployOnceV1(t, rig)
	var ve validationError
	if err == nil || errors.As(err, &ve) {
		t.Fatalf("want an operational failure, got %v", err)
	}
	rec, rerr := readDeployRecord(rig.spec.dirs.deploys, "v1")
	if rerr != nil || !strings.Contains(string(rec), `"status":"failed"`) {
		t.Fatalf("a pre-phase operational failure must be recorded: %v %s", rerr, rec)
	}
}

// A link planted where the records directory should be — by an app
// that could write its app dir before sandboxing existed — is not
// followed: nothing is written through it, read through it, or listed
// from it.
func TestDeployRecordsRefuseAPlantedDirectoryLink(t *testing.T) {
	rig := newTestRig(t)
	must(t, os.MkdirAll(rig.spec.dirs.app, 0o750))
	must(t, os.Symlink("..", rig.spec.dirs.deploys)) // deploys -> the app dir: version "state" would be state.json
	rig.ma.recordDeploy(rig.ma.snapshot(), deployResult{Version: "state", Status: "failed"})
	if _, err := os.Lstat(filepath.Join(rig.spec.dirs.app, "state.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a record was written through the planted link: %v", err)
	}
	if _, err := readDeployRecord(rig.spec.dirs.deploys, "state"); err == nil || errors.Is(err, errNoDeployRecord) {
		t.Fatalf("a read through the planted link must be refused outright, got %v", err)
	}
	if got := listDeploySummaries(rig.spec.dirs.deploys); got != nil {
		t.Fatalf("listed through the planted link: %+v", got)
	}
}

// A record name that is a link is neither served nor summarised, and a
// record whose version is not the name it sits under is not listed.
func TestDeployRecordsDoNotFollowARecordLinkOrTrustAForgedVersion(t *testing.T) {
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "other.txt"), []byte(`{"version":"v1","status":"succeeded"}`), 0o600))
	must(t, os.Symlink("other.txt", deployRecordPath(dir, "v1")))
	if _, err := readDeployRecord(dir, "v1"); err == nil || errors.Is(err, errNoDeployRecord) {
		t.Fatalf("a linked record must be refused outright, got %v", err)
	}
	must(t, writeDeployRecord(dir, "v2", []byte(`{"version":"AKIAFORGEDSECRETVALUE","status":"succeeded"}`)))
	if got := listDeploySummaries(dir); len(got) != 0 {
		t.Fatalf("a linked record or a forged version reached the list: %+v", got)
	}
}

// A planted temp name is never written through: the temp file is
// created fresh under a random name, and the leftover link is pruned.
func TestDeployRecordWriteNeverUsesAPlantedTempName(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "victim.json")
	must(t, os.WriteFile(target, []byte("precious"), 0o600))
	must(t, os.Symlink("victim.json", filepath.Join(dir, "v1.json.tmp")))
	must(t, writeDeployRecord(dir, "v1", []byte(`{"version":"v1","status":"failed"}`)))
	if b, _ := os.ReadFile(target); string(b) != "precious" {
		t.Fatalf("the planted temp link's target was rewritten: %q", b)
	}
	pruneDeployRecords(dir, 5, nil, zap.NewNop())
	if _, err := os.Lstat(filepath.Join(dir, "v1.json.tmp")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the planted temp link was not pruned: %v", err)
	}
}
