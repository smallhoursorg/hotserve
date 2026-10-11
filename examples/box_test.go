package examples

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	hsbox "github.com/smallhoursorg/hotserve/box"
	"github.com/smallhoursorg/hotserve/box/proof"
)

// The box template's scripts — bin/signer, bin/check, bin/apply — in a
// repository made from examples/box, with keys ssh-keygen makes here,
// and bin/apply against a stand-in box that checks every bundle with
// the box's own proof code, as root does.

var (
	buildOnce sync.Once
	builtBin  string
	buildErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if builtBin != "" {
		_ = os.RemoveAll(filepath.Dir(builtBin))
	}
	os.Exit(code)
}

// hotserveBinary is this tree's hotserve, built once: the scripts' `box
// describe`, `box webhook` and `validate` are the code under test.
func hotserveBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "hotserve-bin")
		if err != nil {
			buildErr = err
			return
		}
		builtBin = filepath.Join(dir, "hotserve")
		out, err := exec.Command("go", "build", "-o", builtBin, "github.com/smallhoursorg/hotserve/cmd/hotserve").CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("go build: %w\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return builtBin
}

// boxRepo is a clone of a repository made from the template: its first
// commit is the template as it ships, unsigned, as GitHub makes it.
type boxRepo struct {
	t    *testing.T
	dir  string
	home string
	env  []string
}

