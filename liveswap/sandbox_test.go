package liveswap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestSandboxTierRoundTrip(t *testing.T) {
	for _, tier := range []sandboxTier{sandboxNone, sandboxFull} {
		if got := parseSandboxTier(tier.String()); got != tier {
			t.Errorf("parse(%q) = %v, want %v", tier.String(), got, tier)
		}
	}
	// An older state.json (no field) and garbage both read as none:
	// the relaunch of an instance whose tier is unknown is bare.
	for _, s := range []string{"", "bogus", "FULL"} {
		if got := parseSandboxTier(s); got != sandboxNone {
			t.Errorf("parse(%q) = %v, want none", s, got)
		}
	}
	// The other direction, and the one a `default: return "none"` would
	// silently break: a tier this build does not define must not render
	// as a record validSandboxTierRecord accepts. state() persists
	// whatever String() returns, so a rendering that round-trips as a
	// legitimate value would let an out-of-range in-memory tier become a
	// valid bare record — a sandboxed unit persisted, and relaunched,
	// unsandboxed. 2 is included because it is the value sandboxFull
	// itself held until removing the filesystem tier renumbered the
	// iota — the most plausible stale tier there is.
	for _, undefined := range []sandboxTier{2, 99, -1} {
		rendered := undefined.String()
		if err := validSandboxTierRecord(rendered); err == nil {
			t.Errorf("tier %d rendered as %q, which is an accepted record", int(undefined), rendered)
		}
		if got := parseSandboxTier(rendered); got != sandboxNone {
			t.Errorf("parse(%q) = %v; an unrepresentable tier must not parse back to a real one", rendered, got)
		}
	}
	st := (&systemdHandle{unit: "u.service"}).state()
	if st.Sandbox != "" {
		t.Fatalf("a bare handle must not persist a tier, got %q", st.Sandbox)
	}
	st = (&systemdHandle{unit: "u.service", sandbox: sandboxFull}).state()
	if st.Sandbox != "full" {
		t.Fatalf("persisted tier = %q, want full", st.Sandbox)
	}
}

func TestResolveSandboxTier(t *testing.T) {
	full := sandboxCapability{tier: sandboxFull}
	// The two capabilities the probe can actually report. There is no
	// third: `filesystem` is neither probed for nor an accepted record
	// (see validSandboxTierRecord), so driving the policy with it would
	// test a path no host and no state.json can reach.
	none := sandboxCapability{tier: sandboxNone, reason: "full tier: unit failed: Failed to set up user namespacing: Permission denied"}
	cases := []struct {
		name     string
		mode     string
		cap      sandboxCapability
		wantTier sandboxTier
		wantWarn bool
		wantErr  bool
	}{
		{"auto full", sandboxAuto, full, sandboxFull, false, false},
		{"auto none warns", sandboxAuto, none, sandboxNone, true, false},
		{"require full", sandboxRequire, full, sandboxFull, false, false},
		{"require refuses none", sandboxRequire, none, sandboxNone, false, true},
		{"off ignores capability", sandboxOff, full, sandboxNone, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tier, warn, err := resolveSandboxTier(tc.mode, tc.cap)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tier != tc.wantTier {
				t.Fatalf("tier = %v, want %v", tier, tc.wantTier)
			}
			if (warn != "") != tc.wantWarn {
				t.Fatalf("warn = %q, wantWarn %v", warn, tc.wantWarn)
			}
			if tc.wantErr && !strings.Contains(err.Error(), tc.cap.reason) {
				t.Fatalf("require error must name the reason %q: %v", tc.cap.reason, err)
			}
			if tc.wantWarn && tc.cap.reason != "" && !strings.Contains(warn, tc.cap.reason) {
				t.Fatalf("warn must name the reason %q: %q", tc.cap.reason, warn)
			}
		})
	}
}

// sandboxPropertyNames is the set sandboxProperties emits; the test
// pins it so a property can neither disappear nor appear unnoticed.
var sandboxPropertyNames = []string{
	"PrivateUsers", "PrivatePIDs", "PrivateTmp", "PrivateDevices",
	"ProtectControlGroups", "ProtectKernelTunables", "ProtectKernelModules", "ProtectKernelLogs",
	"RestrictNamespaces", "RestrictRealtime", "RestrictSUIDSGID", "LockPersonality",
	"RestrictAddressFamilies", "SystemCallFilter", "SystemCallErrorNumber", "CapabilityBoundingSet",
	"TemporaryFileSystem", "BindPaths", "BindReadOnlyPaths", "UnsetEnvironment",
}

// retiredSandboxPropertyNames are the properties the deny-by-default
// view removed. Nothing is left for them to act on — an unnamed path is
// absent, not merely unreadable — and emitting them again would be a
// silent return to masking a list.
var retiredSandboxPropertyNames = []string{"InaccessiblePaths", "ProtectSystem", "ProtectHome"}

func propMap(u unitSpec) map[string]any {
	got := map[string]any{}
	for _, p := range unitProperties(u) {
		got[p.Name] = p.Value.Value()
	}
	return got
}

func TestSandboxPropertiesNoneWhenUnsandboxed(t *testing.T) {
	for _, u := range []unitSpec{
		{ExecStart: []string{"/bin/true"}},
		{ExecStart: []string{"/bin/true"}, Sandbox: &sandboxSpec{tier: sandboxNone, root: "/var/lib/liveswap"}},
	} {
		got := propMap(u)
		for _, name := range sandboxPropertyNames {
			if _, present := got[name]; present {
				t.Errorf("%s set on an unsandboxed unit", name)
			}
		}
		if got["NoNewPrivileges"] != true {
			t.Error("the floor (NoNewPrivileges) must stay on every unit")
		}
	}
}

func TestSandboxPropertiesFullTier(t *testing.T) {
	spec := &sandboxSpec{
		tier:    sandboxFull,
		root:    "/var/lib/liveswap",
		appDir:  "/var/lib/liveswap/blog",
		appName: "blog",
		writable: []bindPath{{dest: "/var/lib/liveswap/blog/releases/v3", source: "/var/lib/liveswap/blog/releases/v3"},
			{dest: "/var/lib/liveswap/blog/shared", source: "/var/lib/liveswap/blog/shared"}},
	}
	got := propMap(unitSpec{ExecStart: []string{"/bin/true"}, Sandbox: spec})
	for _, name := range sandboxPropertyNames {
		if _, present := got[name]; !present {
			t.Errorf("%s missing", name)
		}
	}
	for _, name := range retiredSandboxPropertyNames {
		if _, present := got[name]; present {
			t.Errorf("%s is set: the deny-by-default view masks nothing, it names what exists", name)
		}
	}
	// There is one tier above none, so PrivatePIDs= is emitted for
	// every sandboxed unit rather than conditionally.
	for name, want := range map[string]any{
		"PrivatePIDs":           "yes",
		"PrivateUsers":          true,
		"PrivateTmp":            true,
		"PrivateDevices":        true,
		"ProtectControlGroups":  true,
		"RestrictNamespaces":    uint64(0),
		"CapabilityBoundingSet": uint64(0),
		"SystemCallErrorNumber": int32(1),
		"NoNewPrivileges":       true,
	} {
		if got[name] != want {
			t.Errorf("%s = %v (%T), want %v (%T)", name, got[name], got[name], want, want)
		}
	}
	if fam, _ := got["RestrictAddressFamilies"].(allowList); !fam.AllowList || !reflect.DeepEqual(fam.Names, []string{"AF_INET", "AF_INET6", "AF_UNIX", "AF_NETLINK"}) {
		t.Errorf("RestrictAddressFamilies = %+v", got["RestrictAddressFamilies"])
	}
	if f, _ := got["SystemCallFilter"].(allowList); !f.AllowList || !reflect.DeepEqual(f.Names, []string{"@system-service"}) {
		t.Errorf("SystemCallFilter = %+v", got["SystemCallFilter"])
	}
	// The whole filesystem, not just the liveswap root: this one line is
	// the difference between "these paths are hidden" and "only these
	// paths exist".
	if tmp, _ := got["TemporaryFileSystem"].([]tmpfsMount); !reflect.DeepEqual(tmp, []tmpfsMount{{"/", "ro"}}) {
		t.Errorf("TemporaryFileSystem = %+v, want the whole root replaced", got["TemporaryFileSystem"])
	}
	binds := func(name string) map[string]bindMount {
		m := map[string]bindMount{}
		for _, b := range got[name].([]bindMount) {
			m[b.Destination] = b
		}
		return m
	}
	rw := binds("BindPaths")
	// The app's OWN directories are mandatory: a release or shared dir
	// that is not there is a broken deploy, not a boot-order race, and
	// starting without it would give the app an empty tmpfs where its
	// code and data should be.
	for _, p := range []string{"/var/lib/liveswap/blog/releases/v3", "/var/lib/liveswap/blog/shared"} {
		b, ok := rw[p]
		if !ok || b.Source != p || b.Flags != mountRecursive || b.IgnoreENOENT {
			t.Errorf("BindPaths lacks a recursive, mandatory bind of %s at its real path: %+v", p, rw)
		}
	}
	ro := binds("BindReadOnlyPaths")
	// The base view: every entry present, and every one optional — no
	// host has all of it (see sandboxBaseView).
	for _, p := range sandboxBaseView {
		b, ok := ro[p]
		if !ok {
			t.Errorf("BindReadOnlyPaths lacks the base-view entry %s", p)
			continue
		}
		if b.Source != p || !b.IgnoreENOENT || b.Flags != mountRecursive {
			t.Errorf("base-view entry %s = %+v, want a recursive optional bind at its own path", p, b)
		}
	}
	// The base view must carry the runtime an app needs to exec at all,
	// and must NOT carry /etc wholesale — that would hand every app
	// every other app's env_file, which is the derived hidden set this
	// model deletes.
	for _, needed := range []string{"/usr", "/bin", "/etc/resolv.conf", "/etc/ssl/certs", "/run/systemd/resolve"} {
		if _, ok := ro[needed]; !ok {
			t.Errorf("base view lacks %s: apps cannot run without it", needed)
		}
	}
	if _, whole := ro["/etc"]; whole {
		t.Error("/etc is bound wholesale: every app would see every other app's env_file")
	}
	// Nothing but the base view and the app's own dirs is bound at all:
	// nothing widens a view, so
	// the rendered set IS the guarantee.
	for dest := range ro {
		if !slices.Contains(sandboxBaseView, dest) {
			t.Errorf("BindReadOnlyPaths carries %s, which is neither a base-view entry nor one of the app's own dirs", dest)
		}
	}
	if unset, _ := got["UnsetEnvironment"].([]string); !reflect.DeepEqual(unset, []string{"XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"}) {
		t.Errorf("UnsetEnvironment = %v", unset)
	}
}

