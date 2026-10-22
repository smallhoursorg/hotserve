package liveswap

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"go.uber.org/zap"
)

// At a start only the caps that are set reach the unit, so an app with
// none gets exactly the manager's defaults; memory_max brings
// MemorySwapMax=0 with it, since a cap an app can spill past into
// swap bounds nothing.
func TestCapPropertiesAtStartEmitOnlyWhatIsSet(t *testing.T) {
	none := propMap(unitSpec{ExecStart: []string{"/bin/true"}, Sandbox: probeSandboxSpec()})
	for _, name := range []string{"MemoryMax", "MemorySwapMax", "TasksMax", "CPUQuotaPerSecUSec"} {
		if _, set := none[name]; set {
			t.Errorf("%s emitted for a spec with no caps", name)
		}
	}
	got := propMap(unitSpec{ExecStart: []string{"/bin/true"}, Sandbox: probeSandboxSpec(), Caps: resourceCaps{memoryMax: 64 << 20, cpuQuota: 150}})
	for name, want := range map[string]any{
		"MemoryMax":          uint64(64 << 20),
		"MemorySwapMax":      uint64(0),
		"CPUQuotaPerSecUSec": uint64(1_500_000), // 150% of a second, in µs
	} {
		if got[name] != want {
			t.Errorf("%s = %v (%T), want %v (%T)", name, got[name], got[name], want, want)
		}
	}
	if _, set := got["TasksMax"]; set {
		t.Error("TasksMax emitted though unset: the manager's default must stand")
	}
}

// Live, every cap is sent: a cap a reload removed is lifted from the
// running unit, swap allowance included — memory and CPU to infinity,
// their defaults, and tasks to the manager's default, never infinity
// (which would lift the manager's own bound from the unit).
func TestCapPropertiesLiveLiftWhatIsUnset(t *testing.T) {
	got := map[string]any{}
	for _, p := range capProperties(resourceCaps{cpuQuota: 50}, true, 2273) {
		got[p.Name] = p.Value.Value()
	}
	for name, want := range map[string]any{
		"MemoryMax":          infinityUSec,
		"MemorySwapMax":      infinityUSec,
		"TasksMax":           uint64(2273),
		"CPUQuotaPerSecUSec": uint64(500_000),
	} {
		if got[name] != want {
			t.Errorf("%s = %v, want %v", name, got[name], want)
		}
	}
	got = map[string]any{}
	for _, p := range capProperties(resourceCaps{tasksMax: 40}, true, 2273) {
		got[p.Name] = p.Value.Value()
	}
	if got["TasksMax"] != uint64(40) {
		t.Errorf("a set tasks cap must win over the lift: TasksMax = %v", got["TasksMax"])
	}
	// A kept tasks cap does not hold the memory and CPU resets back,
	// and neither does an unreadable default (liftTasksTo 0): the two
	// resets go, and only TasksMax is left as it is.
	for _, lift := range []uint64{2273, 0} {
		got = map[string]any{}
		for _, p := range capProperties(resourceCaps{tasksMax: 40}, true, lift) {
			got[p.Name] = p.Value.Value()
		}
		if got["MemoryMax"] != infinityUSec || got["MemorySwapMax"] != infinityUSec || got["CPUQuotaPerSecUSec"] != infinityUSec {
			t.Errorf("lift %d: removed memory/CPU caps not reset: %v", lift, got)
		}
	}
	got = map[string]any{}
	for _, p := range capProperties(resourceCaps{memoryMax: 1 << 20}, true, 0) {
		got[p.Name] = p.Value.Value()
	}
	if _, has := got["TasksMax"]; has {
		t.Errorf("TasksMax sent with no default to lift to: %v", got)
	}
}

// SetCaps lifts an unset tasks cap to the manager's DefaultTasksMax;
// with that unreadable it leaves the unit's tasks cap alone and the
// handle keeps reporting it, so status stays honest.
func TestSetCapsLiftsTasksToTheManagersDefault(t *testing.T) {
	f := newFakeSystemdConn()
	f.defaultTasksMax = 2273
	r := newSystemdRunner(f, zap.NewNop())
	t.Cleanup(r.close)
	h := &systemdHandle{unit: "hotserve-demo.v1.0a1b2c3d0a1b2c3d.service", done: make(chan struct{})}
	h.caps.Store(&resourceCaps{memoryMax: 64 << 20, tasksMax: 40})
	must(t, r.SetCaps(h, resourceCaps{memoryMax: 128 << 20}))
	sent := map[string]any{}
	for _, p := range f.setProps[h.unit][0] {
		sent[p.Name] = p.Value.Value()
	}
	if sent["TasksMax"] != uint64(2273) || sent["MemoryMax"] != uint64(128<<20) {
		t.Fatalf("sent %v, want TasksMax lifted to the manager's 2273 and MemoryMax 128M", sent)
	}
	if got := h.state().Caps; got != (resourceCaps{memoryMax: 128 << 20}) {
		t.Fatalf("handle reports %+v after the lift, want memory only", got)
	}
	// The default unreadable: no TasksMax is sent, and the handle
	// keeps the cap the unit still carries.
	h.caps.Store(&resourceCaps{tasksMax: 40})
	f.defaultTasksMax = 0
	must(t, r.SetCaps(h, resourceCaps{cpuQuota: 50}))
	sent = map[string]any{}
	for _, p := range f.setProps[h.unit][1] {
		sent[p.Name] = p.Value.Value()
	}
	if _, has := sent["TasksMax"]; has {
		t.Fatalf("TasksMax sent with the default unreadable: %v", sent)
	}
	if got := h.state().Caps; got != (resourceCaps{tasksMax: 40, cpuQuota: 50}) {
		t.Fatalf("handle reports %+v, want the kept tasks cap beside the new quota", got)
	}
}

