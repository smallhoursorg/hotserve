package liveswap

import (
	"net/url"
	"strings"
	"testing"
)

func TestParseAllowlistEntry(t *testing.T) {
	for in, wantErr := range map[string]bool{
		"artifacts.corp":             false,
		"github.com/smallhoursorg/":  false,
		"github.com/smallhoursorg":   false, // trailing slash optional
		"GitHub.com/Org/deep/prefix": false,
		"":                           true,
		"https://github.com/org":     true, // schemes rejected loudly
		"github.com/../escape":       true,
		"github.com//":               true, // cleans to "/", no pin left
		"github.com/a//b":            true, // non-canonical
		"user@github.com/org":        true,
		"github.com:8443/org":        false, // declared port pin
		"github.com/org%2f":          true,  // entries are literal bytes only
		// port declarations
		"artifacts.corp:*":    true,  // wildcards removed — declare the literal port
		"127.0.0.1:8200?p":    false, // literal port + query decl
		"pinned.corp:8443/a/": false,
		"github.com:":         true, // dangling colon
		"github.com:0":        true,
		"github.com:65536":    true,
		"github.com:08443":    true, // leading zero — not literal port bytes
		"github.com:abc":      true,
		"github.com:1:2/x":    true, // host part may not contain :
		// query parameter declarations
		"gitlab.com/api/v4/projects/42/?job":     false,
		"gitlab.com/api/v4/projects/42/?job&ref": false,
		"bucket.s3.corp/releases/?X-Amz-*":       false,
		"artifacts.corp?sig":                     false, // bare host + query decl
		"gitlab.com/x/?":                         true,  // dangling ?
		"gitlab.com/x/?*":                        true,  // bare wildcard = coarse opt-in, refused
		"gitlab.com/x/?job=build":                true,  // names only, no values
		"gitlab.com/x/?job&":                     true,  // empty name
		"gitlab.com/x/?jo b":                     true,  // charset
		"gitlab.com/x/?a*b":                      true,  // wildcard must be trailing
		"gitlab.com/x/?%6aob":                    true,  // literal bytes only (global rule)
	} {
		_, err := parseAllowlistEntry(in)
		if (err != nil) != wantErr {
			t.Errorf("parseAllowlistEntry(%q) err=%v, wantErr=%v", in, err, wantErr)
		}
	}
}

