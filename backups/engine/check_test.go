package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/hotserve/backups/record"
	"github.com/smallhoursorg/hotserve/backups/unit"
)

// The first check reads its ISO week's group; each after it the group
// after the last one read (the owner, 2026-09-30: whatever weeks were
// missed, every group once in fifty-two checks).
func TestWhichGroupACheckReads(t *testing.T) {
	sunday := time.Date(2026, 9, 27, 3, 30, 0, 0, time.UTC) // ISO week 39
	for last, want := range map[string]string{
		"":      "39/52",
		"40/52": "41/52",
		"51/52": "52/52",
		"52/52": "1/52",
		"1/52":  "2/52",
		"0/52":  "39/52", // not a group: as though none had been read
		"53/52": "39/52",
		"7/12":  "39/52",
		"x":     "39/52",
	} {
		if got := nextGroup(last, sunday); got != want {
			t.Errorf("after %q: group %q, want %q", last, got, want)
		}
	}
	seen, last := map[string]bool{}, "17/52"
	for i := 0; i < checkGroups; i++ {
		last = nextGroup(last, sunday)
		seen[last] = true
	}
	if len(seen) != checkGroups {
		t.Errorf("52 checks in a row read %d groups, not every one: %v", len(seen), seen)
	}
}

// The ISO week's group, where a first check starts.
func TestTheWeeksGroup(t *testing.T) {
	for day, want := range map[string]string{
		"2026-01-01": "1/52", // ISO week 1 of 2026
		"2026-09-29": "40/52",
		"2026-12-27": "52/52",
		"2026-12-31": "1/52", // week 53 of 2026
		"2027-01-03": "1/52", // still week 53 of 2026
		"2027-01-04": "1/52", // week 1 of 2027
		"2027-01-11": "2/52",
	} {
		d, err := time.Parse(time.DateOnly, day)
		must(t, err)
		if got := checkGroup(d); got != want {
			t.Errorf("%s: group %q, want %q", day, got, want)
		}
	}
	// The week is UTC's, whatever zone the box's clock is in: Monday
	// 01:00 at UTC+2 is still Sunday, week 39, in UTC (Copilot on #161).
	if got := checkGroup(time.Date(2026, 9, 28, 1, 0, 0, 0, time.FixedZone("UTC+2", 2*60*60))); got != "39/52" {
		t.Errorf("Monday 01:00 at UTC+2: group %q, want UTC's week, 39/52", got)
	}
	seen := map[string]bool{}
	for w, d := 0, time.Date(2027, 1, 4, 3, 30, 0, 0, time.UTC); w < 52; w, d = w+1, d.AddDate(0, 0, 7) {
		seen[checkGroup(d)] = true
	}
	if len(seen) != checkGroups {
		t.Errorf("52 weeks in a row read %d groups, not every one: %v", len(seen), seen)
	}
}

