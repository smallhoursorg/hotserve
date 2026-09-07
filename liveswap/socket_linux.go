package liveswap

import (
	"fmt"
	"strconv"

	"golang.org/x/sys/unix"
)

// linkSocket hard-links the socket file at src to dst without ever
// following a symlink at src. The app owns run/, so src is an
// app-controlled name and connect(2) would follow whatever it points
// at — a symlink to hotserve's admin socket, or to a sibling's — in
// hotserve's own namespace. An O_PATH|O_NOFOLLOW open hands back a
// symlink as itself, which the S_IFSOCK check refuses; a real socket
// is then linked by inode into a directory only hotserve writes, and
// what the app does to its own name afterwards changes nothing about
// what dst reaches.
func linkSocket(src, dst string) error {
	fd, err := unix.Open(src, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return fmt.Errorf("refusing to dial %s: it is %s, not a socket", src, fileKind(st.Mode))
	}
	// The magic link names the inode the descriptor holds, so the
	// link is made from what was verified, not from a second lookup
	// of the name.
	return unix.Linkat(unix.AT_FDCWD, "/proc/self/fd/"+strconv.Itoa(fd), unix.AT_FDCWD, dst, unix.AT_SYMLINK_FOLLOW)
}

func fileKind(mode uint32) string {
	switch mode & unix.S_IFMT {
	case unix.S_IFLNK:
		return "a symlink"
	case unix.S_IFDIR:
		return "a directory"
	case unix.S_IFREG:
		return "a regular file"
	default:
		return "something else"
	}
}
