package liveswap

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// currentState is a test accessor for the loop's reported state.
func (w *watchdogState) currentState() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state
}

func (w *watchdogState) failureCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.failures
}

// startWatchdogT starts the real supervision goroutine on the rig with
// jitter pinned to the nominal backoff, and guarantees teardown so the
// package's goleak gate stays green.
func (rig *testRig) startWatchdogT(t *testing.T) {
	t.Helper()
	rig.ma.wd.jitter = func() float64 { return 0.5 } // 0.8+0.4*0.5 = exactly nominal
	rig.ma.startWatchdog()
	t.Cleanup(rig.ma.stopWatchdog)
}

// pollFor is the one real-time poll loop behind both test builds:
// waitUntil (unit: fake clock, tight cadence) and the integration
// suite's pollUntil (real processes, coarser cadence) delegate here.
func pollFor(t *testing.T, timeout, step time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(step)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

// waitUntil polls (real time) for an observable side effect of the
// watchdog goroutine.
func waitUntil(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	pollFor(t, 5*time.Second, 2*time.Millisecond, desc, cond)
}

// advanceUntil repeatedly advances the fake clock in step increments
// until cond holds. The loop paces itself entirely on the clock, so
// driving it means advancing time, not sleeping.
func advanceUntil(t *testing.T, rig *testRig, step time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		rig.clock.Advance(step)
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

func deployV1(t *testing.T, rig *testRig) {
	t.Helper()
	must(t, rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/1", Version: "v1"}))
}

func TestWatchdogRestartsOnCrash(t *testing.T) {
	rig := newTestRig(t)
	deployV1(t, rig)
	portV1 := rig.ma.activePort.Load()
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})

	rig.runner.handleAt(0).kill() // crash: reaper channel closes

	advanceUntil(t, rig, time.Second, "restart after crash", func() bool {
		return rig.runner.startCount() == 2
	})
	waitUntil(t, "new instance published", func() bool {
		p := rig.ma.activePort.Load()
		return p != 0 && p != portV1
	})
	st, ok, _ := rig.store.load()
	if !ok || st.CurrentVersion != "v1" {
		t.Fatalf("restart must persist state for the same version: %+v", st)
	}
	status := rig.ma.status()
	if status.Watchdog == nil || status.Watchdog.RestartsInWindow != 1 || status.Watchdog.LastRestartCause != "crash" {
		t.Fatalf("watchdog status not recorded: %+v", status.Watchdog)
	}
}

func TestWatchdogHealthFailuresBelowThresholdReset(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.wdGrace = 0
	deployV1(t, rig)
	// Two failures, then a pass: the pass must reset the counter and no
	// restart may happen no matter how long we keep probing (default
	// probeErr stays nil = healthy).
	rig.prober.probeResults = []error{errors.New("boom"), errors.New("boom")}
	rig.startWatchdogT(t)

	advanceUntil(t, rig, rig.spec.healthInterval, "several probes", func() bool {
		return rig.prober.calls() >= 5
	})
	if got := rig.runner.startCount(); got != 1 {
		t.Fatalf("no restart expected below the failure threshold, got %d starts", got)
	}
	if got := rig.ma.wd.failureCount(); got != 0 {
		t.Fatalf("a passing probe must reset the consecutive count, got %d", got)
	}
}

func TestWatchdogRestartsAfterConsecutiveHealthFailures(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.wdGrace = 0
	deployV1(t, rig)
	rig.prober.setProbeErr(errors.New("health check returned 500"))
	rig.startWatchdogT(t)

	advanceUntil(t, rig, rig.spec.healthInterval, "restart after threshold", func() bool {
		return rig.runner.startCount() == 2
	})
	if calls := rig.prober.calls(); calls < rig.spec.wdFailures {
		t.Fatalf("restarted after only %d probes, threshold is %d", calls, rig.spec.wdFailures)
	}
	if rig.runner.stopCount() != 1 {
		t.Fatalf("an alive-but-unhealthy instance must be stopped before relaunch, got %d stops", rig.runner.stopCount())
	}
	status := rig.ma.status()
	if status.Watchdog == nil || status.Watchdog.LastRestartCause != "health" {
		t.Fatalf("restart cause not recorded: %+v", status.Watchdog)
	}
	// Let the replacement be seen healthy so the loop settles.
	rig.prober.setProbeErr(nil)
	advanceUntil(t, rig, rig.spec.healthInterval, "loop settles on replacement", func() bool {
		return rig.ma.wd.currentState() == wdStateWatching
	})
}

func TestWatchdogGraceIgnoresProbeFailures(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.wdGrace = time.Hour
	deployV1(t, rig)
	rig.prober.setProbeErr(errors.New("still booting"))
	rig.startWatchdogT(t)

	advanceUntil(t, rig, rig.spec.healthInterval, "probes during grace", func() bool {
		return rig.prober.calls() >= 3
	})
	if got := rig.runner.startCount(); got != 1 {
		t.Fatalf("probe failures during grace must not restart, got %d starts", got)
	}
	if got := rig.ma.wd.failureCount(); got != 0 {
		t.Fatalf("grace failures must not count, got %d", got)
	}
}

func TestWatchdogCrashDuringGraceStillCounts(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.wdGrace = time.Hour
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool { return rig.ma.wd.currentState() == wdStateGrace })

	rig.runner.handleAt(0).kill()
	advanceUntil(t, rig, time.Second, "restart despite grace", func() bool {
		return rig.runner.startCount() == 2
	})
}

