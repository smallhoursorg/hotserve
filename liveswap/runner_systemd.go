package liveswap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// systemdRunner runs app instances as transient systemd service units
// under the hotserve user's own service manager (user@<uid>.service,
// kept alive by lingering). That manager, not hotserve, is the parent
// of every app: cgroups make "stop" mean the whole process tree, the
// journal captures stdout/stderr, and units keep running while hotserve
// itself restarts or upgrades — Reattach adopts them afterwards.
//
// Invariants (the contract app.go and watchdog.go rely on):
//
//  1. Every unit created here has Restart=no. The liveswap watchdog is
//     the only restart authority; systemd never competes with it.
//  2. A handle's done channel closes only after an OBSERVED terminal
//     unit state (inactive, failed, or no longer loaded). A D-Bus or
//     transport error never marks a handle dead: declaring a live app
//     dead would make the watchdog launch a duplicate.
//  3. Unit names are unique per Start (random nonce) and recorded in
//     state.json. Reattach adopts only a running unit; a failed one is
//     logged with its exit status, reset, and never adopted.
//  4. Stop returns nil only when the stop job reported "done" and the
//     unit was then observed gone — with KillMode=control-group and
//     SendSIGKILL the cgroup is empty at that point. Any other outcome
//     is an error, and callers keep the release on disk and skip GC.
//  5. Failed units are always reset after their exit status has been
//     recorded, so the manager never accumulates dead units.
//  6. A unit's environment is exactly the env the caller built
//     (Environment=) on top of the manager's defaults; hotserve's own
//     environment is never inherited.
//
// The logger lives behind an atomic pointer because config reloads
// swap it while the watcher goroutines of running units still log.
type systemdRunner struct {
	conn   systemdConn
	logger atomic.Pointer[zap.Logger]
	poll   time.Duration // watcher interval between unit-state reads

	// ctx bounds every D-Bus call and every watcher; cancel (close) is
	// for tests — in production watchers live as long as the process.
	ctx    context.Context
	cancel context.CancelFunc
}

// systemdConn is the narrow slice of the systemd D-Bus API the runner
// needs. userManagerClient (systemd_dbus.go) is the real one; tests
// script a fake.
type systemdConn interface {
	// StartTransientUnit creates and starts the unit and blocks until
	// its start job completes, returning the job result ("done",
	// "failed", "canceled", "timeout", "dependency", "skipped"). For a
	// oneshot unit that is when the command has exited.
	StartTransientUnit(ctx context.Context, u unitSpec) (string, error)
	// StopUnit enqueues a stop job and blocks until it completes.
	StopUnit(ctx context.Context, name string) (string, error)
	// UnitStatus reads the unit's load/active state and, for a loaded
	// service, its main PID and exit facts. A unit the manager no
	// longer knows is reported with LoadState "not-found", not an error.
	UnitStatus(ctx context.Context, name string) (unitStatus, error)
	// ResetFailedUnit clears a failed unit so the manager unloads it.
	ResetFailedUnit(ctx context.Context, name string) error
}

// unitSpec is a transient service unit as the runner wants it. The
// D-Bus client turns it into properties; the fixed hardening set
// (Restart=no, KillMode=control-group, NoNewPrivileges) is applied by
// the client to every unit and is not configurable here on purpose.
type unitSpec struct {
	Name             string
	Description      string
	SyslogIdentifier string
	WorkingDirectory string
	ExecStart        []string // ExecStart[0] is an absolute path
	Environment      []string
	Oneshot          bool // Type=oneshot (pre_start) instead of simple
	StopTimeout      time.Duration
}

// unitStatus is a snapshot of one unit as the manager reports it.
type unitStatus struct {
	LoadState   string // "loaded", "not-found", ...
	ActiveState string // "active", "activating", "deactivating", "inactive", "failed"
	SubState    string
	Result      string // service Result= once it has stopped
	MainPID     int
	// ExecMainCode/Status describe how the main process ended, with
	// waitid semantics: code 1 = exited (status is the exit code),
	// 2 = killed and 3 = dumped (status is the signal number).
	ExecMainCode   int
	ExecMainStatus int
}

func (s unitStatus) loaded() bool { return s.LoadState == "loaded" }

