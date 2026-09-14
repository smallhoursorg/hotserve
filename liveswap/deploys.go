package liveswap

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
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
//
// Nothing here follows a symlink. The app dir was writable by the app
// before sandboxing existed, and a link planted then — `deploys ->
// ..` so that a version named "state" rewrites state.json, a record
// name pointing at another file to be served or to forge a summary —
// is exactly what the first use after an upgrade must not follow, as
// resolveBindSources (sandbox.go) says of the bind sources. The
// directory must be a directory, a record must be a regular file, a
// temp file is created fresh under a random name, and a summary's
// version must be the name it was read from.

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
	dir, err := deploysDir(c.spec.dirs)
	if err != nil {
		c.logger.Warn("deploy record: not written", zap.Error(err))
		return
	}
	raw, err := json.Marshal(result)
	if err != nil {
		c.logger.Warn("deploy record: cannot encode", zap.Error(err))
		return
	}
	filtered := ma.recordRedactor(c, result).redactJSON(raw)
	if err := writeDeployRecord(c.spec.dirs, result.Version, []byte(filtered)); err != nil {
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

// recordRedactor is the filter a record is written through: the
// deploy's own spec and the values the filter knows — every env_file
// value seen at any launch, this deploy's included (rememberSecrets
// ran before it launched) — with no withholding. The response filter
// withholds while the *live* env_file cannot be read, because the
// running app's values would be unknown; a record is about a deploy
// that is over, and a reload to an unreadable env_file while it ran
// must not turn its outcome into a placeholder that nothing can
// recover once the file is fixed.
func (ma *managedApp) recordRedactor(c collaborators, result deployResult) *redactor {
	ma.secretsMu.Lock()
	kvs := append([]string(nil), ma.secrets...)
	ma.secretsMu.Unlock()
	safe := []string{ma.name, result.Version}
	if c.spec != nil {
		safe = append(safe, c.spec.dirs.root, c.spec.dirs.app, c.spec.dirs.releases, c.spec.dirs.shared, c.spec.dirs.run)
		// Release names are read off the filesystem: one equal to a
		// known value must not exempt it (namesNotValues).
		safe = append(safe, namesNotValues(kvs, listReleases(c.spec.dirs.releases))...)
	}
	return newRedactor(kvs, safe)
}

// deploysDir makes sure the records directory is the directory it
// names — created if absent, refused if a link or anything else
// stands in its place, or if any ancestor is a link that lands it
// elsewhere: `<root>/blog -> <root>/shop` would have blog's deploys
// list, serve and prune shop's records. The check is the bind
// sources' (resolveBindSources): canonical against canonical, with
// an alias on the liveswap root itself the one difference allowed.
func deploysDir(d appDirs) (string, error) {
	if err := os.MkdirAll(d.deploys, 0o750); err != nil {
		return "", err
	}
	fi, err := os.Lstat(d.deploys)
	if err != nil {
		return "", err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory (a planted link is not followed)", d.deploys)
	}
	rootC := d.root
	if c, err := filepath.EvalSymlinks(d.root); err == nil {
		rootC = c
	}
	got, err := filepath.EvalSymlinks(d.deploys)
	if err != nil {
		return "", err
	}
	if want := filepath.Join(rootC, filepath.Base(d.app), "deploys"); got != want {
		return "", fmt.Errorf("%s resolves to %s, not %s (a planted link is not followed)", d.deploys, got, want)
	}
	return d.deploys, nil
}

// writeDeployRecord writes the record atomically: a fresh temp file
// under a random name (never a path something could have planted),
// then a rename over the record's name. Readable by the hotserve user
// and its group, like state.json.
func writeDeployRecord(d appDirs, version string, filtered []byte) (err error) {
	dir, err := deploysDir(d)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".record-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(append(filtered, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Chmod(0o640); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, deployRecordPath(dir, version))
}

// deployRecordMaxBytes bounds a record read: one holds a bounded
// result (an 8 KiB tail at most); anything larger is not one.
const deployRecordMaxBytes = 1 << 20

// openRecord opens a record without following a link and reads it
// whole, refusing anything that is not a regular file of a record's
// size; the modification time comes from the same open file as the
// bytes, so a record replaced between two lookups cannot pair one
// outcome with another's time.
func openRecord(path string) ([]byte, time.Time, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0) //nolint:gosec // a path under the app's own deploys dir, built here
	if err != nil {
		return nil, time.Time{}, err
	}
	defer f.Close() //nolint:errcheck // read-only
	fi, err := f.Stat()
	if err != nil {
		return nil, time.Time{}, err
	}
	if !fi.Mode().IsRegular() {
		return nil, time.Time{}, fmt.Errorf("%s is not a regular file", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, deployRecordMaxBytes+1))
	if err != nil {
		return nil, time.Time{}, err
	}
	if len(b) > deployRecordMaxBytes {
		return nil, time.Time{}, fmt.Errorf("%s is larger than a record can be", path)
	}
	return b, fi.ModTime(), nil
}

func deployRecordPath(dir, version string) string {
	return filepath.Join(dir, versionPathComponent(version)+".json")
}

// errNoDeployRecord is a version no deploy has been recorded for.
var errNoDeployRecord = errors.New("no deploy recorded for this version")

// readDeployRecord returns the record's bytes — one JSON object, as
// written — or errNoDeployRecord.
func readDeployRecord(d appDirs, version string) (json.RawMessage, error) {
	dir, err := deploysDir(d)
	if err != nil {
		return nil, err
	}
	b, _, err := openRecord(deployRecordPath(dir, version))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errNoDeployRecord
	}
	if err != nil {
		return nil, err
	}
	// The record must be an object naming the version it was asked
	// for: a stale, planted or corrupted file under the name is not
	// the version's record.
	var head struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(b, &head); err != nil || head.Version != version {
		return nil, fmt.Errorf("deploy record for %s is not that version's record", version)
	}
	return json.RawMessage(b), nil
}

// listDeploySummaries reads every record's outcome, newest first by
// the record's write time. nil when there is no directory yet.
func listDeploySummaries(d appDirs) []deploySummary {
	dir, err := deploysDir(d)
	if err != nil {
		return nil
	}
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
		b, written, err := openRecord(filepath.Join(dir, e.Name()))
		if err != nil {
			continue // a link, or not a record
		}
		var s deploySummary
		// The version is the name the record was read from, or the
		// record is not one of ours. (A file planted under a name equal
		// to a secret's value would pass this; it is the response
		// filter that never lets a recorded version equal to a known
		// value into its safe list — redactorFor.)
		if json.Unmarshal(b, &s) != nil || !validVersion(s.Version) || s.Version+".json" != e.Name() {
			continue // a record the filter withheld whole, or a stray
		}
		recs = append(recs, dated{s, written})
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
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".tmp") {
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
		// Only a record of ours holds a retention slot: a link, a file
		// that is not JSON, or a version that is not its name would
		// otherwise crowd out the records the slots are for. Such an
		// entry is not ours to keep either; it goes.
		b, written, err := openRecord(filepath.Join(dir, e.Name()))
		var head struct {
			Version string `json:"version"`
		}
		if err != nil || json.Unmarshal(b, &head) != nil || !validVersion(head.Version) || head.Version+".json" != e.Name() {
			if rmErr := os.Remove(filepath.Join(dir, e.Name())); rmErr == nil {
				logger.Info("deploy record: removed an entry that is not a record", zap.String("file", e.Name()))
			}
			continue
		}
		others = append(others, rec{e.Name(), written})
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
