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
	"sync"
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
//  3. Unit names are unique per Start (random nonce), name their app
//     unambiguously (see unitName), and are recorded in state.json.
//     Reattach adopts only a unit observed running; a failed one is
//     logged with its exit status, reset, and never adopted; a unit
//     whose state cannot be read is an error, never "not running".
//  4. Stop returns nil only when the stop job reported "done" and the
//     unit was then observed gone — with KillMode=control-group and
//     SendSIGKILL the cgroup is empty at that point. Any other outcome
//     is an error, and callers keep the release on disk and skip GC.
//  5. Once a start request may have reached the manager, the unit is
//     reconciled by name: an ambiguous failure ends with the unit
//     observed running (adopted), observed gone, or reported as
//     unitUnconfirmedError — never silently dropped.
//  6. Failed units are always reset after their exit status has been
//     recorded, so the manager never accumulates dead units.
//  7. The manager is the ledger of what runs. Sweep(app, keep) returns
//     nil only when keep is the only loaded unit of app; recovery and
//     GC both sweep first, so nothing hotserve forgot (a unit whose
//     state.json write failed, a stop that could not be confirmed)
//     survives the next start or the next deploy — and nothing is ever
//     launched beside one: every Start is preceded by a sweep that
//     must confirm. Removing an app from the config sweeps all of it,
//     and every start sweeps the units of apps the config no longer
//     names (sweepUnknownApps), so a unit cannot outlive its app
//     definition either. (Removing the whole liveswap block and then
//     restarting hotserve is the one path that leaves units running:
//     on exit they are kept for reattach by design, and no liveswap
//     code runs afterwards to judge them — documented for operators.)
//  8. A unit's environment is exactly the env the caller built
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
	// ListUnits returns the loaded units whose names match the glob.
	ListUnits(ctx context.Context, pattern string) ([]unitStatus, error)
}

// unitSpec is a transient service unit as the runner wants it. The
// D-Bus client turns it into properties; the fixed hardening set
// (Restart=no, KillMode=control-group, NoNewPrivileges) is applied by
// the client to every unit and is not configurable here on purpose.
// Sandbox adds the per-unit isolation set (sandbox.go); nil = none.
type unitSpec struct {
	Name             string
	Description      string
	SyslogIdentifier string
	WorkingDirectory string
	ExecStart        []string // ExecStart[0] is an absolute path
	Environment      []string
	Oneshot          bool // Type=oneshot (pre_start) instead of simple
	StopTimeout      time.Duration
	Sandbox          *sandboxSpec
}

// unitStatus is a snapshot of one unit as the manager reports it.
type unitStatus struct {
	Name        string
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
	// StopTimeout is the unit's own TimeoutStopSec — the SIGTERM→SIGKILL
	// budget it was created with, which is what bounds a stop of it.
	StopTimeout time.Duration
}

