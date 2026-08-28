package liveswap

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// groupAlive must not count zombies. When hotserve is PID 1 (the e2e
// container, a Docker image without --init) orphaned workers re-parent
// to it and are never reaped, so kill(-pgid, 0) keeps succeeding
// forever; trusting it would turn every Stop into a full-grace wait.
// An exited child we deliberately leave unreaped plays the orphan.
func TestGroupAliveIgnoresZombies(t *testing.T) {
	cmd := exec.Command("true")
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Wait() }) // reap it after the assertions
	pgid := cmd.Process.Pid

	deadline := time.Now().Add(2 * time.Second)
	for groupAlive(pgid) {
		if time.Now().After(deadline) {
			t.Fatal("groupAlive still true 2s after the only member exited (zombie counted as live)")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The zombie still holds the pgid: the naive check the /proc scan
	// replaces would report the group alive.
	if !groupSignalable(pgid) {
		t.Fatal("unreaped child should still keep its process group signalable")
	}
	if _, err := os.Stat("/proc/" + strconv.Itoa(pgid) + "/stat"); err != nil {
		t.Fatalf("zombie should still be visible in /proc: %v", err)
	}
}

// scanProcGroup must never turn an incomplete view of /proc into a
// confident "gone": a hidepid mount hides a worker that changed uid,
// and release GC would delete files beneath it.
func TestScanProcGroupIncompleteIsNotGone(t *testing.T) {
	mk := func(t *testing.T, root string, pid int, stat string) {
		t.Helper()
		dir := filepath.Join(root, strconv.Itoa(pid))
		must(t, os.MkdirAll(dir, 0o755))
		must(t, os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644))
	}
	cases := []struct {
		name           string
		setup          func(t *testing.T, root string)
		alive, complete bool
	}{
		{"live member", func(t *testing.T, root string) {
			mk(t, root, 200, "200 (node (worker)) S 100 100 100 0 -1\n")
		}, true, true},
		{"zombie only", func(t *testing.T, root string) {
			mk(t, root, 200, "200 (node) Z 100 100 100 0 -1\n")
		}, false, true},
		{"other group only", func(t *testing.T, root string) {
			mk(t, root, 300, "300 (sshd) S 1 300 300 0 -1\n")
		}, false, true},
		{"unreadable entry", func(t *testing.T, root string) {
			mk(t, root, 300, "300 (sshd) S 1 300 300 0 -1\n")
			// stat as a directory: ReadFile fails with something other
			// than not-exist, like a hidepid-protected process.
			must(t, os.MkdirAll(filepath.Join(root, "200", "stat"), 0o755))
		}, false, false},
		{"garbled entry", func(t *testing.T, root string) {
			mk(t, root, 200, "garbage")
		}, false, false},
		{"vanished entry", func(t *testing.T, root string) {
			must(t, os.MkdirAll(filepath.Join(root, "200"), 0o755)) // no stat: exited mid-scan
		}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tc.setup(t, root)
			alive, complete := scanProcGroup(root, 100)
			if alive != tc.alive || complete != tc.complete {
				t.Fatalf("alive=%v complete=%v, want alive=%v complete=%v", alive, complete, tc.alive, tc.complete)
			}
		})
	}
}

// hidepid=2 omits other users' processes from the /proc listing rather
// than making them unreadable, so it must be detected from the mount
// options; anything but 0/off (or an unreadable mounts file) disables
// trust in a negative scan.
func TestProcHidesPIDs(t *testing.T) {
	cases := map[string]bool{
		"proc /proc proc rw,nosuid,nodev,noexec,relatime 0 0\n":                      false,
		"proc /proc proc rw,nosuid,nodev,noexec,relatime,hidepid=0 0 0\n":            false,
		"proc /proc proc rw,relatime,hidepid=off,subset=pid 0 0\n":                   false,
		"proc /proc proc rw,nosuid,nodev,noexec,relatime,hidepid=2,gid=26 0 0\n":     true,
		"proc /proc proc rw,relatime,hidepid=invisible 0 0\n":                        true,
		"proc /proc proc rw,relatime,hidepid=1 0 0\n":                                true,
		"sysfs /sys sysfs rw 0 0\nproc /proc proc rw,hidepid=ptraceable 0 0\n":      true,
		"proc /run/proc-copy proc rw,hidepid=2 0 0\nproc /proc proc rw,relatime 0 0\n": false, // only the /proc mount matters
	}
	for content, want := range cases {
		path := filepath.Join(t.TempDir(), "mounts")
		must(t, os.WriteFile(path, []byte(content), 0o644))
		if got := procHidesPIDs(path); got != want {
			t.Errorf("%q: got %v want %v", content, got, want)
		}
	}
	if !procHidesPIDs(filepath.Join(t.TempDir(), "missing")) {
		t.Error("an unreadable mounts file must be treated conservatively")
	}
}
