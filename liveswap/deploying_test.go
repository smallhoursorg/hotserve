package liveswap

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// A deploy that a hotserve stop cut short (#318): what it left is a
// marker naming its version and a release under that name. These are
// what the dead deploy would have left on disk.
func leaveInterruptedDeploy(t *testing.T, rig *testRig, version string, startedAt time.Time) {
	t.Helper()
	must(t, rig.spec.dirs.ensure())
	must(t, os.MkdirAll(rig.spec.dirs.release(version), 0o755))
	must(t, saveDeployMarker(rig.spec.dirs, deployMarker{Version: version, StartedAt: startedAt}))
}

func markerPresent(t *testing.T, rig *testRig) bool {
	t.Helper()
	_, err := os.Lstat(rig.spec.dirs.deploying)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

func releasePresent(t *testing.T, rig *testRig, version string) bool {
	t.Helper()
	_, err := os.Stat(rig.spec.dirs.release(version))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

// recordOf is the version's deploy record as written.
func recordOf(t *testing.T, rig *testRig, version string) deployResult {
	t.Helper()
	b, err := readDeployRecord(rig.spec.dirs, version)
	if err != nil {
		t.Fatalf("record of %s: %v", version, err)
	}
	var r deployResult
	must(t, json.Unmarshal(b, &r))
	return r
}

// interruptedAt is a start time no deploy in a rig has: the fake clock
// stands at its epoch unless a test sleeps it.
var interruptedAt = time.Unix(1_600_000_000, 0).UTC()

func TestDeployHoldsAMarkerWhileItRunsAndRemovesItAfter(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome error
	}{{"succeeds", nil}, {"fails", errTest}} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTestRig(t)
			held, release := rig.holdFetch(t)
			done := make(chan error, 1)
			go func() { done <- rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}) }()
			held()
			m, err := loadDeployMarker(rig.spec.dirs)
			if err != nil || m.Version != "v1" || !m.StartedAt.Equal(rig.clock.Now()) {
				t.Fatalf("a deploy in fetch must hold a marker naming it and its start, got %+v, %v", m, err)
			}
			release(tc.outcome)
			if err := <-done; (err == nil) != (tc.outcome == nil) {
				t.Fatalf("deploy: %v", err)
			}
			if markerPresent(t, rig) {
				t.Fatal("a deploy that returned must leave no marker, whatever it came to")
			}
		})
	}
}

func TestRollbackWritesNoMarker(t *testing.T) {
	// A rollback launches a release already on disk: no marker, since
	// nothing it does may ever lead to that release being removed.
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"}))
	held, release := rig.holdFetch(t)
	done := make(chan error, 1)
	go func() { done <- rig.ma.Deploy(context.Background(), deployRequest{version: "v1", rollback: true}) }()
	held()
	present := markerPresent(t, rig)
	release(nil)
	must(t, <-done)
	if present {
		t.Fatal("a rollback must write no marker")
	}
}

func TestPreStartCarriesTheDeadlineAndTheInstanceDoesNot(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.preStart = []string{"migrate"}
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
	if len(rig.runner.started) != 2 {
		t.Fatalf("want the pre_start and the instance, got %d", len(rig.runner.started))
	}
	if got := rig.runner.started[0].startTimeout; got != rig.spec.deadline || got <= 0 {
		t.Fatalf("the pre_start unit must carry deadline (%s) as its own bound, got %s", rig.spec.deadline, got)
	}
	if got := rig.runner.started[1].startTimeout; got != 0 {
		t.Fatalf("the instance carries no start timeout, got %s", got)
	}
}

func TestRecoveryRemovesTheReleaseOfADeployNoHotserveFinished(t *testing.T) {
	rig := newTestRig(t)
	rig.store.state = appState{CurrentVersion: "v1", Nonce: recordedNonce, Handle: handleState{PID: 1}}
	rig.store.ok = true
	must(t, os.MkdirAll(rig.spec.dirs.release("v1"), 0o755))
	leaveInterruptedDeploy(t, rig, "v2", interruptedAt)

	must(t, rig.ma.ensureRunning())
	if releasePresent(t, rig, "v2") {
		t.Fatal("the release of the deploy hotserve did not finish must be removed")
	}
	if !releasePresent(t, rig, "v1") {
		t.Fatal("the serving release must stay")
	}
	if markerPresent(t, rig) {
		t.Fatal("a settled marker must be removed")
	}
	rec := recordOf(t, rig, "v2")
	if rec.Status != "failed" || !strings.Contains(rec.Error, "hotserve") || !rec.StartedAt.Equal(interruptedAt) {
		t.Fatalf("the cut-short deploy must be recorded failed, naming why, at its own start: %+v", rec)
	}
	if last := rig.ma.status().LastDeploy; last == nil || last.Version != "v2" || last.Status != "failed" {
		t.Fatalf("last_deploy must say what became of the deploy: %+v", last)
	}

	// The version is deployable again: no "already exists".
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"}))
	if rec := recordOf(t, rig, "v2"); rec.Status != "succeeded" {
		t.Fatalf("the redeploy's record replaces the failed one: %+v", rec)
	}
}