// What the check meets, column by column (the plan's table): the
// verdict, and which units were started for it.
func TestWhatTheCheckMeets(t *testing.T) {
	exit := func(n int) unit.Outcome { return unit.Outcome{Result: "exit-code", ExitStatus: n} }
	ok := unit.Outcome{Result: "success"}
	for name, tc := range map[string]struct {
		set    func(*box)
		class  record.CheckClass
		detail string // in the verdict's words
		units  string // what the check started, after the drill's own
	}{
		"c1 sound": {
			set: func(*box) {}, class: record.CheckClean, units: "probe repocheck",
		},
		"c2 damage, the repository answering before and after": {
			set: func(b *box) {
				b.outcome["repocheck"] = exit(1)
				b.repocheck = `{"message_type":"summary","num_errors":2,"broken_packs":["3b47","de3f"]}`
			},
			class: record.CheckDamaged, detail: "2 damaged packs", units: "probe repocheck probe",
		},
		"c3 the storage lost half way": {
			set: func(b *box) {
				b.outcome["repocheck"] = exit(1)
				b.then["probe"] = []unit.Outcome{ok, exit(1)}
			},
			class: record.CheckUnreachable, detail: "the check could not finish", units: "probe repocheck probe",
		},
		"c4 no repository": {
			set:   func(b *box) { b.outcome["probe"] = exit(10) },
			class: record.CheckNoRepository, detail: "no repository", units: "probe",
		},
		"c5 wrong password": {
			set:   func(b *box) { b.outcome["probe"] = exit(12) },
			class: record.CheckWrongPassword, detail: "password", units: "probe",
		},
		"c6 exit 1": {
			set:   func(b *box) { b.outcome["probe"] = exit(1) },
			class: record.CheckUnreachable, detail: "could not be reached, or refused the key", units: "probe",
		},
		"c6 no answer in time": {
			set:   func(b *box) { b.hang = "probe"; probeClock = 20 * time.Millisecond },
			class: record.CheckUnreachable, detail: "did not answer within", units: "probe",
		},
		"c7 exit 0 and no summary": {
			set:   func(b *box) { b.repocheck = "" },
			class: record.CheckFailed, detail: "not believed", units: "probe repocheck",
		},
		"c7 exit 0 and a summary with errors": {
			set:   func(b *box) { b.repocheck = `{"message_type":"summary","num_errors":1}` },
			class: record.CheckFailed, detail: "not believed", units: "probe repocheck",
		},
		"c8 the password changed since the probe": {
			set:   func(b *box) { b.outcome["repocheck"] = exit(12) },
			class: record.CheckWrongPassword, units: "probe repocheck",
		},
		"c8 the repository went since the probe": {
			set:   func(b *box) { b.outcome["repocheck"] = exit(10) },
			class: record.CheckNoRepository, units: "probe repocheck",
		},
		"c9 a probe systemd could not set up": {
			set:   func(b *box) { b.outcome["probe"] = exit(217) },
			class: record.CheckFailed, detail: "systemd could not set the unit up", units: "probe",
		},
		"c9 a check ended by a signal": {
			set:   func(b *box) { b.outcome["repocheck"] = unit.Outcome{Result: "signal"} },
			class: record.CheckFailed, detail: "ended by signal", units: "probe repocheck",
		},
		"c9 a check the runner lost sight of": {
			set:   func(b *box) { b.err["repocheck"] = errors.New("lost sight of it") },
			class: record.CheckFailed, detail: "lost sight of it", units: "probe repocheck",
		},
		"c12 exit 11, which --no-lock never gives": {
			set:   func(b *box) { b.outcome["repocheck"] = exit(11) },
			class: record.CheckFailed, units: "probe repocheck",
		},
	} {
		t.Run(name, func(t *testing.T) {
			old := probeClock
			t.Cleanup(func() { probeClock = old })
			b := restoreBox(t)
			tc.set(b)
			drilled := len(strings.Fields("plan history unstage size fetch handover check unstage"))
			// An outer bound, so that a clock that is not there fails the row
			// rather than the test binary.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			st, _ := Drill(ctx, b.cfg, b)
			if st == nil || st.LastCheck == nil {
				t.Fatalf("no verdict: %+v", st)
			}
			c := st.LastCheck
			if c.Class != tc.class || !strings.Contains(c.Detail, tc.detail) {
				t.Errorf("verdict %q: %q\nwant %q, saying %q", c.Class, c.Detail, tc.class, tc.detail)
			}
			if c.Group != checkGroup(c.Time) || c.Time.IsZero() {
				t.Errorf("the verdict says no group or no time: %+v", c)
			}
			if got := strings.Fields(b.roles()); len(got) < drilled || strings.Join(got[drilled:], " ") != tc.units {
				t.Errorf("units: %s\nwant the drill's, then %s", b.roles(), tc.units)
			}
			if tc.class != record.CheckClean && tc.class != record.CheckDamaged && c.Detail == "" {
				t.Error("a check that came to no answer does not say why")
			}
			if tc.class == record.CheckDamaged && !strings.Contains(c.Detail, "journalctl -u "+b.spec("repocheck").Name) {
				t.Errorf("damage does not say where restic's own words are: %q", c.Detail)
			}
			if b.hang == "probe" && !slices.Contains(b.stopped, b.spec("probe").Name) {
				t.Errorf("a probe that did not answer in time was not stopped: %v", b.stopped)
			}
			on, err := record.Read(filepath.Join(b.cfg.StateDir, "status.json"))
			if err != nil || on.LastCheck == nil || on.LastCheck.Class != tc.class {
				t.Errorf("the record on disk: %+v, %v", on.LastCheck, err)
			}
		})
	}
}

