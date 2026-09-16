// Package examples holds the tests of what the examples ship that is
// not an example's own code: scripts/deploy.sh, which every example
// copies and which has to read a box's answer in every shape a box
// can give it; and, for the template repositories each release
// publishes them as, the links in their docs and the script that does
// the publishing.
package examples

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A stand-in box. "single" answers the single response a box without
// stream support (before #103) gives; "stream" streams phases and the
// terminal line; "fail" is a single 500; "cut" streams one phase and
// closes; "redirect" is a 3xx, what an intermediary answers;
// "truncated" promises a single 200 body and drops the connection
// before delivering it; "accepted" is a bodiless 202, what a queue in
// front of the box answers: a 2xx that is not a completed deploy;
// "refused" is the box's 401, the same flat sentence whatever the
// reason (liveswap/handler.go pins it); "refused-elsewhere" is a 401
// from something that is not the box. The stand-in also mints the
// run's OIDC token at /token, the way Actions does.
func box(t *testing.T, mode string) *httptest.Server {
	t.Helper()
	status := map[string]any{"app": "demo", "current_version": "v1", "running": true, "last_deploy": map[string]any{"version": "v1", "status": "succeeded"}}
	single := func(w http.ResponseWriter, code int, v any) {
		b, _ := json.Marshal(v)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write(append(b, '\n'))
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			single(w, 200, map[string]any{"value": "minted-" + r.URL.Query().Get("audience")})
			return
		}
		switch mode {
		case "single":
			single(w, 200, status)
		case "refused":
			single(w, 401, map[string]any{"error": "invalid or missing deploy token (Authorization: Bearer <jwt>)"})
		case "refused-elsewhere":
			single(w, 401, map[string]any{"error": "who are you"})
		case "fail":
			single(w, 500, map[string]any{"error": "health gate: boom", "status": status})
		case "accepted":
			w.WriteHeader(http.StatusAccepted)
		case "redirect":
			http.Redirect(w, r, "https://elsewhere.test/demo", http.StatusMovedPermanently)
		case "truncated":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "4096")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"app":"demo","current_vers`))
			w.(http.Flusher).Flush()
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		default:
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(200)
			f := w.(http.Flusher)
			_, _ = w.Write([]byte(`{"at":"t","event":"phase","phase":"downloading"}` + "\n"))
			f.Flush()
			if mode == "cut" {
				return
			}
			b, _ := json.Marshal(status)
			_, _ = w.Write(append(b[:len(b)-1], []byte(`,"event":"done","http_status":200}`+"\n")...))
		}
	}))
}

// The scripts stay byte-identical, and each reads every shape of
// answer to the same exit status.
func TestDeployShReadsEveryShapeOfAnswer(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Fatal("deploy.sh needs curl; install it to run this test")
	}
	node, err := os.ReadFile(filepath.Join("node", "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	deno, err := os.ReadFile(filepath.Join("deno", "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if string(node) != string(deno) {
		t.Fatal("examples/node/scripts/deploy.sh and examples/deno/scripts/deploy.sh differ; they are one script")
	}
	cases := []struct {
		mode string
		exit int
		want string // in the output
		not  string // not in the output
	}{
		{"single", 0, `"current_version":"v1"`, ""},
		{"stream", 0, `"http_status":200`, ""},
		{"fail", 1, "health gate: boom", ""},
		{"cut", 1, `"phase":"downloading"`, ""},
		{"redirect", 1, "", ""},
		{"truncated", 1, "", ""},
		{"accepted", 1, "", ""},
		// A laptop deploy (HOTSERVE_TOKEN) refused: the hint names the
		// local block, not claims the run does not have.
		{"refused", 1, "deploy_trust local", "claim repository"},
		{"refused-elsewhere", 1, "who are you", "deploy_trust"},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			srv := box(t, tc.mode)
			defer srv.Close()
			out, code := deploySh(t, srv, "HOTSERVE_TOKEN=x", "GITHUB_ACTIONS=", "ACTIONS_ID_TOKEN_REQUEST_URL=")
			if code != tc.exit || !strings.Contains(out, tc.want) || (tc.not != "" && strings.Contains(out, tc.not)) {
				t.Fatalf("mode %s: exit %d (want %d), output:\n%s", tc.mode, code, tc.exit, out)
			}
		})
	}
}

// deploySh runs the script against a stand-in box with env on top of
// the process's, and returns its combined output and exit status.
func deploySh(t *testing.T, srv *httptest.Server, env ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("sh", filepath.Join("node", "scripts", "deploy.sh"), "https://example.test/a.tgz")
	// Later entries win, so a developer's own HOTSERVE_AUDIENCE, or the
	// job summary of an Actions run this test runs in, never reaches
	// the script unless a case sets it.
	cmd.Env = append(append(os.Environ(), "HOTSERVE_URL="+srv.URL+"/demo", "VERSION=v1", "HOTSERVE_AUDIENCE=", "GITHUB_STEP_SUMMARY="), env...)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(out), ee.ExitCode()
	}
	if err != nil {
		t.Fatalf("running deploy.sh: %v\n%s", err, out)
	}
	return string(out), 0
}

// In Actions a refused OIDC token is followed by the claims the run
// minted it with — every value the run's own, so the box gives nothing
// away — in the log, the error annotation and the job summary.
func TestDeployShSaysWhatARefusedRunMustBeTrustedFor(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Fatal("deploy.sh needs curl; install it to run this test")
	}
	srv := box(t, "refused")
	defer srv.Close()
	summary := filepath.Join(t.TempDir(), "summary.md")
	out, code := deploySh(t, srv,
		"GITHUB_ACTIONS=true", "GITHUB_REPOSITORY=org/blog", "GITHUB_REF=refs/heads/main",
		"ACTIONS_ID_TOKEN_REQUEST_URL="+srv.URL+"/token?api-version=1", "ACTIONS_ID_TOKEN_REQUEST_TOKEN=run",
		"HOTSERVE_TOKEN=", "GITHUB_STEP_SUMMARY="+summary)
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	for _, want := range []string{
		"::add-mask::minted-hotserve",
		"audience          hotserve",
		"claim repository  org/blog",
		"claim ref         refs/heads/main",
		"journalctl -u hotserve",
		"::error title=hotserve%3A demo v1 failed::invalid or missing deploy token (Authorization: Bearer \\u003cjwt\\u003e); the log says what the box must trust",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	got, err := os.ReadFile(summary)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "claim repository  org/blog") {
		t.Errorf("job summary lacks the checklist:\n%s", got)
	}
}
