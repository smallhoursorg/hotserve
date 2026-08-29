package liveswap

import (
	"context"
	"errors"
	"time"
)

// runner abstracts how app instances are executed so the orchestration
// pipeline in app.go is testable with a fake. The one production
// implementation is systemdRunner (runner_systemd.go): every instance
// is a transient systemd service unit under the hotserve user's own
// service manager, which is what lets apps outlive hotserve restarts.
type runner interface {
	// Start launches a long-running instance and returns immediately.
	Start(spec startSpec) (handle, error)

	// RunOnce runs a command to completion (pre_start hooks). A non-nil
	// error includes non-zero exits; ctx cancellation kills the process.
	RunOnce(ctx context.Context, spec startSpec) error

	// Alive reports whether the instance is still running.
	Alive(h handle) bool

	// Wait returns a channel that is closed once the instance exits.
	// It may return nil when the runner cannot wait on this handle;
	// callers must then fall back to polling Alive.
	Wait(h handle) <-chan struct{}

	// Stop terminates gracefully: SIGTERM, wait up to the grace the
	// instance was started with, then SIGKILL — delivered to the whole
	// process tree. Returns nil only once the instance is confirmed
	// gone; any error means the caller must assume it may still run.
	Stop(h handle, grace time.Duration) error

	// Reattach tries to re-adopt an instance from persisted state after
	// a hotserve restart: the unit named in st is still running under
	// the service manager. (nil, false, nil) means it was observed not
	// running and the caller may launch afresh; a non-nil error means
	// its state could not be read, and the caller must NOT launch (the
	// unit may well be running) — retry instead.
	Reattach(st handleState) (handle, bool, error)

	// Sweep stops every instance of app other than keep (nil: all of
	// them). The service manager, not state.json, is the ledger of what
	// is running: callers sweep before trusting their own view of an
	// app (recovery) and before deleting any release (GC). nil means
	// keep is now the only unit of the app; any error means something
	// may still be running and nothing may be deleted.
	Sweep(app string, keep handle) error
}

// unitUnconfirmedError reports that a runner operation could not
// establish whether the named unit is still running (the request may
// have reached the manager before the transport failed). Callers must
// treat the unit as possibly alive: keep its release on disk, skip GC.
type unitUnconfirmedError struct {
	unit string
	err  error
}

func (e *unitUnconfirmedError) Error() string {
	return "unit " + e.unit + " may still be running: " + e.err.Error()
}

func (e *unitUnconfirmedError) Unwrap() error { return e.err }

// unitUnconfirmed reports whether err says a unit may still be running.
func unitUnconfirmed(err error) bool {
	var u *unitUnconfirmedError
	return errors.As(err, &u)
}

// startSpec is everything a runner needs to launch one command.
type startSpec struct {
	app     string        // app name (unit naming, journal identifier)
	version string        // version tag (unit naming, description)
	command []string      // argv; command[0] looked up in PATH unless it contains a slash
	dir     string        // working directory (the release dir)
	env     []string      // complete environment, KEY=VALUE form
	grace   time.Duration // SIGTERM→SIGKILL budget applied when the instance is stopped
}

// handle identifies a running instance to its runner.
type handle interface {
	// state returns the serializable form persisted in state.json so a
	// runner can attempt Reattach after a restart.
	state() handleState
}

// handleState is the persisted identity of a running instance.
type handleState struct {
	PID       int       `json:"pid,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	// Unit is the transient systemd unit running the instance; it is
	// what Reattach looks up after a hotserve restart.
	Unit string `json:"unit,omitempty"`
}