// The check reads, and writes nothing that could hold a backup up: no
// lock [M75, the owner], no retrying for one; and the week's group.
func TestTheCheckTakesNoLockAndReadsItsWeeksGroup(t *testing.T) {
	b := restoreBox(t)
	if _, err := Drill(context.Background(), b.cfg, b); err != nil {
		t.Fatal(err)
	}
	check := b.spec("repocheck")
	want := []string{"/usr/bin/restic", "check", "--no-lock", "--json", "--cleanup-cache", "--read-data-subset=" + checkGroup(time.Now())}
	if !slices.Equal(check.Argv, want) {
		t.Errorf("the check's command:\n%q\nwant\n%q", check.Argv, want)
	}
	if probe := b.spec("probe"); !slices.Equal(probe.Argv, []string{"/usr/bin/restic", "cat", "config", "--no-lock"}) {
		t.Errorf("the probe's command: %q", probe.Argv)
	}
	// The config it prints is the repository's, decrypted: root's run
	// directory, never the journal.
	if out := b.spec("probe").StdoutFile; out == "" || !strings.HasPrefix(out, b.cfg.RunDir+"/") {
		t.Errorf("the probe's stdout goes to %q, not a file under %s", out, b.cfg.RunDir)
	}
	for _, s := range []unit.Spec{check, b.spec("probe")} {
		if s.User != backupUser || !s.Network || s.EnvironmentFile != b.cfg.EnvFile || s.CacheDirectory != "hotserve-backup" || len(s.Binds) != 0 || len(s.Capabilities) != 0 {
			t.Errorf("%s is not a query of the repository as the backup account, shown nothing: %+v", s.Name, s)
		}
		if slices.Contains(s.Argv, "--retry-lock") {
			t.Errorf("%s waits for a lock: %q", s.Name, s.Argv)
		}
	}
}

// Interrupted, the check comes to no verdict, and the last one stands.
func TestAnInterruptedCheckLeavesTheLastOne(t *testing.T) {
	last := &record.Check{Time: time.Date(2026, 9, 20, 3, 30, 0, 0, time.UTC), Group: "38/52", Class: record.CheckDamaged, Detail: "found"}
	for _, role := range []string{"probe", "repocheck"} {
		t.Run(role, func(t *testing.T) {
			b := restoreBox(t)
			must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
			must(t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{Apps: map[string]*record.App{}, LastCheck: last}))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			b.hang = role
			b.before = func(s unit.Spec) {
				if strings.Contains(s.Name, "_"+role+"_") {
					cancel()
				}
			}
			st, err := Drill(ctx, b.cfg, b)
			if !errors.Is(err, context.Canceled) {
				t.Errorf("the drill's error: %v", err)
			}
			on, rerr := record.Read(filepath.Join(b.cfg.StateDir, "status.json"))
			if rerr != nil || on.LastCheck == nil || *on.LastCheck != *last {
				t.Errorf("an interrupt was taken for a verdict: on disk %+v (%v), returned %+v", on.LastCheck, rerr, st)
			}
		})
	}
}

