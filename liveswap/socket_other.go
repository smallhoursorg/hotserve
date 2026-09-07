//go:build !linux

package liveswap

import "errors"

// Apps run only on Linux (systemd); elsewhere a socket is never dialled.
func linkSocket(string, string) error {
	return errors.New("pinning a unix socket by inode is Linux-only")
}
