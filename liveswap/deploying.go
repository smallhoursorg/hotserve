package liveswap

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"
)

// The deploy marker, <app>/deploying.json: a URL or push deploy in
// flight. Such a deploy extracts a release that it removes again if
// it fails; a hotserve that stops during it (a crash, a restart, an
// upgrade) removes nothing, and the release would stay under the
// version's name — a redeploy refused as "already exists", a rollback
// launching it without its pre_start (#318). The marker says which
// release that is. Its rules (DESIGN-liveswap.md):
//
//   - Written before the deploy extracts, and only by a deploy that
//     extracts: a rollback or a relaunch launches a release already
//     on disk, which nothing here may remove.
//   - Removed once the deploy's record is written, whatever the deploy
//     came to, so a marker that outlives its deploy is one no hotserve
//     finished, or one whose removal failed after its record was
//     written — told apart by the record's start time.
//   - Settled (settleInterrupted) only after a confirmed sweep, by
//     recovery and by the next deploy, each holding deployMu: nothing
//     runs out of the release by then, and no deploy writes a marker
//     meanwhile.
//
// It lives beside state.json, outside every view, written and read
// under ownfile.go's rules; a marker that does not prove itself one is
// removed and acted on for nothing.
type deployMarker struct {
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
}

// deployMarkerMaxBytes bounds a marker read: one version and one
// timestamp are under 200 bytes.
const deployMarkerMaxBytes = 4 << 10

// deployMarkerTempPattern names a marker write's temp file.
const deployMarkerTempPattern = ".deploying-*.tmp"

// errNotAMarker is a marker that does not prove itself one.
var errNotAMarker = errors.New("not a deploy marker")

func saveDeployMarker(d appDirs, m deployMarker) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	removeMarkerLeftovers(d)
	return writeOwnFile(d.deploying, deployMarkerTempPattern, data)
}

// loadDeployMarker reads the marker: fs.ErrNotExist when there is
// none, errNotAMarker for one whose content is not a marker's.
func loadDeployMarker(d appDirs) (deployMarker, error) {
	var m deployMarker
	data, _, err := readOwnFile(d.deploying, deployMarkerMaxBytes, "a deploy marker")
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("%w: %w", errNotAMarker, err)
	}
	// The version becomes a path: one that is not a version is used
	// for nothing (redact.go, rule 4, as for state.json's).
	if !validVersion(m.Version) {
		return m, fmt.Errorf("%w: it names %q, which is not a version", errNotAMarker, m.Version)
	}
	if m.StartedAt.IsZero() { // every marker hotserve writes carries its deploy's start
		return m, fmt.Errorf("%w: it carries no start time", errNotAMarker)
	}
	return m, nil
}

