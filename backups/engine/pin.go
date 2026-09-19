package engine

import (
	"errors"
	"fmt"
	"os"

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
var (
	bindMount     = func(source, target string) error { return unix.Mount(source, target, "", unix.MS_BIND|unix.MS_REC, "") }
	unmountDetach = func(target string) error { return unix.Unmount(target, unix.MNT_DETACH) }
)

func (p pin) close() { _ = unix.Close(p.fd) }

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
	fd, err := unix.Openat2(p.fd, rel, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	switch {
	case errors.Is(err, unix.ELOOP):
		return pin{}, errLink
	case err != nil:
		return pin{}, &os.PathError{Op: "open", Path: rel, Err: err}
	}
	return pin{fd}, nil
}
