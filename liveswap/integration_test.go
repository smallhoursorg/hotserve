//go:build integration

package liveswap

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddytest"
)

// The integration suite authenticates deploys with a local trust key:
// one ed25519 keypair is generated per test binary, its public half
// written to a temp file the Caddyfile's `deploy_trust local` block
// points at, and deploy requests carry a token minted with the private
// half (audience "itest"). itestPriv/itestPub reuse the shared
// authtest helpers.
var (
	itestPriv, itestPub = mustGenTestKey()
	itestPubPath        string
	itestPubOnce        sync.Once
)

func itestPubKeyPath() string {
	itestPubOnce.Do(func() {
		der, err := x509.MarshalPKIXPublicKey(itestPub)
		if err != nil {
			panic(err)
		}
		f, err := os.CreateTemp("", "itest-deploy-*.pub")
		if err != nil {
			panic(err)
		}
		if _, err := f.Write(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})); err != nil {
			panic(err)
		}
		if err := f.Close(); err != nil {
			panic(err)
		}
		itestPubPath = f.Name()
	})
	return itestPubPath
}

func itestToken(t *testing.T) string { return mintTestToken(t, itestPriv, "itest", nil) }

// The integration suite runs a real Caddy (in-process via caddytest)
// with the module loaded, deploys a real compiled test app through the
// webhook, and drives traffic through reverse_proxy's dynamic
// upstreams. It needs the Go toolchain (present in the dev container)
// to build the test app binary.

const testAppSource = `package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"
)

func main() {
	version := "unknown"
	if b, err := os.ReadFile("version.txt"); err == nil {
		version = strings.TrimSpace(string(b))
	}
	http.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		// Checked per request (not once at boot) so tests can break and
		// heal a RUNNING instance, which is what a watchdog reacts to.
		if _, err := os.Stat("broken"); err == nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "hello %s pid %d", version, os.Getpid())
	})
	if err := http.ListenAndServe("127.0.0.1:"+os.Getenv("PORT"), nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
`

// buildTestApp compiles the test app once and returns the binary path.
func buildTestApp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(testAppSource), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "server")
	cmd := exec.Command("go", "build", "-o", bin, src)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building test app: %v\n%s", err, out)
	}
	return bin
}

