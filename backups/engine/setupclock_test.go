//go:build !e2e

package engine

import (
	"testing"
	"time"
)

// The clocks the package ships: the e2e box's binary shortens setupClock
// (setupclock_e2e.go), so the setup suite no longer shows it is two
// minutes, and this does. The order is what a person waiting sees: the
// look given up on, then the note, then the clock.
func TestSetupClocksAsShipped(t *testing.T) {
	if setupClock != 2*time.Minute || setupProbeClock != 10*time.Second || setupNote != 20*time.Second {
		t.Fatalf("clocks: setup %s, probe %s, note %s; want 2m0s, 10s, 20s", setupClock, setupProbeClock, setupNote)
	}
}
