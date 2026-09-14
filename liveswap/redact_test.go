package liveswap

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactorKnownValues(t *testing.T) {
	const pw = `p@ss"w\ord-12345`
	r := newRedactor([]string{"DATABASE_URL=postgres://app:" + pw + "@db/app", "SHORT=abc", "TOKEN=" + pw}, nil)
	full := "postgres://app:" + pw + "@db/app"
	for name, in := range map[string]string{
		"as written":   "dial " + full + " failed",
		"json escaped": string(mustJSON(t, "dial "+full+" failed")),
		"base64":       "x " + base64.StdEncoding.EncodeToString([]byte(pw)) + " y",
		"base64 raw":   "x " + base64.RawURLEncoding.EncodeToString([]byte(pw)) + " y",
		"hex":          "x " + hex.EncodeToString([]byte(pw)) + " y",
		"url escaped":  "GET /?p=" + url.QueryEscape(pw) + " " + url.PathEscape(pw),
	} {
		out, keys := r.redact(in)
		if strings.Contains(out, pw) || strings.Contains(out, full) {
			t.Errorf("%s: value survived: %q", name, out)
		}
		if !strings.Contains(out, "[redacted:") {
			t.Errorf("%s: no marker in %q", name, out)
		}
		if len(keys) == 0 {
			t.Errorf("%s: no keys reported", name)
		}
	}
	out, keys := r.redact("SHORT is abc and that is fine")
	if out != "SHORT is abc and that is fine" || len(keys) != 0 {
		t.Errorf("a value under %d chars must not be redacted: %q %v", secretMinLen, out, keys)
	}
	// The longer form (DATABASE_URL) wins over the value it contains
	// (TOKEN), and both keys are reported when both appear.
	out, keys = r.redact(full + " and " + pw)
	if out != "[redacted:DATABASE_URL] and [redacted:TOKEN]" {
		t.Errorf("longest-first: %q", out)
	}
	if strings.Join(keys, ",") != "DATABASE_URL,TOKEN" {
		t.Errorf("keys = %v", keys)
	}
}

func TestRedactorSafeList(t *testing.T) {
	sha := "3f9a1c2b4d5e6f708192a3b4c5d6e7f8091a2b3c" // a 40-char git SHA as a version
	r := newRedactor(nil, []string{sha})
	if out, _ := r.redact("current " + sha + " ok"); !strings.Contains(out, sha) {
		t.Errorf("a safe-listed version was masked: %q", out)
	}
	if out, _ := (*redactor)(nil).redact("current " + sha + " ok"); strings.Contains(out, sha) {
		t.Errorf("without the safe list the same SHA must be masked: %q", out)
	}
	// A dotted version is tokenised the way layer 4 tokenises, so its
	// generated segment is safe wherever it appears on its own.
	dotted := "2026.09.14." + sha
	r = newRedactor(nil, []string{dotted})
	if out, _ := r.redact(`"current_version":"` + dotted + `","unit":"hotserve-x.` + dotted + `.0a1b2c3d0a1b2c3d.service"`); strings.Contains(out, "[masked") {
		t.Errorf("a dotted safe version was masked: %q", out)
	}
	// An env_file value equal to a safe string is not a secret.
	r = newRedactor([]string{"RELEASE=" + sha, "ROOT=/var/lib/liveswap/example"}, []string{sha, "/var/lib/liveswap/example"})
	if out, keys := r.redact(sha + " at /var/lib/liveswap/example/run"); strings.Contains(out, "[redacted") || len(keys) != 0 {
		t.Errorf("safe-listed env_file values were redacted: %q %v", out, keys)
	}
}

func TestRedactorSharedValuesAndMarkers(t *testing.T) {
	// Two keys with one value: both are reported, and the marker names
	// both.
	r := newRedactor([]string{"A=abcdefghij", "B=abcdefghij"}, nil)
	out, keys := r.redact("x abcdefghij y")
	if out != "x [redacted:A,B] y" || strings.Join(keys, ",") != "A,B" {
		t.Errorf("shared value: %q %v", out, keys)
	}
	// A value that is a substring of the usual marker — any key's
	// value, not only the one being replaced — moves every marker to
	// one that does not contain it.
	r = newRedactor([]string{"A=abcdefghij", "SECRET=redacted", "OTHER=REDACTED:OTHER"}, nil)
	for _, in := range []string{"value redacted here", "REDACTED:OTHER", "x abcdefghij y", "abcdefghij redacted REDACTED:OTHER"} {
		out, _ := r.redact(in)
		if strings.Contains(out, "redacted") || strings.Contains(out, "REDACTED:OTHER") || strings.Contains(out, "abcdefghij") {
			t.Errorf("a marker reintroduced a value: %q -> %q", in, out)
		}
	}
}

