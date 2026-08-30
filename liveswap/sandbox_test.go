package liveswap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestSandboxTierRoundTrip(t *testing.T) {
	for _, tier := range []sandboxTier{sandboxNone, sandboxFilesystem, sandboxFull} {
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
	fs := sandboxCapability{tier: sandboxFilesystem, reason: "full tier: the user manager is systemd 255, PrivatePIDs= needs 256"}
	none := sandboxCapability{tier: sandboxNone, reason: "filesystem tier: unit failed"}
	cases := []struct {
		name     string
		mode     string
		cap      sandboxCapability
		wantTier sandboxTier
		wantWarn bool
		wantErr  bool
	}{
		{"auto full", sandboxAuto, full, sandboxFull, false, false},
		{"auto filesystem warns", sandboxAuto, fs, sandboxFilesystem, true, false},
		{"auto none warns", sandboxAuto, none, sandboxNone, true, false},
		{"require full", sandboxRequire, full, sandboxFull, false, false},
		{"require refuses filesystem", sandboxRequire, fs, sandboxNone, false, true},
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

func TestValidateExtraPathAndRoot(t *testing.T) {
	root := "/var/lib/liveswap"
	hidden := append(append([]string{}, sandboxHiddenFloor...), "/var/lib/hotserve/caddy", "/etc/blog/blog.env")
	for _, ok := range []string{"/run/postgresql", "/opt/geoip", "/var/cache/blog", "/srv/media"} {
		if err := validateExtraPath(ok, root, hidden); err != nil {
			t.Errorf("%s rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"relative/path", "/opt/../etc", "/opt/", "/",
		"/var/lib/liveswap", "/var/lib/liveswap/other/shared", // the root: siblings live there
		"/var/lib/hotserve", "/var/lib/hotserve/caddy", // TLS keys
		"/run/hotserve", "/etc/hotserve", "/etc/liveswap/blog.env",
		"/etc/blog/blog.env", // a derived hidden path (another app's env_file)
	} {
		if err := validateExtraPath(bad, root, hidden); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	// The sharp cases: anything the sandbox options close by themselves
	// must not be bindable back — /run/user is the manager's private
	// socket (a sandbox escape), /sys/fs/cgroup undoes the read-only
	// cgroupfs the resource caps will rely on. Read-only is no defence:
	// connecting to a unix socket is not a filesystem write.
	for _, closed := range []string{
		"/run/user", "/run/user/997", "/run/user/997/systemd/private",
		"/home", "/home/deploy/data", "/root",
		"/tmp", "/var/tmp/build", "/dev", "/dev/shm",
		"/sys", "/sys/fs/cgroup", "/proc", "/proc/self",
	} {
		err := validateExtraPath(closed, root, hidden)
		if err == nil {
			t.Errorf("%s accepted: it would hand back what the sandbox closes", closed)
			continue
		}
		if !strings.Contains(err.Error(), "sandbox itself closes") {
			t.Errorf("%s refused for the wrong reason: %v", closed, err)
		}
	}
	// A prefix that merely shares characters with a closed or hidden
	// path is fine.
	for _, ok := range []string{"/var/lib/hotserve-data", "/tmpfiles", "/devices", "/run/userdata"} {
		if err := validateExtraPath(ok, root, hidden); err != nil {
			t.Errorf("%s rejected: %v", ok, err)
		}
	}
	if err := validateSandboxRoot("/var/lib/liveswap", hidden); err != nil {
		t.Errorf("default root rejected: %v", err)
	}
	// A root inside hotserve's own state is a config error worth
	// failing on; a root the sandbox merely cannot mount over degrades
	// the tier instead (TestSandboxRootDegradesRatherThanRefusing).
	for _, bad := range []string{"/var/lib/hotserve/apps", "/etc/hotserve/apps"} {
		if err := validateSandboxRoot(bad, hidden); err == nil {
			t.Errorf("root %s accepted: it is inside hotserve's own state", bad)
		}
	}
}

// TestHiddenPathsDerived: the hidden set is what this hotserve actually
// uses, not the packaged layout's literals — otherwise an env_file or a
// Caddy data dir outside the convention stays readable by every sibling
// while the status endpoint reports the app as sandboxed.
func TestHiddenPathsDerived(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/srv/hotserve-data")
	t.Setenv("XDG_CONFIG_HOME", "/srv/hotserve-config")
	t.Setenv("RUNTIME_DIRECTORY", "/run/hotserve-alt")
	a := &App{Apps: map[string]*AppConfig{
		"blog": {EnvFile: "/etc/blog/blog.env"},
		"shop": {EnvFile: "/srv/shop/.env"},
		"bare": {},
	}}
	got := a.hiddenPaths()
	for _, want := range append(append([]string{}, sandboxHiddenFloor...),
		"/srv/hotserve-data/caddy", "/srv/hotserve-config/caddy", "/run/hotserve-alt",
		"/etc/blog/blog.env", "/srv/shop/.env") {
		found := false
		for _, h := range got {
			if h == want {
				found = true
			}
		}
		if !found {
			t.Errorf("hidden set lacks %s: %v", want, got)
		}
	}
	// Deterministic: specEqual compares specs by their rendering, so a
	// map-order-dependent set would make every reload look like a
	// config change.
	if second := a.hiddenPaths(); !reflect.DeepEqual(got, second) {
		t.Fatalf("hiddenPaths is not deterministic:\n%v\n%v", got, second)
	}
	// An app's env_file is hidden as the file, not its directory: an
	// operator's /etc/blog/blog.env must not take /etc/blog with it.
	for _, h := range got {
		if h == "/etc/blog" || h == "/srv/shop" {
			t.Errorf("hidden set contains the env_file's whole directory %s: %v", h, got)
		}
	}
}

// sandboxPropertyNames is the set sandboxProperties emits; the test
// pins it so a property can neither disappear nor appear unnoticed.
var sandboxPropertyNames = []string{
	"PrivateUsers", "ProtectSystem", "ProtectHome", "PrivateTmp", "PrivateDevices",
	"ProtectControlGroups", "ProtectKernelTunables", "ProtectKernelModules", "ProtectKernelLogs",
	"RestrictNamespaces", "RestrictRealtime", "RestrictSUIDSGID", "LockPersonality",
	"RestrictAddressFamilies", "SystemCallFilter", "SystemCallErrorNumber", "CapabilityBoundingSet",
	"InaccessiblePaths", "TemporaryFileSystem", "BindPaths", "BindReadOnlyPaths", "UnsetEnvironment",
}

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
		for _, name := range append(sandboxPropertyNames, "PrivatePIDs") {
			if _, present := got[name]; present {
				t.Errorf("%s set on an unsandboxed unit", name)
			}
		}
		if got["NoNewPrivileges"] != true {
			t.Error("the floor (NoNewPrivileges) must stay on every unit")
		}
	}
}

func TestSandboxPropertiesFilesystemAndFull(t *testing.T) {
	spec := &sandboxSpec{
		tier:     sandboxFilesystem,
		root:     "/var/lib/liveswap",
		writable: []bindPath{{dest: "/var/lib/liveswap/blog/releases/v3", source: "/var/lib/liveswap/blog/releases/v3"},
			{dest: "/var/lib/liveswap/blog/shared", source: "/var/lib/liveswap/blog/shared"}},
		extra:    []extraPath{{path: "/run/postgresql"}, {path: "/var/cache/blog", rw: true}},
		hidden:   append(append([]string{}, sandboxHiddenFloor...), "/var/lib/hotserve/caddy", "/etc/blog/blog.env"),
	}
	got := propMap(unitSpec{ExecStart: []string{"/bin/true"}, Sandbox: spec})
	for _, name := range sandboxPropertyNames {
		if _, present := got[name]; !present {
			t.Errorf("%s missing", name)
		}
	}
	if _, present := got["PrivatePIDs"]; present {
		t.Error("PrivatePIDs must not be set on the filesystem tier (unknown property below systemd 256)")
	}
	for name, want := range map[string]any{
		"PrivateUsers":          true,
		"ProtectSystem":         "strict",
		"ProtectHome":           "tmpfs",
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
	hidden, _ := got["InaccessiblePaths"].([]string)
	for _, p := range spec.hidden {
		found := false
		for _, h := range hidden {
			if h == "-"+p {
				found = true
			}
		}
		if !found {
			t.Errorf("InaccessiblePaths lacks -%s: %v", p, hidden)
		}
	}
	if tmp, _ := got["TemporaryFileSystem"].([]tmpfsMount); !reflect.DeepEqual(tmp, []tmpfsMount{{"/var/lib/liveswap", "ro"}}) {
		t.Errorf("TemporaryFileSystem = %+v", got["TemporaryFileSystem"])
	}
	binds := func(name string) map[string]bindMount {
		m := map[string]bindMount{}
		for _, b := range got[name].([]bindMount) {
			m[b.Destination] = b
		}
		return m
	}
	rw := binds("BindPaths")
	for _, p := range []string{"/var/lib/liveswap/blog/releases/v3", "/var/lib/liveswap/blog/shared", "/var/cache/blog"} {
		b, ok := rw[p]
		if !ok || b.Source != p || b.Flags != mountRecursive || b.IgnoreENOENT {
			t.Errorf("BindPaths lacks a recursive, mandatory bind of %s at its real path: %+v", p, rw)
		}
	}
	ro := binds("BindReadOnlyPaths")
	if b, ok := ro["/run/postgresql"]; !ok || b.Source != "/run/postgresql" || b.IgnoreENOENT {
		t.Errorf("ro extra_path missing or optional: %+v", ro)
	}
	if b, ok := ro["/run/systemd/resolve"]; !ok || !b.IgnoreENOENT {
		t.Errorf("/run/systemd/resolve must be bound read-only, optional: %+v", ro)
	}
	if _, leaked := rw["/run/postgresql"]; leaked {
		t.Error("a read-only extra_path must not be in BindPaths")
	}
	if unset, _ := got["UnsetEnvironment"].([]string); !reflect.DeepEqual(unset, []string{"XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"}) {
		t.Errorf("UnsetEnvironment = %v", unset)
	}

	spec.tier = sandboxFull
	got = propMap(unitSpec{ExecStart: []string{"/bin/true"}, Sandbox: spec})
	if got["PrivatePIDs"] != "yes" {
		t.Errorf("full tier must set PrivatePIDs=yes, got %v", got["PrivatePIDs"])
	}
	if got["PrivateUsers"] != true {
		t.Error("full tier keeps the explicit user namespace")
	}
}

func TestSandboxSpecFor(t *testing.T) {
	spec := testSpec(t)
	spec.extraPaths = []extraPath{{path: "/run/postgresql"}}
	spec.sandboxHidden = []string{"/var/lib/hotserve", "/etc/blog/blog.env"}
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
	if !reflect.DeepEqual(got.extra, spec.extraPaths) {
		t.Fatalf("extra = %v", got.extra)
	}
	if !reflect.DeepEqual(got.hidden, spec.sandboxHidden) {
		t.Fatalf("hidden = %v, want %v", got.hidden, spec.sandboxHidden)
	}
	if !filepath.IsAbs(got.root) {
		t.Fatalf("root must be absolute: %q", got.root)
	}
}

func TestSandboxProbeCommand(t *testing.T) {
	fs := sandboxProbeCommand(sandboxFilesystem)
	full := sandboxProbeCommand(sandboxFull)
	if fs[0] != "/bin/sh" || fs[1] != "-c" || len(fs) != 3 {
		t.Fatalf("probe must be a single sh -c script, got %v", fs)
	}
	if !strings.Contains(fs[2], "uid_map") || strings.Contains(fs[2], `= 1`) {
		t.Fatalf("filesystem probe checks the user namespace only: %q", fs[2])
	}
	// systemd turns "$$" into "$" in ExecStart arguments, so the shell
	// pid must be spelled "$$$$" and a bare "$$" must never appear.
	if !strings.Contains(full[2], "uid_map") || !strings.Contains(full[2], `"$$$$" = 1`) {
		t.Fatalf("full probe checks both namespaces: %q", full[2])
	}
	for _, s := range []string{fs[2], full[2]} {
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
		version   int
		fail      map[sandboxTier]error
		wantTier  sandboxTier
		wantTried []sandboxTier
		wantWords []string
	}{
		{"257 full", 257, nil, sandboxFull, []sandboxTier{sandboxFull}, nil},
		{"257 kernel ignores PID ns", 257, map[sandboxTier]error{sandboxFull: errors.New("exit status 1")}, sandboxFilesystem, []sandboxTier{sandboxFull, sandboxFilesystem}, []string{"full tier", "exit status 1"}},
		{"255 filesystem", 255, nil, sandboxFilesystem, []sandboxTier{sandboxFilesystem}, []string{"systemd 255", "256"}},
		{"252 filesystem", 252, nil, sandboxFilesystem, []sandboxTier{sandboxFilesystem}, []string{"systemd 252"}},
		{"unknown version never tries PrivatePIDs", 0, nil, sandboxFilesystem, []sandboxTier{sandboxFilesystem}, nil},
		{"userns denied", 257, map[sandboxTier]error{sandboxFull: denied, sandboxFilesystem: denied}, sandboxNone, []sandboxTier{sandboxFull, sandboxFilesystem}, []string{"Permission denied", "filesystem tier"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &probeRunner{fakeRunner: &fakeRunner{}, fail: tc.fail}
			got := probeSandboxCapability(r, tc.version, "/var/lib/liveswap", sandboxHiddenFloor)
			if got.tier != tc.wantTier {
				t.Fatalf("tier = %v (%s), want %v", got.tier, got.reason, tc.wantTier)
			}
			if !reflect.DeepEqual(r.tried, tc.wantTried) {
				t.Fatalf("tried %v, want %v", r.tried, tc.wantTried)
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
				if s.sandbox == nil || s.sandbox.root != "/var/lib/liveswap" || len(s.sandbox.writable) != 0 ||
					!reflect.DeepEqual(s.sandbox.hidden, sandboxHiddenFloor) {
					t.Errorf("probe unit must carry the tier's sandbox over the root and hidden set, exposing nothing: %+v", s.sandbox)
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
	// And a recorded filesystem-tier instance relaunches at that tier.
	rig3 := newTestRig(t)
	rig3.spec.sandboxTier = sandboxFull
	must(t, rig3.store.save(appState{CurrentVersion: "v2", Port: 1, Handle: handleState{Unit: "hotserve-demo.v2.abc.service", Sandbox: "filesystem"}}))
	must(t, mkdirRelease(rig3.spec, "v2"))
	if err := rig3.ma.ensureRunning(); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got := rig3.runner.started[0].sandbox; got == nil || got.tier != sandboxFilesystem {
		t.Fatalf("recorded tier not reproduced: %+v", got)
	}
}

func TestCaddyfileSandboxDirectives(t *testing.T) {
	d := caddyfile.NewTestDispenser(`liveswap {
		sandbox require
		app blog {
			command node server.js
			sandbox auto
			extra_path /run/postgresql
			extra_path /var/cache/blog rw
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
	want := []ExtraPathConfig{{Path: "/run/postgresql"}, {Path: "/var/cache/blog", Writable: true}}
	if !reflect.DeepEqual(a.Apps["blog"].ExtraPaths, want) {
		t.Fatalf("extra_paths = %+v", a.Apps["blog"].ExtraPaths)
	}
	for _, bad := range []string{
		"liveswap {\n app x {\n command a\n extra_path /p ro\n }\n}",
		"liveswap {\n app x {\n command a\n extra_path\n }\n}",
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
		"relative extra":  func(a *App) { a.Apps["blog"].ExtraPaths = []ExtraPathConfig{{Path: "data"}} },
		"extra in root":   func(a *App) { a.Apps["blog"].ExtraPaths = []ExtraPathConfig{{Path: "/var/lib/liveswap/api/shared"}} },
		"extra hidden":    func(a *App) { a.Apps["blog"].ExtraPaths = []ExtraPathConfig{{Path: "/run/hotserve"}} },
		"root hidden":     func(a *App) { a.Root = "/var/lib/hotserve/apps" },
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

// TestStartResolvesSandboxPolicy drives App.Start with a scripted host:
// require on a filesystem-only host refuses to start; auto starts and
// warns; off never probes.
func TestStartResolvesSandboxPolicy(t *testing.T) {
	origProbe, origManager := probeSandbox, probeUserManager
	t.Cleanup(func() { probeSandbox, probeUserManager = origProbe, origManager })
	probeUserManager = func() error { return nil }
	probes := 0
	probeSandbox = func(string, []string, *zap.Logger) sandboxCapability {
		probes++
		return sandboxCapability{tier: sandboxFilesystem, reason: "full tier: the user manager is systemd 255, PrivatePIDs= needs 256"}
	}
	newApp := func(mode string) *App {
		spec := testSpec(t)
		spec.sandboxMode = mode
		return &App{
			Root:    spec.dirs.root,
			logger:  zap.NewNop(),
			specs:   map[string]*appSpec{"demo": spec},
			managed: map[string]*managedApp{},
			clients: &fetchClients{},
		}
	}
	a := newApp(sandboxRequire)
	if err := a.Start(); err == nil || !strings.Contains(err.Error(), "systemd 255") {
		t.Fatalf("require on a filesystem-only host must refuse with the reason, got %v", err)
	}
	a = newApp(sandboxAuto)
	if err := a.Start(); err != nil {
		t.Fatalf("auto must start: %v", err)
	}
	if a.specs["demo"].sandboxTier != sandboxFilesystem {
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
		if err := validateSandboxRoot(root, sandboxHiddenFloor); err != nil {
			t.Errorf("root %s must not fail config load: %v", root, err)
		}
	}
}

// TestRelaunchBelowFullWarns pins the "prominent WARN at every spawn":
// a relaunch that reproduces a bare (or filesystem-tier) instance
// while policy is auto logs it, naming the residual; a full-tier
// launch and sandbox off do not.
func TestRelaunchBelowFullWarns(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     string
		recorded string
		want     int
	}{
		{"auto, bare record", sandboxAuto, "", 1},
		{"auto, filesystem record", sandboxAuto, "filesystem", 1},
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

// TestValidateExtraPathFollowsSymlinks: the containment checks are
// lexical, but BindPaths= binds what a path resolves to, so a link
// pointing into a closed or hidden area must be refused by what it
// resolves to, not by how it is spelled.
func TestValidateExtraPathFollowsSymlinks(t *testing.T) {
	// The link itself must live outside every closed prefix, or the
	// lexical check would refuse it before symlinks matter — /run
	// qualifies (only /run/user is closed).
	dir, err := os.MkdirTemp("/run", "extrapath-")
	if err != nil {
		t.Skipf("no writable dir outside the closed prefixes: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	// A real directory standing in for hotserve's own state, so the
	// hidden branch is exercised by something that actually resolves
	// (EvalSymlinks needs every component to exist; a link to a path
	// that does not is checked as written — the documented limit).
	secrets := filepath.Join(dir, "secrets")
	if err := os.MkdirAll(secrets, 0o700); err != nil {
		t.Fatal(err)
	}
	hidden := append(append([]string{}, sandboxHiddenFloor...), secrets)
	for _, target := range []string{"/dev", "/tmp", secrets} {
		link := filepath.Join(dir, "link")
		_ = os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		err := validateExtraPath(link, "/var/lib/liveswap", hidden)
		if err == nil {
			t.Errorf("a symlink to %s was accepted: BindPaths would follow it", target)
			continue
		}
		if !strings.Contains(err.Error(), "resolves to") {
			t.Errorf("symlink to %s refused without naming the resolution: %v", target, err)
		}
	}
	// A link to somewhere legitimate still passes.
	ok := filepath.Join(dir, "fine")
	if err := os.Symlink(dir, ok); err != nil {
		t.Fatal(err)
	}
	if err := validateExtraPath(ok, "/var/lib/liveswap", hidden); err != nil {
		t.Errorf("a symlink to an allowed path was refused: %v", err)
	}
}

// TestHiddenPathsResolvesRelativeEnvFile: a relative env_file is valid
// configuration (parseEnvFile reads it through os.ReadFile, against
// this process's working directory), so the hidden set must resolve it
// the same way rather than drop it and leave the file readable by
// every sibling.
func TestHiddenPathsResolvesRelativeEnvFile(t *testing.T) {
	a := &App{Apps: map[string]*AppConfig{"blog": {EnvFile: "secrets/blog.env"}}}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(wd, "secrets/blog.env")
	found := false
	for _, h := range a.hiddenPaths() {
		if h == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("relative env_file not resolved into the hidden set (want %s): %v", want, a.hiddenPaths())
	}
}

// TestSpecsHideEveryAppsResolvedEnvFile is the ordering invariant
// Provision relies on: the hidden set is derived from every app's
// env_file, so defaults (which resolve {env.*}) must be applied to all
// apps before the first spec is built. Building in one pass over an
// unordered map recorded a sibling's unresolved path and left that
// app's real env file visible.
func TestSpecsHideEveryAppsResolvedEnvFile(t *testing.T) {
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
	for _, cfg := range a.Apps { // pass one: defaults for every app
		cfg.applyDefaults(repl)
	}
	for name, cfg := range a.Apps { // pass two: specs
		spec, err := a.buildSpec(name, cfg)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"/etc/secrets/blog.env", "/etc/secrets/shop.env"} {
			found := false
			for _, h := range spec.sandboxHidden {
				if h == want {
					found = true
				}
			}
			if !found {
				t.Errorf("%s's sandbox does not hide %s: %v", name, want, spec.sandboxHidden)
			}
		}
	}
}

// TestValidateRejectsUncleanRoot: every containment check is lexical,
// so a root spelled with .. would let an extra_path name a sibling
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
	hotserveState := filepath.Join(outside, "fake-hotserve")
	elsewhere := filepath.Join(outside, "data")
	for _, d := range []string{release, shared, sibling, hotserveState, elsewhere} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	hidden := []string{hotserveState}
	spec := func(sharedPath string) *sandboxSpec {
		return &sandboxSpec{tier: sandboxFull, root: root, appDir: appDir,
			writable: []bindPath{{dest: release, source: release}, {dest: sharedPath, source: sharedPath}},
			hidden:   hidden}
	}
	// Baseline: real directories resolve to themselves and are kept.
	s := spec(shared)
	if err := s.resolveBindSources(hidden); err != nil {
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
	for _, tc := range []struct{ name, target, wantIn string }{
		{"a closed tree", "/dev", "sandbox itself closes"},
		{"the private tmp", "/tmp", "sandbox itself closes"},
		{"hotserve's own state", hotserveState, "hidden from every app"},
		{"a sibling app's data", sibling, "belongs to another app"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Remove(link)
			if err := os.Symlink(tc.target, link); err != nil {
				t.Fatal(err)
			}
			err := spec(link).resolveBindSources(hidden)
			if err == nil {
				t.Fatalf("a shared dir pointing at %s was bound into the sandbox", tc.target)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Fatalf("refused for the wrong reason: %v", err)
			}
		})
	}

	// A shared dir legitimately moved to another disk (outside the
	// root, and nowhere the sandbox protects) is allowed, and what gets
	// bound is what it resolves to.
	_ = os.Remove(link)
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatal(err)
	}
	s = spec(link)
	if err := s.resolveBindSources(hidden); err != nil {
		t.Fatalf("a shared dir on another path was refused: %v", err)
	}
	resolved, _ := filepath.EvalSymlinks(elsewhere)
	// Only the SOURCE moves. The destination stays the configured path,
	// because that is what WorkingDirectory, HOME and the
	// {release_dir}/{shared_dir} placeholders name — binding the
	// resolved path at the resolved path would put the data somewhere
	// the app never looks, and its own spelling would be gone with the
	// tmpfs that masks the root.
	if s.writable[1].source != resolved {
		t.Fatalf("bind source not rewritten to the resolved path: %+v", s.writable)
	}
	if s.writable[1].dest != link {
		t.Fatalf("bind destination moved to %q; the app expects its shared dir at %q", s.writable[1].dest, link)
	}

	// A missing bind source is an error, not a silently skipped bind.
	if err := spec(filepath.Join(appDir, "gone")).resolveBindSources(hidden); err == nil {
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
		sandbox: &sandboxSpec{tier: sandboxFull, root: root, appDir: appDir,
			writable: []bindPath{{dest: release, source: release}, {dest: shared, source: shared}},
			hidden:   sandboxHiddenFloor},
	}
	if _, err := r.unitFor(spec, false); err == nil || !strings.Contains(err.Error(), "sandbox itself closes") {
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
	root := t.TempDir()
	appDir := filepath.Join(root, "blog")
	release := filepath.Join(appDir, "releases", "v1")
	shared := filepath.Join(appDir, "shared")
	outside, err := os.MkdirTemp("/run", "binddest-")
	if err != nil {
		t.Skipf("no writable dir outside the closed prefixes: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outside) })
	elsewhere := filepath.Join(outside, "blog-data")
	for _, d := range []string{release, elsewhere} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(elsewhere, shared); err != nil {
		t.Fatal(err)
	}
	s := &sandboxSpec{tier: sandboxFull, root: root, appDir: appDir,
		writable: []bindPath{{dest: release, source: release}, {dest: shared, source: shared}},
		extra:    []extraPath{{path: "/run/postgresql"}},
		hidden:   sandboxHiddenFloor}
	if err := s.resolveBindSources(sandboxHiddenFloor); err != nil {
		t.Fatalf("a shared dir on another disk was refused: %v", err)
	}
	got := propMap(unitSpec{ExecStart: []string{"/bin/true"}, Sandbox: s})
	binds := map[string]bindMount{}
	for _, b := range got["BindPaths"].([]bindMount) {
		binds[b.Destination] = b
	}
	b, ok := binds[shared]
	if !ok {
		t.Fatalf("the shared dir is not bound where the app expects it (%s): %+v", shared, binds)
	}
	resolved, _ := filepath.EvalSymlinks(elsewhere)
	if b.Source != resolved {
		t.Fatalf("shared bound from %q, want the resolved %q", b.Source, resolved)
	}
	if _, moved := binds[resolved]; moved {
		t.Fatal("the shared dir was bound at its resolved path: the app would never find it")
	}
}