// TestSandboxViewIsExactlyWhatIsNamed is the guarantee as an assertion:
// with TemporaryFileSystem=/ the set of bind destinations IS the view,
// so anything that widens it accidentally fails here rather than in
// production. A path that is not a bind destination does not exist
// inside the unit.
func TestSandboxViewIsExactlyWhatIsNamed(t *testing.T) {
	spec := &sandboxSpec{
		tier:    sandboxFull,
		root:    "/var/lib/liveswap",
		appDir:  "/var/lib/liveswap/blog",
		appName: "blog",
		writable: []bindPath{{dest: "/var/lib/liveswap/blog/releases/v3"},
			{dest: "/var/lib/liveswap/blog/shared"}},
	}
	got := propMap(unitSpec{ExecStart: []string{"/bin/true"}, Sandbox: spec})
	if tmp, _ := got["TemporaryFileSystem"].([]tmpfsMount); !reflect.DeepEqual(tmp, []tmpfsMount{{"/", "ro"}}) {
		t.Fatalf("the view is not deny-by-default: TemporaryFileSystem = %+v", tmp)
	}
	view := map[string]bool{}
	for _, name := range []string{"BindPaths", "BindReadOnlyPaths"} {
		for _, b := range got[name].([]bindMount) {
			if view[b.Destination] {
				t.Errorf("%s is bound twice", b.Destination)
			}
			view[b.Destination] = true
		}
	}
	want := map[string]bool{
		"/var/lib/liveswap/blog/releases/v3": true,
		"/var/lib/liveswap/blog/shared":      true,
	}
	for _, p := range sandboxBaseView {
		want[p] = true
	}
	if !reflect.DeepEqual(view, want) {
		for p := range view {
			if !want[p] {
				t.Errorf("the view contains %s, which nothing named", p)
			}
		}
		for p := range want {
			if !view[p] {
				t.Errorf("the view lacks %s", p)
			}
		}
	}
	// The named consequences: not one of these is bound, so not one of
	// them exists inside the unit — no InaccessiblePaths= entry needed,
	// and no way for the list to go stale.
	for _, absent := range []string{
		"/var/lib/liveswap", "/var/lib/liveswap/blog", "/var/lib/liveswap/blog/state.json",
		"/var/lib/liveswap/blog/tmp", "/var/lib/liveswap/shop",
		"/var/lib/hotserve", "/run/hotserve", "/etc/hotserve", "/etc/liveswap",
		"/etc/blog/blog.env", "/run/user", "/home", "/opt", "/srv", "/var/lib",
		// Nothing widens a view any more, so a same-box database
		// socket dir and an out-of-tree cache are absent like
		// everything else nobody named.
		"/run/postgresql", "/var/cache/blog",
	} {
		if view[absent] {
			t.Errorf("%s is in the view", absent)
		}
		if spec.inView(absent) {
			t.Errorf("inView says %s is in the view, but nothing binds it", absent)
		}
	}
	for _, present := range []string{
		"/var/lib/liveswap/blog/releases/v3/server", "/var/lib/liveswap/blog/shared/db.sqlite",
		"/usr/bin/node", "/bin/sh", "/etc/ssl/certs",
	} {
		if !spec.inView(present) {
			t.Errorf("inView says %s is absent, but it is under a bind", present)
		}
	}
}

func TestSandboxSpecFor(t *testing.T) {
	spec := testSpec(t)
	if spec.sandboxSpecFor("/x", sandboxNone) != nil {
		t.Fatal("none must render no spec")
	}
	rel := spec.dirs.release("v1")
	got := spec.sandboxSpecFor(rel, sandboxFull)
	if got.tier != sandboxFull || got.root != spec.dirs.root {
		t.Fatalf("tier/root wrong: %+v", got)
	}
	if !reflect.DeepEqual(got.writable, []bindPath{{dest: rel, source: rel}, {dest: spec.dirs.shared, source: spec.dirs.shared}}) {
		t.Fatalf("writable = %v", got.writable)
	}
	// The view never includes state.json, tmp/ (upload staging) or
	// the other releases: nothing but the two dirs above is bound.
	for _, p := range []string{spec.dirs.state, spec.dirs.tmp, spec.dirs.releases, spec.dirs.app} {
		for _, w := range got.writable {
			if w.dest == p || w.source == p {
				t.Errorf("%s must not be in the writable view", p)
			}
		}
	}
	// appName, not the resolved app dir, is what the launch-time check
	// derives its expected base from — see resolveBindSources.
	if got.appName != spec.name || got.appDir != spec.dirs.app {
		t.Fatalf("appName/appDir = %q/%q, want %q/%q", got.appName, got.appDir, spec.name, spec.dirs.app)
	}
	if !filepath.IsAbs(got.root) {
		t.Fatalf("root must be absolute: %q", got.root)
	}
}

func TestSandboxProbeCommand(t *testing.T) {
	full := sandboxProbeCommand(sandboxFull)
	if full[0] != "/bin/sh" || full[1] != "-c" || len(full) != 3 {
		t.Fatalf("probe must be a single sh -c script, got %v", full)
	}
	// systemd turns "$$" into "$" in ExecStart arguments, so the shell
	// pid must be spelled "$$$$" and a bare "$$" must never appear.
	if !strings.Contains(full[2], "uid_map") || !strings.Contains(full[2], `"$$$$" = 1`) {
		t.Fatalf("full probe checks both namespaces: %q", full[2])
	}
	for _, s := range []string{full[2]} {
		if strings.Contains(strings.ReplaceAll(s, "$$$$", ""), "$$") {
			t.Fatalf("a bare $$ would reach the shell as $: %q", s)
		}
	}
}

// probeRunner scripts RunOnce per tier for probeSandboxCapability.
type probeRunner struct {
	*fakeRunner
	fail  map[sandboxTier]error
	tried []sandboxTier
	specs []startSpec
}

func (p *probeRunner) RunOnce(_ context.Context, spec startSpec) error {
	tier := sandboxTierOf(spec)
	p.tried = append(p.tried, tier)
	p.specs = append(p.specs, spec)
	return p.fail[tier]
}

func TestProbeSandboxCapability(t *testing.T) {
	denied := errors.New("unit failed: Failed to set up user namespacing: Permission denied")
	cases := []struct {
		name      string
		fail      map[sandboxTier]error
		wantTier  sandboxTier
		wantWords []string
	}{
		{"host delivers it", nil, sandboxFull, nil},
		// A manager that accepts the properties but a kernel that
		// silently ignores PrivatePIDs=: the probe exits non-zero
		// because the process is not pid 1. There is no lower tier to
		// fall to, so this is `none` — the operator must see the reason.
		{"kernel ignores the PID namespace", map[sandboxTier]error{sandboxFull: errors.New("exit status 1")}, sandboxNone, []string{"full tier", "exit status 1", "Debian 13"}},
		// A container, an LXC VPS, or a kernel built without user
		// namespaces: the manager refuses the unit outright.
		{"userns denied", map[sandboxTier]error{sandboxFull: denied}, sandboxNone, []string{"Permission denied", "Debian 13"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &probeRunner{fakeRunner: &fakeRunner{}, fail: tc.fail}
			got := probeSandboxCapability(r)
			if got.tier != tc.wantTier {
				t.Fatalf("tier = %v (%s), want %v", got.tier, got.reason, tc.wantTier)
			}
			// One supported tier means exactly one candidate: a probe
			// that tried the removed filesystem tier would be reporting
			// a capability nothing can be launched with.
			if !reflect.DeepEqual(r.tried, []sandboxTier{sandboxFull}) {
				t.Fatalf("tried %v, want exactly [full]", r.tried)
			}
			for _, w := range tc.wantWords {
				if !strings.Contains(got.reason, w) {
					t.Errorf("reason %q lacks %q", got.reason, w)
				}
			}
			if got.tier == sandboxFull && got.reason != "" {
				t.Errorf("full needs no reason, got %q", got.reason)
			}
			for _, s := range r.specs {
				// The probe carries the tier and nothing else: with a
				// deny-by-default view there is no per-host state for it
				// to reproduce, and it exposes not one path of its own.
				if s.sandbox == nil || len(s.sandbox.writable) != 0 || s.sandbox.root != "" {
					t.Errorf("probe unit must carry the tier's sandbox and expose nothing: %+v", s.sandbox)
				}
				if !reflect.DeepEqual(s.command, sandboxProbeCommand(s.sandbox.tier)) {
					t.Errorf("probe command mismatch for %v", s.sandbox.tier)
				}
			}
		})
	}
}

func TestBuildEnvSandboxedHome(t *testing.T) {
	spec := testSpec(t)
	t.Setenv("HOME", "/var/lib/hotserve")
	bare, err := buildEnv(spec, "v1", 8123, spec.dirs.release("v1"), false)
	must(t, err)
	sandboxed, err := buildEnv(spec, "v1", 8123, spec.dirs.release("v1"), true)
	must(t, err)
	find := func(env []string, key string) (string, int) {
		var val string
		n := 0
		for _, kv := range env {
			if strings.HasPrefix(kv, key+"=") {
				val = strings.TrimPrefix(kv, key+"=")
				n++
			}
		}
		return val, n
	}
	if v, _ := find(bare, "HOME"); v != "/var/lib/hotserve" {
		t.Fatalf("bare HOME = %q, want the inherited one", v)
	}
	if v, n := find(sandboxed, "HOME"); v != spec.dirs.shared || n != 1 {
		t.Fatalf("sandboxed HOME = %q (×%d), want %s once", v, n, spec.dirs.shared)
	}
	// env_file / inline env still win: they come later.
	spec.env = map[string]string{"HOME": "/elsewhere"}
	sandboxed, err = buildEnv(spec, "v1", 8123, spec.dirs.release("v1"), true)
	must(t, err)
	if v, _ := find(sandboxed, "HOME"); !strings.HasSuffix(strings.Join(sandboxed, "\n"), "HOST=127.0.0.1") || v == "" {
		t.Fatalf("env ordering broken: %v", sandboxed)
	}
	last := ""
	for _, kv := range sandboxed {
		if strings.HasPrefix(kv, "HOME=") {
			last = kv
		}
	}
	if last != "HOME=/elsewhere" {
		t.Fatalf("an explicit HOME must come after the sandbox default, got last %q", last)
	}
}