// running reports whether the unit still has (or may still have) live
// processes: anything but a terminal state counts, including
// deactivating — a stop in progress is not yet a stopped unit.
func (s unitStatus) running() bool {
	if !s.loaded() {
		return false
	}
	switch s.ActiveState {
	case "inactive", "failed":
		return false
	}
	return true
}

// exitString renders the main process's end for logs and errors.
func (s unitStatus) exitString() string {
	switch s.ExecMainCode {
	case 2, 3: // CLD_KILLED, CLD_DUMPED
		sig := syscall.Signal(s.ExecMainStatus)
		return fmt.Sprintf("killed by signal %d (%s)", s.ExecMainStatus, sig)
	default:
		return fmt.Sprintf("exit status %d", s.ExecMainStatus)
	}
}

const (
	// unitPollInterval bounds how quickly a crash is noticed; the
	// watchdog's own health interval is the coarser clock above it.
	unitPollInterval = 500 * time.Millisecond
	// startJobTimeout bounds the D-Bus start job for a simple service,
	// which completes as soon as the manager has forked the process.
	startJobTimeout = 30 * time.Second
	// stopSlack is how long past the unit's own stop budget a Stop call
	// waits for the manager before giving up on confirmation.
	stopSlack = 30 * time.Second
	// defaultStopTimeout backs a zero grace so a unit never gets
	// TimeoutStopSec=0, which systemd reads as "wait forever".
	defaultStopTimeout = 10 * time.Second
)

func newSystemdRunner(conn systemdConn, logger *zap.Logger) *systemdRunner {
	ctx, cancel := context.WithCancel(context.Background())
	r := &systemdRunner{conn: conn, poll: unitPollInterval, ctx: ctx, cancel: cancel}
	r.logger.Store(logger)
	return r
}

func (r *systemdRunner) setLogger(logger *zap.Logger) { r.logger.Store(logger) }
func (r *systemdRunner) log() *zap.Logger             { return r.logger.Load() }

// close stops every watcher without closing their done channels (tests).
func (r *systemdRunner) close() { r.cancel() }

// systemdHandle tracks one unit. done is closed by the watcher
// goroutine — and only by it — once the unit is observed gone.
type systemdHandle struct {
	unit      string
	pid       int
	startedAt time.Time
	done      chan struct{}
	exit      atomic.Pointer[unitStatus] // final status, set before done closes
}

func (h *systemdHandle) state() handleState {
	return handleState{PID: h.pid, StartedAt: h.startedAt, Unit: h.unit}
}

