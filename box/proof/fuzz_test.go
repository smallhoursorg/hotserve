package proof

// Native fuzz targets for every surface that parses a bundle's bytes:
// the commit and tree objects, the path, a signer line, and the tarball
// itself. Seed corpora run as plain tests in `make test`; real fuzzing
// is `make fuzz` (weekly in CI). Properties asserted, not just absence
// of panics: an id is always computed from the bytes; a parsed tree
// serialises back to its bytes; a signer that parses is one the file
// format accepts; a bundle that reads has every object filed under the
// id it hashes to, and never more than the caps.

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var hex40 = regexp.MustCompile(`^[0-9a-f]{40}$`)

// fixtureObjects adds the committed fixtures as seeds, when present.
func fixtureObjects(f *testing.F, want func(raw []byte) bool) {
	entries, err := os.ReadDir(filepath.Join("testdata", "objects"))
	if err != nil {
		return
	}
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join("testdata", "objects", e.Name()))
		if err == nil && want(raw) {
			f.Add(raw)
		}
	}
}

func FuzzParseCommit(f *testing.F) {
	fixtureObjects(f, func(raw []byte) bool { return bytes.HasPrefix(raw, []byte("tree ")) })
	f.Add(commitObject(emptyTree, []string{zeroID}, nil, "msg\n"))
	f.Add(commitObject(emptyTree, nil, []string{gpgsigHeader(sshArmor)}, "msg\n"))
	f.Add(commitObject(emptyTree, nil, []string{gpgsigHeader(pgpArmor)}, "msg\n"))
	f.Add(commitObject(emptyTree, []string{zeroID, zeroID}, []string{"mergetag object " + zeroID + "\n type commit", gpgsigHeader(sshArmor), "encoding UTF-8"}, ""))
	f.Add([]byte("tree " + strings.Repeat("ab", 32) + "\n\nm"))
	f.Add([]byte(" tree\n\n"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		c, err := ParseCommit(raw)
		if err != nil {
			var r *Refusal
			if !asRefusal(err, &r) {
				t.Fatalf("not a refusal: %v", err)
			}
			return
		}
		if c.ID != ObjectID("commit", raw) || !hex40.MatchString(c.Tree) {
			t.Fatalf("%+v", c)
		}
		for _, p := range c.Parents {
			if !hex40.MatchString(p) {
				t.Fatalf("parent %q", p)
			}
		}
		switch c.Kind {
		case Unsigned:
			if c.Signature != nil || !bytes.Equal(c.Payload, raw) {
				t.Fatal("an unsigned commit's payload is the object")
			}
		default:
			// The header removed is exactly "gpgsig " + the signature
			// with one leading space per continuation line.
			lines := bytes.Count(c.Signature, []byte("\n"))
			if len(raw) != len(c.Payload)+len("gpgsig ")+len(c.Signature)+lines-1 {
				t.Fatalf("payload %d + header != raw %d", len(c.Payload), len(raw))
			}
			if bytes.Contains(c.Payload[:bytes.Index(c.Payload, []byte("\n\n"))+1], []byte("\ngpgsig ")) || bytes.HasPrefix(c.Payload, []byte("gpgsig ")) {
				t.Fatal("a gpgsig header survived in the payload")
			}
		}
		if len(raw) > MaxCommit {
			t.Fatal("over the cap")
		}
	})
}

func FuzzParseTree(f *testing.F) {
	fixtureObjects(f, func(raw []byte) bool {
		return !bytes.HasPrefix(raw, []byte("tree ")) && !bytes.HasPrefix(raw, []byte("#"))
	})
	f.Add([]byte{})
	f.Add(treeObject(Entry{ModeFile, "Caddyfile", emptyBlob}, Entry{ModeDir, "d", emptyTree}))
	f.Add(treeObject(Entry{ModeSymlink, "l", emptyBlob}, Entry{ModeSubmodule, "s", zeroID}, Entry{ModeExecutable, "x", emptyBlob}))
	f.Add(treeObject(Entry{"040000", "d", emptyTree}))
	f.Fuzz(func(t *testing.T, raw []byte) {
		tr, err := ParseTree(raw)
		if err != nil {
			var r *Refusal
			if !asRefusal(err, &r) {
				t.Fatalf("not a refusal: %v", err)
			}
			return
		}
		if tr.ID != ObjectID("tree", raw) || len(raw) > MaxTree {
			t.Fatal(tr.ID)
		}
		seen := map[string]bool{}
		for _, e := range tr.Entries {
			if e.Name == "" || e.Name == "." || e.Name == ".." || strings.ContainsAny(e.Name, "/\x00") || seen[e.Name] || !hex40.MatchString(e.ID) {
				t.Fatalf("%+v", e)
			}
			seen[e.Name] = true
		}
		// Serialising the entries gives the bytes back: nothing was
		// skipped and nothing invented.
		if !bytes.Equal(treeObject(tr.Entries...), raw) {
			t.Fatal("round trip")
		}
	})
}