func newBoxRepo(t *testing.T) *boxRepo {
	t.Helper()
	for _, tool := range []string{"git", "ssh-keygen", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("the box template's scripts need %s on PATH", tool)
		}
	}
	root := t.TempDir()
	r := &boxRepo{t: t, dir: filepath.Join(root, "repo"), home: filepath.Join(root, "home")}
	if err := os.MkdirAll(filepath.Join(r.home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Nothing of the process's own: no git config, no Actions variables,
	// no curl config, no hotserve settings.
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GIT_") || strings.HasPrefix(k, "GITHUB_") || strings.HasPrefix(k, "ACTIONS_") ||
			strings.HasPrefix(k, "HOTSERVE") || strings.HasPrefix(k, "CURL_") || k == "HOME" || k == "XDG_CONFIG_HOME" {
			continue
		}
		r.env = append(r.env, kv)
	}
	r.env = append(r.env, "HOME="+r.home,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=init.defaultBranch", "GIT_CONFIG_VALUE_0=main",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	err := filepath.WalkDir("box", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel("box", path)
		dst := filepath.Join(r.dir, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
	r.git("init", "-q")
	r.git("config", "user.email", "alice@example.com")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "template")
	return r
}

func (r *boxRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = r.dir, r.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// run runs bin/<script> with env on top of the repository's, and
// returns its combined output and exit status.
func (r *boxRepo) run(script string, env ...string) (string, int) {
	r.t.Helper()
	cmd := exec.Command(filepath.Join(r.dir, "bin", script))
	cmd.Dir, cmd.Env = r.dir, append(append([]string{}, r.env...), env...)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(out), exit.ExitCode()
	}
	if err != nil {
		r.t.Fatalf("bin/%s: %v\n%s", script, err, out)
	}
	return string(out), 0
}

// key makes an ed25519 key in the repository's HOME/.ssh and returns the
// public half's path.
func (r *boxRepo) key(name string) string {
	r.t.Helper()
	priv := filepath.Join(r.home, ".ssh", name)
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", name, "-f", priv).CombinedOutput(); err != nil {
		r.t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	return priv + ".pub"
}

// b64 is the base64 field of a public key file.
func (r *boxRepo) b64(pub string) string {
	r.t.Helper()
	f := strings.Fields(r.read(pub))
	if len(f) < 2 {
		r.t.Fatalf("%s: not a public key", pub)
	}
	return f[1]
}

func (r *boxRepo) read(path string) string {
	r.t.Helper()
	if !filepath.IsAbs(path) {
		path = filepath.Join(r.dir, path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		r.t.Fatal(err)
	}
	return string(b)
}

func (r *boxRepo) write(rel, body string) {
	r.t.Helper()
	path := filepath.Join(r.dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *boxRepo) remove(rel string) {
	r.t.Helper()
	if err := os.RemoveAll(filepath.Join(r.dir, rel)); err != nil {
		r.t.Fatal(err)
	}
}

// commit commits everything, signed as the checkout's config says, and
// returns the commit.
func (r *boxRepo) commit(msg string, extra ...string) string {
	r.t.Helper()
	r.git("add", "-A")
	r.git(append(append([]string{}, extra...), "commit", "-q", "--allow-empty", "-m", msg)...)
	return r.git("rev-parse", "HEAD")
}

const boxFile = "deploy.example.com/Caddyfile"

func TestBoxSigner(t *testing.T) {
	r := newBoxRepo(t)
	if out, code := r.run("signer"); code != 1 || !strings.Contains(out, "no key at "+filepath.Join(r.home, ".ssh", "id_ed25519.pub")) {
		t.Fatalf("no key: exit %d\n%s", code, out)
	}
	alice := r.key("id_ed25519")
	if out, code := r.run("signer", "KEY="+strings.TrimSuffix(alice, ".pub")); code != 1 || !strings.Contains(out, "KEY is the public half") {
		t.Fatalf("a private key as KEY: exit %d\n%s", code, out)
	}
	if out, code := r.run("signer", "PRINCIPAL=alice smith"); code != 1 || !strings.Contains(out, "letters, digits and . _ @ + - only") {
		t.Fatalf("a principal with a space: exit %d\n%s", code, out)
	}
	before := r.read(boxFile)
	// A key whose blob is not a key: refused before any file changes.
	bad := filepath.Join(r.home, ".ssh", "bad.pub")
	if err := os.WriteFile(bad, []byte("ssh-ed25519 AAAAnotakey= bad\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, code := r.run("signer", "KEY="+bad); code != 1 || !strings.Contains(out, "ssh-keygen cannot read it as a public key") || r.read(boxFile) != before {
		t.Fatalf("a malformed key: exit %d\n%s", code, out)
	}
	// The defaults: ~/.ssh/id_ed25519.pub, as the checkout's git email.
	if out, code := r.run("signer"); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	line := "\t\tsigner alice@example.com ssh-ed25519 " + r.b64(alice) + "\n"
	after := r.read(boxFile)
	if want := strings.Replace(before, "\t\t# signers\n", "\t\t# signers\n"+line, 1); after != want {
		t.Fatalf("the Caddyfile after make signer:\n%s\nwant:\n%s", after, want)
	}
	for k, want := range map[string]string{
		"gpg.format":      "ssh",
		"user.signingkey": strings.TrimSuffix(alice, ".pub"), // the private file beside the .pub
		"commit.gpgsign":  "true",
	} {
		if got := r.git("config", "--local", k); got != want {
			t.Errorf("git config %s = %q, want %q", k, got, want)
		}
	}
	// A file with other unstaged edits gains the line but is not staged:
	// the signer's commit carries nothing else.
	r.git("reset", "-q")
	r.write(boxFile, strings.Replace(after, "\t\t# signers\n"+line, "\t\t# signers\n", 1)+"# an edit of mine\n")
	if out, code := r.run("signer"); code != 0 || !strings.Contains(out, "has other edits, or is not committed yet, so it is not staged") {
		t.Fatalf("unstaged edits: exit %d\n%s", code, out)
	}
	if got := r.git("diff", "--cached", "--name-only"); got != "" {
		t.Errorf("staged over unstaged edits: %q", got)
	}
	// Nor over edits already staged.
	r.write(boxFile, strings.Replace(after, "\t\t# signers\n"+line, "\t\t# signers\n", 1)+"# an edit of mine\n")
	r.git("add", boxFile)
	if out, code := r.run("signer"); code != 0 || !strings.Contains(out, "has other edits, or is not committed yet, so it is not staged") {
		t.Fatalf("staged edits: exit %d\n%s", code, out)
	}
	if got := r.git("diff", "--name-only"); got != boxFile {
		t.Errorf("the signer line was staged over staged edits: unstaged %q", got)
	}
	r.git("reset", "-q")
	r.write(boxFile, after)
	r.git("add", boxFile)

	// Again: nothing added, signing still set up.
	if got := r.git("diff", "--cached", "--name-only"); got != boxFile {
		t.Errorf("staged after make signer: %q", got)
	}
	if out, code := r.run("signer"); code != 0 || !strings.Contains(out, "already lists this key, as alice@example.com") || r.read(boxFile) != after {
		t.Fatalf("a second run: exit %d\n%s", code, out)
	}

	// A second box with no marker: refused before either file changes.
	other := strings.Replace(after, "deploy.example.com {", "deploy2.example.com {", 1)
	r.write("deploy2.example.com/Caddyfile", strings.Replace(other, "# signers", "# keys", 1))
	bob := r.key("bob")
	if out, code := r.run("signer", "KEY="+bob, "PRINCIPAL=bob"); code != 1 || !strings.Contains(out, "deploy2.example.com/Caddyfile has no '# signers' line") || r.read(boxFile) != after {
		t.Fatalf("a box with no marker: exit %d\n%s", code, out)
	}
	// With it, every box gains the key; a key given as ~/... is expanded.
	r.write("deploy2.example.com/Caddyfile", other)
	if out, code := r.run("signer", "KEY=~/.ssh/bob.pub", "PRINCIPAL=bob"); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, f := range []string{boxFile, "deploy2.example.com/Caddyfile"} {
		if !strings.Contains(r.read(f), "\t\tsigner bob ssh-ed25519 "+r.b64(bob)+"\n\t\tsigner alice@example.com") {
			t.Errorf("%s lacks bob's line, first under the marker:\n%s", f, r.read(f))
		}
	}
	if got := r.git("config", "--local", "user.signingkey"); got != strings.TrimSuffix(bob, ".pub") {
		t.Errorf("user.signingkey = %q after bob's make signer", got)
	}
}

func TestBoxCheck(t *testing.T) {
	bin := hotserveBinary(t)
	r := newBoxRepo(t)
	check := func() (string, int) { return r.run("check", "HOTSERVE="+bin) }
	// The template as it ships: no signer yet.
	if out, code := check(); code != 1 || !strings.Contains(out, "has no signer (make signer adds yours)") {
		t.Fatalf("the template as it ships: exit %d\n%s", code, out)
	}
	alice := r.key("id_ed25519")
	if out, code := r.run("signer"); code != 0 {
		t.Fatalf("signer: exit %d\n%s", code, out)
	}
	if out, code := check(); code != 0 || out != "deploy.example.com/Caddyfile → https://deploy.example.com/ (1 app, 1 signer)\n" {
		t.Fatalf("the template with a signer: exit %d\n%s", code, out)
	}
	good := r.read(boxFile)

	// The directory is the host.
	r.git("mv", "deploy.example.com", "box1")
	if out, code := check(); code != 1 || !strings.Contains(out, "box1/Caddyfile: its box_webhook host is deploy.example.com, so it belongs at deploy.example.com/Caddyfile (git mv box1 deploy.example.com)") {
		t.Fatalf("a box under another name: exit %d\n%s", code, out)
	}
	r.git("mv", "box1", "deploy.example.com")

	// One level down, nothing shallower or deeper; new files count.
	for _, f := range []string{"Caddyfile", "archive/old.example.com/Caddyfile"} {
		r.write(f, good)
		if out, code := check(); code != 1 || !strings.Contains(out, f+": a box's file is <host>/Caddyfile at the top of the repository") {
			t.Fatalf("%s: exit %d\n%s", f, code, out)
		}
		r.remove(f)
	}

	// Every box lists the same keys; a principal may differ.
	second := strings.Replace(good, "deploy.example.com {", "deploy2.example.com {", 1)
	r.write("deploy2.example.com/Caddyfile", strings.Replace(second, "alice@example.com", "alice", 1))
	if out, code := check(); code != 0 || !strings.Contains(out, "deploy2.example.com/Caddyfile → https://deploy2.example.com/ (1 app, 1 signer)") {
		t.Fatalf("the same key under another name: exit %d\n%s", code, out)
	}
	bob := r.key("bob")
	r.write("deploy2.example.com/Caddyfile", strings.Replace(second, r.b64(alice), r.b64(bob), 1))
	if out, code := check(); code != 1 || !strings.Contains(out, "deploy2.example.com/Caddyfile lists other signer keys than deploy.example.com/Caddyfile") {
		t.Fatalf("another key: exit %d\n%s", code, out)
	}
	r.remove("deploy2.example.com")

	// A symlink: git stores its target's name, which the box refuses.
	r.write("real/Caddyfile", good)
	r.remove(boxFile)
	if err := os.Symlink("../real/Caddyfile", filepath.Join(r.dir, boxFile)); err != nil {
		t.Fatal(err)
	}
	if out, code := check(); code != 1 || !strings.Contains(out, boxFile+": a box's file is a regular file, not a symlink") {
		t.Fatalf("a symlinked file: exit %d\n%s", code, out)
	}
	r.remove("real")
	r.remove(boxFile)
	// Every box deleted here: nothing to check is not a pass.
	if out, code := check(); code != 1 || !strings.Contains(out, "no box here") {
		t.Fatalf("every box deleted: exit %d\n%s", code, out)
	}

	// What the box itself would refuse: validate's.
	r.write(boxFile, strings.Replace(good, "\tliveswap_webhook\n", "\tliveswap_webhook\n\tno_such_directive\n", 1))
	if out, code := check(); code != 1 || !strings.Contains(out, boxFile+" does not validate:") || !strings.Contains(out, "no_such_directive") {
		t.Fatalf("a file validate refuses: exit %d\n%s", code, out)
	}
}

// standIn is one box's box_webhook as bin/apply meets it. Each bundle
// is checked with box/proof — parser, chain, file proof, every
// signature by ssh-keygen against box.Walk's signer lists — what root
// runs, less the install; a bundle that does not parse or prove is the
// script's bug and fails the test.
type standIn struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	baseline  string
	installed []byte
	path      string
	results   map[string][]byte
	ids       map[string]bool
	tokens    map[string]bool // minted here: used yet?
	mints     int
	bundles   int
	onGet     string   // "": the baseline; "401"; "nobaseline"
	onPost    []string // a behaviour per POST, in order: "admitting", "unread", "never", "proxy504", "throttled", "html409", "504", "502", "lost", "sync"; then "": 202
	edited    bool     // results say the file was edited on the box
	throttle  int      // polls answered 429 before the result
	mintFails int      // token requests that fail before one succeeds
	audience  string   // the last token request's audience, decoded
}

var boxID = regexp.MustCompile(`^[0-9a-f]{32}$`)

func newStandIn(t *testing.T, baseline string, installed []byte) *standIn {
	s := &standIn{t: t, baseline: baseline, installed: installed, path: boxFile,
		results: map[string][]byte{}, ids: map[string]bool{}, tokens: map[string]bool{}}
	s.srv = httptest.NewTLSServer(s)
	t.Cleanup(s.srv.Close)
	return s
}

// set changes the stand-in under its lock, and seen copies what it
// observed: the script's requests arrive on the server's goroutines.
func (s *standIn) set(f func(s *standIn)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

type seen struct {
	baseline, audience string
	bundles, mints     int
	unused             []string // tokens minted and never sent
}

func (s *standIn) seen() seen {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := seen{baseline: s.baseline, audience: s.audience, bundles: s.bundles, mints: s.mints}
	for tok, used := range s.tokens {
		if !used {
			v.unused = append(v.unused, tok)
		}
	}
	return v
}

func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// reply answers as the box does (liveswap.RespondJSON): json.Marshal,
// which escapes <, > and & — bin/apply must undo that.
func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(encode(v))
}

func (s *standIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.URL.Path == "/token" {
		if s.mintFails > 0 {
			s.mintFails--
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		s.mints++
		s.audience = r.URL.Query().Get("audience")
		tok := fmt.Sprintf("minted-%d", s.mints)
		s.tokens[tok] = false
		reply(w, 200, map[string]string{"value": tok})
		return
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		s.t.Errorf("%s %s without a token", r.Method, r.URL)
	}
	if used, minted := s.tokens[tok]; minted {
		if used {
			s.t.Errorf("%s %s: token %s used twice", r.Method, r.URL, tok)
		}
		s.tokens[tok] = true
	}
	switch {
	case r.Method == http.MethodGet && r.URL.RawQuery != "":
		id := r.URL.Query().Get("result")
		if !boxID.MatchString(id) || r.URL.RawQuery != "result="+id {
			s.t.Errorf("a poll of %q", r.URL.RawQuery)
		}
		if s.throttle > 0 {
			s.throttle--
			reply(w, 429, map[string]string{"error": "too many failed requests"})
			return
		}
		res, ok := s.results[id]
		if !ok {
			reply(w, 404, map[string]string{"error": "no result and no marker for " + id + ": swept, or never admitted"})
			return
		}
		var ph struct{ Phase string }
		_ = json.Unmarshal(res, &ph)
		code := 422
		if ph.Phase == "applied" || ph.Phase == "no_change" {
			code = 200
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write(res)
	case r.Method == http.MethodGet:
		switch s.onGet {
		case "401":
			reply(w, 401, map[string]string{"error": "invalid or missing deploy token (Authorization: Bearer <jwt>)"})
		case "nobaseline":
			reply(w, 409, map[string]string{"error": "this box has no baseline; hotserve init <sha>, as root inside the box's directory, sets one"})
		default:
			reply(w, 200, map[string]string{"commit": s.baseline, "box_webhook": "deploy.example.com", "sha256": digest(s.installed)})
		}
	case r.Method == http.MethodPost:
		s.post(w, r)
	default:
		s.t.Errorf("%s %s", r.Method, r.URL)
	}
}

func (s *standIn) post(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get("X-Box-Request-Id")
	if !boxID.MatchString(id) || r.Header.Get("Content-Type") != "application/gzip" {
		s.t.Errorf("POST: id %q, Content-Type %q", id, r.Header.Get("Content-Type"))
	}
	// The server's goroutine: a fault is an error and a 500, never
	// FailNow.
	fault := func(format string, args ...any) {
		s.t.Errorf(format, args...)
		reply(w, 500, map[string]string{"error": "the stand-in failed the test"})
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		fault("reading the bundle: %v", err)
		return
	}
	how := ""
	if len(s.onPost) > 0 {
		how, s.onPost = s.onPost[0], s.onPost[1:]
	}
	if how == "admitting" {
		reply(w, 409, map[string]string{"error": "a push is being admitted; retry in a moment"})
		return
	}
	if how == "unread" { // the upload broke: nothing admitted, the id not held
		reply(w, 400, map[string]string{"error": "could not read the bundle: unexpected EOF"})
		return
	}
	if how == "proxy504" { // a proxy in front gave up before the box had it
		http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
		return
	}
	if how == "throttled" {
		reply(w, 429, map[string]string{"error": "too many failed requests"})
		return
	}
	if how == "html409" { // something in front of the box, in its own words
		http.Error(w, "<html>conflict</html>", http.StatusConflict)
		return
	}
	if how == "never" { // the connection broke before the box read a byte
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
		return
	}
	if s.ids[id] {
		reply(w, 409, map[string]string{"error": "duplicate request id: poll /?result=" + id})
		return
	}
	s.ids[id] = true
	b, err := proof.ReadBundle(body)
	if err != nil {
		fault("the bundle: %v", err)
		return
	}
	s.bundles++
	if b.Path != s.path {
		s.t.Errorf("bundle path %q, want %q", b.Path, s.path)
	}
	if err := proof.ProveFile(b.Commit, b.Trees, b.Path, b.Caddyfile); err != nil {
		fault("the file proof: %v", err)
		return
	}
	chain, err := proof.Chain(b.Commit, b.Parents, s.baseline)
	if err != nil {
		fault("the chain: %v", err)
		return
	}
	if b.Commit.ID == s.baseline && bytes.Equal(b.Caddyfile, s.installed) {
		// The fast path writes nothing: no id held, no result to poll.
		delete(s.ids, id)
		if how == "lost" {
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
			return
		}
		reply(w, 200, map[string]string{"id": id, "phase": "no_change", "commit": b.Commit.ID, "path": b.Path})
		return
	}
	cur, err := hsbox.Walk(s.installed)
	if err != nil {
		fault("the installed file: %v", err)
		return
	}
	next, err := hsbox.Walk(b.Caddyfile)
	if err != nil {
		fault("the pushed file: %v", err)
		return
	}
	signer, err := proof.VerifyChain(context.Background(), &proof.Verifier{}, chain, cur.Signers, next.Signers, s.baseline)
	var ref *proof.Refusal
	if errors.As(err, &ref) {
		reply(w, 422, map[string]any{"id": id, "phase": "refused", "commit": b.Commit.ID, "path": b.Path, "caddyfile_edited_out_of_band": false, "error": ref.Msg})
		return
	}
	if err != nil {
		fault("the signatures: %v", err)
		return
	}
	res := map[string]any{"id": id, "phase": "verified", "commit": b.Commit.ID, "path": b.Path, "signer": signer,
		"box_webhook": "deploy.example.com", "caddyfile_edited_out_of_band": s.edited, "apps": next.Apps}
	verified := encode(res)
	res["phase"], res["diff"] = "applied", "--- installed\n+++ pushed\n@@ a hunk @@\n-\told \"line\"\n+\tnew\tline <a & b>\x1b[2J\r\n"
	s.results[id] = encode(res)
	s.baseline, s.installed = b.Commit.ID, b.Caddyfile
	switch how {
	case "504":
		reply(w, 504, map[string]string{"id": id})
	case "502": // a proxy in front of the box gave up on the answer
		http.Error(w, "bad gateway", http.StatusBadGateway)
	case "lost":
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	case "sync":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(s.results[id])
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(202)
		_, _ = w.Write(verified)
	}
}

func encode(v any) []byte {
	b, _ := json.Marshal(v)
	return append(b, '\n')
}

// applyRig is a repository whose box runs its first signed commit — as
// after `hotserve init` — and the stand-ins its boxes are reached at.
type applyRig struct {
	*boxRepo
	bin      string
	base     string
	box      *standIn
	curlHome string
}

func newApplyRig(t *testing.T) *applyRig {
	t.Helper()
	g := &applyRig{boxRepo: newBoxRepo(t), bin: hotserveBinary(t)}
	g.key("id_ed25519")
	if out, code := g.run("signer"); code != 0 {
		t.Fatalf("signer: exit %d\n%s", code, out)
	}
	g.base = g.commit("deploy.example.com: the box's config")
	g.box = newStandIn(t, g.base, []byte(g.read(boxFile)))
	g.curlHome = t.TempDir()
	g.route(map[string]*standIn{"deploy.example.com": g.box})
	return g
}

// route points each host at its stand-in, by curl's own config file:
// bin/apply runs curl unchanged, against the https:// address the
// committed file names.
func (g *applyRig) route(hosts map[string]*standIn) {
	g.t.Helper()
	var rc strings.Builder
	for host, s := range hosts {
		fmt.Fprintf(&rc, "connect-to = %s:443:%s\n", host, s.srv.Listener.Addr().String())
		ca := filepath.Join(g.curlHome, host+".pem")
		if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.srv.Certificate().Raw}), 0o644); err != nil {
			g.t.Fatal(err)
		}
		fmt.Fprintf(&rc, "cacert = %s\n", ca)
	}
	if err := os.WriteFile(filepath.Join(g.curlHome, ".curlrc"), []byte(rc.String()), 0o644); err != nil {
		g.t.Fatal(err)
	}
}

// apply runs bin/apply from a laptop: a fixed token.
func (g *applyRig) apply(env ...string) (string, int) {
	g.t.Helper()
	return g.run("apply", append([]string{"HOTSERVE=" + g.bin, "CURL_HOME=" + g.curlHome, "HOTSERVE_TOKEN=laptop"}, env...)...)
}

// runArgs runs bin/apply from a laptop with arguments.
func (g *applyRig) runArgs(args []string) (string, int) {
	g.t.Helper()
	cmd := exec.Command(filepath.Join(g.dir, "bin", "apply"), args...)
	cmd.Dir = g.dir
	cmd.Env = append(append([]string{}, g.env...), "HOTSERVE="+g.bin, "CURL_HOME="+g.curlHome, "HOTSERVE_TOKEN=laptop")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(out), exit.ExitCode()
	}
	if err != nil {
		g.t.Fatalf("bin/apply: %v\n%s", err, out)
	}
	return string(out), 0
}

// change commits a change to the box's file and returns the commit.
func (g *applyRig) change(msg string, extra ...string) string {
	g.t.Helper()
	g.write(boxFile, strings.Replace(g.read(boxFile), "dynamic liveswap example", "dynamic liveswap example # "+msg, 1))
	return g.commit(msg, extra...)
}

func TestBoxApply(t *testing.T) {
	t.Run("the commit the box runs", func(t *testing.T) {
		g := newApplyRig(t)
		out, code := g.apply()
		if code != 0 || !strings.Contains(out, "deploy.example.com at "+g.base[:7]+": the box already runs this Caddyfile") || g.box.seen().bundles != 1 {
			t.Fatalf("exit %d, %d bundles\n%s", code, g.box.seen().bundles, out)
		}
		// Its 200 lost: the fast path left no result, so the push is sent
		// again and answered the same way. By directory, as an operator
		// might type it.
		g.box.set(func(s *standIn) { s.onPost, s.bundles = []string{"lost"}, 0 })
		if out, code = g.runArgs([]string{"./deploy.example.com/"}); code != 0 || !strings.Contains(out, "the box already runs this Caddyfile") {
			t.Fatalf("a lost no_change: exit %d\n%s", code, out)
		}
		if out, code := g.runArgs([]string{"archive/deploy.example.com"}); code != 1 || !strings.Contains(out, "is not a box: name its directory") {
			t.Fatalf("a nested name: exit %d\n%s", code, out)
		}
	})

	t.Run("a chain applied, then the same again", func(t *testing.T) {
		g := newApplyRig(t)
		g.write("README.md", "a commit that does not touch the box's file\n")
		g.commit("readme")
		head := g.change("one")
		out, code := g.apply()
		for _, want := range []string{
			"the box runs " + g.base[:7] + "; sending " + head[:7] + " (2 commits)",
			"deploy.example.com at " + head[:7] + ", signed by alice@example.com; verified; installing",
			// A control character, and a carriage return, stay escaped.
			"--- installed\n+++ pushed\n@@ a hunk @@\n-\told \"line\"\n+\tnew\tline <a & b>\\u001b[2J\\r\n",
			"deploy.example.com at " + head[:7] + ": loaded; running apps are not restarted; apps example",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("no %q in the output", want)
			}
		}
		if code != 0 || g.box.seen().baseline != head || strings.ContainsAny(out, "\x1b\r") {
			t.Fatalf("exit %d, baseline %s\n%q", code, g.box.seen().baseline, out)
		}
		if out, code := g.apply(); code != 0 || !strings.Contains(out, "the box already runs this Caddyfile") {
			t.Fatalf("again: exit %d\n%s", code, out)
		}
	})

	t.Run("a later run got there first", func(t *testing.T) {
		g := newApplyRig(t)
		g.change("one")
		two := g.change("two")
		g.box.set(func(s *standIn) { s.baseline = two })
		g.git("checkout", "-q", "HEAD~1")
		out, code := g.apply()
		if code != 0 || !strings.Contains(out, "superseded, nothing to send") || g.box.seen().bundles != 0 {
			t.Fatalf("exit %d, %d bundles\n%s", code, g.box.seen().bundles, out)
		}
	})

	t.Run("an unsigned commit under a signed one", func(t *testing.T) {
		g := newApplyRig(t)
		wip := g.change("wip", "-c", "commit.gpgsign=false")
		head := g.change("signed")
		out, code := g.apply()
		if code != 1 || !strings.Contains(out, wip+", between the commit this box runs and "+head+", is not signed") || g.box.seen().baseline != g.base {
			t.Fatalf("exit %d\n%s", code, out)
		}
	})

	t.Run("an answer that comes late, or not at all", func(t *testing.T) {
		for how, said := range map[string]string{
			"504":       "the box has the push; asking for its result",
			"lost":      "no answer from the box to the push; asking for its result",
			"admitting": "a push is being admitted; retry in a moment; trying again",
			"sync":      ": loaded; running apps are not restarted",
			"unread":    "could not read the bundle: unexpected EOF; sending it again",
			"502":       "no answer from the box to the push; asking for its result",
			// The box never saw it: every poll is 404, and the push is
			// sent again under its id.
			"never": "no answer from the box to the push; asking for its result",
			// Sent again, and that upload breaks too: tried once more.
			"never,unread": "no answer from the box to the push; asking for its result",
			// A 504 without the push's id is not the box's: sent again.
			"proxy504": "no answer from the box to the push; asking for its result",
			// Sent again, and throttled: not yet, as a throttled poll is.
			"never,throttled": "no answer from the box to the push; asking for its result",
		} {
			t.Run(how, func(t *testing.T) {
				t.Parallel()
				g := newApplyRig(t)
				head := g.change("one")
				g.box.set(func(s *standIn) { s.onPost, s.edited = strings.Split(how, ","), true })
				out, code := g.apply()
				if code != 0 || g.box.seen().baseline != head || !strings.Contains(out, said) || !strings.Contains(out, ": loaded; running apps are not restarted") {
					t.Fatalf("exit %d\n%s", code, out)
				}
				if !strings.Contains(out, "the Caddyfile on the box had been edited there since the last apply; this push replaced the edit") {
					t.Errorf("no warning for an edit on the box:\n%s", out)
				}
			})
		}
	})

	t.Run("a refusal in something else's words", func(t *testing.T) {
		t.Parallel()
		g := newApplyRig(t)
		g.change("one")
		g.box.set(func(s *standIn) { s.onPost = []string{"html409"} })
		if out, code := g.apply(); code != 1 || !strings.Contains(out, "HTTP 409: <html>conflict</html>") {
			t.Fatalf("exit %d\n%s", code, out)
		}
	})

	t.Run("refused before anything is sent", func(t *testing.T) {
		g := newApplyRig(t)
		g.box.set(func(s *standIn) { s.onGet = "nobaseline" })
		if out, code := g.apply(); code != 1 || !strings.Contains(out, "HTTP 409: this box has no baseline; hotserve init <sha>, as root") {
			t.Fatalf("no baseline: exit %d\n%s", code, out)
		}
		g.box.set(func(s *standIn) { s.onGet = "401" })
		if out, code := g.apply(); code != 1 || !strings.Contains(out, "deploy_trust local { public_key") || strings.Contains(out, "claim repository") {
			t.Fatalf("401 from a laptop: exit %d\n%s", code, out)
		}
		g.box.set(func(s *standIn) { s.onGet = "" })
		// A shallow clone: refused before asking the box anything, never
		// read as a rewritten main.
		shallow := filepath.Join(t.TempDir(), "shallow")
		g.git("commit", "-q", "--allow-empty", "-m", "on top")
		g.git("clone", "-q", "--depth", "1", "file://"+g.dir, shallow)
		if err := os.MkdirAll(filepath.Join(shallow, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		script := filepath.Join(shallow, "bin", "apply")
		if err := os.WriteFile(script, []byte(g.read("bin/apply")), 0o755); err != nil {
			t.Fatal(err)
		}
		inShallow := exec.Command(script)
		inShallow.Dir = shallow
		inShallow.Env = append(append([]string{}, g.env...), "HOTSERVE="+g.bin, "CURL_HOME="+g.curlHome, "HOTSERVE_TOKEN=laptop")
		if out, err := inShallow.CombinedOutput(); err == nil || !strings.Contains(string(out), "this clone is shallow") {
			t.Fatalf("a shallow clone: %v\n%s", err, out)
		}
		g.git("reset", "-q", "--hard", "HEAD~1")
		g.box.set(func(s *standIn) { s.baseline = strings.Repeat("ab", 20) })
		if out, code := g.apply(); code != 1 || !strings.Contains(out, "which this repository's history does not hold") {
			t.Fatalf("a baseline this history lacks: exit %d\n%s", code, out)
		}
		// Past the chain's cap: 501 commits since the one the box runs.
		g.box.set(func(s *standIn) { s.baseline = g.base })
		var fi strings.Builder
		for i := 1; i <= 501; i++ {
			fmt.Fprintf(&fi, "commit refs/heads/main\ncommitter t <t@example.com> %d +0000\ndata 1\nc\n", i)
			if i == 1 {
				fmt.Fprintf(&fi, "from %s\n", g.base)
			}
			fi.WriteString("\n")
		}
		cmd := exec.Command("git", "fast-import", "--quiet")
		cmd.Dir, cmd.Env, cmd.Stdin = g.dir, g.env, strings.NewReader(fi.String())
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fast-import: %v\n%s", err, out)
		}
		if out, code := g.apply(); code != 1 || !strings.Contains(out, "is longer than 500 commits; run hotserve box baseline") {
			t.Fatalf("past the cap: exit %d\n%s", code, out)
		}
		if g.box.seen().bundles != 0 {
			t.Errorf("%d bundles sent", g.box.seen().bundles)
		}
	})

	t.Run("in GitHub Actions", func(t *testing.T) {
		g := newApplyRig(t)
		head := g.change("one")
		summary := filepath.Join(t.TempDir(), "summary")
		actions := []string{
			"GITHUB_ACTIONS=true", "GITHUB_SHA=" + head, "GITHUB_STEP_SUMMARY=" + summary,
			"GITHUB_REPOSITORY=your-org/boxes", "GITHUB_REF=refs/heads/main",
			"ACTIONS_ID_TOKEN_REQUEST_URL=" + g.box.srv.URL + "/token?api-version=2.0", "ACTIONS_ID_TOKEN_REQUEST_TOKEN=req",
			"HOTSERVE_TOKEN=",
			// An audience with characters a query must encode.
			"HOTSERVE_AUDIENCE=a&b c#d",
		}
		g.box.set(func(s *standIn) { s.onPost = []string{"504"} })
		// A mint that fails is tried again; a throttled poll is "not yet".
		g.box.set(func(s *standIn) { s.mintFails, s.throttle = 1, 2 })
		out, code := g.apply(actions...)
		if code != 0 || !strings.Contains(out, "::add-mask::minted-1\n") || g.box.seen().audience != "a&b c#d" || !strings.Contains(out, "::group::apply deploy.example.com\n") {
			t.Fatalf("exit %d\n%s", code, out)
		}
		// A fresh token for each request: the GET, the POST, each poll.
		for _, tok := range g.box.seen().unused {
			t.Errorf("token %s minted and never sent", tok)
		}
		if g.box.seen().mints < 3 {
			t.Errorf("%d tokens for a GET, a POST and a poll", g.box.seen().mints)
		}
		if b, _ := os.ReadFile(summary); !strings.Contains(string(b), "**deploy.example.com:** "+head[:7]+" applied") {
			t.Errorf("the job summary:\n%s", b)
		}
		// A checkout that is not the run's commit: its token would name another.
		if out, code := g.apply(append(actions, "GITHUB_SHA="+g.base)...); code != 1 || !strings.Contains(out, "but this run is for "+g.base) {
			t.Fatalf("another commit: exit %d\n%s", code, out)
		}
		g.box.set(func(s *standIn) { s.onGet = "401" })
		if out, code := g.apply(actions...); code != 1 || !strings.Contains(out, "claim repository  your-org/boxes") || !strings.Contains(out, "::error title=apply deploy.example.com failed::") {
			t.Fatalf("401 in Actions: exit %d\n%s", code, out)
		}
	})

	t.Run("two boxes, one refused", func(t *testing.T) {
		g := newApplyRig(t)
		g.write("deploy2.example.com/Caddyfile", strings.Replace(g.read(boxFile), "deploy.example.com {", "deploy2.example.com {", 1))
		head := g.change("both")
		second := newStandIn(t, head, nil)
		second.set(func(s *standIn) { s.onGet = "nobaseline" })
		g.route(map[string]*standIn{"deploy.example.com": g.box, "deploy2.example.com": second})
		out, code := g.apply()
		if code != 1 || g.box.seen().baseline != head || !strings.Contains(out, "https://deploy2.example.com/: HTTP 409") {
			t.Fatalf("exit %d\n%s", code, out)
		}
	})
}
