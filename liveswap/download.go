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
	allowInsecure bool                 // permit plain http URLs
	allowlist     []artifactAllowEntry // required; no any-origin mode
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
	// Every refusal below is a validationError: an unusable URL is the
	// payload's problem, and 422-with-the-reason is what lets the CI
	// author fix it without an operator reading server logs.
	u, err := url.Parse(opts.url)
	if err != nil {
		return "", validationError{fmt.Sprintf("invalid artifact url: %v", err)}
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !opts.allowInsecure {
			return "", validationError{fmt.Sprintf("artifact url %s is plain http; use https or set allow_insecure_http", redactURL(u))}
		}
	default:
		return "", validationError{fmt.Sprintf("artifact url %s has unsupported scheme %q", redactURL(u), u.Scheme)}
	}
	entry, escapedPath, err := matchAllowlist(opts.allowlist, u)
	if err != nil {
		// A validationError so the webhook answers 422 with this exact
		// message in the body: an allowlist refusal is the payload's
		// problem, and the CI caller — who chose the URL — is the one
		// who can fix it (or ask the operator to extend the entry).
		return "", validationError{fmt.Sprintf("artifact url %s refused: %v", redactURL(u), err)}
	}
	// From here on the request's URL is never used directly. The URL
	// the fetch uses is a single concatenation whose provenance reads
	// left to right — scheme (constant), host and port (THE ALLOWLIST
	// ENTRY'S OWN CONFIG BYTES; the request's port bytes only under a
	// declared :* wildcard), the pinned prefix (config bytes again),
	// and only then the request's path suffix and vetted query. See
	// pinnedURLString.
	pinned, err := entry.pinnedURLString(u, escapedPath)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pinned, nil)
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
		// req.URL, not u: report the pinned URL the request actually
		// went to (host casing comes from config, not the payload).
		return "", fmt.Errorf("download %s: %w", redactURL(req.URL), err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("download %s: HTTP %d", redactURL(req.URL), resp.StatusCode)
	}
	if resp.ContentLength > opts.maxBytes {
		return "", fmt.Errorf("artifact too large: %d bytes (max %d)", resp.ContentLength, opts.maxBytes)
	}

	if err := os.MkdirAll(opts.destDir, 0o750); err != nil {
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
//
// CheckRedirect enforces the scheme policy on EVERY hop: Go's client
// happily follows an https -> http redirect (it strips Authorization
// cross-host, but not the scheme), which would let a permitted-looking
// https URL downgrade the fetch to cleartext — quietly defeating both
// the encrypted-artifacts promise and the accidental SSRF barrier that
// https-only provides (metadata/LAN endpoints are typically plain
// http). allowed_artifact_hosts is deliberately NOT re-checked per
// hop: the GitHub -> S3 redirect pattern is load-bearing, so the
// allowlist governs the first hop only (documented in the README).
func newDownloadClient(allowInsecure bool) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			ResponseHeaderTimeout: 30 * time.Second,
			Proxy:                 http.ProxyFromEnvironment,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				// Go's default cap, made explicit and testable.
				return fmt.Errorf("stopped after 10 redirects")
			}
			if req.URL.Scheme != "https" && !allowInsecure {
				return fmt.Errorf("redirect to %s downgrades to %s; artifacts must stay on https (or set allow_insecure_http)",
					redactURL(req.URL), req.URL.Scheme)
			}
			return nil
		},
	}
}

// redactURL renders a URL safe for logs and errors: scheme, host and
// path only. Query strings can carry signed tokens (S3, GitLab).
func redactURL(u *url.URL) string {
	// EscapedPath, not Path: the decoded path could reintroduce
	// characters like '?' or '#' that make the logged string read as
	// having a query or fragment it never had.
	return u.Scheme + "://" + u.Host + u.EscapedPath()
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
	// Rollback: the release is already extracted on disk from a prior
	// deploy — no fetch, no extract, just relaunch it.
	if req.rollback {
		releaseDir := spec.dirs.release(req.Version)
		if _, err := os.Stat(releaseDir); err != nil {
			if os.IsNotExist(err) {
				return "", validationError{fmt.Sprintf("no on-disk release %q to roll back to (it may have been pruned by keep)", req.Version)}
			}
			return "", err // a real I/O/permission error is a server failure, not a missing target
		}
		return releaseDir, nil
	}

	// Source the archive: a pushed upload already staged on disk, or a
	// pull from the artifact URL.
	archive := req.localArchive
	if archive == "" {
		progress("downloading")
		var err error
		archive, err = downloadArtifact(ctx, downloadOpts{
			url:           req.URL,
			authHeader:    req.AuthHeader,
			destDir:       spec.dirs.tmp,
			maxBytes:      spec.maxArtifactSize,
			allowInsecure: spec.allowInsecure,
			allowlist:     spec.allowlist,
			client:        rf.client,
		})
		if err != nil {
			return "", err
		}
	}
	defer func() { _ = os.Remove(archive) }()

	progress("extracting")
	releaseDir := spec.dirs.release(req.Version)
	staging := filepath.Join(spec.dirs.releases, ".extract-"+versionPathComponent(req.Version))
	if err := os.RemoveAll(staging); err != nil {
		return "", err
	}
	if err := extractArchive(archive, staging, spec.maxArtifactSize*decompressionRatioCap); err != nil {
		_ = os.RemoveAll(staging)
		return "", err
	}
	// Defensive: versions are immutable, so Deploy rejects an existing
	// version before we get here — releaseDir normally does not exist.
	// Kept so a stray leftover can't fail the rename.
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
