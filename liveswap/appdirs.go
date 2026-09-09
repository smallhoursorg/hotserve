package liveswap

import (
	"os"
	"path/filepath"
)

// The on-disk layout every other file addresses an app's files
// through. Pure path arithmetic over one root and one app name: it
// holds no state, takes no lock, and knows nothing about the deploy
// pipeline, which is why it is not in app.go.

// appDirs is the on-disk layout for one app:
//
//	<root>/<app>/releases/<version>/   one dir per deployed version
//	<root>/<app>/shared/               persistent data, survives deploys
//	<root>/<app>/tmp/                  download staging
//	<root>/<app>/run/<nonce>/app.sock  one socket per instance, bound by the app; only run/<nonce>/ is in that instance's view
//	<root>/<app>/proxy/<nonce>.sock    the same inode, hard-linked by hotserve; what it dials (not in any view)
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
	for _, dir := range []string{d.releases, d.shared, d.tmp, d.run, d.proxy} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	return nil
}