// Matching is tri-state: admitted, not covered, or rejected outright
// for a non-canonical path (the fail-closed gate). The table encodes
// which of the three each probe must land in.
func TestMatchAllowlist(t *testing.T) {
	entries := mustAllowlist(t, "github.com/smallhoursorg/", "artifacts.corp")

	const (
		admitted     = "admitted"
		notCovered   = "not covered"
		nonCanonical = "non-canonical"
	)
	for rawURL, want := range map[string]string{
		// the org pin
		"https://github.com/smallhoursorg/hotserve/releases/download/v1/a.tgz": admitted,
		"https://github.com/smallhoursorg":                                     admitted,   // the prefix itself
		"https://GITHUB.COM/smallhoursorg/x":                                   admitted,   // host case folds
		"https://github.com/SmallHoursOrg/x":                                   notCovered, // path bytes are literal
		"https://github.com/smallhoursorg-e/x":                                 notCovered, // segment boundary
		"https://github.com/evilorg/x":                                         notCovered,
		// the bare host
		"https://artifacts.corp/anything/at/all.tgz":  admitted,
		"https://ARTIFACTS.CORP/x":                    admitted,
		"https://artifacts.corp":                      admitted, // no path at all
		"https://artifacts.corp.evil.com/x":           notCovered,
		"https://github.com@evil.com/smallhoursorg/x": notCovered, // userinfo trick
		"https://objects.githubusercontent.com/x":     notCovered, // redirect host unlisted
		// the fail-closed canonical gate
		"https://github.com/smallhoursorg/../evilorg/x":     nonCanonical,
		"https://github.com/smallhoursorg/%2e%2e/evilorg/x": nonCanonical, // encoded dots
		"https://github.com/smallhoursorg/.%2e/x":           nonCanonical, // mixed spelling
		"https://github.com/%73mallhoursorg/x":              notCovered,   // encoded org ≠ literal bytes
		"https://github.com/smallhoursorg//x":               nonCanonical, // doubled slash
		"https://github.com/smallhoursorg/x/":               nonCanonical, // trailing slash
		"https://github.com/smallhoursorg/a%2fb":            nonCanonical, // encoded slash
		// backslash: the other separator byte — IIS-style origins
		// normalize \ to /, turning %5C..%5C into a dot segment
		"https://github.com/smallhoursorg/%5C..%5Cetc/x": nonCanonical,
		"https://github.com/smallhoursorg/a%5cb":         nonCanonical, // lowercase spelling
		"https://github.com/smallhoursorg/a\\b":          nonCanonical, // raw backslash (EscapedPath renders %5C)
		// lenient-decoder differentials: overlong UTF-8 and control bytes
		"https://github.com/smallhoursorg/%c0%af.tgz":    nonCanonical, // overlong "/" (IIS-era traversal)
		"https://github.com/smallhoursorg/a%00b.tgz":     nonCanonical, // control byte
		"https://github.com/smallhoursorg/caf%C3%A9.tgz": admitted,     // real UTF-8 names stay legal
		"https://artifacts.corp/./x":                     nonCanonical,
	} {
		u, err := url.Parse(rawURL)
		if err != nil {
			t.Fatalf("parse %q: %v", rawURL, err)
		}
		_, _, err = matchAllowlist(entries, u)
		got := "admitted"
		switch {
		case err != nil && strings.Contains(err.Error(), "not covered"):
			got = "not covered"
		case err != nil:
			got = "non-canonical"
		}
		if got != want {
			t.Errorf("match(%q) = %s (%v), want %s", rawURL, got, err, want)
		}
	}
}

// The constructed URL is asserted byte-for-byte: everything before
// the input's suffix must be constants and the entry's config bytes.
func TestPinnedURLString(t *testing.T) {
	entries := mustAllowlist(t, "github.com/smallhoursorg/?sig", "artifacts.corp?x", "ports.corp:8443?x")

	for rawURL, want := range map[string]string{
		// host case comes from config, not from the caller
		"https://GITHUB.COM/smallhoursorg/hotserve/releases/v1.tgz": "https://github.com/smallhoursorg/hotserve/releases/v1.tgz",
		// the prefix itself, no suffix
		"https://github.com/smallhoursorg": "https://github.com/smallhoursorg",
		// query survives (signed URLs), fragment and userinfo do not
		"https://leak:hunter2@github.com/smallhoursorg/a.tgz?sig=abc#frag": "https://github.com/smallhoursorg/a.tgz?sig=abc",
		// encoding in the suffix is preserved literally, never
		// re-encoded; a declared port is the entry's own config bytes
		"https://ARTIFACTS.CORP/dir/a%20b.tgz?x=1": "https://artifacts.corp/dir/a%20b.tgz?x=1",
		"https://PORTS.CORP:8443/a.tgz?x=1":        "https://ports.corp:8443/a.tgz?x=1",
		// http passes through only as the literal alternative arm
		"http://artifacts.corp/a.tgz": "http://artifacts.corp/a.tgz",
	} {
		u, err := url.Parse(rawURL)
		if err != nil {
			t.Fatalf("parse %q: %v", rawURL, err)
		}
		entry, ep, err := matchAllowlist(entries, u)
		if err != nil {
			t.Fatalf("match(%q): %v", rawURL, err)
		}
		got, err := entry.pinnedURLString(u, ep)
		if err != nil {
			t.Fatalf("pinned(%q): %v", rawURL, err)
		}
		if got != want {
			t.Errorf("pinned(%q)\n  got  %q\n  want %q", rawURL, got, want)
		}
	}
}