// A unit with no TasksMax= of its own reports the manager's resolved
// default as its TasksMax (a number, never "infinity"): a reattach
// reads that as unset, so an uncapped app is not "capped at 2273" in
// status and no sync keeps re-sending it.
func TestReattachReadsTheDefaultTasksMaxAsUnset(t *testing.T) {
	f := newFakeSystemdConn()
	f.defaultTasksMax = 2273
	unit := "hotserve-demo.v1.0a1b2c3d0a1b2c3d.service"
	f.status[unit] = unitStatus{Name: unit, LoadState: "loaded", ActiveState: "active", MainPID: 4242, Sandboxed: true,
		Caps: resourceCaps{memoryMax: 64 << 20, tasksMax: 2273}}
	r := newSystemdRunner(f, zap.NewNop())
	t.Cleanup(r.close)
	h, ok, err := r.Reattach(handleState{Unit: unit})
	if err != nil || !ok {
		t.Fatalf("reattach: ok=%v err=%v", ok, err)
	}
	if got := h.state().Caps; got != (resourceCaps{memoryMax: 64 << 20}) {
		t.Fatalf("reattached handle reports %+v, want the memory cap alone (2273 is the manager's default)", got)
	}
	// A tasks cap that is not the default is the operator's.
	f.status[unit] = unitStatus{Name: unit, LoadState: "loaded", ActiveState: "active", MainPID: 4242, Sandboxed: true, Caps: resourceCaps{tasksMax: 512}}
	h, _, _ = r.Reattach(handleState{Unit: unit})
	if got := h.state().Caps; got != (resourceCaps{tasksMax: 512}) {
		t.Fatalf("reattached handle reports %+v, want tasks_max 512", got)
	}
}

// What the manager reports for an unbounded unit is "infinity", which
// must read as no cap — never as a bound of 2^64, and never as 0.
func TestPropCapsReadsInfinityAsNone(t *testing.T) {
	got := propCaps(map[string]any{"MemoryMax": infinityUSec, "TasksMax": infinityUSec, "CPUQuotaPerSecUSec": infinityUSec})
	if got.any() {
		t.Fatalf("infinity read as a cap: %+v", got)
	}
	if got := propCaps(map[string]any{}); got.any() {
		t.Fatalf("absent properties read as a cap: %+v", got)
	}
	got = propCaps(map[string]any{"MemoryMax": uint64(64 << 20), "TasksMax": uint64(40), "CPUQuotaPerSecUSec": uint64(500_000)})
	if want := (resourceCaps{memoryMax: 64 << 20, tasksMax: 40, cpuQuota: 50}); got != want {
		t.Fatalf("propCaps = %+v, want %+v", got, want)
	}
}

// A reload's changed caps reach the instance already running (and
// status shows them from then on); a reload that drops them lifts
// them, and one that changes nothing about them makes no call.
func TestSyncCapsAppliesAReloadsCapsToTheRunningInstance(t *testing.T) {
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"}))
	if rig.ma.status().Caps != nil {
		t.Fatal("no caps configured, none may be reported")
	}
	capped := testSpec(t)
	capped.caps = resourceCaps{memoryMax: 256 << 20, tasksMax: 512, cpuQuota: 200}
	rig.ma.configure(new(int), capped, nil, zap.NewNop(), &fetchClients{}, userManager)
	rig.ma.syncCaps()
	if len(rig.runner.setCaps) != 1 || rig.runner.setCaps[0] != capped.caps {
		t.Fatalf("SetCaps calls = %+v, want the reload's caps once", rig.runner.setCaps)
	}
	if got := rig.ma.status().Caps; got == nil || got.MemoryMax != 256<<20 || got.TasksMax != 512 || got.CPUQuota != 200 {
		t.Fatalf("status caps = %+v, want the applied ones", got)
	}
	// Unchanged caps: nothing to send.
	rig.ma.syncCaps()
	if len(rig.runner.setCaps) != 1 {
		t.Fatalf("SetCaps called again for unchanged caps: %+v", rig.runner.setCaps)
	}
	// Dropped caps are lifted, as one call carrying none.
	rig.ma.configure(new(int), testSpec(t), nil, zap.NewNop(), &fetchClients{}, userManager)
	rig.ma.syncCaps()
	if len(rig.runner.setCaps) != 2 || rig.runner.setCaps[1].any() {
		t.Fatalf("SetCaps calls = %+v, want a lifting call with no caps", rig.runner.setCaps)
	}
	if rig.ma.status().Caps != nil {
		t.Fatal("lifted caps still reported")
	}
}

