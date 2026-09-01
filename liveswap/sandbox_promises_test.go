package liveswap

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Each test in this file pins ONE normative promise from
// DESIGN-sandbox.md's behaviour specification — the ones whose only
// other statement was prose. Three review rounds on this feature found
// the same class of defect by inspection, one instance at a time, so
// the rule for this feature is: a promise that is not asserted
// somewhere is not a promise. When you add a MUST to the design, add
// its test here (or extend the one that already covers it, listed
// below) in the same change.
//
// Promises already pinned elsewhere, so this file does not repeat them:
//
//	deny-by-default view                 TestSandboxViewIsExactlyWhatIsNamed
//	                                     TestIntegrationSystemdSandboxedUnit
//	app-to-app boundary                  TestAppViewsAreDisjointExceptTheBaseView
//	/etc never bound whole               TestSandboxPropertiesFilesystemAndFull
//	                                     TestBaseViewNamesTheTrustStoreNotTheTree
//	base view is not a write channel     TestExtraPathMayNotOverlapTheBaseView
//	                                     TestBindSourceInsideTheBaseViewRefused
//	binds are the dirs they name         TestMandatoryBindMustBeTheDirectoryItNames
//	                                     TestBindOntoASiblingsExternalDataRefused
//	command must be in the view          TestUnitForRefusesCommandOutsideTheView
//	no empty list property               TestSandboxPropertiesNeverEmitsAnEmptyBindList
//	/sys/fs/cgroup read-only             TestSandboxPropertiesFilesystemAndFull
//	/run/systemd/resolve reachable       TestSandboxPropertiesFilesystemAndFull
//	user + PID namespace per tier        TestSandboxPropertiesFilesystemAndFull
//	                                     TestIntegrationSystemdSandboxProbe
//	WARN below the full tier             TestRelaunchBelowFullWarns
//	engage on next deploy, not relaunch  TestDeployUsesPolicyRelaunchUsesRecord
//	stop semantics unchanged             TestIntegrationSystemdStopKillsWholeTree
//	                                     TestIntegrationSystemdSIGTERMReachesNamespaceInit

// Promise: "The app environment MUST be scrubbed … an allowlist of
// inherited variables, never a blanket os.Environ() inheritance, which
// would leak ACME tokens (and any other supervisor secrets) into every
// app. The sandbox path MUST NOT regress this."
//
// The e2e proves this end-to-end with a decoy token, which catches a
// regression only after a container boots. This catches it in the unit
// lane, and covers the variables the e2e has no reason to set.
func TestSandboxedEnvCarriesNoSupervisorSecrets(t *testing.T) {
	secrets := map[string]string{
		"ACME_DNS_TOKEN":           "s3cret",
		"AWS_SECRET_ACCESS_KEY":    "s3cret",
		"CADDY_ADMIN":              "/run/hotserve/admin.sock",
		"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/997/bus",
		"XDG_RUNTIME_DIR":          "/run/user/997",
		"INVOCATION_ID":            "abc",
	}
	for k, v := range secrets {
		t.Setenv(k, v)
	}
	spec := testSpec(t)
	for _, sandboxed := range []bool{false, true} {
		env, err := buildEnv(spec, "v1", 8123, spec.dirs.release("v1"), sandboxed)
		must(t, err)
		for k := range secrets {
			for _, kv := range env {
				if strings.HasPrefix(kv, k+"=") {
					t.Errorf("sandboxed=%v: the supervisor's %s reached the app: %q", sandboxed, k, kv)
				}
			}
		}
	}
}

// Promise: "`HOME`, `XDG_DATA_HOME` and `XDG_CONFIG_HOME` MUST be set
// to a writable in-sandbox path. Inherited they point at
// /var/lib/hotserve — outside the view — and every runtime that
// touches $HOME would ENOENT."
//
// Satisfied by not inheriting the XDG pair at all rather than by
// setting it: unset, the XDG base-directory spec makes every runtime
// derive them from $HOME, which IS set to the shared dir. The property
// that matters — nothing in the app's environment names a path outside
// its view — is what this asserts, in both directions.
func TestSandboxedEnvNamesNoPathOutsideTheView(t *testing.T) {
	t.Setenv("HOME", "/var/lib/hotserve")
	t.Setenv("XDG_DATA_HOME", "/var/lib/hotserve/caddy")
	t.Setenv("XDG_CONFIG_HOME", "/var/lib/hotserve/config")
	spec := testSpec(t)
	env, err := buildEnv(spec, "v1", 8123, spec.dirs.release("v1"), true)
	must(t, err)
	sb := spec.sandboxSpecFor(spec.dirs.release("v1"), sandboxFull)
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "XDG_DATA_HOME", "XDG_CONFIG_HOME":
			t.Errorf("%s reached the app (%q); unset, runtimes derive it from HOME, which is in the view", k, v)
		case "HOME":
			if !sb.inView(v) {
				t.Errorf("HOME=%q is not inside the sandbox view; every runtime that touches it would ENOENT", v)
			}
		}
	}
}

