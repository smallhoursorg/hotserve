// Package nofollow opens a path beneath a directory without following
// a symbolic link anywhere in it — not the last component, not one on
// the way — and without leaving the directory.
//
// It is openat2(RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS) done by hand, one
// component at a time, each opened relative to the descriptor of the
// one before, so no name is ever walked twice. By hand because openat2
// is not there to call inside a unit with RestrictSUIDSGID=: systemd's
// filter cannot look inside the struct openat2 takes, so it answers
// ENOSYS [measured] — and this has to work both in the dump unit and in
// whatever unit the run itself is one day given.
package nofollow

import (
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// ErrLink is returned for a symbolic link anywhere in the path.
var ErrLink = errors.New("a symbolic link is in the way")

// Open opens rel beneath the directory dirfd with flags (O_CLOEXEC and
// O_NOFOLLOW are added). rel is a clean relative path; "." is the
// directory itself. "No such file" comes back as that, for the caller
// to tell absence from everything else.
func Open(dirfd int, rel string, flags int) (int, error) {
	fail := func(err error) (int, error) { return -1, &os.PathError{Op: "open", Path: rel, Err: err} }
	if rel == "" || strings.HasPrefix(rel, "/") {
		return fail(unix.EINVAL)
	}
	parts := strings.Split(rel, "/")
	cur, owned := dirfd, false
	closeCur := func() {
		if owned {
			_ = unix.Close(cur)
		}
	}
	for i, part := range parts {
		if part == ".." || part == "" || (part == "." && len(parts) > 1) {
			closeCur()
			return fail(unix.EINVAL) // not a clean path beneath the directory
		}
		last := i == len(parts)-1
		f := unix.O_PATH | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if last {
			f = flags | unix.O_NOFOLLOW | unix.O_CLOEXEC
		}
		next, err := unix.Openat(cur, part, f, 0)
		closeCur()
		if errors.Is(err, unix.ELOOP) {
			return -1, ErrLink // O_NOFOLLOW on a link, when not O_PATH
		}
		if err != nil {
			return fail(err)
		}
		// With O_PATH, O_NOFOLLOW opens the link itself instead of
		// failing, so what was opened is looked at.
		var st unix.Stat_t
		if err := unix.Fstat(next, &st); err != nil {
			_ = unix.Close(next)
			return fail(err)
		}
		switch {
		case st.Mode&unix.S_IFMT == unix.S_IFLNK:
			_ = unix.Close(next)
			return -1, ErrLink
		case !last && st.Mode&unix.S_IFMT != unix.S_IFDIR:
			_ = unix.Close(next)
			return fail(unix.ENOTDIR)
		}
		cur, owned = next, true
	}
	return cur, nil
}
