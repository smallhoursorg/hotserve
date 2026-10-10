package box

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/hotserve/box/proof"
)

// FuzzApplyBundle: whatever bytes stand in in/ as a bundle, one run
// settles them — exactly one terminal result (I3), in/ and work/ empty
// and no record (I2, I5) — and the Caddyfile changes only to a file a
// listed signer committed on a chain from the baseline (I1, I6): what
// the result says was applied is the bundle's own file, and the
// baseline is its HEAD.
func FuzzApplyBundle(f *testing.F) {
	seed := newTestBox(f)
	good := seed.repo.commit(boxFile(2, seed.alice), &seed.alice, seed.base)
	f.Add(tgz(f, seed.repo.bundleFiles(good, seed.base)))
	f.Add(tgz(f, seed.repo.bundleFiles(seed.repo.commit(boxFile(3, seed.alice), nil, seed.base), seed.base)))
	f.Add(tgz(f, seed.repo.bundleFiles(seed.repo.commit(seed.v1, &seed.alice, seed.base), seed.base)))
	f.Add([]byte{0x1f, 0x8b})
	f.Add([]byte("not a bundle"))
	f.Fuzz(func(t *testing.T, data []byte) {
		b := newTestBox(t)
		id := b.drop(data, b.clock.Now())
		if err := b.run(hooks{}); err != nil {
			t.Fatal(err)
		}
		b.settled()
		r := b.result(id)
		if r == nil || !terminal(r.Phase) {
			t.Fatalf("result %+v", r)
		}
		switch r.Phase {
		case phaseApplied, phaseNoChange:
			bundle, err := proof.ReadBundle(data)
			if err != nil {
				t.Fatalf("%s from a bundle that does not parse: %v", r.Phase, err)
			}
			if !bytes.Equal(b.installed(), bundle.Caddyfile) || b.applied().SHA != bundle.Commit.ID || r.Commit != bundle.Commit.ID {
				t.Fatal("what was installed is not the bundle's file at its HEAD")
			}
		case phaseRefused, phaseFailed:
			if !bytes.Equal(b.installed(), b.v1) || b.applied().SHA != b.base {
				t.Fatal("a refused or failed push changed the box")
			}
		default:
			t.Fatalf("phase %s with no fault injected", r.Phase)
		}
	})
}

// FuzzRecord: whatever txn.json holds, recovery writes nothing to the
// Caddyfile but the record's own prev bytes (the only rollback source),
// advances the baseline only to the record's commit and only when the
// installed file is the record's new one, and keeps a record it cannot
// read untouched, with nothing else touched either.
func FuzzRecord(f *testing.F) {
	seed := newTestBox(f)
	v2 := boxFile(2, seed.alice)
	head := seed.repo.commit(v2, &seed.alice, seed.base)
	for _, phase := range []string{phaseNoChange, phaseInstalling, phaseSwapped, phaseApplied, phaseRollingBack} {
		for _, o := range []origin{originApplier, originInit} {
			next := v2
			if phase == phaseNoChange {
				next = seed.v1
			}
			rec := record{ID: strings.Repeat("ab", 16), Origin: o, Phase: phase, Commit: head, Path: testPath, Signer: "alice@example.com",
				Prev: seed.v1, PrevSHA256: digest(seed.v1), NewSHA256: digest(next), BoxWebhook: "deploy.example.com"}
			for which := uint8(0); which < 3; which++ {
				f.Add(encodeJSON(rec), which)
			}
		}
	}
	f.Add([]byte("{"), uint8(0))
	f.Fuzz(func(t *testing.T, data []byte, which uint8) {
		b := newTestBox(t)
		files := [][]byte{b.v1, boxFile(2, b.alice), boxFile(9, b.alice)}
		before := files[int(which)%len(files)]
		b.writeInstalled(before)
		if err := os.WriteFile(b.x("txn.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := b.run(hooks{}); err != nil {
			t.Fatal(err)
		}
		var rec record
		valid := json.Unmarshal(data, &rec) == nil && rec.valid() == nil
		after := b.installed()
		if !valid {
			if got, _ := os.ReadFile(b.x("txn.json")); !bytes.Equal(got, data) || !bytes.Equal(after, before) || b.applied().SHA != b.base {
				t.Fatal("an unreadable record was acted on")
			}
			return
		}
		b.settled()
		if !bytes.Equal(after, before) && !bytes.Equal(after, rec.Prev) {
			t.Fatal("recovery installed bytes that are neither what stood nor the record's prev")
		}
		switch sha := b.applied().SHA; {
		case sha == b.base:
		case sha == rec.Commit && digest(after) == rec.NewSHA256:
		default:
			t.Fatalf("the baseline moved to %s with the installed file %s", sha, digest(after))
		}
	})
}

// FuzzRetention: whatever the hotserve uid names and writes in stage/,
// the sweep removes only `<id>.auth` entries, keeps at most 32 ids, and
// never follows, blocks or fails the run.
func FuzzRetention(f *testing.F) {
	f.Add("0123456789abcdef0123456789abcdef.auth", []byte(`{"sha256":"`+strings.Repeat("a", 64)+`","posted":"2026-10-10T00:00:00Z"}`), int32(30))
	f.Add("lock", []byte{}, int32(0))
	f.Add("0123456789abcdef0123456789abcdef.auth", []byte("{"), int32(-60))
	f.Add("x.auth", []byte("{}"), int32(2000))
	f.Fuzz(func(t *testing.T, name string, body []byte, minutes int32) {
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") || len(name) > 200 {
			return
		}
		b := newTestBox(t)
		path := filepath.Join(b.x("stage"), name)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			return
		}
		when := b.clock.Now().Add(-time.Duration(minutes) * time.Minute)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < keepIDs+2; i++ {
			id := randomID(t)
			b.resultAt(id, phaseApplied, b.clock.Now().Add(-time.Duration(i)*time.Second))
		}
		if err := b.run(hooks{}); err != nil {
			t.Fatal(err)
		}
		id, isMarker := strings.CutSuffix(name, ".auth")
		if (!isMarker || !isRequestID(id)) && !exists(path) {
			t.Fatalf("%q is not the applier's to remove", name)
		}
		if n := len(b.names(b.x("out"))); n > keepIDs {
			t.Fatalf("%d results kept", n)
		}
		if !bytes.Equal(b.installed(), b.v1) {
			t.Fatal("the sweep touched the Caddyfile")
		}
	})
}
