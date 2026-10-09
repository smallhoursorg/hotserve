package proof

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
)

// handMade is a complete, valid bundle built from hand-made objects:
// a commit whose tree holds box1/Caddyfile, one parent, both trees.
type handMade struct {
	file         []byte
	head, parent *Commit
	root, inner  *Tree
	files        map[string][]byte
}

func newHandMade(t testing.TB) handMade {
	t.Helper()
	h := handMade{file: []byte("# the box\n")}
	inner, err := ParseTree(treeObject(Entry{ModeFile, "Caddyfile", ObjectID("blob", h.file)}))
	if err != nil {
		t.Fatal(err)
	}
	root, err := ParseTree(treeObject(Entry{ModeDir, "box1", inner.ID}))
	if err != nil {
		t.Fatal(err)
	}
	parent, err := ParseCommit(commitObject(emptyTree, nil, nil, "base\n"))
	if err != nil {
		t.Fatal(err)
	}
	head, err := ParseCommit(commitObject(root.ID, []string{parent.ID}, nil, "head\n"))
	if err != nil {
		t.Fatal(err)
	}
	h.head, h.parent, h.root, h.inner = head, parent, root, inner
	h.files = map[string][]byte{
		"path":              []byte("box1/Caddyfile"),
		"Caddyfile":         h.file,
		"commit":            head.Raw,
		"parents/0001":      parent.Raw,
		"trees/" + root.ID:  root.Raw,
		"trees/" + inner.ID: inner.Raw,
	}
	return h
}

