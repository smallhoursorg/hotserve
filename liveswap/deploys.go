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
// a rollback included — and GET /<app>?deploy=<version> reads it
// back; the status lists each recorded version's outcome.
//
// The store is a trust boundary in both directions, and these are its
// rules; every function here holds them, and the filter's own rules
// (redact.go) hold the second and third.
//
//  1. A record is bytes hotserve wrote once, through the filter, into
//     a directory it verified as its own. Anything read back is text
//     from disk: it may be served only through the filter, and it is
//     trusted for nothing else — not as a name, not as a retention
//     slot — unless it proves itself a record: a regular file, under
//     a valid version name, whose object names that version.
//  2. Nothing from disk reaches a response except through the filter
//     as body text. A recorded version is a safe string like any
//     other name, and no safe string equals a known value (redact.go,
//     rule 1). Nothing is appended to a body after the filter's final
//     pass (withField merges what the filtered body already reports;
//     it adds nothing else).
//  3. Whatever the filter does to a body, the record on disk names
//     its version and outcome, from values that cannot carry source
//     data: the version the deployer named, vocabulary, timestamps.
//     The vocabulary (status, phase) stands outside the filter in
//     every body (redact.go, rule 3), so a value equal to a word of
//     it rewrites nothing; a record the filter leaves unreadable as
//     one, or too large to read back, is an envelope, never an
//     unreadable file.
//  4. The store never blocks a deploy or a status — a failure here is
//     a warning, a missing store is an empty list — and never follows
//     a link: the app dir was writable by the app before sandboxing
//     existed, and a link planted then (`deploys -> ..`, a record name
//     pointing elsewhere, an ancestor pointing at another app) is what
//     the first use after an upgrade must not follow, as the bind
//     sources' check says (resolveBindSources, sandbox.go).
//
// The file holds the result as the record filter left it, with the
// env_file values known at the time already replaced: a secret rotated
// later is not in an old record for the new filter to miss. Records
// are pruned with the releases: one is kept for every version still on
// disk, plus the newest `keep` others — failed deploys, whose release
// is removed at once, and versions release GC has pruned — so the
// directory is bounded by 2×keep whatever the deploy rate.

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
	dir, err := recordsDir(c.spec.dirs, true)
	if err != nil {
		c.logger.Warn("deploy record: not written", zap.Error(err))
		return
	}
	raw, err := json.Marshal(result)
	if err != nil {
		c.logger.Warn("deploy record: cannot encode", zap.Error(err))
		return
	}
	rd, unknown := ma.recordRedactor(c, result)
	filtered := rd.redactJSON(raw)
	// Rule 3's envelope. The filter's whole-body fallback — a known
	// value that is a fragment of the record's own JSON — leaves an
	// object that is not a record of this version (recordHead: no
	// version, or an outcome the filter reached); and a record larger
	// than a reader accepts would be written only to be refused.
	// Either way the record is the version the deployer named, the
	// outcome vocabulary, the times, and why the rest is missing —
	// nothing that could carry source data, so nothing the filter
	// would have had to see.
	why := ""
	switch _, isRecord := recordHead([]byte(filtered), result.Version+".json"); {
	case unknown != "":
		why = unknown
	case !isRecord && !rd.safeExact[result.Version]:
		// Redacted like the value it equals (redact.go, rule 1), so
		// the record could not name itself; the envelope does. (A
		// value too short to be a secret drops the version from the
		// safe list too, but redacts nothing: the record stands.)
		why = "the version equals an env_file value"
	case !isRecord:
		why = "a redacted value overlapped the record's own structure or outcome"
	case len(filtered)+1 > deployRecordMaxBytes: // +1: the newline the file ends with
		why = "larger than a record can be"
	}
	if why != "" {
		envelope := map[string]any{
			"version": result.Version, "status": result.Status,
			"started_at": result.StartedAt, "finished_at": result.FinishedAt,
			"error": "record withheld: " + why,
		}
		if result.Phase != "" { // absent, as a marshalled result has it, not ""
			envelope["phase"] = result.Phase
		}
		env, err := json.Marshal(envelope)
		if err != nil {
			c.logger.Warn("deploy record: cannot encode the envelope", zap.Error(err))
			return
		}
		filtered = string(env)
	}
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
// deploy's own spec and every value the filter knows — remembered at
// any launch, and read from the deploy's own env_file now. It does
// not withhold on the *live* env_file as the response filter does (a
// record is about a deploy that is over, and a reload to an
// unreadable file while it ran must not turn its outcome into a
// placeholder nothing can recover once the file is fixed); but when
// the deploy's own env_file is there and cannot be parsed whole, the
// values before the bad line are unknown to the filter and could be
// in the very error that says so — the second result names that, and
// the record is the envelope (rule 3). A file that is absent holds no
// values to know. Its safe strings are the names the response filter
// exempts: the version, the app's dirs, the releases on disk — and
// none equal to a known value (redact.go, rule 1).
func (ma *managedApp) recordRedactor(c collaborators, result deployResult) (*redactor, string) {
	ma.secretsMu.Lock()
	kvs := append([]string(nil), ma.secrets...)
	ma.secretsMu.Unlock()
	// The values remembered so far are those of every launch, and a
	// deploy that failed before its launch — a download that did not
	// complete — remembered nothing: read the deploy's own env_file
	// too, so what it holds now is known to the filter that writes
	// the record. Unreadable, the remembered values are all there is.
	unknown := ""
	if c.spec != nil && c.spec.envFile != "" {
		switch now, err := parseEnvFile(c.spec.envFile); {
		case err == nil:
			kvs = mergeSecrets(kvs, now)
		case !errors.Is(err, fs.ErrNotExist):
			unknown = "the deploy's env_file could not be read whole, so its values are unknown to the filter"
		}
	}
	safe := []string{ma.name, result.Version}
	if c.spec != nil {
		safe = append(safe, c.spec.dirs.root, c.spec.dirs.app, c.spec.dirs.releases, c.spec.dirs.shared, c.spec.dirs.run)
		safe = append(safe, listReleases(c.spec.dirs.releases)...)
	}
	return newRedactor(kvs, safe), unknown
}

