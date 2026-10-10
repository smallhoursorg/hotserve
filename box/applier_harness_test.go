package box

// The applier's test harness: a box tree under t.TempDir(), a
// repository built in Go — ed25519 keys, SSHSIG signatures, commit and
// tree objects as git lays them out — verified by the real ssh-keygen
// (a missing tool fails, as the proof's tests do), a fake service
// manager and clock, and bundles dropped exactly as admission will drop
// them: the tarball renamed into in/, then the marker renamed into
// stage/.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/crypto/ssh"

	"github.com/smallhoursorg/hotserve/box/proof"
)

const testPath = "box1/Caddyfile"

// testKey is one signer's key pair.
type testKey struct {
	principal string
	priv      ed25519.PrivateKey
	b64       string
}

func (k testKey) line() string { return "signer " + k.principal + " ssh-ed25519 " + k.b64 }

// sshsig is an armored SSH signature over msg in namespace, as
// `ssh-keygen -Y sign` makes one (PROTOCOL.sshsig).
func sshsig(t testing.TB, k testKey, namespace string, msg []byte) string {
	t.Helper()
	signer, err := ssh.NewSignerFromKey(k.priv)
	if err != nil {
		t.Fatal(err)
	}
	h := sha512.Sum512(msg)
	toSign := append([]byte("SSHSIG"), ssh.Marshal(struct {
		Namespace, Reserved, Hash string
		H                         []byte
	}{namespace, "", "sha512", h[:]})...)
	sig, err := signer.Sign(rand.Reader, toSign)
	if err != nil {
		t.Fatal(err)
	}
	blob := append([]byte("SSHSIG"), ssh.Marshal(struct {
		Version                   uint32
		PublicKey                 []byte
		Namespace, Reserved, Hash string
		Signature                 []byte
	}{1, signer.PublicKey().Marshal(), namespace, "", "sha512", ssh.Marshal(sig)})...)
	b64 := base64.StdEncoding.EncodeToString(blob)
	var out strings.Builder
	out.WriteString("-----BEGIN SSH SIGNATURE-----\n")
	for len(b64) > 70 {
		out.WriteString(b64[:70] + "\n")
		b64 = b64[70:]
	}
	out.WriteString(b64 + "\n-----END SSH SIGNATURE-----\n")
	return out.String()
}

// repo is the objects of a box repository: one file at testPath per
// commit, the trees on its path, the commits.
type repo struct {
	t       testing.TB
	objects map[string][]byte // id → raw, by kind below
	files   map[string][]byte // commit id → the file at testPath
	trees   map[string][2]string
	when    int
}

func newRepo(t testing.TB) *repo {
	return &repo{t: t, objects: map[string][]byte{}, files: map[string][]byte{}, trees: map[string][2]string{}}
}

func treeObject(mode, name, id string) []byte {
	raw, _ := hex.DecodeString(id)
	return append([]byte(mode+" "+name+"\x00"), raw...)
}

// commit makes a commit of file at testPath with the given first
// parent ("" for a root), signed by key unless key is nil.
func (r *repo) commit(file []byte, key *testKey, parents ...string) string {
	blob := proof.ObjectID("blob", file)
	sub := treeObject("100644", "Caddyfile", blob)
	subID := proof.ObjectID("tree", sub)
	root := treeObject("40000", "box1", subID)
	rootID := proof.ObjectID("tree", root)
	r.objects[subID], r.objects[rootID] = sub, root
	r.when++
	var hb strings.Builder
	hb.WriteString("tree " + rootID + "\n")
	for _, p := range parents {
		hb.WriteString("parent " + p + "\n")
	}
	stamp := fmt.Sprintf("%d +0000", 1791000000+r.when)
	hb.WriteString("author Alice <alice@example.com> " + stamp + "\n")
	hb.WriteString("committer Alice <alice@example.com> " + stamp + "\n")
	msg := fmt.Sprintf("change %d\n", r.when)
	raw := []byte(hb.String() + "\n" + msg)
	if key != nil {
		sig := sshsig(r.t, *key, "git", raw)
		lines := strings.Split(strings.TrimSuffix(sig, "\n"), "\n")
		raw = []byte(hb.String() + "gpgsig " + strings.Join(lines, "\n ") + "\n\n" + msg)
	}
	id := proof.ObjectID("commit", raw)
	r.objects[id] = raw
	r.files[id] = file
	r.trees[id] = [2]string{rootID, subID}
	return id
}