// packRelease tarballs the test app binary plus a version marker (and
// optionally the "broken" flag file the app's health check honors).
func packRelease(t *testing.T, bin, version string, broken bool) []byte {
	t.Helper()
	binData, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	entries := []tarEntry{
		{name: "server", body: string(binData), mode: 0o755},
		{name: "version.txt", body: version + "\n"},
	}
	if broken {
		entries = append(entries, tarEntry{name: "broken", body: "1"})
	}
	path := buildTarGz(t, entries)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func integrationConfig(root, artifactPort string) string {
	return fmt.Sprintf(`{
	skip_install_trust
	admin localhost:2999
	http_port 9080
	grace_period 1ns

	liveswap {
		root %s
		allow_insecure_http
		artifact_allowlist 127.0.0.1:%s
		deploy_trust local {
			public_key %s
			audience itest
		}

		app demo {
			command ./server
			env GREETING integration
			health_interval 50ms
			health_timeout 1s
			soak 100ms
			deadline 5s
			drain 1ms
			grace 2s
			keep 2
		}
	}
}
http://localhost:9080 {
	reverse_proxy {
		dynamic liveswap demo
	}
}
http://localhost:9081 {
	liveswap_webhook
}
`, root, artifactPort, itestPubKeyPath())
}

func postDeploy(t *testing.T, artifactURL, version string) (*http.Response, string) {
	t.Helper()
	return postDeployApp(t, "demo", artifactURL, version)
}

func getBody(t *testing.T, url string) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

var pidRe = regexp.MustCompile(`pid (\d+)`)

var portRe = regexp.MustCompile(`"port":(\d+)`)

// portFromStatus extracts the active instance's port from the webhook
// status JSON.
func portFromStatus(t *testing.T, status string) int {
	t.Helper()
	m := portRe.FindStringSubmatch(status)
	if m == nil {
		t.Fatalf("no port in status: %s", status)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("bad port %q in status: %v", m[1], err)
	}
	return n
}

func TestIntegrationDeployLifecycle(t *testing.T) {
	bin := buildTestApp(t)
	root := t.TempDir()

	// Artifact "release storage": one tarball per version, like a
	// forge's release assets.
	artifacts := map[string][]byte{
		"/demo-v1.tar.gz":        packRelease(t, bin, "v1", false),
		"/demo-v2.tar.gz":        packRelease(t, bin, "v2", false),
		"/demo-v3-broken.tar.gz": packRelease(t, bin, "v3", true),
	}
	artifactSrv := serveArtifacts(t, artifacts)

	// The allowlist entry declares the artifact server's literal port
	// (there is no wildcard), so the config is built after the server.
	artifactURL, err := url.Parse(artifactSrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	tester := caddytest.NewTester(t)
	tester.InitServer(integrationConfig(root, artifactURL.Port()), "caddyfile")

	t.Run("proxy errors before first deploy", func(t *testing.T) {
		code, _ := getBody(t, "http://localhost:9080/")
		if code < 500 {
			t.Fatalf("expected a 5xx before any deploy, got %d", code)
		}
	})

	t.Run("webhook rejects a bad token", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "http://localhost:9081/demo",
			strings.NewReader(`{"url":"https://x/a.tgz","version":"v0"}`))
		req.Header.Set("Authorization", "Bearer not-a-jwt")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", resp.StatusCode)
		}
	})

	t.Run("deploy v1 and serve it", func(t *testing.T) {
		resp, body := postDeploy(t, artifactSrv.URL+"/demo-v1.tar.gz", "v1")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("deploy v1: %d %s", resp.StatusCode, body)
		}
		code, page := getBody(t, "http://localhost:9080/")
		if code != http.StatusOK || !strings.Contains(page, "hello v1") {
			t.Fatalf("proxied response = %d %q", code, page)
		}
	})

	t.Run("deploy v2 cuts over and stops v1", func(t *testing.T) {
		_, prev := getBody(t, "http://localhost:9080/")
		_, statusBefore := getStatus(t)
		v1Port := portFromStatus(t, statusBefore)

		resp, body := postDeploy(t, artifactSrv.URL+"/demo-v2.tar.gz", "v2")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("deploy v2: %d %s", resp.StatusCode, body)
		}
		code, page := getBody(t, "http://localhost:9080/")
		if code != http.StatusOK || !strings.Contains(page, "hello v2") {
			t.Fatalf("proxied response = %d %q", code, page)
		}
		if page == prev {
			t.Fatal("response did not change after cutover")
		}
		_, status := getStatus(t)
		if !strings.Contains(status, `"current_version":"v2"`) {
			t.Fatalf("status after v2: %s", status)
		}

		// The old port must be RELEASED, not merely unrouted. Port
		// inequality is asserted here — and only here — because with
		// real processes it is a true invariant: v1 was still listening
		// when v2's port was allocated, so the kernel cannot have
		// reused it. (On the unit tests' fake runner nothing binds, so
		// the same assertion is flaky there — see
		// TestGetUpstreamsSeesCutover's history.) The deploy response
		// returns only after drain + stop-old, so by now a connect to
		// v1's port must be refused.
		v2Port := portFromStatus(t, status)
		if v2Port == v1Port {
			t.Fatalf("v2 was allocated v1's port %d while v1 was alive", v1Port)
		}
		if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", v1Port), time.Second); err == nil {
			_ = conn.Close()
			t.Fatalf("old v1 port %d still accepting connections after stop", v1Port)
		}
	})

	t.Run("broken v3 fails and v2 keeps serving", func(t *testing.T) {
		resp, body := postDeploy(t, artifactSrv.URL+"/demo-v3-broken.tar.gz", "v3")
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("broken deploy should 500, got %d %s", resp.StatusCode, body)
		}
		if !strings.Contains(body, "health gate") {
			t.Fatalf("500 body should name the health gate: %s", body)
		}
		code, page := getBody(t, "http://localhost:9080/")
		if code != http.StatusOK || !strings.Contains(page, "hello v2") {
			t.Fatalf("v2 must keep serving after failed v3: %d %q", code, page)
		}
	})

	t.Run("config reload keeps the app process running", func(t *testing.T) {
		_, before := getBody(t, "http://localhost:9080/")
		pidBefore := pidRe.FindStringSubmatch(before)
		if pidBefore == nil {
			t.Fatalf("no pid in response: %q", before)
		}

		// Reload the SAME config through the admin API. The UsagePool
		// must carry the running child across the reload untouched.
		tester.InitServer(integrationConfig(root, artifactURL.Port()), "caddyfile")

		code, after := getBody(t, "http://localhost:9080/")
		if code != http.StatusOK {
			t.Fatalf("post-reload proxy = %d", code)
		}
		pidAfter := pidRe.FindStringSubmatch(after)
		if pidAfter == nil || pidAfter[1] != pidBefore[1] {
			t.Fatalf("app process restarted across reload: before=%v after=%v", pidBefore, pidAfter)
		}
	})

	t.Run("keep-N prunes old releases on the next successful deploy", func(t *testing.T) {
		// GC runs only after a SUCCESSFUL deploy (failed v3's dir is
		// kept for debugging), so trigger one more: v4 reuses the v1
		// artifact under a new version tag.
		resp, body := postDeploy(t, artifactSrv.URL+"/demo-v1.tar.gz", "v4")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("deploy v4: %d %s", resp.StatusCode, body)
		}
		entries, err := os.ReadDir(filepath.Join(root, "demo", "releases"))
		if err != nil {
			t.Fatal(err)
		}
		var kept []string
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), ".") {
				kept = append(kept, e.Name())
			}
		}
		if len(kept) > 2 {
			t.Fatalf("keep=2 violated after successful deploy: %v", kept)
		}
		if _, err := os.Stat(filepath.Join(root, "demo", "releases", "v4")); err != nil {
			t.Fatalf("current release v4 missing: %v", err)
		}
	})

	t.Run("un-allowed query parameter is a 422 that names the parameter", func(t *testing.T) {
		// The config's entry pins the artifact server's literal port
		// but declares no query parameters, so ?p=2 must be refused before any request leaves
		// the box, as a 422 whose body tells the CI author exactly what
		// tripped and how the entry would declare it.
		resp, body := postDeploy(t, artifactSrv.URL+"/demo-v1.tar.gz?p=2", "v5")
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("query refusal: got %d %s, want 422", resp.StatusCode, body)
		}
		for _, want := range []string{`\"p\"`, "declares no query parameters"} {
			if !strings.Contains(body, want) {
				t.Errorf("422 body should contain %s: %s", want, body)
			}
		}
		// The refusal must not have disturbed the running version.
		if _, status := getStatus(t); !strings.Contains(status, `"current_version":"v4"`) {
			t.Fatalf("v4 no longer serving after refused deploy: %s", status)
		}
	})
}

