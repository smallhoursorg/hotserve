//go:build unix

package liveswap

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// execRunner runs app instances as child processes of Caddy. This is
// the v1 default: no privileges, no extra software on the server, and
// it works anywhere Caddy does (including the Docker e2e harness).
// Trade-off, documented in the README: children die when the Caddy
// binary itself restarts (not on config reloads — see the UsagePool in
// liveswap.go); App.Start relaunches the current version on boot.
//
// The logger lives behind an atomic pointer because config reloads
// swap it while the output-piping goroutines of running children are
// still using the old one.
type execRunner struct {
	logger atomic.Pointer[zap.Logger]
}

func newExecRunner(logger *zap.Logger) *execRunner {
	r := new(execRunner)
	r.logger.Store(logger)
	return r
}

func (r *execRunner) setLogger(logger *zap.Logger) { r.logger.Store(logger) }
func (r *execRunner) log() *zap.Logger             { return r.logger.Load() }

// execHandle tracks one spawned process — the *leader* of its process
// group (pgid == pid, see setProcessGroup). The leader's exit is not the
// group's: npm/node workers it spawned keep running (and keep the port)
// after it crashes. Invariants, in the order they matter:
//
//  1. The group is swept exactly once after the leader is gone — by Stop
//     if it holds teardownMu when the leader exits, else by the reaper —
//     and never signalled again once `swept` is set: an exited handle can
//     stay current indefinitely (watchdog off) and the kernel may hand
//     its pgid to an unrelated group in the meantime.
//  2. While the group is swept the leader is left unreaped (Linux:
//     waitid WNOWAIT), so its pid — and therefore the pgid — stays
//     reserved: a signal can only ever reach our own group.
//  3. Alive and Wait (done) describe the leader only; done is closed
//     before the reaper takes teardownMu, and Stop waits on done while
//     holding it, so the two never deadlock.
//  4. Stop returns nil only when the group is confirmed free of live
//     members. A non-nil result means processes of this instance may
//     remain, and every later Stop replays that verdict (sweepErr).
//  5. A sweep the reaper had to perform on its own reports a failure
//     once, via startSpec.onSweepFailure, so the app can record it
//     durably without waiting for a Stop that may never come.
//
// Threat model: apps run as hotserve's own unprivileged user, so group
// members cannot change credentials and every member is signalable and
// visible; EPERM paths are reported as leaks, not defended against.
type execHandle struct {
	pid       int
	startedAt time.Time
	grace     time.Duration // group grace for an unsolicited (crash) sweep
	done      chan struct{}

	teardownMu sync.Mutex
	swept      bool  // the group has been swept; never signal the pgid again
	sweepErr   error // the sweep's verdict, replayed by every later Stop
}

func (h *execHandle) state() handleState {
	return handleState{PID: h.pid, StartedAt: h.startedAt}
}

