//go:build integration

package engine

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/smallhoursorg/hotserve/backups/restore"
	"github.com/smallhoursorg/hotserve/backups/unit"
)

// Both apps run as one uid, so the unit that installs a restore could
// write a sibling's files. The app flips the directory restored into,
// and the database restored over, between the real thing and a link to
// the sibling's, as fast as it can, while real install units run — as
// the engine starts them: the pinned shared dir bound at /target, the
// data user in its own namespaces. However each restore ends, the
// sibling's data is never written.
func TestIntegrationARestoreNeverLandsInASiblingsDataWhateverTheAppDoesToTheNames(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root, mount(2) and a system manager")
	}
	for _, account := range []string{dataUser, backupUser} {
		if exec.Command("id", account).Run() != nil {
			if out, err := exec.Command("useradd", "--system", "--no-create-home", "--shell", "/usr/sbin/nologin", account).CombinedOutput(); err != nil {
				t.Fatalf("useradd: %v: %s", err, out)
			}
		}
	}
	// The command the unit runs, where a unit's view holds it.
	self := "/usr/local/bin/hotserve-backup-restore-race"
	if out, err := exec.Command("go", "build", "-o", self, "../cmd/hotserve-backup").CombinedOutput(); err != nil {
		t.Fatalf("building the command: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = os.Remove(self) })
	r, err := unit.NewSystemRunner(context.Background())
	must(t, err)
	defer r.Close()

	base, err := os.MkdirTemp("/root", "restore-race-")
	must(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	must(t, os.Chmod(base, 0o755))
	shared, sibling, fetched, mounts := filepath.Join(base, "root", "blog", "shared"), filepath.Join(base, "root", "shop", "shared"), filepath.Join(base, "fetched"), filepath.Join(base, "run")
	for _, d := range []string{filepath.Join(shared, "uploads"), sibling, filepath.Join(fetched, "sqlite"), filepath.Join(fetched, "files", "uploads"), mounts} {
		must(t, os.MkdirAll(d, 0o755))
	}
	sqlite := func(db, statements string) string {
		out, err := exec.Command("/usr/bin/sqlite3", db, statements).CombinedOutput()
		if err != nil {
			t.Fatalf("sqlite3 %s: %v: %s", db, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	must(t, os.WriteFile(filepath.Join(fetched, "plan.json"), []byte(`{"sqlite":["app.db"],"files":["uploads"]}`), 0o644))
	must(t, os.WriteFile(filepath.Join(fetched, "files", "uploads", "restored.png"), []byte("blog's"), 0o644))
	sqlite(filepath.Join(fetched, "sqlite", "app.db"), "create table posts(n); insert into posts values (1),(2);")
	sqlite(filepath.Join(shared, "app.db"), "create table posts(n); insert into posts values (7);")
	sqlite(filepath.Join(sibling, "shop.db"), "create table posts(n); insert into posts values (41);")
	must(t, os.WriteFile(filepath.Join(sibling, "r.txt"), []byte("THE SIBLING'S\n"), 0o644))
	if out, err := exec.Command("chown", "-R", dataUser+":", filepath.Join(base, "root"), fetched).CombinedOutput(); err != nil {
		t.Fatalf("chown: %v: %s", err, out)
	}

	var stop atomic.Bool
	toggled := make(chan int, 1)
	go func() {
		n := 0
		flip := func(name, aside, to string) {
			if os.Rename(name, aside) == nil {
				_ = os.Symlink(to, name)
				_ = os.Remove(name)
				_ = os.Rename(aside, name)
				n++
			}
		}
		for !stop.Load() {
			flip(filepath.Join(shared, "uploads"), filepath.Join(shared, "uploads.aside"), "../../shop/shared")
			flip(filepath.Join(shared, "app.db"), filepath.Join(shared, "app.db.aside"), "../../shop/shared/shop.db")
		}
		toggled <- n
	}()

	root, err := pinRoot(filepath.Join(base, "root"))
	must(t, err)
	defer root.close()
	sharedPin, err := root.beneath("blog/shared")
	must(t, err)
	defer sharedPin.close()

	units := 40
	if n, err := strconv.Atoi(os.Getenv("PIN_RACE_UNITS")); err == nil {
		units = n
	}
	installed, refused := 0, 0
	for i := 0; i < units; i++ {
		target := filepath.Join(mounts, "m"+strconv.Itoa(i))
		unmount, err := sharedPin.mountAt(target)
		must(t, err)
		out := filepath.Join(base, "stdout")
		o, err := r.Run(context.Background(), unit.Spec{
			Name: "hotserve_backup_test_restorerace_" + strconv.Itoa(i) + ".service",
			Argv: []string{self, "install"},
			User: dataUser, SameUIDNamespaces: true,
			Binds:      []unit.Bind{{Source: fetched, Dest: "/restore"}, {Source: target, Dest: "/target", Writable: true}},
			StdoutFile: out,
		})
		unmount()
		if err != nil || !o.OK() {
			t.Fatalf("unit %d: %+v, %v", i, o, err)
		}
		raw, _ := os.ReadFile(out)
		var answer restore.Answer
		if err := json.Unmarshal(raw, &answer); err != nil || answer.Error != "" || len(answer.Items) != 2 {
			t.Fatalf("unit %d said %q (%v)", i, raw, err)
		}
		if answer.Items[0].Class == restore.OK && answer.Items[1].Class == restore.OK {
			installed++
		} else {
			refused++
		}
		// Whatever that one did, the sibling's data is as it was.
		if got, _ := os.ReadFile(filepath.Join(sibling, "r.txt")); string(got) != "THE SIBLING'S\n" {
			t.Fatalf("unit %d: the sibling's file now holds %q", i, got)
		}
		if entries, _ := os.ReadDir(sibling); len(entries) > 4 { // shop.db, its two sidecars at most, r.txt
			t.Fatalf("unit %d: the sibling's data dir now holds %v", i, entries)
		}
		if _, err := os.Lstat(filepath.Join(sibling, "restored.png")); err == nil {
			t.Fatalf("unit %d: blog's file landed in the sibling's data", i)
		}
	}
	stop.Store(true)
	n := <-toggled
	if got := sqlite(filepath.Join(sibling, "shop.db"), "select group_concat(n) from posts"); got != "41" {
		t.Fatalf("the sibling's database was restored over: %q", got)
	}
	if n < 100 || installed+refused < units {
		t.Fatalf("%d units (%d installed, %d refused mid-flip), %d flips: not enough of a race to mean anything", installed+refused, installed, refused, n)
	}
	t.Logf("%d units: %d installed, %d refused mid-flip; %d flips of the names meanwhile", units, installed, refused, n)
}
