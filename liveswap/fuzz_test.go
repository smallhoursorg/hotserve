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
	"bytes"
	"compress/gzip"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// fuzzTgz builds a small .tar.gz with a single entry.
func fuzzTgz(name string, mode int64, typeflag byte, linkname, content string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     mode,
		Typeflag: typeflag,
		Linkname: linkname,
		Size:     int64(len(content)),
	})
	_, _ = tw.Write([]byte(content))
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func FuzzExtractArchive(f *testing.F) {
	f.Add(fuzzTgz("server", 0o755, tar.TypeReg, "", "#!/bin/sh\necho hi\n"))
	f.Add(fuzzTgz("../evil", 0o644, tar.TypeReg, "", "escape"))
	f.Add(fuzzTgz("/etc/passwd", 0o644, tar.TypeReg, "", "abs"))
	f.Add(fuzzTgz("link", 0o777, tar.TypeSymlink, "../../outside", ""))
	f.Add(fuzzTgz("dev", 0o644, tar.TypeChar, "", ""))
	f.Add([]byte("not a gzip stream at all"))
	f.Add([]byte{0x1f, 0x8b, 0x08}) // truncated gzip header

	f.Fuzz(func(t *testing.T, archive []byte) {
		parent := t.TempDir()
		arch := filepath.Join(parent, "artifact.tgz")
		if err := os.WriteFile(arch, archive, 0o600); err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(parent, "dest")

		if err := extractArchive(arch, dest, 1<<20); err != nil {
			return // rejection is always a valid outcome
		}

		// Accepted archives must be fully contained: nothing outside
		// dest, and no symlink that resolves outside dest.
		destReal, err := filepath.EvalSymlinks(dest)
		if err != nil {
			t.Fatalf("dest vanished after extract: %v", err)
		}
		_ = filepath.WalkDir(parent, func(p string, d fs.DirEntry, err error) error {
			if err != nil || p == parent || p == arch {
				return nil
			}
			if p != dest && !strings.HasPrefix(p, dest+string(filepath.Separator)) {
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
