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
// 0): when hotserve is PID 1 (the e2e container, a Docker image without
// --init) orphaned grandchildren re-parent to it and stay zombies —
// Go never reaps children it did not spawn — and they would keep the
// group "signalable" forever, turning every stop into a full-grace
// wait; the exited leader itself is deliberately left a zombie while
// its group is swept. A scan that could not see every process
// (unreadable /proc, a hidepid mount hiding a worker that changed uid)
// must not conclude "gone": it falls back to the signal test, which
// over-reports but never misses a live member.
func groupAlive(pgid int) bool {
	return groupAliveWith(
		func() (groupSnapshot, bool) { return scanProcGroup("/proc", pgid) },
		func() bool { return groupSignalable(pgid) },
		procHidesPIDs("/proc/self/mounts"))
}

// maxScanPasses bounds groupAliveWith's search for a stable snapshot.
const maxScanPasses = 4

// groupAliveWith is groupAlive over injectable primitives. A negative
// snapshot is proof only when the kernel agrees nothing is left
// (kill(-pgid, 0) → ESRCH, atomic). Otherwise the group is signalable
// — zombies, or a race: the listing and the per-pid reads are not
// atomic, so a member can fork a same-group child after the listing
// and die before its own read, leaving the child unseen. Such a child
// shows up in the next listing, so "gone" is accepted only once two
// consecutive snapshots are identical and zombie-only. If the view
// keeps changing (something keeps forking), the answer is ALIVE: the
// callers then wait, escalate, or report a leak — every one of which
// is safe, where a wrong "gone" is not.
func groupAliveWith(scan func() (groupSnapshot, bool), signalable func() bool, hidden bool) bool {
	var prev groupSnapshot
	for pass := 0; pass < maxScanPasses; pass++ {
		snap, complete := scan()
		if snap.anyLive() {
			return true
		}
		if !complete || hidden {
			return signalable()
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

// procHidesPIDs reports whether /proc is mounted with hidepid, in which
// case other users' processes are missing from the listing altogether
// (hidepid=2/invisible) rather than present-but-unreadable, and a
// negative scan proves nothing about a worker that changed uid. Any
// value other than 0/off counts; an unreadable mounts file counts too.
func procHidesPIDs(mountsPath string) bool {
	data, err := os.ReadFile(mountsPath) //nolint:gosec // /proc/self/mounts or a test fixture
	if err != nil {
		return true
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		f := bytes.Fields(line)
		if len(f) < 4 || string(f[1]) != "/proc" || string(f[2]) != "proc" {
			continue
		}
		for _, opt := range bytes.Split(f[3], []byte(",")) {
			if v, ok := bytes.CutPrefix(opt, []byte("hidepid=")); ok {
				if s := string(v); s != "0" && s != "off" {
					return true
				}
			}
		}
	}
	return false
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