// Promise: "The network namespace MUST be shared: the app binds
// 127.0.0.1:$PORT and hotserve proxies to it, unchanged."
//
// Nothing asserts the absence of the properties that would break it,
// and unsharing the netns is a one-line change that no filesystem test
// would notice — the app would simply become unreachable through the
// proxy, at deploy time, on a real host.
func TestNetworkNamespaceIsShared(t *testing.T) {
	for _, tier := range []sandboxTier{sandboxFilesystem, sandboxFull} {
		spec := &sandboxSpec{tier: tier, root: "/var/lib/liveswap",
			appDir: "/var/lib/liveswap/blog", appName: "blog",
			writable: []bindPath{{dest: "/var/lib/liveswap/blog/shared"}}}
		got := propMap(unitSpec{ExecStart: []string{"/bin/true"}, Sandbox: spec})
		for _, name := range []string{
			"PrivateNetwork", "NetworkNamespacePath", "PrivateIPC", "IPCNamespacePath",
		} {
			if v, present := got[name]; present {
				t.Errorf("%s tier sets %s=%v: the app must share the network namespace so hotserve can proxy to 127.0.0.1:$PORT", tier, name, v)
			}
		}
		// The address families the app needs to bind and be proxied to
		// are the other half of the same promise.
		fam, _ := got["RestrictAddressFamilies"].(allowList)
		if !fam.AllowList {
			t.Errorf("%s tier: RestrictAddressFamilies must be an allow list, got %+v", tier, fam)
		}
		for _, want := range []string{"AF_INET", "AF_INET6"} {
			found := false
			for _, n := range fam.Names {
				if n == want {
					found = true
				}
			}
			if !found {
				t.Errorf("%s tier: %s missing, the app cannot bind its port: %v", tier, want, fam.Names)
			}
		}
	}
}

// Promise: "`pre_start` MUST run under the same sandbox as the app it
// precedes (a migration that writes where the app cannot read is a bug
// caught at deploy time, not 3am)."
//
// Asserted on the specs the runner actually receives, so it covers the
// wiring rather than the intent: a deploy with a pre_start issues
// RunOnce then Start, and their sandbox specs must be equal — same
// tier, same binds, same extras.
func TestPreStartRunsUnderTheSameSandboxAsItsApp(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.preStart = []string{"./migrate"}
	rig.spec.sandboxMode = sandboxAuto
	rig.spec.sandboxTier = sandboxFull
	rig.spec.extraPaths = []extraPath{{path: "/run/postgresql"}}
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))

	rig.runner.mu.Lock()
	specs := append([]startSpec{}, rig.runner.started...)
	rig.runner.mu.Unlock()
	if len(specs) < 2 {
		t.Fatalf("want a RunOnce (pre_start) then a Start, got %d specs", len(specs))
	}
	pre, app := specs[0], specs[1]
	if pre.command[0] != "./migrate" {
		t.Fatalf("first spec is not the pre_start: %v", pre.command)
	}
	if pre.sandbox == nil || app.sandbox == nil {
		t.Fatalf("pre_start sandbox=%v, app sandbox=%v — both must be sandboxed", pre.sandbox, app.sandbox)
	}
	if !reflect.DeepEqual(pre.sandbox, app.sandbox) {
		t.Fatalf("pre_start runs under a different sandbox than its app:\n pre = %+v\n app = %+v", pre.sandbox, app.sandbox)
	}
	if pre.dir != app.dir {
		t.Errorf("pre_start working dir %q != app's %q", pre.dir, app.dir)
	}
	// And an unsandboxed app's pre_start is unsandboxed too, rather
	// than accidentally stricter than the thing it precedes.
	rig2 := newTestRig(t)
	rig2.spec.preStart = []string{"./migrate"}
	rig2.spec.sandboxMode = sandboxOff
	must(t, rig2.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
	rig2.runner.mu.Lock()
	specs2 := append([]startSpec{}, rig2.runner.started...)
	rig2.runner.mu.Unlock()
	for i, s := range specs2 {
		if s.sandbox != nil {
			t.Errorf("spec %d is sandboxed although the app is `sandbox off`: %+v", i, s.sandbox)
		}
	}
}

