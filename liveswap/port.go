package liveswap

import (
	"net"
	"strconv"
)

// freePort asks the kernel for an unused localhost port. The listener
// is closed before the app starts, so there is a tiny window in which
// another process could grab the port; on a single-operator VPS this
// is acceptable and the deploy simply fails loudly if it ever happens.
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		return 0, err
	}
	return port, nil
}

// portString formats a port for PORT env and dial addresses.
func portString(port int) string { return strconv.Itoa(port) }
