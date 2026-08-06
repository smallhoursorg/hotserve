package liveswap

import "syscall"

// applyPdeathsig asks the kernel to SIGTERM children if the Caddy
// process dies without cleanup (SIGKILL, OOM) — a Linux-only safety
// net against orphaned app processes.
func applyPdeathsig(attr *syscall.SysProcAttr) {
	attr.Pdeathsig = syscall.SIGTERM
}
