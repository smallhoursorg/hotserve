package liveswap

import (
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// groupAlive must not count zombies. When hotserve is PID 1 (the e2e
// container, a Docker image without --init) orphaned workers re-parent
// to it and are never reaped, so kill(-pgid, 0) keeps succeeding
// forever; trusting it would turn every Stop into a full-grace wait.
// An exited child we deliberately leave unreaped plays the orphan.
func TestGroupAliveIgnoresZombies(t *testing.T) {
	cmd := exec.Command("true")
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Wait() }) // reap it after the assertions
	pgid := cmd.Process.Pid

	deadline := time.Now().Add(2 * time.Second)
	for groupAlive(pgid) {
		if time.Now().After(deadline) {
			t.Fatal("groupAlive still true 2s after the only member exited (zombie counted as live)")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The zombie still holds the pgid: the naive check the /proc scan
	// replaces would report the group alive.
	if !groupSignalable(pgid) {
		t.Fatal("unreaped child should still keep its process group signalable")
	}
	if _, err := os.Stat("/proc/" + strconv.Itoa(pgid) + "/stat"); err != nil {
		t.Fatalf("zombie should still be visible in /proc: %v", err)
	}
}