func TestRedactorBasicCredential(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Authorization: Basic dToxMjM0NTY3OA==", "Authorization: Basic [redacted:basic-credential]"},
		{"Authorization: Basic dToxMjM0NTY3OA", "Authorization: Basic [redacted:basic-credential]"},
		{"Basic authentication is required", "Basic authentication is required"},
		{"basic YWJjZGVmZ2hpams=", "basic YWJjZGVmZ2hpams="}, // decodes, but to no user:pass
	} {
		if got, _ := (*redactor)(nil).redact(tc.in); got != tc.want {
			t.Errorf("%q:\n got %q\nwant %q", tc.in, got, tc.want)
		}
	}
}

func TestRedactJSONWithholdsAnUnparsableBody(t *testing.T) {
	// A value made of JSON's own punctuation can match the body's
	// structure rather than a string inside it. The body is withheld,
	// the keys still reported, and the result is always valid JSON.
	r := newRedactor([]string{`ALLOWED=["a","b"]`}, nil)
	out := r.redactJSON([]byte(`{"app":"x","available_versions":["a","b"]}`))
	if !json.Valid([]byte(out)) {
		t.Fatalf("not JSON: %s", out)
	}
	if !strings.Contains(out, "withheld") || !strings.Contains(out, `"redacted_env":["ALLOWED"]`) {
		t.Errorf("withheld body = %s", out)
	}
	// An array body has nowhere to put redacted_env and stays an array.
	if out := r.redactJSON([]byte(`["x"]`)); out != `["x"]` {
		t.Errorf("array body = %s", out)
	}
}

func TestRedactorShapes(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"jwt", "token eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJyZXBvIn0.c2lnbmF0dXJlX2hlcmVfMTIz here", "token [redacted:jwt] here"}, // gitleaks:allow
		{"bearer", "Authorization: Bearer AbCdEfGhIjKlMnOpQrStUvWxYz0123456789", "Authorization: Bearer [redacted:token]"},
		{"401 placeholder untouched", "invalid or missing deploy token (Authorization: Bearer <jwt>)", "invalid or missing deploy token (Authorization: Bearer <jwt>)"},
		{"userinfo", "dial postgres://app:hunter2@db:5432/app", "dial postgres://[redacted:userinfo]@db:5432/app"},
		{"query", `fetch https://s3.example/a.tgz?X-Amz-Signature=abc&x=1 failed`, "fetch https://s3.example/a.tgz?[redacted:query] failed"},
		{"query stops at a quote", `{"url":"https://s3.example/a.tgz?sig=1","v":"x"}`, `{"url":"https://s3.example/a.tgz?[redacted:query]","v":"x"}`},
		{"aws", "key AKIAIOSFODNN7EXAMPLE used", "key [redacted:aws-key] used"},
		{"github", "ghp_" + strings.Repeat("a1B2", 9) + " leaked", "[redacted:github-token] leaked"},
		{"slack", "xoxb-123456789012-abcdefghijkl", "[redacted:slack-token]"},
		{"stripe", "sk_live_" + strings.Repeat("Ab1", 6), "[redacted:stripe-key]"},
		{"pem", "-----BEGIN PRIVATE KEY-----\nMIIE...\n-----END PRIVATE KEY-----", "[redacted:private-key]"},
		{"assignment", "PASSWORD=correct-horse-battery next", "PASSWORD=[redacted:credential] next"},
		{"assignment colon", "api_key: 0123456789abcdef", "api_key: [redacted:credential]"}, // gitleaks:allow
		{"prefixed key", "DB_PASSWORD=correct-horse-battery", "DB_PASSWORD=[redacted:credential]"},
		{"aws secret key", "AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "AWS_SECRET_ACCESS_KEY=[redacted:credential]"},
		{"json quoted", `{"password":"correct-horse-battery","x":1}`, `{"password":"[redacted:credential]","x":1}`},
		{"a path is not a credential", "--api-key=/etc/hotserve/app.key", "--api-key=/etc/hotserve/app.key"},
		{"a placeholder is not a credential", "api_key={shared_dir}/key", "api_key={shared_dir}/key"},
		{"a marker is not re-redacted", "API_KEY=[redacted:API_KEY] next", "API_KEY=[redacted:API_KEY] next"},
		{"prose is not an assignment", "secrets: the examples do not", "secrets: the examples do not"},
		{"short assignment untouched", "password=short", "password=short"},
	} {
		if got, _ := (*redactor)(nil).redact(tc.in); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

func TestRedactorEntropy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		in     string
		masked bool
	}{
		{"random base64", "k3J9xQ2mZ8pL0vT7wR4nY6bH1cD5fG", true},
		{"sha256 hex", "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", true},
		{"identifier", "not_healthy_within_deadline_reached", false},
		{"upper identifier", "SOME_VERY_LONG_ENV_KEY_NAME_HERE", false},
		{"path", "/var/lib/liveswap/example/releases/dx1/server", false},
		{"unit name", "hotserve-example.dx1.0a1b2c3d0a1b2c3d.service", false},
		{"timestamp", "2026-09-14T07:15:33.123456789Z", false},
		{"deploy subject", "repo:smallhoursorg/example:ref:refs/heads/main", false},
		{"commit version", "0a1b2c3d4e5f", false},
		{"lowercase plus digits only", "abcdefghijklmnopqrst1234567890", false},
	} {
		out, _ := (*redactor)(nil).redact("x " + tc.in + " y")
		if masked := strings.Contains(out, "[masked, "); masked != tc.masked {
			t.Errorf("%s: masked=%v, want %v: %q", tc.name, masked, tc.masked, out)
		}
		if !tc.masked && !strings.Contains(out, tc.in) {
			t.Errorf("%s: changed: %q", tc.name, out)
		}
	}
}

