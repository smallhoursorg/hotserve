package liveswap

import (
	"fmt"
	"net/url"
	"path"
	"strings"
)

// artifactAllowEntry is one parsed artifact_allowlist entry: a host,
// optionally pinned to a path prefix. A bare host ("artifacts.corp")
// admits any path on that host — the right shape for single-tenant
// origins. A host with a path ("github.com/your-org/") pins the
// tenant: on multi-tenant platforms a host-only rule is trust
// theater, since anyone can publish under the same host; the tenant
// lives in the path, so the allowlist must too.
type artifactAllowEntry struct {
	host       string // lowercase, no port
	pathPrefix string // "" = any path; else cleaned, "/"-wrapped
}

// parseAllowlistEntry parses "host" or "host/path/prefix". Schemes are
// rejected rather than stripped so a pasted URL fails loudly instead
// of silently pinning less than the operator intended.
func parseAllowlistEntry(s string) (artifactAllowEntry, error) {
	if s == "" {
		return artifactAllowEntry{}, fmt.Errorf("empty artifact_allowlist entry")
	}
	if strings.Contains(s, "://") {
		return artifactAllowEntry{}, fmt.Errorf("artifact_allowlist entry %q must not include a scheme; use host[/path/] (https is enforced separately)", s)
	}
	host, rest, _ := strings.Cut(s, "/")
	if host == "" || strings.ContainsAny(host, " \t@:") {
		return artifactAllowEntry{}, fmt.Errorf("artifact_allowlist entry %q has no usable host", s)
	}
	e := artifactAllowEntry{host: strings.ToLower(host)}
	if rest != "" {
		// The prefix must already be canonical: if cleaning changes it
		// (dot segments, doubled slashes), the operator's mental model
		// and the effective pin disagree — reject rather than silently
		// pin something else.
		p := path.Clean("/" + rest)
		if p == "/" || p != "/"+strings.TrimSuffix(rest, "/") {
			return artifactAllowEntry{}, fmt.Errorf("artifact_allowlist entry %q has a non-canonical path prefix (no dot segments or doubled slashes)", s)
		}
		e.pathPrefix = p + "/"
	}
	return e, nil
}

func parseAllowlist(entries []string) ([]artifactAllowEntry, error) {
	out := make([]artifactAllowEntry, 0, len(entries))
	for _, s := range entries {
		e, err := parseAllowlistEntry(s)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// matches reports whether u is admitted by this entry. Hosts compare
// case-insensitively (DNS is); paths compare case-SENSITIVELY — some
// platforms route paths case-insensitively, but folding could only
// ever reject a differently-cased URL, never mis-admit one, so
// case-sensitive is the fail-closed choice. The path is matched
// decoded-and-cleaned (u.Path), so %2e%2e/%2f dot-segment tricks are
// resolved before the prefix check, and the prefix's trailing slash
// makes the match segment-bounded ("/your-org/" never admits
// "/your-org-evil/...").
func (e artifactAllowEntry) matches(u *url.URL) bool {
	if !strings.EqualFold(u.Hostname(), e.host) {
		return false
	}
	if e.pathPrefix == "" {
		return true
	}
	p := path.Clean("/" + u.Path)
	return p == strings.TrimSuffix(e.pathPrefix, "/") || strings.HasPrefix(p, e.pathPrefix)
}

// matchAllowlist returns the first entry admitting u.
func matchAllowlist(entries []artifactAllowEntry, u *url.URL) (artifactAllowEntry, bool) {
	for _, e := range entries {
		if e.matches(u) {
			return e, true
		}
	}
	return artifactAllowEntry{}, false
}

// describeAllowlist renders the entries for error messages.
func describeAllowlist(entries []artifactAllowEntry) string {
	parts := make([]string, len(entries))
	for i, e := range entries {
		parts[i] = e.host + strings.TrimSuffix(e.pathPrefix, "/")
	}
	return strings.Join(parts, ", ")
}