func TestWatchdogThrottlesThenResumesAfterWindow(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.wdRestarts = 2
	rig.spec.wdWindow = 30 * time.Second
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})

	// Kill every instance the watchdog brings up: 2 restarts fit the
	// window, the third failure throttles.
	for kill := 0; kill < 3; kill++ {
		rig.runner.handleAt(rig.runner.startCount() - 1).kill()
		want := kill + 2
		advanceUntil(t, rig, time.Second, "restart or throttle", func() bool {
			return rig.runner.startCount() == want || rig.ma.wd.currentState() == wdStateThrottled
		})
	}
	if got := rig.ma.wd.currentState(); got != wdStateThrottled {
		t.Fatalf("state = %s, want throttled", got)
	}
	if got := rig.runner.startCount(); got != 3 {
		t.Fatalf("window of 2 must pace to exactly 2 restarts before throttling (3 starts total), got %d", got)
	}
	if rig.ma.activePort.Load() != 0 {
		t.Fatal("a dead instance must not stay routed during the throttle wait")
	}
	if s := rig.ma.status(); s.Watchdog.State != wdStateThrottled {
		t.Fatalf("status must report the throttle: %+v", s.Watchdog)
	}

	// Never give up: once the oldest restart slides out of the window,
	// the watchdog restarts on its own — no deploy, no operator.
	advanceUntil(t, rig, 5*time.Second, "auto-resume after the window frees", func() bool {
		return rig.runner.startCount() == 4
	})
	waitUntil(t, "resumed instance routed again", func() bool {
		return rig.ma.activePort.Load() != 0
	})
}

func TestWatchdogDeployCutsThrottleShort(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.wdRestarts = 1 // default 10m window: the wait is long
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})

	rig.runner.handleAt(0).kill()
	advanceUntil(t, rig, time.Second, "the single budgeted restart", func() bool {
		return rig.runner.startCount() == 2
	})
	rig.runner.handleAt(1).kill()
	advanceUntil(t, rig, time.Second, "throttle once the window is full", func() bool {
		return rig.ma.wd.currentState() == wdStateThrottled
	})

	// A deploy is the fast path out of the wait: it resets the budget
	// and installs a fresh instance immediately.
	must(t, rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/2", Version: "v2"}))
	waitUntil(t, "watchdog re-arms on the new instance", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})
	rig.runner.handleAt(rig.runner.startCount() - 1).kill()
	advanceUntil(t, rig, time.Second, "restart works again after deploy", func() bool {
		return rig.ma.status().CurrentVersion == "v2" && rig.ma.status().Running
	})
}

func TestWatchdogIgnoresDeployStoppedOldInstance(t *testing.T) {
	rig := newTestRig(t)
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})

	// The deploy stops the old handle after promote; its Wait channel
	// closing must read as "replaced", never as a crash.
	must(t, rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/2", Version: "v2"}))
	waitUntil(t, "watchdog adopts v2", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})
	advanceUntil(t, rig, rig.spec.healthInterval, "a probe on the new instance", func() bool {
		return rig.prober.calls() >= 1
	})
	if got := rig.runner.startCount(); got != 2 {
		t.Fatalf("old instance's exit must not trigger a restart, got %d starts", got)
	}
	if got := rig.ma.status().CurrentVersion; got != "v2" {
		t.Fatalf("current version = %s, want v2", got)
	}
}

