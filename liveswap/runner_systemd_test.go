package liveswap

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	godbus "github.com/godbus/dbus/v5"
	"go.uber.org/zap"
)

// fakeSystemdConn scripts the manager: it records every unit it is
// asked to start/stop/reset and serves whatever status a test sets.
type fakeSystemdConn struct {
	mu          sync.Mutex
	started     []unitSpec
	stopped     []string
	reset       []string
	status      map[string]unitStatus
	statusErr   error  // UnitStatus returns this while set
	startResult string // job result for Start ("" = done)
	startErr    error
	// failStatus is what UnitStatus shows for a unit whose start job
	// did not return "done" (the failed unit the manager keeps loaded).
	failStatus *unitStatus
	blockStart bool // StartTransientUnit waits for ctx (oneshot cancellation)
	stopResult string
	stopErr    error
	stopLeaves bool // StopUnit does not mark the unit gone
	listErr    error
	resetErr   error
	listCalls  int
	listHook   func()        // runs inside ListUnits, before it returns (interleaving tests)
	stopDelay  time.Duration // StopUnit sleeps this long (concurrency tests)
	version    int           // ManagerVersion (0 = unknown)
}

func newFakeSystemdConn() *fakeSystemdConn {
	return &fakeSystemdConn{status: map[string]unitStatus{}, version: 257}
}

func (f *fakeSystemdConn) ManagerVersion() int { return f.version }

func (f *fakeSystemdConn) StartTransientUnit(ctx context.Context, u unitSpec) (string, error) {
	f.mu.Lock()
	f.started = append(f.started, u)
	blocking, startErr, res := f.blockStart, f.startErr, f.startResult
	if res == "" {
		res = "done"
	}
	switch {
	case (startErr != nil || res != "done") && f.failStatus != nil:
		f.status[u.Name] = *f.failStatus
	case startErr != nil:
	case !u.Oneshot:
		if _, preset := f.status[u.Name]; !preset {
			f.status[u.Name] = unitStatus{LoadState: "loaded", ActiveState: "active", SubState: "running", MainPID: 4242}
		}
	}
	f.mu.Unlock()
	if startErr != nil {
		return "", startErr
	}
	if blocking {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return res, nil
}

func (f *fakeSystemdConn) StopUnit(_ context.Context, name string) (string, error) {
	f.mu.Lock()
	delay := f.stopDelay
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, name)
	if f.stopErr != nil {
		return "", f.stopErr
	}
	if f.stopResult != "" && f.stopResult != "done" {
		return f.stopResult, nil
	}
	if !f.stopLeaves {
		delete(f.status, name)
	}
	return "done", nil
}

func (f *fakeSystemdConn) UnitStatus(_ context.Context, name string) (unitStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusErr != nil {
		return unitStatus{}, f.statusErr
	}
	st, ok := f.status[name]
	if !ok {
		return unitStatus{LoadState: "not-found"}, nil
	}
	return st, nil
}

func (f *fakeSystemdConn) ResetFailedUnit(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reset = append(f.reset, name)
	if f.resetErr != nil {
		return f.resetErr
	}
	delete(f.status, name)
	return nil
}

func (f *fakeSystemdConn) ListUnits(_ context.Context, pattern string) ([]unitStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if f.listHook != nil {
		f.listHook()
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []unitStatus
	for name, st := range f.status {
		if ok, _ := path.Match(pattern, name); ok {
			st.Name = name
			out = append(out, st)
		}
	}
	return out, nil
}

func (f *fakeSystemdConn) setStatus(name string, st unitStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[name] = st
}

func (f *fakeSystemdConn) setStatusErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusErr = err
}

func (f *fakeSystemdConn) unit(i int) unitSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started[i]
}

func (f *fakeSystemdConn) stops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.stopped...)
}

func (f *fakeSystemdConn) resets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reset...)
}

var failedStatus = unitStatus{LoadState: "loaded", ActiveState: "failed", SubState: "failed", Result: "exit-code", ExecMainCode: 1, ExecMainStatus: 3}

func newTestSystemdRunner(t *testing.T) (*systemdRunner, *fakeSystemdConn) {
	t.Helper()
	conn := newFakeSystemdConn()
	r := newSystemdRunner(conn, zap.NewNop())
	r.poll = time.Millisecond
	t.Cleanup(r.close)
	return r, conn
}

// testApp writes an executable ./server into a temp release dir and
// returns a startSpec for it, the way app.go would build one.
func testApp(t *testing.T) startSpec {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "server"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return startSpec{
		app:     "demo",
		version: "v1.2",
		command: []string{"./server", "--flag"},
		dir:     dir,
		env:     []string{"PORT=8123", "HOST=127.0.0.1"},
		grace:   3 * time.Second,
	}
}

