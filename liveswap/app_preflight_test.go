package liveswap

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A pre-flight refusal is the deployer's error: a 422 in phase
// preparing, before any unit exists — nothing started, no pre_start
// run, no detail (nothing of the app's ran), and the release cleaned
// up so the version can be deployed again once fixed.
func TestPreflightRefusalIs422BeforeAnyUnit(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.preStart = []string{"./migrate"}
	rig.runner.preflightErr = errors.New("this box is arm64 and ./server is an x86-64 executable: build on a runner of the box's architecture (runs-on: ubuntu-24.04-arm)")
	err := deployOnceV1(t, rig)
	var ve validationError
	if !errors.As(err, &ve) || !strings.Contains(err.Error(), "pre-flight: this box is arm64") {
		t.Fatalf("want a validationError naming the pre-flight, got %v", err)
	}
	if n := rig.runner.runOnceCount; n != 0 || len(rig.runner.started) != 0 {
		t.Fatalf("nothing may run after a refusal: pre_start runs %d, starts %d", n, len(rig.runner.started))
	}
	ld := rig.ma.status().LastDeploy
	if ld.Status != "failed" || ld.Phase != "preparing" || ld.Detail != nil {
		t.Fatalf("last deploy = %+v (detail %+v)", ld, ld.Detail)
	}
	rig.runner.preflightErr = nil
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://example.test/v1.tgz", version: "v1", by: "test"}); err != nil {
		t.Fatalf("the refused version deploys once fixed: %v", err)
	}
}

// The command and the pre_start are both checked, in that order, each
// with the spec the launch would use; a rollback checks the command
// only, since it runs no pre_start.
func TestPreflightChecksCommandThenPreStart(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.preStart = []string{"./migrate", "--up"}
	if err := deployOnceV1(t, rig); err != nil {
		t.Fatal(err)
	}
	p := rig.runner.preflights
	if len(p) != 2 || p[0].command[0] != rig.spec.command[0] || p[1].command[0] != "./migrate" || p[1].dir != p[0].dir {
		t.Fatalf("preflights = %+v", p)
	}
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://example.test/v2.tgz", version: "v2", by: "test"}); err != nil {
		t.Fatal(err)
	}
	rig.runner.preflights = nil
	if err := rig.ma.Deploy(context.Background(), deployRequest{version: "v1", rollback: true, by: "test"}); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if p := rig.runner.preflights; len(p) != 1 || p[0].command[0] != rig.spec.command[0] {
		t.Fatalf("a rollback pre-flights the command only: %+v", p)
	}
}