// Promise: "every operator `env_file` [is absent from every app's
// view]" (liveswap/README.md) — for OTHER apps. Its own app receives
// those variables anyway, so self-exposure is a warning; a sibling's
// env file is the secret the feature exists to keep apart, and the
// deny-by-default view does not close either route on its own, because
// both are things the operator named.
func TestEnvFileMayNotLandInAnotherAppsView(t *testing.T) {
	// Through Validate(), not the validator directly: the wiring is
	// half the promise, and a test that calls the function straight
	// passes just as happily when nothing calls it.
	app := func(envFile string, extras ...string) *AppConfig {
		c := defaultedApp(t)
		c.EnvFile = envFile
		for _, e := range extras {
			c.ExtraPaths = append(c.ExtraPaths, ExtraPathConfig{Path: e})
		}
		return c
	}
	for _, tc := range []struct {
		name    string
		apps    map[string]*AppConfig
		wantErr string
	}{
		{"a sibling's extra_path covers it", map[string]*AppConfig{
			"blog": app("", "/srv/common"),
			"shop": app("/srv/common/shop.env"),
		}, "extra_path"},
		{"an ancestor extra_path counts", map[string]*AppConfig{
			"blog": app("", "/srv"),
			"shop": app("/srv/env/shop.env"),
		}, "extra_path"},
		{"the base view exposes it to everyone", map[string]*AppConfig{
			"shop": app("/usr/local/etc/shop.env"),
		}, "EVERY app's sandbox"},
		{"its own extra_path is not cross-app", map[string]*AppConfig{
			"shop": app("/srv/common/shop.env", "/srv/common"),
		}, ""},
		{"an unsandboxed viewer adds no exposure", map[string]*AppConfig{
			"blog": func() *AppConfig { c := app("", "/srv"); c.Sandbox = sandboxOff; return c }(),
			"shop": app("/srv/env/shop.env"),
		}, ""},
		{"the documented location is fine", map[string]*AppConfig{
			"blog": app("/etc/hotserve/blog.env", "/run/postgresql"),
			"shop": app("/etc/hotserve/shop.env"),
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &App{
				Root:              "/var/lib/liveswap",
				ArtifactAllowlist: []string{"github.com/smallhoursorg/"},
				DeployTrust:       githubTrust(),
				Apps:              tc.apps,
			}
			err := a.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("rejected a sound configuration: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("accepted a configuration exposing one app's env_file to another")
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("refused for the wrong reason: %v", err)
			}
		})
	}
}

// Promise: a relaunch reproduces the tier recorded for that instance —
// and a record that does not mean anything must not read as "no
// sandbox". A syntactically corrupt state.json is already a permanent
// recovery error; a semantically corrupt one is the same class.
func TestCorruptRecordedTierFailsClosed(t *testing.T) {
	for _, ok := range []string{"", "none", "filesystem", "full"} {
		if err := validSandboxTierRecord(ok); err != nil {
			t.Errorf("%q is a legitimate record: %v", ok, err)
		}
	}
	for _, bad := range []string{"ful", "Full", "FILESYSTEM", "yes", "true", "sandboxed", " full"} {
		if err := validSandboxTierRecord(bad); err == nil {
			t.Errorf("%q accepted: reading it as none would silently drop the app's sandbox", bad)
		}
	}
	// And the recovery path refuses rather than relaunching bare.
	rig := newTestRig(t)
	must(t, rig.store.save(appState{CurrentVersion: "v1", Port: 1,
		Handle: handleState{Unit: "hotserve-demo.v1.abc.service", Sandbox: "ful"}}))
	must(t, mkdirRelease(rig.spec, "v1"))
	err := rig.ma.ensureRunning()
	if err == nil {
		t.Fatal("recovery accepted a corrupt tier record and relaunched")
	}
	var perm *permanentRecoveryError
	if !errors.As(err, &perm) {
		t.Fatalf("a corrupt tier must be a permanent recovery error, not %T: %v", err, err)
	}
}
