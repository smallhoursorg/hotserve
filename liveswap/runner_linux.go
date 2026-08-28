package liveswap

import (
	"bytes"
	"errors"
	"io/fs"
	"maps"
	"os"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// applyPdeathsig asks the kernel to SIGTERM children if the Caddy
// process dies without cleanup (SIGKILL, OOM) — a Linux-only safety
// net against orphaned app processes.
func applyPdeathsig(attr *syscall.SysProcAttr) {
	attr.Pdeathsig = syscall.SIGTERM
}

// awaitExitUnreaped blocks until the child pid has exited, without
// reaping it: waitid(WNOWAIT) leaves it a zombie, so its pid — and the
// pgid it leads — stay reserved until the caller reaps. Reports false
// if the kernel refused, in which case the caller must reap first.
func awaitExitUnreaped(pid int) bool {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return err == nil
	}
}

// groupSnapshot is one procfs view of a process group: pid → state
// byte for every member found, zombies included.
type groupSnapshot map[int]byte

func (g groupSnapshot) anyLive() bool {
	for _, st := range g {
		if st != 'Z' && st != 'X' {
			return true
		}
	}
	return false
}

// groupAlive reports whether any live (non-zombie) process remains in
// process group pgid. It scans /proc rather than trusting kill(-pgid,
// 0): zombies keep a group signalable — the exited leader itself is
// deliberately left one while its group is swept (execHandle invariant
// 2), and when hotserve is PID 1 (the e2e container, a Docker image
// without --init) orphaned grandchildren re-parent to it and are never
// reaped — so the signal test alone would turn every stop into a
// full-grace wait.
func groupAlive(pgid int) bool {
	return groupAliveWith(
		func() (groupSnapshot, bool) { return scanProcGroup("/proc", pgid) },
		func() bool { return groupSignalable(pgid) })
}

// maxScanPasses bounds groupAliveWith's search for a stable snapshot.
const maxScanPasses = 4

// groupAliveWith is groupAlive over injectable primitives. "Gone" is
// accepted from exactly two sources: the kernel saying nothing is left
// (kill(-pgid, 0) → ESRCH, which is atomic), or two consecutive
// identical zombie-only snapshots. One snapshot is not enough: the
// listing and the per-pid reads are not atomic, so a member can fork a
// same-group child after the listing and die before its own read,
// leaving the child unseen — it shows up in the next listing. A scan
// that could not read every entry, or a view that keeps changing within
// maxScanPasses, is reported ALIVE: the callers then wait, escalate or
// record a leak, all of which are safe where a wrong "gone" is not.
func groupAliveWith(scan func() (groupSnapshot, bool), signalable func() bool) bool {
	var prev groupSnapshot
	for pass := 0; pass < maxScanPasses; pass++ {
		snap, complete := scan()
		if snap.anyLive() || !complete {
			return true
		}
		if !signalable() {
			return false // nothing left, not even zombies
		}
		if prev != nil && maps.Equal(prev, snap) {
			return false // stable: only the same zombies, twice in a row
		}
		prev = snap
	}
	return true // never settled; assume a live member is hiding in the churn
}

// scanProcGroup walks a procfs root for members of pgid. complete is
// false if any entry could not be read or parsed (other than a process
// that exited mid-scan), meaning a negative answer is not trustworthy.
func scanProcGroup(root string, pgid int) (members groupSnapshot, complete bool) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, false
	}
	members, complete = groupSnapshot{}, true
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
		if g, err := strconv.Atoi(string(fields[2])); err == nil && g == pgid {
			members[pid] = fields[0][0]
		}
	}
	return members, complete
}