func TestWatchdogYieldsWhileDeployHoldsLock(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.wdRestarts = 100 // keep the yield loop far from exhaustion
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})

	rig.ma.deployMu.Lock()
	rig.runner.handleAt(0).kill()
	// Give the loop several backoff cycles: it must keep yielding, not
	// start anything, while the deploy lock is held.
	for i := 0; i < 5; i++ {
		rig.clock.Advance(2 * time.Second)
		time.Sleep(5 * time.Millisecond)
	}
	if got := rig.runner.startCount(); got != 1 {
		rig.ma.deployMu.Unlock()
		t.Fatalf("watchdog must not restart while a deploy holds the lock, got %d starts", got)
	}
	rig.ma.deployMu.Unlock()

	advanceUntil(t, rig, time.Second, "restart after the lock is released", func() bool {
		return rig.runner.startCount() == 2
	})
	// Every yielded cycle must refund its claimed slot: only the one
	// restart that actually happened may count against the window.
	waitUntil(t, "budget reflects only the real restart", func() bool {
		s := rig.ma.status()
		return s.Watchdog != nil && s.Watchdog.RestartsInWindow == 1
	})
}

func TestWatchdogReloadAppliesWithoutRestart(t *testing.T) {
	rig := newTestRig(t)
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})

	// Simulate a config reload flipping watchdog off: install a new
	// spec (as configure does) and poke. The loop must adopt it while
	// the instance is healthy — not wait for the next restart.
	off := *rig.spec
	off.watchdogOn = false
	rig.ma.specMu.Lock()
	rig.ma.spec = &off
	rig.ma.specMu.Unlock()
	rig.ma.pokeWatchdog()
	waitUntil(t, "watchdog parks disabled after the reload", func() bool {
		return rig.ma.wd.currentState() == wdStateDisabled
	})
	rig.runner.handleAt(0).kill()
	rig.clock.Advance(time.Minute)
	time.Sleep(10 * time.Millisecond)
	if got := rig.runner.startCount(); got != 1 {
		t.Fatalf("watchdog off (via reload) must not restart, got %d starts", got)
	}

	// Reload it back on: the parked loop must wake and resume — and
	// then recover the crashed instance.
	rig.ma.specMu.Lock()
	rig.ma.spec = rig.spec
	rig.ma.specMu.Unlock()
	rig.ma.pokeWatchdog()
	advanceUntil(t, rig, time.Second, "supervision resumes after re-enable", func() bool {
		return rig.runner.startCount() == 2
	})
}

// reloadSpec simulates a config reload: installs a modified copy of
// the rig's spec (as configure does) and pokes the loop.
func reloadSpec(rig *testRig, mutate func(*appSpec)) {
	s := *rig.spec
	mutate(&s)
	rig.ma.specMu.Lock()
	rig.ma.spec = &s
	rig.ma.specMu.Unlock()
	rig.ma.pokeWatchdog()
}

func TestWatchdogOffDuringBackoffCancelsRestart(t *testing.T) {
	rig := newTestRig(t)
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})

	rig.runner.handleAt(0).kill()
	waitUntil(t, "loop enters backoff", func() bool {
		return rig.ma.wd.currentState() == wdStateBackoff
	})
	// Reload to watchdog off while the restart is pending: the pending
	// restart must be abandoned, and its claimed budget slot refunded.
	reloadSpec(rig, func(s *appSpec) { s.watchdogOn = false })
	waitUntil(t, "watchdog parks disabled", func() bool {
		return rig.ma.wd.currentState() == wdStateDisabled
	})
	rig.clock.Advance(5 * time.Minute)
	time.Sleep(10 * time.Millisecond)
	if got := rig.runner.startCount(); got != 1 {
		t.Fatalf("watchdog off mid-backoff must cancel the pending restart, got %d starts", got)
	}
	if s := rig.ma.status(); s.Watchdog.RestartsInWindow != 0 {
		t.Fatalf("abandoned attempt must refund its budget slot: %+v", s.Watchdog)
	}
}

func TestWatchdogOffDuringThrottleCancelsRestart(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.wdRestarts = 1
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})

	rig.runner.handleAt(0).kill()
	advanceUntil(t, rig, time.Second, "the single budgeted restart", func() bool {
		return rig.runner.startCount() == 2
	})
	rig.runner.handleAt(1).kill()
	advanceUntil(t, rig, time.Second, "throttle once the window is full", func() bool {
		return rig.ma.wd.currentState() == wdStateThrottled
	})
	reloadSpec(rig, func(s *appSpec) { s.watchdogOn = false })
	waitUntil(t, "watchdog parks disabled", func() bool {
		return rig.ma.wd.currentState() == wdStateDisabled
	})
	rig.clock.Advance(time.Hour) // far past the throttle window
	time.Sleep(10 * time.Millisecond)
	if got := rig.runner.startCount(); got != 2 {
		t.Fatalf("watchdog off mid-throttle must cancel the pending restart, got %d starts", got)
	}
}

