//go:build unix && !linux

package liveswap

import "syscall"

// applyPdeathsig is a no-op outside Linux: Pdeathsig does not exist on
// other unixes. macOS is a development platform only.
func applyPdeathsig(_ *syscall.SysProcAttr) {}

// groupAlive reports whether the process group pgid still has members.
// Without /proc this is the signal test; on macOS orphans re-parent to
// launchd, which reaps them promptly, so zombies do not linger.
func groupAlive(pgid int) bool { return groupSignalable(pgid) }