// Where the drill could not go on, the check is not made, and the last
// one stands.
func TestNoCheckWhereTheDrillCouldNotGoOn(t *testing.T) {
	last := &record.Check{Time: time.Date(2026, 9, 20, 3, 30, 0, 0, time.UTC), Group: "38/52", Class: record.CheckClean}
	for name, set := range map[string]func(*box){
		"restic not installed":        func(b *box) { b.haveProgram = func(p string) bool { return p != "/usr/bin/restic" } },
		"the plan could not be made":  func(b *box) { b.outcome["plan"] = unit.Outcome{Result: "exit-code", ExitStatus: 1} },
		"a helper of another version": func(b *box) { b.outcome["check"] = unit.Outcome{Result: "exit-code", ExitStatus: OtherVersionStatus} },
	} {
		t.Run(name, func(t *testing.T) {
			b := restoreBox(t)
			must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
			must(t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{Apps: map[string]*record.App{}, LastCheck: last}))
			set(b)
			_, _ = Drill(context.Background(), b.cfg, b)
			if b.started("probe") || b.started("repocheck") {
				t.Errorf("checked where the drill could not go on: %s", b.roles())
			}
			on, err := record.Read(filepath.Join(b.cfg.StateDir, "status.json"))
			if err != nil || on.LastCheck == nil || *on.LastCheck != *last {
				t.Errorf("the last check on disk: %+v, %v", on.LastCheck, err)
			}
		})
	}
}

// A run and a restore write the record too, and carry the last check
// as they carry the last drill.
func TestEveryCommandCarriesTheLastCheck(t *testing.T) {
	last := &record.Check{Time: time.Date(2026, 9, 20, 3, 30, 0, 0, time.UTC), Group: "38/52", Class: record.CheckDamaged, Detail: "found"}
	for name, do := range map[string]func(*box) error{
		"a run": func(b *box) error { _, err := Run(context.Background(), b.cfg, b); return err },
		"a restore, which backs up first": func(b *box) error {
			_, err := Restore(context.Background(), b.cfg, b, inPlace())
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := restoreBox(t)
			must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
			must(t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{Apps: map[string]*record.App{}, LastCheck: last}))
			if err := do(b); err != nil {
				t.Fatal(err)
			}
			on, err := record.Read(filepath.Join(b.cfg.StateDir, "status.json"))
			if err != nil || on.LastCheck == nil || !on.LastCheck.Time.Equal(last.Time) || on.LastCheck.Class != last.Class {
				t.Errorf("the last check on disk: %+v, %v", on.LastCheck, err)
			}
		})
	}
}

// An app that a run ended ok on is vouched for in the repository: a
// snapshot of its own, in a group of its own, naming the one it vouches
// for — what a rebuilt box has to go on.
func TestAnOKBackupIsVouchedForInTheRepository(t *testing.T) {
	b := newBox(t)
	st, err := Run(context.Background(), b.cfg, b)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(b.roles(), "plan clean dump upload verify clean vouch ") {
		t.Errorf("units: %s\nwant the record after the backup is verified and its copies removed", b.roles())
	}
	// At the snapshot's own time, to the second, which restic reads in
	// the zone it is given: so that a forget policy that keeps the
	// snapshot keeps its record, which falls in the same period.
	want := []string{"/usr/bin/restic", "backup", "--quiet", "--json", "--retry-lock", retryLock, "--host", "hotserve",
		"--time", "2026-09-02 07:08:09",
		"--tag", "hotserve-clean", "--tag", "vouches:" + snapA,
		"--stdin-from-command", "--stdin-filename", "hotserve-clean-blog", "--", "/usr/bin/echo", snapA}
	s := b.spec("vouch")
	if !slices.Equal(s.Argv, want) {
		t.Errorf("the record's command:\n%q\nwant\n%q", s.Argv, want)
	}
	if s.User != backupUser || !s.Network || s.EnvironmentFile != b.cfg.EnvFile || len(s.Binds) != 0 || len(s.Capabilities) != 0 {
		t.Errorf("the record's unit is not the backup account's, shown nothing: %+v", s)
	}
	if !slices.Contains(s.Environment, "TZ=UTC") {
		t.Errorf("the record's time is read in the unit's own zone: %q", s.Environment)
	}
	if st.Apps["blog"].Class != record.OK || st.Warning != "" {
		t.Errorf("%+v, warning %q", st.Apps["blog"], st.Warning)
	}
}

