//go:build linux

package liveswap

import "syscall"

// HardenProcess marks the calling process non-dumpable
// (PR_SET_DUMPABLE=0). It is idempotent and cheap; cmd/hotserve calls
// it at entry, and App.Start calls it again so a Caddy built with
// xcaddy that merely imports this module gets the same floor.
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
// hotserve (`hotserve start`) sets it again on its own entry.
func HardenProcess() error {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0); errno != 0 {
		return errno
	}
	return nil
}
