package hotswap

import (
	"archive/tar"
	"compress/gzip"
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

// extractArchive validates and then extracts a .tar.gz into destDir.
// It is a pure-Go port of the hardened `tar` wrapper from the webhook
// this module replaces, with the same rejections: absolute paths, `..`
// traversal, symlink/hardlink targets escaping the archive root, and
// special files (devices, FIFOs). Validation is a full first pass over
// the archive so nothing is written to disk unless every entry is
// clean.
func extractArchive(archivePath, destDir string, maxDecompressed int64) error {
	if err := walkArchive(archivePath, maxDecompressed, validateEntry); err != nil {
		return err
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	return walkArchive(archivePath, maxDecompressed, func(hdr *tar.Header, r io.Reader) error {
		return writeEntry(destDir, hdr, r)
	})
}

// walkArchive iterates the archive's entries, capping the total bytes
// read out of the gzip stream so header floods and bombs stop early.
func walkArchive(archivePath string, maxDecompressed int64, fn func(*tar.Header, io.Reader) error) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("not a gzip archive: %v", err)
	}
	defer func() { _ = gz.Close() }()

	lr := &io.LimitedReader{R: gz, N: maxDecompressed + 1}
	tr := tar.NewReader(lr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if lr.N <= 0 {
			return fmt.Errorf("archive decompresses beyond the %d-byte cap", maxDecompressed)
		}
		if err != nil {
			return fmt.Errorf("corrupt archive: %v", err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		if err := fn(hdr, tr); err != nil {
			if lr.N <= 0 {
				// The entry read hit the cap; report the cap, not the
				// confusing mid-entry EOF it causes.
				return fmt.Errorf("archive decompresses beyond the %d-byte cap", maxDecompressed)
			}
			return err
		}
	}
}

// validateEntry rejects anything that could write outside the
// extraction root or that has no business in an app artifact.
func validateEntry(hdr *tar.Header, r io.Reader) error {
	name, err := safeRelPath(hdr.Name)
	if err != nil {
		return fmt.Errorf("archive entry %q: %v", hdr.Name, err)
	}
	switch hdr.Typeflag {
	case tar.TypeReg, tar.TypeDir:
		// fine
	case tar.TypeSymlink:
		if err := linkTargetStaysInside(name, hdr.Linkname, true); err != nil {
			return fmt.Errorf("archive symlink %q -> %q: %v", hdr.Name, hdr.Linkname, err)
		}
	case tar.TypeLink:
		if err := linkTargetStaysInside(name, hdr.Linkname, false); err != nil {
			return fmt.Errorf("archive hardlink %q -> %q: %v", hdr.Name, hdr.Linkname, err)
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
		return os.MkdirAll(target, 0o755)
	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		// Perm() keeps rwx bits only — setuid/setgid/sticky never
		// survive extraction. Owner rwx is forced so the app user can
		// always read (and re-deploys can delete) what it shipped.
		mode := hdr.FileInfo().Mode().Perm() | 0o600
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, r)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		return err
	case tar.TypeSymlink:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.Symlink(hdr.Linkname, target)
	case tar.TypeLink:
		linkSrc, err := safeRelPath(hdr.Linkname)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.Link(filepath.Join(destDir, filepath.FromSlash(linkSrc)), target)
	default:
		return fmt.Errorf("unsupported type %q reached extraction", hdr.Typeflag)
	}
}

// safeRelPath normalizes an archive path and rejects absolute paths
// and any `..` traversal. Returned paths are slash-separated and
// relative to the archive root.
func safeRelPath(name string) (string, error) {
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