// firstParent is the commit's first parent, from its raw headers.
func (r *repo) firstParent(id string) string {
	c, err := proof.ParseCommit(r.objects[id])
	if err != nil {
		r.t.Fatal(err)
	}
	if len(c.Parents) == 0 {
		return ""
	}
	return c.Parents[0]
}

// files of a bundle for head over baseline; edit may change any file
// before it is packed.
func (r *repo) bundleFiles(head, baseline string) map[string][]byte {
	files := map[string][]byte{
		"path":      []byte(testPath + "\n"),
		"Caddyfile": r.files[head],
		"commit":    r.objects[head],
	}
	tr := r.trees[head]
	files["trees/"+tr[0]], files["trees/"+tr[1]] = r.objects[tr[0]], r.objects[tr[1]]
	n := 0
	for c := r.firstParent(head); c != "" && c != baseline; c = r.firstParent(c) {
		n++
		files[fmt.Sprintf("parents/%04d", n)] = r.objects[c]
	}
	return files
}

func tgz(t testing.TB, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(files[n]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// boxFile is a box's Caddyfile listing signers, with the version in
// a comment and, optionally, a second app — so that two versions differ
// in a line, as a real change does.
func boxFile(version int, signers ...testKey) []byte {
	var lines []string
	for _, k := range signers {
		lines = append(lines, "\t\t"+k.line())
	}
	return []byte(fmt.Sprintf(`{
	admin unix//run/hotserve/admin.sock

	box {
		deploy_trust github {
			audience hotserve
			claim repository your-org/boxes
		}
%s
	}

	liveswap {
		app example {
			command node server.js
		}
	}
}

# version %d

example.com {
	reverse_proxy {
		dynamic liveswap example
	}
}

deploy.example.com {
	liveswap_webhook
	box_webhook
}
`, strings.Join(lines, "\n"), version))
}

// fakeSystemd answers is-active from a script and counts reloads.
type fakeSystemd struct {
	mu       sync.Mutex
	states   []string // successive is-active answers; the last repeats
	stateErr error
	reloads  []error // successive reload results; past the end, nil
	reloaded int
	asked    int
	// onReload runs at each reload, once counted, before it returns:
	// a test panics there for a crash after the reload took effect.
	onReload func(n int)
}

func (f *fakeSystemd) IsActive(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked++
	if f.stateErr != nil {
		return "", f.stateErr
	}
	if len(f.states) == 0 {
		return "active", nil
	}
	s := f.states[0]
	if len(f.states) > 1 {
		f.states = f.states[1:]
	}
	return s, nil
}

func (f *fakeSystemd) Reload(context.Context) error {
	f.mu.Lock()
	n := f.reloaded
	f.reloaded++
	var err error
	if n < len(f.reloads) {
		err = f.reloads[n]
	}
	hook := f.onReload
	f.mu.Unlock()
	if hook != nil {
		hook(n + 1)
	}
	return err
}

// fakeClock is time that moves only when told, or slept on.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	c.advance(d)
	return nil
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// testBox is one box: its tree, its repository, its fakes.
type testBox struct {
	t         testing.TB
	root      string
	repo      *repo
	alice     testKey
	bob       testKey
	mallory   testKey
	base      string // the commit applied.json names at the start
	v1        []byte // the installed file at the start
	sd        *fakeSystemd
	clock     *fakeClock
	core      zapcore.Core
	logs      *observer.ObservedLogs
	verifier  *proof.Verifier
	lastPoint []string // the hook points of the last run, in order
}

// processKeys are alice, bob and mallory for every box of the process:
// ed25519 signatures are deterministic, so every box's baseline commit
// is the same object and a bundle built in one box (a fuzz seed)
// verifies in the next.
var processKeys = sync.OnceValue(func() [3]testKey {
	return [3]testKey{mustKey("alice@example.com"), mustKey("bob"), mustKey("mallory")}
})

func mustKey(principal string) testKey {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		panic(err)
	}
	return testKey{principal: principal, priv: priv, b64: base64.StdEncoding.EncodeToString(k.Marshal())}
}

var needSSHKeygen = sync.OnceValue(func() error {
	_, err := exec.LookPath("ssh-keygen")
	return err
})

func newTestBox(t testing.TB) *testBox {
	t.Helper()
	if err := needSSHKeygen(); err != nil {
		t.Fatalf("ssh-keygen is needed to verify the fixtures' signatures: %v", err)
	}
	b := &testBox{
		t: t, root: t.TempDir(), repo: newRepo(t),
		alice: processKeys()[0], bob: processKeys()[1], mallory: processKeys()[2],
		sd: &fakeSystemd{},
		// The clock starts at the wall clock, so files the run writes have
		// modification times Retention ages sensibly.
		clock:    &fakeClock{now: time.Now().Truncate(time.Second)},
		verifier: &proof.Verifier{},
	}
	b.core, b.logs = observer.New(zapcore.DebugLevel)
	for _, d := range []string{"etc/hotserve", "var/lib/hotserve-box/in", "var/lib/hotserve-box/work", "var/lib/hotserve-box/out", "var/lib/hotserve-box/stage"} {
		if err := os.MkdirAll(filepath.Join(b.root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	b.v1 = boxFile(1, b.alice)
	b.base = b.repo.commit(b.v1, &b.alice)
	b.writeInstalled(b.v1)
	b.writeApplied(applied{SHA: b.base, Path: testPath, SHA256: digest(b.v1), Signer: b.alice.principal, When: b.clock.Now()})
	return b
}

func (b *testBox) applier(h hooks) *Applier {
	return &Applier{
		root: b.root, systemd: b.sd, clock: b.clock, verifier: b.verifier,
		logger: zap.New(b.core), hooks: h,
	}
}

// requireRoot skips a test of what the applier does with root's
// CAP_DAC_OVERRIDE and CAP_FOWNER (a chmod 000 entry); the dev
// container, where `make test` runs, is root.
func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root, as the applier runs: the dev container is")
	}
}

// crashed is the panic a crash hook raises.
type crashed struct{ point string }

// run is one applier run with the given hooks; points records every
// hook point reached.
func (b *testBox) run(h hooks) error {
	b.t.Helper()
	b.lastPoint = nil
	inner := h.crash
	h.crash = func(p string) {
		b.lastPoint = append(b.lastPoint, p)
		if inner != nil {
			inner(p)
		}
	}
	return b.applier(h).Run(context.Background())
}

// runCrash runs with h and expects a crash: it returns the point.
func (b *testBox) runCrash(h hooks) string {
	b.t.Helper()
	var point string
	func() {
		defer func() {
			if r := recover(); r != nil {
				c, ok := r.(crashed)
				if !ok {
					panic(r)
				}
				point = c.point
			}
		}()
		_ = b.run(h)
	}()
	if point == "" {
		b.t.Fatal("expected a crash, the run finished")
	}
	return point
}

// crashAfter panics at the nth arrival (1-based) at point.
func crashAfter(point string, n int) func(string) {
	seen := 0
	return func(p string) {
		if p == point {
			seen++
			if seen == n {
				panic(crashed{point: p})
			}
		}
	}
}

// errNoSpace stands for ENOSPC.
var errNoSpace = errors.New("no space left on device")

// failAt fails point times times (−1: always).
func failAt(points map[string]int) func(string) error {
	left := map[string]int{}
	for p, n := range points {
		left[p] = n
	}
	return func(p string) error {
		n, ok := left[p]
		if !ok || n == 0 {
			return nil
		}
		if n > 0 {
			left[p] = n - 1
		}
		return fmt.Errorf("%s: %w", p, errNoSpace)
	}
}

func (b *testBox) path(rel string) string { return filepath.Join(b.root, rel) }
func (b *testBox) x(name string) string   { return filepath.Join(b.root, exchangeDir, name) }

func (b *testBox) writeInstalled(file []byte) {
	b.t.Helper()
	if err := os.WriteFile(b.path(installedFile), file, 0o644); err != nil {
		b.t.Fatal(err)
	}
}

func (b *testBox) installed() []byte {
	b.t.Helper()
	data, err := os.ReadFile(b.path(installedFile))
	if err != nil {
		b.t.Fatal(err)
	}
	return data
}

func (b *testBox) writeApplied(a applied) {
	b.t.Helper()
	data, _ := json.Marshal(a)
	if err := os.WriteFile(b.x("applied.json"), data, 0o640); err != nil {
		b.t.Fatal(err)
	}
}

func (b *testBox) applied() applied {
	b.t.Helper()
	a, err := readApplied(b.x(""))
	if err != nil {
		b.t.Fatal(err)
	}
	return *a
}

// push drops the bundle files as admission will: the tarball written in
// stage/ and renamed into in/, then the marker written beside it and
// renamed to stage/<id>.auth, posted now. It returns the id.
func (b *testBox) push(files map[string][]byte) string {
	b.t.Helper()
	return b.pushAt(files, b.clock.Now())
}

func (b *testBox) pushAt(files map[string][]byte, posted time.Time) string {
	b.t.Helper()
	return b.drop(tgz(b.t, files), posted)
}

func (b *testBox) drop(body []byte, posted time.Time) string {
	b.t.Helper()
	id := randomID(b.t)
	tmp := filepath.Join(b.x("stage"), "push-"+id+".tar")
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		b.t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(b.x("in"), id+".tar")); err != nil {
		b.t.Fatal(err)
	}
	b.writeMarker(id, marker{Posted: posted})
	return id
}

func (b *testBox) writeMarker(id string, m marker) {
	b.t.Helper()
	data, _ := json.Marshal(m)
	tmp := filepath.Join(b.x("stage"), "marker-"+id)
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		b.t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(b.x("stage"), id+".auth")); err != nil {
		b.t.Fatal(err)
	}
}