// TestDeployUsesPolicyRelaunchUsesRecord is the upgrade contract: a
// deploy applies the tier policy resolved at Start, while recovery and
// watchdog relaunches reproduce the tier recorded for the instance —
// so enabling the sandbox never forces a running bare app into one on
// the relaunch that has no fallback.
func TestDeployUsesPolicyRelaunchUsesRecord(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.sandboxMode = sandboxAuto
	rig.spec.sandboxTier = sandboxFull
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"}))
	if got := rig.runner.started[0].sandbox; got == nil || got.tier != sandboxFull {
		t.Fatalf("deploy must apply the policy tier: %+v", got)
	}
	if st := rig.ma.status(); st.Sandbox != "full" {
		t.Fatalf("status sandbox = %q, want full", st.Sandbox)
	}
	// pre_start runs under the same sandbox.
	rig.spec.preStart = []string{"./migrate"}
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/b.tgz", version: "v2"}))
	// The fake records RunOnce beside Start: pre_start, then the app.
	if n := len(rig.runner.started); n != 3 {
		t.Fatalf("started = %d, want deploy, pre_start, deploy", n)
	}
	for i := 1; i <= 2; i++ {
		if got := rig.runner.started[i].sandbox; got == nil || got.tier != sandboxFull {
			t.Fatalf("started[%d] must carry the same sandbox as the app: %+v", i, got)
		}
	}

	// A recorded bare instance relaunches bare although policy now
	// says full.
	rig2 := newTestRig(t)
	rig2.spec.sandboxMode = sandboxAuto
	rig2.spec.sandboxTier = sandboxFull
	must(t, rig2.store.save(appState{CurrentVersion: "v1", Port: 1, Handle: handleState{Unit: "hotserve-demo.v1.abc.service"}}))
	must(t, mkdirRelease(rig2.spec, "v1"))
	rig2.runner.reattachOK = false
	if err := rig2.ma.ensureRunning(); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got := rig2.runner.started[0].sandbox; got != nil {
		t.Fatalf("a bare-recorded instance must relaunch bare, got %+v", got)
	}
	if st := rig2.ma.status(); st.Sandbox != "none" {
		t.Fatalf("status sandbox = %q, want none", st.Sandbox)
	}
	// And the converse: a recorded full-tier instance relaunches at
	// that tier although policy now says off. The record decides a
	// relaunch in both directions — that is the whole mechanism, and
	// with one tier these two rigs are the only ways to state it.
	rig3 := newTestRig(t)
	rig3.spec.sandboxMode = sandboxOff
	rig3.spec.sandboxTier = sandboxNone
	must(t, rig3.store.save(appState{CurrentVersion: "v2", Port: 1, Handle: handleState{Unit: "hotserve-demo.v2.abc.service", Sandbox: "full"}}))
	must(t, mkdirRelease(rig3.spec, "v2"))
	if err := rig3.ma.ensureRunning(); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got := rig3.runner.started[0].sandbox; got == nil || got.tier != sandboxFull {
		t.Fatalf("recorded tier not reproduced: %+v", got)
	}
	if st := rig3.ma.status(); st.Sandbox != "full" {
		t.Fatalf("status sandbox = %q, want full", st.Sandbox)
	}

	// The third path a record reaches, and the one nothing pinned: a
	// reattach, where the unit is still running and is adopted rather
	// than relaunched. systemdRunner.Reattach reproduces the tier from
	// the record; without this, deleting that line left the whole
	// suite green while status under-reported a sandboxed app as
	// "none" and the next watchdog restart relaunched it bare.
	rig4 := newTestRig(t)
	rig4.spec.sandboxMode = sandboxAuto
	rig4.spec.sandboxTier = sandboxFull
	rig4.runner.reattachOK = true
	must(t, rig4.store.save(appState{CurrentVersion: "v1", Port: 1, Handle: handleState{Unit: "hotserve-demo.v1.abc.service", Sandbox: "full"}}))
	must(t, mkdirRelease(rig4.spec, "v1"))
	if err := rig4.ma.ensureRunning(); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if n := len(rig4.runner.started); n != 0 {
		t.Fatalf("a reattachable unit must be adopted, not relaunched: started = %d", n)
	}
	if st := rig4.ma.status(); st.Sandbox != "full" {
		t.Fatalf("reattach dropped the recorded tier: status sandbox = %q, want full", st.Sandbox)
	}
}

func TestCaddyfileSandboxDirectives(t *testing.T) {
	d := caddyfile.NewTestDispenser(`liveswap {
		sandbox require
		app blog {
			command node server.js
			sandbox auto
		}
		app api {
			command ./server
		}
	}`)
	var a App
	if err := a.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if a.Sandbox != "require" || a.Apps["blog"].Sandbox != "auto" || a.Apps["api"].Sandbox != "" {
		t.Fatalf("sandbox modes wrong: %q %q %q", a.Sandbox, a.Apps["blog"].Sandbox, a.Apps["api"].Sandbox)
	}
	for _, bad := range []string{
		// extra_path was removed; the directive must not be silently accepted.
		"liveswap {\n app x {\n command a\n extra_path /p\n }\n}",
		"liveswap {\n sandbox\n}",
		"liveswap {\n app x {\n command a\n sandbox auto extra\n }\n}",
	} {
		var a App
		if err := a.UnmarshalCaddyfile(caddyfile.NewTestDispenser(bad)); err == nil {
			t.Errorf("accepted: %s", bad)
		}
	}
}

