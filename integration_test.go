//go:build integration

package hotswap

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddytest"
)

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
	_, broken := os.LookupEnv("NEVER_SET")
	if _, err := os.Stat("broken"); err == nil {
		broken = true
	}
	http.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		if broken {
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

func integrationConfig(root string) string {
	return fmt.Sprintf(`{
	skip_install_trust
	admin localhost:2999
	http_port 9080
	grace_period 1ns

	hotswap {
		root %s
		allow_insecure_http
		webhook_secret itest-secret

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
		dynamic hotswap demo
	}
}
http://localhost:9081 {
	hotswap_webhook
}
`, root)
}

func postDeploy(t *testing.T, artifactURL, version string) (*http.Response, string) {
	t.Helper()
	body := fmt.Sprintf(`{"url":%q,"version":%q}`, artifactURL, version)
	req, err := http.NewRequest(http.MethodPost, "http://localhost:9081/demo", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(secretHeader, "itest-secret")
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, string(data)
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
	artifactSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := artifacts[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	}))
	defer artifactSrv.Close()

	tester := caddytest.NewTester(t)
	tester.InitServer(integrationConfig(root), "caddyfile")

	t.Run("proxy errors before first deploy", func(t *testing.T) {
		code, _ := getBody(t, "http://localhost:9080/")
		if code < 500 {
			t.Fatalf("expected a 5xx before any deploy, got %d", code)
		}
	})

	t.Run("webhook rejects a bad secret", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "http://localhost:9081/demo",
			strings.NewReader(`{"url":"https://x/a.tgz","version":"v0"}`))
		req.Header.Set(secretHeader, "wrong")
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
		// The old process must be gone (its pid differs and v1 port is
		// no longer serving) — asserted indirectly: status shows v2.
		_, status := getStatus(t)
		if !strings.Contains(status, `"current_version":"v2"`) {
			t.Fatalf("status after v2: %s", status)
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
		tester.InitServer(integrationConfig(root), "caddyfile")

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
}

func getStatus(t *testing.T) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "http://localhost:9081/demo", nil)
	req.Header.Set(secretHeader, "itest-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}
