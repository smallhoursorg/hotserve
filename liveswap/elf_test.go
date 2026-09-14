package liveswap

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// elfHeader builds the first twenty bytes of an ELF file for a machine.
func elfHeader(class byte, bigEndian bool, machine uint16) []byte {
	h := make([]byte, 20)
	copy(h, "\x7fELF")
	h[4] = class
	h[5] = 1
	if bigEndian {
		h[5] = 2
		h[18], h[19] = byte(machine>>8), byte(machine)
	} else {
		h[18], h[19] = byte(machine), byte(machine>>8)
	}
	return h
}

func writeFile(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestElfMachine(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		want string
	}{
		{"x86-64 little-endian", elfHeader(2, false, 62), "amd64"},
		{"arm64 little-endian", elfHeader(2, false, 183), "arm64"},
		{"arm64 big-endian header", elfHeader(2, true, 183), "arm64"},
		{"riscv64", elfHeader(2, false, 243), "riscv64"},
		{"32-bit x86 is the kernel's call", elfHeader(1, false, 3), ""},
		{"an unknown 64-bit machine", elfHeader(2, false, 22), ""},
		{"a shell script", []byte("#!/bin/sh\nexec ./server-bin \"$@\"\n"), ""},
		{"shorter than a header", []byte("\x7fELF"), ""},
		{"empty", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := elfMachine(writeFile(t, "f", tc.body))
			if err != nil || got != tc.want {
				t.Fatalf("elfMachine = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	if _, _, err := elfMachine(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("a missing file is an error, not a pass")
	}
}

// checkMachine passes the box's own machine, anything that is not a
// known 64-bit ELF, and refuses the other 64-bit machines naming the
// box, the file, the machine and the runner to build on.
func TestCheckMachine(t *testing.T) {
	own := machineFor(runtime.GOARCH)
	if own == 0 {
		t.Skipf("no ELF machine known for %s; the check passes everything here", runtime.GOARCH)
	}
	if err := checkMachine(writeFile(t, "own", elfHeader(2, false, own))); err != nil {
		t.Fatalf("the box's own machine must pass: %v", err)
	}
	if err := checkMachine(writeFile(t, "script", []byte("#!/bin/sh\n"))); err != nil {
		t.Fatalf("a script must pass: %v", err)
	}
	other := uint16(62)
	if runtime.GOARCH == "amd64" {
		other = 183
	}
	p := writeFile(t, "other", elfHeader(2, false, other))
	err := checkMachine(p)
	if err == nil {
		t.Fatal("an executable for another machine must be refused")
	}
	for _, want := range []string{"this box is " + runtime.GOARCH, p, elfMachines[other].name, "build on a runner of the box's architecture"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q lacks %q", err, want)
		}
	}
	if runtime.GOARCH == "arm64" && !strings.Contains(err.Error(), "runs-on: ubuntu-24.04-arm") {
		t.Errorf("an arm64 box names its hosted runner: %v", err)
	}
	if runtime.GOARCH == "amd64" && !strings.Contains(err.Error(), "runs-on: ubuntu-24.04)") {
		t.Errorf("an amd64 box names its hosted runner: %v", err)
	}
}
