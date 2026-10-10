package liveswap

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
)

// appState is what survives a Caddy restart: enough to reattach to, or
// relaunch, the current version.
type appState struct {
	CurrentVersion string `json:"current_version"`
	// Nonce identifies the instance: it names the unit (Handle.Unit
	// carries it too) and the socket, appDirs.socket(Nonce), which is
	// derived at load rather than recorded so the two cannot disagree.
	Nonce     string      `json:"nonce"`
	Handle    handleState `json:"handle"`
	UpdatedAt time.Time   `json:"updated_at"`
}

// stateStore persists appState; an interface so pipeline unit tests
// can run against an in-memory fake.
type stateStore interface {
	load() (appState, bool, error) // bool: state file exists
	save(appState) error
}

// fileStateStore keeps state.json next to the app's releases, written
// and read by the functions the deploy records are (ownfile.go): the
// app dir was the app's to write before sandboxing existed, and a link
// planted then — at state.json, or at the temp name an earlier
// hotserve wrote through — is not followed. The app dir itself is not
// checked here (ownfile.go says why). A state.json that is a
// link, a FIFO or anything but a regular file, or larger than any
// state hotserve writes, is an error like a corrupt one, which
// recovery never silently resets (ensureRunning).
type fileStateStore struct {
	path string
}

// stateMaxBytes bounds a state read with the margin a record's bound
// has over a record (deployRecordMaxBytes: 1 MiB over an 8 KiB tail):
// the largest state hotserve writes — a version, a nonce, a unit name
// built from the two and the app's name, two timestamps — is under
// 512 bytes, so the bound refuses nothing hotserve wrote.
const stateMaxBytes = 64 << 10

// stateTempPattern names save's temp file (writeOwnFile).
const stateTempPattern = ".state-*.tmp"

func (s *fileStateStore) load() (appState, bool, error) {
	var st appState
	data, _, err := readOwnFile(s.path, stateMaxBytes, "a state file")
	if errors.Is(err, fs.ErrNotExist) {
		return st, false, nil
	}
	if err != nil {
		return st, false, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, false, fmt.Errorf("corrupt state file %s: %w", s.path, err)
	}
	return st, true, nil
}

// save writes atomically (temp file + rename) so a crash mid-write
// never leaves a truncated state file, and never through a link
// (writeOwnFile).
func (s *fileStateStore) save(st appState) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	s.removeLeftovers()
	return writeOwnFile(s.path, stateTempPattern, data)
}

// removeLeftovers removes the temp files an interrupted save leaves
// behind: save's own, and state.json.tmp, the fixed name an earlier
// hotserve wrote through — on a box upgraded from one it may still be
// there, or be a link planted where it was. os.Remove unlinks the
// name, never what a link points at. Only these names: the app dir
// holds temp names that are not save's (persistState's current.tmp).
// Saves are serialized per app — every caller of persistState holds
// deployMu — so no save is in flight to lose its temp file here. Best
// effort: a leftover that cannot be removed costs a little disk, never
// a save.
func (s *fileStateStore) removeLeftovers() {
	_ = os.Remove(s.path + ".tmp") // absent is the usual case; best effort, as above
	dir := filepath.Dir(s.path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // the write that follows reports a dir it cannot use
	}
	prefix, suffix, _ := strings.Cut(stateTempPattern, "*")
	for _, e := range entries {
		if n := e.Name(); !e.IsDir() && strings.HasPrefix(n, prefix) && strings.HasSuffix(n, suffix) {
			_ = os.Remove(filepath.Join(dir, n)) // best effort, as above
		}
	}
}

var _ stateStore = (*fileStateStore)(nil)

// listReleases returns the on-disk release versions, newest-first, so
// the status endpoint can tell an operator what is available to roll
// back to. Best-effort: a read error yields nil and status still
// renders. A release is a directory named as a version, nothing else
// (gcReleases enumerates more loosely: it also removes orphans).
func listReleases(releasesDir string) []string {
	entries, err := os.ReadDir(releasesDir)
	if err != nil {
		return nil
	}
	type rel struct {
		name    string
		modTime time.Time
	}
	var rels []rel
	for _, e := range entries {
		// A directory whose name is not a version is not a release:
		// nothing could deploy it, roll back to it, or record it
		// (redact.go, rule 4). Dotfiles and staging dirs are among
		// what the alphabet refuses.
		if !e.IsDir() || !validVersion(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		rels = append(rels, rel{e.Name(), info.ModTime()})
	}
	sort.Slice(rels, func(i, j int) bool { return rels[i].modTime.After(rels[j].modTime) })
	out := make([]string, len(rels))
	for i, r := range rels {
		out[i] = r.name
	}
	return out
}

// gcReleases prunes the releases directory down to the newest keep
// entries by modification time (extraction time = deploy order, which
// is robust against arbitrary version naming schemes). The protected
// version — the one currently serving — is never deleted regardless of
// age. Failures are logged, not fatal: GC must never break a deploy
// that already succeeded.
func gcReleases(releasesDir string, keep int, protect string, logger *zap.Logger) {
	entries, err := os.ReadDir(releasesDir)
	if err != nil {
		logger.Warn("release GC: cannot list releases", zap.Error(err))
		return
	}
	type rel struct {
		name    string
		modTime time.Time
	}
	var rels []rel
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), ".extract-") {
			// A staging dir still present here is a crash orphan:
			// deploys are serialized per app (deployMu), and the deploy
			// running this GC renamed its own staging away before the
			// call. Left alone they accumulate forever, invisible —
			// hidden by the same dot-prefix that exempts them below.
			if err := os.RemoveAll(filepath.Join(releasesDir, e.Name())); err != nil {
				logger.Warn("release GC: cannot remove orphaned staging dir",
					zap.String("dir", e.Name()), zap.Error(err))
			} else {
				logger.Info("release GC: removed orphaned staging dir",
					zap.String("dir", e.Name()))
			}
			continue
		}
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue // dotfile strays are not ours to manage
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		rels = append(rels, rel{e.Name(), info.ModTime()})
	}
	sort.Slice(rels, func(i, j int) bool { return rels[i].modTime.After(rels[j].modTime) })
	for i, r := range rels {
		if i < keep || r.name == protect {
			continue
		}
		if err := os.RemoveAll(filepath.Join(releasesDir, r.name)); err != nil {
			logger.Warn("release GC: cannot remove old release",
				zap.String("version", r.name), zap.Error(err))
			continue
		}
		logger.Info("release GC: removed old release", zap.String("version", r.name))
	}
}
