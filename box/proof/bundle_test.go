package proof

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"strconv"
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
	// base is the baseline: not bundled, named by parent's header.
	base, err := ParseCommit(commitObject(emptyTree, nil, nil, "base\n"))
	if err != nil {
		t.Fatal(err)
	}
	parent, err := ParseCommit(commitObject(emptyTree, []string{base.ID}, nil, "parent\n"))
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
	if b.Path != "box1/Caddyfile" || !bytes.Equal(b.Caddyfile, h.file) || b.Commit.ID != h.head.ID || len(b.Parents) != 1 || b.Parents[0].ID != h.parent.ID || len(b.Trees) != 2 || b.Trees[h.root.ID] == nil || b.Trees[h.inner.ID] == nil {
		t.Fatalf("%s", b)
	}
	if err := ProveFile(b.Commit, b.Trees, b.Path, b.Caddyfile); err != nil {
		t.Fatal(err)
	}
	baseline := h.parent.Parents[0]
	if chain, err := Chain(b.Commit, b.Parents, baseline); err != nil || len(chain) != 2 {
		t.Fatal(chain, err)
	}
	// The same object twice reads as two parents; the chain walk is
	// what refuses it, as an entry past the end.
	b, err = ReadBundle(tgz(t, h.with("parents/0002", h.parent.Raw)))
	if err != nil || len(b.Parents) != 2 {
		t.Fatal(b, err)
	}
	_, err = Chain(b.Commit, b.Parents, baseline)
	refusalContaining(t, err, "bundle: parents/0002 is past the end of the chain")
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
		"parents zero":        {h.with("parents/0000", h.parent.Raw), "bundle: parents/0000 is outside parents/0001 to parents/0499"},
		"parents 500":         {h.with("parents/0500", h.parent.Raw), "is outside parents/0001 to parents/0499"},
		"parents gap":         {h.moved("parents/0001", "parents/0003"), "bundle: parents/ is not a sequence from 0001"},
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
	t.Run("tree count cap", func(t *testing.T) {
		files := h.with("", nil) // the two real trees, plus 31 tiny ones
		for i := 0; i < 31; i++ {
			raw := treeObject(Entry{ModeFile, "f" + strconv.Itoa(i), emptyBlob})
			files["trees/"+ObjectID("tree", raw)] = raw
		}
		_, err := ReadBundle(tgz(t, files))
		refusalContaining(t, err, "bundle: more than 32 trees")
	})
	t.Run("decompressed cap", func(t *testing.T) {
		// Seventeen valid trees of a mebibyte each: every entry is
		// within its own cap, and the stream is not. (Seventeen is under
		// the tree-count cap, so the stream's is the bound that fires.)
		files := h.with("", nil)
		for i := 0; i < 17; i++ {
			var raw bytes.Buffer
			for n := 0; raw.Len() < MaxTree-64; n++ {
				raw.Write(treeObject(Entry{ModeFile, fmt.Sprintf("f%02d-%06d", i, n), emptyBlob})) // zero-padded: git's order
			}
			files["trees/"+ObjectID("tree", raw.Bytes())] = raw.Bytes()
		}
		_, err := ReadBundle(tgz(t, files))
		refusalContaining(t, err, "bundle: larger than 16 MiB")
	})
	t.Run("the stream past the tar's end is read against the cap", func(t *testing.T) {
		// A small valid tar, then a second gzip member of 17 MiB: the
		// tar reader stops at the end markers, the drain does not.
		var tail bytes.Buffer
		zw := gzip.NewWriter(&tail)
		if _, err := zw.Write(bytes.Repeat([]byte{0}, 17<<20)); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		_, err := ReadBundle(append(tgz(t, h.files), tail.Bytes()...))
		refusalContaining(t, err, "bundle: larger than 16 MiB")
		// Padding after the end markers, as tar writers add, is fine.
		var padded bytes.Buffer
		zw = gzip.NewWriter(&padded)
		if _, err := zw.Write(bytes.Repeat([]byte{0}, 10240)); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadBundle(append(tgz(t, h.files), padded.Bytes()...)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a corrupt gzip trailer is seen", func(t *testing.T) {
		data := tgz(t, h.files)
		data[len(data)-1] ^= 0xff // the last byte of the size trailer
		_, err := ReadBundle(data)
		refusalContaining(t, err, "bundle: not a gzip stream")
		_, err = ReadBundle(data[:len(data)-4])
		refusalContaining(t, err, "bundle: not a gzip stream")
	})
	t.Run("corruption inside the deflate stream is named as gzip's", func(t *testing.T) {
		// Every byte of the compressed body flipped in turn: whatever
		// surfaces — a deflate error mid-archive, a checksum at the end,
		// or junk the tar reader refuses — the refusal names the layer
		// that saw it, and nothing panics or is accepted.
		base := tgz(t, h.files)
		inflate := func(data []byte) []byte {
			zr, err := gzip.NewReader(bytes.NewReader(data))
			if err != nil {
				return nil
			}
			out, err := io.ReadAll(zr)
			if err != nil {
				return nil
			}
			return out
		}
		want := inflate(base)
		gzipSaid, tarSaid := 0, 0
		for i := 10; i < len(base)-8; i++ { // past the gzip header, before the trailer
			data := append([]byte{}, base...)
			data[i] ^= 0x55
			_, err := ReadBundle(data)
			if err == nil {
				// A flip in a deflate block's padding bits changes no
				// output byte; that bundle is the same bundle, and the
				// checksum agrees. Anything else accepted is a bug.
				if !bytes.Equal(inflate(data), want) {
					t.Fatalf("flip at %d accepted with different content", i)
				}
				continue
			}
			switch {
			case strings.Contains(err.Error(), "not a gzip stream"):
				gzipSaid++
			case strings.Contains(err.Error(), "not a tar stream"):
				tarSaid++
			}
		}
		if gzipSaid == 0 {
			t.Fatalf("no flip was reported as gzip's (%d as tar's)", tarSaid)
		}
	})
	t.Run("a name a refusal quotes is bounded", func(t *testing.T) {
		_, err := ReadBundle(tgz(t, h.with("x\ny", []byte("x"))))
		refusalContaining(t, err, `bundle: "x\ny" is not a bundle file`)
	})
}
