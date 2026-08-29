package liveswap

import (
	"context"
	"errors"
	"os"
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
}

func newFakeSystemdConn() *fakeSystemdConn {
	return &fakeSystemdConn{status: map[string]unitStatus{}}
}

func (f *fakeSystemdConn) StartTransientUnit(ctx context.Context, u unitSpec) (string, error) {
	f.mu.Lock()
	f.started = append(f.started, u)
	blocking, startErr, res := f.blockStart, f.startErr, f.startResult
	if res == "" {
		res = "done"
	}
	switch {
	case startErr != nil:
	case res != "done" && f.failStatus != nil:
		f.status[u.Name] = *f.failStatus
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
	delete(f.status, name)
	return nil
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

var unitNamePattern = regexp.MustCompile(`^hotserve-demo-v1\.2-[0-9a-f]{8}\.service$`)

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
	if !u.Oneshot || !strings.HasSuffix(u.Name, "-prestart.service") {
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

	if _, ok := r.Reattach(handleState{PID: 1}); ok {
		t.Fatal("no unit recorded ⇒ cannot reattach")
	}
	if _, ok := r.Reattach(handleState{Unit: "hotserve-demo-v1-aaaaaaaa.service"}); ok {
		t.Fatal("unknown unit ⇒ cannot reattach")
	}

	conn.setStatus("live.service", unitStatus{LoadState: "loaded", ActiveState: "active", SubState: "running", MainPID: 77})
	h, ok := r.Reattach(handleState{Unit: "live.service", StartedAt: started})
	if !ok {
		t.Fatal("running unit must be adopted")
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
	if _, ok := r.Reattach(handleState{Unit: "dead.service"}); ok {
		t.Fatal("a failed unit must never be adopted")
	}
	if rs := conn.resets(); len(rs) != 1 || rs[0] != "dead.service" {
		t.Fatalf("failed unit must be reset on discovery, got %v", rs)
	}

	conn.setStatusErr(errors.New("dbus down"))
	if _, ok := r.Reattach(handleState{Unit: "live.service"}); ok {
		t.Fatal("an unreadable unit is not adopted")
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
	if s := (unitStatus{ExecMainCode: 2, ExecMainStatus: 9}).exitString(); s != "killed by signal 9 (killed)" {
		t.Errorf("exitString = %q", s)
	}
}
