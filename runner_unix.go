//go:build unix

package hotswap

import (
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
