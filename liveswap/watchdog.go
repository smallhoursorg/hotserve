package liveswap

import (
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// The watchdog is the continuous half of supervision: deploys and boot
// recovery start instances, the watchdog keeps them running. One
// goroutine per pooled app watches the current instance and restarts
// the same version when the process exits or when the health endpoint
// fails wdFailures consecutive probes — with a per-restart grace
// period, exponential backoff, and a restart budget. The watchdog
// never gives up: when the budget for the current window is spent it
// throttles — waits for the oldest restart to slide out of the window,
// then tries again — so an app that died during a transient incident
// (a DDoS, a dependency outage) comes back on its own once the
// incident ends, with nobody at the keyboard. The budget is what
// keeps that persistence safe: it bounds the restart rate, so a crash
// loop (or an attacker who can induce health failures) costs at most
// wdRestarts restarts per wdWindow, not an unbounded kill loop.

// Watchdog states reported via the status endpoint.
const (
	wdStateDisabled  = "disabled"
	wdStateIdle      = "idle"
	wdStateGrace     = "grace"
	wdStateWatching  = "watching"
	wdStateBackoff   = "backoff"
	wdStateThrottled = "throttled"
)

// Backoff policy between watchdog restarts, deliberately not
// configurable: 1s doubling to a 60s cap, with a ±20% jitter factor so
// several apps sharing a failed dependency do not restart in lockstep.
// The step resets only after the app has been continuously healthy for
// max(wdBackoffResetAfterMin, watchdog_grace) — a single good probe is
// not evidence the storm is over.
const (
	wdBackoffFloor         = time.Second
	wdBackoffCap           = time.Minute
	wdBackoffResetAfterMin = 30 * time.Second
)

// failureKind distinguishes the two restart triggers in logs and
// status; both consume the same budget.
type failureKind int

const (
	failureCrash failureKind = iota
	failureHealth
)

func (k failureKind) String() string {
	if k == failureCrash {
		return "crash"
	}
	return "health"
}

// watchdogState is the loop's bookkeeping, behind its own mutex so the
// status endpoint can read it without touching the loop.
type watchdogState struct {
	mu               sync.Mutex
	state            string
	failures         int         // consecutive failed probes on the current instance
	restarts         []time.Time // restart times inside the sliding budget window
	backoffStep      int
	healthySince     time.Time
	lastRestartAt    time.Time
	lastRestartCause string
	lastFailure      string
	jitter           func() float64 // test seam; nil = rand.Float64
}

// watchdogSnapshot is the status-endpoint view.
type watchdogSnapshot struct {
	State            string    `json:"state"`
	Failures         int       `json:"consecutive_failures,omitempty"`
	RestartsInWindow int       `json:"restarts_in_window,omitempty"`
	LastRestartAt    time.Time `json:"last_restart_at,omitzero"`
	LastRestartCause string    `json:"last_restart_cause,omitempty"`
	LastFailure      string    `json:"last_failure,omitempty"`
}

func (w *watchdogState) setState(s string) {
	w.mu.Lock()
	w.state = s
	w.mu.Unlock()
}

// arm resets the per-instance counters for a freshly (re)started
// instance. The backoff step is deliberately NOT reset here — it decays
// only through sustained health or a successful deploy.
func (w *watchdogState) arm() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failures = 0
	w.healthySince = time.Time{}
	w.state = wdStateGrace
}

func (w *watchdogState) recordHealthy(now time.Time, resetAfter time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failures = 0
	w.lastFailure = ""
	if w.healthySince.IsZero() {
		w.healthySince = now
	}
	if now.Sub(w.healthySince) >= resetAfter {
		w.backoffStep = 0
	}
}

func (w *watchdogState) recordFailure(desc string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failures++
	w.lastFailure = desc
	w.healthySince = time.Time{}
	return w.failures
}

// noteFlap breaks the continuous-health streak without counting toward
// the failure threshold. Used for probe failures during grace: they
// must not trigger a restart, but they also must not let a pre-flap
// pass keep an old streak alive and reset the backoff.
func (w *watchdogState) noteFlap() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.healthySince = time.Time{}
}