// unitNameRe is the subset of systemd's unit-name alphabet the runner
// produces: app names and version tags are already validated to
// [A-Za-z0-9._-] in liveswap.go, so no escaping is ever needed.
var unitNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+\.service$`)

func unitName(spec startSpec, oneshot bool) (string, error) {
	var nonce [4]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	name := "hotserve-" + spec.app + "-" + spec.version + "-" + hex.EncodeToString(nonce[:])
	if oneshot {
		name += "-prestart"
	}
	name += ".service"
	if !unitNameRe.MatchString(name) {
		return "", fmt.Errorf("cannot derive a unit name from app %q version %q", spec.app, spec.version)
	}
	return name, nil
}

// resolveCommand turns command[0] into the absolute path a transient
// unit needs: transient ExecStart= is not resolved against the unit's
// working directory, so "./server" must become "<release>/server".
// exec.LookPath also checks the file is executable, which turns a
// missing or non-executable binary into a clear Start error instead of
// a unit that fails a moment later with status 203.
func resolveCommand(cmd, dir string) (string, error) {
	if strings.Contains(cmd, "/") {
		if !filepath.IsAbs(cmd) {
			cmd = filepath.Join(dir, cmd)
		}
		return exec.LookPath(cmd)
	}
	return exec.LookPath(cmd)
}

func (r *systemdRunner) unitFor(spec startSpec, oneshot bool) (unitSpec, error) {
	if len(spec.command) == 0 {
		return unitSpec{}, errors.New("empty command")
	}
	name, err := unitName(spec, oneshot)
	if err != nil {
		return unitSpec{}, err
	}
	argv0, err := resolveCommand(spec.command[0], spec.dir)
	if err != nil {
		return unitSpec{}, err
	}
	desc := "hotserve app " + spec.app + " " + spec.version
	if oneshot {
		desc += " (pre_start)"
	}
	grace := spec.grace
	if grace <= 0 {
		grace = defaultStopTimeout
	}
	env := spec.env
	if env == nil {
		env = []string{}
	}
	return unitSpec{
		Name:             name,
		Description:      desc,
		SyslogIdentifier: "hotserve-" + spec.app,
		WorkingDirectory: spec.dir,
		ExecStart:        append([]string{argv0}, spec.command[1:]...),
		Environment:      env,
		Oneshot:          oneshot,
		StopTimeout:      grace,
	}, nil
}

// Start creates the unit; the start job of a simple service completes
// once the manager has forked it, so this returns promptly. A unit
// that then dies at exec time is caught by the watcher and surfaces as
// an instance exit, exactly like any other early crash.
func (r *systemdRunner) Start(spec startSpec) (handle, error) {
	u, err := r.unitFor(spec, false)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(r.ctx, startJobTimeout)
	defer cancel()
	res, err := r.conn.StartTransientUnit(ctx, u)
	if err != nil {
		return nil, fmt.Errorf("starting unit %s: %w", u.Name, err)
	}
	if res != "done" {
		st := r.reapFailed(ctx, u.Name)
		return nil, fmt.Errorf("unit %s: start job %s (%s)", u.Name, res, st.exitString())
	}
	h := &systemdHandle{unit: u.Name, startedAt: time.Now(), done: make(chan struct{})}
	if st, err := r.conn.UnitStatus(ctx, u.Name); err == nil {
		h.pid = st.MainPID
	} else {
		r.log().Warn("unit started but its main PID could not be read", zap.String("unit", u.Name), zap.Error(err))
	}
	r.log().Info("instance started", zap.String("unit", u.Name), zap.Int("pid", h.pid))
	go r.watch(h)
	return h, nil
}

// RunOnce runs a oneshot unit to completion. Its start job only
// completes when the command has exited, so the job result is the
// verdict; a failed unit is reset after its exit status is read.
func (r *systemdRunner) RunOnce(ctx context.Context, spec startSpec) error {
	u, err := r.unitFor(spec, true)
	if err != nil {
		return err
	}
	res, err := r.conn.StartTransientUnit(ctx, u)
	if err != nil {
		if ctx.Err() != nil {
			// Cancelled (deadline or aborted deploy): the unit may still
			// be running. Stop it on the runner's own context — the
			// caller's is already done.
			stopCtx, cancel := context.WithTimeout(r.ctx, stopSlack)
			defer cancel()
			if _, serr := r.conn.StopUnit(stopCtx, u.Name); serr != nil {
				r.log().Warn("stopping cancelled pre_start unit", zap.String("unit", u.Name), zap.Error(serr))
			}
			return ctx.Err()
		}
		return fmt.Errorf("starting unit %s: %w", u.Name, err)
	}
	if res == "done" {
		return nil
	}
	reapCtx, cancel := context.WithTimeout(r.ctx, stopSlack)
	defer cancel()
	st := r.reapFailed(reapCtx, u.Name)
	return fmt.Errorf("%s (unit %s: job %s)", st.exitString(), u.Name, res)
}

// Alive reports whether the watcher has not yet seen the unit gone.
func (r *systemdRunner) Alive(h handle) bool {
	sh, ok := h.(*systemdHandle)
	if !ok {
		return false
	}
	select {
	case <-sh.done:
		return false
	default:
		return true
	}
}

// Wait returns the watcher's done channel (never nil for our handles).
func (r *systemdRunner) Wait(h handle) <-chan struct{} {
	sh, ok := h.(*systemdHandle)
	if !ok {
		return nil
	}
	return sh.done
}

// Stop asks the manager to stop the unit, which applies the unit's own
// TimeoutStopSec (the grace it was started with) between SIGTERM and
// SIGKILL across the whole cgroup, then waits for the watcher to
// observe the unit gone. grace here only sizes that wait: a reattached
// unit keeps the budget it was created with.
func (r *systemdRunner) Stop(h handle, grace time.Duration) error {
	sh, ok := h.(*systemdHandle)
	if !ok {
		return errors.New("not a systemd handle")
	}
	select {
	case <-sh.done:
		return nil
	default:
	}
	if grace <= 0 {
		grace = defaultStopTimeout
	}
	ctx, cancel := context.WithTimeout(r.ctx, grace+stopSlack)
	defer cancel()
	res, err := r.conn.StopUnit(ctx, sh.unit)
	if err != nil {
		return fmt.Errorf("stopping unit %s: %w", sh.unit, err)
	}
	if res != "done" {
		return fmt.Errorf("unit %s: stop job %s", sh.unit, res)
	}
	select {
	case <-sh.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("unit %s: stop job done but the unit was not observed gone within %s", sh.unit, grace+stopSlack)
	}
}

// Reattach adopts the unit recorded in state.json if the manager still
// runs it. Whatever happened to it while hotserve was away is logged;
// a failed unit is reset so it cannot linger, and the caller relaunches.
func (r *systemdRunner) Reattach(st handleState) (handle, bool) {
	if st.Unit == "" {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(r.ctx, startJobTimeout)
	defer cancel()
	us, err := r.conn.UnitStatus(ctx, st.Unit)
	if err != nil {
		// Reported, not defended: Provision has just proven the manager
		// reachable, so this is unexpected. Relaunching is the safe
		// side — a unit that does survive stays visible in the manager.
		r.log().Error("cannot read recorded unit; relaunching instead of reattaching", zap.String("unit", st.Unit), zap.Error(err))
		return nil, false
	}
	if us.running() {
		h := &systemdHandle{unit: st.Unit, pid: us.MainPID, startedAt: st.StartedAt, done: make(chan struct{})}
		go r.watch(h)
		return h, true
	}
	if us.ActiveState == "failed" {
		r.log().Warn("recorded instance died while hotserve was down", zap.String("unit", st.Unit), zap.String("exit", us.exitString()), zap.String("result", us.Result))
		r.resetFailed(ctx, st.Unit)
	} else {
		r.log().Info("recorded unit is no longer running", zap.String("unit", st.Unit), zap.String("load_state", us.LoadState), zap.String("active_state", us.ActiveState))
	}
	return nil, false
}

// watch polls the unit until it is observed in a terminal state, then
// records the exit, resets a failed unit and closes done. Transport
// errors are logged once per outage and never end the watch.
func (r *systemdRunner) watch(h *systemdHandle) {
	t := time.NewTicker(r.poll)
	defer t.Stop()
	outage := false
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-t.C:
		}
		st, err := r.conn.UnitStatus(r.ctx, h.unit)
		if err != nil {
			if r.ctx.Err() != nil {
				return
			}
			if !outage {
				outage = true
				r.log().Warn("unit state unavailable; instance is assumed running until the manager answers", zap.String("unit", h.unit), zap.Error(err))
			}
			continue
		}
		if outage {
			outage = false
			r.log().Info("unit state readable again", zap.String("unit", h.unit))
		}
		if st.running() {
			continue
		}
		r.finish(h, st)
		return
	}
}

// finish is the single place a handle is declared dead.
func (r *systemdRunner) finish(h *systemdHandle, st unitStatus) {
	fields := []zap.Field{zap.String("unit", h.unit), zap.Int("pid", h.pid)}
	switch {
	case st.ActiveState == "failed":
		fields = append(fields, zap.String("exit", st.exitString()), zap.String("result", st.Result))
		r.log().Warn("instance exited", fields...)
		ctx, cancel := context.WithTimeout(r.ctx, stopSlack)
		r.resetFailed(ctx, h.unit)
		cancel()
	case st.loaded():
		r.log().Info("instance exited", append(fields, zap.String("exit", st.exitString()))...)
	default:
		// A clean exit unloads a transient unit before we look again;
		// the exit status is then gone with it, and that is fine.
		r.log().Info("instance exited", fields...)
	}
	h.exit.Store(&st)
	close(h.done)
}

// reapFailed reads a failed unit's exit facts and resets it.
func (r *systemdRunner) reapFailed(ctx context.Context, name string) unitStatus {
	st, err := r.conn.UnitStatus(ctx, name)
	if err != nil {
		r.log().Warn("cannot read failed unit", zap.String("unit", name), zap.Error(err))
	}
	if st.loaded() {
		r.resetFailed(ctx, name)
	}
	return st
}

func (r *systemdRunner) resetFailed(ctx context.Context, name string) {
	if err := r.conn.ResetFailedUnit(ctx, name); err != nil {
		r.log().Warn("resetting failed unit", zap.String("unit", name), zap.Error(err))
	}
}

var _ runner = (*systemdRunner)(nil)
