package box

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap/zapcore"
)

// What a hostile hotserve uid can put in in/ (DESIGN-box.md, step 9 and
// the Paths notes): every entry leaves in/ in the run that lists it
// (I2); what is not a regular file named `<id>.tar` is removed with an
// error-level line and no result (I3); nothing is followed, waited on or
// left behind; a target outside the tree is never touched.
func TestHostileIn(t *testing.T) {
	requireRoot(t)
	b := newTestBox(t)
	in := b.x("in")
	precious := b.path("precious")
	if err := os.WriteFile(precious, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	link, fifo, dir := randomID(t), randomID(t), randomID(t)
	must(os.Symlink(precious, filepath.Join(in, link+".tar")))
	mkfifo(t, filepath.Join(in, fifo+".tar"))
	must(os.MkdirAll(filepath.Join(in, dir+".tar", "sub", "deeper"), 0o755))
	must(os.WriteFile(filepath.Join(in, dir+".tar", "sub", "f"), []byte("x"), 0o600))
	must(os.Chmod(filepath.Join(in, dir+".tar", "sub"), 0))
	notIDs := []string{"x", strings.ToUpper(randomID(t)) + ".tar", randomID(t) + ".tar.tmp", randomID(t), ".hidden"}
	for _, n := range notIDs {
		must(os.WriteFile(filepath.Join(in, n), tgz(t, b.repo.bundleFiles(b.base, b.base)), 0o644))
	}
	// A real bundle, chmod 000: root reads it all the same.
	c1 := b.repo.commit(boxFile(2, b.alice), &b.alice, b.base)
	good := b.push(b.repo.bundleFiles(c1, b.base))
	must(os.Chmod(filepath.Join(in, good+".tar"), 0))

	if err := b.run(hooks{}); err != nil {
		t.Fatal(err)
	}
	b.settled()
	if r := b.result(good); r == nil || r.Phase != phaseApplied {
		t.Fatalf("the chmod 000 bundle: %+v", r)
	}
	for _, id := range []string{link, fifo, dir} {
		if b.result(id) != nil {
			t.Errorf("%s.tar is not a regular file and got a result", id)
		}
	}
	if entries := b.names(b.x("out")); len(entries) != 1 {
		t.Errorf("out/ holds %v: names that are not ids got results", entries)
	}
	if data, err := os.ReadFile(precious); err != nil || string(data) != "keep" {
		t.Error("the symlink's target was touched")
	}
	if n := len(b.logged(zapcore.ErrorLevel, "not a bundle")); n != 3+len(notIDs) {
		t.Errorf("%d error lines for entries that are not bundles, want %d", n, 3+len(notIDs))
	}
}

// A writer holding a descriptor from before the rename cannot change
// what was verified: the bundle is read once, and nothing is read from
// disk again (step 9).
func TestHostileWriterAfterRead(t *testing.T) {
	b := newTestBox(t)
	v2 := boxFile(2, b.alice)
	c1 := b.repo.commit(v2, &b.alice, b.base)
	id := b.push(b.repo.bundleFiles(c1, b.base))
	f, err := os.OpenFile(filepath.Join(b.x("in"), id+".tar"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	evil := b.repo.commit(boxFile(66, b.alice, b.mallory), &b.mallory, b.base)
	rewrite := func(string) {
		if err := f.Truncate(0); err != nil {
			t.Error(err)
		}
		if _, err := f.WriteAt(tgz(t, b.repo.bundleFiles(evil, b.base)), 0); err != nil {
			t.Error(err)
		}
	}
	if err := b.run(hooks{read: rewrite}); err != nil {
		t.Fatal(err)
	}
	b.settled()
	if r := b.result(id); r == nil || r.Phase != phaseApplied || r.Commit != c1 {
		t.Fatalf("%+v", r)
	}
	if !bytes.Equal(b.installed(), v2) {
		t.Error("installed bytes are not the ones read")
	}
}