func TestValidateSandboxConfig(t *testing.T) {
	base := func() *App {
		return &App{
			Root:              "/var/lib/liveswap",
			ArtifactAllowlist: []string{"github.com/smallhoursorg/"},
			DeployTrust:       githubTrust(),
			Apps:              map[string]*AppConfig{"blog": defaultedApp(t)},
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("base config rejected: %v", err)
	}
	// Precedence: the app's mode beats the global one; "" defers.
	a := base()
	a.Sandbox = "require"
	spec, err := a.buildSpec("blog", a.Apps["blog"])
	must(t, err)
	if spec.sandboxMode != "require" {
		t.Fatalf("global mode not inherited: %q", spec.sandboxMode)
	}
	a.Apps["blog"].Sandbox = "off"
	spec, err = a.buildSpec("blog", a.Apps["blog"])
	must(t, err)
	if spec.sandboxMode != "off" {
		t.Fatalf("app mode must override: %q", spec.sandboxMode)
	}
	for name, mutate := range map[string]func(*App){
		"bad global mode": func(a *App) { a.Sandbox = "yes" },
		"bad app mode":    func(a *App) { a.Apps["blog"].Sandbox = "on" },
		"root in state":   func(a *App) { a.Root = "/var/lib/hotserve/apps" },
	} {
		t.Run(name, func(t *testing.T) {
			a := base()
			mutate(a)
			if err := a.Validate(); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// TestStartResolvesSandboxPolicy drives App.Start with a scripted host
// that cannot deliver the sandbox — a container, or a kernel that
// refuses user namespaces: require refuses to start and names the
// reason; auto starts unsandboxed and warns; off never probes.
func TestStartResolvesSandboxPolicy(t *testing.T) {
	probes, managerProbes := 0, 0
	newApp := func(mode string) *App {
		spec := testSpec(t)
		spec.sandboxMode = mode
		// A managed app, not an empty map: Start gates the
		// reachability probe on there being one, so an empty pool
		// leaves that seam dead and the ordering it guarantees —
		// manager reachable BEFORE the host is measured — unasserted.
		rig := newTestRig(t)
		rig.spec = spec
		rig.ma.spec = spec
		// Start installs its own watchdog context over the rig's, so
		// the rig's t.Cleanup no longer reaches it; stop it explicitly
		// or goleak fails the package on the leaked goroutine.
		t.Cleanup(rig.ma.stopWatchdog)
		// Per-App, not shared: the ordering being pinned is "this
		// config proved its manager reachable before it measured the
		// host", and a counter shared across the three apps below
		// would be satisfied by the previous app's probe.
		measuredHere := false
		return &App{
			Root:    spec.dirs.root,
			logger:  zap.NewNop(),
			specs:   map[string]*appSpec{"demo": spec},
			managed: map[string]*managedApp{"demo": rig.ma},
			clients: &fetchClients{},
			// A fake connection, so the unknown-app sweep Start spawns
			// cannot reach a real manager and stop units belonging to
			// whoever is running one on this machine.
			manager: newFakeSystemdConn(),
			managerProbe: func() error {
				managerProbes++
				measuredHere = true
				return nil
			},
			sandboxProbe: func(*zap.Logger) sandboxCapability {
				probes++
				if !measuredHere {
					t.Error("this App measured the host before proving its manager reachable")
				}
				return sandboxCapability{tier: sandboxNone, reason: "full tier: unit failed: Failed to set up user namespacing: Permission denied"}
			},
		}
	}
	a := newApp(sandboxRequire)
	if err := a.Start(); err == nil || !strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("require on a host without the sandbox must refuse with the reason, got %v", err)
	}
	a = newApp(sandboxAuto)
	if err := a.Start(); err != nil {
		t.Fatalf("auto must start: %v", err)
	}
	if a.specs["demo"].sandboxTier != sandboxNone {
		t.Fatalf("auto tier = %v", a.specs["demo"].sandboxTier)
	}
	_ = a.Cleanup()
	before := probes
	a = newApp(sandboxOff)
	if err := a.Start(); err != nil {
		t.Fatalf("off must start: %v", err)
	}
	if probes != before {
		t.Fatal("off must not probe the host")
	}
	if a.specs["demo"].sandboxTier != sandboxNone {
		t.Fatalf("off tier = %v", a.specs["demo"].sandboxTier)
	}
	_ = a.Cleanup()

	// Every one of those starts had a managed app, so every one had to
	// prove the manager reachable first — the seam that replaced the
	// probeUserManager package var, and the precondition the cached
	// measurement's own early return depends on.
	if managerProbes != 3 {
		t.Fatalf("manager reachability probed %d times across 3 starts, want 3", managerProbes)
	}
}

// TestSandboxRootUnderTmpIsAllowed: only hotserve's own state is a
// root worth failing config load over. A root under a path the sandbox
// replaces (/tmp, /var/tmp — every t.TempDir, and the real-systemd
// integration lane's own root) still works, because systemd creates
// the mount point for TemporaryFileSystem= and BindPaths= inside the
// namespace; refusing it would have turned an odd-but-working setup
// into a server that will not start.
func TestSandboxRootUnderTmpIsAllowed(t *testing.T) {
	for _, root := range []string{"/tmp/liveswap-test", "/var/tmp/x", "/home/deploy/apps", "/srv/apps"} {
		if err := validateSandboxRoot(root, true); err != nil {
			t.Errorf("root %s must not fail config load: %v", root, err)
		}
	}
}

// TestRelaunchBelowFullWarns pins the "prominent WARN at every spawn":
// a relaunch that reproduces a bare instance while policy wants a
// sandbox logs it, naming the residual; a full-tier launch and
// sandbox off do not.
func TestRelaunchBelowFullWarns(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     string
		recorded string
		want     int
	}{
		{"auto, bare record", sandboxAuto, "", 1},
		// require, not a second spelling of the bare record above:
		// warnSandboxTier short-circuits on sandboxOff alone, and
		// nothing else in the suite pins that require still warns.
		{"require, bare record", sandboxRequire, "", 1},
		{"auto, full record", sandboxAuto, "full", 0},
		{"off, bare record", sandboxOff, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTestRig(t)
			core, logs := observer.New(zap.WarnLevel)
			rig.ma.logger = zap.New(core)
			rig.spec.sandboxMode = tc.mode
			rig.spec.sandboxTier = sandboxFull
			must(t, rig.store.save(appState{CurrentVersion: "v1", Port: 1, Handle: handleState{Unit: "hotserve-demo.v1.abc.service", Sandbox: tc.recorded}}))
			must(t, mkdirRelease(rig.spec, "v1"))
			if err := rig.ma.ensureRunning(); err != nil {
				t.Fatalf("recover: %v", err)
			}
			got := logs.FilterMessage("launching without the full sandbox").Len()
			if got != tc.want {
				t.Fatalf("warned %d times, want %d: %v", got, tc.want, logs.All())
			}
		})
	}
}

// TestEnvFilesAreAbsentWithoutBeingListed is what replaced the derived
// hidden set: no app's env_file is named anywhere in a sibling's spec,
// and none of them is reachable inside its view — not because they
// were enumerated and masked, but because nothing bound them. This is
// the same guarantee the old set gave only for the paths it happened
// to know about, and only for units started after they were declared.
func TestEnvFilesAreAbsentWithoutBeingListed(t *testing.T) {
	t.Setenv("SECRETS", "/etc/secrets")
	a := &App{
		Root:              "/var/lib/liveswap",
		ArtifactAllowlist: []string{"github.com/smallhoursorg/"},
		DeployTrust:       githubTrust(),
		Apps: map[string]*AppConfig{
			"blog": {Command: []string{"x"}, EnvFile: "{env.SECRETS}/blog.env"},
			"shop": {Command: []string{"x"}, EnvFile: "{env.SECRETS}/shop.env"},
		},
	}
	repl := caddy.NewReplacer()
	for name, cfg := range a.Apps {
		cfg.applyDefaults(repl)
		spec, err := a.buildSpec(name, cfg)
		if err != nil {
			t.Fatal(err)
		}
		sb := spec.sandboxSpecFor(spec.dirs.release("v1"), sandboxFull)
		for _, secret := range []string{"/etc/secrets/blog.env", "/etc/secrets/shop.env", "/etc/secrets"} {
			if sb.inView(secret) {
				t.Errorf("%s's view reaches %s", name, secret)
			}
		}
		// And an env_file declared *later* — the case the snapshot
		// model could not cover, because a running unit's view is never
		// rebuilt — is absent for exactly the same reason.
		if sb.inView("/etc/secrets/added-tomorrow.env") {
			t.Errorf("%s's view reaches an env_file that does not exist yet", name)
		}
	}
}

// TestWarnEnvFileInView: the one place an env_file can still land in a
// view is the app's own directories, which are the only host paths
// bound writable. Its own environment is legitimately reachable, but
// under shared/ the app can rewrite the file and so choose its own
// next launch's environment — worth saying out loud at config load.
func TestWarnEnvFileInView(t *testing.T) {
	spec := testSpec(t)
	for _, tc := range []struct {
		name    string
		envFile string
		mode    string
		want    int
	}{
		{"outside every view", "/etc/hotserve/demo.env", sandboxAuto, 0},
		{"in shared", filepath.Join(spec.dirs.shared, "demo.env"), sandboxAuto, 1},
		{"in releases", filepath.Join(spec.dirs.releases, "v1", "demo.env"), sandboxAuto, 1},
		{"in shared but unsandboxed", filepath.Join(spec.dirs.shared, "demo.env"), sandboxOff, 0},
		{"no env_file", "", sandboxAuto, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zap.WarnLevel)
			s := *spec
			s.envFile, s.sandboxMode = tc.envFile, tc.mode
			warnEnvFileInView(zap.New(core), map[string]*appSpec{"demo": &s})
			if got := logs.Len(); got != tc.want {
				t.Fatalf("warned %d times, want %d: %v", got, tc.want, logs.All())
			}
		})
	}
	// A nil logger is the pre-Provision case (Validate called directly
	// by a test or by `caddy validate`); it must not panic.
	warnEnvFileInView(nil, map[string]*appSpec{"demo": spec})

	// A symlink must not walk past the warning. An env_file that only
	// reaches shared/ through a link is exactly as rewritable by the
	// app, and a warning a symlink defeats is worse than none: it reads
	// as "checked, and fine".
	t.Run("through a symlink into shared", func(t *testing.T) {
		outside, err := os.MkdirTemp("/var", "envlink-")
		if err != nil {
			t.Skipf("no writable dir outside the refused prefixes: %v", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(outside) })
		must(t, os.MkdirAll(spec.dirs.shared, 0o750))
		target := filepath.Join(spec.dirs.shared, "demo.env")
		must(t, os.WriteFile(target, []byte("A=1\n"), 0o600))
		link := filepath.Join(outside, "demo.env")
		must(t, os.Symlink(target, link))

		core, logs := observer.New(zap.WarnLevel)
		s := *spec
		s.envFile, s.sandboxMode = link, sandboxAuto
		warnEnvFileInView(zap.New(core), map[string]*appSpec{"demo": &s})
		if logs.Len() != 1 {
			t.Fatalf("an env_file linked into shared/ must warn: %v", logs.All())
		}
	})
}

// TestValidateRejectsUncleanRoot: every containment check is lexical,
// so a root spelled with .. would let a bind reach a sibling
// through a spelling the check never sees.
func TestValidateRejectsUncleanRoot(t *testing.T) {
	base := func(root string) *App {
		a := &App{Root: root, ArtifactAllowlist: []string{"github.com/smallhoursorg/"},
			DeployTrust: githubTrust(), Apps: map[string]*AppConfig{"blog": defaultedApp(t)}}
		return a
	}
	if err := base("/var/lib/liveswap").Validate(); err != nil {
		t.Fatalf("clean root rejected: %v", err)
	}
	for _, bad := range []string{"/srv/liveswap/../liveswap", "/srv//liveswap", "/srv/liveswap/."} {
		if err := base(bad).Validate(); err == nil {
			t.Errorf("unclean root %q accepted", bad)
		}
	}
}

// TestResolveBindSourcesRefusesPlantedSymlink is the escape a bare app
// could arrange for its own sandboxed future: `shared` and the release
// dirs live under the shared hotserve UID and appDirs.ensure's MkdirAll
// succeeds on a symlink, so an app running with `sandbox off` (or on a
// host stuck at the none tier, or simply before its first sandboxed
// deploy — the documented rollout) can point its own shared dir at the
// user manager's socket directory and have BindPaths= mount it inside
// the sandbox on the next launch.
func TestResolveBindSourcesRefusesPlantedSymlink(t *testing.T) {
	root := t.TempDir()
	appDir := filepath.Join(root, "blog")
	release := filepath.Join(appDir, "releases", "v1")
	shared := filepath.Join(appDir, "shared")
	sibling := filepath.Join(root, "shop", "shared")
	// "Outside" areas live under /run: the root itself is a t.TempDir
	// under /tmp, which is a closed prefix, so a link resolving there
	// would be refused for that reason instead of the one under test.
	outside, err := os.MkdirTemp("/run", "bindsrc-")
	if err != nil {
		t.Skipf("no writable dir outside the closed prefixes: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outside) })
	elsewhere := filepath.Join(outside, "data")
	for _, d := range []string{release, shared, sibling, elsewhere} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	// These have to exist for the containment branch to be the one that
	// refuses them: a link to a path that does not resolve is refused
	// by the missing-source case instead — fail-closed either way, but
	// a different rule than the one under test. Neither is present in
	// the plain build container.
	for _, p := range []string{"/run/hotserve", "/run/user"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			continue
		}
		if err := os.MkdirAll(p, 0o750); err != nil {
			t.Skipf("cannot create %s: %v", p, err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(p) })
	}
	spec := func(sharedPath string) *sandboxSpec {
		return &sandboxSpec{tier: sandboxFull, root: root, appDir: appDir, appName: "blog",
			writable: []bindPath{{dest: release, source: release}, {dest: sharedPath, source: sharedPath}}}
	}
	// Baseline: real directories resolve to themselves and are kept.
	s := spec(shared)
	if err := s.resolveBindSources(); err != nil {
		t.Fatalf("plain directories refused: %v", err)
	}
	if !reflect.DeepEqual(s.writable, []bindPath{{dest: release, source: release}, {dest: shared, source: shared}}) {
		t.Fatalf("writable rewritten unnecessarily: %v", s.writable)
	}

	link := filepath.Join(appDir, "shared-link")
	// Targets that exist here, so resolution succeeds and the
	// containment check is what refuses them. (A link to a target that
	// does not resolve is refused too, by the missing-source case at
	// the end — fail-closed either way.)
	// Every planted target is refused by the same rule now: a mandatory
	// bind must BE the directory it names. The deny lists are no longer
	// what catches these — they are the backstop for a root re-aimed
	// after config load, tested directly elsewhere.
	for _, tc := range []struct{ name, target, wantIn string }{
		{"a closed tree", "/dev", "not the directory it names"},
		{"the private tmp", "/tmp", "not the directory it names"},
		{"hotserve's own state", "/run/hotserve", "not the directory it names"},
		{"the manager's socket dir", "/run/user", "not the directory it names"},
		{"a sibling app's data", sibling, "not the directory it names"},
		{"a sibling's data on another disk", elsewhere, "not the directory it names"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Remove(link)
			if err := os.Symlink(tc.target, link); err != nil {
				t.Fatal(err)
			}
			err := spec(link).resolveBindSources()
			if err == nil {
				t.Fatalf("a shared dir pointing at %s was bound into the sandbox", tc.target)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Fatalf("refused for the wrong reason: %v", err)
			}
		})
	}

	// A shared dir symlinked anywhere outside the root is refused too —
	// including somewhere that looks perfectly innocent. The app's own
	// directory is writable by the app, so a symlink placed there
	// cannot be told from one the operator meant; the refusal names the
	// mechanism that CAN be told apart.
	_ = os.Remove(link)
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatal(err)
	}
	err = spec(link).resolveBindSources()
	if err == nil {
		t.Fatal("a shared dir symlinked outside the root was accepted; an app can plant that symlink itself")
	}
	if !strings.Contains(err.Error(), "bind-mount") {
		t.Fatalf("the refusal must name the supported alternative: %v", err)
	}

	// A missing bind source is an error, not a silently skipped bind.
	if err := spec(filepath.Join(appDir, "gone")).resolveBindSources(); err == nil {
		t.Fatal("a missing shared dir must fail the launch")
	}
}

