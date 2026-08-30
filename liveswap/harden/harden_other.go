//go:build !linux

// Package harden makes the process that imports it non-dumpable on
// Linux; see harden_linux.go.
package harden

// Process is a no-op off Linux: servers are Linux, and the same-UID
// /proc routes it closes there do not exist elsewhere. (No init
// either: nothing to harden.)
func Process() error { return nil }
