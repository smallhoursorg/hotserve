package liveswap

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// staleInstance makes the run dir and pin a dead instance leaves.
func staleInstance(t *testing.T, d appDirs, nonce string) (runDir, pin string) {
	t.Helper()
	if err := d.ensure(); err != nil {
		t.Fatal(err)
	}
	runDir, pin = d.runDir(nonce), filepath.Join(d.proxy, nonce+".sock")
	if err := os.Mkdir(runDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pin, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return runDir, pin
}

const linkTestNonce = "0123456789abcdef"

// An app dir, or its run/ or proxy/, that is a link to a sibling's —
// `<root>/blog -> shop` — is not blog's to prune: a recovery, sweep or
// removal of blog removes nothing through it, so shop's live socket
// and pin survive, and one warning names the link. A dangling link is
// refused as well: it is a link, whatever it resolves to later.
func TestPruneSocketsDoesNotFollowALink(t *testing.T) {
	// Each case makes blog's link under root and returns where it is.
	linkBlog := func(t *testing.T, root string, subs map[string]string) string {
		t.Helper()
		blog := newAppDirs(root, "blog")
		if err := blog.ensure(); err != nil {
			t.Fatal(err)
		}
		first := ""
		for _, sub := range []string{"run", "proxy"} {
			target, ok := subs[sub]
			if !ok {
				continue
			}
			p := filepath.Join(blog.app, sub)
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, p); err != nil {
				t.Fatal(err)
			}
			if first == "" {
				first = p
			}
		}
		return first
	}
	for name, link := range map[string]func(t *testing.T, root string) string{
		"app dir": func(t *testing.T, root string) string {
			p := filepath.Join(root, "blog")
			if err := os.Symlink("shop", p); err != nil {
				t.Fatal(err)
			}
			return p
		},
		"run and proxy": func(t *testing.T, root string) string {
			return linkBlog(t, root, map[string]string{"run": "../shop/run", "proxy": "../shop/proxy"})
		},
		// The inverse direction: proxy/ is no bind source, so blog
		// launches, and its prune is what would reach shop's pins.
		"proxy only": func(t *testing.T, root string) string {
			return linkBlog(t, root, map[string]string{"proxy": "../shop/proxy"})
		},
		"dangling": func(t *testing.T, root string) string {
			return linkBlog(t, root, map[string]string{"run": "../shop/gone"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			runDir, pin := staleInstance(t, newAppDirs(root, "shop"), linkTestNonce)
			want := link(t, root)
			core, logs := observer.New(zap.WarnLevel)
			pruneSockets(newAppDirs(root, "blog"), nil, zap.New(core))
			for _, p := range []string{runDir, pin} {
				if _, err := os.Lstat(p); err != nil {
					t.Errorf("shop's %s removed through blog's link: %v", p, err)
				}
			}
			got := logs.All()
			if len(got) != 1 || got[0].ContextMap()["path"] != want || got[0].ContextMap()["app_dir"] != filepath.Join(root, "blog") {
				t.Errorf("want one warning naming the link %s and the app dir, got %v", want, got)
			}
		})
	}
}

// A dir that cannot be checked for a link fails closed: the reads
// after a failed Lstat could succeed through the link it missed, so
// the prune is skipped with a warning naming the path and the error.
// A transient EIO cannot be made on demand; ENOTDIR (a file where the
// app dir should be, so Lstat of its run/ fails) takes the same branch.
func TestPruneSocketsSkipsWhatItCannotCheck(t *testing.T) {
	root := t.TempDir()
	blog := newAppDirs(root, "blog")
	must(t, os.WriteFile(blog.app, nil, 0o600))
	core, logs := observer.New(zap.WarnLevel)
	pruneSockets(blog, nil, zap.New(core))
	got := logs.All()
	if len(got) != 1 || got[0].ContextMap()["path"] != blog.run || got[0].ContextMap()["error"] == nil {
		t.Errorf("want one warning naming %s and the error, got %v", blog.run, got)
	}
}

// The directory it names is pruned as before, through an alias on the
// root itself too (/srv/liveswap -> /mnt/liveswap): the kept nonce
// stays, the stale one goes, and nothing is warned.
func TestPruneSocketsPrunesTheDirsItNames(t *testing.T) {
	const stale, live = "0123456789abcdef", "fedcba9876543210"
	for name, alias := range map[string]bool{"plain root": false, "aliased root": true} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if alias {
				target := filepath.Join(root, "real")
				if err := os.Mkdir(target, 0o750); err != nil {
					t.Fatal(err)
				}
				root = filepath.Join(root, "alias")
				if err := os.Symlink(target, root); err != nil {
					t.Fatal(err)
				}
			}
			d := newAppDirs(root, "shop")
			staleRun, stalePin := staleInstance(t, d, stale)
			liveRun, livePin := staleInstance(t, d, live)
			core, logs := observer.New(zap.WarnLevel)
			pruneSockets(d, []string{live}, zap.New(core))
			for _, p := range []string{staleRun, stalePin} {
				if _, err := os.Lstat(p); !os.IsNotExist(err) {
					t.Errorf("stale %s not pruned: %v", p, err)
				}
			}
			for _, p := range []string{liveRun, livePin} {
				if _, err := os.Lstat(p); err != nil {
					t.Errorf("kept %s removed: %v", p, err)
				}
			}
			if n := logs.Len(); n != 0 {
				t.Errorf("want no warning, got %v", logs.All())
			}
		})
	}
}