// Start launches the command in its own process group so Stop — or the
// reaper, if the leader dies first — can signal the whole tree (Node
// apps routinely spawn children).
func (r *execRunner) Start(spec startSpec) (handle, error) {
	cmd := exec.Command(spec.command[0], spec.command[1:]...) //nolint:gosec // running the operator's configured app command is this module's purpose
	cmd.Dir = spec.dir
	cmd.Env = spec.env
	setProcessGroup(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	go r.pipeLines(stdout, "stdout")
	go r.pipeLines(stderr, "stderr")

	h := &execHandle{pid: cmd.Process.Pid, startedAt: time.Now(), grace: spec.grace, done: make(chan struct{})}
	go func() {
		// Observe the exit WITHOUT reaping where the OS allows it
		// (Linux: waitid WNOWAIT). The leader then stays a zombie until
		// after the sweep, and a zombie keeps its pgid reserved — so the
		// kernel cannot hand that pgid to an unrelated process group in
		// the window between "leader gone" and "group signalled". Where
		// that is unavailable the old order (reap first) is the fallback,
		// with the narrow window it implies.
		var exitErr error
		reaped := !awaitExitUnreaped(h.pid)
		if reaped {
			exitErr = cmd.Wait()
		}
		r.log().Info("process exited", zap.Int("pid", h.pid), zap.Error(exitErr))
		close(h.done)
		// A leader that exited on its own (crash, clean exit) leaves its
		// group unsupervised: callers gate Stop behind Alive, so nobody
		// else will ever signal these processes. Sweep them here, now,
		// while the pgid is still ours. If a Stop was in flight it holds
		// the lock until it has swept.
		h.teardownMu.Lock()
		var unsolicited error // the verdict, only if THIS goroutine swept
		if !h.swept {
			h.sweepErr = r.sweepGroup(h.pid, h.grace, "leader exited unsolicited")
			h.swept = true
			unsolicited = h.sweepErr
		}
		h.teardownMu.Unlock()
		if unsolicited != nil {
			r.log().Error("orphaned process group could not be swept; its workers may have leaked",
				zap.Int("pgid", h.pid), zap.Error(unsolicited))
			if spec.onSweepFailure != nil {
				spec.onSweepFailure(unsolicited) // durable record now, not at some future Stop
			}
		}
		if !reaped {
			// Teardown is settled; now release the pid.
			if err := cmd.Wait(); err != nil {
				r.log().Info("process exit status", zap.Int("pid", h.pid), zap.Error(err))
			}
		}
	}()
	return h, nil
}

// RunOnce runs a command to completion, streaming its output to the
// logs. Cancellation (deploy deadline, client gone) kills the whole
// process group, not just the direct child.
func (r *execRunner) RunOnce(ctx context.Context, spec startSpec) error {
	cmd := exec.Command(spec.command[0], spec.command[1:]...) //nolint:gosec // running the operator's configured pre_start command is this module's purpose
	cmd.Dir = spec.dir
	cmd.Env = spec.env
	setProcessGroup(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go r.pipeLines(stdout, "stdout")
	go r.pipeLines(stderr, "stderr")

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = signalGroup(cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return ctx.Err()
	}
}

func (r *execRunner) Alive(h handle) bool {
	eh, ok := h.(*execHandle)
	if !ok {
		return false
	}
	select {
	case <-eh.done:
		return false
	default:
		return true
	}
}

func (r *execRunner) Wait(h handle) <-chan struct{} {
	eh, ok := h.(*execHandle)
	if !ok {
		return nil
	}
	return eh.done
}

func (r *execRunner) Stop(h handle, grace time.Duration) (err error) {
	eh, ok := h.(*execHandle)
	if !ok {
		return fmt.Errorf("not an exec handle")
	}
	eh.teardownMu.Lock()
	defer eh.teardownMu.Unlock()
	if eh.swept {
		// The pgid may belong to someone else by now: never signal it
		// again. But replay the verdict — a caller synchronising before
		// release cleanup must learn that workers were left behind.
		return eh.sweepErr
	}
	select {
	case <-eh.done:
		// Leader just exited and we beat the reaper to the lock: the
		// group is still ours (live members keep the pgid reserved), so
		// sweep it with the caller's grace and let the reaper stand down.
		err = r.sweepGroup(eh.pid, grace, "stop after leader exit")
		eh.swept, eh.sweepErr = true, err
		return err
	default:
	}
	// On Linux the reaper leaves an exited leader unreaped until the
	// sweep is settled, so the pgid stays reserved and this signal can
	// only ever reach our own group. Elsewhere (macOS dev) the child
	// can exit and be reaped between the done-check and this signal,
	// and the kernel could in principle recycle the pgid; that window
	// is narrow and unavoidable without waitid(WNOWAIT).
	if err := signalGroup(eh.pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			// The group emptied between the done check and the signal:
			// not even a zombie is left, so the leader has been reaped
			// and the reaper is about to close done. A clean stop.
			<-eh.done
			eh.swept = true
			return nil
		}
		return err // not swept: the reaper will try, and record its verdict
	}
	// Every path below ends with the group swept: a confirmed-empty
	// group, or SIGKILL with its verdict. Record both before unlocking
	// so the reaper (blocked on the lock if the leader dies during the
	// wait) and any later Stop leave the pgid alone and see the result.
	defer func() { eh.swept, eh.sweepErr = true, err }()
	deadline := time.Now().Add(grace)
	select {
	case <-eh.done:
	case <-time.After(grace):
		r.log().Warn("grace period expired, killing process group", zap.Int("pid", eh.pid))
		if err = killGroup(eh.pid); err != nil {
			// The leader may be among the survivors (uninterruptible
			// sleep, changed credentials): waiting on done here could
			// block forever while holding teardownMu.
			return err
		}
		<-eh.done // group confirmed empty, so the reaper is about to close it
		return nil
	}
	// The leader went quietly; its workers get the rest of the grace
	// before the survivors are killed. `npm start` forwards SIGTERM and
	// outlives node, so this is usually already empty.
	if waitGroupGone(eh.pid, deadline) {
		return nil
	}
	r.log().Warn("grace period expired for process group survivors, killing them", zap.Int("pgid", eh.pid))
	return killGroup(eh.pid)
}

// sweepGroup terminates whatever is left of process group pgid after
// its leader has exited: SIGTERM, up to grace for the members to leave
// on their own, then SIGKILL the survivors. Nothing to do is the common
// case and stays silent. A non-nil error means live members remain
// that we could not kill (EPERM — outside the threat model, see
// execHandle — or a member stuck in uninterruptible sleep); callers
// still record the group as swept, because retrying later is futile
// and, once the pgid is recycled, dangerous — but they must not
// pretend it was clean.
func (r *execRunner) sweepGroup(pgid int, grace time.Duration, why string) error {
	if !groupAlive(pgid) {
		return nil
	}
	r.log().Info("sweeping orphaned process group", zap.Int("pgid", pgid), zap.String("cause", why))
	if err := signalGroup(pgid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil // gone between the check and the signal
		}
		return fmt.Errorf("SIGTERM process group %d: %w", pgid, err)
	}
	if waitGroupGone(pgid, time.Now().Add(grace)) {
		return nil
	}
	r.log().Warn("grace period expired for orphaned process group, killing it", zap.Int("pgid", pgid), zap.String("cause", why))
	return killGroup(pgid)
}