func waitDone(t *testing.T, r *systemdRunner, h handle) {
	t.Helper()
	select {
	case <-r.Wait(h):
	case <-time.After(2 * time.Second):
		t.Fatal("instance never reported exited")
	}
}

var unitNamePattern = regexp.MustCompile(`^hotserve-demo\.v1\.2\.[0-9a-f]{8}\.service$`)

func TestSystemdRunnerStartBuildsUnit(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	spec := testApp(t)
	h, err := r.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	u := conn.unit(0)
	if !unitNamePattern.MatchString(u.Name) {
		t.Fatalf("unit name %q", u.Name)
	}
	if u.Oneshot {
		t.Fatal("app units are simple services, not oneshot")
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(spec.dir, "server"))
	got, _ := filepath.EvalSymlinks(u.ExecStart[0])
	if !filepath.IsAbs(u.ExecStart[0]) || got != want || u.ExecStart[1] != "--flag" {
		t.Fatalf("ExecStart must be the absolute resolved command, got %v", u.ExecStart)
	}
	if u.WorkingDirectory != spec.dir || u.SyslogIdentifier != "hotserve-demo" || u.StopTimeout != 3*time.Second {
		t.Fatalf("unit spec: %+v", u)
	}
	if strings.Join(u.Environment, ",") != "PORT=8123,HOST=127.0.0.1" {
		t.Fatalf("environment must be exactly the caller's env, got %v", u.Environment)
	}
	st := h.state()
	if st.Unit != u.Name || st.PID != 4242 || st.StartedAt.IsZero() {
		t.Fatalf("handle state %+v", st)
	}
	if !r.Alive(h) {
		t.Fatal("freshly started instance must be alive")
	}
}

func TestSystemdRunnerUnitNamesAreUnique(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	spec := testApp(t)
	if _, err := r.Start(spec); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Start(spec); err != nil {
		t.Fatal(err)
	}
	if conn.unit(0).Name == conn.unit(1).Name {
		t.Fatal("two starts of the same version must get distinct unit names")
	}
}

func TestSystemdRunnerStartRejectsMissingBinary(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	spec := testApp(t)
	spec.command = []string{"./nope"}
	if _, err := r.Start(spec); err == nil {
		t.Fatal("a missing binary must fail Start, not become a unit that dies at exec")
	}
	if len(conn.started) != 0 {
		t.Fatal("no unit must be created for an unresolvable command")
	}
}

func TestSystemdRunnerZeroGraceGetsDefaultStopTimeout(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	spec := testApp(t)
	spec.grace = 0
	if _, err := r.Start(spec); err != nil {
		t.Fatal(err)
	}
	if conn.unit(0).StopTimeout != defaultStopTimeout {
		t.Fatalf("TimeoutStopSec=0 would mean wait forever; got %s", conn.unit(0).StopTimeout)
	}
}

func TestUnitPropertiesApplyHardening(t *testing.T) {
	props := unitProperties(unitSpec{Name: "x.service", ExecStart: []string{"/bin/true"}, StopTimeout: 3 * time.Second, Oneshot: true})
	got := map[string]any{}
	for _, p := range props {
		got[p.Name] = p.Value.Value()
	}
	for name, want := range map[string]any{
		"Restart":          "no",
		"KillMode":         "control-group",
		"NoNewPrivileges":  true,
		"TimeoutStopUSec":  uint64(3_000_000),
		"Type":             "oneshot",
		"StandardOutput":   "journal",
		"StandardError":    "journal",
		"WorkingDirectory": "",
	} {
		if got[name] != want {
			t.Errorf("%s = %v, want %v", name, got[name], want)
		}
	}
	if _, ok := got["Environment"].([]string); !ok {
		t.Errorf("Environment must be a string array even when empty, got %T", got["Environment"])
	}
	if _, ok := got["ExecStart"]; !ok {
		t.Error("ExecStart missing")
	}
	for _, p := range unitProperties(unitSpec{ExecStart: []string{"/bin/true"}, StopTimeout: 500 * time.Nanosecond}) {
		if p.Name == "TimeoutStopUSec" && p.Value.Value() != uint64(1) {
			t.Errorf("a sub-microsecond grace must clamp to 1µs, not 0 (= never SIGKILL); got %v", p.Value.Value())
		}
	}
	_ = godbus.MakeVariant // keep the import honest: properties are godbus variants
}

func TestSystemdRunnerWatcherClosesDoneWhenUnitGone(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	h, err := r.Start(testApp(t))
	if err != nil {
		t.Fatal(err)
	}
	conn.setStatus(h.state().Unit, unitStatus{LoadState: "not-found"})
	waitDone(t, r, h)
	if r.Alive(h) {
		t.Fatal("Alive must be false once the unit is gone")
	}
	if len(conn.resets()) != 0 {
		t.Fatal("a cleanly unloaded unit needs no reset")
	}
}