// A sync that fails is a warning, and the instance keeps reporting
// what it had: the caps still apply at the next launch (startSpec).
func TestSyncCapsFailureIsAWarning(t *testing.T) {
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"}))
	capped := testSpec(t)
	capped.caps = resourceCaps{cpuQuota: 50}
	rig.runner.setCapsErr = os.ErrDeadlineExceeded
	rig.ma.configure(new(int), capped, nil, zap.NewNop(), &fetchClients{}, userManager)
	rig.ma.syncCaps()
	if rig.ma.status().Caps != nil {
		t.Fatal("a failed sync must not report the caps as applied")
	}
	// The next launch carries them regardless.
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/b.tgz", version: "v2"}))
	if last := rig.runner.started[len(rig.runner.started)-1]; last.caps != capped.caps {
		t.Fatalf("the launch after a failed sync carried %+v, want %+v", last.caps, capped.caps)
	}
}

// A deploy launches from the spec it snapshotted at its start; a
// reload during its fetch or soak changes the caps on the instance it
// is replacing (or on none, for a first deploy), not on the one about
// to be published. Publication reconciles against the spec as it is
// now, so the serving instance carries the accepted configuration's
// caps whichever path published it.
func TestAReloadDuringADeployReachesTheInstanceItPublishes(t *testing.T) {
	rig := newTestRig(t)
	held, release := rig.holdFetch(t)
	done := make(chan error, 1)
	go func() {
		done <- rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"})
	}()
	held()
	capped := testSpec(t)
	capped.caps = resourceCaps{cpuQuota: 150}
	rig.ma.configure(new(int), capped, nil, zap.NewNop(), &fetchClients{}, userManager) // nothing runs yet: nothing to sync
	release(nil)
	must(t, <-done)
	if last := rig.runner.started[len(rig.runner.started)-1]; last.caps.any() {
		t.Fatalf("the deploy launched with %+v, want the snapshot's none (the point of the test)", last.caps)
	}
	if len(rig.runner.setCaps) != 1 || rig.runner.setCaps[0] != capped.caps {
		t.Fatalf("SetCaps calls = %+v, want the reload's caps once, on the published instance", rig.runner.setCaps)
	}
	if got := rig.ma.status().Caps; got == nil || got.CPUQuota != 150 {
		t.Fatalf("status caps = %+v, want cpu_quota 150 on the serving instance", got)
	}
}

// A unit reattached after a hotserve restart carries the caps it was
// started with; a config changed in between reaches it on recovery.
func TestEnsureRunningSyncsCapsOfAReattachedUnit(t *testing.T) {
	rig := newTestRig(t)
	rig.runner.reattachOK = true
	rig.spec.caps = resourceCaps{memoryMax: 128 << 20}
	bindSocket(t, rig.spec.dirs.socket(recordedNonce))
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{Unit: "hotserve-demo.v7.0a1b2c3d0a1b2c3d.service"}}
	rig.store.ok = true
	if err := os.MkdirAll(rig.spec.dirs.release("v7"), 0o755); err != nil {
		t.Fatal(err)
	}
	must(t, rig.ma.ensureRunning())
	if len(rig.runner.setCaps) != 1 || rig.runner.setCaps[0] != rig.spec.caps {
		t.Fatalf("SetCaps calls = %+v, want the spec's caps once, on the reattached unit", rig.runner.setCaps)
	}
}

// Every launch carries the spec's caps: a deploy's instance, its
// pre_start, a watchdog relaunch — they are the app's, not the
// deploy's.
func TestEveryLaunchCarriesTheSpecsCaps(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.caps = resourceCaps{tasksMax: 64}
	rig.spec.preStart = []string{"./migrate"}
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"}))
	if n := len(rig.runner.started); n != 2 {
		t.Fatalf("started %d units, want the pre_start and the instance", n)
	}
	for _, s := range rig.runner.started {
		if s.caps != rig.spec.caps {
			t.Errorf("unit for %s %s started with %+v, want %+v", s.app, s.version, s.caps, rig.spec.caps)
		}
	}
}

// A CPU quota is written as systemd writes it, with its percent sign:
// a bare number would read as a count of something, and a 2 as two
// percent of one CPU.
func TestCaddyfileRefusesACPUQuotaWithoutItsPercent(t *testing.T) {
	for _, line := range []string{"cpu_quota 150", "cpu_quota two%", "cpu_quota %", "cpu_quota 1.5%"} {
		var a App
		err := a.UnmarshalCaddyfile(caddyfile.NewTestDispenser("liveswap {\n\tapp a {\n\t\tcommand x\n\t\t" + line + "\n\t}\n}"))
		if err == nil || !strings.Contains(err.Error(), "a percentage of one CPU, like 200% for two CPUs' worth") {
			t.Errorf("%q: want the percentage error, got %v", line, err)
		}
	}
}