// clearDeployMarker removes the marker; none there is no error. The
// name is unlinked, never what a link at it points to.
func clearDeployMarker(d appDirs) error {
	if err := os.Remove(d.deploying); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// removeMarkerLeftovers removes the temp files an interrupted marker
// write leaves, by its own pattern, as fileStateStore.removeLeftovers
// does for state.json's. Best effort.
func removeMarkerLeftovers(d appDirs) {
	entries, err := os.ReadDir(d.app)
	if err != nil {
		return
	}
	prefix, suffix, _ := strings.Cut(deployMarkerTempPattern, "*")
	for _, e := range entries {
		if n := e.Name(); !e.IsDir() && strings.HasPrefix(n, prefix) && strings.HasSuffix(n, suffix) {
			_ = os.Remove(filepath.Join(d.app, n))
		}
	}
}

// interruptedDeployError is what a deploy cut short by a hotserve stop
// is recorded as.
const interruptedDeployError = "the deploy was cut short: hotserve stopped or restarted during it; its release was removed, so this version can be deployed again"

// settleInterrupted settles a marker a deploy no hotserve finished
// left, under the rules above. The caller holds deployMu and has just
// confirmed a sweep keeping the instance of version keep ("" for
// none). It returns the version a marker it could not settle names,
// and why; the marker then stays for the next try.
func (ma *managedApp) settleInterrupted(c collaborators, keep string) (string, error) {
	d := c.spec.dirs
	m, err := loadDeployMarker(d)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", nil
	case errors.Is(err, errNotAMarker):
		c.logger.Error("deploy marker removed: it is not one, so nothing is done for it", zap.String("path", d.deploying), zap.Error(err))
		return "", ma.clearMarker(c)
	case err != nil:
		// Unreadable is not absent: kept, for the next try to read.
		c.logger.Warn("deploy marker unreadable; leaving it for the next start or deploy", zap.String("path", d.deploying), zap.Error(err))
		return "", nil
	}
	logger := c.logger.With(zap.String("version", m.Version))
	if m.Version == keep {
		// That deploy reached the cutover: what it put there serves.
		return "", ma.clearMarker(c)
	}
	if recordedStart(d, m.Version).Equal(m.StartedAt) {
		// That deploy was recorded; only its marker's removal failed.
		return "", ma.clearMarker(c)
	}
	if p, why := d.linkAt(d.releases); p != "" {
		return m.Version, fmt.Errorf("release %s of a deploy hotserve did not finish not removed: %s is not the directory it names: %w (%s)", m.Version, p, why, notOwnDirFix)
	}
	if err := os.RemoveAll(d.release(m.Version)); err != nil {
		return m.Version, fmt.Errorf("removing release %s of a deploy hotserve did not finish: %w", m.Version, err)
	}
	logger.Warn("removed the release of a deploy hotserve did not finish; the version can be deployed again")
	result := deployResult{
		Version:    m.Version,
		Status:     "failed",
		Error:      interruptedDeployError,
		StartedAt:  m.StartedAt,
		FinishedAt: c.clock.Now(),
	}
	ma.recordDeploy(c, result)
	ma.mu.Lock()
	if ma.lastDeploy == nil || !ma.lastDeploy.StartedAt.After(m.StartedAt) {
		ma.lastDeploy = &result
	}
	ma.mu.Unlock()
	return "", ma.clearMarker(c)
}

func (ma *managedApp) clearMarker(c collaborators) error {
	if err := clearDeployMarker(c.spec.dirs); err != nil {
		return fmt.Errorf("removing the deploy marker: %w", err)
	}
	return nil
}

// recordedStart is when the deploy version's record holds began; the
// zero time when there is no record, or none that reads.
func recordedStart(d appDirs, version string) time.Time {
	b, err := readDeployRecord(d, version)
	if err != nil {
		return time.Time{}
	}
	var r struct {
		StartedAt time.Time `json:"started_at"`
	}
	if json.Unmarshal(b, &r) != nil {
		return time.Time{}
	}
	return r.StartedAt
}

// settleBeforeDeploy settles a marker before a deploy goes on, once a
// sweep keeping old confirms: the deploy that left the marker may have
// left its pre_start running too. Only a marker there costs that
// sweep. One this cannot settle refuses the deploy only when it names
// the version asked for, whose leftover release the deploy would
// otherwise trip on; any other deploy goes on, and leaves it for the
// next try.
func (ma *managedApp) settleBeforeDeploy(c collaborators, old *instance, version string) error {
	if _, err := os.Lstat(c.spec.dirs.deploying); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := ma.sweepOrWhy(c, old, ""); err != nil {
		return fmt.Errorf("%w; not deploying until the deploy hotserve did not finish is settled", err)
	}
	keep := ""
	if old != nil {
		keep = old.version
	}
	pending, err := ma.settleInterrupted(c, keep)
	if err == nil {
		return nil
	}
	if pending == version {
		return err
	}
	c.logger.Error("deploy marker not settled; deploying on", zap.Error(err))
	return nil
}

// settleAfterRecovery settles a marker once recovery's sweep has
// confirmed, keeping the instance of version keep. One it cannot
// settle is logged and kept: the app is up, and the next start or
// deploy tries again.
func (ma *managedApp) settleAfterRecovery(c collaborators, keep string) {
	if _, err := ma.settleInterrupted(c, keep); err != nil {
		c.logger.Error("deploy marker not settled; the next start or deploy tries again", zap.Error(err))
	}
}
