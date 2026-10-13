package box

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"filippo.io/age"
)

// The relations between a record's fields, and a baseline's (DESIGN-box.md,
// "Record and result fields", F1–F9), held as a family: records and
// baselines are taken from real transactions of every shape a set can
// have, and each is mutated one field at a time. A mutant must be refused
// by the one validator of its file — unless the mutation is listed as a
// valid variant, with the reason no relation ties that field to the rest.

// encryptTB is encrypt for any testing.TB.
func encryptTB(t testing.TB, plain []byte, to age.Recipient) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := age.Encrypt(&out, to)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// generated runs one transaction of each shape and keeps every record
// root wrote — read back from txn.json after each record write — and
// every baseline it wrote.
func generated(t testing.TB) (records []record, baselines []applied) {
	t.Helper()
	keep := func(b *testBox) func(string) {
		return func(p string) {
			switch {
			case strings.HasPrefix(p, "record:") && p != "record:remove":
				var r record
				if data, err := os.ReadFile(b.x("txn.json")); err == nil && json.Unmarshal(data, &r) == nil {
					records = append(records, r)
				}
			case p == "applied.json":
				baselines = append(baselines, b.applied())
			}
		}
	}
	b, id := newSecretsBox(t)
	enc := func(v string) []byte { return encryptTB(t, []byte(v), id.Recipient()) }
	push := func(head, base, want string) {
		t.Helper()
		pushed := b.push(b.repo.bundleFiles(head, base))
		if err := b.run(hooks{crash: keep(b)}); err != nil {
			t.Fatal(err)
		}
		if r := b.result(pushed); r == nil || r.Phase != want {
			t.Fatalf("a generating push ended %+v, want %s", r, want)
		}
	}
	v2 := boxFile(2, b.alice)

	// The Caddyfile alone.
	c1 := b.repo.commit(v2, &b.alice, b.base)
	push(c1, b.base, phaseApplied)
	// Env files added, with a Caddyfile that changes again.
	c2 := b.repo.commitSecrets(boxFile(3, b.alice), secrets{"api": {"TOKEN": enc("t")}, "example": {"KEY": enc("v"), "OTHER": enc("o")}}, &b.alice, c1)
	push(c2, c1, phaseApplied)
	// One value changed, one removed, the Caddyfile kept.
	c3 := b.repo.commitSecrets(boxFile(3, b.alice), secrets{"api": {"TOKEN": enc("t")}, "example": {"KEY": enc("w")}}, &b.alice, c2)
	push(c3, c2, phaseApplied)
	// An app's secrets removed, another's file rewritten for its mode.
	if err := os.Chmod(b.envPath("api"), 0o644); err != nil {
		t.Fatal(err)
	}
	c4 := b.repo.commitSecrets(boxFile(4, b.alice), secrets{"api": {"TOKEN": enc("t")}}, &b.alice, c3)
	push(c4, c3, phaseApplied)
	// A reload that fails: a rolling_back record.
	c5 := b.repo.commitSecrets(boxFile(5, b.alice), secrets{"api": {"TOKEN": enc("u")}, "example": {"KEY": enc("x")}}, &b.alice, c4)
	b.sd.reloads = []error{nil, nil, nil, errReload} // the three pushes above reloaded
	push(c5, c4, phaseRolledBack)
	// no_change: a new commit of the bytes the box runs.
	c6 := b.repo.commitSecrets(boxFile(4, b.alice), secrets{"api": {"TOKEN": enc("t")}}, &b.alice, c4)
	push(c6, c4, phaseNoChange)
	// init, over a baseline that lists env files.
	file := boxFile(6, b.alice)
	sha := b.repo.commit(file, &b.alice, c6)
	a := b.applier(hooks{crash: keep(b)})
	if err := runInitCmd(context.Background(), 0, a, initDir(t, file), sha, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	return records, baselines
}

type recordMutation struct {
	name string
	// apply mutates r, reporting whether the mutation applies to it.
	apply func(r *record) bool
	// valid, when set, makes the mutant a valid variant, and says why no
	// relation ties the field to the rest.
	valid func(r *record) string
}

func cloneRecord(r record) record {
	c := r
	c.Secrets = slices.Clone(r.Secrets)
	c.SecretsInstalled = maps.Clone(r.SecretsInstalled)
	c.SecretsAdded, c.SecretsChanged, c.SecretsRemoved = slices.Clone(r.SecretsAdded), slices.Clone(r.SecretsChanged), slices.Clone(r.SecretsRemoved)
	c.SecretsOutOfBand = slices.Clone(r.SecretsOutOfBand)
	return c
}

func (r *record) names() []string {
	return slices.Concat(r.SecretsAdded, r.SecretsChanged, r.SecretsRemoved)
}

func (r *record) namesApp(app string) bool {
	return slices.ContainsFunc(r.names(), func(n string) bool { return strings.HasPrefix(n, app+"/") })
}

var recordMutations = []recordMutation{
	{name: "phase no_change", apply: func(r *record) bool {
		if r.Phase == phaseNoChange {
			return false
		}
		r.Phase = phaseNoChange
		return true
	}},
	{name: "phase verified", apply: func(r *record) bool { r.Phase = phaseVerified; return true }},
	{name: "origin init", apply: func(r *record) bool {
		if r.Origin == originInit {
			return false
		}
		r.Origin = originInit
		return true
	}, valid: func(r *record) string {
		if len(r.Secrets) == 0 && len(r.names()) == 0 {
			return "a set of the Caddyfile alone is one init makes too; the origin decides only who is told"
		}
		return ""
	}},
	{name: "an entry whose values change dropped", apply: func(r *record) bool {
		for i, e := range r.Secrets {
			if r.namesApp(e.App) {
				r.Secrets = slices.Delete(r.Secrets, i, i+1)
				return true
			}
		}
		return false
	}},
	{name: "an entry whose values do not change dropped", apply: func(r *record) bool {
		for i, e := range r.Secrets {
			if !r.namesApp(e.App) {
				r.Secrets = slices.Delete(r.Secrets, i, i+1)
				return true
			}
		}
		return false
	}, valid: func(*record) string {
		return "a file rewritten for its mode alone changes no value, so no other field names it; its rollback restores no bytes"
	}},
	{name: "an entry twice", apply: func(r *record) bool {
		if len(r.Secrets) == 0 {
			return false
		}
		r.Secrets = append(r.Secrets, r.Secrets[len(r.Secrets)-1])
		return true
	}},
	{name: "entries out of order", apply: func(r *record) bool {
		if len(r.Secrets) < 2 {
			return false
		}
		slices.Reverse(r.Secrets)
		return true
	}},
	{name: "an entry's app out of the grammar", apply: func(r *record) bool {
		if len(r.Secrets) == 0 {
			return false
		}
		r.Secrets[0].App = "../" + r.Secrets[0].App
		return true
	}},
	{name: "an entry's prev bytes changed", apply: func(r *record) bool {
		if len(r.Secrets) == 0 || r.Secrets[0].PrevSHA256 == "" {
			return false
		}
		r.Secrets[0].Prev = append(slices.Clone(r.Secrets[0].Prev), 'x')
		return true
	}},
	{name: "an entry's prev bytes dropped", apply: func(r *record) bool {
		if len(r.Secrets) == 0 || r.Secrets[0].PrevSHA256 == "" {
			return false
		}
		r.Secrets[0].Prev = nil
		return true
	}, valid: func(r *record) string {
		if r.Secrets[0].PrevSHA256 == digest(nil) {
			return "an empty previous file's bytes are none: dropping them changes nothing"
		}
		return ""
	}},
	{name: "an entry's new digest changed", apply: func(r *record) bool {
		if len(r.Secrets) == 0 {
			return false
		}
		r.Secrets[0].NewSHA256 = digest([]byte("other"))
		return true
	}},
	{name: "an entry's new digest cleared", apply: func(r *record) bool {
		if len(r.Secrets) == 0 || r.Secrets[0].NewSHA256 == "" {
			return false
		}
		r.Secrets[0].NewSHA256 = ""
		return true
	}},
	{name: "the secrets tree cleared", apply: func(r *record) bool {
		if r.SecretsTree == "" {
			return false
		}
		r.SecretsTree = ""
		return true
	}, valid: func(r *record) string {
		if len(r.SecretsInstalled) == 0 {
			return "with no env file listed the tree is recorded for the fast path alone; a commit with only a recipients file has one"
		}
		return ""
	}},
	{name: "the secrets tree not an id", apply: func(r *record) bool { r.SecretsTree = "tree"; return true }},
	{name: "a written app unlisted", apply: func(r *record) bool {
		for _, e := range r.Secrets {
			if e.NewSHA256 != "" {
				delete(r.SecretsInstalled, e.App)
				return true
			}
		}
		return false
	}},
	{name: "a removed app listed", apply: func(r *record) bool {
		for _, e := range r.Secrets {
			if e.NewSHA256 == "" {
				if r.SecretsInstalled == nil {
					r.SecretsInstalled = map[string]string{}
				}
				r.SecretsInstalled[e.App] = digest([]byte("x"))
				return true
			}
		}
		return false
	}},
	{name: "a written app listed at another digest", apply: func(r *record) bool {
		for _, e := range r.Secrets {
			if e.NewSHA256 != "" {
				r.SecretsInstalled[e.App] = digest([]byte("other"))
				return true
			}
		}
		return false
	}},
	{name: "a kept app listed at another digest", apply: func(r *record) bool {
		for app := range r.SecretsInstalled {
			if !slices.ContainsFunc(r.Secrets, func(e envEntry) bool { return e.App == app }) {
				r.SecretsInstalled[app] = digest([]byte("other"))
				return true
			}
		}
		return false
	}, valid: func(*record) string {
		return "an env file the push keeps is not in the set: advance holds its digest against the disk, not against the record"
	}},
	{name: "a listed app out of the grammar", apply: func(r *record) bool {
		if r.SecretsInstalled == nil {
			r.SecretsInstalled = map[string]string{}
		}
		r.SecretsInstalled["Bad"] = digest(nil)
		return true
	}},
	{name: "a name for an app not in the set", apply: func(r *record) bool {
		r.SecretsChanged = append(r.SecretsChanged, "nosuch/K")
		return true
	}},
	{name: "a name out of the grammar", apply: func(r *record) bool {
		r.SecretsRemoved = append(r.SecretsRemoved, "example")
		return true
	}},
	{name: "a name dropped", apply: func(r *record) bool {
		switch {
		case len(r.SecretsAdded) > 0:
			r.SecretsAdded = r.SecretsAdded[1:]
		case len(r.SecretsChanged) > 0:
			r.SecretsChanged = r.SecretsChanged[1:]
		case len(r.SecretsRemoved) > 0:
			r.SecretsRemoved = r.SecretsRemoved[1:]
		default:
			return false
		}
		return true
	}, valid: func(*record) string { return "the names are a report of what changed, never a decision" }},
	{name: "an app out of band", apply: func(r *record) bool {
		r.SecretsOutOfBand = append(r.SecretsOutOfBand, "example")
		return true
	}, valid: func(*record) string { return "the out-of-band list is a report, for any app the box ever listed" }},
	{name: "an out-of-band name out of the grammar", apply: func(r *record) bool {
		r.SecretsOutOfBand = append(r.SecretsOutOfBand, "a/b")
		return true
	}},
}

func TestRecordRelations(t *testing.T) {
	records, _ := generated(t)
	shapes := map[string]bool{}
	for _, r := range records {
		shapes[r.Phase+"/"+string(r.Origin)] = true
		if len(r.Secrets) > 0 {
			shapes["with env files"] = true
		}
	}
	for _, want := range []string{"installing/applier", "swapped/applier", "applied/applier", "rolling_back/applier", "no_change/applier", "with env files"} {
		if !shapes[want] {
			t.Errorf("no generated record is %s: %v", want, shapes)
		}
	}
	for i, r := range records {
		if err := r.valid(); err != nil {
			t.Fatalf("record %d, as root wrote it, does not read: %v\n%+v", i, err, r)
		}
		for _, m := range recordMutations {
			mutant := cloneRecord(r)
			if !m.apply(&mutant) {
				continue
			}
			reason := ""
			if m.valid != nil {
				reason = m.valid(&mutant)
			}
			err := mutant.valid()
			switch {
			case reason == "" && err == nil:
				t.Errorf("record %d (%s), %s: accepted", i, r.Phase, m.name)
			case reason != "" && err != nil:
				t.Errorf("record %d (%s), %s: refused (%v), though listed as valid: %s", i, r.Phase, m.name, err, reason)
			}
		}
	}
}

type appliedMutation struct {
	name  string
	apply func(a *applied) bool
	valid func(a *applied) string
}

var appliedMutations = []appliedMutation{
	{name: "the sha not an id", apply: func(a *applied) bool { a.SHA = "HEAD"; return true }},
	{name: "the secrets tree cleared", apply: func(a *applied) bool {
		if a.SecretsTree == "" {
			return false
		}
		a.SecretsTree = ""
		return true
	}, valid: func(a *applied) string {
		if len(a.Secrets) == 0 {
			return "with no env file listed the tree is the fast path's alone"
		}
		return ""
	}},
	{name: "the secrets tree not an id", apply: func(a *applied) bool { a.SecretsTree = "tree"; return true }},
	{name: "an app listed", apply: func(a *applied) bool {
		if a.Secrets == nil {
			a.Secrets = map[string]string{}
		}
		a.Secrets["zz"] = digest(nil)
		return true
	}, valid: func(a *applied) string {
		if a.SecretsTree != "" {
			return "the list is what the last apply left; no other field names its apps"
		}
		return ""
	}},
	{name: "an app out of the grammar", apply: func(a *applied) bool {
		if a.Secrets == nil {
			a.Secrets = map[string]string{}
		}
		a.Secrets["Bad"] = digest(nil)
		return true
	}},
	{name: "a digest not hex", apply: func(a *applied) bool {
		for app := range a.Secrets {
			a.Secrets[app] = "x"
			return true
		}
		return false
	}},
}

func TestAppliedRelations(t *testing.T) {
	_, baselines := generated(t)
	listed := false
	for i, a := range baselines {
		if err := a.valid(); err != nil {
			t.Fatalf("baseline %d, as root wrote it, does not read: %v\n%+v", i, err, a)
		}
		listed = listed || len(a.Secrets) > 0
		for _, m := range appliedMutations {
			mutant := a
			mutant.Secrets = maps.Clone(a.Secrets)
			if !m.apply(&mutant) {
				continue
			}
			reason := ""
			if m.valid != nil {
				reason = m.valid(&mutant)
			}
			err := mutant.valid()
			switch {
			case reason == "" && err == nil:
				t.Errorf("baseline %d, %s: accepted", i, m.name)
			case reason != "" && err != nil:
				t.Errorf("baseline %d, %s: refused (%v), though listed as valid: %s", i, m.name, err, reason)
			}
		}
	}
	if !listed {
		t.Error("no generated baseline lists an env file")
	}
}

// Root writes no record that would not read back, and no baseline: the
// write is refused as a failed write, which the transaction handles.
func TestWritesHoldTheRelations(t *testing.T) {
	b := newTestBox(t)
	a := b.applier(hooks{})
	bad := record{ID: strings.Repeat("ab", 16), Origin: originApplier, Phase: phaseInstalling}
	if err := a.writeRecord(&bad); err == nil || !strings.Contains(err.Error(), "would not read back") {
		t.Errorf("record: %v", err)
	}
	if err := a.putApplied(applied{SHA: strings.Repeat("c", 40), Secrets: map[string]string{"example": digest(nil)}}); err == nil || !strings.Contains(err.Error(), "would not read back") {
		t.Errorf("baseline: %v", err)
	}
}
