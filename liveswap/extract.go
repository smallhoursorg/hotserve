package liveswap

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// decompressionRatioCap bounds the decompressed size of an artifact at
// this multiple of max_artifact_size — a guard against gzip bombs. A
// legitimate app tarball compresses far below 10:1.
const decompressionRatioCap = 10

// maxEntryNameLen and maxEntryComponentLen bound an entry's name and
// link target: PATH_MAX (4096, NUL included) over the whole path and
// NAME_MAX (255) per component on Linux. A name past either could
// never be created, so the validate pass refuses it up front — the
// name alone here, and the name joined under the release directory in
// extractArchive — instead of paying the path work and failing
// mid-write. Not knobs: no real path is close.
const (
	maxEntryNameLen      = 4096
	maxEntryComponentLen = 255
)

// archiveLimits is what an archive may cost: bytes over the
// decompressed stream (max_artifact_size × decompressionRatioCap) and
// entries (max_artifact_entries). The byte cap bounds the tar stream,
// not what extraction consumes — a stream of 1-byte files costs an
// inode and a 4 KB block per entry, so a budget of 1 GB is ~1M inodes
// and ~4 GB of blocks, enough to take a small disk to ENOSPC for
// everything else on the box. The entry cap is what bounds that.
type archiveLimits struct {
	maxBytes   int64
	maxEntries int
}

// archiveStats is what the archive did cost, reported so a deploy can
// see a cap coming (see warnNearCaps): entries is the filesystem
// objects extraction creates — files, directories and links, the
// parent directories an entry implies included — and bytes the larger
// of the decompressed stream and the content the entries declare.
type archiveStats struct {
	entries int
	bytes   int64
}

// extractArchive validates and then extracts a .tar.gz into destDir.
// It is a pure-Go port of the hardened `tar` wrapper from the webhook
// this module replaces, with the same rejections: absolute paths, `..`
// traversal, symlink/hardlink targets escaping the archive root, and
// special files (devices, FIFOs). Validation is a full first pass over
// the archive so nothing is written to disk unless every entry is
// clean — the entry and byte caps included.
func extractArchive(archivePath, destDir string, lim archiveLimits) (archiveStats, error) {
	validate := func(hdr *tar.Header, r io.Reader) error {
		// PATH_MAX counts the whole path and its NUL, and the entry
		// lands under destDir: a name the filesystem would refuse
		// there is refused here, before anything is written. destDir
		// is the staging dir, longer than the release dir it becomes.
		if len(destDir)+1+len(hdr.Name) >= maxEntryNameLen {
			return fmt.Errorf("archive entry name: %d bytes under the release directory exceeds PATH_MAX", len(destDir)+1+len(hdr.Name))
		}
		if hdr.Typeflag == tar.TypeLink && len(destDir)+1+len(hdr.Linkname) >= maxEntryNameLen {
			return fmt.Errorf("archive entry: hardlink target of %d bytes under the release directory exceeds PATH_MAX", len(destDir)+1+len(hdr.Linkname))
		}
		return validateEntry(hdr, r)
	}
	stats, err := walkArchive(archivePath, lim, validate)
	if err != nil {
		return archiveStats{}, err
	}
	if err := os.MkdirAll(destDir, 0o750); err != nil {
		return archiveStats{}, err
	}
	if _, err := walkArchive(archivePath, lim, func(hdr *tar.Header, r io.Reader) error {
		return writeEntry(destDir, hdr, r)
	}); err != nil {
		return archiveStats{}, err
	}
	return stats, nil
}

