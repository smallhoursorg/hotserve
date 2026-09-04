package liveswap

import (
	"context"
	"os"
	"path/filepath"
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
//	/etc never bound whole               TestSandboxPropertiesEveryUnit
//	                                     TestBaseViewNamesTheTrustStoreNotTheTree
//	                                     TestBindSourceInsideTheBaseViewRefused
//	binds are the dirs they name         TestMandatoryBindMustBeTheDirectoryItNames
//	                                     TestBindOntoASiblingsExternalDataRefused
//	command must be in the view          TestUnitForRefusesCommandOutsideTheView
//	no empty list property               TestSandboxPropertiesNeverEmitsAnEmptyBindList
//	/sys/fs/cgroup read-only             TestSandboxPropertiesEveryUnit
//	/run/systemd/resolve reachable       TestSandboxPropertiesEveryUnit
//	user + PID namespace, every unit     TestSandboxPropertiesEveryUnit
//	                                     TestIntegrationSystemdSandboxProbe
//	every launch is sandboxed            TestEveryLaunchIsSandboxed
//	no unit without a sandbox            TestUnitRefusesToRunWithoutASandbox
//	reattach refuses a bare unit         TestSystemdRunnerReattach
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
	{
		env, err := buildEnv(spec, "v1", 8123, spec.dirs.release("v1"))
		must(t, err)
		for k := range secrets {
			for _, kv := range env {
				if strings.HasPrefix(kv, k+"=") {
					t.Errorf("the supervisor's %s reached the app: %q", k, kv)
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
	env, err := buildEnv(spec, "v1", 8123, spec.dirs.release("v1"))
	must(t, err)
	sb := spec.sandboxSpecFor(spec.dirs.release("v1"))
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
	spec := &sandboxSpec{root: "/var/lib/liveswap",
		appDir: "/var/lib/liveswap/blog", appName: "blog",
		writable: []bindPath{{dest: "/var/lib/liveswap/blog/shared"}}}
	got := propMap(unitSpec{ExecStart: []string{"/bin/true"}, Sandbox: spec})
	for _, name := range []string{
		"PrivateNetwork", "NetworkNamespacePath", "PrivateIPC", "IPCNamespacePath",
	} {
		if v, present := got[name]; present {
			t.Errorf("%s=%v is set: the app must share the network namespace so hotserve can proxy to 127.0.0.1:$PORT", name, v)
		}
	}
	// The address families the app needs to bind and be proxied to
	// are the other half of the same promise.
	fam, _ := got["RestrictAddressFamilies"].(allowList)
	if !fam.AllowList {
		t.Errorf("RestrictAddressFamilies must be an allow list, got %+v", fam)
	}
	for _, want := range []string{"AF_INET", "AF_INET6"} {
		found := false
		for _, n := range fam.Names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s missing, the app cannot bind its port: %v", want, fam.Names)
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
// tier, same binds.
func TestPreStartRunsUnderTheSameSandboxAsItsApp(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.preStart = []string{"./migrate"}
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
	app := func(envFile string) *AppConfig {
		c := defaultedApp(t)
		c.EnvFile = envFile
		return c
	}
	for _, tc := range []struct {
		name    string
		apps    map[string]*AppConfig
		wantErr string
	}{
		{"the base view exposes it to everyone", map[string]*AppConfig{
			"shop": app("/usr/local/etc/shop.env"),
		}, "EVERY app's sandbox"},
		{"the documented location is fine", map[string]*AppConfig{
			"blog": app("/etc/hotserve/blog.env"),
			"shop": app("/etc/hotserve/shop.env"),
		}, ""},
		// A neighbour's own dirs are bound into its unit: shared/
		// read-WRITE, releases/ read-only. With nothing else able to
		// widen a view, these and the base view are the whole of what
		// can expose one app's env file to another.
		{"a sibling's shared dir is bound read-write into its unit", map[string]*AppConfig{
			"blog": app(""),
			"shop": app("/var/lib/liveswap/blog/shared/shop.env"),
		}, "read and rewrite"},
		{"a sibling's release dirs are bound too", map[string]*AppConfig{
			"blog": app(""),
			"shop": app("/var/lib/liveswap/blog/releases/v1/shop.env"),
		}, "release dirs"},
		{"an app's own shared dir is not cross-app", map[string]*AppConfig{
			"blog": app(""),
			"shop": app("/var/lib/liveswap/shop/shared/shop.env"),
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

// Promise: the sandbox HOME is a DEFAULT, not a fixed value — buildEnv
// applies it before env_file and inline env on purpose — but an
// override the view does not contain is reported rather than left to
// surface as an unexplained ENOENT from the first runtime that touches
// $HOME. DESIGN-sandbox.md, "Nothing in the app's environment may name
// a path outside its view".
func TestHomeOutsideTheViewIsReported(t *testing.T) {
	root := t.TempDir()
	appDir := filepath.Join(root, "blog")
	release := filepath.Join(appDir, "releases", "v1")
	shared := filepath.Join(appDir, "shared")
	for _, d := range []string{release, shared} {
		must(t, os.MkdirAll(d, 0o750))
	}
	sb := &sandboxSpec{
		root: root, appDir: appDir, appName: "blog",
		writable: []bindPath{{dest: release, source: release}, {dest: shared, source: shared}},
	}
	env := func(home string) []string {
		return []string{"PATH=/usr/bin", "HOME=" + home, "PORT=8080"}
	}
	for _, tc := range []struct {
		name string
		home string
		want bool
	}{
		{"the default shared dir is in view", shared, false},
		{"a release dir is in view", release, false},
		{"the base view is in view", "/usr/share", false},
		{"a path nothing binds is not", "/opt/app", true},
		{"the app dir root is not bound", appDir, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, outside := homeOutsideView(env(tc.home), sb)
			if outside != tc.want {
				t.Fatalf("homeOutsideView(%q) = %q,%v; want outside=%v", tc.home, got, outside, tc.want)
			}
			if outside && got != tc.home {
				t.Fatalf("reported %q, want the configured spelling %q", got, tc.home)
			}
		})
	}
	// The LAST assignment wins, as it does for systemd's Environment=:
	// buildEnv appends the sandbox HOME first, then env_file, then
	// inline env, so checking the first one would clear an override.
	if _, outside := homeOutsideView([]string{"HOME=" + shared, "HOME=/opt/app"}, sb); !outside {
		t.Fatal("an override appended after the default must be the one checked")
	}
}

// Promise: an env_file that does not exist YET is still compared in
// both spellings. env_file is allowed to be absent until the first
// deploy, and EvalSymlinks fails on a missing leaf — so resolving the
// whole path would hand back only the lexical spelling for exactly the
// configurations that need checking. The link lives in the parent.
func TestEnvFileIsolationResolvesASymlinkedParentOfAMissingFile(t *testing.T) {
	base, err := os.MkdirTemp("/var", "envparent-")
	if err != nil {
		t.Skipf("no writable dir outside the refused prefixes: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	root := filepath.Join(base, "liveswap")
	blogShared := filepath.Join(root, "blog", "shared")
	must(t, os.MkdirAll(blogShared, 0o750))
	// A link whose TARGET is a sibling's shared dir, with the env file
	// itself not created yet — the normal state before a first deploy.
	link := filepath.Join(base, "link")
	must(t, os.Symlink(blogShared, link))

	blog := defaultedApp(t)
	shop := defaultedApp(t)
	shop.EnvFile = filepath.Join(link, "shop.env")
	if _, err := os.Stat(shop.EnvFile); !os.IsNotExist(err) {
		t.Fatalf("the env file must not exist for this case to mean anything: %v", err)
	}
	a := &App{
		Root:              root,
		ArtifactAllowlist: []string{"github.com/smallhoursorg/"},
		DeployTrust:       githubTrust(),
		Apps:              map[string]*AppConfig{"blog": blog, "shop": shop},
	}
	err = a.Validate()
	if err == nil {
		t.Fatal("accepted an env_file whose parent links into a sibling's shared dir; blog would read and rewrite it the moment it is created")
	}
	if !strings.Contains(err.Error(), "shared dir") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// Promise: a sibling's own directories are compared in BOTH spellings.
// The env file is canonicalised before the comparison, so matching it
// against a lexical app directory alone misses the case where the
// liveswap root is a symlink — the app dir the operator configured and
// the one the bind actually exposes are then different strings, and
// only one of them is ever tested.
func TestEnvFileIsolationComparesBothSpellingsOfASiblingsDirs(t *testing.T) {
	// A real symlinked root: <tmp>/link -> <tmp>/real, with blog's
	// shared dir existing so EvalSymlinks has something to resolve.
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real")
	link := filepath.Join(tmp, "link")
	must(t, os.MkdirAll(filepath.Join(real, "blog", "shared"), 0o750))
	must(t, os.Symlink(real, link))

	blog := defaultedApp(t)
	shop := defaultedApp(t)
	// Configured through the RESOLVED spelling, while the root — and so
	// every derived app dir — is the link.
	shop.EnvFile = filepath.Join(real, "blog", "shared", "shop.env")
	a := &App{
		Root:              link,
		ArtifactAllowlist: []string{"github.com/smallhoursorg/"},
		DeployTrust:       githubTrust(),
		Apps:              map[string]*AppConfig{"blog": blog, "shop": shop},
	}
	err := a.Validate()
	if err == nil {
		t.Fatal("accepted an env_file inside a sibling's shared dir reached through the root's other spelling; blog's bind exposes and can rewrite it")
	}
	if !strings.Contains(err.Error(), "shared dir") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// Promise: a liveswap root inside the base view is refused at config
// load, unconditionally — every app's data would be bound read-only
// into every other app's sandbox. There is no sandbox setting that
// could make such a root harmless, so there is no case in which it
// loads. The supervisor-state rule is refused for its own reason: a
// root inside hotserve's own keys is a mistake with or without a view.
func TestRootUnderTheBaseViewIsRefused(t *testing.T) {
	const underBaseView = "/usr/local/liveswap"
	if err := validateSandboxRoot(underBaseView); err == nil {
		t.Fatal("a root under the base view must be refused: every app would read every other's data")
	}
	if err := validateSandboxRoot("/var/lib/hotserve/apps"); err == nil {
		t.Error("a root inside hotserve's own state must always be refused")
	}
	// And end to end, through Validate.
	a := &App{
		Root:              underBaseView,
		ArtifactAllowlist: []string{"github.com/smallhoursorg/"},
		DeployTrust:       githubTrust(),
		Apps:              map[string]*AppConfig{"blog": defaultedApp(t)},
	}
	if err := a.Validate(); err == nil {
		t.Fatal("a root under the base view must be refused at config load")
	}
}

// Promise: every unit is sandboxed. The runner is the last thing
// between a start spec and a running process, so it is where "no
// sandbox" is refused rather than rendered with the floor only: a
// caller that forgot the spec gets an error and no unit, never a bare
// app. Nothing in production can reach this — every launch path goes
// through sandboxSpecFor — which is exactly why it is pinned here.
func TestUnitRefusesToRunWithoutASandbox(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	spec := testApp(t)
	spec.sandbox = nil
	if _, err := r.Start(spec); err == nil || !strings.Contains(err.Error(), "no sandbox spec") {
		t.Fatalf("Start without a sandbox spec must be refused, got %v", err)
	}
	if err := r.RunOnce(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "no sandbox spec") {
		t.Fatalf("RunOnce without a sandbox spec must be refused, got %v", err)
	}
	conn.mu.Lock()
	n := len(conn.started)
	conn.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d unit(s) reached the manager without a sandbox", n)
	}
}