// TestUnitForResolvesBindSources: the check runs at unit creation, the
// last point before systemd follows the paths, so a launch whose bind
// source was replaced after config load fails instead of mounting it.
func TestUnitForResolvesBindSources(t *testing.T) {
	r, _ := newTestSystemdRunner(t)
	root := t.TempDir()
	appDir := filepath.Join(root, "blog")
	release := filepath.Join(appDir, "releases", "v1")
	if err := os.MkdirAll(release, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(release, "server"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(appDir, "shared")
	// /dev stands in for /run/user/<uid> — the real target on a live
	// host, absent in the test container; both are closed prefixes.
	if err := os.Symlink("/dev", shared); err != nil {
		t.Fatal(err)
	}
	spec := startSpec{
		app: "blog", version: "v1", command: []string{"./server"}, dir: release,
		sandbox: &sandboxSpec{tier: sandboxFull, root: root, appDir: appDir, appName: "blog",
			writable: []bindPath{{dest: release, source: release}, {dest: shared, source: shared}}},
	}
	if _, err := r.unitFor(spec, false); err == nil || !strings.Contains(err.Error(), "not the directory it names") {
		t.Fatalf("unitFor must refuse a planted bind source, got %v", err)
	}
	if _, err := r.Start(spec); err == nil {
		t.Fatal("Start must fail rather than launch with the planted bind")
	}
}

// TestBindDestinationsAreTheConfiguredPaths: a permitted symlink (the
// operator's `shared` on another disk) must change only where the data
// is read FROM. The app is told about its directories by
// WorkingDirectory, HOME and the {release_dir}/{shared_dir}
// placeholders, all of which name the configured path — and that path
// lives under the liveswap root, which the sandbox replaces with an
// empty tmpfs, so a bind that moved the destination would leave the
// app pointing at something that no longer exists inside its view.
func TestBindDestinationsAreTheConfiguredPaths(t *testing.T) {
	// The one way a bind source may legitimately differ from its
	// destination is an alias on the liveswap root itself, so that is
	// what this exercises. Built under /var: a t.TempDir root lives
	// under /tmp, and the app dirs must resolve somewhere the deny list
	// for bind sources does not object to.
	base, err := os.MkdirTemp("/var", "binddest-")
	if err != nil {
		t.Skipf("no writable dir outside the refused prefixes: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	real := filepath.Join(base, "mnt-liveswap")
	release := filepath.Join(real, "blog", "releases", "v1")
	shared := filepath.Join(real, "blog", "shared")
	for _, d := range []string{release, shared} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	root := filepath.Join(base, "srv-liveswap")
	if err := os.Symlink(real, root); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	aliasRelease := filepath.Join(root, "blog", "releases", "v1")
	aliasShared := filepath.Join(root, "blog", "shared")
	sb := &sandboxSpec{tier: sandboxFull, root: root, appDir: filepath.Join(root, "blog"), appName: "blog",
		writable: []bindPath{{dest: aliasRelease, source: aliasRelease}, {dest: aliasShared, source: aliasShared}}}
	if err := sb.resolveBindSources(); err != nil {
		t.Fatalf("a symlinked root was refused: %v", err)
	}
	got := propMap(unitSpec{ExecStart: []string{"/bin/true"}, Sandbox: sb})
	binds := map[string]bindMount{}
	for _, b := range got["BindPaths"].([]bindMount) {
		binds[b.Destination] = b
	}
	b, ok := binds[aliasShared]
	if !ok {
		t.Fatalf("the shared dir is not bound where the app expects it (%s): %+v", aliasShared, binds)
	}
	// Only the SOURCE moves. The destination stays the configured path,
	// because that is what WorkingDirectory, HOME and the
	// {release_dir}/{shared_dir} placeholders name — binding the
	// resolved path at the resolved path would put the data somewhere
	// the app never looks, and its own spelling would be gone with the
	// tmpfs that replaces the filesystem.
	if b.Source != shared {
		t.Fatalf("shared bound from %q, want the resolved %q", b.Source, shared)
	}
	if _, moved := binds[shared]; moved {
		t.Fatal("the shared dir was bound at its resolved path: the app would never find it")
	}
}

// TestOverlapIsSymmetric: a path that CONTAINS a protected tree
// exposes it exactly as surely as one inside it, because the binds are
// recursive — `shared -> /run` carries /run/user (the manager's
// socket) and /run/hotserve (the admin socket) in with it, and
// a root under one of them would carry every app's data in.
func TestOverlapIsSymmetric(t *testing.T) {
	if err := refusedAsBindSource("/run"); err == nil {
		t.Error("a bind source containing /run/user was accepted at launch time")
	}
	if err := refusedAsBindSource("/etc"); err == nil {
		t.Error("a bind source containing hotserve's own config was accepted at launch time")
	}
}

// TestMandatoryBindMustBeTheDirectoryItNames: inside the root, a
// mandatory bind may differ from its configured path only by an
// ancestor's symlink. Pointing `shared` at the app's own dir or its
// releases passes a naive "is it inside appDir" test, and the
// recursive bind then carries state.json, the upload staging dir and
// every other release in under the shared path — the integrity
// boundary the design states as normative.
func TestMandatoryBindMustBeTheDirectoryItNames(t *testing.T) {
	root := t.TempDir()
	appDir := filepath.Join(root, "blog")
	release := filepath.Join(appDir, "releases", "v1")
	shared := filepath.Join(appDir, "shared")
	for _, d := range []string{release, shared, filepath.Join(appDir, "tmp")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(appDir, "state.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(appDir, "shared-link")
	spec := func(sharedPath string) *sandboxSpec {
		return &sandboxSpec{tier: sandboxFull, root: root, appDir: appDir, appName: "blog",
			writable: []bindPath{{dest: release, source: release}, {dest: sharedPath, source: sharedPath}}}
	}
	for _, target := range []string{appDir, filepath.Join(appDir, "releases"), filepath.Join(appDir, "tmp"), root} {
		_ = os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		err := spec(link).resolveBindSources()
		if err == nil {
			t.Errorf("a shared dir pointing at %s was accepted; its recursive bind would expose state.json, tmp/ and other releases", target)
			continue
		}
		if !strings.Contains(err.Error(), "not the directory it names") {
			t.Errorf("refused for the wrong reason: %v", err)
		}
	}
	// The legitimate difference — an ancestor is itself a symlink — must
	// still resolve cleanly. Built under /run: a t.TempDir root lives
	// under /tmp, which the sandbox replaces, so a source resolving back
	// into it would be refused for that reason instead of this one.
	base, err := os.MkdirTemp("/run", "rootalias-")
	if err != nil {
		t.Skipf("no writable dir outside the closed prefixes: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	realRoot := filepath.Join(base, "real")
	realShared := filepath.Join(realRoot, "blog", "shared")
	if err := os.MkdirAll(realShared, 0o750); err != nil {
		t.Fatal(err)
	}
	aliasRoot := filepath.Join(base, "liveswap")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Fatal(err)
	}
	aliasShared := filepath.Join(aliasRoot, "blog", "shared")
	viaAlias := &sandboxSpec{tier: sandboxFull, root: aliasRoot, appDir: filepath.Join(aliasRoot, "blog"), appName: "blog",
		writable: []bindPath{{dest: aliasShared, source: aliasShared}}}
	if err := viaAlias.resolveBindSources(); err != nil {
		t.Fatalf("a symlinked root must still work: %v", err)
	}
	if got := viaAlias.writable[0]; got.dest != aliasShared || got.source != filepath.Join(realRoot, "blog", "shared") {
		t.Fatalf("through a symlinked root the app must still see its dir where it names it, bound from the real one: %+v", got)
	}
}

// TestAppDirAliasToSiblingRefused: the expected base for a mandatory
// bind is derived from the canonical root plus the configured app
// name, never from what the app's own directory resolves to. Resolving
// appDir would let the alias vouch for itself — a bare app that
// replaces <root>/blog with a link to <root>/shop makes shop's
// directory the expected base, and both of blog's binds then match it
// exactly, handing the sibling's release and shared data to the new
// sandbox.
func TestAppDirAliasToSiblingRefused(t *testing.T) {
	root := t.TempDir()
	sibling := filepath.Join(root, "shop")
	for _, d := range []string{filepath.Join(sibling, "releases", "v1"), filepath.Join(sibling, "shared")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	appDir := filepath.Join(root, "blog")
	if err := os.Symlink(sibling, appDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	release := filepath.Join(appDir, "releases", "v1")
	shared := filepath.Join(appDir, "shared")
	s := &sandboxSpec{tier: sandboxFull, root: root, appDir: appDir, appName: "blog",
		writable: []bindPath{{dest: release, source: release}, {dest: shared, source: shared}}}
	err := s.resolveBindSources()
	if err == nil {
		t.Fatalf("an app dir aliased to the sibling %s was accepted: blog would bind shop's release and shared data", sibling)
	}
	if !strings.Contains(err.Error(), "not the directory it names") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// TestUnitForRefusesCommandOutsideTheView: LookPath runs in the
// supervisor's view of the filesystem, which under a deny-by-default
// sandbox is not the unit's. A runtime installed outside /usr —
// /opt/node/bin/node, an nvm shim under a home directory — resolves
// here and then does not exist in there, and systemd reports that as a
// bare status=203/EXEC after the unit has already been created.
func TestUnitForRefusesCommandOutsideTheView(t *testing.T) {
	r, _ := newTestSystemdRunner(t)
	root := t.TempDir()
	appDir := filepath.Join(root, "blog")
	release := filepath.Join(appDir, "releases", "v1")
	shared := filepath.Join(appDir, "shared")
	for _, d := range []string{release, shared} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	// A runtime outside the base view. Under /var, which is not bound
	// at all (/run is, but the container mounts it noexec, so LookPath
	// would fail there for the wrong reason).
	outside, err := os.MkdirTemp("/var", "runtime-")
	if err != nil {
		t.Skipf("no writable dir outside the base view: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outside) })
	runtime := filepath.Join(outside, "node")
	if err := os.WriteFile(runtime, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(release, "server"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sb := func() *sandboxSpec {
		return &sandboxSpec{tier: sandboxFull, root: root, appDir: appDir, appName: "blog",
			writable: []bindPath{{dest: release, source: release}, {dest: shared, source: shared}}}
	}
	base := startSpec{app: "blog", version: "v1", dir: release}

	spec := base
	spec.command, spec.sandbox = []string{runtime}, sb()
	_, err = r.unitFor(spec, false)
	if err == nil || !strings.Contains(err.Error(), "sandbox off") {
		t.Fatalf("unitFor must refuse a command outside the view and name the fix, got %v", err)
	}
	// There is no way to widen the view, so `sandbox off` is the whole
	// of the answer for a runtime that lives outside /usr.
	// The ordinary cases still pass: a binary in the release dir, and
	// one in the base view.
	for _, cmd := range []string{"./server", "/bin/sh"} {
		spec := base
		spec.command, spec.sandbox = []string{cmd}, sb()
		if _, err := r.unitFor(spec, false); err != nil {
			t.Errorf("%s refused: %v", cmd, err)
		}
	}
	// Unsandboxed, the check does not apply at all.
	spec = base
	spec.command = []string{runtime}
	if _, err := r.unitFor(spec, false); err != nil {
		t.Errorf("an unsandboxed unit must not be subject to the view check: %v", err)
	}
}

// TestSandboxPropertiesNeverEmitsAnEmptyBindList: BindPaths= and
// BindReadOnlyPaths= are two views of one list in the manager, and
// setting either to an empty array resets the whole list rather than
// adding nothing — taking the base view back out and failing every
// such unit with 203/EXEC. The capability probe is exactly this case:
// it has no directories of its own.
func TestSandboxPropertiesNeverEmitsAnEmptyBindList(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec *sandboxSpec
		want []string // properties that must be present
		gone []string // properties that must not be emitted at all
	}{
		{"the capability probe: nothing of its own", probeSandboxSpec(sandboxFull),
			[]string{"BindReadOnlyPaths", "TemporaryFileSystem"}, []string{"BindPaths"}},
		{"an app with no read-only binds of its own", &sandboxSpec{tier: sandboxFull, root: "/var/lib/liveswap",
			writable: []bindPath{{dest: "/var/lib/liveswap/blog/shared"}}},
			[]string{"BindPaths", "BindReadOnlyPaths"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := propMap(unitSpec{ExecStart: []string{"/bin/true"}, Sandbox: tc.spec})
			for _, name := range tc.want {
				if _, ok := got[name]; !ok {
					t.Errorf("%s missing", name)
				}
			}
			for _, name := range tc.gone {
				if _, ok := got[name]; ok {
					t.Errorf("%s emitted empty: it resets the manager's whole bind list, base view included", name)
				}
			}
			for _, name := range []string{"BindPaths", "BindReadOnlyPaths"} {
				if b, ok := got[name].([]bindMount); ok && len(b) == 0 {
					t.Errorf("%s emitted as an empty list", name)
				}
			}
			// Whatever else is missing, the probe must still get an OS
			// it can exec in.
			ro, _ := got["BindReadOnlyPaths"].([]bindMount)
			var sawUsr bool
			for _, b := range ro {
				if b.Destination == "/usr" {
					sawUsr = true
				}
			}
			if !sawUsr {
				t.Error("the base view is missing /usr: the unit cannot exec anything")
			}
		})
	}
}

// TestSymlinkedRootUnderPrivateTmpLaunches: validateSandboxRoot
// deliberately allows a liveswap root under /tmp, /var/tmp or /home —
// the binds nest into the unit's own private copies, and the
// real-systemd integration lane runs from /var/tmp. The launch-time
// check must agree: a root reached through a symlink resolves into one
// of those trees, and refusing it there would make the same root fail
// every launch merely for being spelled with a link.
func TestSymlinkedRootUnderPrivateTmpLaunches(t *testing.T) {
	for _, under := range []string{"/var/tmp", "/tmp"} {
		t.Run(under, func(t *testing.T) {
			real, err := os.MkdirTemp(under, "liveswap-real-")
			if err != nil {
				t.Skipf("cannot create a root under %s: %v", under, err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(real) })
			// The alias lives outside the tree it points at, the way
			// /srv/liveswap -> /var/tmp/ls would.
			base, err := os.MkdirTemp("/var", "liveswap-alias-")
			if err != nil {
				t.Skipf("no writable dir for the alias: %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(base) })
			root := filepath.Join(base, "liveswap")
			if err := os.Symlink(real, root); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			release := filepath.Join(root, "blog", "releases", "v1")
			shared := filepath.Join(root, "blog", "shared")
			for _, d := range []string{release, shared} {
				if err := os.MkdirAll(d, 0o750); err != nil {
					t.Fatal(err)
				}
			}
			s := &sandboxSpec{tier: sandboxFull, root: root, appDir: filepath.Join(root, "blog"), appName: "blog",
				writable: []bindPath{{dest: release, source: release}, {dest: shared, source: shared}}}
			if err := s.resolveBindSources(); err != nil {
				t.Fatalf("a symlinked root under %s must launch, exactly as an unaliased one does: %v", under, err)
			}
			// Only the source moves; the app still finds its dirs where
			// its command line and HOME name them.
			for i, want := range []string{release, shared} {
				if s.writable[i].dest != want {
					t.Errorf("bind %d destination moved to %q, want %q", i, s.writable[i].dest, want)
				}
				if !strings.HasPrefix(s.writable[i].source, real) {
					t.Errorf("bind %d source %q is not under the real root %q", i, s.writable[i].source, real)
				}
			}
			// The deny list still applies to a source that is NOT the
			// directory it names — that is the planted-symlink case, and
			// it must not be let through with the root's alias.
			planted := filepath.Join(root, "blog", "shared-link")
			if err := os.Symlink("/dev", planted); err != nil {
				t.Fatal(err)
			}
			bad := &sandboxSpec{tier: sandboxFull, root: root, appDir: filepath.Join(root, "blog"), appName: "blog",
				writable: []bindPath{{dest: release, source: release}, {dest: planted, source: planted}}}
			if err := bad.resolveBindSources(); err == nil {
				t.Error("a planted shared -> /dev was accepted under a symlinked root")
			}
		})
	}
}

// TestUnitForResolvesTheCommandBeforeTestingTheView: LookPath does not
// follow symlinks, so a shim under a bound directory pointing at an
// unbound one (the vendored-runtime layout /usr/local/bin/node ->
// /opt/node/bin/node) would pass an unresolved check and then die as
// the bare 203/EXEC the check exists to replace.
func TestUnitForResolvesTheCommandBeforeTestingTheView(t *testing.T) {
	r, _ := newTestSystemdRunner(t)
	root := t.TempDir()
	appDir := filepath.Join(root, "blog")
	release := filepath.Join(appDir, "releases", "v1")
	if err := os.MkdirAll(release, 0o750); err != nil {
		t.Fatal(err)
	}
	// The real runtime, outside the base view; /var is bound nowhere.
	outside, err := os.MkdirTemp("/var", "runtime-")
	if err != nil {
		t.Skipf("no writable dir outside the base view: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outside) })
	realBin := filepath.Join(outside, "node")
	if err := os.WriteFile(realBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The shim, inside the release dir (which IS bound), pointing out.
	shim := filepath.Join(release, "node")
	if err := os.Symlink(realBin, shim); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	spec := startSpec{app: "blog", version: "v1", dir: release, command: []string{shim},
		sandbox: &sandboxSpec{tier: sandboxFull, root: root, appDir: appDir, appName: "blog",
			writable: []bindPath{{dest: release, source: release}}}}
	_, err = r.unitFor(spec, false)
	if err == nil {
		t.Fatal("a shim inside the view pointing at a target outside it was accepted")
	}
	if !strings.Contains(err.Error(), realBin) || !strings.Contains(err.Error(), "sandbox off") {
		t.Fatalf("the refusal must name the resolved target and the fix: %v", err)
	}
}

// TestBaseViewNamesTheTrustStoreNotTheTree: /etc/ssl also holds
// /etc/ssl/private. It is 0700 root:root on every cell of the support
// matrix, so binding /etc/ssl disclosed nothing — but a base view
// should name what apps need rather than depend on the permissions of
// what it sweeps up, and the narrower bind costs nothing.
func TestBaseViewNamesTheTrustStoreNotTheTree(t *testing.T) {
	for _, tree := range []string{"/etc/ssl", "/etc/pki", "/etc"} {
		for _, e := range sandboxBaseView {
			if e == tree {
				t.Errorf("base view binds the whole %s tree; name the entries apps need instead", tree)
			}
		}
	}
	for _, needed := range []string{"/etc/ssl/certs", "/etc/pki/tls/certs"} {
		found := false
		for _, e := range sandboxBaseView {
			if e == needed {
				found = true
			}
		}
		if !found {
			t.Errorf("base view lacks %s: TLS verification would fail inside the sandbox", needed)
		}
	}
	// And the private-key directory must not be reachable through any
	// base-view entry, by prefix or otherwise.
	spec := &sandboxSpec{tier: sandboxFull, root: "/var/lib/liveswap", appDir: "/var/lib/liveswap/blog", appName: "blog"}
	if spec.inView("/etc/ssl/private") || spec.inView("/etc/ssl/private/host.key") {
		t.Error("/etc/ssl/private is inside the view")
	}
}

// TestBindSourceInsideTheBaseViewRefused: the base view is the one part
// of the host every unit shares, so an app whose own data resolves into
// it is readable by every sibling — silently, with status still
// reporting the tier as applied. Both the per-app bind and the liveswap
// root itself must refuse it.
func TestBindSourceInsideTheBaseViewRefused(t *testing.T) {
	for _, p := range []string{"/usr/local/app-data", "/usr", "/etc/ssl/certs/x", "/bin/data"} {
		if err := refusedAsBindSource(p); err == nil {
			t.Errorf("a mandatory bind source at %s was accepted: every other app can read it", p)
		} else if !strings.Contains(err.Error(), "EVERY app's sandbox") {
			t.Errorf("%s refused for the wrong reason: %v", p, err)
		}
	}
	// Somewhere genuinely outside the view is still fine — that is the
	// supported "shared moved to another disk" case.
	for _, p := range []string{"/mnt/blog-data", "/srv/blog-data", "/var/cache/blog"} {
		if err := refusedAsBindSource(p); err != nil {
			t.Errorf("%s refused, but it is outside every app's view: %v", p, err)
		}
	}
	for _, bad := range []string{"/usr/local/liveswap", "/usr/share/liveswap", "/etc/ssl/certs/apps"} {
		if err := validateSandboxRoot(bad, true); err == nil {
			t.Errorf("root %s accepted: every app's data would be readable by every other app", bad)
		}
	}
	// Overlap is symmetric: a root that CONTAINS a base-view entry is
	// the same exposure from the other side, because an app's own
	// directory is derived from the root plus its name. `root /etc/ssl`
	// sits inside nothing, and then an app named `certs` lands its
	// releases and shared dir on /etc/ssl/certs — bound read-only and
	// recursively into every sandbox on the box.
	for _, bad := range []string{"/etc/ssl", "/etc", "/"} {
		if err := validateSandboxRoot(bad, true); err == nil {
			t.Errorf("root %s accepted: it contains a base-view entry, so an app named for that entry would be readable by every other app", bad)
		}
	}
	for _, ok := range []string{"/var/lib/liveswap", "/srv/apps", "/mnt/apps", "/tmp/liveswap-test"} {
		if err := validateSandboxRoot(ok, true); err != nil {
			t.Errorf("root %s refused: %v", ok, err)
		}
	}
}

// TestValidateSandboxRootFollowsSymlinks: the root check is lexical, so
// `/srv/liveswap -> /var/lib/hotserve/apps` would otherwise walk past
// the hotserve-state refusal entirely.
func TestValidateSandboxRootFollowsSymlinks(t *testing.T) {
	base, err := os.MkdirTemp("/var", "rootalias-")
	if err != nil {
		t.Skipf("no writable dir for the alias: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	for _, target := range []string{"/var/lib/hotserve", "/usr/local"} {
		if _, err := os.Stat(target); os.IsNotExist(err) {
			if err := os.MkdirAll(target, 0o750); err != nil {
				t.Skipf("cannot create %s: %v", target, err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(target) })
		}
		link := filepath.Join(base, "liveswap")
		_ = os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := validateSandboxRoot(link, true); err == nil {
			t.Errorf("a root symlinked to %s was accepted: the lexical spelling hides it", target)
		}
	}
}

// TestUnitForAcceptsACommandUnderASymlinkedRoot: LookPath is resolved
// before the view test, and a resolved path is canonical — so with a
// symlinked liveswap root the command resolves to the bind's SOURCE
// while the destination keeps the configured spelling. Testing
// destinations alone would refuse every launch on such a host.
func TestUnitForAcceptsACommandUnderASymlinkedRoot(t *testing.T) {
	r, _ := newTestSystemdRunner(t)
	base, err := os.MkdirTemp("/var", "symroot-")
	if err != nil {
		t.Skipf("no writable dir outside the refused prefixes: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	real := filepath.Join(base, "mnt-liveswap")
	release := filepath.Join(real, "blog", "releases", "v1")
	shared := filepath.Join(real, "blog", "shared")
	for _, d := range []string{release, shared} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(release, "server"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "srv-liveswap")
	if err := os.Symlink(real, root); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	aliasRelease := filepath.Join(root, "blog", "releases", "v1")
	spec := startSpec{app: "blog", version: "v1", dir: aliasRelease, command: []string{"./server"},
		sandbox: &sandboxSpec{tier: sandboxFull, root: root, appDir: filepath.Join(root, "blog"), appName: "blog",
			writable: []bindPath{{dest: aliasRelease, source: aliasRelease},
				{dest: filepath.Join(root, "blog", "shared"), source: filepath.Join(root, "blog", "shared")}}}}
	if _, err := r.unitFor(spec, false); err != nil {
		t.Fatalf("a command under a symlinked root must launch, exactly as under an unaliased one: %v", err)
	}
	// The escape this check exists for still fails: a shim inside the
	// release dir pointing at a target outside every bind.
	outside := filepath.Join(base, "runtime")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "node"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "node"), filepath.Join(release, "node")); err != nil {
		t.Fatal(err)
	}
	spec.command = []string{"./node"}
	if _, err := r.unitFor(spec, false); err == nil {
		t.Error("a shim pointing outside every bind was accepted under a symlinked root")
	}
}

// TestPlantedBindOntoTheSupervisorsRealStateRefused: the refusal list
// is advisory for the VIEW — a path it omits is absent anyway, because
// nothing binds it — but a mandatory bind that resolves onto such a
// path *names* it, and naming is what puts something in the view. So
// here the list must be both canonical and derived from where this
// hotserve actually keeps its state, not a set of literals.
func TestPlantedBindOntoTheSupervisorsRealStateRefused(t *testing.T) {
	base, err := os.MkdirTemp("/var", "supervisor-")
	if err != nil {
		t.Skipf("no writable dir outside the refused prefixes: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	// (a) A protected path reached through its canonical spelling: the
	// literal is /var/lib/hotserve, the keys are really elsewhere.
	realState := filepath.Join(base, "srv-hotserve")
	if err := os.MkdirAll(realState, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat("/var/lib/hotserve"); os.IsNotExist(err) {
		if err := os.MkdirAll("/var/lib", 0o755); err != nil {
			t.Skipf("cannot prepare /var/lib: %v", err)
		}
		if err := os.Symlink(realState, "/var/lib/hotserve"); err != nil {
			t.Skipf("cannot alias /var/lib/hotserve: %v", err)
		}
		t.Cleanup(func() { _ = os.Remove("/var/lib/hotserve") })
		if err := refusedAsBindSource(realState); err == nil {
			t.Errorf("a bind onto %s was accepted, but /var/lib/hotserve resolves there — the supervisor's keys", realState)
		}
	}

	// (b) Caddy's data dir where XDG actually points it, which no
	// literal in the package mentions.
	xdg := filepath.Join(base, "xdg-data")
	if err := os.MkdirAll(filepath.Join(xdg, "caddy"), 0o750); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", xdg)
	dataDir := caddy.AppDataDir()
	if !strings.HasPrefix(dataDir, xdg) {
		t.Skipf("caddy.AppDataDir() does not follow XDG_DATA_HOME here (%s)", dataDir)
	}
	if err := refusedAsBindSource(dataDir); err == nil {
		t.Errorf("a bind onto %s was accepted: that is where this hotserve keeps its TLS keys", dataDir)
	}

	// (c) The admin socket directory as systemd actually made it.
	runtimeDir := filepath.Join(base, "run-hotserve")
	if err := os.MkdirAll(runtimeDir, 0o750); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RUNTIME_DIRECTORY", runtimeDir)
	if err := refusedAsBindSource(runtimeDir); err == nil {
		t.Errorf("a bind onto RUNTIME_DIRECTORY %s was accepted: the admin socket lives there", runtimeDir)
	}

	// A neighbour that merely shares a prefix is still fine.
	if err := refusedAsBindSource(filepath.Join(base, "xdg-data-elsewhere")); err != nil {
		t.Errorf("an unrelated sibling path was refused: %v", err)
	}
}

// TestBindOntoASiblingsExternalDataRefused is the finding that removed
// the out-of-root escape hatch. `shop/shared` legitimately resolving to
// /mnt/shop-data used to make /mnt/shop-data an acceptable target for
// ANY app: a bare `blog` could aim its own shared symlink there and
// bind the sibling's data — read-write — into a unit whose status
// reported it sandboxed. Enumerating every other app's external
// locations cannot fix that (it is incomplete by construction, and it
// is the derived, ageing set this model exists to delete); the answer
// is that a mandatory bind must be the directory it names.
func TestBindOntoASiblingsExternalDataRefused(t *testing.T) {
	base, err := os.MkdirTemp("/var", "sibling-")
	if err != nil {
		t.Skipf("no writable dir outside the refused prefixes: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	root := filepath.Join(base, "liveswap")
	shopData := filepath.Join(base, "mnt-shop-data")
	blogRelease := filepath.Join(root, "blog", "releases", "v1")
	for _, d := range []string{blogRelease, shopData} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(shopData, "shop.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// blog, running bare, aims its own shared dir at shop's data.
	blogShared := filepath.Join(root, "blog", "shared")
	if err := os.Symlink(shopData, blogShared); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	spec := &sandboxSpec{tier: sandboxFull, root: root, appDir: filepath.Join(root, "blog"), appName: "blog",
		writable: []bindPath{{dest: blogRelease, source: blogRelease}, {dest: blogShared, source: blogShared}}}
	err = spec.resolveBindSources()
	if err == nil {
		t.Fatalf("blog was allowed to bind %s — a sibling's data — as its own shared dir", shopData)
	}
	if !strings.Contains(err.Error(), "not the directory it names") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	// Nothing was rewritten: the launch fails rather than mounting.
	if spec.writable[1].source != blogShared {
		t.Errorf("bind source was rewritten despite the refusal: %+v", spec.writable)
	}
	// An env_file directory belonging to another app is the same shape
	// and is refused by the same rule — no list of secret locations to
	// keep current.
	secrets := filepath.Join(base, "etc-shop")
	if err := os.MkdirAll(secrets, 0o750); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(blogShared)
	if err := os.Symlink(secrets, blogShared); err != nil {
		t.Fatal(err)
	}
	if err := spec.resolveBindSources(); err == nil {
		t.Errorf("blog was allowed to bind another app's env_file directory %s", secrets)
	}
}

// TestAppViewsAreDisjointExceptTheBaseView is the app-to-app boundary
// as an executable invariant rather than something a reviewer has to
// notice. Three separate review findings on this PR were the same
// defect wearing different clothes — one app reaching another's data,
// via an out-of-root bind, via the shared base view, via a root inside
// it — and each was caught one at a time by inspection. This asserts
// the property directly: for any two apps, the only paths their views
// share are the base view, which is read-only and identical for
// everyone by design.
//
// It is deliberately written over the whole rendered property set, not
// over sandboxSpec, so it also covers how the binds are emitted.
func TestAppViewsAreDisjointExceptTheBaseView(t *testing.T) {
	root := t.TempDir()
	view := func(name string) map[string]bindMount {
		spec := &appSpec{name: name, dirs: newAppDirs(root, name)}
		sb := spec.sandboxSpecFor(spec.dirs.release("v1"), sandboxFull)
		got := propMap(unitSpec{ExecStart: []string{"/bin/true"}, Sandbox: sb})
		m := map[string]bindMount{}
		for _, prop := range []string{"BindPaths", "BindReadOnlyPaths"} {
			if bs, ok := got[prop].([]bindMount); ok {
				for _, b := range bs {
					m[b.Destination] = b
				}
			}
		}
		return m
	}
	base := map[string]bool{}
	for _, p := range sandboxBaseView {
		base[p] = true
	}

	blog := view("blog")
	shop := view("shop")
	for dest, b := range blog {
		if base[dest] {
			if !b.IgnoreENOENT {
				t.Errorf("base-view entry %s is not optional", dest)
			}
			continue
		}
		// Not a base-view entry: it must be blog's own, and it must not
		// appear in shop's view at all.
		if _, shared := shop[dest]; shared {
			t.Errorf("%s is in both blog's and shop's view", dest)
		}
		if !strings.HasPrefix(dest, filepath.Join(root, "blog")+"/") && dest != filepath.Join(root, "blog") {
			t.Errorf("blog's view contains %s, which is not under its own app directory", dest)
		}
	}
	// Every writable path in a view must be the app's own. This is the
	// half that matters: a read-only leak is a disclosure, a writable
	// one lets an app corrupt a sibling.
	for name, other := range map[string]string{"blog": "shop", "shop": "blog"} {
		got := propMap(unitSpec{ExecStart: []string{"/bin/true"},
			Sandbox: (&appSpec{name: name, dirs: newAppDirs(root, name)}).sandboxSpecFor(
				newAppDirs(root, name).release("v1"), sandboxFull)})
		for _, b := range got["BindPaths"].([]bindMount) {
			if !strings.HasPrefix(b.Destination, filepath.Join(root, name)+"/") {
				t.Errorf("%s has a writable bind at %s, outside its own app directory", name, b.Destination)
			}
			if strings.HasPrefix(b.Destination, filepath.Join(root, other)+"/") {
				t.Errorf("%s has a writable bind into %s's directory: %s", name, other, b.Destination)
			}
			if b.Source != b.Destination && strings.HasPrefix(b.Source, filepath.Join(root, other)) {
				t.Errorf("%s's bind at %s is taken FROM %s's data (%s)", name, b.Destination, other, b.Source)
			}
		}
	}
	// And an app's own state must not be in its own view either — the
	// integrity boundary the design states as normative.
	d := newAppDirs(root, "blog")
	for _, p := range []string{d.app, d.state, d.tmp, d.current, d.releases} {
		if _, present := blog[p]; present {
			t.Errorf("blog's view contains %s", p)
		}
	}
}

// TestSandboxCapabilityCachedPerConnection pins the rule that keeps a
// throwaway unit off Caddy's critical path: a delivered capability is
// measured at most once per manager connection, and again after a
// redial. Drives the caching directly, so it needs no manager to dial.
func TestSandboxCapabilityCachedPerConnection(t *testing.T) {
	c := &userManagerClient{}
	measured := 0
	full := func() sandboxCapability {
		measured++
		return sandboxCapability{tier: sandboxFull}
	}

	c.generation.Add(1) // a first dial
	for i := 0; i < 5; i++ {
		if got := c.cachedSandboxCapability(full); got.tier != sandboxFull {
			t.Fatalf("call %d: tier = %v, want full", i, got.tier)
		}
	}
	if measured != 1 {
		t.Fatalf("measured %d times across 5 config loads, want 1: the probe starts a unit and belongs to the connection", measured)
	}

	// A redial invalidates it: the manager that restarted may not be
	// the one that was measured.
	c.generation.Add(1)
	if got := c.cachedSandboxCapability(full); got.tier != sandboxFull {
		t.Fatalf("after redial: tier = %v", got.tier)
	}
	if measured != 2 {
		t.Fatalf("measured %d times, want 2: a redial must re-measure", measured)
	}

	// A redial *during* a measurement means it described a manager that
	// is no longer current: report it, cache nothing, measure again.
	c.generation.Add(1)
	gen := c.generation.Load()
	racing := func() sandboxCapability {
		measured++
		c.generation.Add(1)
		return sandboxCapability{tier: sandboxFull, reason: "raced"}
	}
	if got := c.cachedSandboxCapability(racing); got.reason != "raced" {
		t.Fatalf("the caller that asked must still get the measurement it took: %+v", got)
	}
	if c.sandboxGen == gen {
		t.Fatal("a measurement taken across a redial was cached against the stale generation")
	}

	// Generation 0 is "never dialed": nothing to cache against, in
	// either direction. The write guard is asserted as well as the
	// read one, so the two halves cannot disagree about the sentinel.
	fresh := &userManagerClient{}
	measured = 0
	fresh.cachedSandboxCapability(full)
	fresh.cachedSandboxCapability(full)
	if measured != 2 {
		t.Fatalf("measured %d times at generation 0, want 2: no connection means nothing to cache against", measured)
	}
	if fresh.sandboxGen != 0 {
		t.Fatalf("sandboxGen = %d after measuring with no connection; 0 is reserved for \"never measured\"", fresh.sandboxGen)
	}
}

// TestSandboxFailedMeasurementIsNotCached pins the half of the caching
// rule that decides whether a bad minute costs a host its sandbox for
// the life of a connection. probeSandboxCapability reports the same
// {none, reason} whether the namespaces are genuinely unavailable or
// the probe unit merely timed out under boot load, and a timeout
// leaves the connection Connected() — so nothing would ever drop it
// and re-measure.
func TestSandboxFailedMeasurementIsNotCached(t *testing.T) {
	c := &userManagerClient{}
	c.generation.Add(1)
	measured := 0
	failing := func() sandboxCapability {
		measured++
		return sandboxCapability{tier: sandboxNone, reason: "full tier: unit failed: probe timed out"}
	}
	for i := 0; i < 3; i++ {
		if got := c.cachedSandboxCapability(failing); got.tier != sandboxNone {
			t.Fatalf("call %d: tier = %v, want none", i, got.tier)
		}
	}
	if measured != 3 {
		t.Fatalf("measured %d times, want 3: a failed verdict must not pin every app unsandboxed until a redial", measured)
	}
	if c.sandboxGen != 0 {
		t.Fatal("a failed verdict was cached")
	}
	// And once the host answers, that verdict is cached as usual.
	if got := c.cachedSandboxCapability(func() sandboxCapability { return sandboxCapability{tier: sandboxFull} }); got.tier != sandboxFull {
		t.Fatalf("tier = %v, want full", got.tier)
	}
	if c.cachedSandboxCapability(failing); measured != 3 {
		t.Fatalf("measured %d: a cached success must still be served", measured)
	}
}

// TestSandboxedStartFailureForgetsCapability pins the invalidation the
// connection generation cannot provide. The capability is cached
// against the manager connection, but what it measures is the kernel
// and the LSM: user.max_user_namespaces, an AppArmor policy reload or
// a container limit can take the namespaces away while the connection
// stays up. Without this the cached `full` would stand and `sandbox
// auto` would keep choosing a tier whose units no longer start —
// failing every deploy, where auto's contract is to degrade with a
// WARN and keep serving.
func TestSandboxedStartFailureForgetsCapability(t *testing.T) {
	c := &userManagerClient{}
	c.generation.Add(1)
	measured := 0
	full := func() sandboxCapability {
		measured++
		return sandboxCapability{tier: sandboxFull}
	}
	if got := c.cachedSandboxCapability(full); got.tier != sandboxFull || measured != 1 {
		t.Fatalf("tier=%v measured=%d", got.tier, measured)
	}

	// A unit that failed to start WITHOUT a sandbox says nothing about
	// the host's namespaces, and must not cost a measurement.
	r := &systemdRunner{conn: c}
	r.sandboxedStartFailed(unitSpec{Name: "bare.service"})
	r.sandboxedStartFailed(unitSpec{Name: "off.service", Sandbox: &sandboxSpec{tier: sandboxNone}})
	if c.cachedSandboxCapability(full); measured != 1 {
		t.Fatalf("measured %d: an unsandboxed start failure is not evidence about the sandbox", measured)
	}

	// One that failed WITH a sandbox applied is. Driven through Start,
	// not by calling the helper: the wiring is the thing that can be
	// deleted, and asserting the helper alone leaves Start free to
	// stop calling it with every test still green.
	r.sandboxedStartFailed(unitSpec{Name: "app.service", Sandbox: &sandboxSpec{tier: sandboxFull}})
	if c.sandboxGen != 0 {
		t.Fatal("a sandboxed start failure left the stale capability cached")
	}
	degraded := func() sandboxCapability {
		measured++
		return sandboxCapability{tier: sandboxNone, reason: "full tier: unit failed: Permission denied"}
	}
	got := c.cachedSandboxCapability(degraded)
	if got.tier != sandboxNone || measured != 2 {
		t.Fatalf("tier=%v measured=%d: the next start must re-measure the host", got.tier, measured)
	}
	// And auto now degrades with a warning instead of selecting a tier
	// whose units cannot start.
	tier, warn, err := resolveSandboxTier(sandboxAuto, got)
	if err != nil || tier != sandboxNone || warn == "" {
		t.Fatalf("auto must degrade with a WARN: tier=%v warn=%q err=%v", tier, warn, err)
	}
}

// forgetCountingConn is a fake connection that also carries the
// optional capability cache, so a test can see Start reach for it.
type forgetCountingConn struct {
	*fakeSystemdConn
	forgotten int
}

func (f *forgetCountingConn) forgetSandboxCapability() { f.forgotten++ }

// TestStartForgetsCapabilityOnSandboxedFailure pins the WIRING, not the
// helper: that Start's failure path is what reports a sandboxed unit
// the manager refused. Without this, deleting the one call from Start
// leaves every other sandbox test green while the stale capability
// survives and auto keeps choosing a tier whose units cannot start.
func TestStartForgetsCapabilityOnSandboxedFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sandbox *sandboxSpec
		want    int
	}{
		{"sandboxed unit the manager refused", &sandboxSpec{tier: sandboxFull, root: "/var/lib/liveswap"}, 1},
		{"bare unit that failed for its own reasons", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := &forgetCountingConn{fakeSystemdConn: newFakeSystemdConn()}
			conn.startResult = "failed"
			conn.failStatus = &unitStatus{LoadState: "loaded", ActiveState: "failed", Result: "exit-code", ExecMainCode: 1, ExecMainStatus: 226}
			r := newSystemdRunner(conn, zap.NewNop())
			t.Cleanup(r.cancel)
			if _, err := r.Start(startSpec{app: "demo", version: "v1", command: []string{"/bin/true"}, sandbox: tc.sandbox}); err == nil {
				t.Fatal("a start job that did not return done must be an error")
			}
			if conn.forgotten != tc.want {
				t.Fatalf("capability forgotten %d times, want %d", conn.forgotten, tc.want)
			}
		})
	}
}