// Round-trip invariant: reparsing our own construction must always
// yield the entry's host, a path under the pin, and no credentials —
// for every admitted URL, not just the table above.
func assertPinnedInvariants(t *testing.T, entry artifactAllowEntry, built string) {
	t.Helper()
	rt, err := url.Parse(built)
	if err != nil {
		t.Fatalf("constructed URL does not reparse: %q: %v", built, err)
	}
	if rt.Hostname() != entry.host {
		t.Fatalf("constructed host %q escaped the pin %q (%q)", rt.Hostname(), entry.host, built)
	}
	if entry.pathPrefix != "" &&
		rt.EscapedPath() != strings.TrimSuffix(entry.pathPrefix, "/") &&
		!strings.HasPrefix(rt.EscapedPath(), entry.pathPrefix) {
		t.Fatalf("constructed path %q escaped the pin %q (%q)", rt.EscapedPath(), entry.pathPrefix, built)
	}
	if rt.User != nil {
		t.Fatalf("constructed URL carries credentials: %q", built)
	}
	if rt.Fragment != "" {
		t.Fatalf("constructed URL carries a fragment: %q", built)
	}
	if err := entry.vetQuery(rt.RawQuery); err != nil {
		t.Fatalf("constructed URL carries an unvetted query: %q: %v", built, err)
	}
}

func FuzzPinnedURL(f *testing.F) {
	for _, s := range []string{
		"https://github.com/smallhoursorg/a.tgz",
		"https://github.com/smallhoursorg/../../x",
		"https://github.com/smallhoursorg/%2e%2e/x",
		"https://user:pw@github.com/smallhoursorg/x?sig=1#f",
		"https://github.com/smallhoursorg/x?p=2",
		"https://github.com/smallhoursorg/x?%70=2",
		"https://github.com/smallhoursorg/x?sig=1&p=2",
		"https://artifacts.corp/x?X-Amz-Signature=abc&X-Amz-Date=1",
		"https://github.com/smallhoursorg/x?sig=a b",
		"https://github.com/smallhoursorg/x?sig=%zz",
		"https://github.com/smallhoursorg/x?sig=ok;p=2",
		"https://github.com@evil.com/smallhoursorg/x",
		"https://github.com/smallhoursorg/a%2fb%00c",
		"https://github.com/smallhoursorg/%5C..%5Cetc/x",
		"https://github.com/smallhoursorg/%c0%af",
		"http://artifacts.corp:9/x//y",
		"https://ports.corp:8443/x?X-Amz-Date=1",
		"https://ports.corp:9/x",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, rawURL string) {
		u, err := url.Parse(rawURL)
		if err != nil {
			return
		}
		entries := []artifactAllowEntry{
			{host: "github.com", pathPrefix: "/smallhoursorg/", queryParams: []string{"sig"}},
			{host: "artifacts.corp", queryParams: []string{"X-Amz-*"}},
			{host: "ports.corp", port: "8443", queryParams: []string{"X-Amz-*"}},
		}
		entry, ep, err := matchAllowlist(entries, u)
		if err != nil {
			return // rejected is always a safe outcome
		}
		built, err := entry.pinnedURLString(u, ep)
		if err != nil {
			return // refusing is always a safe outcome
		}
		assertPinnedInvariants(t, entry, built)
	})
}

// The construction defends its own boundary even against a broken or
// bypassed matcher: fed a lookalike path directly, it must refuse
// rather than emit "/prefix-not/..." wearing the config's bytes.
func TestPinnedURLStringRefusesBoundaryViolation(t *testing.T) {
	entry := artifactAllowEntry{host: "github.com", pathPrefix: "/smallhoursorg/"}
	u, _ := url.Parse("https://github.com/smallhoursorg-not/x.tgz")
	if built, err := entry.pinnedURLString(u, u.EscapedPath()); err == nil {
		t.Fatalf("boundary violation constructed %q; want refusal", built)
	}
	// And the safe complement: any "/"-rooted suffix — even one the
	// matcher would never have admitted — lands under the pin.
	u2, _ := url.Parse("https://github.com/evilorg/x.tgz")
	built, err := entry.pinnedURLString(u2, u2.EscapedPath())
	if err != nil {
		t.Fatalf("rooted suffix should construct (safely): %v", err)
	}
	assertPinnedInvariants(t, entry, built)
	if built != "https://github.com/smallhoursorg/evilorg/x.tgz" {
		t.Fatalf("rooted suffix landed outside the pin: %q", built)
	}
}

