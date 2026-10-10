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
	t.Run("recovery, still reloading after the wait", func(t *testing.T) {
		b := newTestBox(t)
		v2 := boxFile(2, b.alice)
		head := b.repo.commit(v2, &b.alice, b.base)
		b.writeInstalled(v2)
		id := randomID(t)
		rec := record{ID: id, Origin: originApplier, Phase: phaseSwapped, Commit: head, Path: testPath, Signer: "alice@example.com",
			Prev: b.v1, PrevSHA256: digest(b.v1), NewSHA256: digest(v2)}
		b.lyingRecord(rec)
		b.sd.states = []string{"reloading"}
		if err := b.run(hooks{}); err != nil {
			t.Fatal(err)
		}
		if r := b.record(); r == nil || r.Phase != phaseRollingBack {
			t.Fatalf("the record should stay for the next run: %+v", r)
		}
		if b.result(id) != nil || !b.errorLogged("left for the next run") {
			t.Error("a result was written, or nothing said why")
		}
		b.sd.states = []string{"active"}
		if err := b.run(hooks{}); err != nil {
			t.Fatal(err)
		}
		b.settled()
		if r := b.result(id); r == nil || r.Phase != phaseRolledBack {
			t.Fatalf("%+v", r)
		}
	})
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

// Ids dated past the clock hold none of the 32 slots.
func TestRetentionFutureIDsHoldNoSlots(t *testing.T) {
	b := newTestBox(t)
	now := b.clock.Now()
	var real []string
	for i := 0; i < keepIDs; i++ {
		id := randomID(t)
		b.resultAt(id, phaseApplied, now.Add(time.Hour))
		b.markerAt(id, now.Add(time.Hour))
	}
	for i := 0; i < 3; i++ {
		id := randomID(t)
		real = append(real, id)
		b.resultAt(id, phaseApplied, now.Add(-time.Minute))
		b.markerAt(id, now.Add(-time.Minute))
	}
	if err := b.run(hooks{}); err != nil {
		t.Fatal(err)
	}
	for _, id := range real {
		if b.result(id) == nil {
			t.Error("a real result was evicted by ids from the future")
		}
	}
	if n := len(b.names(b.x("out"))); n != len(real) {
		t.Errorf("%d results kept", n)
	}
}