// stopBudget is how long a stop of this unit can take before the
// manager's SIGKILL lands: its own TimeoutStopSec, or the caller's
// grace when that is unknown or larger.
func (s unitStatus) stopBudget(grace time.Duration) time.Duration {
	if s.StopTimeout > grace {
		return s.StopTimeout
	}
	if grace <= 0 {
		return defaultStopTimeout
	}
	return grace
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

// exitString renders the main process's end for logs and errors. An
// ExecMainCode of 0 means the manager recorded no exit at all (the
// unit never got as far as a process — a start job that was canceled,
// skipped or failed a dependency), which must not read as "exit 0".
func (s unitStatus) exitString() string {
	switch s.ExecMainCode {
	case 1: // CLD_EXITED
		return fmt.Sprintf("exit status %d", s.ExecMainStatus)
	case 2, 3: // CLD_KILLED, CLD_DUMPED
		sig := syscall.Signal(s.ExecMainStatus)
		return fmt.Sprintf("killed by signal %d (%s)", s.ExecMainStatus, sig)
	default:
		if s.Result != "" && s.Result != "success" {
			return "no process exit recorded (result " + s.Result + ")"
		}
		return "no process exit recorded"
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
	// pollTimeout bounds one watcher status read.
	pollTimeout = 10 * time.Second
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
	pid       atomic.Int64 // MainPID; 0 until read, backfilled by the watcher
	startedAt time.Time
	// stopTimeout is the unit's own SIGTERM→SIGKILL budget as known
	// when the handle was made (the start spec's grace, or the value
	// read at reattach) — the fallback when a fresh read fails.
	stopTimeout time.Duration
	// sandbox is the tier the unit was started with (or recorded with,
	// at reattach) — persisted so a relaunch reproduces it.
	sandbox sandboxTier
	done    chan struct{}
	exit    atomic.Pointer[unitStatus] // final status, set before done closes
}

func (h *systemdHandle) state() handleState {
	st := handleState{PID: int(h.pid.Load()), StartedAt: h.startedAt, Unit: h.unit}
	if h.sandbox != sandboxNone {
		st.Sandbox = h.sandbox.String()
	}
	return st
}

// Unit naming: hotserve-<app>.<version>.<nonce>[.prestart].service.
// App names are [a-z0-9-] and version tags [A-Za-z0-9._-] (validated
// in liveswap.go), all legal unit-name characters, so nothing is ever
// escaped. The "." after the app is the one character an app name
// cannot contain, which is what lets Sweep match "blog" without also
// matching "blog-api"; the nonce makes every Start unique.
const unitPrefix = "hotserve-"

// unitNameRe is derived from the app-name and version alphabets in
// liveswap.go so the three can never drift apart.
var unitNameRe = regexp.MustCompile(
	`^` + regexp.QuoteMeta(unitPrefix) +
		`(` + strings.Trim(appNameRe.String(), "^$") + `)` +
		`\.(` + strings.Trim(versionRe.String(), "^$") + `)` +
		`\.[0-9a-f]{8}(\.prestart)?\.service$`)

func unitName(spec startSpec, oneshot bool) (string, error) {
	// The webhook validates both before a deploy gets this far; the
	// runner enforces its own precondition anyway ("." and ".." match
	// the version alphabet but are not versions).
	if !spec.probe && (!appNameRe.MatchString(spec.app) || !validVersion(spec.version)) {
		return "", fmt.Errorf("cannot derive a unit name from app %q version %q", spec.app, spec.version)
	}
	var nonce [4]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	if spec.probe {
		// Underscores are outside the app-name alphabet, so this name
		// can never be produced from config and unitNameRe never
		// matches it: unitApp reports "not an app unit" and every
		// sweep skips it (a probe named like an app would be stopped
		// by a concurrent sweepUnknownApps, or collide with an app
		// actually called "sandbox-probe", downgrading the tier or
		// failing `sandbox require` for no reason).
		return unitPrefix + "sandboxprobe_" + hex.EncodeToString(nonce[:]) + ".service", nil
	}
	name := unitPrefix + spec.app + "." + spec.version + "." + hex.EncodeToString(nonce[:])
	if oneshot {
		name += ".prestart"
	}
	name += ".service"
	if !unitNameRe.MatchString(name) {
		return "", fmt.Errorf("cannot derive a unit name from app %q version %q", spec.app, spec.version)
	}
	return name, nil
}

// unitBelongsTo reports whether name is one of app's units.
func unitBelongsTo(name, app string) bool {
	got, ok := unitApp(name)
	return ok && got == app
}

// unitApp extracts the app name from one of our unit names.
func unitApp(name string) (string, bool) {
	m := unitNameRe.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// appConfigured reports whether any loaded config — the live one, or
// a candidate being provisioned — holds the app. The pool is the
// authority: a candidate that later fails to activate still holds its
// pool references until its Cleanup, and the config it would have
// replaced holds its own throughout, so nothing a sweep judges
// "unknown" can belong to a config that is or may yet be serving. A
// variable so tests can substitute their own ledger.
var appConfigured = func(app string) bool {
	refs, ok := appPool.References(poolKey(app))
	return ok && refs > 0
}

// unknownSweepMu serializes start-time sweeps with each other (two
// configs starting back to back), never with config publication.
var unknownSweepMu sync.Mutex

// sweepUnknownApps stops every hotserve unit whose app the current
// config does not name (invariant 7): an app removed or renamed while
// hotserve was down has no managedApp left to sweep it, so App.Start
// does it here against the manager's own listing.
func sweepUnknownApps(ctx context.Context, conn systemdConn, logger *zap.Logger) error {
	unknownSweepMu.Lock()
	defer unknownSweepMu.Unlock()
	if caddyExiting() {
		return nil // shutdown drops pool references; the units are meant to survive
	}
	units, err := conn.ListUnits(ctx, unitPrefix+"*.service")
	if err != nil {
		return fmt.Errorf("listing hotserve units: %w", err)
	}
	r := newSystemdRunner(conn, logger)
	defer r.close()
	var errs []error
	seen := map[string]bool{}
	for _, u := range units {
		app, ok := unitApp(u.Name)
		if !ok || seen[app] {
			continue
		}
		seen[app] = true
		// Judged now, not at listing time: a reload may have added the
		// app back while the listing was in flight.
		if appConfigured(app) {
			continue
		}
		logger.Warn("stopping units of an app no longer in the config", zap.String("app", app), zap.String("unit", u.Name))
		// The caller's deadline bounds the whole sweep, so a slow
		// manager cannot hold the start-time sweep open indefinitely;
		// each stop re-checks the pool first, so a reload adopting the
		// app between listing and stopping keeps its unit.
		if serr := r.sweep(ctx, app, nil, func() bool { return !caddyExiting() && !appConfigured(app) }); serr != nil {
			errs = append(errs, serr)
		}
	}
	return errors.Join(errs...)
}

// resolveCommand turns command[0] into the absolute path a transient
// unit needs: transient ExecStart= is not resolved against the unit's
// working directory, so "./server" must become "<release>/server".
// exec.LookPath also checks the file is executable, which turns a
// missing or non-executable binary into a clear Start error instead of
// a unit that fails a moment later with status 203.
func resolveCommand(cmd, dir string) (string, error) {
	if strings.Contains(cmd, "/") && !filepath.IsAbs(cmd) {
		cmd = filepath.Join(dir, cmd)
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
	// As late as possible, and before the manager follows any of them:
	// what a bind source points at is only knowable now (see
	// resolveBindSources). A refusal fails the launch — a deploy falls
	// back to the version still serving.
	if spec.sandbox != nil {
		if err := spec.sandbox.resolveBindSources(); err != nil {
			return unitSpec{}, err
		}
		// HOME is a default buildEnv applies before env_file and inline
		// env, so an operator may point it elsewhere on purpose. One
		// pointed outside the view is allowed and reported: it is the
		// difference between "npm failed" and "npm's HOME was never in
		// my view".
		if home, outside := homeOutsideView(spec.env, spec.sandbox); outside {
			r.log().Warn("HOME is set to a path the sandbox does not bind; it does not exist inside the unit",
				zap.String("app", spec.app),
				zap.String("home", home),
				zap.String("effect", "any runtime that touches $HOME (npm, corepack, pip) will fail with ENOENT naming no cause"),
				zap.String("fix", "leave HOME unset to get the app's shared dir, or declare the path as an rw extra_path"))
		}
		// LookPath above ran in *this* process's view of the filesystem,
		// which under a deny-by-default sandbox is not the unit's: a
		// runtime installed outside /usr — /opt/node/bin/node, an asdf
		// or nvm shim under a home directory — resolves here and then
		// does not exist in there. systemd would report that as a bare
		// status=203/EXEC after the unit has already been created, so
		// say it now, in the language the operator can act on.
		// Resolved, because LookPath does not follow symlinks: a shim at
		// /usr/local/bin/node -> /opt/node/bin/node is under the /usr
		// base entry and would pass unresolved, then die as the very
		// 203/EXEC this check exists to replace — the symlink is inside
		// the view, its target is not.
		target := argv0
		if t, err := filepath.EvalSymlinks(argv0); err == nil {
			target = t
		}
		if !spec.sandbox.inView(target) {
			via := ""
			if target != argv0 {
				via = fmt.Sprintf(" (via %s, which is)", argv0)
			}
			return unitSpec{}, fmt.Errorf("%s is not inside the sandbox view of app %s%s: an app sees its release dir, its shared dir, the OS runtime (/usr and a named handful of /etc) and its extra_paths, and nothing else on this host — declare the directory holding it with `extra_path`, or use `sandbox off` for this app", target, spec.app, via)
		}
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
		Sandbox:          spec.sandbox,
	}, nil
}

// sandboxTierOf is the tier a start spec asks for (none when nil).
func sandboxTierOf(spec startSpec) sandboxTier {
	if spec.sandbox == nil {
		return sandboxNone
	}
	return spec.sandbox.tier
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
		// The request may or may not have reached the manager
		// (invariant 5): reconcile by name rather than guess.
		return r.reconcileStart(u, fmt.Errorf("starting unit %s: %w", u.Name, err))
	}
	if res != "done" {
		st := r.reapFailed(ctx, u.Name)
		return nil, fmt.Errorf("unit %s: start job %s (%s)", u.Name, res, st.exitString())
	}
	return r.adopt(ctx, u.Name, time.Now(), u.StopTimeout, sandboxTierOf(spec)), nil
}

// adopt builds a watched handle for a unit known to be running.
func (r *systemdRunner) adopt(ctx context.Context, unit string, startedAt time.Time, stopTimeout time.Duration, tier sandboxTier) *systemdHandle {
	h := &systemdHandle{unit: unit, startedAt: startedAt, stopTimeout: stopTimeout, sandbox: tier, done: make(chan struct{})}
	if st, err := r.conn.UnitStatus(ctx, unit); err == nil {
		h.pid.Store(int64(st.MainPID))
		if tier == sandboxFull {
			r.settleMainPID(ctx, h, st.MainPID)
		}
	} else {
		r.log().Warn("unit started but its main PID could not be read yet", zap.String("unit", unit), zap.Error(err))
	}
	r.log().Info("instance started", zap.String("unit", unit), zap.Int64("pid", h.pid.Load()))
	go r.watch(h)
	return h
}

// settleMainPID waits, briefly, for the MainPID of a unit in its own
// PID namespace to be the app's: with PrivatePIDs= the manager first
// reports the intermediate it forked to set the namespace up, and
// switches to that intermediate's child — the app — a few
// milliseconds later. A pid read in between is stale (and dead almost
// at once); status, state.json and the watchdog all read the handle,
// so the switch is awaited here rather than at the next watcher poll.
// Bounded: if the manager never changes its mind the first value
// stands and the watcher keeps following it.
func (r *systemdRunner) settleMainPID(ctx context.Context, h *systemdHandle, first int) {
	deadline := time.Now().Add(mainPIDSettle)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(mainPIDSettleStep):
		}
		st, err := r.conn.UnitStatus(ctx, h.unit)
		if err != nil || !st.running() {
			return
		}
		if st.MainPID != 0 && st.MainPID != first {
			h.pid.Store(int64(st.MainPID))
			return
		}
	}
}

const (
	mainPIDSettle     = 500 * time.Millisecond
	mainPIDSettleStep = 10 * time.Millisecond
)

// reconcileStart settles an ambiguous start: the unit is observed
// running (adopted — the deploy proceeds), observed gone (a plain
// failure), or, when the manager cannot even be asked, stopped on a
// best-effort basis and reported as possibly running.
func (r *systemdRunner) reconcileStart(u unitSpec, cause error) (handle, error) {
	unit := u.Name
	readCtx, readCancel := context.WithTimeout(r.ctx, pollTimeout)
	st, err := r.conn.UnitStatus(readCtx, unit)
	readCancel()
	switch {
	case err == nil && st.running():
		r.log().Warn("start reported an error but the unit is running; adopting it", zap.String("unit", unit), zap.Error(cause))
		ctx, cancel := context.WithTimeout(r.ctx, pollTimeout)
		defer cancel()
		tier := sandboxNone
		if u.Sandbox != nil {
			tier = u.Sandbox.tier
		}
		return r.adopt(ctx, unit, time.Now(), u.StopTimeout, tier), nil
	case err == nil:
		if st.loaded() {
			ctx, cancel := context.WithTimeout(r.ctx, pollTimeout)
			_ = r.resetFailed(ctx, unit)
			cancel()
		}
		return nil, cause
	}
	// The manager cannot be asked: stop by name and confirm, or report
	// the unit as possibly running — the same sequence RunOnce uses,
	// given the stop budget the unit was created with.
	ctx, cancel := context.WithTimeout(r.ctx, u.StopTimeout+stopSlack)
	defer cancel()
	return nil, r.stopUnobserved(ctx, unit, cause)
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
		cause := fmt.Errorf("starting unit %s: %w", u.Name, err)
		if ctx.Err() != nil {
			cause = ctx.Err()
		}
		// Cancelled, or the transport failed mid-flight: either way
		// the unit may be running, and a pre_start whose outcome we
		// cannot observe is a failed pre_start. Stop it and confirm,
		// on the runner's own context — the caller's is already done.
		stopCtx, cancel := context.WithTimeout(r.ctx, u.StopTimeout+stopSlack)
		defer cancel()
		return r.stopUnobserved(stopCtx, u.Name, cause)
	}
	if res == "done" {
		return nil
	}
	reapCtx, cancel := context.WithTimeout(r.ctx, stopSlack)
	defer cancel()
	st := r.reapFailed(reapCtx, u.Name)
	return fmt.Errorf("%s (unit %s: job %s)", st.exitString(), u.Name, res)
}