// closeBoth finishes a tar-in-gzip stream.
func closeBoth(t *testing.T, tw *tar.Writer, zw *gzip.Writer) {
	t.Helper()
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

// moved is the bundle's files with one filed under another name.
func (h handMade) moved(from, to string) map[string][]byte {
	out := h.with(from, nil)
	out[to] = h.files[from]
	return out
}

// with is the bundle's files with one changed, added or (nil) removed.
func (h handMade) with(name string, data []byte) map[string][]byte {
	out := map[string][]byte{}
	for k, v := range h.files {
		out[k] = v
	}
	if data == nil {
		delete(out, name)
	} else {
		out[name] = data
	}
	return out
}

func TestReadBundle(t *testing.T) {
	h := newHandMade(t)
	b, err := ReadBundle(tgz(t, h.files))
	if err != nil {
		t.Fatal(err)
	}
	if b.Path != "box1/Caddyfile" || !bytes.Equal(b.Caddyfile, h.file) || b.Commit.ID != h.head.ID || len(b.Parents) != 1 || b.Parents[h.parent.ID] == nil || len(b.Trees) != 2 || b.Trees[h.root.ID] == nil || b.Trees[h.inner.ID] == nil {
		t.Fatalf("%s", b)
	}
	if err := ProveFile(b.Commit, b.Trees, b.Path, b.Caddyfile); err != nil {
		t.Fatal(err)
	}
	if chain, err := Chain(b.Commit, b.Parents, h.parent.ID); err != nil || len(chain) != 1 {
		t.Fatal(chain, err)
	}
	// One trailing newline on the path is forgiven; two are not.
	if b, err := ReadBundle(tgz(t, h.with("path", []byte("box1/Caddyfile\n")))); err != nil || b.Path != "box1/Caddyfile" {
		t.Fatal(b, err)
	}
	_, err = ReadBundle(tgz(t, h.with("path", []byte("box1/Caddyfile\n\n"))))
	refusalContaining(t, err, "bundle: path is not a relative path of safe components")
	// No parents is a bundle whose HEAD is the baseline.
	if b, err := ReadBundle(tgz(t, h.with("parents/0001", nil))); err != nil || len(b.Parents) != 0 {
		t.Fatal(b, err)
	}

	for name, c := range map[string]struct {
		files map[string][]byte
		want  string
	}{
		"no path":             {h.with("path", nil), "bundle: no path file"},
		"no Caddyfile":        {h.with("Caddyfile", nil), "bundle: no Caddyfile file"},
		"no commit":           {h.with("commit", nil), "bundle: no commit file"},
		"empty Caddyfile":     {h.with("Caddyfile", []byte{}), "bundle: Caddyfile is empty"},
		"bad path":            {h.with("path", []byte("../x")), "bundle: path is not a relative path"},
		"extra file":          {h.with("README", []byte("x")), "bundle: README is not a bundle file"},
		"dot slash":           {h.with("./path", []byte("x")), "bundle: ./path is not a bundle file"},
		"parents one digit":   {h.with("parents/1", h.parent.Raw), "bundle: parents/1 is not a bundle file"},
		"parents five digits": {h.with("parents/00001", h.parent.Raw), "is not a bundle file"},
		"parents letters":     {h.with("parents/abcd", h.parent.Raw), "is not a bundle file"},
		"parents zero":        {h.with("parents/0000", h.parent.Raw), "bundle: parents/0000 is outside parents/0001 to parents/0500"},
		"parents 501":         {h.with("parents/0501", h.parent.Raw), "is outside parents/0001 to parents/0500"},
		"parents gap":         {h.moved("parents/0001", "parents/0003"), "bundle: parents/ is not a sequence from 0001"},
		"parent twice":        {h.with("parents/0002", h.parent.Raw), "bundle: commit " + h.parent.ID + " is bundled twice"},
		"parent malformed":    {h.with("parents/0001", []byte("junk")), "bundle: commit:"},
		"trees short name":    {h.with("trees/abc", h.root.Raw), "bundle: trees/abc is not a bundle file"},
		"trees upper name":    {h.with("trees/"+strings.ToUpper(h.root.ID), h.root.Raw), "is not a bundle file"},
		"tree under wrong id": {h.with("trees/"+h.root.ID, h.inner.Raw), "bundle: trees/" + h.root.ID + " does not hash to its name"},
		"tree malformed":      {h.with("trees/"+emptyTree, []byte("junk")), "bundle: tree"},
		"commit malformed":    {h.with("commit", []byte("junk")), "bundle: commit:"},
		"commit over cap":     {h.with("commit", bytes.Repeat([]byte("x"), MaxCommit+1)), "bundle: commit is larger than its cap of 65536 bytes"},
		"Caddyfile over cap":  {h.with("Caddyfile", bytes.Repeat([]byte("x"), MaxCaddyfile+1)), "bundle: Caddyfile is larger than its cap"},
		"path over cap":       {h.with("path", bytes.Repeat([]byte("x"), MaxPath+2)), "bundle: path is larger than its cap"},
		"tree over cap":       {h.with("trees/"+h.root.ID, bytes.Repeat([]byte("x"), MaxTree+1)), "is larger than its cap"},
		"a bare prefix":       {h.with("trees", []byte{}), "bundle: trees is not a bundle file"},
		"a parents prefix":    {h.with("parents", []byte{}), "bundle: parents is not a bundle file"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ReadBundle(tgz(t, c.files))
			refusalContaining(t, err, c.want)
		})
	}

	t.Run("not gzip", func(t *testing.T) {
		_, err := ReadBundle([]byte("not gzip"))
		refusalContaining(t, err, "bundle: not a gzip stream")
		_, err = ReadBundle(nil)
		refusalContaining(t, err, "bundle: not a gzip stream")
	})
	t.Run("gzip of not tar", func(t *testing.T) {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write([]byte("not a tar stream at all, but long enough to be looked at as one")); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		_, err := ReadBundle(buf.Bytes())
		refusalContaining(t, err, "bundle: not a tar stream")
	})
	t.Run("entries that are not regular files", func(t *testing.T) {
		for _, flag := range []byte{tar.TypeDir, tar.TypeSymlink, tar.TypeLink, tar.TypeFifo} {
			var buf bytes.Buffer
			zw := gzip.NewWriter(&buf)
			tw := tar.NewWriter(zw)
			if err := tw.WriteHeader(&tar.Header{Name: "Caddyfile", Typeflag: flag, Linkname: "/etc/passwd", Mode: 0o644}); err != nil {
				t.Fatal(err)
			}
			closeBoth(t, tw, zw)
			_, err := ReadBundle(buf.Bytes())
			refusalContaining(t, err, "bundle: Caddyfile is not a regular file")
		}
	})
	t.Run("twice", func(t *testing.T) {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(zw)
		for i := 0; i < 2; i++ {
			if err := tw.WriteHeader(&tar.Header{Name: "path", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
		}
		closeBoth(t, tw, zw)
		_, err := ReadBundle(buf.Bytes())
		refusalContaining(t, err, "bundle: path appears twice")
	})
	t.Run("decompressed cap", func(t *testing.T) {
		// Seventeen valid trees of a mebibyte each: every entry is
		// within its own cap, and the stream is not.
		files := h.with("", nil)
		for i := 0; i < 17; i++ {
			var raw bytes.Buffer
			for n := 0; raw.Len() < MaxTree-64; n++ {
				raw.Write(treeObject(Entry{ModeFile, "f" + itoa(i) + "-" + itoa(n), emptyBlob}))
			}
			files["trees/"+ObjectID("tree", raw.Bytes())] = raw.Bytes()
		}
		_, err := ReadBundle(tgz(t, files))
		refusalContaining(t, err, "bundle: larger than 16 MiB")
	})
	t.Run("a name a refusal quotes is bounded", func(t *testing.T) {
		_, err := ReadBundle(tgz(t, h.with("x\ny", []byte("x"))))
		refusalContaining(t, err, `bundle: "x\ny" is not a bundle file`)
	})
}
