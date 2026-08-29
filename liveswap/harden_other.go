//go:build !linux

package liveswap

// HardenProcess is a no-op off Linux: servers are Linux, and the
// same-UID /proc routes it closes there do not exist elsewhere.
func HardenProcess() error { return nil }
