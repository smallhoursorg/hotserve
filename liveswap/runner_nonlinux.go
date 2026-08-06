//go:build unix && !linux

package liveswap

import "syscall"

// applyPdeathsig is a no-op outside Linux: Pdeathsig does not exist on
// other unixes. macOS is a development platform only.
func applyPdeathsig(_ *syscall.SysProcAttr) {}