func TestSystemdRunnerWatcherResetsFailedUnit(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	h, err := r.Start(testApp(t))
	if err != nil {
		t.Fatal(err)
	}
	unit := h.state().Unit
	conn.setStatus(unit, failedStatus)
	waitDone(t, r, h)
	if rs := conn.resets(); len(rs) != 1 || rs[0] != unit {
		t.Fatalf("failed unit must be reset exactly once, got %v", rs)
	}
	if exit := h.(*systemdHandle).exit.Load(); exit == nil || exit.exitString() != "exit status 3" {
		t.Fatalf("exit facts must be recorded before done closes, got %v", exit)
	}
}

func TestSystemdRunnerTransportErrorNeverKillsHandle(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	h, err := r.Start(testApp(t))
	if err != nil {
		t.Fatal(err)
	}
	conn.setStatusErr(errors.New("dbus: connection closed"))
	time.Sleep(20 * time.Millisecond) // many polls' worth of errors
	if !r.Alive(h) {
		t.Fatal("a manager outage must not be reported as an instance exit")
	}
	select {
	case <-r.Wait(h):
		t.Fatal("done closed during a transport outage")
	default:
	}
	conn.setStatusErr(nil)
	conn.setStatus(h.state().Unit, unitStatus{LoadState: "not-found"})
	waitDone(t, r, h)
}

