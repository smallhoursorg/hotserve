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

// A deploy's outcome used to live in two places: the response the
// deployer got, and last_deploy in the status until the next deploy
// replaced it. The record of why v1.4.1 failed last Tuesday was gone
// by Wednesday. Now every deploy writes its result to
// <app>/deploys/<version>.json — the latest outcome of that version,
// a rollback included — outside every app's sandbox view (beside
// proxy/, never inside a release dir, which the app can write), and
// GET /<app>?deploy=<version> reads it back.
//
// The file holds the result as the response filter left it (redact.go),
// with the env_file values known at the time already replaced: a
// secret rotated later is not in an old record for the new filter to
// miss. Reading passes the filter again, as every body does.
//
// Records are pruned with the releases: one is kept for every version
// still on disk, plus the newest `keep` others — failed deploys, whose
// release is removed at once, and versions release GC has pruned — so
// the directory is bounded by 2×keep whatever the deploy rate.

// deploySummary is a record's outcome as the status lists it. The
// times are carried as the record has them, not parsed: a record is
// the filter's output, and a filter that replaced a value the shape
// of a timestamp must not make the record vanish from the list. The
// list is ordered by the record's write time instead.
type deploySummary struct {
	Version    string          `json:"version"`
	Status     string          `json:"status"`
	Phase      string          `json:"phase,omitempty"` // where it failed
	By         string          `json:"deployed_by,omitempty"`
	StartedAt  json.RawMessage `json:"started_at,omitempty"`
	FinishedAt json.RawMessage `json:"finished_at,omitempty"`
}

// recordDeploy writes result's filtered form as the record of its
// version, then prunes. Failures are logged, not fatal: the deploy's
// outcome is already known and the response must say it.
func (ma *managedApp) recordDeploy(c collaborators, result deployResult) {
	dir := c.spec.dirs.deploys
	raw, err := json.Marshal(result)
	if err != nil {
		c.logger.Warn("deploy record: cannot encode", zap.Error(err))
		return
	}
	filtered := ma.redactorFor(ma.status()).redactJSON(raw)
	if err := writeDeployRecord(dir, result.Version, []byte(filtered)); err != nil {
		c.logger.Warn("deploy record: cannot write", zap.String("version", result.Version), zap.Error(err))
		return
	}
	// The protection set is read strictly: a releases dir that cannot
	// be listed is no reason to prune, since an on-disk release would
	// then look like an "other" and lose its record.
	onDisk, err := releaseNames(c.spec.dirs.releases)
	if err != nil {
		c.logger.Warn("deploy record: releases unreadable, not pruning", zap.Error(err))
		return
	}
	pruneDeployRecords(dir, c.spec.keep, onDisk, c.logger)
}

// releaseNames is every release directory's name, or an error when the
// directory cannot be listed — unlike listReleases, which is
// best-effort and ordered, this is the set pruning must not miss.
func releaseNames(releasesDir string) ([]string, error) {
	entries, err := os.ReadDir(releasesDir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// writeDeployRecord writes the record atomically (temp file + rename),
// readable by the hotserve user and its group, like state.json.
func writeDeployRecord(dir, version string, filtered []byte) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	path := deployRecordPath(dir, version)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(filtered, '\n'), 0o640); err != nil { //nolint:gosec // group-readable on purpose: the administrator's group reads records as it reads state.json
		return err
	}
	return os.Rename(tmp, path)
}

func deployRecordPath(dir, version string) string {
	return filepath.Join(dir, versionPathComponent(version)+".json")
}

// errNoDeployRecord is a version no deploy has been recorded for.
var errNoDeployRecord = errors.New("no deploy recorded for this version")

// readDeployRecord returns the record's bytes — one JSON object, as
// written — or errNoDeployRecord.
func readDeployRecord(dir, version string) (json.RawMessage, error) {
	b, err := os.ReadFile(deployRecordPath(dir, version))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errNoDeployRecord
	}
	if err != nil {
		return nil, err
	}
	if !json.Valid(b) {
		return nil, fmt.Errorf("deploy record for %s is not JSON", version)
	}
	return json.RawMessage(b), nil
}

// listDeploySummaries reads every record's outcome, newest first by
// the record's write time. nil when there is no directory yet.
func listDeploySummaries(dir string) []deploySummary {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type dated struct {
		summary deploySummary
		written time.Time
	}
	var recs []dated
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // the app's own deploys dir, entries listed from it
		if err != nil {
			continue
		}
		var s deploySummary
		if json.Unmarshal(b, &s) != nil || s.Version == "" {
			continue // a record the filter withheld whole, or a stray
		}
		recs = append(recs, dated{s, info.ModTime()})
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].written.After(recs[j].written) })
	out := make([]deploySummary, len(recs))
	for i, r := range recs {
		out[i] = r.summary
	}
	return out
}

// pruneDeployRecords keeps a record for every version still on disk
// and the newest keep others, by modification time, and removes the
// rest.
func pruneDeployRecords(dir string, keep int, onDisk []string, logger *zap.Logger) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	disk := make(map[string]bool, len(onDisk))
	for _, v := range onDisk {
		disk[versionPathComponent(v)+".json"] = true
	}
	type rec struct {
		name    string
		modTime time.Time
	}
	var others []rec
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json.tmp") {
			// A write interrupted between the temp file and the rename:
			// deploys are serialized per app, so no write is in flight
			// now, and the leftover would otherwise sit outside the
			// bound — like release GC's crashed staging dirs.
			if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
				logger.Info("deploy record: removed orphaned temp file", zap.String("file", e.Name()))
			}
			continue
		}
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || disk[e.Name()] {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		others = append(others, rec{e.Name(), info.ModTime()})
	}
	sort.Slice(others, func(i, j int) bool { return others[i].modTime.After(others[j].modTime) })
	for i, r := range others {
		if i < keep {
			continue
		}
		if err := os.Remove(filepath.Join(dir, r.name)); err != nil {
			logger.Warn("deploy record: cannot prune", zap.String("record", r.name), zap.Error(err))
			continue
		}
		logger.Info("deploy record: pruned", zap.String("record", r.name))
	}
}
