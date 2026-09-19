package backup

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// serveAdmin stands up an admin API on a unix socket, the way the
// packaged Caddyfile does, and returns the address flag value for it.
func serveAdmin(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	// Short path: a unix socket path must fit sun_path (108 bytes),
	// and t.TempDir() under a long test name does not always.
	dir, err := os.MkdirTemp("", "adm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "admin.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: handler}}
	srv.Start()
	t.Cleanup(srv.Close)
	return "unix/" + sock
}

const twoApps = `{
  "root": "/srv/liveswap",
  "apps": {
    "shop": {"command": ["./server"], "state": [{"kind": "sqlite", "path": "app.db"}, {"kind": "files", "path": "uploads"}]},
    "blog": {"command": ["./server"], "keep": 5},
    "wiki": {"command": ["./server"], "state": [{"kind": "files", "path": "pages"}]}
  }
}`

func TestFetchAppsReturnsOnlyAppsWithState(t *testing.T) {
	addr := serveAdmin(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/config/apps/liveswap" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(twoApps))
	})
	apps, err := FetchApps(context.Background(), addr)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(apps) != 2 {
		t.Fatalf("want 2 apps with state, got %d: %+v", len(apps), apps)
	}
	// Sorted, so the journal reads like a list and this can be pinned.
	if apps[0].Name != "shop" || apps[1].Name != "wiki" {
		t.Fatalf("want shop then wiki, got %+v", apps)
	}
	if apps[0].Shared != "/srv/liveswap/shop/shared" {
		t.Errorf("shared dir = %q, want it under the configured root", apps[0].Shared)
	}
	if got := apps[0].Databases(); len(got) != 1 || got[0] != "app.db" {
		t.Errorf("databases = %v", got)
	}
	if got := apps[0].Files(); len(got) != 1 || got[0] != "uploads" {
		t.Errorf("files = %v", got)
	}
}

// The admin API serves the config as loaded, so an operator who never
// wrote `root` leaves it empty even though liveswap is using its
// default.
func TestFetchAppsAppliesDefaultRoot(t *testing.T) {
	addr := serveAdmin(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"apps": {"blog": {"state": [{"kind": "files", "path": "uploads"}]}}}`))
	})
	apps, err := FetchApps(context.Background(), addr)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if apps[0].Shared != DefaultLiveswapRoot+"/blog/shared" {
		t.Fatalf("shared dir = %q, want the default root", apps[0].Shared)
	}
}

// A box with no liveswap config at all must say so, not crash or
// report success.
func TestFetchAppsExplainsAMissingLiveswapApp(t *testing.T) {
	addr := serveAdmin(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error": "unknown object"}`))
	})
	_, err := FetchApps(context.Background(), addr)
	if err == nil || !strings.Contains(err.Error(), "nothing declares state") {
		t.Fatalf("want an explanatory error, got %v", err)
	}
}

func TestFetchAppsUnreachableAdminSaysWhatToCheck(t *testing.T) {
	_, err := FetchApps(context.Background(), "unix//nonexistent/hotserve-admin.sock")
	if err == nil || !strings.Contains(err.Error(), "is hotserve running") {
		t.Fatalf("want a hint about the socket, got %v", err)
	}
}

// The admin API serves `root` as it was written, so an {env.NAME} in it
// reaches this command unresolved. It is resolved from this command's
// own environment, and a name it cannot see is an error that says so:
// resolving it to "" would point every backup at a path that does not
// exist, which reads as "never deployed" and skips the app.
func TestFetchAppsResolvesAnEnvPlaceholderInRoot(t *testing.T) {
	const cfg = `{"root": "{env.LIVESWAP_ROOT}/apps", "apps": {"blog": {"state": [{"kind": "files", "path": "uploads"}]}}}`
	addr := serveAdmin(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(cfg)) })

	t.Setenv("LIVESWAP_ROOT", "/data")
	apps, err := FetchApps(context.Background(), addr)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(apps) != 1 || apps[0].Shared != "/data/apps/blog/shared" {
		t.Errorf("shared = %+v, want /data/apps/blog/shared", apps)
	}

	if err := os.Unsetenv("LIVESWAP_ROOT"); err != nil {
		t.Fatal(err)
	}
	_, err = FetchApps(context.Background(), addr)
	if err == nil || !strings.Contains(err.Error(), "LIVESWAP_ROOT is not set") || !strings.Contains(err.Error(), "hotserve-backup.service") {
		t.Errorf("want an error naming the variable and where to set it, got %v", err)
	}
}

func TestFetchAppsRefusesARootItCannotUse(t *testing.T) {
	for name, tc := range map[string]struct{ root, want string }{
		"an unterminated placeholder": {"{env.LIVESWAP_ROOT", "unterminated placeholder"},
		"a relative path":             {"var/lib/liveswap", "not an absolute path"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := `{"root": "` + tc.root + `", "apps": {"blog": {"state": [{"kind": "files", "path": "uploads"}]}}}`
			addr := serveAdmin(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(cfg)) })
			if _, err := FetchApps(context.Background(), addr); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want an error saying %q, got %v", tc.want, err)
			}
		})
	}
}

// `run` and `restore` are root, and what the admin API says becomes a
// unit's name, its StateDirectory= and a BindPaths= value. The API is
// served by the hotserve process — the first thing an attacker on the
// internet reaches — so its answer is held to this package's own rules,
// whatever liveswap validated when it loaded the config.
func TestFetchAppsHoldsTheAdminAPIsAnswerToItsOwnRules(t *testing.T) {
	state := `"state": [{"kind": "files", "path": "uploads"}]`
	for name, cfg := range map[string]string{
		"a name that is two binds":    `{"apps": {"a/shared /root:/var/lib/liveswap/b": {` + state + `}}}`,
		"a name that climbs":          `{"apps": {"../../etc": {` + state + `}}}`,
		"a name with a unit suffix":   `{"apps": {"blog.service": {` + state + `}}}`,
		"an empty name":               `{"apps": {"": {` + state + `}}}`,
		"a root that is two binds":    `{"root": "/srv/apps /etc:/srv", "apps": {"blog": {` + state + `}}}`,
		"a root with a colon":         `{"root": "/srv/apps:/etc", "apps": {"blog": {` + state + `}}}`,
		"a root that climbs":          `{"root": "/var/lib/liveswap/../../etc", "apps": {"blog": {` + state + `}}}`,
		"a root that is not absolute": `{"root": "srv/apps", "apps": {"blog": {` + state + `}}}`,
		"a kind that is a flag":       `{"apps": {"blog": {"state": [{"kind": "--shared=/etc", "path": "x"}]}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			addr := serveAdmin(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(cfg)) })
			apps, err := FetchApps(context.Background(), addr)
			if err == nil {
				t.Fatalf("accepted, and would have launched units for %+v", apps)
			}
		})
	}
	// An app that declares no state is never launched, so its name is
	// nobody's argument: it does not stop the apps that do.
	ok := `{"root": "/srv/apps/", "apps": {"Odd Name": {}, "blog": {` + state + `}}}`
	addr := serveAdmin(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(ok)) })
	apps, err := FetchApps(context.Background(), addr)
	if err != nil || len(apps) != 1 || apps[0].Shared != "/srv/apps/blog/shared" {
		t.Fatalf("a plain root with a trailing slash and a plain name are fine: %+v, %v", apps, err)
	}
}
