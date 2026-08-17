package liveswap

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
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
		allowlist:     mustAllowlist(t, "127.0.0.1:*"),
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

// Every payload-caused refusal is a validationError, so the webhook
// answers 422 with the reason — never a generic 500 the CI author
// cannot act on.
func TestDownloadPayloadRefusalsAreValidationErrors(t *testing.T) {
	for name, rawURL := range map[string]string{
		"unparseable url": "https://exa mple.com/x",
		"weird scheme":    "ftp://example.com/a.tgz",
		"plain http":      "http://example.com/a.tgz",
		"not allowlisted": "https://evil.example/a.tgz",
		"undeclared port": "https://127.0.0.1:1234/a.tgz",
	} {
		opts := testDownloadOpts(t, rawURL)
		opts.allowInsecure = false
		opts.allowlist = mustAllowlist(t, "127.0.0.1")
		_, err := downloadArtifact(context.Background(), opts)
		var vErr validationError
		if err == nil || !errors.As(err, &vErr) {
			t.Errorf("%s: want validationError, got %T: %v", name, err, err)
		}
	}
}

// Go's client drops Authorization when a redirect changes the host —
// the GitHub -> S3 pattern depends on the credential NOT following the
// redirect. That stdlib behavior is load-bearing for credential scope,
// so pin it: 127.0.0.1 and localhost are the same server here but
// different hostnames, which is exactly what a cross-host hop is.
func TestDownloadDropsAuthHeaderOnCrossHostRedirect(t *testing.T) {
	authByPath := map[string]string{}
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authByPath[r.URL.Path] = r.Header.Get("Authorization")
		if r.URL.Path == "/hop1.tgz" {
			u, _ := url.Parse(srvURL)
			http.Redirect(w, r, "http://localhost:"+u.Port()+"/hop2.tgz", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()
	srvURL = srv.URL

	opts := testDownloadOpts(t, srv.URL+"/hop1.tgz")
	opts.authHeader = "Bearer tok123"
	opts.client = newDownloadClient(true)
	if _, err := downloadArtifact(context.Background(), opts); err != nil {
		t.Fatalf("download: %v", err)
	}
	if authByPath["/hop1.tgz"] != "Bearer tok123" {
		t.Fatalf("first hop should carry the credential, got %q", authByPath["/hop1.tgz"])
	}
	if got, ok := authByPath["/hop2.tgz"]; !ok {
		t.Fatal("redirect target never reached")
	} else if got != "" {
		t.Fatalf("credential followed a cross-host redirect: %q", got)
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

	opts.allowlist = mustAllowlist(t, "github.com/smallhoursorg/", "127.0.0.1:*")
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

// An un-allowed query parameter must be refused BEFORE any request
// leaves the box, and the refusal must be a validationError — that is
// what makes the webhook answer 422 with the explanation in the body,
// so the CI author sees exactly which parameter tripped the gate.
func TestDownloadRejectsUnallowedQueryParam(t *testing.T) {
	contacted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		contacted = true
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()

	opts := testDownloadOpts(t, srv.URL+"/a.tgz?p=2&token=SECRETVALUE")
	_, err := downloadArtifact(context.Background(), opts)
	if err == nil {
		t.Fatal("un-allowed query admitted")
	}
	var vErr validationError
	if !errors.As(err, &vErr) {
		t.Fatalf("refusal must be a validationError (422 to the caller), got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), `"p"`) || !strings.Contains(err.Error(), "query parameter") {
		t.Fatalf("refusal should name the parameter: %v", err)
	}
	if strings.Contains(err.Error(), "SECRETVALUE") {
		t.Fatalf("refusal leaked a query value: %v", err)
	}
	if contacted {
		t.Fatal("artifact server was contacted despite the refusal")
	}

	// Declaring the names in the entry admits the same URL.
	opts.allowlist = mustAllowlist(t, "127.0.0.1:*?p&token")
	if _, err := downloadArtifact(context.Background(), opts); err != nil {
		t.Fatalf("declared params should admit: %v", err)
	}
	if !contacted {
		t.Fatal("download did not happen after declaring the params")
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
		allowlist: mustAllowlist(t, "127.0.0.1:*"),
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
		allowlist:     mustAllowlist(t, "127.0.0.1:*"),
		client:        client,
	})
	if err != nil {
		t.Fatalf("downgrade should be permitted with allow_insecure_http: %v", err)
	}
	if path == "" {
		t.Fatal("no artifact path returned")
	}
}

// Userinfo embedded in a webhook URL must never reach the wire: the
// outgoing URL is rebuilt field-by-field from the matched allowlist
// entry, and userinfo is deliberately not among the fields —
// credentials travel via auth_header only.
func TestDownloadDropsURLUserinfo(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	withUser := "http://leak:hunter2@" + u.Host + "/a.tgz"
	opts := testDownloadOpts(t, withUser)
	if _, err := downloadArtifact(context.Background(), opts); err != nil {
		t.Fatalf("download: %v", err)
	}
	if gotAuth != "" {
		t.Fatalf("userinfo leaked as Authorization: %q", gotAuth)
	}
}
