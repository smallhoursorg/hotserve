package liveswap

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
)

// appState is what survives a Caddy restart: enough to relaunch (or,
// with a future systemd runner, reattach to) the current version.
type appState struct {
	CurrentVersion string      `json:"current_version"`
	Port           int         `json:"port"`
	Handle         handleState `json:"handle"`
	UpdatedAt      time.Time   `json:"updated_at"`
}

// stateStore persists appState; an interface so pipeline unit tests
// can run against an in-memory fake.
type stateStore interface {
	load() (appState, bool, error) // bool: state file exists
	save(appState) error
}

// fileStateStore keeps state.json next to the app's releases.
type fileStateStore struct {
	path string
}

func (s *fileStateStore) load() (appState, bool, error) {
	var st appState
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
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
// never leaves a truncated state file.
func (s *fileStateStore) save(st appState) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

var _ stateStore = (*fileStateStore)(nil)

// listReleases returns the on-disk release versions, newest-first, so
// the status endpoint can tell an operator what is available to roll
// back to. Best-effort: a read error yields nil and status still
// renders. Same enumeration rules as gcReleases (skip dotfiles and
// staging dirs).
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
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
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
