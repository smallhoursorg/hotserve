package liveswap

import (
	"context"
	"time"
)

// runner abstracts how app instances are executed so the orchestration
// pipeline in app.go is testable with a fake, and so a systemd-backed
// implementation (units that survive Caddy restarts) can slot in later
// without touching the state machine. v1 ships execRunner only.
type runner interface {
	// Start launches a long-running instance and returns immediately.
	Start(spec startSpec) (handle, error)

	// RunOnce runs a command to completion (pre_start hooks). A non-nil
	// error includes non-zero exits; ctx cancellation kills the process.
	RunOnce(ctx context.Context, spec startSpec) error

	// Alive reports whether the instance's group leader is still running.
	// Children the leader spawned into its process group are not tracked
	// here; a crashed leader's orphans are swept by the runner, not
	// reported by Alive.
	Alive(h handle) bool

	// Wait returns a channel that is closed once the instance exits.
	// It may return nil when the runner cannot wait on this handle (a
	// reattached instance the runner did not spawn); callers must then
	// fall back to polling Alive.
	Wait(h handle) <-chan struct{}

	// Stop terminates gracefully: SIGTERM to the process group, wait up
	// to grace, then SIGKILL the group. Returns once the leader has
	// exited and its process group has been swept, so no worker the
	// leader spawned outlives the call.
	Stop(h handle, grace time.Duration) error

	// Reattach tries to re-adopt an instance from persisted state after
	// a Caddy restart. The exec runner always reports false (children
	// died with the old Caddy); a future systemd runner returns the
	// still-running unit.
	Reattach(st handleState) (handle, bool)
}

// startSpec is everything a runner needs to launch one command.
type startSpec struct {
	command []string // argv; command[0] looked up in PATH unless it contains a slash
	dir     string   // working directory (the release dir)
	env     []string // complete environment, KEY=VALUE form
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
	// Unit names a systemd unit (v2 systemd runner); empty for exec.
	Unit string `json:"unit,omitempty"`
}
