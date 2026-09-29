// Native fuzz targets for the untrusted-input surfaces: artifact
// tarballs from arbitrary URLs (extract), the path-containment core
// (safeRelPath), and secret-bearing URL redaction. Seed corpora run as
// plain tests in `make test`; real fuzzing happens via `make fuzz`
// (weekly in CI). Properties asserted, not just absence of panics:
// extraction may never place or resolve anything outside its dest dir,
// safeRelPath may never return an escaping path, redactURL may never
// echo credentials or query values.
package liveswap

import (
	"archive/tar"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// fuzzTgzEntries builds a .tar.gz from several entries; a seed that
// fails to render is a test-author error, so it panics.
func fuzzTgzEntries(entries []tarEntry) []byte {
	data, err := tgzBytes(entries)
	if err != nil {
		panic(err)
	}
	return data
}

// fuzzTgz builds a small .tar.gz with a single entry.
func fuzzTgz(name string, mode int64, typeflag byte, linkname, content string) []byte {
	return fuzzTgzEntries([]tarEntry{{name: name, mode: mode, typeflag: typeflag, linkname: linkname, body: content}})
}

func FuzzExtractArchive(f *testing.F) {
	f.Add(fuzzTgz("server", 0o755, tar.TypeReg, "", "#!/bin/sh\necho hi\n"))
	f.Add(fuzzTgz("../evil", 0o644, tar.TypeReg, "", "escape"))
	f.Add(fuzzTgz("/etc/passwd", 0o644, tar.TypeReg, "", "abs"))
	f.Add(fuzzTgz("link", 0o777, tar.TypeSymlink, "../../outside", ""))
	f.Add(fuzzTgz("dev", 0o644, tar.TypeChar, "", ""))
	f.Add([]byte("not a gzip stream at all"))
	f.Add([]byte{0x1f, 0x8b, 0x08}) // truncated gzip header
	// Multi-entry shapes the single-entry helper cannot express: a
	// symlink chain that is inside symbolically and outside on disk (the
	// file variant lands in dest's parent, exactly where the walk below
	// looks); a link target that climbs out through another link, in
	// both orders; and a hard link to a symlink, which re-bases that
	// symlink's relative target.
	f.Add(fuzzTgzEntries(symlinkChainEntries()))
	f.Add(fuzzTgzEntries([]tarEntry{
		{name: "l1", typeflag: tar.TypeSymlink, linkname: "."},
		{name: "l1/x/y", typeflag: tar.TypeSymlink, linkname: "../.."},
		{name: "l1/x/y/evil", body: "outside"},
	}))
	f.Add(fuzzTgzEntries([]tarEntry{
		{name: "a", typeflag: tar.TypeSymlink, linkname: "."},
		{name: "l", typeflag: tar.TypeSymlink, linkname: "a/.."},
	}))
	f.Add(fuzzTgzEntries([]tarEntry{
		{name: "l", typeflag: tar.TypeSymlink, linkname: "a/.."},
		{name: "a", typeflag: tar.TypeSymlink, linkname: "."},
	}))
	f.Add(fuzzTgzEntries([]tarEntry{
		{name: "deep/a/b/link", typeflag: tar.TypeSymlink, linkname: "../../../x"},
		{name: "h", typeflag: tar.TypeLink, linkname: "deep/a/b/link"},
	}))
	// An escape behind more symlink hops than os.Root follows, and a
	// dangling target whose missing part climbs — mutation does not
	// reach either shape from the seeds above.
	f.Add(fuzzTgzEntries(append(hopChainEntries(9), tarEntry{name: "l", typeflag: tar.TypeSymlink, linkname: "c1/../x"})))
	f.Add(fuzzTgzEntries([]tarEntry{
		{name: "a", typeflag: tar.TypeSymlink, linkname: "."},
		{name: "l", typeflag: tar.TypeSymlink, linkname: "gap/../a/../x"},
	}))

	f.Fuzz(func(t *testing.T, archive []byte) {
		parent := t.TempDir()
		arch := filepath.Join(parent, "artifact.tgz")
		if err := os.WriteFile(arch, archive, 0o600); err != nil {
			t.Fatal(err)
		}
		// Deep enough under parent that a chain climbing several
		// hops still lands inside the walked tree.
		dest := filepath.Join(parent, "apps", "app", "releases", "dest")
		if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
			t.Fatal(err)
		}

		if _, err := extractArchive(arch, dest, archiveLimits{maxBytes: 1 << 20, maxEntries: 100_000}); err != nil {
			return // rejection is always a valid outcome
		}

		// Accepted archives must be fully contained: nothing outside
		// dest, and no symlink that resolves outside dest.
		destReal, err := filepath.EvalSymlinks(dest)
		if err != nil {
			t.Fatalf("dest vanished after extract: %v", err)
		}
		sep := string(filepath.Separator)
		_ = filepath.WalkDir(parent, func(p string, d fs.DirEntry, err error) error {
			if err != nil || p == parent || p == arch {
				return nil
			}
			if p != dest && !strings.HasPrefix(p, dest+sep) && !strings.HasPrefix(dest, p+sep) {
				t.Errorf("extraction wrote outside dest: %s", p)
			}
			if d.Type()&fs.ModeSymlink != 0 {
				resolved, err := filepath.EvalSymlinks(p)
				if err != nil {
					return nil // dangling symlink cannot escape
				}
				if resolved != destReal && !strings.HasPrefix(resolved, destReal+string(filepath.Separator)) {
					t.Errorf("symlink %s resolves outside dest: %s", p, resolved)
				}
			}
			return nil
		})
	})
}

