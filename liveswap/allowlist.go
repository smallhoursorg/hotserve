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
	// port pins which port on the host may be fetched from: "" admits
	// only the scheme default (no explicit port in the URL), a number
	// admits exactly that port, and "*" admits any (loopback/dev,
	// where test servers bind randomly). Closed by default for the
	// same reason as the path and query: the port selects which
	// SERVICE on the host answers, and that choice belongs to the
	// operator, not the webhook payload.
	port string
	// queryParams are the query parameter NAMES this entry vouches
	// for, closed by default: an entry that declares none admits no
	// query string at all. The query is the one part of an artifact
	// URL where a name can carry routing semantics on the far server
	// (WordPress-style ?p= overrides the path entirely), so each name
	// is an operator assertion "I know what this parameter does on
	// this endpoint". A trailing * declares a prefix family
	// (X-Amz-* for presigned URLs). Values are never re-encoded or
	// matched — a value cannot select a route, only a name can — but
	// every query byte must be an RFC 3986 query character (see
	// isQueryByte).
	queryParams []string
}

// parseAllowlistEntry parses "host[/path/prefix][?param&param...]".
// Schemes are rejected rather than stripped so a pasted URL fails
// loudly instead of silently pinning less than the operator intended.
func parseAllowlistEntry(s string) (artifactAllowEntry, error) {
	if s == "" {
		return artifactAllowEntry{}, fmt.Errorf("empty artifact_allowlist entry")
	}
	if strings.Contains(s, "://") {
		return artifactAllowEntry{}, fmt.Errorf("artifact_allowlist entry %q must not include a scheme; use host[/path/][?param...] (https is enforced separately)", s)
	}
	if strings.Contains(s, "%") {
		return artifactAllowEntry{}, fmt.Errorf("artifact_allowlist entry %q must be literal (no percent-encoding): the entry's own bytes become the outgoing URL prefix", s)
	}
	hostAndPath, queryDecl, hasQuery := strings.Cut(s, "?")
	hostPort, rest, _ := strings.Cut(hostAndPath, "/")
	host, port, hasPort := strings.Cut(hostPort, ":")
	if host == "" || strings.ContainsAny(host, " \t@:") {
		return artifactAllowEntry{}, fmt.Errorf("artifact_allowlist entry %q has no usable host", s)
	}
	e := artifactAllowEntry{host: strings.ToLower(host)}
	if hasPort {
		if err := validPortDecl(port); err != nil {
			return artifactAllowEntry{}, fmt.Errorf("artifact_allowlist entry %q: %w", s, err)
		}
		e.port = port
	}
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
	if hasQuery {
		if queryDecl == "" {
			return artifactAllowEntry{}, fmt.Errorf("artifact_allowlist entry %q has a dangling ? — declare parameter names (e.g. ?job) or drop the ?", s)
		}
		for _, name := range strings.Split(queryDecl, "&") {
			if err := validQueryParamDecl(name); err != nil {
				return artifactAllowEntry{}, fmt.Errorf("artifact_allowlist entry %q: %w", s, err)
			}
			e.queryParams = append(e.queryParams, name)
		}
	}
	return e, nil
}

// validPortDecl vets a declared port: a literal port number the
// input URL must match byte-for-byte, or * to admit any port (for
// loopback and test hosts where servers bind randomly).
func validPortDecl(port string) error {
	if port == "*" {
		return nil
	}
	if port == "" {
		return fmt.Errorf("dangling colon — declare a port number (e.g. :8443), :* for any, or drop the colon")
	}
	if len(port) > 5 || port[0] == '0' {
		return fmt.Errorf("port %q is not a valid port number", port)
	}
	n := 0
	for i := 0; i < len(port); i++ {
		if port[i] < '0' || port[i] > '9' {
			return fmt.Errorf("port %q is not a valid port number", port)
		}
		n = n*10 + int(port[i]-'0')
	}
	if n < 1 || n > 65535 {
		return fmt.Errorf("port %q is out of range (1-65535)", port)
	}
	return nil
}