// stopUnobserved stops a unit and returns cause if the unit is
// confirmed gone, or an unconfirmed error if not.
func (r *systemdRunner) stopUnobserved(ctx context.Context, unit string, cause error) error {
	res, err := r.conn.StopUnit(ctx, unit)
	if err != nil {
		return &unitUnconfirmedError{unit: unit, err: cause}
	}
	if res != "done" {
		return &unitUnconfirmedError{unit: unit, err: fmt.Errorf("%w (stop job %s)", cause, res)}
	}
	st, err := r.conn.UnitStatus(ctx, unit)
	if err != nil || st.running() {
		return &unitUnconfirmedError{unit: unit, err: cause}
	}
	if st.loaded() {
		if rerr := r.resetFailed(ctx, unit); rerr != nil {
			return &unitUnconfirmedError{unit: unit, err: errors.Join(cause, rerr)}
		}
	}
	return cause
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
	// The wait is sized from the unit's own TimeoutStopSec — a
	// reattached unit keeps the grace it was created with, whatever
	// the config says now — read fresh, else as recorded on the
	// handle, else the caller's grace. Callers hold deployMu for this
	// long, which is the operator's grace by construction.
	fallback := sh.stopTimeout
	if grace > fallback {
		fallback = grace
	}
	budget := r.stopBudgetFor(r.ctx, sh.unit, fallback)
	ctx, cancel := context.WithTimeout(r.ctx, budget+stopSlack)
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
		return fmt.Errorf("unit %s: stop job done but the unit was not observed gone within %s", sh.unit, budget+stopSlack)
	}
}

