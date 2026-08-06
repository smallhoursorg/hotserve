package liveswap

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// downloadOpts parameterizes one artifact download.
type downloadOpts struct {
	url           string
	authHeader    string // full Authorization value ("Bearer x", "token y"); optional
	destDir       string // staging dir (the app's tmp/); created if missing
	maxBytes      int64
	allowInsecure bool                // permit plain http URLs
	allowedHosts  map[string]struct{} // empty = any host
	client        *http.Client
}

// downloadArtifact streams the artifact to a file in destDir and
// returns its path. The size cap is enforced twice, matching the
// hardened webhook this module replaces: once against Content-Length
// before reading the body, and again on the running byte count while
// streaming (Content-Length can lie or be absent).
//
// Secrets never reach the logs from here: errors carry the URL's host
// and path only, never its query string or the auth header.
func downloadArtifact(ctx context.Context, opts downloadOpts) (string, error) {
	u, err := url.Parse(opts.url)
	if err != nil {
		return "", fmt.Errorf("invalid artifact url: %v", err)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !opts.allowInsecure {
			return "", fmt.Errorf("artifact url %s is plain http; use https or set allow_insecure_http", redactURL(u))
		}
	default:
		return "", fmt.Errorf("artifact url %s has unsupported scheme %q", redactURL(u), u.Scheme)
	}
	if len(opts.allowedHosts) > 0 {
		if _, ok := opts.allowedHosts[u.Hostname()]; !ok {
			return "", fmt.Errorf("artifact host %q is not in allowed_artifact_hosts", u.Hostname())
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.url, nil)
	if err != nil {
		return "", err
	}
	// GitHub release-asset API URLs return the binary only with this
	// Accept; it is harmless everywhere else.
	req.Header.Set("Accept", "application/octet-stream")
	if opts.authHeader != "" {
		// Go's http.Client drops Authorization on cross-host redirects,
		// which is exactly right: GitHub asset URLs redirect to S3,
		// where a forwarded Authorization header would be rejected.
		req.Header.Set("Authorization", opts.authHeader)
	}

	resp, err := opts.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %v", redactURL(u), err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("download %s: HTTP %d", redactURL(u), resp.StatusCode)
	}
	if resp.ContentLength > opts.maxBytes {
		return "", fmt.Errorf("artifact too large: %d bytes (max %d)", resp.ContentLength, opts.maxBytes)
	}

	if err := os.MkdirAll(opts.destDir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(opts.destDir, "artifact-*.tar.gz")
	if err != nil {
		return "", err
	}
	// LimitReader with one extra byte: reading maxBytes+1 proves the
	// body exceeded the cap without ever buffering more than that.
	n, err := io.Copy(f, io.LimitReader(resp.Body, opts.maxBytes+1))
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil && n > opts.maxBytes {
		err = fmt.Errorf("artifact exceeded max size during download (max %d bytes)", opts.maxBytes)
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// newDownloadClient builds the shared artifact HTTP client. No overall
// timeout — large artifacts on slow links are legitimate — but a
// server that accepts the connection and then stalls is cut off at the
// header stage, and the request context bounds the rest.
func newDownloadClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			ResponseHeaderTimeout: 30 * time.Second,
			Proxy:                 http.ProxyFromEnvironment,
		},
	}
}

// redactURL renders a URL safe for logs and errors: scheme, host and
// path only. Query strings can carry signed tokens (S3, GitLab).
func redactURL(u *url.URL) string {
	return u.Scheme + "://" + u.Host + u.Path
}

// fetcher turns a webhook request into an extracted release directory.
// It exists as an interface so the deploy pipeline's unit tests can
// substitute a fake; the real implementation is releaseFetcher.
type fetcher interface {
	fetch(ctx context.Context, spec *appSpec, req deployRequest, progress func(phase string)) (string, error)
}

type releaseFetcher struct {
	client *http.Client
}

// fetch downloads, validates and extracts one artifact, returning the
// final release directory. Extraction happens into a hidden staging
// dir that is renamed into place only on success, so releases/ never
// contains a half-extracted version.
func (rf *releaseFetcher) fetch(ctx context.Context, spec *appSpec, req deployRequest, progress func(string)) (string, error) {
	progress("downloading")
	archive, err := downloadArtifact(ctx, downloadOpts{
		url:           req.URL,
		authHeader:    req.AuthHeader,
		destDir:       spec.dirs.tmp,
		maxBytes:      spec.maxArtifactSize,
		allowInsecure: spec.allowInsecure,
		allowedHosts:  spec.allowedHosts,
		client:        rf.client,
	})
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(archive) }()

	progress("extracting")
	releaseDir := spec.dirs.release(req.Version)
	staging := filepath.Join(spec.dirs.releases, ".extract-"+req.Version)
	if err := os.RemoveAll(staging); err != nil {
		return "", err
	}
	if err := extractArchive(archive, staging, spec.maxArtifactSize*decompressionRatioCap); err != nil {
		_ = os.RemoveAll(staging)
		return "", err
	}
	// A re-deploy of a non-running version replaces its directory.
	if err := os.RemoveAll(releaseDir); err != nil {
		_ = os.RemoveAll(staging)
		return "", err
	}
	if err := os.Rename(staging, releaseDir); err != nil {
		_ = os.RemoveAll(staging)
		return "", err
	}
	return releaseDir, nil
}

var _ fetcher = (*releaseFetcher)(nil)
