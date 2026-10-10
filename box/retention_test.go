package box

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// resultAt writes a result as the applier would have, aged to when.
func (b *testBox) resultAt(id, phase string, when time.Time) {
	b.t.Helper()
	path := filepath.Join(b.x("out"), id+".json")
	if err := os.WriteFile(path, encodeJSON(result{ID: id, Phase: phase}), 0o640); err != nil {
		b.t.Fatal(err)
	}
	if err := os.Chtimes(path, when, when); err != nil {
		b.t.Fatal(err)
	}
}

func (b *testBox) markerAt(id string, posted time.Time) {
	b.t.Helper()
	b.writeMarker(id, marker{SHA256: id + id, Posted: posted})
}

func (b *testBox) hasMarker(id string) bool { return exists(filepath.Join(b.x("stage"), id+".auth")) }

// The Retention table, row by row, and the count and age sweep.
func TestRetention(t *testing.T) {
	t.Run("result and marker: kept while young and among the newest", func(t *testing.T) {
		b := newTestBox(t)
		now := b.clock.Now()
		young, old := randomID(t), randomID(t)
		b.resultAt(young, phaseApplied, now.Add(-time.Hour))
		b.markerAt(young, now.Add(-time.Hour))
		b.resultAt(old, phaseApplied, now.Add(-25*time.Hour))
		b.markerAt(old, now.Add(-25*time.Hour))
		if err := b.run(hooks{}); err != nil {
			t.Fatal(err)
		}
		if b.result(young) == nil || !b.hasMarker(young) {
			t.Error("a young pair was swept")
		}
		if b.result(old) != nil || b.hasMarker(old) {
			t.Error("a pair older than a day was kept")
		}
		// The marker goes first, so a crash between the two leaves a
		// result with no marker, which the same rules sweep.
		if !slices.Equal(b.lastPoint, []string{"marker:remove", "result:remove"}) {
			t.Errorf("removals %v", b.lastPoint)
		}
	})
	t.Run("result, no marker: the same two rules, by the result's age", func(t *testing.T) {
		b := newTestBox(t)
		now := b.clock.Now()
		young, old := randomID(t), randomID(t)
		b.resultAt(young, phaseApplied, now.Add(-time.Hour))
		b.resultAt(old, phaseApplied, now.Add(-25*time.Hour))
		if err := b.run(hooks{}); err != nil {
			t.Fatal(err)
		}
		if b.result(young) == nil || b.result(old) != nil {
			t.Error("wrong sweep")
		}
	})
	t.Run("marker, no result, older than fifteen minutes: stranded, failed written", func(t *testing.T) {
		b := newTestBox(t)
		id := randomID(t)
		b.markerAt(id, b.clock.Now().Add(-16*time.Minute))
		if err := b.run(hooks{}); err != nil {
			t.Fatal(err)
		}
		if r := b.result(id); r == nil || r.Phase != phaseFailed || r.Error != msgNoRecord {
			t.Fatalf("%+v", r)
		}
		if !b.hasMarker(id) {
			t.Error("a kept id lost its marker")
		}
	})
	t.Run("marker, no result, younger than fifteen minutes: pending, untouched", func(t *testing.T) {
		b := newTestBox(t)
		id := randomID(t)
		b.markerAt(id, b.clock.Now().Add(-14*time.Minute))
		if err := b.run(hooks{}); err != nil {
			t.Fatal(err)
		}
		if b.result(id) != nil || !b.hasMarker(id) {
			t.Error("a pending marker was touched")
		}
	})
	t.Run("a stranded marker that would be swept gets no result first", func(t *testing.T) {
		b := newTestBox(t)
		id := randomID(t)
		b.markerAt(id, b.clock.Now().Add(-25*time.Hour))
		if err := b.run(hooks{}); err != nil {
			t.Fatal(err)
		}
		if b.result(id) != nil || b.hasMarker(id) {
			t.Error("not swept")
		}
		if slices.Contains(b.lastPoint, "result:failed") {
			t.Error("a result was written for an id swept at once")
		}
	})
	t.Run("a marker dated past the clock", func(t *testing.T) {
		b := newTestBox(t)
		far, near := randomID(t), randomID(t)
		b.markerAt(far, b.clock.Now().Add(time.Hour))
		b.markerAt(near, b.clock.Now().Add(5*time.Minute))
		if err := b.run(hooks{}); err != nil {
			t.Fatal(err)
		}
		if b.hasMarker(far) || b.result(far) != nil {
			t.Error("a marker an hour ahead would block admission and hold a slot until the clock caught up: it counts as older than a day")
		}
		if b.result(near) != nil {
			t.Error("a marker minutes ahead is pending, as clocks differ")
		}
	})
	t.Run("an unreadable marker is aged by its mtime", func(t *testing.T) {
		b := newTestBox(t)
		old, fresh := randomID(t), randomID(t)
		for _, id := range []string{old, fresh} {
			if err := os.WriteFile(filepath.Join(b.x("stage"), id+".auth"), []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		then := b.clock.Now().Add(-time.Hour)
		if err := os.Chtimes(filepath.Join(b.x("stage"), old+".auth"), then, then); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filepath.Join(b.x("stage"), fresh+".auth"), b.clock.Now(), b.clock.Now()); err != nil {
			t.Fatal(err)
		}
		if err := b.run(hooks{}); err != nil {
			t.Fatal(err)
		}
		if r := b.result(old); r == nil || r.Phase != phaseFailed {
			t.Errorf("old: %+v", r)
		}
		if b.result(fresh) != nil || !b.hasMarker(fresh) {
			t.Error("fresh: touched")
		}
	})
	t.Run("count: the 32 newest ids are kept", func(t *testing.T) {
		b := newTestBox(t)
		now := b.clock.Now()
		var ids []string
		for i := 0; i < 40; i++ {
			id := randomID(t)
			ids = append(ids, id)
			when := now.Add(-time.Duration(i) * time.Minute) // ids[0] newest
			b.resultAt(id, phaseNoChange, when)
			b.markerAt(id, when)
		}
		if err := b.run(hooks{}); err != nil {
			t.Fatal(err)
		}
		for i, id := range ids {
			if kept := b.result(id) != nil; kept != (i < keepIDs) {
				t.Errorf("id %d (of 40, newest first): kept = %v", i, kept)
			}
			if b.hasMarker(id) != (i < keepIDs) {
				t.Errorf("id %d: marker kept = %v", i, b.hasMarker(id))
			}
		}
	})
}

// The stranded rule never touches a push that is still in in/ or work/
// or under a record (Retention, third row).
func TestStrandedRespectsWorkInFlight(t *testing.T) {
	b := newTestBox(t)
	a := b.applier(hooks{})
	old := b.clock.Now().Add(-time.Hour)
	for _, where := range []string{"in", "work"} {
		id := randomID(t)
		if err := os.WriteFile(filepath.Join(b.x(where), id+".tar"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if a.stranded(id, "", old, b.clock.Now()) {
			t.Errorf("a marker whose bundle is in %s/ is stranded", where)
		}
		if err := os.Remove(filepath.Join(b.x(where), id+".tar")); err != nil {
			t.Fatal(err)
		}
	}
	id := randomID(t)
	if a.stranded(id, id, old, b.clock.Now()) {
		t.Error("a marker whose push holds the record is stranded")
	}
	if !a.stranded(randomID(t), id, old, b.clock.Now()) {
		t.Error("an old marker with nothing behind it is not stranded")
	}
}

// What the hotserve uid can put in stage/: root reads only `<id>.auth`,
// never follows a symlink there, never blocks on a FIFO, removes what it
// sweeps whole, and leaves every other name alone.
func TestRetentionHostileStage(t *testing.T) {
	requireRoot(t)
	b := newTestBox(t)
	stage := b.x("stage")
	target := b.path("precious")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link, fifo, dir, locked, big := randomID(t), randomID(t), randomID(t), randomID(t), randomID(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Symlink(target, filepath.Join(stage, link+".auth")))
	mkfifo(t, filepath.Join(stage, fifo+".auth"))
	must(os.MkdirAll(filepath.Join(stage, dir+".auth", "deep", "er"), 0o755))
	must(os.Chmod(filepath.Join(stage, dir+".auth", "deep"), 0))
	must(os.WriteFile(filepath.Join(stage, locked+".auth"), encodeJSON(marker{SHA256: strings.Repeat("a", 64), Posted: b.clock.Now()}), 0))
	must(os.WriteFile(filepath.Join(stage, big+".auth"), make([]byte, maxMarker+1), 0o600))
	others := []string{"lock", "x.auth", strings.ToUpper(randomID(t)) + ".auth", "push-tmp"}
	for _, n := range others {
		must(os.WriteFile(filepath.Join(stage, n), nil, 0o600))
	}
	// A day on, every hostile marker is old by its mtime (lstat, never
	// followed).
	b.clock.advance(26 * time.Hour)
	if err := b.run(hooks{}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{link, fifo, dir, locked, big} {
		if b.hasMarker(id) {
			t.Errorf("%s.auth not swept", id)
		}
		if b.result(id) != nil {
			t.Errorf("%s got a result although it is a day old", id)
		}
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "keep" {
		t.Error("a symlinked marker's target was touched")
	}
	for _, n := range others {
		if !exists(filepath.Join(stage, n)) {
			t.Errorf("%s is not the applier's, and was removed", n)
		}
	}
}
