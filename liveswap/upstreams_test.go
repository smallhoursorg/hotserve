package liveswap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
	"go.uber.org/zap"
)

// Deploys in this file bind a real socket: GetUpstreams pins the
// socket file by descriptor, and there is nothing to pin otherwise.
func newUpstreamsRig(t *testing.T) *testRig {
	t.Helper()
	rig := newTestRig(t)
	rig.prober.bind = true
	return rig
}

// Before the first deploy is after recovery found nothing recorded:
// the error comes at once, not after a hold.
func TestGetUpstreamsBeforeFirstDeploy(t *testing.T) {
	rig := newTestRig(t)
	rig.ma.recover(context.Background(), zap.NewNop())
	u := &Upstreams{App: "demo", ma: rig.ma, waitCap: time.Hour}
	err := upstreamsErrWithin(t, u, httptest.NewRequest("GET", "/", nil))
	if err == nil || !strings.Contains(err.Error(), "no running version") {
		t.Fatalf("want no-version error, got %v", err)
	}
}

// upstreamsErrWithin returns GetUpstreams' error, failing the test if
// it is still holding after two seconds rather than letting a hold
// that should not be there hang the run.
func upstreamsErrWithin(t *testing.T, u *Upstreams, req *http.Request) error {
	t.Helper()
	got := make(chan error, 1)
	go func() {
		_, err := u.GetUpstreams(req)
		got <- err
	}()
	select {
	case err := <-got:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("GetUpstreams still holding after 2s")
		return nil
	}
}

// recordReattachable leaves the state a restart finds: v7 recorded,
// its unit alive and its socket bound, so recovery reattaches.
func recordReattachable(t *testing.T, rig *testRig) {
	t.Helper()
	bindSocket(t, rig.spec.dirs.socket(recordedNonce))
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{Unit: "hotserve-demo.v7.0a1b2c3d0a1b2c3d.service"}}
	rig.store.ok = true
	must(t, os.MkdirAll(rig.spec.dirs.release("v7"), 0o755))
	rig.runner.reattachOK = true
}