// Query admission is per-name against the entry's declaration, closed
// by default. Only names are judged: on a WordPress-style host a query
// NAME can override the path routing entirely (?p=2 serves post 2
// regardless of path), so every name needs an operator's vouching —
// while values stay free (a value cannot select a route, and presigned
// values must pass through byte-identical).
func TestMatchAllowlistQuery(t *testing.T) {
	entries := mustAllowlist(t,
		"github.com/smallhoursorg/",               // declares no params
		"gitlab.corp/api/v4/projects/42/?job&ref", // named params
		"bucket.s3.corp/releases/?X-Amz-*",        // wildcard family
	)
	for rawURL, wantAdmitted := range map[string]bool{
		"https://github.com/smallhoursorg/a.tgz":                                 true,  // no query, nothing to vet
		"https://github.com/smallhoursorg/a.tgz?p=2":                             false, // entry declares none
		"https://gitlab.corp/api/v4/projects/42/download?job=build":              true,
		"https://gitlab.corp/api/v4/projects/42/download?job=build&ref=v1":       true,
		"https://gitlab.corp/api/v4/projects/42/download?job=a&job=b":            true,  // duplicates of a declared name
		"https://gitlab.corp/api/v4/projects/42/download?job":                    true,  // valueless
		"https://gitlab.corp/api/v4/projects/42/download?job=build&p=2":          false, // one bad name spoils it
		"https://gitlab.corp/api/v4/projects/42/download?jobs=build":             false, // exact names, no prefix creep
		"https://gitlab.corp/api/v4/projects/42/download?%6aob=build":            false, // encoded name refused outright
		"https://gitlab.corp/api/v4/projects/42/download?=x":                     false, // nameless component
		"https://gitlab.corp/api/v4/projects/42/download?job=build&":             false, // empty trailing component
		"https://bucket.s3.corp/releases/a.tgz?X-Amz-Signature=abc&X-Amz-Date=1": true,  // wildcard family
		"https://bucket.s3.corp/releases/a.tgz?X-Amz-Signature=abc&versionId=9":  false,
		// values: never re-encoded, but bytes must be RFC 3986 query
		// characters — anything outside would hit the request line raw
		"https://gitlab.corp/api/v4/projects/42/download?job=a%2Fb%20c":      true,  // escapes pass through
		"https://gitlab.corp/api/v4/projects/42/download?job=a!$'()*+,:@/?b": true,  // sub-delims + extra pchars
		"https://gitlab.corp/api/v4/projects/42/download?job=a b":            false, // raw space
		"https://gitlab.corp/api/v4/projects/42/download?job=a\"b":           false, // raw quote
		"https://gitlab.corp/api/v4/projects/42/download?job=<a>":            false,
		"https://gitlab.corp/api/v4/projects/42/download?job=a|b":            false,
		"https://gitlab.corp/api/v4/projects/42/download?job=a\\b":           false, // raw backslash
		"https://gitlab.corp/api/v4/projects/42/download?job=caf\u00e9":      false, // non-ASCII
		// semicolon-separator differential: some servers split on ";"
		// too, so a raw semicolon would smuggle an undeclared name
		"https://gitlab.corp/api/v4/projects/42/download?job=ok;p=2":   false,
		"https://gitlab.corp/api/v4/projects/42/download?job=ok%3Bp=2": true, // encoded is unambiguous
		"https://gitlab.corp/api/v4/projects/42/download?job;p=2":      false,
	} {
		u, err := url.Parse(rawURL)
		if err != nil {
			t.Fatalf("parse %q: %v", rawURL, err)
		}
		_, _, err = matchAllowlist(entries, u)
		if (err == nil) != wantAdmitted {
			t.Errorf("match(%q) err=%v, wantAdmitted=%v", rawURL, err, wantAdmitted)
		}
	}
}

