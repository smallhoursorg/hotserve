package box

import (
	"bytes"
	"context"
	"testing"
)

// The origin: init rows of the Transitions and States tables, through
// runInit, the entry `hotserve init` (PR 4) calls: no is-active
// refusal, no result files, and the swap counting as applied on a box
// where hotserve is not running.
func TestInitOrigin(t *testing.T) {
	initRec := func(b *testBox, file []byte, sha string) record {
		shape, err := Walk(file)
		if err != nil {
			b.t.Fatal(err)
		}
		return record{ID: randomID(b.t), Commit: sha, Path: testPath, BoxWebhook: shape.Host, Apps: shape.Apps}
	}
	for _, c := range []struct {
		name     string
		same     bool
		state    string
		reloads  []error
		phase    string
		file     int
		advanced bool
		reloaded int
	}{
		{name: "no_change on a box that is not running", same: true, state: "inactive", phase: phaseNoChange, file: 1, advanced: true},
		{name: "no_change on an active box", same: true, state: "active", phase: phaseNoChange, file: 1, advanced: true},
		{name: "not running: the swap counts as applied, no reload", state: "inactive", phase: phaseApplied, file: 2, advanced: true},
		{name: "still activating after the wait: not running", state: "activating", phase: phaseApplied, file: 2, advanced: true},
		{name: "active: reloaded", state: "active", phase: phaseApplied, file: 2, advanced: true, reloaded: 1},
		{name: "active, reload fails: rolled back", state: "active", reloads: []error{errReload}, phase: phaseRolledBack, file: 1, reloaded: 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newTestBox(t)
			b.sd.states, b.sd.reloads = []string{c.state}, c.reloads
			file := boxFile(2, b.alice)
			if c.same {
				file = b.v1
			}
			sha := b.repo.commit(file, &b.alice, b.base)
			out, err := b.applier(hooks{}).runInit(context.Background(), initRec(b, file, sha), file)
			if err != nil {
				t.Fatal(err)
			}
			b.settled()
			if out.Phase != c.phase || out.Signer != "init" {
				t.Errorf("outcome %+v, want %s by init", out, c.phase)
			}
			want := b.v1
			if c.file == 2 {
				want = file
			}
			if !bytes.Equal(b.installed(), want) {
				t.Errorf("installed is not version %d", c.file)
			}
			a := b.applied()
			if (a.SHA == sha) != c.advanced || (c.advanced && a.Signer != "init") {
				t.Errorf("applied.json %+v", a)
			}
			if n := b.names(b.x("out")); len(n) != 0 {
				t.Errorf("init wrote results: %v", n)
			}
			// init asks once, when it begins, whatever follows: rollback
			// included. (A wait asks again until it ends: activating.)
			if c.state != "activating" && b.sd.asked != 1 {
				t.Errorf("%d is-active questions, want 1", b.sd.asked)
			}
			if b.sd.reloaded != c.reloaded {
				t.Errorf("%d reloads, want %d", b.sd.reloaded, c.reloaded)
			}
		})
	}
}

// init takes root's lock and settles a record a crash left before it
// starts its own transaction.
func TestInitRecoversFirst(t *testing.T) {
	b := newTestBox(t)
	v2 := boxFile(2, b.alice)
	head := b.repo.commit(v2, &b.alice, b.base)
	b.writeInstalled(v2)
	crashedID := randomID(t)
	b.lyingRecord(record{ID: crashedID, Origin: originApplier, Phase: phaseSwapped, Commit: head, Path: testPath, Signer: "alice@example.com",
		Prev: b.v1, PrevSHA256: digest(b.v1), NewSHA256: digest(v2)})
	v3 := boxFile(3, b.alice)
	sha := b.repo.commit(v3, &b.alice, b.base)
	out, err := b.applier(hooks{}).runInit(context.Background(), record{ID: randomID(t), Commit: sha, Path: testPath, BoxWebhook: "deploy.example.com"}, v3)
	if err != nil {
		t.Fatal(err)
	}
	b.settled()
	if r := b.result(crashedID); r == nil || r.Phase != phaseRolledBack {
		t.Errorf("the crashed push: %+v", r)
	}
	if out.Phase != phaseApplied || !bytes.Equal(b.installed(), v3) {
		t.Errorf("init: %+v", out)
	}
}
