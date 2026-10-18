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

// Release order. Retention (gcReleases) and the list an operator picks
// a rollback target from (listReleases, the status's
// available_versions) order releases newest first by deploy time, and
// must agree, so the key and the sort are written once, here.
//
// The key is an input to what retention deletes — the way back, which
// nothing else holds — so it is held to the deploy store's rule 1
// (deploys.go): nothing the app can write decides it. A release dir's
// modification time is not such a key: the release being started is
// bound read-write into its sandbox (sandbox.go), so its pre_start and
// its instance can set that dir's mtime to anything, future included.
// The key is the release's deploy record's write time — when hotserve
// last deployed or rolled back to that version, a failed attempt
// included — read off the open record (readOwnFile) in a directory no
// app's view holds, and only from a record that proves itself one.
// Two exceptions:
//
//   - The version the deploy running GC promoted is newest: it was
//     deployed now, and its record is written as the deploy returns.
//   - A release with no such record — deployed before records
//     existed, or its record could not be written — is placed by its
//     dir's mtime, and the status names it (ordered_by_mtime): that
//     one key is the app's to move.
//
// Deploy times that tie order by name, so the order is the same on
// every read.
type release struct {
	name    string
	at      time.Time
	byMtime bool // no record: at is the dir's mtime
}

// placeRelease is e's place in the order, from its record in records
// (the records dir, or "" when it is absent or refused), else from its
// dir's mtime; false when neither can be read.
func placeRelease(records string, e os.DirEntry) (release, bool) {
	if at, ok := recordWriteTime(records, e.Name()); ok {
		return release{name: e.Name(), at: at}, true
	}
	info, err := e.Info()
	if err != nil {
		return release{}, false
	}
	return release{name: e.Name(), at: info.ModTime(), byMtime: true}, true
}

// newestFirst sorts rels by deploy time, first (when not "") ahead of
// every other.
func newestFirst(rels []release, first string) {
	sort.Slice(rels, func(i, j int) bool {
		if (rels[i].name == first) != (rels[j].name == first) {
			return rels[i].name == first
		}
		if !rels[i].at.Equal(rels[j].at) {
			return rels[i].at.After(rels[j].at)
		}
		return rels[i].name > rels[j].name
	})
}

// readableRecordsDir is the records dir for placing releases, or ""
// when there is none to read: absent before the first record, or
// refused as a planted link (recordsDir). Every release is then placed
// by its dir's mtime, and named so.
func readableRecordsDir(d appDirs) string {
	dir, err := recordsDir(d, false)
	if err != nil {
		return ""
	}
	return dir
}

// listReleases returns the on-disk release versions newest first (the
// order above), so the status endpoint can tell an operator what is
// available to roll back to, and byMtime, those of them placed by
// their dir's mtime, in the same order. Best-effort: a read error
// yields nil and status still renders. A release is a directory named
// as a version, nothing else (gcReleases enumerates more loosely: it
// also removes orphans).
func listReleases(d appDirs) (versions, byMtime []string) {
	entries, err := os.ReadDir(d.releases)
	if err != nil {
		return nil, nil
	}
	records := readableRecordsDir(d)
	var rels []release
	for _, e := range entries {
		// A directory whose name is not a version is not a release:
		// nothing could deploy it, roll back to it, or record it
		// (redact.go, rule 4). Dotfiles and staging dirs are among
		// what the alphabet refuses.
		if !e.IsDir() || !validVersion(e.Name()) {
			continue
		}
		if r, ok := placeRelease(records, e); ok {
			rels = append(rels, r)
		}
	}
	newestFirst(rels, "")
	versions = make([]string, len(rels))
	for i, r := range rels {
		versions[i] = r.name
		if r.byMtime {
			byMtime = append(byMtime, r.name)
		}
	}
	return versions, byMtime
}

// gcReleases prunes the releases directory down to the newest keep
// releases in the order above: protect — the version this deploy
// promoted, now serving — first, so it is never deleted and holds one
// of the keep places. Failures are logged, not fatal: GC must never
// break a deploy that already succeeded.
func gcReleases(d appDirs, keep int, protect string, logger *zap.Logger) {
	entries, err := os.ReadDir(d.releases)
	if err != nil {
		logger.Warn("release GC: cannot list releases", zap.Error(err))
		return
	}
	records := readableRecordsDir(d)
	var rels []release
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), ".extract-") {
			// A staging dir still present here is a crash orphan:
			// deploys are serialized per app (deployMu), and the deploy
			// running this GC renamed its own staging away before the
			// call. Left alone they accumulate forever, invisible —
			// hidden by the same dot-prefix that exempts them below.
			if err := os.RemoveAll(filepath.Join(d.releases, e.Name())); err != nil {
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
		if r, ok := placeRelease(records, e); ok {
			rels = append(rels, r)
		}
	}
	newestFirst(rels, protect)
	for i, r := range rels {
		if i < keep || r.name == protect {
			continue
		}
		placedBy := "deploy record"
		if r.byMtime {
			placedBy = "mtime"
		}
		if err := os.RemoveAll(filepath.Join(d.releases, r.name)); err != nil {
			logger.Warn("release GC: cannot remove old release",
				zap.String("version", r.name), zap.String("placed_by", placedBy), zap.Error(err))
			continue
		}
		logger.Info("release GC: removed old release", zap.String("version", r.name), zap.String("placed_by", placedBy))
	}
}
