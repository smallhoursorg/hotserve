package liveswap

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime"
	"syscall"
)

// A tarball built on the wrong runner — an x86-64 Node single
// executable pushed to an arm64 box — used to fail as the app's exit
// (status 203, "exec format error") a phase later. The ELF header says
// what machine a file is for in its first twenty bytes, so the deploy
// asks before any unit exists, and the refusal names the fix.
//
// Only the 64-bit machine families a box can be are known here: a
// file for any other machine, and any file that is not ELF (a script,
// a shim), passes — the check exists to name one mistake, not to
// second-guess what the kernel will run (a 32-bit executable under a
// compat kernel, say).

// preflightError is a refusal the deployer can act on — the release
// or the box's runtime layout as given — which the deploy reports as
// a 422. Any other error from a pre-flight is hotserve's own (a bind
// source that cannot be resolved, a file that cannot be read) and is
// reported as such.
type preflightError struct{ msg string }

func (e *preflightError) Error() string { return e.msg }

// elfMachines maps e_machine to the Go architecture name a box reports
// as runtime.GOARCH, and the machine's own name for the message.
var elfMachines = map[uint16]struct{ goarch, name string }{
	62:  {"amd64", "x86-64"},
	183: {"arm64", "AArch64 (arm64)"},
	243: {"riscv64", "RISC-V 64"},
}

// elfMachine reads path's ELF header and returns the Go architecture
// name of the machine it was built for. Both "" when the file is not
// ELF or is for a machine this check does not know.
func elfMachine(path string) (goarch, name string, err error) {
	// Non-blocking, then checked: a FIFO where the command should be —
	// a release dir is the app's to write, and a rollback relaunches
	// one the app has had — would otherwise hold the open, and the
	// app's deploy lock with it, for good. Only a regular file is
	// classified; the launch is where anything else fails.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // the resolved command of a release the deploy is about to run
	if errors.Is(err, fs.ErrPermission) {
		// Executable but not readable (mode 0111): the kernel can run
		// it and this check cannot read it, so it passes unclassified,
		// like a machine the check does not know.
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	defer f.Close() //nolint:errcheck // read-only
	if fi, err := f.Stat(); err != nil {
		return "", "", err
	} else if !fi.Mode().IsRegular() {
		return "", "", nil
	}
	var hdr [20]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return "", "", nil // shorter than a header: not ELF
		}
		return "", "", err // could not be read: not a pass
	}
	if string(hdr[:4]) != "\x7fELF" {
		return "", "", nil
	}
	var order binary.ByteOrder
	switch hdr[5] { // EI_DATA
	case 1:
		order = binary.LittleEndian
	case 2:
		order = binary.BigEndian
	default:
		return "", "", nil // not an encoding ELF defines: unclassified
	}
	if hdr[4] != 2 { // EI_CLASS: not ELFCLASS64 — a 32-bit file is the kernel's call
		return "", "", nil
	}
	m, ok := elfMachines[order.Uint16(hdr[18:20])]
	if !ok {
		return "", "", nil
	}
	return m.goarch, m.name, nil
}

// checkMachine refuses an executable built for a 64-bit machine that
// is not this box's, in the words the deployer can act on.
func checkMachine(path string) error {
	goarch, name, err := elfMachine(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if goarch == "" || goarch == runtime.GOARCH {
		return nil
	}
	if _, known := elfMachines[machineFor(runtime.GOARCH)]; !known {
		return nil // a box whose machine this check does not know cannot compare
	}
	return &preflightError{fmt.Sprintf("this box is %s and %s is an %s executable: build on a runner of the box's architecture%s", runtime.GOARCH, path, name, runsOnHint(runtime.GOARCH))}
}

// machineFor is elfMachines' inverse for the box's own architecture.
func machineFor(goarch string) uint16 {
	for m, v := range elfMachines {
		if v.goarch == goarch {
			return m
		}
	}
	return 0
}

// runsOnHint is the GitHub-hosted runner label for the box, where the
// examples' workflows set it; "" for a box with no hosted runner.
func runsOnHint(goarch string) string {
	switch goarch {
	case "arm64":
		return " (runs-on: ubuntu-24.04-arm)"
	case "amd64":
		return " (runs-on: ubuntu-24.04)"
	}
	return ""
}