// consumeBudget prunes restarts that have slid out of the window and,
// if there is room left, claims a slot for the restart about to
// happen. Claiming before the restart (not after it succeeds) is what
// bounds a launch-fails-instantly storm.
func (w *watchdogState) consumeBudget(now time.Time, budget int, window time.Duration) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	kept := w.restarts[:0]
	for _, t := range w.restarts {
		if now.Sub(t) < window {
			kept = append(kept, t)
		}
	}
	w.restarts = kept
	if len(w.restarts) >= budget {
		return false
	}
	w.restarts = append(w.restarts, now)
	return true
}

// refundBudget returns the most recently claimed slot. Called when a
// claimed restart is abandoned without a Start ever happening (a
// deploy owns the lifecycle, or replaced the instance): those cycles
// must not drain the budget, or a long deploy over a dead instance
// exhausts it with zero restarts performed. No-op if a deploy's reset
// already cleared the window.
func (w *watchdogState) refundBudget() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if n := len(w.restarts); n > 0 {
		w.restarts = w.restarts[:n-1]
	}
}

// untilBudgetFrees returns how long until the oldest in-window restart
// slides out and a slot opens. Only meaningful right after
// consumeBudget refused (the window is full, so restarts is non-empty).
func (w *watchdogState) untilBudgetFrees(now time.Time, window time.Duration) time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.restarts) == 0 {
		return wdBackoffFloor
	}
	wait := w.restarts[0].Add(window).Sub(now)
	if wait < wdBackoffFloor {
		wait = wdBackoffFloor
	}
	return wait
}

// nextBackoff returns the jittered delay before the next restart and
// advances the step.
func (w *watchdogState) nextBackoff() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	d := wdBackoffFloor
	for i := 0; i < w.backoffStep && d < wdBackoffCap; i++ {
		d *= 2
	}
	if d > wdBackoffCap {
		d = wdBackoffCap
	}
	w.backoffStep++
	r := w.jitter
	if r == nil {
		r = rand.Float64
	}
	return time.Duration(float64(d) * (0.8 + 0.4*r()))
}

func (w *watchdogState) recordRestart(now time.Time, kind failureKind) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastRestartAt = now
	w.lastRestartCause = kind.String()
}

// reset clears counters, budget and backoff. Called on a successful
// deploy: the operator (or CI) shipping a new instance is the fast
// path out of a throttle wait. Restart history for the status endpoint
// is kept.
func (w *watchdogState) reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failures = 0
	w.restarts = nil
	w.backoffStep = 0
	w.healthySince = time.Time{}
	w.lastFailure = ""
}

func (w *watchdogState) statusSnapshot(now time.Time, window time.Duration) *watchdogSnapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state == "" {
		return nil // watchdog never armed (e.g. before first Provision)
	}
	inWindow := 0
	for _, t := range w.restarts {
		if now.Sub(t) < window {
			inWindow++
		}
	}
	return &watchdogSnapshot{
		State:            w.state,
		Failures:         w.failures,
		RestartsInWindow: inWindow,
		LastRestartAt:    w.lastRestartAt,
		LastRestartCause: w.lastRestartCause,
		LastFailure:      w.lastFailure,
	}
}

// watchdogLoop runs until ctx is canceled (Destruct). Each cycle
// re-snapshots the spec, so reloads — including `watchdog off` — take
// effect on the next iteration without touching the goroutine.
func (ma *managedApp) watchdogLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		c := ma.snapshot()
		inst := ma.currentInstance()
		if c.spec == nil || !c.spec.watchdogOn || inst == nil {
			ma.parkWatchdog(ctx, c)
			continue
		}
		if !ma.superviseInstance(ctx, c, inst) {
			return
		}
	}
}

// parkWatchdog blocks while there is nothing to supervise: watchdog
// off, or no instance. A poke (new instance, config nudge) re-enters
// the outer loop.
func (ma *managedApp) parkWatchdog(ctx context.Context, c collaborators) {
	if c.spec != nil && !c.spec.watchdogOn {
		ma.wd.setState(wdStateDisabled)
	} else {
		ma.wd.setState(wdStateIdle)
	}
	select {
	case <-ctx.Done():
	case <-ma.wdNotify:
	}
}