// walkArchive iterates the archive's entries under three caps, all
// against lim: the bytes read out of the gzip stream, so header
// floods and bombs stop early; the content the entries declare, which
// is what reaches the disk — a sparse entry's holes are synthesized by
// the reader without a byte of stream behind them, so the stream cap
// alone would let one small entry write a disk full of zeros; and the
// filesystem objects extraction creates, so a flood of tiny entries
// (or one entry with a thousand implied parent directories) cannot
// exhaust inodes within the byte budget.
func walkArchive(archivePath string, lim archiveLimits, fn func(*tar.Header, io.Reader) error) (archiveStats, error) {
	f, err := os.Open(archivePath) //nolint:gosec // path is our own just-downloaded temp file, not request input
	if err != nil {
		return archiveStats{}, err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return archiveStats{}, fmt.Errorf("not a gzip archive: %w", err)
	}
	defer func() { _ = gz.Close() }()

	lr := &io.LimitedReader{R: gz, N: lim.maxBytes + 1}
	tr := tar.NewReader(lr)
	var stats archiveStats
	var declared int64
	objects := newObjectCounter()
	for {
		hdr, err := tr.Next()
		// The cap first: a stream that ends exactly as the budget does
		// is over it, not complete.
		if lr.N <= 0 {
			return archiveStats{}, fmt.Errorf("archive decompresses beyond the %d-byte cap", lim.maxBytes)
		}
		if err == io.EOF {
			stats.bytes = max(lim.maxBytes+1-lr.N, declared)
			return stats, nil
		}
		if err != nil {
			return archiveStats{}, fmt.Errorf("corrupt archive: %w", err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		if hdr.Size < 0 || hdr.Size > lim.maxBytes-declared {
			return archiveStats{}, fmt.Errorf("archive content declared beyond the %d-byte cap", lim.maxBytes)
		}
		declared += hdr.Size
		// Bound the name before anything walks it: the counter below
		// hashes every component, and a PAX name can be megabytes.
		if err := checkNameLength(hdr.Name); err != nil {
			return archiveStats{}, fmt.Errorf("archive entry name: %w", err)
		}
		stats.entries += objects.add(hdr.Name)
		if stats.entries > lim.maxEntries {
			return archiveStats{}, fmt.Errorf("archive creates more than %d files, directories and links (max_artifact_entries)", lim.maxEntries)
		}
		if err := fn(hdr, tr); err != nil {
			if lr.N <= 0 {
				// The entry read hit the cap; report the cap, not the
				// confusing mid-entry EOF it causes.
				return archiveStats{}, fmt.Errorf("archive decompresses beyond the %d-byte cap", lim.maxBytes)
			}
			return archiveStats{}, err
		}
	}
}

// validateEntry rejects anything that could write outside the
// extraction root or that has no business in an app artifact.
func validateEntry(hdr *tar.Header, r io.Reader) error {
	// The name's own length was bounded in walkArchive, before it was
	// counted; the link target is only ever looked at here.
	if err := checkNameLength(hdr.Linkname); err != nil {
		return fmt.Errorf("archive entry: link target: %w", err)
	}
	name, err := safeRelPath(hdr.Name)
	if err != nil {
		return fmt.Errorf("archive entry %q: %w", hdr.Name, err)
	}
	switch hdr.Typeflag {
	case tar.TypeReg, tar.TypeDir:
		// fine
	case tar.TypeSymlink:
		if err := linkTargetStaysInside(name, hdr.Linkname, true); err != nil {
			return fmt.Errorf("archive symlink %q -> %q: %w", hdr.Name, hdr.Linkname, err)
		}
	case tar.TypeLink:
		if err := linkTargetStaysInside(name, hdr.Linkname, false); err != nil {
			return fmt.Errorf("archive hardlink %q -> %q: %w", hdr.Name, hdr.Linkname, err)
		}
	default:
		return fmt.Errorf("archive entry %q has unsupported type %q", hdr.Name, hdr.Typeflag)
	}
	// Drain the entry so the decompressed-size cap in walkArchive sees
	// file contents, not just headers.
	_, err = io.Copy(io.Discard, r)
	return err
}

// writeEntry materializes one already-validated entry under destDir.
func writeEntry(destDir string, hdr *tar.Header, r io.Reader) error {
	name, err := safeRelPath(hdr.Name)
	if err != nil {
		return err
	}
	target := filepath.Join(destDir, filepath.FromSlash(name))
	switch hdr.Typeflag {
	case tar.TypeDir:
		return os.MkdirAll(target, 0o750)
	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		// Perm() keeps rwx bits only — setuid/setgid/sticky never
		// survive extraction. Owner rwx is forced so the app user can
		// always read (and re-deploys can delete) what it shipped.
		mode := hdr.FileInfo().Mode().Perm() | 0o600
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode) //nolint:gosec // target passed safeRelPath containment in validateEntry
		if err != nil {
			return err
		}
		_, err = io.Copy(f, r)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		return err
	case tar.TypeSymlink:
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		return os.Symlink(hdr.Linkname, target)
	case tar.TypeLink:
		linkSrc, err := safeRelPath(hdr.Linkname)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		return os.Link(filepath.Join(destDir, filepath.FromSlash(linkSrc)), target)
	default:
		return fmt.Errorf("unsupported type %q reached extraction", hdr.Typeflag)
	}
}

// objectCounter counts the distinct filesystem objects a sequence of
// entry names creates: each name once, and each parent directory it
// implies once (writeEntry MkdirAlls them). Keyed by a SHA-256 of the
// path rather than the path itself so a hostile archive of maximal
// names costs the counter a few megabytes, not hundreds — and by a
// cryptographic hash rather than a fast one because the names are the
// attacker's: a crafted collision would mark a new path as seen, and
// with it every parent the walk below would otherwise have counted.
type objectCounter struct {
	seen map[[sha256.Size]byte]struct{}
}

func newObjectCounter() *objectCounter {
	return &objectCounter{seen: map[[sha256.Size]byte]struct{}{}}
}

// add records name and returns how many objects it newly creates.
// Names are counted as cleaned; a name that validation will reject
// (absolute, traversing) counts whatever it counts and is then
// rejected.
func (c *objectCounter) add(name string) int {
	n := 0
	p := path.Clean(name)
	for p != "." && p != "/" {
		key := sha256.Sum256([]byte(p))
		if _, ok := c.seen[key]; ok {
			break // a seen path has seen ancestors
		}
		c.seen[key] = struct{}{}
		n++
		p = path.Dir(p)
	}
	return n
}

// checkNameLength applies the PATH_MAX / NAME_MAX bounds to one name.
func checkNameLength(name string) error {
	// PATH_MAX counts the NUL: 4095 bytes is the longest name that can
	// exist, and a symlink target gets no other check before os.Symlink.
	if len(name) >= maxEntryNameLen {
		return fmt.Errorf("%d bytes exceeds PATH_MAX (%d with its NUL)", len(name), maxEntryNameLen)
	}
	for _, c := range strings.Split(name, "/") {
		if len(c) > maxEntryComponentLen {
			return fmt.Errorf("has a component over %d bytes", maxEntryComponentLen)
		}
	}
	return nil
}

// safeRelPath normalizes an archive path and rejects anything that
// could resolve outside the extraction root. The gate is the standard
// library's filepath.IsLocal — the canonical "not absolute, cannot
// traverse out" validator — applied to the cleaned path. On unix this
// accepts exactly what the previous hand-rolled prefix checks did
// (FuzzSafeRelPath differentially proves nothing newly accepted, and
// identical results, against the old implementation kept as a
// reference); where the two differ, IsLocal is strictly tighter
// (Windows reserved device names, embedded NUL). Using the stdlib
// validator also lets static analysis recognize the sanitization
// (CodeQL models IsLocal as a path-injection barrier) instead of
// flagging every downstream use of the returned path.
// Returned paths are slash-separated and relative to the archive root.
func safeRelPath(name string) (string, error) {
	clean := path.Clean(name)
	if clean == "." {
		// The archive root itself ("." / "./" — common as a tarball's
		// first entry); callers just MkdirAll it. An explicit accept
		// because filepath.IsLocal(".") is false by definition.
		return ".", nil
	}
	if !filepath.IsLocal(clean) {
		return "", fmt.Errorf("path is not local to the archive root")
	}
	return clean, nil
}

// linkTargetStaysInside verifies that a link target, resolved from the
// link's own directory (symlinks resolve relative to their location;
// hardlink targets are archive-root-relative), stays inside the
// archive root. Absolute targets are rejected outright.
func linkTargetStaysInside(linkName, target string, relativeToLinkDir bool) error {
	if strings.HasPrefix(target, "/") {
		return fmt.Errorf("absolute target")
	}
	base := "."
	if relativeToLinkDir {
		base = path.Dir(linkName)
	}
	resolved := path.Join(base, target)
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("target escapes archive root")
	}
	return nil
}