// Only a run that ended ok is vouched for — not one that ended
// incomplete, whatever made it so — and a record that could not be
// written is said, and does not make a good backup bad.
func TestOnlyAnOKRunIsVouchedForAndARecordNotWrittenIsSaid(t *testing.T) {
	for name, tc := range map[string]struct {
		set   func(*box)
		class record.Class
		vouch bool
		warn  string
	}{
		"restic exit 3":  {set: func(b *box) { b.uploadExit = map[string]int{"blog": 3} }, class: record.Incomplete},
		"an item absent": {set: func(b *box) { b.ls = func(string) string { return "" } }, class: record.Incomplete},
		"copies left": {
			set: func(b *box) {
				b.then["clean"] = []unit.Outcome{{Result: "success"}, {Result: "exit-code", ExitStatus: 1}}
			},
			class: record.Incomplete,
		},
		"the record not written": {
			set:   func(b *box) { b.outcome["vouch"] = unit.Outcome{Result: "exit-code", ExitStatus: 1} },
			class: record.OK, vouch: true, warn: "blog: snapshot aaaaaaaa is a complete backup, and the record that says so could not be written into the repository",
		},
		"the record with no id": {
			set:   func(b *box) { b.vouch = "" },
			class: record.OK, vouch: true, warn: "blog: snapshot aaaaaaaa is a complete backup, and the record that says so could not be written into the repository",
		},
		"no time for the snapshot in its listing": {
			set: func(b *box) {
				ls := b.ls
				b.ls = func(parent string) string {
					return strings.Replace(ls(parent), `,"time":"2026-09-02T07:08:09.123456789Z"`, "", 1)
				}
			},
			class: record.OK, warn: "blog: snapshot aaaaaaaa is a complete backup, and the record that says so was not written: its listing said nothing of when it was made",
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := newBox(t)
			tc.set(b)
			st, _ := Run(context.Background(), b.cfg, b)
			if st == nil || st.Apps["blog"] == nil {
				t.Fatalf("%+v", st)
			}
			if got := st.Apps["blog"].Class; got != tc.class {
				t.Errorf("class %q, want %q (%s)", got, tc.class, st.Apps["blog"].Detail)
			}
			if b.started("vouch") != tc.vouch {
				t.Errorf("vouched for: %v, want %v (%s)", b.started("vouch"), tc.vouch, b.roles())
			}
			if !strings.Contains(st.Warning, tc.warn) {
				t.Errorf("warning %q, want %q", st.Warning, tc.warn)
			}
		})
	}
}

