package liveswap

import (
	"path/filepath"
	"strings"
	"testing"
)

// state.json is hotserve's own file, but it is a file, and the nonce
// it records becomes a path component under run/ and proxy/. The
// property: whatever the recorded string, either nonceRe refuses it
// (and ensureRunning derives no path from it) or every path derived
// from it is exactly the app's own — never outside run/<nonce>/ and
// proxy/, never a traversal.
func FuzzRecordedNonce(f *testing.F) {
	for _, s := range []string{
		"0a1b2c3d0a1b2c3d", strings.Repeat("f", 16), "../../target", "", ".", "..",
		"0A1B2C3D0A1B2C3D", "0a1b2c3d0a1b2c3d/x", "0a1b2c3d0a1b2c3d\x00", strings.Repeat("f", 15),
		strings.Repeat("f", 17), "/etc/passwd", "0a1b2c3d0a1b2c3d..", "..0a1b2c3d0a1b2c3d",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, nonce string) {
		d := newAppDirs("/var/lib/liveswap", "blog")
		valid := nonceRe.MatchString(nonce)
		if want := len(nonce) == 16 && strings.Trim(nonce, "0123456789abcdef") == ""; valid != want {
			t.Fatalf("nonceRe(%q) = %v, want %v: the nonce alphabet is 16 lowercase hex digits and nothing else", nonce, valid, want)
		}
		if !valid {
			return
		}
		ref := d.socketRef(nonce)
		if ref.dir != filepath.Join(d.run, nonce) || ref.path != filepath.Join(ref.dir, "app.sock") ||
			ref.pinned != filepath.Join(d.proxy, nonce+".sock") {
			t.Fatalf("nonce %q derives %+v", nonce, ref)
		}
		for _, p := range []string{ref.dir, ref.path, ref.pinned} {
			if !pathWithin(p, d.app) || filepath.Clean(p) != p {
				t.Fatalf("nonce %q derives %s, outside the app's own directory %s", nonce, p, d.app)
			}
		}
	})
}