// portAllowed compares the URL's literal port bytes against the
// declaration. An entry with no port admits only the scheme default —
// an explicit :443 is refused too, because no legitimate artifact URL
// spells the default out and literal comparison is the fail-closed
// direction.
func (e artifactAllowEntry) portAllowed(u *url.URL) bool {
	switch e.port {
	case "*":
		return true
	case "":
		return u.Port() == ""
	default:
		return u.Port() == e.port
	}
}

// validQueryParamDecl vets one declared parameter name: unreserved
// characters only (so the literal-bytes comparison in vetQuery can
// never be ambiguous), with an optional trailing * for a prefix
// family. A bare * is rejected — "any query at all" would reopen
// exactly the routing-override hole the declaration exists to close.
func validQueryParamDecl(name string) error {
	if name == "" {
		return fmt.Errorf("empty query parameter name")
	}
	if name == "*" {
		return fmt.Errorf("bare * would allow every query parameter; declare the names this endpoint actually uses")
	}
	base := strings.TrimSuffix(name, "*")
	if base == "" || strings.Contains(base, "*") {
		return fmt.Errorf("query parameter %q: * is only valid as a trailing wildcard (e.g. X-Amz-*)", name)
	}
	for _, r := range base {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '-' || r == '_' || r == '.' || r == '~' {
			continue
		}
		return fmt.Errorf("query parameter %q contains %q; names are unreserved characters only (A-Za-z0-9-._~)", name, r)
	}
	return nil
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

// canonicalEscapedPath is the fail-closed gate every artifact URL
// path passes before matching: the ENCODED path (EscapedPath) must
// already be in canonical form — rooted, no empty segments, no
// segments that are or decode to "." / "..", and no encoded slashes
// (%2F), which would make the encoded and decoded segment structures
// disagree. Anything non-canonical is rejected rather than
// normalized: a deploy endpoint has no legitimate caller that
// percent-encodes dots or doubles slashes, and rejection means the
// literal-bytes prefix matching below can never be tricked by an
// alternate spelling.
func canonicalEscapedPath(u *url.URL) (string, error) {
	ep := u.EscapedPath()
	if ep == "" {
		return "", nil // https://host with no path at all
	}
	if !strings.HasPrefix(ep, "/") {
		return "", fmt.Errorf("path %q is not rooted", ep)
	}
	if strings.Contains(strings.ToLower(ep), "%2f") {
		return "", fmt.Errorf("path contains an encoded slash")
	}
	for _, seg := range strings.Split(ep[1:], "/") {
		if seg == "" {
			return "", fmt.Errorf("path has an empty segment (doubled or trailing slash)")
		}
		dec, err := url.PathUnescape(seg)
		if err != nil || dec == "." || dec == ".." {
			return "", fmt.Errorf("path has a dot segment")
		}
	}
	return ep, nil
}

// vetQuery gates the input URL's raw query against the entry's
// declared parameter names. Only NAMES are checked (values cannot
// select a route; names can) and only names appear in errors — a
// query value may be a presigned-URL signature, and error strings
// end up in logs and webhook responses. Names must match the
// declaration as literal bytes: a percent-encoded spelling like
// %70=2 (which many servers decode to p=2) is refused outright, so
// there is no decode step whose behavior could differ from the
// artifact host's.
func (e artifactAllowEntry) vetQuery(rawQuery string) error {
	if rawQuery == "" {
		return nil
	}
	// Character allowlist first: every byte must be an RFC 3986 query
	// character. Legitimate URLs are properly encoded by definition —
	// presigned and API URLs never carry raw spaces or quotes — so
	// this refuses only URLs that were already broken, and it is the
	// layer the name check cannot provide: a declared parameter's
	// VALUE passes the name gate, and without this rule its bytes
	// would reach the request line raw (a raw space garbles it into a
	// baffling remote 400; CR/LF is already impossible — url.Parse
	// rejects control characters — so this is hygiene, not smuggling).
	for i := 0; i < len(rawQuery); i++ {
		if !isQueryByte(rawQuery[i]) {
			return fmt.Errorf("query string contains %q at byte %d; only RFC 3986 query characters are allowed — percent-encode everything else", rawQuery[i], i)
		}
	}
	for _, comp := range strings.Split(rawQuery, "&") {
		name, _, _ := strings.Cut(comp, "=")
		if name == "" {
			return fmt.Errorf("query string has a component with no parameter name")
		}
		if strings.ContainsAny(name, "%+") {
			return fmt.Errorf("query parameter name %q is encoded; parameter names must be literal bytes", name)
		}
		if e.queryParamAllowed(name) {
			continue
		}
		if len(e.queryParams) == 0 {
			return fmt.Errorf("query parameter %q is not permitted: allowlist entry %q declares no query parameters — if this endpoint legitimately uses it, declare it in the entry: %q", name, e.String(), e.String()+"?"+name)
		}
		return fmt.Errorf("query parameter %q is not among the parameters declared by allowlist entry %q — add it to the entry if this endpoint legitimately uses it", name, e.String())
	}
	return nil
}

// isQueryByte reports whether b may appear in a query per RFC 3986:
// pchar (unreserved / sub-delims / ":" / "@") plus "/" and "?", plus
// "%" as the escape introducer (escape triplets are not validated —
// values pass through byte-identical, and how to decode its own query
// is the artifact host's business).
func isQueryByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	}
	switch b {
	case '-', '.', '_', '~', // unreserved
		'!', '$', '&', '\'', '(', ')', '*', '+', ',', ';', '=', // sub-delims
		':', '@', '/', '?', '%':
		return true
	}
	return false
}

