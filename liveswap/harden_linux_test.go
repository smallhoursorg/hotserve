//go:build linux

package liveswap

import (
	"syscall"
	"testing"
)

// TestHardenProcessNonDumpable pins the floor of the threat model: after
// HardenProcess the process reports dumpable=0, so /proc/<pid>/environ
// and /proc/<pid>/root need CAP_SYS_PTRACE from any other process,
// same UID included.
func TestHardenProcessNonDumpable(t *testing.T) {
	if err := HardenProcess(); err != nil {
		t.Fatalf("HardenProcess: %v", err)
	}
	got, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_GET_DUMPABLE, 0, 0)
	if errno != 0 {
		t.Fatalf("PR_GET_DUMPABLE: %v", errno)
	}
	if got != 0 {
		t.Fatalf("dumpable = %d, want 0", got)
	}
}
