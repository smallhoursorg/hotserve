package liveswap

import (
	"slices"
	"sync"
)

// BackupSource is a file a backup declaration was read from: an app's
// `app` line, its `backup` block and each of the block's lines — or, with
// no App, the liveswap `root` the apps are under. The file is the
// Caddyfile's own word for it, after imports, snippets and {$NAME}: an
// imported file's path, absolute; a snippet's, the file the snippet is
// defined in; the main Caddyfile's as the adapter was given it.
//
// A backup run reads the Caddyfile in a view that holds its directory
// and nothing else, as an account that owns nothing there; hotserve's
// validate and reload hold each of these files to that.
type BackupSource struct {
	App  string
	File string
}

var (
	backupSourcesMu sync.Mutex
	backupSources   []BackupSource
)

func setBackupSources(s []BackupSource) {
	backupSourcesMu.Lock()
	defer backupSourcesMu.Unlock()
	backupSources = s
}

// ClearBackupSources forgets what was read, before a Caddyfile is
// adapted: one with no liveswap block at all leaves the record alone.
func ClearBackupSources() { setBackupSources(nil) }

// BackupSources are the files the liveswap block this process last
// read from a Caddyfile took its backup declarations from, in the order
// read, repeats included; none when no app declares a backup.
func BackupSources() []BackupSource {
	backupSourcesMu.Lock()
	defer backupSourcesMu.Unlock()
	return slices.Clone(backupSources)
}