// watchdogConfig is integrationConfig with fast watchdog settings and
// a configurable restart budget/window. The app name must be unique
// per test function: the UsagePool is process-global, so a `demo`
// still running from another test would be adopted here.
func watchdogConfig(root, artifactPort, app string, restarts int, window string) string {
	return fmt.Sprintf(`{
	skip_install_trust
	admin localhost:2999
	http_port 9080
	grace_period 1ns

	liveswap {
		root %[1]s
		allow_insecure_http
		artifact_allowlist 127.0.0.1:%[2]s
		deploy_trust local {
			public_key %[6]s
			audience itest
		}

		app %[3]s {
			command ./server
			health_interval 50ms
			health_timeout 1s
			soak 100ms
			deadline 5s
			drain 1ms
			grace 1s
			watchdog_grace 200ms
			watchdog_failures 3
			watchdog_restarts %[4]d
			watchdog_window %[5]s
			keep 2
		}
	}
}
http://localhost:9080 {
	reverse_proxy {
		dynamic liveswap %[3]s
	}
}
http://localhost:9081 {
	liveswap_webhook
}
`, root, artifactPort, app, restarts, window, itestPubKeyPath())
}

func postDeployApp(t *testing.T, app, artifactURL, version string) (*http.Response, string) {
	t.Helper()
	body := fmt.Sprintf(`{"url":%q,"version":%q}`, artifactURL, version)
	req, err := http.NewRequest(http.MethodPost, "http://localhost:9081/"+app, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+itestToken(t))
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, string(data)
}

func getStatusApp(t *testing.T, app string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "http://localhost:9081/"+app, nil)
	req.Header.Set("Authorization", "Bearer "+itestToken(t))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

