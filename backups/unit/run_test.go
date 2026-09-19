package unit

import (
	"context"
	"errors"
	"testing"
	"time"

	sddbus "github.com/coreos/go-systemd/v22/dbus"
)

// fakeConn is a manager that can be made to do what a real one cannot
// be asked to: lose a signal, refuse to stop a unit.
type fakeConn struct {
	jobResult string // sent when the unit is started; "" sends nothing, a lost signal
	props     map[string]any
	stopErr   error
	stopped   []string
	readErr   error // what reading the unit's state returns, when set
}

func (f *fakeConn) StartTransientUnitContext(_ context.Context, _ string, _ string, _ []sddbus.Property, ch chan<- string) (int, error) {
	if f.jobResult != "" {
		ch <- f.jobResult
	}
	return 1, nil
}

func (f *fakeConn) StopUnitContext(_ context.Context, name, _ string, ch chan<- string) (int, error) {
	f.stopped = append(f.stopped, name)
	if f.stopErr != nil {
		return 0, f.stopErr
	}
	ch <- "done"
	return 1, nil
}

func (f *fakeConn) GetAllPropertiesContext(context.Context, string) (map[string]any, error) {
	if f.readErr != nil && len(f.stopped) == 0 {
		return nil, f.readErr
	}
	return f.props, nil
}
func (f *fakeConn) ResetFailedUnitContext(context.Context, string) error { return nil }
func (f *fakeConn) Close()                                               {}

func fakeRunner(c *fakeConn) *Runner {
	return &Runner{conn: c, stopWithin: time.Second, lookEvery: 20 * time.Millisecond}
}

// The job's end is a signal, and a signal can be lost; nothing here has
// a time limit to notice by.
func TestRunNoticesAUnitThatEndedWithNoWordOfItsJob(t *testing.T) {
	ok, err := fakeRunner(&fakeConn{props: map[string]any{"ActiveState": "inactive", "Result": "success"}}).Run(context.Background(), validSpec())
	if err != nil || !ok.OK() {
		t.Fatalf("a unit that ended cleanly: %+v, %v", ok, err)
	}
	failed, err := fakeRunner(&fakeConn{props: map[string]any{"ActiveState": "failed", "Result": "exit-code", "ExecMainStatus": int32(12)}}).Run(context.Background(), validSpec())
	if err != nil || failed.ExitStatus != 12 || failed.OK() {
		t.Fatalf("a unit that failed: %+v, %v", failed, err)
	}
}

func TestAUnitThatCannotBeStoppedIsSaidNotToBeGone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &fakeConn{props: map[string]any{"ActiveState": "activating"}, stopErr: errors.New("the manager said no")}
	_, err := fakeRunner(c).Run(ctx, validSpec())
	if !errors.Is(err, ErrNotConfirmedGone) || !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	// And one that is stopped but still there afterwards.
	c = &fakeConn{props: map[string]any{"ActiveState": "deactivating"}}
	if _, err := fakeRunner(c).Run(ctx, validSpec()); !errors.Is(err, ErrNotConfirmedGone) {
		t.Fatalf("%v", err)
	}
	if err := fakeRunner(&fakeConn{props: map[string]any{"ActiveState": "inactive"}}).Stop(validSpec().Name); err != nil {
		t.Fatalf("stopping a unit that goes: %v", err)
	}
	if err := fakeRunner(c).Stop("anything.service"); err == nil {
		t.Fatal("a name that is not one of this package's was stopped")
	}
}

// A unit whose state cannot be read is not waited on for ever — that
// would hold the run's lock for ever: after ten looks it is stopped, by
// name, and that is the error.
func TestAUnitThatCannotBeWatchedIsStoppedNotWaitedOn(t *testing.T) {
	c := &fakeConn{readErr: errors.New("the manager is not answering"), props: map[string]any{"ActiveState": "inactive"}}
	done := make(chan error, 1)
	go func() { _, err := fakeRunner(c).Run(context.Background(), validSpec()); done <- err }()
	select {
	case err := <-done:
		if err == nil || len(c.stopped) != 1 || c.stopped[0] != validSpec().Name {
			t.Fatalf("err %v, stopped %v", err, c.stopped)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still waiting")
	}
}

// Gone with no word of its job and no failure on record: a failed unit
// stays loaded until it is reset, so this one ended cleanly.
func TestAUnitThatIsGoneEndedCleanly(t *testing.T) {
	out, err := fakeRunner(&fakeConn{props: map[string]any{"LoadState": "not-found", "ActiveState": "inactive"}}).Run(context.Background(), validSpec())
	if err != nil || !out.OK() {
		t.Fatalf("%+v, %v", out, err)
	}
}

// Not knowing how a unit ended is not knowing that it ended.
func TestAStatusThatCannotBeReadAfterTheJobEndsStopsTheUnit(t *testing.T) {
	c := &fakeConn{jobResult: "dependency", readErr: errors.New("no answer"), props: map[string]any{"ActiveState": "inactive"}}
	_, err := fakeRunner(c).Run(context.Background(), validSpec())
	if err == nil || len(c.stopped) != 1 {
		t.Fatalf("err %v, stopped %v", err, c.stopped)
	}
}
