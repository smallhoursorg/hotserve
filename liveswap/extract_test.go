package liveswap

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

// testLimits is roomy enough that no happy-path fixture is near a cap.
var testLimits = archiveLimits{maxBytes: 1 << 20, maxEntries: 1000}

// longPath is over PATH_MAX with every component under NAME_MAX, so
// only the whole-path bound can be what refuses it.
var longPath = strings.TrimSuffix(strings.Repeat(strings.Repeat("a", 250)+"/", maxEntryNameLen/250+1), "/")

func TestExtractHappyPath(t *testing.T) {
	archive := buildTarGz(t, []tarEntry{
		{name: "dir/", typeflag: tar.TypeDir},
		{name: "dir/app.js", body: "console.log('hi')"},
		{name: "server", body: "#!/bin/sh\n", mode: 0o755},
		{name: "deep/nested/file.txt", body: "no dir entry for parents"},
		{name: "link-inside", typeflag: tar.TypeSymlink, linkname: "dir/app.js"},
	})
	dest := filepath.Join(t.TempDir(), "out")
	_, err := extractArchive(archive, dest, testLimits)
	must(t, err)

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
		{"name over PATH_MAX", []tarEntry{{name: longPath, body: "x"}}, "exceeds PATH_MAX"},
		{"component over NAME_MAX", []tarEntry{{name: "dir/" + strings.Repeat("a", maxEntryComponentLen+1), body: "x"}}, "name: has a component over"},
		{"link target over PATH_MAX", []tarEntry{{name: "l", typeflag: tar.TypeSymlink, linkname: longPath}}, fmt.Sprintf("link target: %d bytes exceeds PATH_MAX", len(longPath))},
		{"symlink target at PATH_MAX", []tarEntry{{name: "l", typeflag: tar.TypeSymlink, linkname: strings.Repeat("a/", 2047) + "aa"}}, "link target: 4096 bytes exceeds PATH_MAX"},
		{"link target component over NAME_MAX", []tarEntry{{name: "l", typeflag: tar.TypeLink, linkname: strings.Repeat("b", maxEntryComponentLen+1)}}, "link target: has a component over"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			archive := buildTarGz(t, tc.entries)
			dest := filepath.Join(t.TempDir(), "out")
			_, err := extractArchive(archive, dest, testLimits)
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
	_, err := extractArchive(archive, dest, testLimits)
	must(t, err)
}

func TestExtractDecompressionCap(t *testing.T) {
	big := strings.Repeat("A", 64*1024) // compresses tiny, inflates big
	archive := buildTarGz(t, []tarEntry{{name: "bomb", body: big}})
	_, err := extractArchive(archive, filepath.Join(t.TempDir(), "out"), archiveLimits{maxBytes: 16 * 1024, maxEntries: 1000})
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("want decompression-cap error, got %v", err)
	}
}

