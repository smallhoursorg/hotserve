//go:build linux

// Package harden makes the process that imports it non-dumpable as
// early as Go allows. It is a leaf package on purpose: it depends on
// syscall alone, and Go (1.21+) initializes packages in import-path
// order among those whose dependencies are ready, so this init runs
// right after syscall's — before os, fmt, Caddy and every other
// dependency of a binary that imports liveswap.
package harden

import (
	"fmt"
	"syscall"
)

// init runs Process before main and before every heavier initializer.
// It has to be this early: app units outlive hotserve restarts, so an
// app of the same UID is already running when a new supervisor process
// starts, and any later hook (App.Start, Provision) would leave the
// whole Go initialization window open — and is never reached at all
// when the liveswap block has been removed while units keep running.
// A failure is fatal: PR_SET_DUMPABLE=0 cannot fail on a real kernel,
// so failing closed costs nothing, and a supervisor whose /proc is
// readable by the apps it supervises must not run.
//
// What remains open is the interval between execve and this init —
// the Go runtime's own start-up plus the initializers of syscall's
// dependencies, well under a millisecond — during which a same-UID
// process racing on /proc could still read the new supervisor's
// environment. Only the kernel can close that: a PID namespace for
// hotserve.service (PrivatePIDs=, systemd >= 256) or an exec under
// AT_SECURE. DESIGN-threat-model.md records it as a residual.
func init() {
	if err := Process(); err != nil {
		panic(fmt.Sprintf("liveswap/harden: cannot mark the process non-dumpable (%v): refusing to run a supervisor whose /proc is readable by the apps it supervises", err))
	}
}

// Process marks the calling process non-dumpable (PR_SET_DUMPABLE=0).
// Idempotent and cheap; init calls it, tests pin the result.
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
func Process() error {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0); errno != 0 {
		return errno
	}
	return nil
}