// Promise: a request that arrives before hotserve has reattached to a
// running app — Caddy can start serving before recovery has run — is
// held, then routed to the reattached instance rather than failed.
func TestGetUpstreamsHoldsUntilRecoveryReattaches(t *testing.T) {
	rig := newTestRig(t)
	recordReattachable(t, rig)
	u := &Upstreams{App: "demo", ma: rig.ma} // as Caddy builds it: the default cap
	type result struct {
		ups []*reverseproxy.Upstream
		err error
	}
	got := make(chan result, 1)
	go func() {
		ups, err := u.GetUpstreams(httptest.NewRequest("GET", "/", nil))
		got <- result{ups, err}
	}()
	select {
	case r := <-got:
		t.Fatalf("returned before recovery ran: %v, %v", r.ups, r.err)
	case <-time.After(100 * time.Millisecond):
	}
	go rig.ma.recover(context.Background(), zap.NewNop())
	select {
	case r := <-got:
		must(t, r.err)
		if want := "unix/" + rig.ma.currentInstance().sock.pinned; len(r.ups) != 1 || r.ups[0].Dial != want {
			t.Fatalf("held request routed to %v, want %s", r.ups, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still holding after recovery reattached")
	}
}

// Promise: a held request is let go, with the error it would have had
// without the hold, as soon as any of these happens: recovery's first
// attempt fails (the backoff before the next try is not waited out),
// recovery is cancelled before its first attempt (its config cleaned
// up), the client gives up, or the cap passes.
func TestGetUpstreamsReleasesAHeldRequest(t *testing.T) {
	req := func() *http.Request { return httptest.NewRequest("GET", "/", nil) }
	cases := []struct {
		name    string
		prepare func(t *testing.T, rig *testRig) (*Upstreams, *http.Request)
		want    string
	}{
		{"first attempt fails", func(t *testing.T, rig *testRig) (*Upstreams, *http.Request) {
			recordReattachable(t, rig)
			rig.runner.reattachErrs = []error{errTest, errTest, errTest}
			ctx, cancel := context.WithCancel(context.Background())
			exited := make(chan struct{})
			go func() { rig.ma.recover(ctx, zap.NewNop()); close(exited) }()
			t.Cleanup(func() { cancel(); <-exited })
			waitUntil(t, "first attempt", func() bool { return rig.runner.reattachCount() >= 1 })
			return &Upstreams{App: "demo", ma: rig.ma, waitCap: time.Hour}, req()
		}, "no running version"},
		{"recovery cancelled before its first attempt", func(t *testing.T, rig *testRig) (*Upstreams, *http.Request) {
			recordReattachable(t, rig)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			rig.ma.recover(ctx, zap.NewNop())
			if rig.runner.reattachCount() != 0 {
				t.Fatal("a cancelled recovery must not attempt anything")
			}
			return &Upstreams{App: "demo", ma: rig.ma, waitCap: time.Hour}, req()
		}, "no running version"},
		{"client gives up", func(t *testing.T, rig *testRig) (*Upstreams, *http.Request) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return &Upstreams{App: "demo", ma: rig.ma, waitCap: time.Hour}, req().WithContext(ctx)
		}, "still being recovered"},
		{"cap passes", func(t *testing.T, rig *testRig) (*Upstreams, *http.Request) {
			return &Upstreams{App: "demo", ma: rig.ma, waitCap: 20 * time.Millisecond}, req()
		}, "still being recovered"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, r := tc.prepare(t, newTestRig(t))
			if err := upstreamsErrWithin(t, u, r); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// Promise: after recovery, an instance that dies is not held for — the
// watchdog's relaunch window fails as fast as it always did.
func TestGetUpstreamsDoesNotHoldForACrash(t *testing.T) {
	rig := newUpstreamsRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1.tgz", version: "v1"}))
	rig.ma.recover(context.Background(), zap.NewNop()) // a started config: the instance is alive
	rig.ma.unrouteIf(rig.ma.currentInstance())         // then it dies
	u := &Upstreams{App: "demo", ma: rig.ma, waitCap: time.Hour}
	err := upstreamsErrWithin(t, u, httptest.NewRequest("GET", "/", nil))
	if err == nil || !strings.Contains(err.Error(), "no running version") {
		t.Fatalf("want the no-version error, got %v", err)
	}
}

func TestGetUpstreamsReturnsActiveSocket(t *testing.T) {
	rig := newUpstreamsRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1.tgz", version: "v1"}))
	u := &Upstreams{App: "demo", ma: rig.ma}
	ups, err := u.GetUpstreams(httptest.NewRequest("GET", "/", nil))
	if err != nil || len(ups) != 1 {
		t.Fatalf("upstreams: %v %v", ups, err)
	}
	// Caddy's unix//abs/path spelling: the network, a slash, then the
	// absolute path with its own leading slash — and the path is the
	// pinned name under proxy/, never the app-writable one under run/.
	inst := rig.ma.currentInstance()
	want := "unix/" + inst.sock.pinned
	if ups[0].Dial != want || !strings.HasPrefix(ups[0].Dial, "unix//") || !pathWithin(inst.sock.pinned, rig.spec.dirs.proxy) {
		t.Fatalf("dial = %q, want %q under %s", ups[0].Dial, want, rig.spec.dirs.proxy)
	}
	pinned, err := os.Lstat(inst.sock.pinned)
	must(t, err)
	bound, err := os.Lstat(inst.socket)
	must(t, err)
	if !os.SameFile(pinned, bound) {
		t.Fatal("the pinned name must be a hard link to the socket the app bound")
	}
}

// Promise: the proxy never follows the app-writable name. An app that
// replaces its socket with a symlink — to hotserve's admin socket, to
// a sibling's — gets a 502, not a connection through it.
func TestGetUpstreamsRefusesASymlinkUnderTheSocketName(t *testing.T) {
	rig := newTestRig(t) // no bind: nothing pinned yet when the symlink appears
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1.tgz", version: "v1"}))
	victim := filepath.Join(t.TempDir(), "admin.sock")
	bindSocket(t, victim)
	must(t, os.Symlink(victim, rig.ma.currentInstance().socket))
	u := &Upstreams{App: "demo", ma: rig.ma}
	_, err := u.GetUpstreams(httptest.NewRequest("GET", "/", nil))
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("want a refusal naming the symlink, got %v", err)
	}
}

// Cutover is visible to the proxy immediately: after a second deploy,
// GetUpstreams returns the new socket with no re-provisioning.
func TestGetUpstreamsSeesCutover(t *testing.T) {
	rig := newUpstreamsRig(t)
	u := &Upstreams{App: "demo", ma: rig.ma}
	ctx := context.Background()

	assertDial := func(version string) string {
		t.Helper()
		ups, err := u.GetUpstreams(httptest.NewRequest("GET", "/", nil))
		if err != nil {
			t.Fatalf("GetUpstreams: %v", err)
		}
		inst := rig.ma.currentInstance()
		if inst == nil || inst.version != version {
			t.Fatalf("current instance = %+v, want version %s", inst, version)
		}
		if want := "unix/" + inst.sock.pinned; ups[0].Dial != want {
			t.Fatalf("GetUpstreams dial = %s, want %s", ups[0].Dial, want)
		}
		return ups[0].Dial
	}

	must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/1.tgz", version: "v1"}))
	v1 := assertDial("v1")
	must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/2.tgz", version: "v2"}))
	if v2 := assertDial("v2"); v2 == v1 {
		t.Fatalf("v2 was handed v1's socket %s: sockets are per instance", v1)
	}
	// v1 is retired: its pinned name is gone, so a request that still
	// holds it dials nobody — never another instance.
	if _, err := os.Lstat(strings.TrimPrefix(v1, "unix/")); !os.IsNotExist(err) {
		t.Fatalf("v1's pinned name must be removed after the cutover: %v", err)
	}
}