// killGroup SIGKILLs process group pgid and confirms it emptied: kill(2)
// on a group succeeds if it reached *any* member, and SIGKILL is not
// instantaneous. An already-empty group is not an error; anything still
// live afterwards is a leak the caller reports.
func killGroup(pgid int) error {
	err := signalGroup(pgid, syscall.SIGKILL)
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("SIGKILL process group %d: %w", pgid, err)
	}
	// SIGKILL is not instantaneous; a bound covers a member stuck in
	// uninterruptible sleep without letting Stop hang on it.
	if !waitGroupGone(pgid, time.Now().Add(killConfirm)) {
		return fmt.Errorf("process group %d still has live members after SIGKILL", pgid)
	}
	return nil
}

// killConfirm bounds how long killGroup waits for a SIGKILLed group to
// disappear before reporting survivors.
const killConfirm = time.Second

// waitGroupGone polls until no live member of pgid remains or the
// deadline passes, reporting whether the group emptied in time. Each
// poll is a /proc walk on Linux, so the interval backs off: fast for
// the common case of workers that leave within milliseconds, slow for
// a TERM-ignoring worker that will use the whole grace anyway.
func waitGroupGone(pgid int, deadline time.Time) bool {
	const minPoll, maxPoll = 25 * time.Millisecond, 250 * time.Millisecond
	poll := minPoll
	for {
		if !groupAlive(pgid) {
			return true
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		time.Sleep(min(poll, remaining))
		poll = min(poll*2, maxPoll)
	}
}

// Reattach always fails for exec: children of the previous Caddy
// process are gone. The signature exists for the v2 systemd runner.
func (r *execRunner) Reattach(_ handleState) (handle, bool) {
	return nil, false
}

func (r *execRunner) pipeLines(pipe io.Reader, stream string) {
	sc := bufio.NewScanner(pipe)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		r.log().Info(sc.Text(), zap.String("stream", stream))
	}
	if err := sc.Err(); err != nil {
		// A line beyond the buffer cap (or any read error) stops the
		// scanner — but this goroutine must never stop READING: an
		// undrained pipe fills within ~64KB and the child then blocks
		// forever on its next write. Log once, drain to EOF.
		r.log().Warn("log pipe scan failed; draining without logging",
			zap.String("stream", stream), zap.Error(err))
		_, _ = io.Copy(io.Discard, pipe)
	}
}

// freePort asks the kernel for an unused localhost port. The listener
// is closed before the app starts, so there is a tiny window in which
// another process could grab the port; on a single-operator VPS this
// is acceptable and the deploy simply fails loudly if it ever happens.
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		return 0, err
	}
	return port, nil
}

var _ runner = (*execRunner)(nil)

// portString formats a port for PORT env and dial addresses.
func portString(port int) string { return strconv.Itoa(port) }