// bindCheckingRunner is the fake runner with the real launch-time
// refusal in front: Start and Preflight resolve the bind sources
// first, as resolveInView does. The plain fake launches through a
// linked app dir, which no real launch does.
type bindCheckingRunner struct{ *fakeRunner }

func (r bindCheckingRunner) Start(spec startSpec) (handle, error) {
	if err := spec.sandbox.resolveBindSources(); err != nil {
		return nil, err
	}
	return r.fakeRunner.Start(spec)
}

func (r bindCheckingRunner) Preflight(spec startSpec) error {
	if err := spec.sandbox.resolveBindSources(); err != nil {
		return err
	}
	return r.fakeRunner.Preflight(spec)
}

// Recovery of an app whose dir is a link to a serving sibling's
// (#300): it reads the sibling's state.json, relaunches that version,
// sweeps first, and is refused at launch. The refusal is transient, so
// recover() repeats all of it about once a minute; each attempt's
// sweep must leave the sibling's live socket and pin alone. The
// sibling's state.json is never written either, since a save follows
// only a launch.
func TestRecoveryThroughALinkedAppDirLeavesTheSiblingServing(t *testing.T) {
	rig := newTestRig(t)
	shop := newAppDirs(rig.spec.dirs.root, "shop")
	runDir, pin := staleInstance(t, shop, linkTestNonce)
	must(t, os.MkdirAll(shop.release("v1"), 0o750))
	must(t, (&fileStateStore{path: shop.state}).save(appState{
		CurrentVersion: "v1", Nonce: linkTestNonce,
		Handle: handleState{Unit: "hotserve-shop.v1." + linkTestNonce + ".service"},
	}))
	state, err := os.ReadFile(shop.state)
	must(t, err)
	must(t, os.Symlink("shop", rig.spec.dirs.app)) // <root>/demo -> shop
	rig.ma.store = &fileStateStore{path: rig.spec.dirs.state}
	rig.ma.runner = bindCheckingRunner{rig.runner}
	for attempt := 1; attempt <= 2; attempt++ {
		if err := rig.ma.ensureRunning(); err == nil {
			t.Fatalf("attempt %d: recovery through a linked app dir launched", attempt)
		}
		for _, p := range []string{runDir, pin} {
			if _, err := os.Lstat(p); err != nil {
				t.Fatalf("attempt %d: shop's %s removed by demo's recovery: %v", attempt, p, err)
			}
		}
	}
	if got, err := os.ReadFile(shop.state); err != nil || !bytes.Equal(got, state) {
		t.Errorf("shop's state.json changed by demo's recovery: %v", err)
	}
}
