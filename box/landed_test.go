package box

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A write can land — its rename done — and still fail, at the
// directory's fsync. These hooks do the write's visible half themselves
// and then fail, so what is on disk, not the error, decides.
func TestWritesThatLandedThenFailed(t *testing.T) {
	errSync := errors.New("fsync: input/output error")

	t.Run("the swap landed: the post-swap rollback", func(t *testing.T) {
		b := newTestBox(t)
		v2 := boxFile(2, b.alice)
		id := b.push(b.repo.bundleFiles(b.repo.commit(v2, &b.alice, b.base), b.base))
		fail := func(p string) error {
			if p == "caddyfile:new" {
				b.writeInstalled(v2)
				return errSync
			}
			return nil
		}
		if err := b.run(hooks{fail: fail}); err != nil {
			t.Fatal(err)
		}
		b.settled()
		if r := b.result(id); r.Phase != phaseRolledBack || !bytes.Equal(b.installed(), b.v1) {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("the swap failed with a third file on disk: left as found", func(t *testing.T) {
		b := newTestBox(t)
		id := b.push(b.repo.bundleFiles(b.repo.commit(boxFile(2, b.alice), &b.alice, b.base), b.base))
		other := boxFile(9, b.alice)
		fail := func(p string) error {
			if p == "caddyfile:new" {
				b.writeInstalled(other)
				return errSync
			}
			return nil
		}
		if err := b.run(hooks{fail: fail}); err != nil {
			t.Fatal(err)
		}
		b.settled()
		if r := b.result(id); r.Phase != phaseUnknown || r.Error != msgChanged || !bytes.Equal(b.installed(), other) {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("verified landed: removed, so Retention settles the push", func(t *testing.T) {
		b := newTestBox(t)
		id := b.push(b.repo.bundleFiles(b.repo.commit(boxFile(2, b.alice), &b.alice, b.base), b.base))
		fail := func(p string) error {
			if p == "result:verified" {
				if err := os.WriteFile(filepath.Join(b.x("out"), id+".json"), encodeJSON(result{ID: id, Phase: phaseVerified}), 0o640); err != nil {
					t.Fatal(err)
				}
				return errSync
			}
			return nil
		}
		if err := b.run(hooks{fail: fail}); err != nil {
			t.Fatal(err)
		}
		b.settled()
		if r := b.result(id); r == nil || r.Phase != phaseFailed || !strings.Contains(r.Error, "nothing changed") {
			t.Fatalf("a verified with nothing behind it was not settled: %+v", r)
		}
	})
	t.Run("verified landed and failed cannot be written: removed, Retention settles it", func(t *testing.T) {
		b := newTestBox(t)
		id := b.push(b.repo.bundleFiles(b.repo.commit(boxFile(2, b.alice), &b.alice, b.base), b.base))
		fail := func(p string) error {
			switch p {
			case "result:verified":
				if err := os.WriteFile(filepath.Join(b.x("out"), id+".json"), encodeJSON(result{ID: id, Phase: phaseVerified}), 0o640); err != nil {
					t.Fatal(err)
				}
				return errSync
			case "result:failed":
				return errNoSpace
			}
			return nil
		}
		if err := b.run(hooks{fail: fail}); err != nil {
			t.Fatal(err)
		}
		b.settled()
		if r := b.result(id); r != nil {
			t.Fatalf("a verified with nothing behind it stayed: %+v", r)
		}
		b.clock.advance(16 * time.Minute)
		if err := b.run(hooks{}); err != nil {
			t.Fatal(err)
		}
		if r := b.result(id); r == nil || r.Phase != phaseFailed || r.Error != msgNoRecord {
			t.Fatalf("Retention: %+v", r)
		}
	})
	t.Run("take landed: the entry is taken, not failed", func(t *testing.T) {
		b := newTestBox(t)
		id := b.push(b.repo.bundleFiles(b.repo.commit(boxFile(2, b.alice), &b.alice, b.base), b.base))
		fail := func(p string) error {
			if p == "take" {
				if err := os.Rename(filepath.Join(b.x("in"), id+".tar"), filepath.Join(b.x("work"), id+".tar")); err != nil {
					t.Fatal(err)
				}
				return errSync
			}
			return nil
		}
		if err := b.run(hooks{fail: fail}); err != nil {
			t.Fatal(err)
		}
		b.settled()
		if r := b.result(id); r.Phase != phaseApplied {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("a take that fails on what is not a regular file gives no result", func(t *testing.T) {
		b := newTestBox(t)
		id := randomID(t)
		if err := os.Mkdir(filepath.Join(b.x("in"), id+".tar"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := b.run(hooks{fail: failAt(map[string]int{"take": -1})}); err != nil {
			t.Fatal(err)
		}
		b.settled()
		if b.result(id) != nil {
			t.Error("a directory got a result")
		}
	})
}

// A reload in flight is waited out like `activating` (step 16); in
// recovery, one still in flight after the wait leaves the record for
// the next run rather than reporting a stopped hotserve.
func TestReloadingIsWaitedOut(t *testing.T) {
	t.Run("push", func(t *testing.T) {
		b := newTestBox(t)
		b.sd.states = []string{"reloading", "reloading", "active"}
		r := b.pushOne(b.repo.bundleFiles(b.repo.commit(boxFile(2, b.alice), &b.alice, b.base), b.base))
		if r.Phase != phaseApplied {
			t.Fatalf("%+v", r)
		}
	})
	recovery := func(t *testing.T, states []string) (*testBox, string) {
		b := newTestBox(t)
		v2 := boxFile(2, b.alice)
		head := b.repo.commit(v2, &b.alice, b.base)
		b.writeInstalled(v2)
		id := randomID(t)
		b.lyingRecord(record{ID: id, Origin: originApplier, Phase: phaseSwapped, Commit: head, Path: testPath, Signer: "alice@example.com",
			Prev: b.v1, PrevSHA256: digest(b.v1), NewSHA256: digest(v2)})
		b.sd.states = states
		if err := b.run(hooks{}); err != nil {
			t.Fatal(err)
		}
		b.settled()
		if !bytes.Equal(b.installed(), b.v1) {
			t.Error("the previous bytes are not back")
		}
		return b, id
	}
	t.Run("recovery, still reloading after the wait: the reload queues behind it", func(t *testing.T) {
		b, id := recovery(t, []string{"reloading"})
		if r := b.result(id); r == nil || r.Phase != phaseRolledBack || b.sd.reloaded != 1 {
			t.Fatalf("%+v, %d reloads", r, b.sd.reloaded)
		}
	})
	t.Run("recovery, a restart loop still activating after the wait: settled as not running", func(t *testing.T) {
		b, id := recovery(t, []string{"activating"})
		if r := b.result(id); r == nil || r.Phase != phaseFailed || r.Error != msgInterruptedStopped || b.sd.reloaded != 0 {
			t.Fatalf("%+v, %d reloads", r, b.sd.reloaded)
		}
	})
}

// Bundles queued behind a hotserve that never settles share one wait.
func TestOneWaitPerRun(t *testing.T) {
	b := newTestBox(t)
	b.sd.states = []string{"activating"}
	c1 := b.repo.commit(boxFile(2, b.alice), &b.alice, b.base)
	first := b.pushAt(b.repo.bundleFiles(c1, b.base), b.clock.Now().Add(-time.Minute))
	second := b.push(b.repo.bundleFiles(c1, b.base))
	start := b.clock.Now()
	if err := b.run(hooks{}); err != nil {
		t.Fatal(err)
	}
	b.settled()
	for _, id := range []string{first, second} {
		if r := b.result(id); r == nil || r.Error != msgStillStarting {
			t.Errorf("%+v", r)
		}
	}
	if waited := b.clock.Now().Sub(start); waited > activatingWait+activatingPoll {
		t.Errorf("waited %v for two bundles", waited)
	}
}

// init on a hotserve still reloading after the wait reloads (the
// reload queues behind the one in flight), never records `applied`
// for bytes the running hotserve may not have read.
func TestInitOnReloadingHotserveReloads(t *testing.T) {
	b := newTestBox(t)
	b.sd.states = []string{"reloading"}
	v2 := boxFile(2, b.alice)
	sha := b.repo.commit(v2, &b.alice, b.base)
	out, err := b.applier(hooks{}).runInit(context.Background(), record{ID: randomID(t), Commit: sha, Path: testPath, BoxWebhook: "deploy.example.com"}, v2)
	if err != nil {
		t.Fatal(err)
	}
	if out.Phase != phaseApplied || b.sd.reloaded != 1 {
		t.Fatalf("%+v, %d reloads", out, b.sd.reloaded)
	}
}

// init on a box that is not running never reloads, its rollback
// included.
func TestInitRollbackOnStoppedBoxDoesNotReload(t *testing.T) {
	b := newTestBox(t)
	b.sd.states = []string{"inactive"}
	v2 := boxFile(2, b.alice)
	sha := b.repo.commit(v2, &b.alice, b.base)
	a := b.applier(hooks{fail: failAt(map[string]int{"record:swapped": -1})})
	out, err := a.runInit(context.Background(), record{ID: randomID(t), Commit: sha, Path: testPath, BoxWebhook: "deploy.example.com"}, v2)
	if err != nil {
		t.Fatal(err)
	}
	b.settled()
	if out.Phase != phaseFailed || out.Error != msgInterruptedStopped || b.sd.reloaded != 0 || !bytes.Equal(b.installed(), b.v1) {
		t.Fatalf("%+v, %d reloads", out, b.sd.reloaded)
	}
}

func TestRunInitRefusesARecordRecoveryCouldNotRead(t *testing.T) {
	b := newTestBox(t)
	sha := b.repo.commit(b.v1, &b.alice, b.base)
	for _, rec := range []record{
		{ID: "../../etc/x", Commit: sha, Path: testPath},
		{ID: randomID(t), Commit: strings.Repeat("a", 64), Path: testPath},
		{ID: randomID(t), Commit: sha, Path: "../x"},
	} {
		if _, err := b.applier(hooks{}).runInit(context.Background(), rec, b.v1); err == nil {
			t.Errorf("%+v accepted", rec)
		}
	}
}

// A changed line longer than the pre-redaction cut still gets the note.
func TestCaddyfileDiffLongLineNoted(t *testing.T) {
	long := strings.Repeat("x", maxDiffRedacted+10) + "\n"
	d := caddyfileDiff([]byte("a\n"), []byte("a\n"+long))
	if !strings.HasSuffix(d, diffCutNote) || len(d) > maxDiff {
		t.Fatalf("%d bytes: %q", len(d), d[:min(len(d), 80)])
	}
}

// Markers dated past the clock with no result — a hostile writer's —
// hold none of the 32 slots and get no result; a result whose marker is
// from the future (a push settled just before the clock stepped back)
// is aged by root's own write instead, and survives.
func TestRetentionFutureDates(t *testing.T) {
	b := newTestBox(t)
	now := b.clock.Now()
	var hostile, real []string
	for i := 0; i < keepIDs+5; i++ {
		id := randomID(t)
		hostile = append(hostile, id)
		b.markerAt(id, now.Add(time.Hour))
	}
	for i := 0; i < 3; i++ {
		id := randomID(t)
		real = append(real, id)
		b.resultAt(id, phaseApplied, now.Add(-time.Minute))
		b.markerAt(id, now.Add(-time.Minute))
	}
	stepped := randomID(t)
	b.resultAt(stepped, phaseApplied, now.Add(20*time.Minute))
	b.markerAt(stepped, now.Add(20*time.Minute))
	if err := b.run(hooks{}); err != nil {
		t.Fatal(err)
	}
	for _, id := range append(real, stepped) {
		if b.result(id) == nil || !b.hasMarker(id) {
			t.Error("a real result was swept")
		}
	}
	for _, id := range hostile {
		if b.hasMarker(id) || b.result(id) != nil {
			t.Error("a marker from the future was kept or given a result")
		}
	}
}

// Retention leaves the push under the record alone, reading the record
// itself.
func TestSweepReadsTheRecord(t *testing.T) {
	b := newTestBox(t)
	id := randomID(t)
	b.markerAt(id, b.clock.Now().Add(-time.Hour))
	b.lyingRecord(record{ID: id, Origin: originInit, Phase: phaseInstalling, Commit: b.base, Path: testPath, Prev: b.v1, PrevSHA256: digest(b.v1), NewSHA256: digest(b.v1)})
	b.applier(hooks{}).sweep()
	if b.result(id) != nil || !b.hasMarker(id) {
		t.Error("the push holding the record was settled as stranded")
	}
}

// A result a run wrote survives that run's sweep, whatever else is kept:
// here 32 results dated past the clock, as after a clock stepped back,
// which count as written now and so as newer than the push's own.
func TestRetentionKeepsThisRunsResults(t *testing.T) {
	b := newTestBox(t)
	b.clock.advance(time.Minute) // the push's result is written before the fake now
	now := b.clock.Now()
	for i := 0; i < keepIDs; i++ {
		id := randomID(t)
		b.resultAt(id, phaseApplied, now.Add(time.Hour))
		b.markerAt(id, now.Add(time.Hour))
	}
	id := b.push(b.repo.bundleFiles(b.repo.commit(boxFile(2, b.alice), &b.alice, b.base), b.base))
	if err := b.run(hooks{}); err != nil {
		t.Fatal(err)
	}
	if r := b.result(id); r == nil || r.Phase != phaseApplied || !b.hasMarker(id) {
		t.Fatalf("this run's result was swept: %+v", r)
	}
}

// The run has one budget for waiting: once it is spent, a later
// episode in the same run gets no wait, and root's lock is held for
// waits at most activatingWait per run.
func TestWaitBudgetIsPerRun(t *testing.T) {
	b := newTestBox(t)
	b.sd.states = []string{"activating", "active", "activating", "activating", "active"}
	c1 := b.repo.commit(boxFile(2, b.alice), &b.alice, b.base)
	c2 := b.repo.commit(boxFile(3, b.alice), &b.alice, c1)
	first := b.pushAt(b.repo.bundleFiles(c1, b.base), b.clock.Now().Add(-time.Minute))
	second := b.push(b.repo.bundleFiles(c2, c1))
	read := func(id string) {
		if id == second {
			b.clock.advance(2 * activatingWait) // a long verification, say
		}
	}
	if err := b.run(hooks{read: read}); err != nil {
		t.Fatal(err)
	}
	if r := b.result(first); r == nil || r.Phase != phaseApplied {
		t.Errorf("first: %+v", r)
	}
	if r := b.result(second); r == nil || r.Phase != phaseRefused || r.Error != msgStillStarting {
		t.Errorf("second: %+v", r)
	}
}

// A `failed` that landed in place of a failed `verified` is kept.
func TestLandedFailedIsKept(t *testing.T) {
	b := newTestBox(t)
	id := b.push(b.repo.bundleFiles(b.repo.commit(boxFile(2, b.alice), &b.alice, b.base), b.base))
	errSync := errors.New("fsync: input/output error")
	fail := func(p string) error {
		switch p {
		case "result:verified":
			return errNoSpace
		case "result:failed":
			if err := os.WriteFile(filepath.Join(b.x("out"), id+".json"), encodeJSON(result{ID: id, Phase: phaseFailed, Error: "x"}), 0o640); err != nil {
				t.Fatal(err)
			}
			return errSync
		}
		return nil
	}
	if err := b.run(hooks{fail: fail}); err != nil {
		t.Fatal(err)
	}
	if r := b.result(id); r == nil || r.Phase != phaseFailed {
		t.Fatalf("%+v", r)
	}
}
