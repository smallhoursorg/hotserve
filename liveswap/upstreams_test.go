package liveswap

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetUpstreamsBeforeFirstDeploy(t *testing.T) {
	rig := newTestRig(t)
	u := &Upstreams{App: "demo", ma: rig.ma}
	_, err := u.GetUpstreams(httptest.NewRequest("GET", "/", nil))
	if err == nil || !strings.Contains(err.Error(), "no running version") {
		t.Fatalf("want no-version error, got %v", err)
	}
}

func TestGetUpstreamsReturnsActivePort(t *testing.T) {
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{URL: "https://x/1.tgz", Version: "v1"}))
	u := &Upstreams{App: "demo", ma: rig.ma}
	ups, err := u.GetUpstreams(httptest.NewRequest("GET", "/", nil))
	if err != nil || len(ups) != 1 {
		t.Fatalf("upstreams: %v %v", ups, err)
	}
	want := "127.0.0.1:" + portString(int(rig.ma.activePort.Load()))
	if ups[0].Dial != want {
		t.Fatalf("dial = %q, want %q", ups[0].Dial, want)
	}
}

// Cutover is visible to the proxy immediately: after a second deploy,
// GetUpstreams returns the new port with no re-provisioning.
func TestGetUpstreamsSeesCutover(t *testing.T) {
	rig := newTestRig(t)
	u := &Upstreams{App: "demo", ma: rig.ma}
	ctx := context.Background()

	// Assert that GetUpstreams follows the active instance, not that
	// two deploys get different ports: freePort's probe listener is
	// closed and the fake runner binds nothing, so the kernel may hand
	// v2 the very port v1 had — an inequality assertion is flaky (it
	// failed exactly that way on a quiet arm runner).
	assertDial := func(version string) {
		t.Helper()
		ups, err := u.GetUpstreams(httptest.NewRequest("GET", "/", nil))
		if err != nil {
			t.Fatalf("GetUpstreams: %v", err)
		}
		inst := rig.ma.currentInstance()
		if inst == nil || inst.version != version {
			t.Fatalf("current instance = %+v, want version %s", inst, version)
		}
		if want := "127.0.0.1:" + portString(inst.port); ups[0].Dial != want {
			t.Fatalf("GetUpstreams dial = %s, want %s", ups[0].Dial, want)
		}
	}

	must(t, rig.ma.Deploy(ctx, deployRequest{URL: "https://x/1.tgz", Version: "v1"}))
	assertDial("v1")
	must(t, rig.ma.Deploy(ctx, deployRequest{URL: "https://x/2.tgz", Version: "v2"}))
	assertDial("v2")
}
