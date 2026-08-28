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

	// Alive reports whether the instance's leader process is still
	// running. It says nothing about the rest of its process group:
	// workers can outlive a crashed leader, which is why the runner
	// itself sweeps the group when the leader exits on its own.
	Alive(h handle) bool

	// Wait returns a channel that is closed once the leader exits.
	// It may return nil when the runner cannot wait on this handle (a
	// reattached instance the runner did not spawn); callers must then
	// fall back to polling Alive.
	Wait(h handle) <-chan struct{}

	// Stop terminates gracefully: SIGTERM to the process group, wait up
	// to grace for the whole group to exit, then SIGKILL the survivors.
	// Returns once the leader has exited and the group has been swept
	// (a SIGKILLed survivor may still be mid-teardown in the kernel).
	// Safe on an already-exited handle: the group is swept at most
	// once (by Stop or by the runner at exit time) — the pgid may have
	// been recycled by then — and every later Stop just replays that
	// sweep's result. A non-nil error means processes of this instance
	// may still be running; callers must not delete its release.
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
	// grace bounds the runner's own sweep of the process group when the
	// leader exits without a Stop (crash): SIGTERM, wait grace, SIGKILL.
	// Stop takes its grace as an argument; this one covers the path on
	// which nobody calls Stop. Unused by RunOnce.
	grace time.Duration
	// onSweepFailure, if set, is called (off the runner's locks) when
	// that unsolicited sweep leaves processes behind — the same verdict
	// a later Stop replays, delivered immediately so the app can record
	// it durably even if no Stop ever reaches this handle (Caddy killed
	// or OOMed first). Unused by RunOnce.
	onSweepFailure func(error)
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
