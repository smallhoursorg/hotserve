package liveswap

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// warnedOnce fails t unless logs hold exactly one prune warning,
// naming path and appDir, carrying why (a snippet of the error) and
// the remedy.
func warnedOnce(t *testing.T, logs *observer.ObservedLogs, path, appDir, why string) {
	t.Helper()
	got := logs.All()
	if len(got) != 1 {
		t.Fatalf("want one warning, got %v", got)
	}
	f := got[0].ContextMap()
	if f["path"] != path || f["app_dir"] != appDir || !strings.Contains(fmt.Sprint(f["error"]), why) || !strings.Contains(fmt.Sprint(f["fix"]), "bind-mount") {
		t.Errorf("want a warning naming %s (%s), the app dir %s and the remedy, got %v", path, why, appDir, f)
	}
}

// An app dir, or its run/ or proxy/, that is a link to a sibling's —
// `<root>/blog -> shop` — is not blog's to prune: a recovery, sweep or
// removal of blog removes nothing through it, so shop's live socket
// and pin survive, and one warning names the link and the remedy.
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
			warnedOnce(t, logs, want, filepath.Join(root, "blog"), "is a link")
		})
	}
}

// A dangling link is refused too, rather than passed as the name it
// stands in for: once its target appears — shop's first launch making
// the run/ blog's link names — blog's prune still does not follow it.
func TestPruneSocketsRefusesADanglingLink(t *testing.T) {
	root := t.TempDir()
	blog := newAppDirs(root, "blog")
	must(t, blog.ensure())
	must(t, os.Remove(blog.run))
	must(t, os.Symlink("../shop/run", blog.run)) // shop has no run/ yet
	for _, step := range []string{"dangling", "landed"} {
		var runDir string
		if step == "landed" {
			runDir, _ = staleInstance(t, newAppDirs(root, "shop"), linkTestNonce)
		}
		core, logs := observer.New(zap.WarnLevel)
		pruneSockets(blog, nil, zap.New(core))
		if runDir != "" {
			if _, err := os.Lstat(runDir); err != nil {
				t.Errorf("shop's %s removed through blog's link once it landed: %v", runDir, err)
			}
		}
		warnedOnce(t, logs, blog.run, blog.app, "is a link")
	}
}

// What is not the directory it names, or cannot be checked for a link,
// is not pruned through either: a file stands where a dir should be,
// or Lstat fails, after which reads could still succeed through a
// link the check missed (EIO, which cannot be made on demand;
// ENAMETOOLONG takes the same branch). The warning names the path at
// fault, not one below it.
func TestPruneSocketsSkipsWhatItCannotCheck(t *testing.T) {
	for name, tc := range map[string]struct {
		app  string
		make func(t *testing.T, d appDirs) string // returns the path at fault
		why  string
	}{
		"file at the app dir": {"blog", func(t *testing.T, d appDirs) string {
			must(t, os.WriteFile(d.app, nil, 0o600))
			return d.app
		}, "not a directory"},
		"file at run": {"blog", func(t *testing.T, d appDirs) string {
			must(t, d.ensure())
			must(t, os.Remove(d.run))
			must(t, os.WriteFile(d.run, nil, 0o600))
			return d.run
		}, "not a directory"},
		"lstat fails": {strings.Repeat("a", 256), func(_ *testing.T, d appDirs) string {
			return d.app // a component past NAME_MAX: ENAMETOOLONG, not ENOENT
		}, "cannot be checked"},
	} {
		t.Run(name, func(t *testing.T) {
			d := newAppDirs(t.TempDir(), tc.app)
			want := tc.make(t, d)
			core, logs := observer.New(zap.WarnLevel)
			pruneSockets(d, nil, zap.New(core))
			warnedOnce(t, logs, want, d.app, tc.why)
		})
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
		// The refusal recover() repeats, reached after the sweep: a
		// failure before it would leave shop alone without the fix.
		err := rig.ma.ensureRunning()
		if !transientRecovery(err) || !strings.Contains(fmt.Sprint(err), "refusing to launch sandboxed") {
			t.Fatalf("attempt %d: want the transient bind refusal, got %v", attempt, err)
		}
		if n := len(rig.runner.sweeps); n != attempt {
			t.Fatalf("attempt %d: %d sweeps; the prune under test was not reached", attempt, n)
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