// The refusal messages are the operator's and CI author's debugging
// surface: they must name the offending parameter and the entry's own
// declaration — and must NEVER include a query value, which may be a
// presigned signature or token (they end up in logs and the webhook
// response body).
func TestQueryRefusalMessages(t *testing.T) {
	entries := mustAllowlist(t, "gitlab.corp/api/?job", "plain.corp/dl/")

	u, _ := url.Parse("https://gitlab.corp/api/a.tgz?job=ok&private_token=SECRETVALUE")
	_, _, err := matchAllowlist(entries, u)
	if err == nil {
		t.Fatal("undeclared parameter admitted")
	}
	msg := err.Error()
	for _, want := range []string{`"private_token"`, `"gitlab.corp/api?job"`} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal should contain %s: %q", want, msg)
		}
	}
	if strings.Contains(msg, "SECRETVALUE") {
		t.Fatalf("refusal leaked a query value: %q", msg)
	}

	// The declares-none case should show the ready-to-paste fix.
	u2, _ := url.Parse("https://plain.corp/dl/a.tgz?sig=SECRETVALUE")
	_, _, err = matchAllowlist(entries, u2)
	if err == nil {
		t.Fatal("query admitted by a no-params entry")
	}
	msg = err.Error()
	for _, want := range []string{`"sig"`, "declares no query parameters", `"plain.corp/dl?sig"`} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal should contain %s: %q", want, msg)
		}
	}
	if strings.Contains(msg, "SECRETVALUE") {
		t.Fatalf("refusal leaked a query value: %q", msg)
	}
}

// Builder self-defense, query edition: like the path-boundary guard,
// an unvetted query must be refused by the construction itself even
// when the matcher is bypassed.
func TestPinnedURLStringRefusesUnvettedQuery(t *testing.T) {
	entry := artifactAllowEntry{host: "github.com", pathPrefix: "/smallhoursorg/"}
	u, _ := url.Parse("https://github.com/smallhoursorg/x.tgz?p=2")
	if built, err := entry.pinnedURLString(u, u.EscapedPath()); err == nil {
		t.Fatalf("unvetted query constructed %q; want refusal", built)
	}
}

// The port selects which SERVICE on the host answers, so it follows
// the same closed default as the path and query: an entry admits only
// the scheme default unless it declares a port, or :* for the
// loopback/dev case where test servers bind randomly.
func TestMatchAllowlistPort(t *testing.T) {
	entries := mustAllowlist(t, "plain.corp/dl/", "pinned.corp:8443")

	for rawURL, wantAdmitted := range map[string]bool{
		"https://plain.corp/dl/a.tgz":      true,
		"https://plain.corp:8443/dl/a.tgz": false, // undeclared port
		"https://plain.corp:443/dl/a.tgz":  false, // explicit default is not literal-default
		"https://pinned.corp:8443/a.tgz":   true,
		"https://pinned.corp/a.tgz":        false, // pin means exactly that port
		"https://pinned.corp:9/a.tgz":      false,
	} {
		u, err := url.Parse(rawURL)
		if err != nil {
			t.Fatalf("parse %q: %v", rawURL, err)
		}
		_, _, err = matchAllowlist(entries, u)
		if (err == nil) != wantAdmitted {
			t.Errorf("match(%q) err=%v, wantAdmitted=%v", rawURL, err, wantAdmitted)
		}
	}

	// The refusal names the input port and shows the entry to declare.
	u, _ := url.Parse("https://plain.corp:8443/dl/a.tgz")
	_, _, err := matchAllowlist(entries, u)
	if err == nil {
		t.Fatal("undeclared port admitted")
	}
	for _, want := range []string{`"8443"`, `"plain.corp:8443"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("port refusal should contain %s: %q", want, err)
		}
	}
}

// Builder self-defense, port edition: an entry pinning the default
// port must refuse to emit any other, even with the matcher bypassed.
func TestPinnedURLStringRefusesUnadmittedPort(t *testing.T) {
	entry := artifactAllowEntry{host: "github.com", pathPrefix: "/smallhoursorg/"}
	u, _ := url.Parse("https://github.com:6379/smallhoursorg/x.tgz")
	if built, err := entry.pinnedURLString(u, u.EscapedPath()); err == nil {
		t.Fatalf("unadmitted port constructed %q; want refusal", built)
	}
}