// result is out/<id>.json, or nil.
func (b *testBox) result(id string) *result {
	b.t.Helper()
	data, err := os.ReadFile(filepath.Join(b.x("out"), id+".json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		b.t.Fatal(err)
	}
	var r result
	if err := json.Unmarshal(data, &r); err != nil {
		b.t.Fatal(err)
	}
	return &r
}

// record is txn.json, or nil.
func (b *testBox) record() *record {
	b.t.Helper()
	data, err := os.ReadFile(b.x("txn.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		b.t.Fatal(err)
	}
	var r record
	if err := json.Unmarshal(data, &r); err != nil {
		b.t.Fatal(err)
	}
	return &r
}

func (b *testBox) names(dir string) []string {
	b.t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		b.t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// settled asserts what every settled run leaves: no record, in/ and
// work/ empty (I2, I5), and no temporary of the applier's anywhere.
func (b *testBox) settled() {
	b.t.Helper()
	if r := b.record(); r != nil {
		b.t.Errorf("txn.json left behind: phase %s", r.Phase)
	}
	if n := b.names(b.x("in")); len(n) != 0 {
		b.t.Errorf("in/ not empty: %v", n)
	}
	if n := b.names(b.x("work")); len(n) != 0 {
		b.t.Errorf("work/ not empty: %v", n)
	}
	for _, n := range b.names(b.path("etc/hotserve")) {
		if n != "Caddyfile" {
			b.t.Errorf("/etc/hotserve holds %s", n)
		}
	}
}

// errorLogged reports whether an error-level line containing msg was
// written.
func (b *testBox) errorLogged(msg string) bool {
	for _, e := range b.logs.All() {
		if e.Level >= zapcore.ErrorLevel && strings.Contains(e.Message, msg) {
			return true
		}
	}
	return false
}

func (b *testBox) logged(level zapcore.Level, msg string) []observer.LoggedEntry {
	var out []observer.LoggedEntry
	for _, e := range b.logs.All() {
		if e.Level == level && strings.Contains(e.Message, msg) {
			out = append(out, e)
		}
	}
	return out
}
