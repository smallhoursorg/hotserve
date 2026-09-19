// Package record is what a run leaves behind for whoever asks how the
// backups are doing: one JSON file, written whole and atomically by
// root, holding no secret and nothing an app chose except the paths it
// declared.
package record

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Class is how an app's backup ended, in the words a status report
// uses.
type Class string

const (
	// OK: restic exited 0, this run's own snapshot id was read, and
	// every declared item was found in that snapshot.
	OK Class = "ok"
	// Incomplete: a snapshot exists and something declared is not in
	// it, or restic said it could not read everything (exit 3).
	Incomplete Class = "incomplete"
	// Pending: the app has no data dir and has never been backed up —
	// declared, not yet deployed.
	Pending Class = "pending"
	// DataMissing: the app has no data dir and HAS been backed up
	// before. Never reported as pending.
	DataMissing Class = "data missing"
	// Failed: no snapshot was made.
	Failed Class = "failed"
	// NotAttempted: the repository refused an earlier app in this run
	// for a reason that is the same for every app.
	NotAttempted Class = "not attempted"
)

// Item is one declared path.
type Item struct {
	Kind   string `json:"kind"` // "sqlite" or "files"
	Path   string `json:"path"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Snapshot names a snapshot and when it was made.
type Snapshot struct {
	ID   string    `json:"id"`
	Time time.Time `json:"time"`
}

// App is one app's last run.
type App struct {
	Class  Class  `json:"class"`
	Detail string `json:"detail,omitempty"`
	// Looked is the directory that was looked at for the app's data: a
	// report that says "no data yet" says where.
	Looked   string    `json:"looked"`
	Snapshot *Snapshot `json:"snapshot,omitempty"`
	Items    []Item    `json:"items,omitempty"`
	// LastOK is the newest run that ended OK, carried from record to
	// record.
	LastOK *Snapshot `json:"last_ok,omitempty"`
}

// Status is the whole file.
type Status struct {
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	// Error is why the run could not look at any app at all.
	Error string          `json:"error,omitempty"`
	Root  string          `json:"root,omitempty"`
	Apps  map[string]*App `json:"apps"`
}

// Read returns the last status, or an empty one when no run has
// written any.
func Read(path string) (*Status, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // a constant path under the state dir, root-owned
	if errors.Is(err, fs.ErrNotExist) {
		return &Status{Apps: map[string]*App{}}, nil
	}
	if err != nil {
		return nil, err
	}
	s := new(Status)
	if err := json.Unmarshal(raw, s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if s.Apps == nil {
		s.Apps = map[string]*App{}
	}
	return s, nil
}

// Write replaces the file atomically; it is world-readable, which is
// why nothing secret goes in it.
func Write(path string, s *Status) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".status-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone already when the rename succeeded
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close() //nolint:errcheck,gosec // the write error is the one reported
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close() //nolint:errcheck,gosec // as above
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