// superviseInstance watches one instance until it is replaced, the
// watchdog acts on a failure, or ctx ends. Returns false only when the
// loop should exit entirely (ctx canceled).
func (ma *managedApp) superviseInstance(ctx context.Context, c collaborators, inst *instance) bool {
	waitCh := c.runner.Wait(inst.handle) // nil ⇒ Alive polling on the tick below
	graceUntil := c.clock.Now().Add(c.spec.wdGrace)
	ma.wd.arm()

	for {
		// Re-snapshot every cycle: a healthy instance can be watched
		// for weeks, and a reload's new spec (watchdog off, changed
		// intervals/thresholds) must apply on the next tick, not on
		// the next restart.
		c = ma.snapshot()
		spec := c.spec
		if spec == nil || !spec.watchdogOn {
			return true // outer loop parks as disabled
		}
		if c.clock.Now().Before(graceUntil) {
			ma.wd.setState(wdStateGrace)
		} else {
			ma.wd.setState(wdStateWatching)
		}
		select {
		case <-ctx.Done():
			return false
		case <-ma.wdNotify:
			if ma.currentInstance() != inst {
				return true // promote/recovery installed a new instance; re-arm on it
			}
			continue // config nudge: re-snapshot without re-arming grace
		case <-waitCh:
			if ma.currentInstance() != inst {
				return true // deploy stopped the old handle; not a crash
			}
			c.logger.Warn("watchdog: process exited unexpectedly",
				zap.String("version", inst.version), zap.Int("pid", inst.handle.state().PID))
			return ma.handleFailure(ctx, c, inst, failureCrash)
		case <-c.clock.After(spec.healthInterval):
			if ma.currentInstance() != inst {
				return true
			}
			if !c.runner.Alive(inst.handle) {
				c.logger.Warn("watchdog: process exited unexpectedly",
					zap.String("version", inst.version), zap.Int("pid", inst.handle.state().PID))
				return ma.handleFailure(ctx, c, inst, failureCrash)
			}
			if spec.healthPath == "" {
				// health_path off: liveness is the whole check.
				ma.wd.recordHealthy(c.clock.Now(), backoffResetAfter(spec))
				continue
			}
			err := c.prober.probeOnce(ctx, "http://127.0.0.1:"+portString(inst.port)+spec.healthPath, spec.healthTimeout)
			if ma.currentInstance() != inst {
				// A deploy replaced the instance while the probe was in
				// flight; its result belongs to the old era and must not
				// touch the freshly reset counters.
				return true
			}
			if err == nil {
				ma.wd.recordHealthy(c.clock.Now(), backoffResetAfter(spec))
				continue
			}
			if c.clock.Now().Before(graceUntil) {
				ma.wd.noteFlap() // don't count, but break the healthy streak
				c.logger.Debug("watchdog: probe failed during grace", zap.Error(err))
				continue
			}
			n := ma.wd.recordFailure(err.Error())
			c.logger.Warn("watchdog: health probe failed",
				zap.String("kind", classifyProbeErr(err)),
				zap.Int("consecutive", n),
				zap.Int("threshold", spec.wdFailures),
				zap.Error(err))
			if n >= spec.wdFailures {
				return ma.handleFailure(ctx, c, inst, failureHealth)
			}
		}
	}
}

