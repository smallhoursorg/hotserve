package liveswap

import (
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
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

// An app dir, or its run/ or proxy/, that is a link to a sibling's —
// `<root>/blog -> shop` — is not blog's to prune: a recovery or sweep
// of blog removes nothing through it, so shop's live socket and pin
// survive.
func TestPruneSocketsDoesNotFollowALink(t *testing.T) {
	const nonce = "0123456789abcdef"
	for name, link := range map[string]func(t *testing.T, root string){
		"app dir": func(t *testing.T, root string) {
			if err := os.Symlink("shop", filepath.Join(root, "blog")); err != nil {
				t.Fatal(err)
			}
		},
		"run and proxy": func(t *testing.T, root string) {
			blog := newAppDirs(root, "blog")
			if err := os.Mkdir(blog.app, 0o750); err != nil {
				t.Fatal(err)
			}
			for _, d := range []string{"run", "proxy"} {
				if err := os.Symlink(filepath.Join("..", "shop", d), filepath.Join(blog.app, d)); err != nil {
					t.Fatal(err)
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			runDir, pin := staleInstance(t, newAppDirs(root, "shop"), nonce)
			link(t, root)
			pruneSockets(newAppDirs(root, "blog"), nil, zap.NewNop())
			for _, p := range []string{runDir, pin} {
				if _, err := os.Lstat(p); err != nil {
					t.Errorf("shop's %s removed through blog's link: %v", p, err)
				}
			}
		})
	}
}

// The directory it names is pruned as before, through an alias on the
// root itself too (/srv/liveswap -> /mnt/liveswap): the kept nonce
// stays, the stale one goes.
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
			pruneSockets(d, []string{live}, zap.NewNop())
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
		})
	}
}
