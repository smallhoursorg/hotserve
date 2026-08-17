package liveswap

import (
	"crypto/tls"
	"crypto/x509"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

func testDownloadOpts(t *testing.T, rawURL string) downloadOpts {
	t.Helper()
	return downloadOpts{
		url:           rawURL,
		destDir:       t.TempDir(),
		maxBytes:      1024,
		allowInsecure: true, // httptest servers are plain http
		allowlist:     mustAllowlist(t, "127.0.0.1"),
		client:        &http.Client{},
	}
}

func mustAllowlist(t *testing.T, entries ...string) []artifactAllowEntry {
	t.Helper()
	parsed, err := parseAllowlist(entries)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestDownloadArtifactHappyPath(t *testing.T) {
	var gotAuth, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		_, _ = w.Write([]byte("artifact-bytes"))
	}))
	defer srv.Close()

	opts := testDownloadOpts(t, srv.URL+"/demo.tar.gz")
	opts.authHeader = "Bearer tok123"
	path, err := downloadArtifact(context.Background(), opts)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "artifact-bytes" {
		t.Fatalf("content = %q", data)
	}
	if gotAuth != "Bearer tok123" {
		t.Fatalf("auth header not forwarded: %q", gotAuth)
	}
	if gotAccept != "application/octet-stream" {
		t.Fatalf("accept header = %q", gotAccept)
	}
}

func TestDownloadArtifactRejectsPlainHTTPByDefault(t *testing.T) {
	opts := testDownloadOpts(t, "http://example.com/a.tgz")
	opts.allowInsecure = false
	_, err := downloadArtifact(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "allow_insecure_http") {
		t.Fatalf("want https-only error, got %v", err)
	}
}

func TestDownloadArtifactRejectsWeirdScheme(t *testing.T) {
	_, err := downloadArtifact(context.Background(), testDownloadOpts(t, "ftp://example.com/a.tgz"))
	if err == nil || !strings.Contains(err.Error(), "unsupported scheme") {
		t.Fatalf("want scheme error, got %v", err)
	}
}

func TestDownloadArtifactHostAllowlist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()

	opts := testDownloadOpts(t, srv.URL+"/a.tgz")
	opts.allowlist = mustAllowlist(t, "github.com/smallhoursorg/")
	_, err := downloadArtifact(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "artifact_allowlist") {
		t.Fatalf("want allowlist error, got %v", err)
	}

	opts.allowlist = mustAllowlist(t, "github.com/smallhoursorg/", "127.0.0.1")
	if _, err := downloadArtifact(context.Background(), opts); err != nil {
		t.Fatalf("allowlisted host rejected: %v", err)
	}
}

func TestDownloadArtifactContentLengthCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "99999")
		_, _ = w.Write(make([]byte, 99999))
	}))
	defer srv.Close()
	_, err := downloadArtifact(context.Background(), testDownloadOpts(t, srv.URL))
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("want content-length cap error, got %v", err)
	}
}

func TestDownloadArtifactStreamingCap(t *testing.T) {
	// No Content-Length (chunked) but body exceeds the cap anyway.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 64; i++ {
			_, _ = w.Write(make([]byte, 64))
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()
	opts := testDownloadOpts(t, srv.URL)
	opts.maxBytes = 512
	_, err := downloadArtifact(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "exceeded max size") {
		t.Fatalf("want streaming cap error, got %v", err)
	}
	// The partial download must have been cleaned up.
	entries, _ := os.ReadDir(opts.destDir)
	if len(entries) != 0 {
		t.Fatalf("partial download left behind: %v", entries)
	}
}

func TestDownloadArtifactHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()
	_, err := downloadArtifact(context.Background(), testDownloadOpts(t, srv.URL+"/missing.tgz"))
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("want HTTP 404 error, got %v", err)
	}
}

func TestRedactURLDropsQuery(t *testing.T) {
	u, _ := url.Parse("https://gitlab.com/api/artifacts/7?private_token=SECRET")
	got := redactURL(u)
	if strings.Contains(got, "SECRET") || !strings.Contains(got, "gitlab.com/api/artifacts/7") {
		t.Fatalf("redactURL leaked or mangled: %q", got)
	}
}

// A permitted-looking https URL must not be able to downgrade the
// fetch to cleartext via a redirect: Go's client follows https->http
// redirects by default, so the scheme policy is enforced per hop in
// CheckRedirect.
func TestDownloadRefusesHTTPSToHTTPDowngrade(t *testing.T) {
	// The cleartext destination that must never be reached.
	reached := false
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		_, _ = w.Write([]byte("plaintext"))
	}))
	defer httpSrv.Close()
	// The https entry point that tries the downgrade.
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, httpSrv.URL, http.StatusFound)
	}))
	defer tlsSrv.Close()

	client := newDownloadClient(false)
	// Trust the httptest TLS cert on our transport (and only ours —
	// the CheckRedirect under test must be the real one).
	pool := x509.NewCertPool()
	pool.AddCert(tlsSrv.Certificate())
	client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: pool}

	_, err := downloadArtifact(context.Background(), downloadOpts{
		url:       tlsSrv.URL + "/artifact.tar.gz",
		destDir:   t.TempDir(),
		maxBytes:  1 << 20,
		allowlist: mustAllowlist(t, "127.0.0.1"),
		client:    client,
	})
	if err == nil || !strings.Contains(err.Error(), "downgrades") {
		t.Fatalf("want downgrade refusal, got %v", err)
	}
	if reached {
		t.Fatal("the cleartext destination was contacted despite the refusal")
	}
}

// With allow_insecure_http the same redirect is permitted — the policy
// belongs to the operator, per hop.
func TestDownloadAllowsDowngradeWhenInsecureAllowed(t *testing.T) {
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("payload"))
	}))
	defer httpSrv.Close()
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, httpSrv.URL, http.StatusFound)
	}))
	defer tlsSrv.Close()

	client := newDownloadClient(true)
	pool := x509.NewCertPool()
	pool.AddCert(tlsSrv.Certificate())
	client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: pool}

	path, err := downloadArtifact(context.Background(), downloadOpts{
		url:           tlsSrv.URL + "/artifact.tar.gz",
		destDir:       t.TempDir(),
		maxBytes:      1 << 20,
		allowInsecure: true,
		allowlist:     mustAllowlist(t, "127.0.0.1"),
		client:        client,
	})
	if err != nil {
		t.Fatalf("downgrade should be permitted with allow_insecure_http: %v", err)
	}
	if path == "" {
		t.Fatal("no artifact path returned")
	}
}
