package liveswap

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type tarEntry struct {
	name     string
	typeflag byte
	linkname string
	mode     int64
	body     string
}

func buildTarGz(t *testing.T, entries []tarEntry) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Linkname: e.linkname,
			Mode:     mode,
			Size:     int64(len(e.body)),
		}
		if e.typeflag == 0 {
			hdr.Typeflag = tar.TypeReg
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	must(t, tw.Close())
	must(t, gz.Close())
	path := filepath.Join(t.TempDir(), "artifact.tar.gz")
	must(t, os.WriteFile(path, buf.Bytes(), 0o600))
	return path
}

func TestExtractHappyPath(t *testing.T) {
	archive := buildTarGz(t, []tarEntry{
		{name: "dir/", typeflag: tar.TypeDir},
		{name: "dir/app.js", body: "console.log('hi')"},
		{name: "server", body: "#!/bin/sh\n", mode: 0o755},
		{name: "deep/nested/file.txt", body: "no dir entry for parents"},
		{name: "link-inside", typeflag: tar.TypeSymlink, linkname: "dir/app.js"},
	})
	dest := filepath.Join(t.TempDir(), "out")
	must(t, extractArchive(archive, dest, 1<<20))

	data, err := os.ReadFile(filepath.Join(dest, "dir", "app.js"))
	if err != nil || string(data) != "console.log('hi')" {
		t.Fatalf("file content wrong: %q %v", data, err)
	}
	info, err := os.Stat(filepath.Join(dest, "server"))
	if err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("exec bit lost: %v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "deep", "nested", "file.txt")); err != nil {
		t.Fatalf("implicit parent dirs not created: %v", err)
	}
	target, err := os.Readlink(filepath.Join(dest, "link-inside"))
	if err != nil || target != "dir/app.js" {
		t.Fatalf("symlink wrong: %q %v", target, err)
	}
}

func TestExtractRejectsMaliciousArchives(t *testing.T) {
	cases := []struct {
		name    string
		entries []tarEntry
		want    string
	}{
		{"not local", []tarEntry{{name: "/etc/passwd", body: "x"}}, "not local"},
		{"dotdot traversal", []tarEntry{{name: "../../evil", body: "x"}}, "not local"},
		{"sneaky traversal", []tarEntry{{name: "ok/../../evil", body: "x"}}, "not local"},
		{"absolute symlink", []tarEntry{{name: "l", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"}}, "absolute target"},
		{"escaping symlink", []tarEntry{{name: "l", typeflag: tar.TypeSymlink, linkname: "../outside"}}, "escapes archive root"},
		{"nested escaping symlink", []tarEntry{{name: "a/b/l", typeflag: tar.TypeSymlink, linkname: "../../../outside"}}, "escapes archive root"},
		{"absolute hardlink", []tarEntry{{name: "h", typeflag: tar.TypeLink, linkname: "/etc/passwd"}}, "absolute target"},
		{"escaping hardlink", []tarEntry{{name: "h", typeflag: tar.TypeLink, linkname: "../outside"}}, "escapes archive root"},
		{"device node", []tarEntry{{name: "dev", typeflag: tar.TypeChar}}, "unsupported type"},
		{"fifo", []tarEntry{{name: "pipe", typeflag: tar.TypeFifo}}, "unsupported type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			archive := buildTarGz(t, tc.entries)
			dest := filepath.Join(t.TempDir(), "out")
			err := extractArchive(archive, dest, 1<<20)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
			// Validation is a pre-pass: nothing may have been written.
			if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
				t.Fatal("validation failure must not leave extracted files")
			}
		})
	}
}

// A symlink whose target resolves inside the root via a subdirectory
// is legitimate (node_modules/.bin does this constantly).
func TestExtractAllowsInternalRelativeSymlink(t *testing.T) {
	archive := buildTarGz(t, []tarEntry{
		{name: "bin/tool", body: "x", mode: 0o755},
		{name: "nested/.bin/tool", typeflag: tar.TypeSymlink, linkname: "../../bin/tool"},
	})
	dest := filepath.Join(t.TempDir(), "out")
	must(t, extractArchive(archive, dest, 1<<20))
}

func TestExtractDecompressionCap(t *testing.T) {
	big := strings.Repeat("A", 64*1024) // compresses tiny, inflates big
	archive := buildTarGz(t, []tarEntry{{name: "bomb", body: big}})
	err := extractArchive(archive, filepath.Join(t.TempDir(), "out"), 16*1024)
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("want decompression-cap error, got %v", err)
	}
}

func TestExtractRejectsNonGzip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not.tar.gz")
	must(t, os.WriteFile(path, []byte("plain text"), 0o600))
	err := extractArchive(path, filepath.Join(t.TempDir(), "out"), 1<<20)
	if err == nil || !strings.Contains(err.Error(), "gzip") {
		t.Fatalf("want gzip error, got %v", err)
	}
}

func TestSafeRelPath(t *testing.T) {
	for input, wantErr := range map[string]bool{
		"ok.txt":         false,
		"a/b/c":          false,
		"./fine":         false,
		"/abs":           true,
		"..":             true,
		"../up":          true,
		"a/../../out":    true,
		"a/./b/../c":     false,
		"trailing/../..": true,
		"":               false, // cleans to "." — the archive root, accepted
		"./":             false, // ditto
		"a/..":           false, // cleans to "."
		"a//b/":          false, // cleaned to a/b
		"..\\up":         false, // backslash is an ordinary byte in tar names on unix
	} {
		_, err := safeRelPath(input)
		if (err != nil) != wantErr {
			t.Errorf("safeRelPath(%q) err=%v, wantErr=%v", input, err, wantErr)
		}
	}
}
