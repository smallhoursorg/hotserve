package liveswap

import (
	"fmt"
	"os"
	"path/filepath"
)

// The on-disk layout every other file addresses an app's files
// through. Path arithmetic over one root and one app name, and the one
// check of what those paths resolve to (ownDir): it holds no state,
// takes no lock, and knows nothing about the deploy pipeline, which is
// why it is not in app.go.

// appDirs is the on-disk layout for one app:
//
//	<root>/<app>/releases/<version>/   one dir per deployed version
//	<root>/<app>/shared/               persistent data, survives deploys
//	<root>/<app>/tmp/                  download staging
//	<root>/<app>/run/<nonce>/app.sock  one socket per instance, bound by the app; only run/<nonce>/ is in that instance's view
//	<root>/<app>/proxy/<nonce>.sock    the same inode, hard-linked by hotserve; what it dials (not in any view)
//	<root>/<app>/deploys/<version>.json  the latest deploy's outcome for that version, filtered (not in any view)
//	<root>/<app>/state.json            current version + process handle
//	<root>/<app>/current -> releases/<version>   convenience symlink
type appDirs struct {
	root     string // the liveswap root every app lives under
	app      string
	releases string
	shared   string
	tmp      string
	run      string
	proxy    string
	deploys  string
	state    string
	current  string
}

func newAppDirs(root, name string) appDirs {
	app := filepath.Join(root, name)
	return appDirs{
		root:     root,
		app:      app,
		releases: filepath.Join(app, "releases"),
		shared:   filepath.Join(app, "shared"),
		tmp:      filepath.Join(app, "tmp"),
		run:      filepath.Join(app, "run"),
		proxy:    filepath.Join(app, "proxy"),
		deploys:  filepath.Join(app, "deploys"),
		state:    filepath.Join(app, "state.json"),
		current:  filepath.Join(app, "current"),
	}
}

// runDir is the one directory an instance may bind its socket in: its
// own, so that no other instance of the app — the one it replaces, or
// the one replacing it — can touch the name before hotserve pins it.
func (d appDirs) runDir(nonce string) string {
	return filepath.Join(d.run, nonce)
}

// socket is the path the instance identified by nonce listens on.
func (d appDirs) socket(nonce string) string {
	return filepath.Join(d.runDir(nonce), "app.sock")
}

// socketRef is how hotserve dials that instance: the socket, pinned
// under proxy/.
func (d appDirs) socketRef(nonce string) *socketRef {
	return newSocketRef(d.socket(nonce), filepath.Join(d.proxy, nonce+".sock"), d.runDir(nonce))
}

func (d appDirs) release(version string) string {
	// versionPathComponent: identity for valid tags, mechanical
	// traversal containment (and the analyzer-modeled sanitizer) for
	// anything else — validVersion remains the actual gate upstream.
	return filepath.Join(d.releases, versionPathComponent(version))
}

func (d appDirs) ensure() error {
	// deploys/ is not here: the record store is optional, made on
	// first use by recordsDir, and a failure there is a warning, not a
	// deploy that cannot start.
	for _, dir := range []string{d.releases, d.shared, d.tmp, d.run, d.proxy} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	return nil
}

// ownDir refuses dir — the app dir, or a directory directly under it —
// unless it resolves to that same path under the canonical root. A
// link at the app dir or at dir lands whatever is done there in
// another directory: `<root>/blog -> <root>/shop` would have blog's
// socket prune remove shop's live socket and pin (pruneSockets) and
// blog's deploys list, serve and prune shop's records (recordsDir).
// The rule is the bind sources' (resolveBindSources, sandbox.go):
// canonical against canonical, with an alias on the liveswap root
// itself the one difference allowed. A directory not made yet passes
// when the app dir does, since canonicalDeepest resolves as far as the
// path exists.
func (d appDirs) ownDir(dir string) error {
	want := filepath.Join(canonicalDeepest(d.root), filepath.Base(d.app))
	if got := canonicalDeepest(d.app); got != want {
		return fmt.Errorf("%s resolves to %s (a planted link is not followed)", d.app, got)
	}
	if dir == d.app {
		return nil
	}
	want = filepath.Join(want, filepath.Base(dir))
	if got := canonicalDeepest(dir); got != want {
		return fmt.Errorf("%s resolves to %s, not %s (a planted link is not followed)", dir, got, want)
	}
	return nil
}

// canonicalDeepest is EvalSymlinks for a path whose leaf may not exist
// yet: it resolves the deepest ancestor that does and re-appends the
// rest unchanged, falling back to p when nothing resolves.
//
// EvalSymlinks fails outright on a missing final component, and an
// env_file is explicitly allowed to be absent until the first deploy —
// so resolving the whole path would silently give back only the
// lexical spelling for exactly the configurations that need checking.
// With `/safe/link -> /var/lib/liveswap/blog/shared` and
// `env_file /safe/link/shop.env` not yet created, blog's own bind
// exposes that file the moment it appears, while a lexical comparison
// sees two unrelated strings. The link is in the PARENT, which is why the
// deepest existing ancestor is the thing to resolve.
func canonicalDeepest(p string) string {
	rest := ""
	for cur := p; ; {
		if c, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(c, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p // reached the root without resolving anything
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}
