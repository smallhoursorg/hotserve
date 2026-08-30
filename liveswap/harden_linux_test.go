//go:build linux

package liveswap

import (
	"syscall"
	"testing"
)

func dumpable(t *testing.T) uintptr {
	t.Helper()
	got, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_GET_DUMPABLE, 0, 0)
	if errno != 0 {
		t.Fatalf("PR_GET_DUMPABLE: %v", errno)
	}
	return got
}

// TestHardenProcessNonDumpable pins the floor of the threat model: the
// package's init has already made this test binary non-dumpable before
// any test ran (so /proc/<pid>/environ and /proc/<pid>/root need
// CAP_SYS_PTRACE from any other process, same UID included), and
// HardenProcess is idempotent on top of it.
func TestHardenProcessNonDumpable(t *testing.T) {
	if got := dumpable(t); got != 0 {
		t.Fatalf("dumpable = %d before any call: init did not harden the process", got)
	}
	if err := HardenProcess(); err != nil {
		t.Fatalf("HardenProcess: %v", err)
	}
	if got := dumpable(t); got != 0 {
		t.Fatalf("dumpable = %d after HardenProcess, want 0", got)
	}
}
