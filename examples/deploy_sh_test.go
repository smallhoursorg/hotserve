// Package examples holds the tests of what the examples ship that is
// not an example's own code: scripts/deploy.sh, which every example
// copies and which has to read a box's answer in every shape a box
// can give it; and the links in their docs, which have to hold in the
// template repositories each release publishes them as.
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
// front of the box answers: a 2xx that is not a completed deploy.
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
		switch mode {
		case "single":
			single(w, 200, status)
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
	}{
		{"single", 0, `"current_version":"v1"`},
		{"stream", 0, `"http_status":200`},
		{"fail", 1, "health gate: boom"},
		{"cut", 1, `"phase":"downloading"`},
		{"redirect", 1, ""},
		{"truncated", 1, ""},
		{"accepted", 1, ""},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			srv := box(t, tc.mode)
			defer srv.Close()
			cmd := exec.Command("sh", filepath.Join("node", "scripts", "deploy.sh"), "https://example.test/a.tgz")
			cmd.Env = append(os.Environ(), "HOTSERVE_URL="+srv.URL+"/demo", "HOTSERVE_TOKEN=x", "VERSION=v1", "GITHUB_ACTIONS=", "ACTIONS_ID_TOKEN_REQUEST_URL=")
			out, err := cmd.CombinedOutput()
			code := 0
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			} else if err != nil {
				t.Fatalf("running deploy.sh: %v\n%s", err, out)
			}
			if code != tc.exit || !strings.Contains(string(out), tc.want) {
				t.Fatalf("mode %s: exit %d (want %d), output:\n%s", tc.mode, code, tc.exit, out)
			}
		})
	}
}
