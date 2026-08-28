package liveswap

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"strconv"
	"syscall"
)

// applyPdeathsig asks the kernel to SIGTERM children if the Caddy
// process dies without cleanup (SIGKILL, OOM) — a Linux-only safety
// net against orphaned app processes.
func applyPdeathsig(attr *syscall.SysProcAttr) {
	attr.Pdeathsig = syscall.SIGTERM
}

// groupAlive reports whether any live (non-zombie) process remains in
// process group pgid. It scans /proc rather than trusting kill(-pgid,
// 0): when hotserve is PID 1 (the e2e container, a Docker image without
// --init) orphaned grandchildren re-parent to it and stay zombies —
// Go never reaps children it did not spawn — and they would keep the
// group "signalable" forever, turning every stop into a full-grace
// wait. A scan that could not see every process (unreadable /proc, a
// hidepid mount hiding a worker that changed uid) must not conclude
// "gone": it falls back to the signal test, which over-reports but
// never misses a live member.
func groupAlive(pgid int) bool {
	alive, complete := scanProcGroup("/proc", pgid)
	if alive {
		return true
	}
	if !complete {
		return groupSignalable(pgid)
	}
	return false
}

// scanProcGroup walks a procfs root for live members of pgid. alive is
// true as soon as one is found; complete is false if any entry could
// not be read or parsed (other than a process that exited mid-scan),
// meaning a negative answer is not trustworthy.
func scanProcGroup(root string, pgid int) (alive, complete bool) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return false, false
	}
	complete = true
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		// /proc/<pid>/stat: "pid (comm) state ppid pgrp ..." — comm may
		// contain spaces or parens, so split after the LAST ')'.
		stat, err := os.ReadFile(root + "/" + e.Name() + "/stat") //nolint:gosec // root is /proc (or a test fixture) and e.Name() a pid directory entry; nothing here is user input
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
				continue // exited between the listing and the read
			}
			complete = false // hidden from us: cannot rule it out
			continue
		}
		i := bytes.LastIndexByte(stat, ')')
		if i < 0 {
			complete = false
			continue
		}
		fields := bytes.Fields(stat[i+1:])
		if len(fields) < 3 || len(fields[0]) == 0 {
			complete = false
			continue
		}
		state := fields[0][0]
		if state == 'Z' || state == 'X' {
			continue
		}
		if g, err := strconv.Atoi(string(fields[2])); err == nil && g == pgid {
			return true, complete
		}
	}
	return false, complete
}
