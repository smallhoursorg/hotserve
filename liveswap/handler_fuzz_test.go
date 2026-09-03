// Fuzz target for the deploy webhook's request parsing: the JSON body
// (deployURL) and the version query parameter (deployPush,
// deployRollback), both attacker-shaped once a token is accepted. The
// property is that every request the gate lets through carries a
// version that is a single, non-escaping path component under the
// releases dir — the deploy pipeline os.RemoveAll's that path — and a
// non-empty URL and a header value the transport will not choke on.
// Whether the URL is allowed is FuzzPinnedURL's job; this target stops
// where the payload becomes a deployRequest.
package liveswap

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func FuzzDeployRequest(f *testing.F) {
	for _, s := range [][2]string{
		{`{"url":"https://x/a.tgz","version":"v1"}`, "v1"},
		{`{"url":"https://x/a.tgz","version":".."}`, ".."},
		{`{"url":"https://x/a.tgz","version":"."}`, "."},
		{`{"url":"https://x/a.tgz","version":"../x"}`, "../x"},
		{`{"url":"https://x/a.tgz","version":"a/b"}`, "a/b"},
		{`{"url":"https://x/a.tgz","version":"a\\b"}`, `a\b`},
		{`{"url":"https://x/a.tgz","version":"v1\u0000"}`, "v1\x00"},
		{`{"url":"https://x/a.tgz","version":""}`, ""},
		{`{"url":"https://x/a.tgz","version":"` + strings.Repeat("a", 65) + `"}`, strings.Repeat("a", 65)},
		{`{"url":"https://x/a.tgz","version":"...."}`, "...."},
		{`{"url":"https://x/a.tgz","version":"-"}`, "-"},
		{`{"url":"","version":"v1"}`, "v1"},
		{`{"version":"v1"}`, "v1"},
		{`{"url":1,"version":"v1"}`, "v1"},
		{`{"url":"https://x/a.tgz","version":1}`, "1"},
		{`{"url":"https://x/a.tgz","version":"v1","extra":true}`, "v1"},
		{`{"url":"https://x/a.tgz","version":"v1"} trailing`, "v1"},
		{`{"url":"https://x/a.tgz","version":"v1","auth_header":"Bearer x\r\nX-Injected: y"}`, "v1"},
		{`{"url":"https://x/a.tgz","version":"v1","auth_header":"x\u007f"}`, "v1"},
		{`{"url":"https://x/a.tgz","version":"v1","auth_header":"Bearer ok"}`, "v1"},
		{`[{"url":"https://x/a.tgz","version":"v1"}]`, "v1"},
		{`{"url":{"url":"https://x/a.tgz"},"version":"v1"}`, "v1"},
		{strings.Repeat("[", 10000), "v1"},
		{``, ""},
		{`null`, "%2e%2e"},
	} {
		f.Add([]byte(s[0]), s[1])
	}
	f.Fuzz(func(t *testing.T, body []byte, queryVersion string) {
		p, status, msg := parseDeployPayload(body)
		if status != 0 {
			if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
				t.Fatalf("parseDeployPayload(%q) refused with %d, want 400 or 422", body, status)
			}
			if msg == "" {
				t.Fatalf("parseDeployPayload(%q) refused with %d and no message", body, status)
			}
			if p != (deployPayload{}) {
				t.Fatalf("parseDeployPayload(%q) refused but returned payload %+v", body, p)
			}
		} else {
			if p.URL == "" {
				t.Fatalf("parseDeployPayload(%q) accepted an empty url", body)
			}
			if strings.ContainsFunc(p.AuthHeader, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
				t.Fatalf("parseDeployPayload(%q) accepted control characters in auth_header %q", body, p.AuthHeader)
			}
			assertVersionIsOneComponent(t, "body", p.Version)
		}
		// deployPush and deployRollback read the version from the query
		// and gate on the same validVersion.
		if validVersion(queryVersion) {
			assertVersionIsOneComponent(t, "query", queryVersion)
		}
	})
}

// assertVersionIsOneComponent is the filesystem property behind
// validVersion: the version must land exactly one level below the
// releases dir, as itself, through the same helper the pipeline uses.
func assertVersionIsOneComponent(t *testing.T, where, v string) {
	t.Helper()
	if !validVersion(v) {
		t.Fatalf("%s version %q accepted but validVersion rejects it", where, v)
	}
	if v == "" || v == "." || v == ".." {
		t.Fatalf("%s version %q accepted", where, v)
	}
	if strings.ContainsAny(v, `/\`+"\x00") {
		t.Fatalf("%s version %q contains a separator or NUL", where, v)
	}
	if got := versionPathComponent(v); got != v {
		t.Fatalf("%s version %q is not its own path component (%q)", where, v, got)
	}
	d := appDirs{releases: "/r"}
	rel := d.release(v)
	if filepath.Dir(rel) != "/r" || filepath.Base(rel) != v {
		t.Fatalf("%s version %q resolves to %q, not directly under the releases dir", where, v, rel)
	}
}