// Reattach adopts the unit recorded in state.json if the manager still
// runs it. Whatever happened to it while hotserve was away is logged;
// a failed unit is reset so it cannot linger, and the caller relaunches.
// A unit whose state cannot be read is an error: the caller must not
// launch beside a unit that may be running (invariant 3).
func (r *systemdRunner) Reattach(st handleState) (handle, bool, error) {
	if st.Unit == "" {
		return nil, false, nil
	}
	ctx, cancel := context.WithTimeout(r.ctx, startJobTimeout)
	defer cancel()
	us, err := r.conn.UnitStatus(ctx, st.Unit)
	if err != nil {
		return nil, false, fmt.Errorf("reading recorded unit %s: %w", st.Unit, err)
	}
	if us.running() {
		h := &systemdHandle{unit: st.Unit, startedAt: st.StartedAt, stopTimeout: us.StopTimeout, sandbox: parseSandboxTier(st.Sandbox), done: make(chan struct{})}
		h.pid.Store(int64(us.MainPID))
		go r.watch(h)
		return h, true, nil
	}
	if us.ActiveState == "failed" {
		r.log().Warn("recorded instance died while hotserve was down", zap.String("unit", st.Unit), zap.String("exit", us.exitString()), zap.String("result", us.Result))
		_ = r.resetFailed(ctx, st.Unit) // Sweep reports it if it stays loaded
	} else {
		r.log().Info("recorded unit is no longer running", zap.String("unit", st.Unit), zap.String("load_state", us.LoadState), zap.String("active_state", us.ActiveState))
	}
	return nil, false, nil
}