func FuzzSafeRelPath(f *testing.F) {
	for _, s := range []string{
		"server", "./a/b", "a/../b", "..", "../x", "/abs", "a//b/",
		"a/./../../b", "..\\windows", "a\x00b", strings.Repeat("../", 50) + "etc/passwd",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		rel, err := safeRelPath(name)
		if err != nil {
			return
		}
		if filepath.IsAbs(rel) {
			t.Errorf("safeRelPath(%q) returned absolute path %q", name, rel)
		}
		clean := filepath.Clean(rel)
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			t.Errorf("safeRelPath(%q) returned escaping path %q", name, rel)
		}
		// Differential guard against LOOSENING: everything the IsLocal
		// gate accepts must also have been accepted by the pre-IsLocal
		// implementation (kept below as the reference), with an
		// identical normalized result. The converse is deliberately
		// not asserted — the new gate is allowed to be stricter.
		refRel, refErr := referenceSafeRelPath(name)
		if refErr != nil {
			t.Errorf("LOOSENED: safeRelPath(%q) accepts %q but the reference rejected it (%v)", name, rel, refErr)
		} else if refRel != rel {
			t.Errorf("DRIFT: safeRelPath(%q) = %q, reference = %q", name, rel, refRel)
		}
	})
}

// referenceSafeRelPath is the pre-filepath.IsLocal implementation,
// frozen verbatim as the differential-fuzz reference. Do not "fix" it:
// its whole value is being exactly what shipped before the swap.
func referenceSafeRelPath(name string) (string, error) {
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("absolute path")
	}
	clean := path.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path traversal")
	}
	if clean == "." {
		return ".", nil
	}
	return clean, nil
}

func FuzzRedactURL(f *testing.F) {
	f.Add("https://user:hunter2@example.com/release.tgz?token=SECRET&sig=x")
	f.Add("https://example.com/a?b=c#frag")
	f.Add("http://[::1]:8080/x%2f..%2fy?k=v")
	f.Fuzz(func(t *testing.T, raw string) {
		u, err := url.Parse(raw)
		if err != nil {
			return
		}
		out := redactURL(u)
		// Structural property, not substring matching: the fuzzer
		// proved substrings collide (a host or path can mirror the
		// password — see testdata corpus). Instead, re-parse the
		// redacted string: it must carry no userinfo, query or
		// fragment, whatever went in.
		ru, err := url.Parse(out)
		if err != nil {
			return // garbage in, unparseable out — nothing leaked
		}
		if ru.User != nil {
			t.Errorf("redactURL kept userinfo: %q -> %q", raw, out)
		}
		if ru.RawQuery != "" {
			t.Errorf("redactURL kept a query: %q -> %q", raw, out)
		}
		if ru.Fragment != "" || ru.RawFragment != "" {
			t.Errorf("redactURL kept a fragment: %q -> %q", raw, out)
		}
	})
}