// The warning of a record not written names the snapshot before
// anything a cut could take, whatever the app is called.
func TestARecordNotWrittenNamesItsSnapshotWhateverTheName(t *testing.T) {
	b := newBox(t)
	long := strings.Repeat("a", 63)
	must(t, os.MkdirAll(filepath.Join(b.root, long, "shared", "uploads"), 0o755))
	b.plan = strings.Replace(b.plan, `"blog":`, `"`+long+`":`, 1)
	ls := b.ls
	b.ls = func(parent string) string {
		return strings.ReplaceAll(ls(strings.Replace(parent, "/backup/"+long, "/backup/blog", 1)), "/backup/blog", "/backup/"+long)
	}
	b.outcome["vouch"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	st, _ := Run(context.Background(), b.cfg, b)
	if st == nil || !strings.Contains(st.Warning, "snapshot aaaaaaaa") || !strings.Contains(st.Warning, "could not be written") {
		t.Errorf("the warning: %q", st.Warning)
	}
}

// On a rebuilt box a restore asks the repository what vouches, and says
// what the box's own record would have said; it chooses as it would
// have (the owner, 2026-09-29).
func TestARestoreOnARebuiltBoxSaysWhatVouches(t *testing.T) {
	vouching := func(ids ...string) string {
		var out []string
		for _, id := range ids {
			out = append(out, `{"id":"`+snapC+`","time":"2026-09-02T00:00:01Z","tags":["hotserve-clean","vouches:`+id+`"]}`)
		}
		return "[" + strings.Join(out, ",") + "]"
	}
	for name, tc := range map[string]struct {
		set      func(*box)
		lastOK   string
		notKnown string
		asked    bool // whether the repository's records were asked for
	}{
		"the newest vouched for": {set: func(b *box) { b.vouches = vouching(snapB, snapA) }, asked: true},
		"an older one vouched for": {
			set:    func(b *box) { b.vouches = vouching(snapB) },
			lastOK: snapB, asked: true,
		},
		"none vouched for": {
			set:      func(b *box) { b.vouches = vouching() },
			notKnown: "nothing in the repository vouches for any snapshot of blog", asked: true,
		},
		"a record of a snapshot that is not there": {
			set:      func(b *box) { b.vouches = vouching(strings.Repeat("d", 64)) },
			notKnown: "nothing in the repository vouches for any snapshot of blog", asked: true,
		},
		"the records could not be asked": {
			set:      func(b *box) { b.outcome["vouches"] = unit.Outcome{Result: "exit-code", ExitStatus: 1} },
			notKnown: "could not be asked", asked: true,
		},
		"the records say nothing readable": {
			set:      func(b *box) { b.vouches = "not json" },
			notKnown: "could not be asked", asked: true,
		},
		// restic leaves a snapshot it cannot load out and exits 0 — a
		// cold cache, which is every rebuilt box's [measured]: an answer
		// with anything beside it is not taken for the whole.
		"restic said something beside the records": {
			set: func(b *box) {
				b.vouches = vouching(snapB)
				b.vouchesErr = `Ignoring "` + snapC + `", could not load snapshot: ciphertext verification failed`
			},
			notKnown: "not believed", asked: true,
		},
		"the box's own record knows": {
			set: func(b *box) {
				must(b.t, os.MkdirAll(b.cfg.StateDir, 0o755))
				ok := &record.Snapshot{ID: snapB, Time: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
				must(b.t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{Apps: map[string]*record.App{"blog": {Class: record.OK, LastOK: ok, LastSnapshot: ok}}}))
			},
			lastOK: snapB,
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := restoreBox(t)
			tc.set(b)
			var asked *RestoreAsk
			o := inPlace()
			o.Confirm = func(a RestoreAsk) bool { asked = &a; return true }
			rep, err := Restore(context.Background(), b.cfg, b, o)
			if err != nil {
				t.Fatal(err)
			}
			if rep.Snapshot.ID != snapA {
				t.Errorf("restored %.8s, not the newest", rep.Snapshot.ID)
			}
			if b.started("vouches") != tc.asked {
				t.Errorf("the repository's records asked for: %v, want %v (%s)", b.started("vouches"), tc.asked, b.roles())
			}
			for what, got := range map[string]*record.Snapshot{"report": rep.LastOK, "question": asked.LastOK} {
				id := ""
				if got != nil {
					id = got.ID
				}
				if id != tc.lastOK {
					t.Errorf("the %s names %.8s as the last ok, want %.8s", what, id, tc.lastOK)
				}
			}
			for what, got := range map[string]string{"report": rep.NotKnown, "question": asked.NotKnown} {
				if (tc.notKnown == "") != (got == "") || !strings.Contains(got, tc.notKnown) {
					t.Errorf("the %s says %q, want %q", what, got, tc.notKnown)
				}
			}
			if tc.asked {
				s := b.spec("vouches")
				if want := []string{"/usr/bin/restic", "snapshots", "--json", "--no-lock", "--host", "hotserve", "--tag", "hotserve-clean"}; !slices.Equal(s.Argv, want) || s.StderrFile == "" {
					t.Errorf("the records' listing: %q", s.Argv)
				}
			}
		})
	}
}