// Sweep stops every unit of app except keep (invariant 7). Failed
// leftovers are reset; running ones are stopped and confirmed gone.
// nil only when, afterwards, keep is the only loaded unit of app.
func (r *systemdRunner) Sweep(app string, keep handle) error {
	return r.sweep(r.ctx, app, keep, nil)
}

// sweep is Sweep under a parent context. Listing and each reset are
// bounded by pollTimeout. Strays are stopped concurrently, each under
// its own stop budget plus slack, so a sweep takes as long as its
// longest stray's TimeoutStopSec — never the sum, and never a flat cap
// that would report a slow-draining unit unconfirmed. still, when
// given, is consulted immediately before each stop; a false answer
// means someone now owns the app and that stray is left alone.
func (r *systemdRunner) sweep(parent context.Context, app string, keep handle, still func() bool) error {
	keepUnit := ""
	if kh, ok := keep.(*systemdHandle); ok {
		keepUnit = kh.unit
	}
	listCtx, listCancel := context.WithTimeout(parent, pollTimeout)
	units, err := r.conn.ListUnits(listCtx, unitPrefix+app+".*.service")
	listCancel()
	if err != nil {
		return fmt.Errorf("listing units of %s: %w", app, err)
	}
	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)
	record := func(err error) {
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}
	for _, u := range units {
		if u.Name == keepUnit || !unitBelongsTo(u.Name, app) {
			continue
		}
		if !u.running() {
			if u.loaded() {
				r.log().Warn("resetting leftover unit", zap.String("unit", u.Name), zap.String("active_state", u.ActiveState), zap.String("exit", u.exitString()))
				ctx, cancel := context.WithTimeout(parent, pollTimeout)
				if rerr := r.resetFailed(ctx, u.Name); rerr != nil {
					record(rerr)
				}
				cancel()
			}
			continue
		}
		wg.Add(1)
		go func(u unitStatus) {
			defer wg.Done()
			budget := r.stopBudgetFor(parent, u.Name, 0)
			if still != nil && !still() {
				r.log().Info("stray unit adopted by a config meanwhile; leaving it", zap.String("unit", u.Name))
				return
			}
			r.log().Warn("stopping stray unit not owned by this instance", zap.String("unit", u.Name), zap.Int("pid", u.MainPID))
			ctx, cancel := context.WithTimeout(parent, budget+stopSlack)
			defer cancel()
			if serr := r.stopUnobserved(ctx, u.Name, fmt.Errorf("stray unit %s", u.Name)); unitUnconfirmed(serr) {
				record(serr)
			}
		}(u)
	}
	wg.Wait()
	return errors.Join(errs...)
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
		// Each poll is bounded on its own: a manager that accepts the
		// request but never answers must not freeze the watch forever
		// (a crash would then never be observed).
		pollCtx, cancel := context.WithTimeout(r.ctx, pollTimeout)
		st, err := r.conn.UnitStatus(pollCtx, h.unit)
		cancel()
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
			// The handle follows the unit's MainPID: a read that failed
			// right after start is repaired here, and so is the pid of a
			// sandboxed unit — with PrivatePIDs= the manager forks an
			// intermediate that sets the namespace up and then reports
			// its child (the app, "supervising a process which is not
			// our child" in the journal) as MainPID a moment later.
			if st.MainPID != 0 && int64(st.MainPID) != h.pid.Load() {
				h.pid.Store(int64(st.MainPID))
			}
			continue
		}
		r.finish(h, st)
		return
	}
}

