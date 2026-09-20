// Package record is what a run leaves behind for whoever asks how the
// backups are doing: one JSON file, written whole and atomically by
// root, holding no secret. What it holds of an app's choosing — the
// paths it declared, and what sqlite3 said about its databases — has
// been through Text.
package record

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
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
	// NotRun: the run ended before it reached this app. Nothing is said
	// about it but when it was last ok.
	NotRun Class = "not run"
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
	// Seen is when the repository was last found to hold this snapshot:
	// by the run that made it, a drill that fetched it, or a run's
	// listing. One older than Status.Listed — or none, under a Listed —
	// is of a snapshot a listing since did not hold: it is gone.
	Seen *time.Time `json:"seen,omitempty"`
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
	// LastOK is the newest run that ended OK, and LastSnapshot the
	// newest that made a snapshot at all, each carried from record to
	// record. An app with a LastSnapshot has been backed up: its data
	// going missing is never "not deployed yet".
	LastOK       *Snapshot `json:"last_ok,omitempty"`
	LastSnapshot *Snapshot `json:"last_snapshot,omitempty"`
	// RestoreProven is the last restore drill that fetched a snapshot,
	// handed it over and read it whole; RestoreDrill is the last drill
	// that proved nothing, until one does. Both are carried from record
	// to record: a backup run does not unprove a restore.
	RestoreProven *Drill `json:"restore_proven,omitempty"`
	RestoreDrill  *Drill `json:"restore_drill,omitempty"`
}

// Drill is one restore drill of one snapshot: when, and — where it
// proved nothing — why.
type Drill struct {
	Snapshot Snapshot  `json:"snapshot"`
	Time     time.Time `json:"time"`
	Detail   string    `json:"detail,omitempty"`
}

// Status is the whole file.
type Status struct {
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	// Error is why the run ended early; the apps it did not reach are
	// NotRun.
	Error string `json:"error,omitempty"`
	// Warning is something the run got past and a person should know.
	Warning string          `json:"warning,omitempty"`
	Root    string          `json:"root,omitempty"`
	Apps    map[string]*App `json:"apps"`
	// LastDrill is when a drill last ran, and — where it could not
	// drill anything — why; each app's own verdict is on the app.
	LastDrill *Drill `json:"last_drill,omitempty"`
	// Listed is when a run last listed the repository and was answered.
	// A listing that failed leaves it, and every Seen, as they were.
	Listed *time.Time `json:"listed,omitempty"`
	// Unlisted is since when every listing a run tried has gone
	// unanswered, or been answered in a way not to be believed; none,
	// once one is answered.
	Unlisted *time.Time `json:"unlisted,omitempty"`
}

// Text makes a string from a unit fit to print and to keep: no control
// characters (a terminal reads escapes in them), and no longer than a
// line. A unit's words are the app's words, where the unit handled the
// app's bytes.
func Text(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	if r := []rune(s); len(r) > 300 {
		s = string(r[:300]) + "…"
	}
	return strings.TrimSpace(s)
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
	// To the disk before it takes the old one's place, and the rename
	// after it: a power cut leaves the old record or the new, not an
	// empty file.
	if err := tmp.Sync(); err != nil {
		tmp.Close() //nolint:errcheck,gosec // as above
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close() //nolint:errcheck // read-only
	return dir.Sync()
}
