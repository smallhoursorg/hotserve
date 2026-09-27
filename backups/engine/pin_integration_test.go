//go:build integration

package engine

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/smallhoursorg/hotserve/backups/unit"
)

// The app flips its declared path between the real directory and a link
// to a sibling's, as fast as it can, while real units start — the race
// a check-then-bind loses, and that handing the manager /proc/<pid>/fd
// loses too, since the manager walks that link's text again. What a
// unit is shown must be the app's own directory every time, and never
// the sibling's.
func TestIntegrationWhatIsBoundIsWhatWasPinnedWhateverTheAppDoesToTheName(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root, mount(2) and a system manager")
	}
	// Its own account: a fresh box has none, and the packages run in an
	// order that does not make one first.
	if exec.Command("id", "hotserve-backup").Run() != nil {
		if out, err := exec.Command("useradd", "--system", "--no-create-home", "--shell", "/usr/sbin/nologin", "hotserve-backup").CombinedOutput(); err != nil {
			t.Fatalf("useradd: %v: %s", err, out)
		}
	}
	r, err := unit.NewSystemRunner(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	base, err := os.MkdirTemp("/root", "pin-race-")
	must(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	shared, sibling, mounts := filepath.Join(base, "blog", "shared"), filepath.Join(base, "shop", "shared"), filepath.Join(base, "run")
	for _, d := range []string{filepath.Join(shared, "uploads"), sibling, mounts} {
		must(t, os.MkdirAll(d, 0o755))
	}
	must(t, os.WriteFile(filepath.Join(shared, "uploads", "marker"), []byte("mine\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(sibling, "marker"), []byte("THE SIBLING'S\n"), 0o644))

	var stop atomic.Bool
	toggled := make(chan int, 1)
	go func() {
		name, aside := filepath.Join(shared, "uploads"), filepath.Join(shared, "uploads.aside")
		n := 0
		for !stop.Load() {
			if os.Rename(name, aside) == nil {
				_ = os.Symlink(sibling, name)
				_ = os.Remove(name)
				_ = os.Rename(aside, name)
				n++
			}
		}
		toggled <- n
	}()

	root, err := pinRoot(base)
	must(t, err)
	defer root.close()
	sharedPin, err := root.beneath("blog/shared")
	must(t, err)
	defer sharedPin.close()

	units := 60
	if n, err := strconv.Atoi(os.Getenv("PIN_RACE_UNITS")); err == nil {
		units = n
	}
	shown, refused := 0, 0
	for i := 0; shown < units && i < 100*units; i++ {
		item, err := sharedPin.beneath("uploads")
		if err != nil { // caught mid-flip: a link, or nothing — refused, which is fine
			refused++
			continue
		}
		target := filepath.Join(mounts, "m"+strconv.Itoa(shown))
		unmount, err := item.mountAt(target)
		if err != nil {
			item.close()
			t.Fatalf("mounting the pin: %v", err)
		}
		out := filepath.Join(base, "stdout")
		o, err := r.Run(context.Background(), unit.Spec{
			Name: "hotserve_backup_test_pinrace_" + strconv.Itoa(shown) + ".service", User: "hotserve-backup",
			Capabilities: []unit.Capability{unit.CapDACReadSearch},
			Argv:         []string{"/bin/cat", "/backup/x/files/uploads/marker"},
			Binds:        []unit.Bind{{Source: target, Dest: "/backup/x/files/uploads"}}, StdoutFile: out,
		})
		unmount()
		item.close()
		if err != nil || !o.OK() {
			t.Fatalf("run %d: %+v, %v", shown, o, err)
		}
		got, _ := os.ReadFile(out)
		if string(got) != "mine\n" {
			t.Fatalf("run %d: the unit was shown %q", shown, got)
		}
		shown++
	}
	stop.Store(true)
	n := <-toggled
	if shown < units || n < 100 {
		t.Fatalf("%d units shown the directory, %d flips (%d pins refused mid-flip): not enough of a race to mean anything", shown, n, refused)
	}
	t.Logf("%d units, %d flips of the name meanwhile, %d pins refused mid-flip", shown, n, refused)
}

// What a run binds and takes away is its own, and nothing of the
// app's goes with it. On a box systemd has booted the root mount is
// shared — in a container it is not, which is why no lane saw this —
// and a recursive bind of a shared mount is a peer of what it was
// bound from: unmounting the disk beneath the bind unmounted, by
// propagation, the operator's disk beneath the app's own directory,
// under the live app, at the end of every run [M71]. Staged on a
// shared mount of the test's own: the disk stays where the operator
// put it, through a bind taken away, and through the sweep of one a
// killed run of an earlier version left shared.
func TestIntegrationTakingABindAwayLeavesTheAppsDiskMounted(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root and mount(2)")
	}
	base, err := os.MkdirTemp("/root", "propagation-")
	must(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	sh := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
	// A box's root, as systemd leaves it: shared.
	sh("mount", "-t", "tmpfs", "tmpfs", base)
	t.Cleanup(func() { _ = exec.Command("umount", "-R", "-l", base).Run() })
	sh("mount", "--make-rshared", base)
	shared, disk, run := filepath.Join(base, "blog", "shared"), filepath.Join(base, "blog", "shared", "uploads", "disk"), filepath.Join(base, "run")
	must(t, os.MkdirAll(disk, 0o755))
	must(t, os.MkdirAll(run, 0o700))
	sh("mount", "-t", "tmpfs", "tmpfs", disk)
	must(t, os.WriteFile(filepath.Join(disk, "kept"), []byte("the operator's\n"), 0o644))
	mounted := func(when string) {
		t.Helper()
		if _, err := os.Stat(filepath.Join(disk, "kept")); err != nil {
			t.Fatalf("%s, the disk inside the app's own directory was unmounted with it: %v", when, err)
		}
	}
	mounted("before anything")

	p, err := pinRoot(shared)
	must(t, err)
	defer p.close()
	unmount, err := p.mountAt(filepath.Join(run, "mount-1"))
	must(t, err)
	if _, err := os.Stat(filepath.Join(run, "mount-1", "uploads", "disk", "kept")); err != nil {
		t.Fatalf("the disk did not come along with the bind: %v", err)
	}
	unmount()
	mounted("a run's bind taken away")

	// As an earlier version bound it, and a killed run left it.
	left := filepath.Join(run, "mount-2")
	must(t, os.Mkdir(left, 0o700))
	sh("mount", "--rbind", shared, left)
	must(t, unmountDetach(left))
	mounted("a bind an earlier version left, swept")
	under, err := mountsUnder(run)
	must(t, err)
	if len(under) != 0 {
		t.Fatalf("still mounted under the run directory: %q", under)
	}
}

// Where a bind cannot be made private it is not taken away as it is:
// detached while it is still a peer of what it was bound from, it
// takes the app's disk with it, which is what making it private is
// for (Copilot on #155). It is left, and said; a later sweep that can
// make it private takes it away. A bind with nothing mounted beneath
// it has nothing to take, and goes.
func TestIntegrationABindThatCannotBeMadePrivateIsNotDetached(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root and mount(2)")
	}
	base, err := os.MkdirTemp("/root", "propagation-closed-")
	must(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	sh := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
	sh("mount", "-t", "tmpfs", "tmpfs", base)
	t.Cleanup(func() { _ = exec.Command("umount", "-R", "-l", base).Run() })
	sh("mount", "--make-rshared", base)
	shared, disk, run := filepath.Join(base, "blog", "shared"), filepath.Join(base, "blog", "shared", "uploads", "disk"), filepath.Join(base, "run")
	must(t, os.MkdirAll(disk, 0o755))
	must(t, os.MkdirAll(filepath.Join(shared, "plain"), 0o755))
	must(t, os.MkdirAll(run, 0o700))
	sh("mount", "-t", "tmpfs", "tmpfs", disk)
	must(t, os.WriteFile(filepath.Join(disk, "kept"), []byte("the operator's\n"), 0o644))
	mounted := func(when string) {
		t.Helper()
		if _, err := os.Stat(filepath.Join(disk, "kept")); err != nil {
			t.Fatalf("%s, the disk inside the app's own directory was unmounted: %v", when, err)
		}
	}
	old := private
	private = func(string) error { return errors.New("operation not permitted") }
	t.Cleanup(func() { private = old })

	p, err := pinRoot(shared)
	must(t, err)
	defer p.close()
	// The disk first, whatever was said: it is what must not happen.
	_, err = p.mountAt(filepath.Join(run, "mount-1"))
	mounted("a bind that could not be made private, refused")
	if err == nil || !strings.Contains(err.Error(), "could not be made private") {
		t.Fatalf("a bind that could not be made private: err = %v", err)
	}

	// One an earlier version left, shared: the sweep refuses it, and
	// it is still there for a sweep that can do better.
	left := filepath.Join(run, "mount-2")
	must(t, os.Mkdir(left, 0o700))
	sh("mount", "--rbind", shared, left)
	err = unmountDetach(left)
	mounted("a bind that could not be made private, swept")
	if err == nil || !strings.Contains(err.Error(), "could not be made private") {
		t.Fatalf("the sweep of a bind that could not be made private: err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(left, "uploads", "disk", "kept")); err != nil {
		t.Fatalf("the bind was taken away all the same: %v", err)
	}
	private = old
	must(t, unmountDetach(left))
	mounted("the same bind, swept once it could be made private")

	// With nothing mounted beneath, there is nothing to take along.
	private = func(string) error { return errors.New("operation not permitted") }
	plain, err := pinRoot(filepath.Join(shared, "plain"))
	must(t, err)
	defer plain.close()
	if _, err := plain.mountAt(filepath.Join(run, "mount-3")); err == nil {
		t.Fatal("a bind that could not be made private was kept")
	}
	under, err := mountsUnder(run)
	must(t, err)
	for _, m := range under {
		if strings.HasSuffix(m, "mount-3") {
			t.Fatalf("a bind with nothing beneath it, refused, was left mounted: %q", under)
		}
	}
}