func (e artifactAllowEntry) queryParamAllowed(name string) bool {
	for _, p := range e.queryParams {
		if fam, ok := strings.CutSuffix(p, "*"); ok {
			if strings.HasPrefix(name, fam) {
				return true
			}
		} else if name == p {
			return true
		}
	}
	return false
}

func (e artifactAllowEntry) portDescription() string {
	if e.port == "" {
		return "the scheme's default port"
	}
	return "port " + e.port
}

// String renders the entry back in its config syntax, for error
// messages that quote the operator's own declaration.
func (e artifactAllowEntry) String() string {
	s := e.host
	if e.port != "" {
		s += ":" + e.port
	}
	s += strings.TrimSuffix(e.pathPrefix, "/")
	if len(e.queryParams) > 0 {
		s += "?" + strings.Join(e.queryParams, "&")
	}
	return s
}

// matchAllowlist gates u and returns the admitting entry plus u's
// canonical escaped path. Hosts compare case-insensitively (DNS is);
// the path prefix compares as LITERAL BYTES against the canonical
// escaped path — case-sensitive and encoding-exact, which is the
// fail-closed direction (an alternate spelling is rejected, never
// mis-admitted). Admission requires the query to pass too: when an
// entry covers the host+path but refuses the query, that refusal is
// reported in preference to a generic "not covered", so the caller's
// error says exactly which parameter tripped the gate.
func matchAllowlist(entries []artifactAllowEntry, u *url.URL) (artifactAllowEntry, string, error) {
	ep, err := canonicalEscapedPath(u)
	if err != nil {
		return artifactAllowEntry{}, "", err
	}
	var nearMiss error // an entry covered host+path but refused port or query
	for _, e := range entries {
		if !strings.EqualFold(u.Hostname(), e.host) {
			continue
		}
		if e.pathPrefix == "" ||
			ep == strings.TrimSuffix(e.pathPrefix, "/") ||
			strings.HasPrefix(ep, e.pathPrefix) {
			if !e.portAllowed(u) {
				if nearMiss == nil {
					nearMiss = fmt.Errorf("url uses port %q but allowlist entry %q admits only %s — declare the port in the entry (e.g. %q) if this host really serves artifacts there", u.Port(), e.String(), e.portDescription(), e.host+":"+u.Port())
				}
				continue
			}
			if err := e.vetQuery(u.RawQuery); err != nil {
				if nearMiss == nil {
					nearMiss = err
				}
				continue
			}
			return e, ep, nil
		}
	}
	if nearMiss != nil {
		return artifactAllowEntry{}, "", nearMiss
	}
	return artifactAllowEntry{}, "", fmt.Errorf("not covered by artifact_allowlist (%s)", describeAllowlist(entries))
}

