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

// execHandle tracks one spawned process. done is closed by the reaper
// goroutine once Wait returns, so Alive/Stop never race on Wait.
//
// A leader can spawn children into its own process group (Node/npm
// workers) that outlive it. mu guards the two latches that make the
// group sweep happen exactly once: stopping (a Stop owns the teardown,
// so the reaper must not SIGKILL survivors and cut its grace short) and
// swept (the group-sweeping SIGKILL has already been sent).
type execHandle struct {
	pid       int
	startedAt time.Time
	done      chan struct{}

	mu       sync.Mutex
	stopping bool
	swept    bool
}

func (h *execHandle) state() handleState {
	return handleState{PID: h.pid, StartedAt: h.startedAt}
}

// markStopping records that a Stop is managing this instance's teardown,
// so the reaper defers the group sweep to Stop's grace.
func (h *execHandle) markStopping() {
	h.mu.Lock()
	h.stopping = true
	h.mu.Unlock()
}

// beginSweep reports whether the caller should send the group-sweeping
// SIGKILL now, latching swept so only one caller ever does. force skips
// the stopping guard: the reaper passes force=false (it must defer to an
// in-flight Stop), Stop passes force=true (it *is* the teardown).
func (h *execHandle) beginSweep(force bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.swept || (!force && h.stopping) {
		return false
	}
	h.swept = true
	return true
}

// Start launches the command in its own process group so Stop can
// signal the whole tree (Node apps routinely spawn children).
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

	h := &execHandle{pid: cmd.Process.Pid, startedAt: time.Now(), done: make(chan struct{})}
	go func() {
		// Reap the child; its exit is observable via the closed channel.
		err := cmd.Wait()
		// A leader that exits with no Stop in flight has crashed. Sweep
		// its process group so children it spawned don't survive as
		// orphans (leaking resources, holding ports). The leader was just
		// reaped, so any surviving member keeps the pgid live — not yet
		// recyclable — and the SIGKILL targets exactly those orphans; an
		// already-empty group makes it a harmless no-op. A graceful Stop
		// sets stopping so its own grace is honored instead.
		if h.beginSweep(false) {
			_ = signalGroup(h.pid, syscall.SIGKILL)
		}
		r.log().Info("process exited", zap.Int("pid", h.pid), zap.Error(err))
		close(h.done)
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
	// Claim the teardown so the reaper defers its crash sweep to our
	// grace rather than SIGKILLing survivors the instant the leader exits.
	eh.markStopping()
	select {
	case <-eh.done:
		// The leader already exited. The reaper normally swept it at that
		// moment; sweep here only if it didn't (it saw stopping set and
		// deferred, because we raced the reap) — still crash-time-tight,
		// so the pgid is ours.
		if eh.beginSweep(true) {
			_ = signalGroup(eh.pid, syscall.SIGKILL)
		}
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
		return err
	}
	select {
	case <-eh.done:
	case <-time.After(grace):
		r.log().Warn("grace period expired, killing process group", zap.Int("pid", eh.pid))
	}
	// The leader is gone (exited within grace) or about to be (grace
	// expired); SIGKILL sweeps any surviving group members — workers the
	// leader spawned that outlived it. Sent within grace of our SIGTERM,
	// so the pgid is still ours. done-close tracks only the leader, so
	// this is what actually guarantees the group is clean on return.
	if eh.beginSweep(true) {
		_ = signalGroup(eh.pid, syscall.SIGKILL)
	}
	<-eh.done
	return nil
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