func FuzzSplitPath(f *testing.F) {
	for _, s := range []string{"box1/Caddyfile", "Caddyfile", "", "/", "./x", "a/../b", "a//b", "a/", strings.Repeat("a/", 40), "a\x00b", "é/ü"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, path string) {
		parts, err := SplitPath(path)
		if err != nil {
			return
		}
		if len(parts) == 0 || len(parts) > MaxDepth || len(path) > MaxPath || strings.Join(parts, "/") != path {
			t.Fatalf("%q -> %q", path, parts)
		}
		for _, p := range parts {
			if p == "" || p == "." || p == ".." || strings.ContainsAny(p, "/\x00\n\x7f") || strings.ContainsFunc(p, isControl) {
				t.Fatalf("%q", p)
			}
		}
	})
}

func FuzzParseSigner(f *testing.F) {
	f.Add("alice@example.com", "ssh-ed25519", "AAAAC3NzaC1lZDI1NTE5AAAAIGl0ZXJhdGVkIGtleSBieXRlcyB0aGF0IGFyZSAzMg==")
	f.Add("bob", "ssh-rsa", "AAAA")
	f.Add("*", "ssh-ed25519", "")
	f.Add("a b", "sk-ssh-ed25519@openssh.com", "AAAA")
	f.Fuzz(func(t *testing.T, principal, keyType, b64 string) {
		s, err := ParseSigner(principal, keyType, b64)
		if err != nil {
			return
		}
		if !principalRE.MatchString(s.Principal) || !keyTypes[s.Type] || len(s.Key) == 0 || s.B64 != b64 {
			t.Fatalf("%+v", s)
		}
		if _, err := (Signers{s}).AllowedSigners(); err != nil {
			t.Fatal(err)
		}
	})
}

func FuzzReadBundle(f *testing.F) {
	h := newHandMade(f)
	f.Add(tgz(f, h.files))
	f.Add(tgz(f, h.with("parents/0001", nil)))
	f.Add(tgz(f, h.with("path", []byte("box1/Caddyfile\n"))))
	f.Add(tgz(f, h.with("README", []byte("x"))))
	f.Add(tgz(f, h.with("parents/0002", h.parent.Raw)))
	f.Add(tgz(f, h.with("trees/"+h.root.ID, h.inner.Raw)))
	f.Add(tgz(f, map[string][]byte{}))
	f.Add([]byte("not gzip"))
	f.Fuzz(func(t *testing.T, data []byte) {
		b, err := ReadBundle(data)
		if err != nil {
			var r *Refusal
			if !asRefusal(err, &r) {
				t.Fatalf("not a refusal: %v", err)
			}
			return
		}
		if _, err := SplitPath(b.Path); err != nil {
			t.Fatal(err)
		}
		if len(b.Caddyfile) == 0 || len(b.Caddyfile) > MaxCaddyfile || b.Commit == nil || b.Commit.ID != ObjectID("commit", b.Commit.Raw) {
			t.Fatalf("%s", b)
		}
		if len(b.Parents) > MaxChain {
			t.Fatal("over the chain cap")
		}
		for _, c := range b.Parents {
			if c.ID != ObjectID("commit", c.Raw) {
				t.Fatalf("parent %s", c.ID)
			}
		}
		for id, tr := range b.Trees {
			if tr.ID != id || ObjectID("tree", tr.Raw) != id {
				t.Fatalf("tree %s filed as %s", tr.ID, id)
			}
		}
	})
}

// asRefusal is errors.As for the fuzz targets, which must never see
// an error that is not a verdict.
func asRefusal(err error, r **Refusal) bool {
	var ok bool
	*r, ok = err.(*Refusal) //nolint:errorlint // the proof package wraps nothing; a wrapped refusal here would itself be the bug
	return ok
}
