//go:build unix

package hotswap

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os/exec"
	"strconv"
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
// hotswap.go); App.Start relaunches the current version on boot.
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
type execHandle struct {
	pid       int
	startedAt time.Time
	done      chan struct{}
}

func (h *execHandle) state() handleState {
	return handleState{PID: h.pid, StartedAt: h.startedAt}
}

// Start launches the command in its own process group so Stop can
// signal the whole tree (Node apps routinely spawn children).
func (r *execRunner) Start(spec startSpec) (handle, error) {
	cmd := exec.Command(spec.command[0], spec.command[1:]...)
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
		r.log().Info("process exited", zap.Int("pid", h.pid), zap.Error(err))
		close(h.done)
	}()
	return h, nil
}

// RunOnce runs a command to completion, streaming its output to the
// logs. Cancellation (deploy deadline, client gone) kills the whole
// process group, not just the direct child.
func (r *execRunner) RunOnce(ctx context.Context, spec startSpec) error {
	cmd := exec.Command(spec.command[0], spec.command[1:]...)
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

func (r *execRunner) Stop(h handle, grace time.Duration) error {
	eh, ok := h.(*execHandle)
	if !ok {
		return fmt.Errorf("not an exec handle")
	}
	select {
	case <-eh.done:
		return nil // already exited
	default:
	}
	if err := signalGroup(eh.pid, syscall.SIGTERM); err != nil {
		return err
	}
	select {
	case <-eh.done:
		return nil
	case <-time.After(grace):
	}
	r.log().Warn("grace period expired, killing process group", zap.Int("pid", eh.pid))
	_ = signalGroup(eh.pid, syscall.SIGKILL)
	<-eh.done
	return nil
}

// Reattach always fails for exec: children of the previous Caddy
// process are gone. The signature exists for the v2 systemd runner.
func (r *execRunner) Reattach(_ handleState) (handle, bool) {
	return nil, false
}

func (r *execRunner) pipeLines(pipe interface{ Read([]byte) (int, error) }, stream string) {
	sc := bufio.NewScanner(pipe)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		r.log().Info(sc.Text(), zap.String("stream", stream))
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