// A listing of the repository — an app's history, the run's listing,
// the verify of what a backup holds, the records — is given up at its
// backstop (D4: 30 minutes; the owner, 2026-09-29), the unit stopped,
// and what did not answer is not asked again for the next app.
func TestAListingThatDoesNotAnswerIsGivenUpAtItsBackstop(t *testing.T) {
	old := listClock
	t.Cleanup(func() { listClock = old })
	listClock = 20 * time.Millisecond
	// An outer bound, so that a backstop that is not there fails the row
	// rather than the test binary.
	bounded := func(t *testing.T) context.Context {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		t.Cleanup(cancel)
		return ctx
	}
	two := func(t *testing.T, b *box) {
		must(t, os.MkdirAll(filepath.Join(b.root, "shop", "shared"), 0o755))
		b.plan = strings.Replace(b.plan, `"apps":{`, `"apps":{"shop":{"files":["."]},`, 1)
	}
	t.Run("history, in a drill", func(t *testing.T) {
		b := restoreBox(t)
		two(t, b)
		b.hang = "history"
		st, _ := Drill(bounded(t), b.cfg, b)
		if n := strings.Count(b.roles(), "history"); n != 1 {
			t.Errorf("asked %d times: %s", n, b.roles())
		}
		if !slices.Contains(b.stopped, b.spec("history").Name) {
			t.Errorf("the history unit was not stopped: %v", b.stopped)
		}
		for _, app := range []string{"blog", "shop"} {
			if a := st.Apps[app]; a == nil || a.RestoreDrill == nil || !strings.Contains(a.RestoreDrill.Detail, "did not answer within") {
				t.Errorf("%s: %+v", app, a)
			}
		}
	})
	t.Run("verify, in a run", func(t *testing.T) {
		b := newBox(t)
		b.hang = "verify"
		st, _ := Run(bounded(t), b.cfg, b)
		if app := st.Apps["blog"]; app.Class != record.Incomplete || !strings.Contains(app.Detail, "did not answer within") {
			t.Errorf("%+v", app)
		}
		if b.started("vouch") {
			t.Error("vouched for a snapshot that was not checked")
		}
	})
	t.Run("the listing, in a run", func(t *testing.T) {
		b := newBox(t)
		b.hang = "listing"
		st, _ := Run(bounded(t), b.cfg, b)
		if !strings.Contains(st.Warning, "did not answer within") {
			t.Errorf("warning %q", st.Warning)
		}
	})
	// The record is a write of some four hundred bytes: bounded as a
	// listing is (the owner, 2026-09-30), and a record not written is
	// the run's warning, the app still ok.
	t.Run("the record, in a run", func(t *testing.T) {
		b := newBox(t)
		b.hang = "vouch"
		st, _ := Run(bounded(t), b.cfg, b)
		if app := st.Apps["blog"]; app.Class != record.OK || !strings.Contains(st.Warning, "did not answer within") {
			t.Errorf("%+v, warning %q", app, st.Warning)
		}
		if !slices.Contains(b.stopped, b.spec("vouch").Name) {
			t.Errorf("the record's unit was not stopped: %v", b.stopped)
		}
	})
	t.Run("the records, in a restore", func(t *testing.T) {
		b := restoreBox(t)
		b.hang = "vouches"
		rep, err := Restore(bounded(t), b.cfg, b, inPlace())
		if err != nil || !strings.Contains(rep.NotKnown, "did not answer within") {
			t.Errorf("%+v, %v", rep, err)
		}
	})
}