// recordsDir is the records directory as the directory it names
// (rule 4): with create, made if absent; refused if a link or anything
// else stands in its place, or if any ancestor is a link that lands it
// elsewhere — `<root>/blog -> <root>/shop` would have blog's deploys
// list, serve and prune shop's records. The check is the bind
// sources': canonical against canonical (canonicalDeepest, so a root
// not yet on disk under a linked ancestor resolves as far as it can),
// with an alias on the liveswap root itself the one difference
// allowed. Without create — every read — a missing directory is
// fs.ErrNotExist and nothing is made: a status poll must not create
// an app's tree.
func recordsDir(d appDirs, create bool) (string, error) {
	rootC := canonicalDeepest(d.root)
	want := filepath.Join(rootC, filepath.Base(d.app), "deploys")
	// The app dir first, before anything is created through it: an
	// ancestor link would otherwise have MkdirAll make the directory
	// where the link points before the check below refused it.
	if got := canonicalDeepest(d.app); got != filepath.Dir(want) {
		return "", fmt.Errorf("%s resolves to %s (a planted link is not followed)", d.app, got)
	}
	if create {
		if err := os.MkdirAll(d.deploys, 0o750); err != nil {
			return "", err
		}
	}
	fi, err := os.Lstat(d.deploys)
	if err != nil {
		return "", err // fs.ErrNotExist when absent and not creating
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory (a planted link is not followed)", d.deploys)
	}
	if got := canonicalDeepest(d.deploys); got != want {
		return "", fmt.Errorf("%s resolves to %s, not %s (a planted link is not followed)", d.deploys, got, want)
	}
	return d.deploys, nil
}

