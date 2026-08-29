package liveswap

import (
	"context"
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
	// the service manager. false means the caller must launch afresh.
	Reattach(st handleState) (handle, bool)
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
