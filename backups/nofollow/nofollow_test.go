package nofollow

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestOpen(t *testing.T) {
	base, outside := t.TempDir(), t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(base, "a", "b"), 0o755))
	must(os.WriteFile(filepath.Join(base, "a", "b", "f"), []byte("inside"), 0o644))
	must(os.WriteFile(filepath.Join(outside, "f"), []byte("OUTSIDE"), 0o644))
	must(os.Symlink("f", filepath.Join(base, "a", "b", "link-last")))
	must(os.Symlink("b", filepath.Join(base, "a", "link-mid")))
	must(os.Symlink(outside, filepath.Join(base, "a", "link-out")))
	must(os.Symlink(filepath.Join(outside, "f"), filepath.Join(base, "link-abs")))

	dir, err := unix.Open(base, unix.O_PATH|unix.O_DIRECTORY, 0)
	must(err)
	defer unix.Close(dir) //nolint:errcheck // a path descriptor

	for rel, want := range map[string]error{
		"a/b/f":         nil,
		"a/b":           nil,
		".":             nil,
		"a/b/link-last": ErrLink,
		"a/link-mid/f":  ErrLink, // a link on the way, though it stays inside
		"a/link-out/f":  ErrLink,
		"link-abs":      ErrLink,
		"a/b/missing":   fs.ErrNotExist,
		"missing/f":     fs.ErrNotExist,
		"a/b/f/under":   unix.ENOTDIR,
		"../x":          unix.EINVAL,
		"a/../a/b/f":    unix.EINVAL,
		"/etc/passwd":   unix.EINVAL,
		"":              unix.EINVAL,
	} {
		for name, flags := range map[string]int{"to read": unix.O_RDONLY | unix.O_NONBLOCK, "as a path": unix.O_PATH} {
			fd, err := Open(dir, rel, flags)
			if !errors.Is(err, want) || (want == nil && err != nil) {
				t.Errorf("%q %s: %v, want %v", rel, name, err, want)
			}
			if err == nil {
				if rel == "a/b/f" && flags&unix.O_PATH == 0 {
					buf := make([]byte, 16)
					n, _ := unix.Read(fd, buf)
					if string(buf[:n]) != "inside" {
						t.Errorf("read %q", buf[:n])
					}
				}
				_ = unix.Close(fd)
			}
		}
	}
}
