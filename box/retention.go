package box

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/smallhoursorg/hotserve/box/proof"
)

// Retention (DESIGN-box.md, "Retention"): root is the only sweeper of
// results and markers, so no half of a pair is orphaned by two processes
// disagreeing.
const (
	// keepIDs and keepAge are the two rules: an id is kept while it is
	// among the keepIDs newest and younger than keepAge.
	keepIDs = 32
	keepAge = 24 * time.Hour
	// pendingAge is how long a marker with no result blocks admission;
	// past it, with nothing in in/ or work/ and no record, root settles
	// it as stranded.
	pendingAge = 15 * time.Minute
)

// idState is what the sweep knows of one id.
type idState struct {
	result, marker bool
	// key is the id's age: the result's modification time when there
	// is a result, else the marker's posted time, else the marker's
	// modification time.
	key time.Time
	// written is the result's modification time: root's own clock.
	written time.Time
	// stranded is a marker the table's third row settles.
	stranded bool
}

// sweep runs Retention once, after the run's bundles: every id in out/
// or stage/*.auth, by the table's rows. It writes `failed` for a
// stranded marker only if the id is kept — one that would be swept at
// once ends the same with no write — and removes a pair marker first,
// so a crash between the two leaves "result present, marker absent",
// which the same rules sweep.
func (a *Applier) sweep() {
	now := a.clock.Now()
	ids := map[string]*idState{}
	get := func(id string) *idState {
		if ids[id] == nil {
			ids[id] = &idState{}
		}
		return ids[id]
	}
	if entries, err := os.ReadDir(a.x("out")); err == nil {
		for _, e := range entries {
			id, ok := strings.CutSuffix(e.Name(), ".json")
			if !ok || !isRequestID(id) {
				continue
			}
			s := get(id)
			s.result = true
			if fi, err := e.Info(); err == nil {
				s.written = fi.ModTime()
			}
		}
	} else {
		a.logger.Error("box: out/ cannot be listed", zap.String("error", proof.Bound(err.Error())))
		return
	}
	if entries, err := os.ReadDir(a.x("stage")); err == nil {
		for _, e := range entries {
			// Only `<id>.auth` is the applier's to read or remove; the
			// handler's lock and temporaries are its own.
			id, ok := strings.CutSuffix(e.Name(), ".auth")
			if !ok || !isRequestID(id) {
				continue
			}
			s := get(id)
			s.marker = true
			if m, err := readMarker(filepath.Join(a.x("stage"), e.Name())); err == nil {
				s.key = m.Posted
			} else if fi, err := os.Lstat(filepath.Join(a.x("stage"), e.Name())); err == nil {
				s.key = fi.ModTime()
			}
		}
	} else {
		a.logger.Error("box: stage/ cannot be listed", zap.String("error", proof.Bound(err.Error())))
		return
	}

	// The record's id, read once: a push under it is in root's hands.
	held := ""
	if rec, err := a.readRecord(); err == nil {
		held = rec.ID
	}
	var candidates []string
	for id, s := range ids {
		switch {
		case s.result:
			// A result is aged by root's own write, which the hotserve
			// uid cannot touch; one dated past the clock (the clock
			// stepped back since) counts as written now.
			s.key = minTime(s.written, now)
		case s.key.Sub(now) > pendingAge:
			// A marker with no result dated past the clock cannot be
			// aged: it counts as older than keepAge, so it neither
			// blocks admission nor holds a slot until the clock catches
			// up.
			s.key = time.Time{}
		}
		if !s.result {
			// An id this run settled whose result did not land is not
			// stranded yet: its outcome is in the journal (I3), and the
			// next run settles it by the table like any other.
			if a.wrote[id] || !a.stranded(id, held, s.key, now) {
				continue // pending, or still in root's hands: untouched
			}
			s.stranded = true
		}
		candidates = append(candidates, id)
	}
	// Newest first; an id's age ties broken by the id, so a run is
	// deterministic.
	sort.Slice(candidates, func(i, j int) bool {
		ki, kj := ids[candidates[i]].key, ids[candidates[j]].key
		if !ki.Equal(kj) {
			return ki.After(kj)
		}
		return candidates[i] < candidates[j]
	})
	for n, id := range candidates {
		s := ids[id]
		// A result this run wrote is never swept by it, so the push it
		// settled can be polled at least until root's next run.
		keep := a.wrote[id] || (n < keepIDs && now.Sub(s.key) < keepAge)
		if keep {
			if s.stranded {
				// The push was lost before root saw it, or its result
				// could not be written.
				a.finish(&txn{rec: record{ID: id, Origin: originApplier}}, phaseFailed, msgNoRecord)
			}
			continue
		}
		if s.marker {
			if err := a.removeDurable("marker:remove", filepath.Join(a.x("stage"), id+".auth")); err != nil {
				a.logger.Error("box: could not remove a marker", zap.String("id", id), zap.String("error", proof.Bound(err.Error())))
				continue // the result stays with it: a marker never outlives its result
			}
		}
		if s.result {
			a.removeResult(id)
		}
	}
}

// stranded is the table's third row: a marker with no result, nothing
// in in/ or work/ and no record for its id, at least pendingAge old. A
// marker up to pendingAge ahead of the clock is pending, as clocks
// differ; one further ahead reaches here already made the oldest.
func (a *Applier) stranded(id, held string, posted, now time.Time) bool {
	if id == held || exists(filepath.Join(a.x("in"), id+".tar")) || exists(filepath.Join(a.x("work"), id+".tar")) {
		return false
	}
	return now.Sub(posted) >= pendingAge
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
