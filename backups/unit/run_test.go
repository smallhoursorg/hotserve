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