// pinnedURLString builds the URL the fetch will actually use, as one
// concatenation whose provenance reads left to right:
//
//	scheme  ://  host:port     /pinned/prefix  /suffix  ?query
//	consts       CONFIG BYTES  CONFIG BYTES    input    input, names vetted
//
// The port is the entry's own declaration; the input's port bytes are
// used only under a declared :* wildcard.
//
// Everything before the suffix is a constant or the allowlist entry's
// own config string — the request contributes only its numeric port,
// the path suffix beyond the pinned prefix, and the query, whose
// parameter names must all be declared by the entry (values pass
// through untouched: signed URLs break if the query is re-encoded).
// Userinfo and fragment are structurally absent: credentials travel
// via auth_header only.
func (e artifactAllowEntry) pinnedURLString(u *url.URL, escapedPath string) (string, error) {
	scheme := "https"
	if u.Scheme == "http" { // both arms are literals; vetted upstream
		scheme = "http"
	}
	host := e.host
	switch e.port {
	case "*":
		if p := u.Port(); p != "" {
			host += ":" + p
		}
	case "":
		// Self-defense, like the path boundary and query re-vet: an
		// entry that pins the default port must never emit another.
		if u.Port() != "" {
			return "", fmt.Errorf("internal: url port %q was not admitted by entry %q", u.Port(), e.String())
		}
	default:
		host += ":" + e.port // the config's own bytes
	}
	var b strings.Builder
	b.WriteString(scheme)
	b.WriteString("://")
	b.WriteString(host)
	if e.pathPrefix != "" {
		cfg := strings.TrimSuffix(e.pathPrefix, "/")
		suffix := strings.TrimPrefix(escapedPath, cfg)
		// The construction enforces its own boundary, independent of
		// the matcher: a suffix is either empty or begins with "/".
		// With that single invariant, every constructible URL lands
		// under the pinned prefix — "/prefix" + "/anything" cannot
		// spell a lookalike like "/prefix-not/...". Refusing here
		// means a future matcher bug fails a deploy loudly instead of
		// fetching from an adjacent-looking tenant.
		if suffix != "" && !strings.HasPrefix(suffix, "/") {
			return "", fmt.Errorf("internal: path %q does not sit on the pinned prefix boundary %q", escapedPath, e.pathPrefix)
		}
		b.WriteString(cfg)    // the config's own bytes
		b.WriteString(suffix) // the input's suffix: "" or "/..."
	} else {
		b.WriteString(escapedPath)
	}
	if u.RawQuery != "" {
		// Same self-defense as the path boundary above: the builder
		// re-vets the query itself, so an unvetted query can never be
		// emitted even if a future caller skips the matcher.
		if err := e.vetQuery(u.RawQuery); err != nil {
			return "", fmt.Errorf("internal: %w", err)
		}
		b.WriteString("?")
		b.WriteString(u.RawQuery)
	}
	return b.String(), nil
}

// describeAllowlist renders the entries for error messages.
func describeAllowlist(entries []artifactAllowEntry) string {
	parts := make([]string, len(entries))
	for i, e := range entries {
		parts[i] = e.String()
	}
	return strings.Join(parts, ", ")
}