// The run's listing of every app is of the apps' snapshots: the records
// of clean runs, one per app per run, are none of its business, and
// would double what it loads.
func TestTheRunsListingLeavesTheRecordsOut(t *testing.T) {
	b := newBox(t)
	if _, err := Run(context.Background(), b.cfg, b); err != nil {
		t.Fatal(err)
	}
	if got, want := b.spec("listing").Argv, []string{"/usr/bin/restic", "snapshots", "--json", "--no-lock", "--host", "hotserve", "--tag", "hotserve"}; !slices.Equal(got, want) {
		t.Errorf("the listing: %q\nwant %q", got, want)
	}
}

// A check reads the group after the last one read, and only a check
// that read the data — clean, or damaged — moves it on.
func TestTheNextCheckReadsTheGroupAfterTheLastOneRead(t *testing.T) {
	for name, tc := range map[string]struct {
		last, reads, after string
		set                func(*box)
	}{
		"after 40": {last: "40/52", reads: "41/52", after: "41/52", set: func(*box) {}},
		"after 52": {last: "52/52", reads: "1/52", after: "1/52", set: func(*box) {}},
		"damage read": {last: "40/52", reads: "41/52", after: "41/52", set: func(b *box) {
			b.outcome["repocheck"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
			b.repocheck = `{"message_type":"summary","num_errors":1}`
		}},
		"unreachable":        {last: "40/52", after: "40/52", set: func(b *box) { b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 1} }},
		"killed by a signal": {last: "40/52", reads: "41/52", after: "40/52", set: func(b *box) { b.outcome["repocheck"] = unit.Outcome{Result: "signal"} }},
	} {
		t.Run(name, func(t *testing.T) {
			b := restoreBox(t)
			must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
			must(t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{Apps: map[string]*record.App{}, CheckRead: tc.last}))
			tc.set(b)
			st, _ := Drill(context.Background(), b.cfg, b)
			if tc.reads != "" {
				if argv := b.spec("repocheck").Argv; argv[len(argv)-1] != "--read-data-subset="+tc.reads {
					t.Errorf("the check read %q, want group %s", argv[len(argv)-1], tc.reads)
				}
			}
			on, err := record.Read(filepath.Join(b.cfg.StateDir, "status.json"))
			if err != nil || st.CheckRead != tc.after || on.CheckRead != tc.after {
				t.Errorf("last group read: %q (on disk %q, %v), want %q", st.CheckRead, on.CheckRead, err, tc.after)
			}
		})
	}
}

// Every command carries the group last read, as it carries the last
// check.
func TestARunCarriesTheGroupLastRead(t *testing.T) {
	b := newBox(t)
	must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
	must(t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{Apps: map[string]*record.App{}, CheckRead: "17/52"}))
	st, err := Run(context.Background(), b.cfg, b)
	if err != nil || st.CheckRead != "17/52" {
		t.Errorf("after a run: %q, %v", st.CheckRead, err)
	}
}

// Damage found while something else held the repository — a drill
// that met exit 11 — is still damage, and says what may explain it:
// a prune run off the box makes packs vanish under a check that takes
// no lock.
func TestDamageFoundBesideSomethingThatHeldTheRepositorySaysSo(t *testing.T) {
	b := restoreBox(t)
	b.outcome["fetch"] = unit.Outcome{Result: "exit-code", ExitStatus: 11}
	b.outcome["repocheck"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	b.repocheck = `{"message_type":"summary","num_errors":1}`
	st, _ := Drill(context.Background(), b.cfg, b)
	if c := st.LastCheck; c == nil || c.Class != record.CheckDamaged || !strings.Contains(c.Detail, "something else held the repository locked during this drill") {
		t.Errorf("%+v", c)
	}
	// And not said where nothing did.
	b = restoreBox(t)
	b.outcome["repocheck"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	b.repocheck = `{"message_type":"summary","num_errors":1}`
	st, _ = Drill(context.Background(), b.cfg, b)
	if c := st.LastCheck; c == nil || strings.Contains(c.Detail, "held the repository") {
		t.Errorf("%+v", c)
	}
}