func TestAMarkerForTheServingVersionIsOnlyCleared(t *testing.T) {
	// A deploy that reached the cutover before hotserve stopped: its
	// version is the one serving, and is never removed.
	rig := newTestRig(t)
	rig.store.state = appState{CurrentVersion: "v2", Nonce: recordedNonce, Handle: handleState{PID: 1}}
	rig.store.ok = true
	leaveInterruptedDeploy(t, rig, "v2", interruptedAt)
	must(t, rig.ma.ensureRunning())
	if !releasePresent(t, rig, "v2") {
		t.Fatal("the serving release must never be removed")
	}
	if markerPresent(t, rig) {
		t.Fatal("the marker must be cleared")
	}
	if _, err := readDeployRecord(rig.spec.dirs, "v2"); !errors.Is(err, errNoDeployRecord) {
		t.Fatalf("nothing failed: no record may be written, got %v", err)
	}
}

func TestAStaleMarkerOfARecordedDeployIsOnlyCleared(t *testing.T) {
	// A deploy that was recorded and whose marker removal then failed:
	// the record carries the marker's start, and the release is a real
	// rollback target.
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"}))
	must(t, rig.ma.Deploy(context.Background(), deployRequest{version: "v1", rollback: true}))
	must(t, saveDeployMarker(rig.spec.dirs, deployMarker{Version: "v2", StartedAt: recordOf(t, rig, "v2").StartedAt}))
	must(t, rig.ma.ensureRunning())
	if !releasePresent(t, rig, "v2") {
		t.Fatal("a recorded release must not be removed for a stale marker")
	}
	if markerPresent(t, rig) {
		t.Fatal("the stale marker must be cleared")
	}
	if rec := recordOf(t, rig, "v2"); rec.Status != "succeeded" {
		t.Fatalf("the record must be left as it was: %+v", rec)
	}
}

func TestAnUnconfirmedSweepRemovesNothingAMarkerNames(t *testing.T) {
	// The dead deploy's pre_start, or its instance, may still run out
	// of the release: nothing is removed until a sweep confirms.
	rig := newTestRig(t)
	leaveInterruptedDeploy(t, rig, "v2", interruptedAt)
	rig.runner.sweepErr = &migrationRunningError{unit: "hotserve-demo.v2.5555555555555555.prestart.service", version: "v2"}
	err := rig.ma.ensureRunning()
	if !transientRecovery(err) || !strings.Contains(err.Error(), "pre_start of v2") {
		t.Fatalf("recovery must retry, naming the migration it waits for, got %v", err)
	}
	if !releasePresent(t, rig, "v2") || !markerPresent(t, rig) {
		t.Fatal("nothing may be removed while the migration may run")
	}

	// A deploy meanwhile is refused, naming the migration, and leaves
	// both too.
	err = rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/3", version: "v3"})
	if !errors.Is(err, errSweepUnconfirmed) || !strings.Contains(err.Error(), "pre_start of v2") {
		t.Fatalf("a deploy beside the migration must be refused, naming it, got %v", err)
	}
	if !releasePresent(t, rig, "v2") || !markerPresent(t, rig) || releasePresent(t, rig, "v3") {
		t.Fatal("a refused deploy must change nothing on disk")
	}

	// Once the migration has ended, the next attempt settles it.
	rig.runner.sweepErr = nil
	must(t, rig.ma.ensureRunning())
	if releasePresent(t, rig, "v2") || markerPresent(t, rig) {
		t.Fatal("a confirmed sweep must settle the marker")
	}
}

func TestADeploySettlesAMarkerFirst(t *testing.T) {
	// No restart needed for the next deploy to clear the way: one of
	// the orphan's own version proceeds rather than 422.
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
	leaveInterruptedDeploy(t, rig, "v2", interruptedAt)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"}))
	if rec := recordOf(t, rig, "v2"); rec.Status != "succeeded" {
		t.Fatalf("the redeploy must succeed and be recorded: %+v", rec)
	}
	if markerPresent(t, rig) {
		t.Fatal("no marker may outlive the deploy")
	}
}

func TestAMarkerThatIsNotOneIsRemovedAndActedOnForNothing(t *testing.T) {
	for name, body := range map[string]string{
		"not json":      "{",
		"not a version": `{"version":"../v1","started_at":"2026-10-19T00:00:00Z"}`,
		"no version":    `{"started_at":"2026-10-19T00:00:00Z"}`,
	} {
		t.Run(name, func(t *testing.T) {
			rig := newTestRig(t)
			must(t, rig.spec.dirs.ensure())
			must(t, os.MkdirAll(rig.spec.dirs.release("v1"), 0o755))
			must(t, os.WriteFile(rig.spec.dirs.deploying, []byte(body), 0o600))
			must(t, rig.ma.ensureRunning())
			if markerPresent(t, rig) {
				t.Fatal("a marker that is not one must be removed")
			}
			if !releasePresent(t, rig, "v1") {
				t.Fatal("and nothing removed for it")
			}
		})
	}
}