// finish is the single place a handle is declared dead.
func (r *systemdRunner) finish(h *systemdHandle, st unitStatus) {
	fields := []zap.Field{zap.String("unit", h.unit), zap.Int64("pid", h.pid.Load())}
	switch {
	case st.ActiveState == "failed":
		fields = append(fields, zap.String("exit", st.exitString()), zap.String("result", st.Result))
		r.log().Warn("instance exited", fields...)
		ctx, cancel := context.WithTimeout(r.ctx, stopSlack)
		_ = r.resetFailed(ctx, h.unit) // logged; the next Sweep reports a leftover
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

// stopBudgetFor is how long stopping unit may take before SIGKILL: its
// own TimeoutStopSec when readable, else the caller's grace, else the
// default — one policy for Stop and sweep alike.
func (r *systemdRunner) stopBudgetFor(parent context.Context, unit string, grace time.Duration) time.Duration {
	ctx, cancel := context.WithTimeout(parent, pollTimeout)
	defer cancel()
	st, err := r.conn.UnitStatus(ctx, unit)
	if err != nil {
		st = unitStatus{}
	}
	return st.stopBudget(grace)
}

// reapFailed reads a failed unit's exit facts and resets it.
func (r *systemdRunner) reapFailed(ctx context.Context, name string) unitStatus {
	st, err := r.conn.UnitStatus(ctx, name)
	if err != nil {
		r.log().Warn("cannot read failed unit", zap.String("unit", name), zap.Error(err))
	}
	if st.loaded() {
		_ = r.resetFailed(ctx, name)
	}
	return st
}

func (r *systemdRunner) resetFailed(ctx context.Context, name string) error {
	if err := r.conn.ResetFailedUnit(ctx, name); err != nil {
		r.log().Warn("resetting failed unit", zap.String("unit", name), zap.Error(err))
		return fmt.Errorf("resetting failed unit %s: %w", name, err)
	}
	return nil
}

var _ runner = (*systemdRunner)(nil)
