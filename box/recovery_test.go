package box

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func randomID(t testing.TB) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// lyingRecord writes a record as a crashed run would have left it, with
// its work/ entry when the origin is the applier's.
func (b *testBox) lyingRecord(rec record) {
	b.t.Helper()
	if err := os.WriteFile(b.x("txn.json"), encodeJSON(rec), 0o600); err != nil {
		b.t.Fatal(err)
	}
	if rec.Origin == originApplier {
		if err := os.WriteFile(filepath.Join(b.x("work"), rec.ID+".tar"), []byte("taken"), 0o644); err != nil {
			b.t.Fatal(err)
		}
	}
}

// The States table (DESIGN-box.md): recovery acts on the phase after
// checking d, the installed file's digest, against what the phase
// implies — including every "d must be" violation.
func TestRecoveryStates(t *testing.T) {
	const (
		dPrev  = "prev"
		dNew   = "new"
		dOther = "other"
	)
	type row struct {
		phase    string
		origin   origin
		d        string
		state    string  // is-active's answer
		reloads  []error // reload results
		recError string  // a rolling_back record's error
		file     string  // the installed file at the end: prev, new, other
		advanced bool
		result   string // result phase ("" none: init writes none)
		msg      string
		reloaded int
	}
	rows := []row{
		{phase: phaseNoChange, d: dPrev, file: dPrev, advanced: true, result: phaseNoChange},
		{phase: phaseNoChange, d: dOther, file: dOther, result: phaseUnknown, msg: msgChanged},

		{phase: phaseInstalling, d: dPrev, file: dPrev, result: phaseFailed, msg: msgInterrupted},
		{phase: phaseInstalling, d: dNew, state: "active", file: dPrev, result: phaseRolledBack, msg: msgRolledBackInterrupted, reloaded: 1},
		{phase: phaseInstalling, d: dNew, state: "inactive", file: dPrev, result: phaseFailed, msg: msgInterruptedStopped},
		{phase: phaseInstalling, d: dOther, file: dOther, result: phaseUnknown, msg: msgChanged},

		{phase: phaseSwapped, d: dNew, state: "active", file: dPrev, result: phaseRolledBack, msg: msgRolledBackInterrupted, reloaded: 1},
		{phase: phaseSwapped, d: dNew, state: "failed", file: dPrev, result: phaseFailed, msg: msgInterruptedStopped},
		{phase: phaseSwapped, d: dNew, state: "active", reloads: []error{errReload}, file: dPrev, result: phaseUnknown, msg: msgBothReloads, reloaded: 1},
		{phase: phaseSwapped, d: dPrev, state: "active", file: dPrev, result: phaseRolledBack, msg: msgRolledBackInterrupted, reloaded: 1},
		{phase: phaseSwapped, d: dOther, file: dOther, result: phaseUnknown, msg: msgChanged},
		{phase: phaseSwapped, origin: originInit, d: dNew, state: "inactive", file: dNew, advanced: true},
		{phase: phaseSwapped, origin: originInit, d: dNew, state: "active", file: dPrev, reloaded: 1},

		{phase: phaseApplied, d: dNew, file: dNew, advanced: true, result: phaseApplied},
		{phase: phaseApplied, d: dPrev, file: dPrev, result: phaseUnknown, msg: msgChanged},
		{phase: phaseApplied, d: dOther, file: dOther, result: phaseUnknown, msg: msgChanged},

		{phase: phaseRollingBack, d: dPrev, state: "active", recError: msgRecordAfterReload, file: dPrev, result: phaseRolledBack, msg: msgRecordAfterReload, reloaded: 1},
		{phase: phaseRollingBack, d: dNew, state: "active", recError: msgReloadFailed, file: dPrev, result: phaseRolledBack, msg: msgReloadFailed, reloaded: 1},
		{phase: phaseRollingBack, d: dPrev, state: "active", reloads: []error{errReload}, file: dPrev, result: phaseUnknown, msg: msgBothReloads, reloaded: 1},
		{phase: phaseRollingBack, d: dPrev, state: "inactive", file: dPrev, result: phaseFailed, msg: msgInterruptedStopped},
		{phase: phaseRollingBack, d: dNew, state: "inactive", file: dPrev, result: phaseFailed, msg: msgInterruptedStopped},
		{phase: phaseRollingBack, d: dOther, file: dOther, result: phaseUnknown, msg: msgChanged},
	}
	for _, r := range rows {
		if r.origin == "" {
			r.origin = originApplier
		}
		name := r.phase + "/" + string(r.origin) + "/d=" + r.d + "/" + r.state
		t.Run(name, func(t *testing.T) {
			b := newTestBox(t)
			v2 := boxFile(2, b.alice)
			head := b.repo.commit(v2, &b.alice, b.base)
			next := v2
			if r.phase == phaseNoChange {
				next = b.v1
			}
			other := boxFile(9, b.alice)
			files := map[string][]byte{dPrev: b.v1, dNew: next, dOther: other}
			b.writeInstalled(files[r.d])
			if r.state != "" {
				b.sd.states = []string{r.state}
			}
			b.sd.reloads = r.reloads
			id := randomID(t)
			signer := b.alice.principal
			if r.origin == originInit {
				signer = "init"
			}
			b.lyingRecord(record{
				ID: id, Origin: r.origin, Phase: r.phase, Commit: head, Path: testPath, Signer: signer,
				Prev: b.v1, PrevSHA256: digest(b.v1), NewSHA256: digest(next), BoxWebhook: "deploy.example.com",
				Error: r.recError,
			})
			// A temporary the crash left beside the Caddyfile goes too.
			if err := os.WriteFile(filepath.Join(b.path("etc/hotserve"), ".Caddyfile.box-"+id), []byte("half"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := b.run(hooks{}); err != nil {
				t.Fatal(err)
			}
			b.settled()
			if got := string(b.installed()); got != string(files[r.file]) {
				t.Errorf("installed file is not %s", r.file)
			}
			if adv := b.applied().SHA == head; adv != r.advanced {
				t.Errorf("baseline advanced = %v, want %v", adv, r.advanced)
			}
			res := b.result(id)
			switch {
			case r.result == "" && res != nil:
				t.Errorf("a result was written for %s: %+v", r.origin, res)
			case r.result != "" && (res == nil || res.Phase != r.result || res.Error != r.msg):
				t.Errorf("result %+v, want %s %q", res, r.result, r.msg)
			}
			if b.sd.reloaded != r.reloaded {
				t.Errorf("%d reloads, want %d", b.sd.reloaded, r.reloaded)
			}
		})
	}
}

// A record whose result is already terminal finished all but its
// removals (Failure-mode table, "terminal result", crash after).
func TestRecoveryTerminalResultRemovesRecord(t *testing.T) {
	b := newTestBox(t)
	v2 := boxFile(2, b.alice)
	head := b.repo.commit(v2, &b.alice, b.base)
	b.writeInstalled(v2)
	id := randomID(t)
	b.lyingRecord(record{ID: id, Origin: originApplier, Phase: phaseApplied, Commit: head, Path: testPath, Signer: "alice@example.com",
		Prev: b.v1, PrevSHA256: digest(b.v1), NewSHA256: digest(v2)})
	if err := os.WriteFile(filepath.Join(b.x("out"), id+".json"), encodeJSON(result{ID: id, Phase: phaseApplied}), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := b.run(hooks{}); err != nil {
		t.Fatal(err)
	}
	b.settled()
	if b.sd.reloaded != 0 || b.applied().SHA != b.base {
		t.Error("a finished transaction was acted on again")
	}
}

// A record that does not read is kept and nothing is touched: not the
// file, not in/, not work/ (States table, "record unreadable").
func TestRecoveryUnreadableRecord(t *testing.T) {
	for name, body := range map[string]string{
		"not json":       "{",
		"unknown phase":  `{"id":"0123456789abcdef0123456789abcdef","origin":"applier","phase":"verified"}`,
		"prev mismatch":  `{"id":"0123456789abcdef0123456789abcdef","origin":"applier","phase":"swapped","commit":"` + strings.Repeat("a", 40) + `","prev":"eA==","prev_sha256":"` + strings.Repeat("0", 64) + `","new_sha256":"` + strings.Repeat("1", 64) + `"}`,
		"unknown origin": `{"id":"0123456789abcdef0123456789abcdef","origin":"cron","phase":"swapped"}`,
	} {
		t.Run(name, func(t *testing.T) {
			b := newTestBox(t)
			if err := os.WriteFile(b.x("txn.json"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			head := b.repo.commit(boxFile(2, b.alice), &b.alice, b.base)
			b.push(b.repo.bundleFiles(head, b.base))
			if err := b.run(hooks{}); err != nil {
				t.Fatalf("an unreadable record is not the full-disk end: %v", err)
			}
			if data, _ := os.ReadFile(b.x("txn.json")); string(data) != body {
				t.Error("the record was changed")
			}
			if len(b.names(b.x("in"))) != 1 || string(b.installed()) != string(b.v1) {
				t.Error("something was taken or installed under an unreadable record")
			}
			if !b.errorLogged("txn.json cannot be read") {
				t.Error("no error-level line")
			}
		})
	}
}

// No record: what work/ holds is settled by its result (States table,
// "(no record)"); what is not a bundle is removed with no result.
func TestRecoveryNoRecord(t *testing.T) {
	b := newTestBox(t)
	work := b.x("work")
	terminalID, verifiedID, bareID := randomID(t), randomID(t), randomID(t)
	for _, id := range []string{terminalID, verifiedID, bareID} {
		if err := os.WriteFile(filepath.Join(work, id+".tar"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(b.x("out"), terminalID+".json"), encodeJSON(result{ID: terminalID, Phase: phaseRefused, Error: "no"}), 0o640); err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("c", 40)
	if err := os.WriteFile(filepath.Join(b.x("out"), verifiedID+".json"), encodeJSON(result{ID: verifiedID, Phase: phaseVerified, Commit: head, Signer: "alice@example.com"}), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(work, "junk"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tmp := range []string{b.x(".txn.json.tmp"), b.x(".applied.json.tmp"), filepath.Join(b.x("out"), "."+bareID+".json.tmp")} {
		if err := os.WriteFile(tmp, []byte("half"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.run(hooks{}); err != nil {
		t.Fatal(err)
	}
	b.settled()
	if r := b.result(terminalID); r.Phase != phaseRefused || r.Error != "no" {
		t.Errorf("a terminal result was rewritten: %+v", r)
	}
	if r := b.result(verifiedID); r.Phase != phaseFailed || r.Error != msgInterrupted || r.Commit != head || r.Signer != "alice@example.com" {
		t.Errorf("verified, no record: %+v", r)
	}
	if r := b.result(bareID); r == nil || r.Phase != phaseFailed || r.Error != msgInterrupted {
		t.Errorf("taken, no record: %+v", r)
	}
	if r := b.result("junk"); r != nil {
		t.Error("an entry that is not a bundle got a result")
	}
	for _, tmp := range []string{b.x(".txn.json.tmp"), b.x(".applied.json.tmp"), filepath.Join(b.x("out"), "."+bareID+".json.tmp")} {
		if exists(tmp) {
			t.Errorf("%s left behind", filepath.Base(tmp))
		}
	}
}