// handleFailure is the restart decision: consume budget, back off,
// then — still holding the same one-deploy-at-a-time lock deploys use —
// stop what is left of the instance and relaunch its version. Every
// wake-up re-checks that inst is still current: promote swaps current
// before stopping the old handle, so a stale instance here always means
// "a deploy already replaced it" and the watchdog stands down.
func (ma *managedApp) handleFailure(ctx context.Context, c collaborators, inst *instance, kind failureKind) bool {
	spec := c.spec
	if !c.runner.Alive(inst.handle) {
		// Never route to a dead port while pacing the restart: a fast
		// clean 5xx beats dialing it, and the freed port could be
		// re-bound by another app's deploy in the meantime.
		ma.unrouteIf(inst)
	}
	// The window is a rate limiter, not a give-up point: when it is
	// full, throttle until the oldest restart slides out, then keep
	// going. An app taken down by a transient incident comes back on
	// its own once the incident ends; a deploy (wdNotify) is the
	// fast path out of the wait.
	for !ma.wd.consumeBudget(c.clock.Now(), spec.wdRestarts, spec.wdWindow) {
		wait := ma.wd.untilBudgetFrees(c.clock.Now(), spec.wdWindow)
		ma.wd.setState(wdStateThrottled)
		alive := c.runner.Alive(inst.handle)
		if !alive {
			ma.unrouteIf(inst)
		}
		c.logger.Error("watchdog: restart budget exhausted; throttling before the next restart",
			zap.String("cause", kind.String()),
			zap.Int("budget", spec.wdRestarts),
			zap.Duration("window", spec.wdWindow),
			zap.Duration("wait", wait),
			zap.Bool("process_alive", alive))
		select {
		case <-ctx.Done():
			return false
		case <-ma.wdNotify:
			if ma.currentInstance() != inst {
				return true // a deploy replaced the instance and reset the budget
			}
			if ma.watchdogDisabled() {
				return true // reload turned the watchdog off mid-wait; no slot was claimed
			}
			continue // config nudge: recompute the wait, keep throttling
		case <-c.clock.After(wait):
		}
	}

	d := ma.wd.nextBackoff()
	ma.wd.setState(wdStateBackoff)
	deadline := c.clock.Now().Add(d)
	for {
		wait := deadline.Sub(c.clock.Now())
		if wait <= 0 {
			break
		}
		select {
		case <-ctx.Done():
			ma.wd.refundBudget()
			return false
		case <-ma.wdNotify:
			if ma.currentInstance() != inst {
				return true // deploy replaced the instance; its reset cleared the window anyway
			}
			if ma.watchdogDisabled() {
				ma.wd.refundBudget()
				return true // reload turned the watchdog off mid-wait
			}
			continue // config nudge: keep waiting out the backoff
		case <-c.clock.After(wait):
		}
	}

	if !ma.deployMu.TryLock() {
		// A running deploy owns the lifecycle and will poke wdNotify.
		// Give the claimed slot back: no restart happened, and without
		// the refund a long deploy over a dead instance would drain
		// the whole budget with zero restarts performed.
		ma.wd.refundBudget()
		return true
	}
	defer ma.deployMu.Unlock()
	if ctx.Err() != nil {
		ma.wd.refundBudget()
		return false // Destruct won the race; it waits for this goroutine before stopping the child
	}
	if ma.currentInstance() != inst {
		return true // deploy replaced the instance; its reset cleared the window anyway
	}
	if ma.watchdogDisabled() {
		// A reload during the final wait can land without its poke
		// being consumed (the timer may fire first). Re-check before
		// acting: `watchdog off` must never be followed by one more
		// restart — least of all one that kills a live process.
		ma.wd.refundBudget()
		return true
	}

	if c.runner.Alive(inst.handle) {
		if err := c.runner.Stop(inst.handle, spec.grace); err != nil {
			c.logger.Warn("watchdog: stopping unhealthy instance", zap.Error(err))
		}
	}
	newInst, err := ma.launchVersion(c, inst.version)
	if err != nil {
		// The budget slot stays consumed, so a launch that fails
		// instantly is bounded exactly like any other restart storm;
		// the next cycle sees the dead instance and tries again. The
		// instance is gone either way (crashed, or stopped above), so
		// stop routing to its port.
		ma.unrouteIf(inst)
		c.logger.Error("watchdog: restart failed",
			zap.String("version", inst.version), zap.Error(err))
		return true
	}
	ma.publishInstance(c, newInst)
	ma.wd.recordRestart(c.clock.Now(), kind)
	c.logger.Warn("watchdog: restarted app",
		zap.String("cause", kind.String()),
		zap.String("version", newInst.version),
		zap.Int("port", newInst.port),
		zap.Int("pid", newInst.handle.state().PID),
		zap.Duration("backoff_was", d))
	return true
}

// watchdogDisabled re-reads the live spec: the answer to "may I still
// restart?" must come from the config as it is NOW, not from the
// snapshot taken when the failure was detected — an operator flipping
// `watchdog off` mid-wait means it.
func (ma *managedApp) watchdogDisabled() bool {
	s := ma.snapshot().spec
	return s == nil || !s.watchdogOn
}

func backoffResetAfter(spec *appSpec) time.Duration {
	if spec.wdGrace > wdBackoffResetAfterMin {
		return spec.wdGrace
	}
	return wdBackoffResetAfterMin
}

// classifyProbeErr labels a probe failure for logs. A refused
// connection is strong evidence the process is gone or wedged; a
// timeout is weak evidence (GC pause, load) — both count identically
// toward the threshold, but operators reading logs deserve the
// distinction.
func classifyProbeErr(err error) string {
	var nerr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded),
		errors.As(err, &nerr) && nerr.Timeout():
		return "timeout"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	default:
		return "unhealthy"
	}
}
