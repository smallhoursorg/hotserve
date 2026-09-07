package liveswap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"go.uber.org/zap"
)

// unixSocketPathMax is the longest socket path Linux accepts: sun_path
// is 108 bytes including the terminating NUL.
const unixSocketPathMax = 107

// nonceRe is the instance identifier's shape: 64 random bits, so a
// pinned name under proxy/ can be promised never to recur (a collision
// takes on the order of 2^32 launches of one app) while the longest
// default-root path still fits sun_path.
var nonceRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

// newNonce returns the identifier one instance is known by. It names
// both the unit and the socket, which is what lets a reattach check
// the recorded socket against the recorded unit.
func newNonce() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// socketRef is one instance's socket as hotserve dials it. The app
// binds path, under run/, which the app can write; hotserve never
// dials that name. The first successful dial verifies it is a socket
// (a symlink is refused, not followed) and hard-links the inode to
// pinned, under proxy/, which no app's view contains — and every dial
// from then on, the proxy's and the prober's alike, goes to pinned.
// What the app does to its own name afterwards changes nothing; the
// consequence for apps is to bind the socket once per instance, since
// an instance that unlinks and re-binds its path is dialling nobody
// as far as hotserve is concerned and the watchdog restarts it. The
// pinned name is unique per instance and never reused, so a request
// that dials it after the instance is retired gets nobody — never
// another app.
type socketRef struct {
	path   string // what the app binds
	pinned string // what hotserve dials
	dir    string // the instance's own run dir, removed with the pin; "" = none

	mu      sync.Mutex
	linked  bool
	retired bool
}

func newSocketRef(path, pinned, dir string) *socketRef {
	return &socketRef{path: path, pinned: pinned, dir: dir}
}

// dial returns the address to connect to, pinning the socket on first
// use, or why the socket cannot be dialled.
func (s *socketRef) dial() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retired {
		return "", fmt.Errorf("instance on %s is gone", s.path)
	}
	if !s.linked {
		if st, err := os.Lstat(s.pinned); err == nil && st.Mode()&os.ModeSocket != 0 {
			// Pinned by an earlier hotserve: a reattach finds the
			// link it left, which is the inode the unit still serves.
			s.linked = true
			return s.pinned, nil
		}
		_ = os.Remove(s.pinned)
		if err := linkSocket(s.path, s.pinned); err != nil {
			return "", err
		}
		s.linked = true
	}
	return s.pinned, nil
}

// retire removes the pinned name, and the instance's run dir, once the
// instance is stopped; a later dial is refused rather than re-pinning
// a name nobody owns. What cannot be removed is left for the next
// confirmed sweep's prune — harmless meanwhile, since nothing routes
// to it and the name is never reused.
func (s *socketRef) retire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retired = true
	_ = os.Remove(s.pinned)
	if s.dir != "" {
		_ = os.RemoveAll(s.dir)
	}
}

// dialUnix returns a DialContext that reaches the pinned socket
// whatever address the request names: the URL host is a placeholder,
// the socket is the instance.
func dialUnix(s *socketRef) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		addr, err := s.dial()
		if err != nil {
			return nil, err
		}
		var d net.Dialer
		return d.DialContext(ctx, "unix", addr)
	}
}

// ownerLock is the per-app lock that serializes deciding an app's
// socket dirs belong to nobody — and pruning them — with recovery
// adopting or relaunching that app: sweepUnknownApps holds it across
// its ownership check and prune, ensureRunning across the whole of
// recovery. A prune therefore either completes before a reload's
// recovery touches the dirs (which then finds nothing to adopt and
// relaunches — routable) or is skipped because the app is owned; it
// can never remove what an adoption in flight is about to route to.
// Per app, so one app's slow recovery holds up nobody else's. Lock
// order: deployMu or unknownSweepMu first, then this.
func ownerLock(app string) *sync.Mutex {
	mu, _ := ownerLocks.LoadOrStore(app, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

var ownerLocks sync.Map // app name → *sync.Mutex; one per name ever seen

// pruneSockets removes every instance's run/<nonce> dir and pinned
// proxy/<nonce>.sock except the kept instances' — the one serving,
// and the one being launched, whose run dir already exists. The unit
// itself removes its socket when it stops (ExecStopPost=) and retire
// removes the rest; this is the belt for anything left behind — an
// instance that died while hotserve was down — run when the manager's
// word is that nothing else is running. Best-effort: what cannot be
// removed is logged, and the next launch's fresh nonce never collides
// with it anyway.
func pruneSockets(d appDirs, keep []string, logger *zap.Logger) {
	kept := map[string]bool{}
	for _, n := range keep {
		kept[n] = true
	}
	for _, dir := range []string{d.run, d.proxy} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !os.IsNotExist(err) {
				logger.Warn("cannot list instance sockets", zap.String("dir", dir), zap.Error(err))
			}
			continue
		}
		for _, e := range entries {
			nonce := strings.TrimSuffix(e.Name(), ".sock")
			if !nonceRe.MatchString(nonce) || kept[nonce] {
				continue
			}
			path := filepath.Join(dir, e.Name())
			if err := os.RemoveAll(path); err != nil {
				logger.Warn("cannot remove stale instance socket", zap.String("path", path), zap.Error(err))
			}
		}
	}
}

// validateSocketPath refuses a root and app name whose socket paths
// would not fit in sun_path — the one the app binds under run/ and
// the longer one hotserve dials under proxy/. Checked at config load
// because the bind, or every connect, would otherwise fail at deploy
// time with an error that names no cause.
func validateSocketPath(root, name string) error {
	ref := newAppDirs(root, name).socketRef(strings.Repeat("0", 16))
	for _, socket := range []string{ref.path, ref.pinned} {
		if len(socket) > unixSocketPathMax {
			return fmt.Errorf("app %s: socket path %q is %d bytes; unix socket paths are limited to %d — use a shorter root or app name",
				name, socket, len(socket), unixSocketPathMax)
		}
	}
	return nil
}
