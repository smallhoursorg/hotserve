//go:build integration

package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