func TestWatchdogUnroutesDeadInstanceDuringBackoff(t *testing.T) {
	rig := newTestRig(t)
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})

	rig.runner.handleAt(0).kill()
	// Without advancing the clock the loop sits in backoff: the dead
	// port must already be unrouted (clean 5xx, and the freed port
	// cannot leak another app's traffic here if reused).
	waitUntil(t, "dead instance unrouted before the restart", func() bool {
		return rig.ma.activePort.Load() == 0
	})
	if got := rig.runner.startCount(); got != 1 {
		t.Fatalf("unroute must happen before any restart, got %d starts", got)
	}
	advanceUntil(t, rig, time.Second, "restart republishes the port", func() bool {
		return rig.ma.activePort.Load() != 0 && rig.runner.startCount() == 2
	})
}

func TestWatchdogUnroutesWhenRelaunchKeepsFailing(t *testing.T) {
	rig := newTestRig(t)
	// Wider than the cumulative backoff (1+2+4+8+16s), or slots slide
	// out before the window fills and the loop never throttles.
	rig.spec.wdWindow = 5 * time.Minute
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})

	rig.runner.setStartErr(errors.New("no such binary"))
	rig.runner.handleAt(0).kill()
	advanceUntil(t, rig, time.Second, "launch failures drain into throttle", func() bool {
		return rig.ma.wd.currentState() == wdStateThrottled
	})
	if rig.ma.activePort.Load() != 0 {
		t.Fatal("port must stay unrouted while relaunches fail")
	}
	// Heal the launcher: the next budgeted attempt must recover and
	// route the app again.
	rig.runner.setStartErr(nil)
	advanceUntil(t, rig, 5*time.Second, "recovery once launches succeed again", func() bool {
		return rig.ma.activePort.Load() != 0 && rig.ma.status().Running
	})
}

func TestWatchdogGraceFlapBreaksHealthyStreak(t *testing.T) {
	w := &watchdogState{jitter: func() float64 { return 0.5 }}
	base := time.Unix(1_700_000_000, 0)
	// Grow the backoff, then: pass, flap (grace failure), pass 31s
	// later. The flap must have broken the streak, so the late pass
	// must NOT reset the backoff off the pre-flap timestamp.
	w.backoffStep = 6 // next nominal backoff = 60s cap
	w.recordHealthy(base, 30*time.Second)
	w.noteFlap()
	w.recordHealthy(base.Add(31*time.Second), 30*time.Second)
	if got := w.nextBackoff(); got != time.Minute {
		t.Fatalf("flap-interrupted streak must not reset backoff, got %v", got)
	}
	// An uninterrupted 31s streak does reset it.
	w.reset()
	w.backoffStep = 6
	w.recordHealthy(base, 30*time.Second)
	w.recordHealthy(base.Add(31*time.Second), 30*time.Second)
	if got := w.nextBackoff(); got != time.Second {
		t.Fatalf("uninterrupted streak must reset backoff to the floor, got %v", got)
	}
}

func TestWatchdogRefundBudget(t *testing.T) {
	w := &watchdogState{}
	base := time.Unix(1_700_000_000, 0)
	if !w.consumeBudget(base, 1, 10*time.Minute) {
		t.Fatal("first claim must fit")
	}
	if w.consumeBudget(base, 1, 10*time.Minute) {
		t.Fatal("window of 1 must refuse a second claim")
	}
	w.refundBudget()
	if !w.consumeBudget(base, 1, 10*time.Minute) {
		t.Fatal("a refunded slot must be claimable again")
	}
	w.reset()
	w.refundBudget() // refund after a deploy's reset must be a safe no-op
	if !w.consumeBudget(base, 1, 10*time.Minute) {
		t.Fatal("budget must be intact after a no-op refund")
	}
}

func TestWatchdogDestructDuringBackoffExitsCleanly(t *testing.T) {
	rig := newTestRig(t)
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})

	rig.runner.handleAt(0).kill()
	waitUntil(t, "loop enters backoff", func() bool {
		return rig.ma.wd.currentState() == wdStateBackoff
	})
	// Destruct while the restart is pending: the goroutine must exit
	// (stopWatchdog waits for it) and no orphan Start may follow.
	must(t, rig.ma.Destruct())
	if got := rig.runner.startCount(); got != 1 {
		t.Fatalf("no restart may happen after Destruct, got %d starts", got)
	}
}

