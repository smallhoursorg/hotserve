package liveswap

import (
	"bytes"
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
// wait. Falls back to the signal test if /proc is unreadable.
func groupAlive(pgid int) bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return groupSignalable(pgid)
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		// /proc/<pid>/stat: "pid (comm) state ppid pgrp ..." — comm may
		// contain spaces or parens, so split after the LAST ')'.
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue // raced with exit, or not ours to read
		}
		i := bytes.LastIndexByte(stat, ')')
		if i < 0 {
			continue
		}
		fields := bytes.Fields(stat[i+1:])
		if len(fields) < 3 {
			continue
		}
		state := fields[0][0]
		if state == 'Z' || state == 'X' {
			continue
		}
		if g, err := strconv.Atoi(string(fields[2])); err == nil && g == pgid {
			return true
		}
	}
	return false
}