// recordHead is what proves a file a record (rule 1): a valid version
// equal to the name it was read under. The rest of the object is body
// text for the filter.
func recordHead(b []byte, name string) (deploySummary, bool) {
	var s deploySummary
	if json.Unmarshal(b, &s) != nil || !validVersion(s.Version) || s.Version+".json" != name {
		return deploySummary{}, false
	}
	// And an outcome in the vocabulary: a status that is one, and a
	// phase — the one a failure reached — only on a failure, and one
	// of the vocabulary. The writer guarantees all of it (rule 3), so
	// a file without is not a record of ours.
	if !outcomeStatuses[s.Status] || (s.Phase != "" && (s.Status != "failed" || !outcomePhases[s.Phase])) {
		return deploySummary{}, false
	}
	// And its times strings, as the writer marshals them: the summary
	// carries them as they are into the status body, and a string is
	// the one value that re-encodes as text rather than structure.
	for _, raw := range []json.RawMessage{s.StartedAt, s.FinishedAt} {
		var str string
		if len(raw) != 0 && (string(raw) == "null" || json.Unmarshal(raw, &str) != nil) {
			return deploySummary{}, false
		}
	}
	return s, true
}

// writeDeployRecord writes the record atomically: a fresh temp file
// under a random name (O_EXCL, never a path something could have
// planted), then a rename over the record's name. Mode 0600 like
// state.json: a record carries the app's own lines, filtered but
// still the app's, and nothing but hotserve reads records — the
// webhook is the interface.
func writeDeployRecord(d appDirs, version string, filtered []byte) (err error) {
	dir, err := recordsDir(d, true)
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
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, deployRecordPath(dir, version))
}

// deployRecordMaxBytes bounds a record read: one holds a bounded
// result (an 8 KiB tail at most); anything larger is not one.
const deployRecordMaxBytes = 1 << 20

// openRecord opens a record without following a link (rule 4) and
// reads it whole, refusing anything that is not a regular file of a
// record's size; the modification time comes from the same open file
// as the bytes, so a record replaced between two lookups cannot pair
// one outcome with another's time.
func openRecord(path string) ([]byte, time.Time, error) {
	// O_NONBLOCK as elf.go opens the command: a FIFO where a record
	// should be would otherwise hold the open — and the status, and
	// the deploy lock — for good.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // a path under the app's own deploys dir, built here
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
	dir, err := recordsDir(d, false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errNoDeployRecord
	}
	if err != nil {
		return nil, err
	}
	name := versionPathComponent(version) + ".json"
	b, _, err := openRecord(filepath.Join(dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errNoDeployRecord
	}
	if err != nil {
		return nil, err
	}
	if _, ok := recordHead(b, name); !ok {
		return nil, fmt.Errorf("deploy record for %s is not that version's record", version)
	}
	return json.RawMessage(b), nil
}

// listDeploySummaries reads every record's outcome, newest first by
// the record's write time. nil when there is no directory yet.
func listDeploySummaries(d appDirs) []deploySummary {
	dir, err := recordsDir(d, false)
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
		// What its version then does in a response is rule 2's, in
		// redactorFor.
		s, ok := recordHead(b, e.Name())
		if !ok {
			continue
		}
		recs = append(recs, dated{s, written})
	}
	// Newest first; two written within one timestamp tick by name, so
	// the order is the same on every read.
	sort.Slice(recs, func(i, j int) bool {
		if !recs[i].written.Equal(recs[j].written) {
			return recs[i].written.After(recs[j].written)
		}
		return recs[i].summary.Version > recs[j].summary.Version
	})
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
		// Rule 1 again: only a record of ours holds a retention slot —
		// a link, a file that is not JSON, or a version that is not
		// its name would otherwise crowd out the records the slots are
		// for. Such an entry is not ours to keep either; it goes.
		b, written, err := openRecord(filepath.Join(dir, e.Name()))
		if _, ok := recordHead(b, e.Name()); err != nil || !ok {
			if rmErr := os.Remove(filepath.Join(dir, e.Name())); rmErr == nil {
				logger.Info("deploy record: removed an entry that is not a record", zap.String("file", e.Name()))
			}
			continue
		}
		others = append(others, rec{e.Name(), written})
	}
	sort.Slice(others, func(i, j int) bool {
		if !others[i].modTime.Equal(others[j].modTime) {
			return others[i].modTime.After(others[j].modTime)
		}
		return others[i].name > others[j].name
	})
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