func TestWatchdogOffNeverRestarts(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.watchdogOn = false
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog parks disabled", func() bool {
		return rig.ma.wd.currentState() == wdStateDisabled
	})
	rig.runner.handleAt(0).kill()
	rig.clock.Advance(time.Minute)
	time.Sleep(10 * time.Millisecond)
	if got := rig.runner.startCount(); got != 1 {
		t.Fatalf("watchdog off must never restart, got %d starts", got)
	}
}

func TestWatchdogHealthPathOffIsLivenessOnly(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.healthPath = ""
	rig.spec.wdGrace = 0
	deployV1(t, rig)
	rig.prober.setProbeErr(errors.New("must never be called"))
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		return rig.ma.wd.currentState() == wdStateWatching || rig.ma.wd.currentState() == wdStateGrace
	})

	rig.clock.Advance(rig.spec.healthInterval * 3)
	time.Sleep(10 * time.Millisecond)
	if rig.prober.calls() != 0 {
		t.Fatalf("health_path off must not probe, got %d probes", rig.prober.calls())
	}
	if rig.runner.startCount() != 1 {
		t.Fatal("liveness-only watchdog restarted a healthy process")
	}
	rig.runner.handleAt(0).kill()
	advanceUntil(t, rig, time.Second, "crash restart still works", func() bool {
		return rig.runner.startCount() == 2
	})
}

func TestWatchdogBackoffGrowsAndCaps(t *testing.T) {
	w := &watchdogState{jitter: func() float64 { return 0.5 }} // nominal
	want := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 32 * time.Second, time.Minute, time.Minute,
	}
	for i, exp := range want {
		if got := w.nextBackoff(); got != exp {
			t.Fatalf("backoff step %d = %v, want %v", i, got, exp)
		}
	}
	// Sustained health resets the curve; a single good probe does not.
	base := time.Unix(1_700_000_000, 0)
	w.recordHealthy(base, 30*time.Second)
	if got := w.nextBackoff(); got != time.Minute {
		t.Fatalf("one good probe must not reset backoff, got %v", got)
	}
	w.recordHealthy(base, 30*time.Second)
	w.recordHealthy(base.Add(31*time.Second), 30*time.Second)
	if got := w.nextBackoff(); got != time.Second {
		t.Fatalf("sustained health must reset backoff to the floor, got %v", got)
	}
}

func TestWatchdogBackoffJitterBounds(t *testing.T) {
	for _, r := range []float64{0, 1} {
		w := &watchdogState{jitter: func() float64 { return r }}
		got := w.nextBackoff()
		if got < 800*time.Millisecond || got > 1200*time.Millisecond {
			t.Fatalf("jittered floor backoff %v outside ±20%% band", got)
		}
	}
}

func TestWatchdogBudgetWindowSlides(t *testing.T) {
	w := &watchdogState{}
	base := time.Unix(1_700_000_000, 0)
	if !w.consumeBudget(base, 2, 10*time.Minute) || !w.consumeBudget(base.Add(time.Minute), 2, 10*time.Minute) {
		t.Fatal("first two restarts must fit the budget")
	}
	if w.consumeBudget(base.Add(2*time.Minute), 2, 10*time.Minute) {
		t.Fatal("third restart inside the window must be refused")
	}
	if !w.consumeBudget(base.Add(11*time.Minute), 2, 10*time.Minute) {
		t.Fatal("a restart must be admitted again once the oldest slid out of the window")
	}
}

func TestWatchdogFallsBackToPollingWithoutWaitChannel(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.wdGrace = 0
	// A reattached-style handle has no done channel: Wait returns nil
	// and crash detection must ride the health tick's Alive poll.
	h := &fakeHandle{id: "reattached", alive: true}
	rig.ma.mu.Lock()
	rig.ma.current = &instance{version: "v1", port: 1234, handle: h}
	rig.ma.mu.Unlock()
	rig.ma.activePort.Store(1234)
	must(t, rig.store.save(appState{CurrentVersion: "v1", Port: 1234, Handle: handleState{PID: 1}}))
	must(t, mkdirRelease(rig.spec, "v1"))
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})

	h.kill()
	advanceUntil(t, rig, rig.spec.healthInterval, "poll-detected crash restart", func() bool {
		return rig.runner.startCount() == 1
	})
}

func mkdirRelease(spec *appSpec, version string) error {
	return os.MkdirAll(spec.dirs.release(version), 0o755)
}