// The entry cap is a validate-pass rejection: one entry past it and
// nothing is written, however small the entries are (the byte cap
// would let a stream of empty files through).
func TestExtractEntryCap(t *testing.T) {
	entries := make([]tarEntry, 0, 4)
	for i := range 4 {
		entries = append(entries, tarEntry{name: "f" + strconv.Itoa(i)})
	}
	lim := archiveLimits{maxBytes: 1 << 20, maxEntries: 3}

	dest := filepath.Join(t.TempDir(), "out")
	_, err := extractArchive(buildTarGz(t, entries), dest, lim)
	if err == nil || !strings.Contains(err.Error(), "more than 3 files") {
		t.Fatalf("want entry-cap error, got %v", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatal("entry-cap rejection must not leave extracted files")
	}

	// Exactly at the cap is fine, and a global header is not an entry.
	dest = filepath.Join(t.TempDir(), "out")
	stats, err := extractArchive(buildTarGz(t, entries[:3]), dest, lim)
	must(t, err)
	if stats.entries != 3 {
		t.Fatalf("entries = %d, want 3", stats.entries)
	}
}

// Every filesystem object counts, not every header: a directory an
// entry implies is created by extraction just as an explicit one is,
// so a single deep entry cannot smuggle a thousand directories past
// the cap. Explicit and implicit, and repeats, count once.
func TestExtractEntryCapCountsImpliedDirectories(t *testing.T) {
	entries := []tarEntry{
		{name: "a/b/c/d/file", body: "x"},     // a, a/b, a/b/c, a/b/c/d, file = 5
		{name: "a/b/", typeflag: tar.TypeDir}, // already implied
		{name: "a/b/c/d/file", body: "y"},     // repeat
	}
	dest := filepath.Join(t.TempDir(), "out")
	_, err := extractArchive(buildTarGz(t, entries), dest, archiveLimits{maxBytes: 1 << 20, maxEntries: 4})
	if err == nil || !strings.Contains(err.Error(), "more than 4 files") {
		t.Fatalf("want entry-cap error for the implied directories, got %v", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatal("entry-cap rejection must not leave extracted files")
	}
	stats, err := extractArchive(buildTarGz(t, entries), filepath.Join(t.TempDir(), "out"), archiveLimits{maxBytes: 1 << 20, maxEntries: 5})
	must(t, err)
	if stats.entries != 5 {
		t.Fatalf("entries = %d, want 5 distinct objects", stats.entries)
	}
}

// ustarBlock is one raw ustar header block, checksummed.
func ustarBlock(name string, typeflag byte, size int64) []byte {
	blk := make([]byte, 512)
	put := func(off int, s string) { copy(blk[off:], s) }
	put(0, name)
	put(100, "0000644\x00")
	put(108, "0000000\x00")
	put(116, "0000000\x00")
	put(124, fmt.Sprintf("%011o\x00", size))
	put(136, "00000000000\x00")
	blk[156] = typeflag
	put(257, "ustar\x00")
	put(263, "00")
	put(148, "        ") // the checksum is computed with its own field as spaces
	sum := 0
	for _, b := range blk {
		sum += int(b)
	}
	put(148, fmt.Sprintf("%06o\x00 ", sum))
	return blk
}

// sparseTarGz hand-builds a PAX-format GNU sparse entry (format 1.0);
// archive/tar reads these — as a plain regular file whose Size is the
// real size, synthesizing the holes from no stream at all — but its
// writer drops the GNU.sparse records, so the blocks are written by
// hand. (The old GNU form has its own typeflag and is refused by the
// type allowlist; this form is TypeReg on the way out of the reader,
// so only the declared-content cap can catch it.) The stored data is
// just the sparse map — "0\n", no data runs — in one 512-byte block.
func sparseTarGz(t *testing.T, logical int64) string {
	t.Helper()
	var records string
	for _, kv := range []string{"GNU.sparse.major=1", "GNU.sparse.minor=0", "GNU.sparse.realsize=" + fmt.Sprint(logical)} {
		// "<len> key=value\n", the length counting its own digits.
		base := len(" " + kv + "\n")
		l := base + len(strconv.Itoa(base))
		if len(strconv.Itoa(l)) > len(strconv.Itoa(base)) {
			l++
		}
		records += fmt.Sprintf("%d %s\n", l, kv)
	}
	pad := func(b []byte) []byte { return append(b, make([]byte, (512-len(b)%512)%512)...) }

	var raw []byte
	raw = append(raw, ustarBlock("PaxHeaders/hole", tar.TypeXHeader, int64(len(records)))...)
	raw = append(raw, pad([]byte(records))...)
	raw = append(raw, ustarBlock("hole", tar.TypeReg, 512)...)
	raw = append(raw, pad([]byte("0\n"))...)
	raw = append(raw, make([]byte, 1024)...) // end of archive

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, err := gz.Write(raw)
	must(t, err)
	must(t, gz.Close())
	path := filepath.Join(t.TempDir(), "sparse.tar.gz")
	must(t, os.WriteFile(path, buf.Bytes(), 0o600))
	return path
}

// A sparse entry's holes come from nowhere in the stream, so the
// stream cap never sees them; the declared-content cap does, and
// nothing is written.
func TestExtractCapsDeclaredContent(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out")
	_, err := extractArchive(sparseTarGz(t, 1<<30), dest, archiveLimits{maxBytes: 64 * 1024, maxEntries: 1000})
	if err == nil || !strings.Contains(err.Error(), "declared beyond") {
		t.Fatalf("want declared-content cap error, got %v", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatal("cap rejection must not leave extracted files")
	}
	// Under the cap it is a legitimate (if odd) file, and the reported
	// bytes are what reached the disk, not the few the stream carried.
	stats, err := extractArchive(sparseTarGz(t, 4096), dest, archiveLimits{maxBytes: 64 * 1024, maxEntries: 1000})
	must(t, err)
	if info, err := os.Stat(filepath.Join(dest, "hole")); err != nil || info.Size() != 4096 {
		t.Fatalf("sparse file not materialized at its logical size: %v %v", info, err)
	}
	if stats.bytes != 4096 {
		t.Fatalf("bytes = %d, want the declared 4096", stats.bytes)
	}
}

// The joined path is what the filesystem sees, so a name that fits
// PATH_MAX on its own but not under the release directory is refused
// in the validate pass, not by ENAMETOOLONG mid-write.
func TestExtractRejectsNameTooLongUnderDest(t *testing.T) {
	parent := t.TempDir()
	dest := filepath.Join(parent, strings.Repeat("d/", 2040), "out")
	archive := buildTarGz(t, []tarEntry{{name: "fine", body: "x"}})
	_, err := extractArchive(archive, dest, testLimits)
	if err == nil || !strings.Contains(err.Error(), "exceeds PATH_MAX") {
		t.Fatalf("want PATH_MAX error for the joined path, got %v", err)
	}
	// dest itself is too long to stat; its first component under the
	// temp dir is what MkdirAll would have created.
	if _, statErr := os.Stat(filepath.Join(parent, "d")); !os.IsNotExist(statErr) {
		t.Fatal("rejection must not create the destination")
	}
}

// The reported stats are what the caps are measured against: entries
// of every allowed type count, and bytes are the decompressed stream
// (headers included), not file contents.
func TestExtractReportsStats(t *testing.T) {
	body := strings.Repeat("x", 1000)
	archive := buildTarGz(t, []tarEntry{
		{name: "dir/", typeflag: tar.TypeDir},
		{name: "dir/a", body: body},
		{name: "link", typeflag: tar.TypeSymlink, linkname: "dir/a"},
		{name: "hard", typeflag: tar.TypeLink, linkname: "dir/a"},
	})
	stats, err := extractArchive(archive, filepath.Join(t.TempDir(), "out"), testLimits)
	must(t, err)
	if stats.entries != 4 {
		t.Fatalf("entries = %d, want 4", stats.entries)
	}
	// Four 512-byte headers plus the body padded to a 512-byte block,
	// plus the two zero blocks that end a tar stream: the reader
	// consumes at least that many bytes, and never more than the cap.
	if stats.bytes < 4*512+1024 || stats.bytes > testLimits.maxBytes {
		t.Fatalf("bytes = %d, want the decompressed stream size", stats.bytes)
	}
}

func TestExtractRejectsNonGzip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not.tar.gz")
	must(t, os.WriteFile(path, []byte("plain text"), 0o600))
	_, err := extractArchive(path, filepath.Join(t.TempDir(), "out"), testLimits)
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
