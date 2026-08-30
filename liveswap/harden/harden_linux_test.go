//go:build linux

package harden_test

// External test package on purpose: in-package test files are compiled
// into the package under test, so their imports (testing → os, fmt)
// would become harden's own dependencies in the test binary and push
// its init behind os — the very thing TestInitRunsBeforeOS measures.
import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"github.com/smallhoursorg/hotserve/liveswap/harden"
)

func dumpable(t *testing.T) uintptr {
	t.Helper()
	got, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_GET_DUMPABLE, 0, 0)
	if errno != 0 {
		t.Fatalf("PR_GET_DUMPABLE: %v", errno)
	}
	return got
}

// TestProcessNonDumpable pins the floor of the threat model: the
// package's init has already made this test binary non-dumpable before
// any test ran (so /proc/<pid>/environ and /proc/<pid>/root need
// CAP_SYS_PTRACE from any other process, same UID included), and
// Process is idempotent on top of it.
func TestProcessNonDumpable(t *testing.T) {
	if got := dumpable(t); got != 0 {
		t.Fatalf("dumpable = %d before any call: init did not harden the process", got)
	}
	if err := harden.Process(); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got := dumpable(t); got != 0 {
		t.Fatalf("dumpable = %d after Process, want 0", got)
	}
}

// TestInitRunsBeforeOS pins the ordering the package comment promises:
// syscall < harden < os — this package initializes after syscall and
// before os (hence before fmt, Caddy and everything depending on
// them). It does not assert that nothing runs between syscall and
// harden: syscall-closure-only leaves that sort earlier may. It
// re-runs this test binary with GODEBUG=inittrace=1, which prints one
// "init <pkg>" line per initializer in execution order.
func TestInitRunsBeforeOS(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), "GODEBUG=inittrace=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("re-exec: %v\n%s", err, out)
	}
	var order []string
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "init" {
			order = append(order, f[1])
		}
	}
	pos := func(pkg string) int {
		for i, p := range order {
			if p == pkg {
				return i
			}
		}
		t.Fatalf("no init line for %s in:\n%s", pkg, out)
		return -1
	}
	self, sys, osPos := pos("github.com/smallhoursorg/hotserve/liveswap/harden"), pos("syscall"), pos("os")
	if sys >= self || self >= osPos {
		t.Fatalf("init order: syscall=%d harden=%d os=%d, want syscall < harden < os", sys, self, osPos)
	}
	t.Logf("harden initialized %dth of %d, after syscall (%d) and before os (%d)", self+1, len(order), sys+1, osPos+1)
}