func serveArtifacts(t *testing.T, artifacts map[string][]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := artifacts[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// tryBody is getBody without the fatals, for polling through the
// window in which the proxy legitimately 502s.
func tryBody() (int, string) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://localhost:9080/")
	if err != nil {
		return 0, ""
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

func currentPID(t *testing.T) int {
	t.Helper()
	code, page := getBody(t, "http://localhost:9080/")
	m := pidRe.FindStringSubmatch(page)
	if code != http.StatusOK || m == nil {
		t.Fatalf("no pid in proxied response: %d %q", code, page)
	}
	pid, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func pollUntil(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	pollFor(t, timeout, 25*time.Millisecond, desc, cond)
}

func TestIntegrationWatchdogRestarts(t *testing.T) {
	bin := buildTestApp(t)
	root := t.TempDir()
	artifactSrv := serveArtifacts(t, map[string][]byte{
		"/demo-v1.tar.gz": packRelease(t, bin, "v1", false),
	})
	artifactURL, err := url.Parse(artifactSrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	const app = "wdcrash"
	tester := caddytest.NewTester(t)
	tester.InitServer(watchdogConfig(root, artifactURL.Port(), app, 50, "10m"), "caddyfile")

	if resp, body := postDeployApp(t, app, artifactSrv.URL+"/demo-v1.tar.gz", "v1"); resp.StatusCode != http.StatusOK {
		t.Fatalf("deploy v1: %d %s", resp.StatusCode, body)
	}

	t.Run("SIGKILLed app comes back with a new pid", func(t *testing.T) {
		pid := currentPID(t)
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		pollUntil(t, 15*time.Second, "restart with a new pid", func() bool {
			code, page := tryBody()
			if code != http.StatusOK {
				return false
			}
			m := pidRe.FindStringSubmatch(page)
			return m != nil && m[1] != strconv.Itoa(pid)
		})
		_, status := getStatusApp(t, app)
		if !strings.Contains(status, `"last_restart_cause":"crash"`) {
			t.Fatalf("status after crash restart: %s", status)
		}
	})

	t.Run("sustained health failure triggers a restart", func(t *testing.T) {
		pid := currentPID(t)
		brokenPath := filepath.Join(root, app, "releases", "v1", "broken")
		if err := os.WriteFile(brokenPath, []byte("1"), 0o600); err != nil {
			t.Fatal(err)
		}
		pollUntil(t, 20*time.Second, "health-triggered restart", func() bool {
			code, page := tryBody()
			if code != http.StatusOK {
				return false
			}
			m := pidRe.FindStringSubmatch(page)
			return m != nil && m[1] != strconv.Itoa(pid)
		})
		// Heal the release so the replacement stays up.
		if err := os.Remove(brokenPath); err != nil {
			t.Fatal(err)
		}
		_, status := getStatusApp(t, app)
		if !strings.Contains(status, `"last_restart_cause":"health"`) {
			t.Fatalf("status after health restart: %s", status)
		}
	})
}

func TestIntegrationWatchdogThrottleAutoRecovers(t *testing.T) {
	bin := buildTestApp(t)
	root := t.TempDir()
	artifactSrv := serveArtifacts(t, map[string][]byte{
		"/demo-v1.tar.gz": packRelease(t, bin, "v1", false),
	})
	artifactURL, err := url.Parse(artifactSrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	const app = "wdex"
	tester := caddytest.NewTester(t)
	tester.InitServer(watchdogConfig(root, artifactURL.Port(), app, 1, "10s"), "caddyfile")

	if resp, body := postDeployApp(t, app, artifactSrv.URL+"/demo-v1.tar.gz", "v1"); resp.StatusCode != http.StatusOK {
		t.Fatalf("deploy v1: %d %s", resp.StatusCode, body)
	}

	// Budget is 1 per 10s window: the first kill restarts immediately,
	// the second throttles until the window frees.
	pid := currentPID(t)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	pollUntil(t, 15*time.Second, "the single budgeted restart", func() bool {
		code, page := tryBody()
		if code != http.StatusOK {
			return false
		}
		m := pidRe.FindStringSubmatch(page)
		return m != nil && m[1] != strconv.Itoa(pid)
	})
	secondPID := currentPID(t)
	if err := syscall.Kill(secondPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	pollUntil(t, 15*time.Second, "throttle once the window is full", func() bool {
		_, status := getStatusApp(t, app)
		return strings.Contains(status, `"state":"throttled"`)
	})
	pollUntil(t, 5*time.Second, "clean 5xx while the dead app waits", func() bool {
		code, _ := tryBody()
		return code >= 500
	})

	// Never give up: with no deploy and no operator, the watchdog
	// restarts on its own once the oldest restart slides out of the
	// window (~10s), and the app serves again.
	pollUntil(t, 30*time.Second, "auto-recovery without a deploy", func() bool {
		code, page := tryBody()
		if code != http.StatusOK || !strings.Contains(page, "hello v1") {
			return false
		}
		m := pidRe.FindStringSubmatch(page)
		return m != nil && m[1] != strconv.Itoa(secondPID)
	})
	if _, status := getStatusApp(t, app); strings.Contains(status, `"state":"throttled"`) {
		t.Fatalf("throttle must clear after the auto-recovery: %s", status)
	}
}

func getStatus(t *testing.T) (int, string) {
	t.Helper()
	return getStatusApp(t, "demo")
}

// oidcConfig runs an app whose deploys are authenticated by an OIDC
// provider (our own in-process mock issuer), exercising the production
// OIDC path — Caddyfile parsing, Provision wiring, the JWKS client,
// discovery and token verification — end to end through the real
// binary, which the local-key e2e path does not.
func oidcConfig(root, artifactPort, issuer string) string {
	return fmt.Sprintf(`{
	skip_install_trust
	admin localhost:2999
	http_port 9080
	grace_period 1ns

	liveswap {
		root %[1]s
		allow_insecure_http
		artifact_allowlist 127.0.0.1:%[2]s
		deploy_trust oidc {
			issuer %[3]s
			audience e2e
			claim sub ci
		}

		app oidcdemo {
			command ./server
			health_interval 50ms
			health_timeout 1s
			soak 100ms
			deadline 5s
			drain 1ms
			grace 2s
			keep 2
		}
	}
}
http://localhost:9080 {
	reverse_proxy {
		dynamic liveswap oidcdemo
	}
}
http://localhost:9081 {
	liveswap_webhook
}
`, root, artifactPort, issuer)
}

func TestIntegrationOIDCDeploy(t *testing.T) {
	bin := buildTestApp(t)
	root := t.TempDir()
	artifacts := map[string][]byte{"/demo-v1.tar.gz": packRelease(t, bin, "v1", false)}
	artifactSrv := serveArtifacts(t, artifacts)
	artifactURL, err := url.Parse(artifactSrv.URL)
	if err != nil {
		t.Fatal(err)
	}

	// Our own OIDC provider, in-process: serves discovery + JWKS and
	// signs tokens, standing in for GitHub/GitLab.
	iss := newMockIssuer(t)

	tester := caddytest.NewTester(t)
	tester.InitServer(oidcConfig(root, artifactURL.Port(), iss.url), "caddyfile")

	hook := "http://localhost:9081/oidcdemo"
	deploy := func(token string) (int, string) {
		body := fmt.Sprintf(`{"url":%q,"version":"v1"}`, artifactSrv.URL+"/demo-v1.tar.gz")
		req, _ := http.NewRequest(http.MethodPost, hook, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(data)
	}
	future := time.Now().Add(5 * time.Minute)

	t.Run("valid OIDC token deploys and serves", func(t *testing.T) {
		tok := iss.mint(t, iss.priv, "e2e", map[string]string{"sub": "ci"}, future)
		if code, body := deploy(tok); code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", code, body)
		}
		code, b := getBody(t, "http://localhost:9080/")
		if code != http.StatusOK || !strings.HasPrefix(b, "hello v1") {
			t.Fatalf("proxy did not serve v1: %d %q", code, b)
		}
	})

	// Each negative case is a validly-signed token that must still be
	// rejected — the whole point of the claim allowlist.
	t.Run("wrong identity claim is rejected", func(t *testing.T) {
		tok := iss.mint(t, iss.priv, "e2e", map[string]string{"sub": "attacker"}, future)
		if code, _ := deploy(tok); code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for wrong sub, got %d", code)
		}
	})

	t.Run("wrong audience is rejected", func(t *testing.T) {
		tok := iss.mint(t, iss.priv, "other", map[string]string{"sub": "ci"}, future)
		if code, _ := deploy(tok); code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for wrong audience, got %d", code)
		}
	})

	t.Run("expired token is rejected", func(t *testing.T) {
		tok := iss.mint(t, iss.priv, "e2e", map[string]string{"sub": "ci"}, time.Now().Add(-time.Hour))
		if code, _ := deploy(tok); code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for expired token, got %d", code)
		}
	})

	t.Run("garbage token is rejected", func(t *testing.T) {
		if code, _ := deploy("not-a-jwt"); code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for garbage token, got %d", code)
		}
	})
}
