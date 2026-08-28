//go:build unix

package liveswap

import (
	"bufio"
	"context"
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
// group (pgid == pid, see setProcessGroup). done is closed by the reaper
// goroutine once Wait returns, so Alive/Stop never race on Wait.
//
// The leader's exit is not the group's: npm/node workers it spawned
// keep running (and keep the port) after it crashes. The group is
// swept exactly once after the leader is gone — by Stop if it was
// called, else by the reaper — coordinated by teardownMu/swept: Stop
// holds the mutex for its whole run, and the reaper takes it only
// after closing done (Stop waits on done while holding the lock).
// Once swept, the pgid is never touched again: an exited handle can
// stay current indefinitely (watchdog off) and the kernel is free to
// hand its pgid to an unrelated process group in the meantime, so a
// late Stop (Destruct, hours later) must be a no-op, not a re-sweep.
type execHandle struct {
	pid       int
	startedAt time.Time
	grace     time.Duration // group grace for an unsolicited (crash) sweep
	done      chan struct{}

	teardownMu sync.Mutex
	swept      bool // the group has been swept; never signal the pgid again
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
		// Reap the child; its exit is observable via the closed channel.
		err := cmd.Wait()
		r.log().Info("process exited", zap.Int("pid", h.pid), zap.Error(err))
		close(h.done)
		// A leader that exited on its own (crash, clean exit) leaves its
		// group unsupervised: callers gate Stop behind Alive, so nobody
		// else will ever signal these processes. Sweep them here, now,
		// while the pgid is still ours (live members keep it reserved).
		// If a Stop was in flight it holds the lock until it has swept.
		h.teardownMu.Lock()
		defer h.teardownMu.Unlock()
		if !h.swept {
			r.sweepGroup(h.pid, h.grace, "leader exited unsolicited")
			h.swept = true
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

func (r *execRunner) Stop(h handle, grace time.Duration) error {
	eh, ok := h.(*execHandle)
	if !ok {
		return fmt.Errorf("not an exec handle")
	}
	eh.teardownMu.Lock()
	defer eh.teardownMu.Unlock()
	if eh.swept {
		return nil // the pgid may belong to someone else by now
	}
	select {
	case <-eh.done:
		// Leader just exited and we beat the reaper to the lock: the
		// group is still ours (live members keep the pgid reserved), so
		// sweep it with the caller's grace and let the reaper stand down.
		r.sweepGroup(eh.pid, grace, "stop after leader exit")
		eh.swept = true
		return nil
	default:
	}
	// Between the done-check above and this signal the child can exit
	// and the kernel can recycle the pgid, landing our SIGTERM/SIGKILL
	// on an unrelated process group. The window is unavoidable for
	// process *groups* (pidfd covers only the direct child); it is
	// narrow, and every pgid we ever signal was one of our own app
	// instances moments earlier.
	if err := signalGroup(eh.pid, syscall.SIGTERM); err != nil {
		return err // ESRCH: the group emptied under us; the reaper finds nothing to sweep
	}
	// Every path below ends with the group swept: SIGKILL to the whole
	// group, or a confirmed-empty group. Record that before unlocking so
	// the reaper (blocked on the lock if the leader dies during the
	// wait) and any later Stop leave the pgid alone.
	defer func() { eh.swept = true }()
	deadline := time.Now().Add(grace)
	select {
	case <-eh.done:
	case <-time.After(grace):
		r.log().Warn("grace period expired, killing process group", zap.Int("pid", eh.pid))
		_ = signalGroup(eh.pid, syscall.SIGKILL)
		<-eh.done
		return nil
	}
	// The leader went quietly; its workers get the rest of the grace
	// before the survivors are killed. `npm start` forwards SIGTERM and
	// outlives node, so this is usually already empty.
	if waitGroupGone(eh.pid, deadline) {
		return nil
	}
	r.log().Warn("grace period expired for process group survivors, killing them", zap.Int("pgid", eh.pid))
	_ = signalGroup(eh.pid, syscall.SIGKILL)
	return nil
}

// sweepGroup terminates whatever is left of process group pgid after
// its leader has exited: SIGTERM, up to grace for the members to leave
// on their own, then SIGKILL the survivors. Nothing to do is the common
// case and stays silent.
func (r *execRunner) sweepGroup(pgid int, grace time.Duration, why string) {
	if !groupAlive(pgid) {
		return
	}
	r.log().Info("sweeping orphaned process group", zap.Int("pgid", pgid), zap.String("cause", why))
	if err := signalGroup(pgid, syscall.SIGTERM); err != nil {
		return // ESRCH: gone between the check and the signal
	}
	if waitGroupGone(pgid, time.Now().Add(grace)) {
		return
	}
	r.log().Warn("grace period expired for orphaned process group, killing it", zap.Int("pgid", pgid), zap.String("cause", why))
	_ = signalGroup(pgid, syscall.SIGKILL)
}

// waitGroupGone polls until no live member of pgid remains or the
// deadline passes, reporting whether the group emptied in time.
func waitGroupGone(pgid int, deadline time.Time) bool {
	const poll = 25 * time.Millisecond
	for {
		if !groupAlive(pgid) {
			return true
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		time.Sleep(min(poll, remaining))
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
