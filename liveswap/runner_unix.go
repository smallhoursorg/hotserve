//go:build unix

package liveswap

import (
	"errors"
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group so signals
// reach the whole tree (Node/npm spawn grandchildren that must not
// outlive a stopped release).
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	applyPdeathsig(cmd.SysProcAttr)
}

// signalGroup signals the entire process group of pid.
func signalGroup(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}

// groupSignalable reports whether the kernel still knows the process
// group pgid: kill(-pgid, 0) fails with ESRCH once the last member is
// gone. Zombies count as members, so on its own this over-reports on
// hosts where nothing reaps orphans (hotserve as PID 1 in a container);
// groupAlive layers a precise check on top where the OS allows one.
func groupSignalable(pgid int) bool {
	return !errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH)
}