func TestSystemdRunnerStopConfirmsUnitGone(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	h, err := r.Start(testApp(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Stop(h, time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if stops := conn.stops(); len(stops) != 1 || stops[0] != h.state().Unit {
		t.Fatalf("stops %v", stops)
	}
	if r.Alive(h) {
		t.Fatal("stopped instance must not be alive")
	}
	if err := r.Stop(h, time.Second); err != nil || len(conn.stops()) != 1 {
		t.Fatal("a second Stop is a no-op nil")
	}
}

func TestSystemdRunnerStopErrorsWhenJobDoesNotFinish(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	h, err := r.Start(testApp(t))
	if err != nil {
		t.Fatal(err)
	}
	conn.mu.Lock()
	conn.stopResult = "failed"
	conn.mu.Unlock()
	if err := r.Stop(h, time.Second); err == nil || !strings.Contains(err.Error(), "stop job failed") {
		t.Fatalf("a non-done stop job must be an error, got %v", err)
	}
	if !r.Alive(h) {
		t.Fatal("an unconfirmed stop must leave the handle alive")
	}
	conn.mu.Lock()
	conn.stopResult = ""
	conn.stopErr = errors.New("dbus down")
	conn.mu.Unlock()
	if err := r.Stop(h, time.Second); err == nil || !strings.Contains(err.Error(), "dbus down") {
		t.Fatalf("a transport error on stop must be reported, got %v", err)
	}
}

func TestSystemdRunnerStopWaitsForObservedExit(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	h, err := r.Start(testApp(t))
	if err != nil {
		t.Fatal(err)
	}
	// The stop job reports done but the unit lingers (say, the manager
	// is slow to unload it). Stop must not return nil on the job alone.
	conn.mu.Lock()
	conn.stopLeaves = true
	conn.mu.Unlock()
	go func() {
		time.Sleep(20 * time.Millisecond)
		conn.setStatus(h.state().Unit, failedStatus)
	}()
	if err := r.Stop(h, time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if r.Alive(h) {
		t.Fatal("handle must be dead after Stop returns nil")
	}
}

func TestSystemdRunnerStartJobFailureResetsUnit(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	fs := failedStatus
	conn.mu.Lock()
	conn.startResult = "failed"
	conn.failStatus = &fs
	conn.mu.Unlock()
	spec := testApp(t)
	_, err := r.Start(spec)
	if err == nil || !strings.Contains(err.Error(), "start job failed") || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("got %v", err)
	}
	if len(conn.resets()) != 1 {
		t.Fatalf("a unit whose start job failed must be reset, got %v", conn.resets())
	}
}

func TestSystemdRunnerRunOnce(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	spec := testApp(t)
	if err := r.RunOnce(context.Background(), spec); err != nil {
		t.Fatalf("RunOnce success: %v", err)
	}
	u := conn.unit(0)
	if !u.Oneshot || !strings.HasSuffix(u.Name, ".prestart.service") || !unitBelongsTo(u.Name, "demo") {
		t.Fatalf("pre_start must be a oneshot unit, got %+v", u)
	}
	if u.SyslogIdentifier != "hotserve-demo" || u.StopTimeout != spec.grace {
		t.Fatalf("pre_start unit shares the app's identity: %+v", u)
	}
}

func TestSystemdRunnerRunOnceReportsExitStatus(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	fs := failedStatus
	conn.mu.Lock()
	conn.startResult = "failed"
	conn.failStatus = &fs
	conn.mu.Unlock()
	err := r.RunOnce(context.Background(), testApp(t))
	if err == nil || !strings.Contains(err.Error(), "exit status 3") || !strings.Contains(err.Error(), "job failed") {
		t.Fatalf("error must carry the exit status and job result, got %v", err)
	}
	if rs := conn.resets(); len(rs) != 1 || rs[0] != conn.unit(0).Name {
		t.Fatalf("failed oneshot must be reset, got %v", rs)
	}
}

func TestSystemdRunnerRunOnceCancelStopsUnit(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	conn.mu.Lock()
	conn.blockStart = true
	conn.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := r.RunOnce(ctx, testApp(t))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if stops := conn.stops(); len(stops) != 1 || stops[0] != conn.unit(0).Name {
		t.Fatalf("a cancelled pre_start must be stopped, stops=%v", stops)
	}
}

func TestSystemdRunnerReattach(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	started := time.Now().Add(-time.Hour)

	if _, ok, err := r.Reattach(handleState{PID: 1}); ok || err != nil {
		t.Fatal("no unit recorded ⇒ cannot reattach, not an error")
	}
	if _, ok, err := r.Reattach(handleState{Unit: "hotserve-demo.v1.aaaaaaaa.service"}); ok || err != nil {
		t.Fatal("unknown unit ⇒ observed not running")
	}

	conn.setStatus("live.service", unitStatus{LoadState: "loaded", ActiveState: "active", SubState: "running", MainPID: 77})
	h, ok, err := r.Reattach(handleState{Unit: "live.service", StartedAt: started})
	if !ok || err != nil {
		t.Fatalf("running unit must be adopted: ok=%v err=%v", ok, err)
	}
	if st := h.state(); st.PID != 77 || st.Unit != "live.service" || !st.StartedAt.Equal(started) {
		t.Fatalf("adopted state %+v", st)
	}
	if !r.Alive(h) || r.Wait(h) == nil {
		t.Fatal("adopted handle must be watched like a spawned one")
	}
	conn.setStatus("live.service", unitStatus{LoadState: "not-found"})
	waitDone(t, r, h)

	conn.setStatus("dead.service", failedStatus)
	if _, ok, err := r.Reattach(handleState{Unit: "dead.service"}); ok || err != nil {
		t.Fatal("a failed unit must never be adopted, and is not an error")
	}
	if rs := conn.resets(); len(rs) != 1 || rs[0] != "dead.service" {
		t.Fatalf("failed unit must be reset on discovery, got %v", rs)
	}

	conn.setStatusErr(errors.New("dbus down"))
	if _, ok, err := r.Reattach(handleState{Unit: "live.service"}); ok || err == nil {
		t.Fatal("an unreadable unit is an error, never 'not running'")
	}
}

func TestSystemdRunnerStartReconcilesAmbiguousFailure(t *testing.T) {
	// The D-Bus call errors but the unit is running: adopt it.
	r, conn := newTestSystemdRunner(t)
	running := unitStatus{LoadState: "loaded", ActiveState: "active", MainPID: 99}
	conn.mu.Lock()
	conn.startErr = errors.New("dbus: reply lost")
	conn.failStatus = &running
	conn.mu.Unlock()
	spec := testApp(t)
	h, err := r.Start(spec)
	if err != nil || h == nil {
		t.Fatalf("a unit observed running after an ambiguous start must be adopted: %v", err)
	}
	if h.state().PID != 99 || !r.Alive(h) {
		t.Fatalf("adopted handle %+v", h.state())
	}

	// The unit is observed absent: a plain failure, no unconfirmed flag.
	r2, conn2 := newTestSystemdRunner(t)
	conn2.mu.Lock()
	conn2.startErr = errors.New("dbus: reply lost")
	conn2.mu.Unlock()
	_, err = r2.Start(spec)
	if err == nil || unitUnconfirmed(err) {
		t.Fatalf("unit observed gone ⇒ ordinary error, got %v", err)
	}

	// Nothing can be read and the stop fails too: unconfirmed.
	r3, conn3 := newTestSystemdRunner(t)
	conn3.mu.Lock()
	conn3.startErr = errors.New("dbus: reply lost")
	conn3.statusErr = errors.New("dbus down")
	conn3.stopErr = errors.New("dbus down")
	conn3.mu.Unlock()
	_, err = r3.Start(spec)
	if !unitUnconfirmed(err) {
		t.Fatalf("unreadable + unstoppable ⇒ unconfirmed, got %v", err)
	}
	if len(conn3.stops()) != 1 {
		t.Fatal("a best-effort stop must be attempted")
	}

	// Nothing can be read; the stop job says done but the unit can
	// still not be observed: unconfirmed (invariant 4 wants an
	// observation, not a job result).
	r4, conn4 := newTestSystemdRunner(t)
	conn4.mu.Lock()
	conn4.startErr = errors.New("dbus: reply lost")
	conn4.statusErr = errors.New("dbus down")
	conn4.mu.Unlock()
	_, err = r4.Start(spec)
	if !unitUnconfirmed(err) {
		t.Fatalf("unobservable after stop ⇒ unconfirmed, got %v", err)
	}
}

func TestParseManagerUint(t *testing.T) {
	for in, want := range map[string]uint64{
		"@t 524288":               524288,
		"524288":                  524288,
		"@t 18446744073709551615": 0, // unlimited: leave the unit alone
		"garbage":                 0,
		"":                        0,
	} {
		if got := parseManagerUint(in); got != want {
			t.Errorf("parseManagerUint(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestIsNoSuchUnit(t *testing.T) {
	if !isNoSuchUnit(godbus.Error{Name: "org.freedesktop.systemd1.NoSuchUnit"}) || !isNoSuchUnit(godbus.Error{Name: "org.freedesktop.DBus.Error.UnknownObject"}) {
		t.Fatal("systemd's not-loaded errors must map to not-found")
	}
	if isNoSuchUnit(errors.New("dbus: connection closed")) || isNoSuchUnit(godbus.Error{Name: "org.freedesktop.DBus.Error.NoReply"}) {
		t.Fatal("transport failures must not read as not-found")
	}
}

func TestStatusFromPropsAndStopBudget(t *testing.T) {
	st := statusFromProps(map[string]any{
		"LoadState": "loaded", "ActiveState": "active", "SubState": "running",
		"MainPID": uint32(77), "ExecMainCode": int32(1), "ExecMainStatus": int32(3),
		"TimeoutStopUSec": uint64(60_000_000),
	})
	if st.MainPID != 77 || st.ExecMainStatus != 3 || st.StopTimeout != time.Minute {
		t.Fatalf("parsed %+v", st)
	}
	// The unit's own budget wins over a smaller caller grace; a larger
	// caller grace wins; nothing known ⇒ default, never zero.
	if st.stopBudget(3*time.Second) != time.Minute || st.stopBudget(2*time.Minute) != 2*time.Minute {
		t.Fatal("stopBudget must take the larger of unit budget and grace")
	}
	if (unitStatus{}).stopBudget(0) != defaultStopTimeout {
		t.Fatal("unknown budget and zero grace must fall back to the default")
	}
}

func TestSystemdRunnerRunOnceCancelUnconfirmedWhenStopFails(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	conn.mu.Lock()
	conn.blockStart = true
	conn.stopErr = errors.New("dbus down")
	conn.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := r.RunOnce(ctx, testApp(t))
	if !unitUnconfirmed(err) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a cancelled pre_start that could not be stopped is unconfirmed and still carries the cause, got %v", err)
	}
}

func TestSystemdRunnerSweep(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	spec := testApp(t)
	keep, err := r.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	stray, err := r.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	conn.setStatus("hotserve-demo.v0.9.11111111.service", failedStatus) // leftover failed unit of ours
	conn.setStatus("hotserve-demo-api.v1.22222222.service", unitStatus{LoadState: "loaded", ActiveState: "active"})
	conn.setStatus("hotserve-demo.v1.notanonce.service", unitStatus{LoadState: "loaded", ActiveState: "active"})

	if err := r.Sweep("demo", keep); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	stops := conn.stops()
	if len(stops) != 1 || stops[0] != stray.state().Unit {
		t.Fatalf("exactly the stray unit of this app must be stopped, got %v", stops)
	}
	if rs := conn.resets(); len(rs) != 1 || rs[0] != "hotserve-demo.v0.9.11111111.service" {
		t.Fatalf("the failed leftover must be reset, got %v", rs)
	}
	waitDone(t, r, stray)
	if !r.Alive(keep) {
		t.Fatal("keep must be untouched")
	}

	// A stop that cannot be confirmed is an error: nothing may be GC'd.
	conn.setStatus("hotserve-demo.v2.33333333.service", unitStatus{LoadState: "loaded", ActiveState: "active"})
	conn.mu.Lock()
	conn.stopErr = errors.New("dbus down")
	conn.mu.Unlock()
	if err := r.Sweep("demo", keep); !unitUnconfirmed(err) {
		t.Fatalf("unconfirmed stray stop must surface, got %v", err)
	}
	conn.mu.Lock()
	conn.stopErr = nil
	conn.listErr = errors.New("dbus down")
	conn.mu.Unlock()
	if err := r.Sweep("demo", keep); err == nil {
		t.Fatal("a listing failure is an error")
	}

	// A failed unit that cannot be reset stays loaded: not a clean sweep.
	conn.mu.Lock()
	conn.listErr = nil
	conn.resetErr = errors.New("dbus down")
	conn.mu.Unlock()
	conn.setStatus("hotserve-demo.v3.44444444.service", failedStatus)
	if err := r.Sweep("demo", keep); err == nil {
		t.Fatal("a reset failure must be reported: the unit is still loaded")
	}
}

func TestSystemdRunnerWatcherBackfillsPID(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	// Start succeeds but the immediate MainPID read fails: the unit is
	// fine, hotserve just could not ask. The watcher repairs the PID
	// once the manager answers again.
	running := unitStatus{LoadState: "loaded", ActiveState: "active", MainPID: 4242}
	conn.mu.Lock()
	conn.failStatus = &running
	conn.mu.Unlock()
	h, err := r.Start(testApp(t))
	if err != nil {
		t.Fatal(err)
	}
	if h.state().PID != 4242 {
		t.Fatalf("sanity: pid read at start, got %d", h.state().PID)
	}
	// Now the same with the read failing at start.
	conn.setStatusErr(errors.New("dbus hiccup"))
	h2, err := r.Start(testApp(t))
	if err != nil {
		t.Fatalf("the start job succeeded; a failed PID read must not fail Start: %v", err)
	}
	if h2.state().PID != 0 {
		t.Fatalf("PID should be unknown after a failed read, got %d", h2.state().PID)
	}
	conn.setStatusErr(nil)
	deadline := time.Now().Add(2 * time.Second)
	for h2.state().PID != 4242 {
		if time.Now().After(deadline) {
			t.Fatalf("watcher never backfilled the PID, still %d", h2.state().PID)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSweepUnknownApps(t *testing.T) {
	conn := newFakeSystemdConn()
	running := unitStatus{LoadState: "loaded", ActiveState: "active"}
	conn.setStatus("hotserve-demo.v1.0a1b2c3d.service", running)     // configured: keep
	conn.setStatus("hotserve-old.v3.0a1b2c3d.service", running)      // removed app: stop
	conn.setStatus("hotserve-old.v2.0a1b2c3e.service", failedStatus) // removed app: reset
	conn.setStatus("hotserve-demo-api.v1.0a1b2c3d.service", running) // another configured app: keep
	conn.setStatus("hotserve-weird.service", running)                // not ours: ignore
	configured := map[string]bool{"demo": true, "demo-api": true}
	orig := appConfigured
	appConfigured = func(app string) bool { return configured[app] }
	t.Cleanup(func() { appConfigured = orig })
	err := sweepUnknownApps(context.Background(), conn, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if stops := conn.stops(); len(stops) != 1 || stops[0] != "hotserve-old.v3.0a1b2c3d.service" {
		t.Fatalf("only the removed app's running unit must be stopped, got %v", stops)
	}
	conn.mu.Lock()
	lists := conn.listCalls
	conn.mu.Unlock()
	if lists != 2 { // one global listing + one per unknown app, not per unit
		t.Fatalf("each unknown app must be swept once, got %d listings", lists)
	}
	if rs := conn.resets(); len(rs) != 1 || rs[0] != "hotserve-old.v2.0a1b2c3e.service" {
		t.Fatalf("the removed app's failed unit must be reset, got %v", rs)
	}
	conn.mu.Lock()
	conn.listErr = errors.New("dbus down")
	conn.mu.Unlock()
	if err := sweepUnknownApps(context.Background(), conn, zap.NewNop()); err == nil {
		t.Fatal("a listing failure must be reported")
	}
}

func TestSweepUnknownAppsDoesNothingWhileExiting(t *testing.T) {
	conn := newFakeSystemdConn()
	conn.setStatus("hotserve-old.v3.0a1b2c3d.service", unitStatus{LoadState: "loaded", ActiveState: "active"})
	orig := caddyExiting
	caddyExiting = func() bool { return true }
	t.Cleanup(func() { caddyExiting = orig })
	if err := sweepUnknownApps(context.Background(), conn, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	if len(conn.stops()) != 0 {
		t.Fatalf("shutdown drops pool references; units must survive it, stops=%v", conn.stops())
	}
}

func TestSweepUnknownAppsJudgesAgainstLiveConfig(t *testing.T) {
	// A reload adds "late" back while the sweep is still listing: the
	// sweep must judge against the ledger as it is right before acting
	// — never a capture from before.
	conn := newFakeSystemdConn()
	running := unitStatus{LoadState: "loaded", ActiveState: "active"}
	conn.setStatus("hotserve-late.v1.0a1b2c3d.service", running)
	var mu sync.Mutex
	configured := map[string]bool{} // "late" is not configured when the sweep starts
	orig := appConfigured
	appConfigured = func(app string) bool { mu.Lock(); defer mu.Unlock(); return configured[app] }
	t.Cleanup(func() { appConfigured = orig })
	conn.mu.Lock()
	conn.listHook = func() { mu.Lock(); configured["late"] = true; mu.Unlock() } // the reload lands mid-listing
	conn.mu.Unlock()
	if err := sweepUnknownApps(context.Background(), conn, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	if len(conn.stops()) != 0 {
		t.Fatalf("an app the live config names must never be swept, got %v", conn.stops())
	}
}

func TestUnitApp(t *testing.T) {
	for name, want := range map[string]string{
		"hotserve-blog.v1.4.2.0a1b2c3d.service":          "blog",
		"hotserve-blog-api.v1.0a1b2c3d.prestart.service": "blog-api",
		"hotserve-blog.service":                          "",
		"dbus.service":                                   "",
	} {
		got, ok := unitApp(name)
		if got != want || ok != (want != "") {
			t.Errorf("unitApp(%q) = %q,%v want %q", name, got, ok, want)
		}
	}
}

func TestSystemdRunnerSweepStopsStraysConcurrentlyAndHonoursTheGuard(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	running := unitStatus{LoadState: "loaded", ActiveState: "active"}
	conn.setStatus("hotserve-demo.v1.11111111.service", running)
	conn.setStatus("hotserve-demo.v2.22222222.service", running)
	// A guard that turns false after the listing: nothing may be stopped.
	if err := r.sweep(context.Background(), "demo", nil, func() bool { return false }); err != nil {
		t.Fatal(err)
	}
	if len(conn.stops()) != 0 {
		t.Fatalf("guard says the app is owned now; stops=%v", conn.stops())
	}
	// Without the guard both strays go, and the sweep is one round of
	// stops, not two in sequence: a slow StopUnit is paid once.
	conn.mu.Lock()
	conn.stopDelay = 150 * time.Millisecond
	conn.mu.Unlock()
	started := time.Now()
	if err := r.sweep(context.Background(), "demo", nil, nil); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took > 280*time.Millisecond {
		t.Fatalf("strays must be stopped concurrently; two sequential stops would take ~300ms, took %s", took)
	}
	if len(conn.stops()) != 2 {
		t.Fatalf("both strays must be stopped, got %v", conn.stops())
	}
}

func TestSystemdRunnerHandleRecordsStopBudget(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	spec := testApp(t)
	spec.grace = 42 * time.Second
	h, err := r.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	if h.(*systemdHandle).stopTimeout != 42*time.Second {
		t.Fatalf("Start must record the unit's stop budget, got %s", h.(*systemdHandle).stopTimeout)
	}
	conn.setStatus("old.service", unitStatus{LoadState: "loaded", ActiveState: "active", StopTimeout: 7 * time.Minute})
	h2, ok, err := r.Reattach(handleState{Unit: "old.service"})
	if !ok || err != nil {
		t.Fatal("adopt")
	}
	if h2.(*systemdHandle).stopTimeout != 7*time.Minute {
		t.Fatalf("Reattach must record the unit's own budget, got %s", h2.(*systemdHandle).stopTimeout)
	}
	conn.setStatus("old.service", unitStatus{LoadState: "not-found"})
	waitDone(t, r, h2)
}

func TestUsecDuration(t *testing.T) {
	if usecDuration(60_000_000) != time.Minute || usecDuration(0) != 0 || usecDuration(-1) != 0 {
		t.Fatal("usec conversion")
	}
	if usecDuration(int(^uint64(0)>>1)) != 0 {
		t.Fatal("an overflowing (infinity) budget must read as unknown, not negative")
	}
}

func TestUnitBelongsTo(t *testing.T) {
	for name, want := range map[string]bool{
		"hotserve-blog.v1.4.2.0a1b2c3d.service":          true,
		"hotserve-blog.v1.4.2.0a1b2c3d.prestart.service": true,
		"hotserve-blog-api.v1.0a1b2c3d.service":          false, // another app
		"hotserve-blog.v1.service":                       false, // no nonce
		"hotserve-blogx.v1.0a1b2c3d.service":             false,
		"blog.v1.0a1b2c3d.service":                       false,
	} {
		if got := unitBelongsTo(name, "blog"); got != want {
			t.Errorf("unitBelongsTo(%q, blog) = %v, want %v", name, got, want)
		}
	}
}

func TestUnitStatusRunning(t *testing.T) {
	for _, tc := range []struct {
		st   unitStatus
		want bool
	}{
		{unitStatus{LoadState: "loaded", ActiveState: "active"}, true},
		{unitStatus{LoadState: "loaded", ActiveState: "activating"}, true},
		{unitStatus{LoadState: "loaded", ActiveState: "deactivating"}, true},
		{unitStatus{LoadState: "loaded", ActiveState: "inactive"}, false},
		{unitStatus{LoadState: "loaded", ActiveState: "failed"}, false},
		{unitStatus{LoadState: "not-found", ActiveState: "inactive"}, false},
	} {
		if got := tc.st.running(); got != tc.want {
			t.Errorf("%+v running=%v want %v", tc.st, got, tc.want)
		}
	}
	for st, want := range map[unitStatus]string{
		{ExecMainCode: 1, ExecMainStatus: 3}:                           "exit status 3",
		{ExecMainCode: 1, ExecMainStatus: 0}:                           "exit status 0",
		{ExecMainCode: 2, ExecMainStatus: 9}:                           "killed by signal 9 (killed)",
		{ExecMainCode: 0, ExecMainStatus: 0}:                           "no process exit recorded",
		{ExecMainCode: 0, ExecMainStatus: 0, Result: "exec-condition"}: "no process exit recorded (result exec-condition)",
	} {
		if got := st.exitString(); got != want {
			t.Errorf("%+v exitString = %q want %q", st, got, want)
		}
	}
}

// TestSystemdRunnerWatcherFollowsMainPID: a sandboxed unit's MainPID
// changes shortly after start (PrivatePIDs= makes the manager fork an
// intermediate that sets the namespace up, then report its child), so
// the handle must track the manager's current MainPID, not the first
// value it read.
func TestSystemdRunnerWatcherFollowsMainPID(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	h, err := r.Start(testApp(t))
	if err != nil {
		t.Fatal(err)
	}
	if h.state().PID != 4242 { // the fake's MainPID for a started unit
		t.Fatalf("sanity: pid read at start, got %d", h.state().PID)
	}
	conn.setStatus(h.state().Unit, unitStatus{LoadState: "loaded", ActiveState: "active", MainPID: 4243})
	deadline := time.Now().Add(2 * time.Second)
	for h.state().PID != 4243 {
		if time.Now().After(deadline) {
			t.Fatalf("watcher did not follow the MainPID change, still %d", h.state().PID)
		}
		time.Sleep(time.Millisecond)
	}
	if r.Alive(h) != true {
		t.Fatal("a MainPID change must not read as an exit")
	}
}

// TestSystemdRunnerStartSettlesMainPIDForFullTier: a full-tier start
// returns with the app's pid, not the intermediate the manager reports
// for its first few milliseconds; other tiers take the first read.
func TestSystemdRunnerStartSettlesMainPIDForFullTier(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	spec := testApp(t)
	// A realistic spec: the release dir is always a writable bind, which
	// is also what puts the command inside the unit's view.
	spec.sandbox = &sandboxSpec{tier: sandboxFull, root: "/var/lib/liveswap",
		writable: []bindPath{{dest: spec.dir, source: spec.dir}}}
	// The fake presets MainPID 4242 at start; flip it to 4243 shortly
	// after, like the manager does once the namespace is set up.
	go func() {
		for {
			time.Sleep(5 * time.Millisecond)
			conn.mu.Lock()
			n := len(conn.started)
			conn.mu.Unlock()
			if n > 0 {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		conn.setStatus(conn.unit(0).Name, unitStatus{LoadState: "loaded", ActiveState: "active", MainPID: 4243})
	}()
	h, err := r.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	if h.state().PID != 4243 {
		t.Fatalf("full-tier start returned the intermediate pid %d, want the settled 4243", h.state().PID)
	}
}

// TestProbeUnitsAreNotAppUnits: the capability probe's unit must sit
// outside the app-name grammar, or a concurrent sweepUnknownApps would
// stop it mid-probe — or an app legitimately called "sandbox-probe"
// would collide with it — downgrading the tier, or failing `sandbox
// require`, for no reason at all.
func TestProbeUnitsAreNotAppUnits(t *testing.T) {
	name, err := unitName(startSpec{app: "sandbox-probe", version: "full", probe: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(name, unitPrefix) || !strings.HasSuffix(name, ".service") {
		t.Fatalf("probe unit name %q is not a hotserve unit", name)
	}
	if app, ok := unitApp(name); ok {
		t.Fatalf("probe unit %q parses as app %q; a sweep would stop it", name, app)
	}
	// And an app really named sandbox-probe gets its own, distinct units.
	appUnit, err := unitName(startSpec{app: "sandbox-probe", version: "v1"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := unitApp(appUnit); !ok || got != "sandbox-probe" {
		t.Fatalf("a configured app must still parse: %q -> %q %v", appUnit, got, ok)
	}
	if strings.HasPrefix(appUnit, unitPrefix+"sandboxprobe_") {
		t.Fatal("a configured app collides with the probe namespace")
	}
}