// The corpus: a status body and a failure body in the shapes the
// webhook produces today, and app output in the shape a later change
// will carry in responses, each with what must survive (.keep) and
// what must not (.gone), so the heuristics can never drift toward
// blanking what a reader needs.
func TestRedactorCorpus(t *testing.T) {
	files, err := filepath.Glob("testdata/redact/*.txt")
	if err != nil || len(files) == 0 {
		t.Fatalf("no corpus: %v", err)
	}
	r := newRedactor([]string{"DATABASE_URL=postgres://app:s3cr3t-pw-value@db/app", "API_KEY=zK9dQ2mX8pL0vT7wR4nY6bH1"}, []string{"dx1", "dx2", "3f9a1c2b4d5e6f708192a3b4c5d6e7f8091a2b3c"}) // gitleaks:allow
	for _, f := range files {
		in, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := r.redact(string(in))
		base := strings.TrimSuffix(f, ".txt")
		for _, want := range lines(t, base+".keep") {
			if !strings.Contains(out, want) {
				t.Errorf("%s: %q did not survive:\n%s", f, want, out)
			}
		}
		for _, gone := range lines(t, base+".gone") {
			if strings.Contains(out, gone) {
				t.Errorf("%s: %q leaked:\n%s", f, gone, out)
			}
		}
	}
}

func lines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

func TestRespondJSONRedacts(t *testing.T) {
	r := newRedactor([]string{"SECRET=hunter2hunter2"}, nil)
	rec := httptest.NewRecorder()
	body := map[string]any{"error": "start failed: hunter2hunter2 rejected", "status": map[string]any{"app": "x"}}
	if err := respondJSON(rec, http.StatusInternalServerError, body, r); err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v: %s", err, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "hunter2hunter2") {
		t.Errorf("value in body: %s", rec.Body.String())
	}
	if string(got["redacted_env"]) != `["SECRET"]` {
		t.Errorf("redacted_env = %s", got["redacted_env"])
	}
	if string(got["error"]) != `"start failed: [redacted:SECRET] rejected"` {
		t.Errorf("error = %s", got["error"])
	}
	if rec.Code != http.StatusInternalServerError || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("code %d, type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	// Nothing to redact: the body is the encoder's, byte for byte, with
	// no redacted_env field.
	rec = httptest.NewRecorder()
	if err := respondJSON(rec, http.StatusOK, map[string]string{"error": "plain"}, r); err != nil {
		t.Fatal(err)
	}
	if rec.Body.String() != "{\"error\":\"plain\"}\n" {
		t.Errorf("untouched body = %q", rec.Body.String())
	}
}

func mustJSON(t *testing.T, s string) []byte {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return b[1 : len(b)-1]
}
