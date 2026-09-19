package engine

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// A pin is a directory or file held by descriptor, so that it can be
// named to the manager as a bind source that no one can re-aim.
//
// A bind source is a path the manager resolves as root, following
// links. An app's declared path is the app's own to replace, while it
// runs, with a link to a sibling's data or to /etc — and the upload
// unit reads whatever it is shown [measured: it read the sibling's
// file]. Checking the path first and binding it afterwards only moves
// the race. So the path is opened here without following any link, and
// what the manager is given is /proc/<this pid>/fd/<n>: the thing that
// was opened, whatever its name points at by then [measured].
//
// O_PATH: the descriptor names the file and cannot read it. Root still
// opens nothing an app wrote.
type pin struct{ fd int }

// source is the pin as a bind source, good for as long as it is open.
func (p pin) source() string { return fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), p.fd) }

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
