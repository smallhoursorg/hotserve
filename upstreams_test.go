package hotswap

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
	must(t, rig.ma.Deploy(ctx, deployRequest{URL: "https://x/1.tgz", Version: "v1"}))
	first, _ := u.GetUpstreams(httptest.NewRequest("GET", "/", nil))
	must(t, rig.ma.Deploy(ctx, deployRequest{URL: "https://x/2.tgz", Version: "v2"}))
	second, _ := u.GetUpstreams(httptest.NewRequest("GET", "/", nil))
	if first[0].Dial == second[0].Dial {
		t.Fatal("cutover not visible through GetUpstreams")
	}
}
