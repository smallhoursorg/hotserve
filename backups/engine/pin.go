package engine

import (
	"errors"
	"fmt"
	"os"

	"github.com/smallhoursorg/hotserve/backups/nofollow"
	"golang.org/x/sys/unix"
)

// A pin is a directory or file held by descriptor, so that what a unit
// is shown is what was found here and nothing an app can re-aim.
//
// A bind source is a path the manager resolves as root, following
// links. An app's declared path is the app's own to replace, while it
// runs, with a link to a sibling's data or to /etc — and the upload
// unit reads whatever it is shown. Checking the path and binding it
// afterwards only moves the race, and so does handing the manager
// /proc/<pid>/fd/<n>: it does not follow that link, it reads its text
// and walks the path again [measured: a pinned directory that was then
// deleted could not be bound].
//
// So the path is opened here without following any link, and bound
// here, by mount(2) from /proc/self/fd/<n> — which the kernel does
// follow, to the very directory that was opened — onto a mount point in
// the run's own directory, which only root can reach. That mount point
// is what the manager is given.
//
// O_PATH: the descriptor names the file and cannot read it.
type pin struct{ fd int }

// mountAt binds what is pinned onto target, a new directory (or, for a
// pinned file, a new empty file) only root can reach, and returns how
// to take it away again. Recursive, so that a disk the operator has
// mounted inside comes along.
func (p pin) mountAt(target string) (unmount func(), err error) {
	if p.isDir() {
		err = os.Mkdir(target, 0o700)
	} else {
		var f *os.File
		if f, err = os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err == nil { //nolint:gosec // a path in the run's own directory, made of a counter
			err = f.Close()
		}
	}
	if err != nil {
		return nil, err
	}
	if err := bindMount(fmt.Sprintf("/proc/self/fd/%d", p.fd), target); err != nil {
		return nil, fmt.Errorf("binding it: %w", err)
	}
	return func() { _ = unmountDetach(target) }, nil
}

// The two mount calls, as variables: a test that is not root stands
// something else in for them.
//
// What is bound is made private, the whole tree of it, before anything
// else is done with it and again before it is taken away. A recursive
// bind of a mount that is shared — and on a box systemd has booted the
// root mount is, though in a container it is not — is a peer of what
// it was bound from: unmounting a disk beneath the bind unmounts, by
// propagation, the disk beneath the app's own directory, under the
// live app [M71]. Private, the bind's mounts are its own, and taking
// them away takes nothing else. Before the unmount as well as after
// the bind: a mount a killed run of an earlier version left is swept
// by this one.
//
// And where it cannot be made private it is never detached as it is,
// which is the very thing making it private is for: it is unmounted
// plainly, which the kernel refuses while anything is mounted beneath
// it — so that what goes had nothing to take along — and is otherwise
// left where it is, and said, for a sweep that can do better.
var (
	bindMount = func(source, target string) error {
		if err := unix.Mount(source, target, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			return err
		}
		return unshared(target, private(target))
	}
	unmountDetach = func(target string) error {
		if err := private(target); err != nil {
			if plain := unix.Unmount(target, 0); plain == nil || errors.Is(plain, unix.EINVAL) {
				// Gone, with nothing beneath it; or no mount at all,
				// which the unmount says as it always did.
				return plain
			}
			return unshared(target, err)
		}
		return unix.Unmount(target, unix.MNT_DETACH)
	}
	private = func(target string) error { return unix.Mount("", target, "", unix.MS_REC|unix.MS_PRIVATE, "") }
)

// unshared is what becomes of a bind that could not be made private,
// and nil where it could.
func unshared(target string, err error) error {
	if err == nil {
		return nil
	}
	if unix.Unmount(target, 0) == nil {
		return fmt.Errorf("what was bound at %s could not be made private, and was unmounted: %w", target, err)
	}
	return fmt.Errorf("what was bound at %s could not be made private (%w), and is left mounted: taken away as it is, it would take with it what is mounted beneath what it was bound from", target, err)
}

func (p pin) close() { _ = unix.Close(p.fd) }

// identity is which file is pinned: its inode and whose it is. Read
// from the descriptor, so of the very file that is bound.
type identity struct {
	inode    uint64
	uid, gid uint32
}

func (p pin) identity() (identity, error) {
	var st unix.Stat_t
	if err := unix.Fstat(p.fd, &st); err != nil {
		return identity{}, err
	}
	return identity{inode: st.Ino, uid: st.Uid, gid: st.Gid}, nil
}

// owner is the uid that owns what is pinned, or -1.
func (p pin) owner() int {
	var st unix.Stat_t
	if unix.Fstat(p.fd, &st) != nil {
		return -1
	}
	return int(st.Uid)
}

// kind is what is pinned, in a word, when it is neither a file nor a
// directory; "" when it is one of those.
func (p pin) kind() string {
	var st unix.Stat_t
	if unix.Fstat(p.fd, &st) != nil {
		return "something that cannot be looked at"
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG, unix.S_IFDIR:
		return ""
	case unix.S_IFIFO:
		return "a fifo"
	case unix.S_IFSOCK:
		return "a socket"
	case unix.S_IFCHR, unix.S_IFBLK:
		return "a device"
	}
	return "a special file"
}

func (p pin) isDir() bool {
	var st unix.Stat_t
	return unix.Fstat(p.fd, &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFDIR
}

// errLink is what a symbolic link anywhere in a pinned path comes back
// as.
var errLink = errors.New("a symbolic link is in the way")

// pinRoot opens the liveswap root. Links are followed: the root is the
// operator's, from the Caddyfile, and liveswap lets it be an alias.
func pinRoot(root string) (pin, error) {
	fd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return pin{}, &os.PathError{Op: "open", Path: root, Err: err}
	}
	return pin{fd}, nil
}

// beneath opens rel under p, refusing any symbolic link on the way and
// anything that would leave p. "No such file" stays that, for the
// caller to tell absence from everything else.
func (p pin) beneath(rel string) (pin, error) {
	fd, err := nofollow.Open(p.fd, rel, unix.O_PATH)
	if errors.Is(err, nofollow.ErrLink) {
		return pin{}, errLink
	}
	if err != nil {
		return pin{}, err
	}
	return pin{fd}, nil
}
