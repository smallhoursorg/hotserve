//go:build linux

package liveswap

import (
	"fmt"
	"syscall"
)

// init makes any binary that imports liveswap non-dumpable before its
// main runs. It has to be this early: app units outlive hotserve
// restarts, so an app of the same UID is already running when a new
// supervisor process starts, and App.Start (or any later hook) would
// leave a window — and is never reached at all when the liveswap block
// has been removed while units keep running. A failure is fatal: the
// floor is documented as unconditional, and PR_SET_DUMPABLE=0 cannot
// fail on a real kernel, so failing closed costs nothing.
func init() {
	if err := HardenProcess(); err != nil {
		panic(fmt.Sprintf("liveswap: cannot mark the process non-dumpable (%v): refusing to run a supervisor whose /proc is readable by the apps it supervises", err))
	}
}

// HardenProcess marks the calling process non-dumpable
// (PR_SET_DUMPABLE=0). Idempotent and cheap; init calls it, tests pin
// the result.
//
// Every liveswap app runs as the same UID as the supervisor, and the
// kernel gates /proc/<pid>/{environ,root,cwd,mem,fd,maps} on
// ptrace_may_access, which any same-UID process passes while the
// target is dumpable — whatever mount namespace the reader sits in. A
// compromised app could so read the supervisor's environment (ACME
// DNS tokens) or walk the host filesystem through /proc/<pid>/root. A
// non-dumpable target needs CAP_SYS_PTRACE instead, which apps under
// NoNewPrivileges never hold: this closes the supervisor-secret route
// on every host, with or without a sandbox (DESIGN-threat-model.md,
// "The shared-UID rule").
//
// Cost: the kernel writes no core dump of the process (Go prints its
// own goroutine trace on a fatal error) and the per-process files
// under /proc/<pid> become root-owned; the process's own reads of
// /proc/self are unaffected. execve resets the flag, so a child
// hotserve (`hotserve start`) sets it again through its own init.
func HardenProcess() error {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0); errno != 0 {
		return errno
	}
	return nil
}
