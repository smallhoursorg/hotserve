package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/hotserve/backups/record"
	"github.com/smallhoursorg/hotserve/backups/unit"
)

// check_next is the group the next check reads; where it is not one —
// none yet, or a word the record should not hold — the ISO week's, in
// UTC, as for a first check. A clean check moves it to the group after.
func TestWhichGroupACheckReads(t *testing.T) {
	sunday := time.Date(2026, 9, 27, 3, 30, 0, 0, time.UTC) // ISO week 39
	for next, want := range map[string]string{
		"":      "39/52",
		"41/52": "41/52",
		"52/52": "52/52",
		"1/52":  "1/52",
		"0/52":  "39/52", // not a group
		"53/52": "39/52",
		"7/12":  "39/52",
		"01/52": "39/52",
		"x":     "39/52",
	} {
		if got := groupToRead(next, sunday); got != want {
			t.Errorf("told %q: group %q, want %q", next, got, want)
		}
	}
	// A word that is not a group is given back as it came, and read as
	// none by the next check.
	for g, want := range map[string]string{"40/52": "41/52", "51/52": "52/52", "52/52": "1/52", "1/52": "2/52", "x": "x", "": ""} {
		if got := groupAfter(g); got != want {
			t.Errorf("after %s: %q, want %q", g, got, want)
		}
	}
	seen, g := map[string]bool{}, "17/52"
	for i := 0; i < checkGroups; i++ {
		g = groupAfter(g)
		seen[g] = true
	}
	if len(seen) != checkGroups {
		t.Errorf("52 clean checks in a row read %d groups, not every one: %v", len(seen), seen)
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
			st, checked, _ := Drill(ctx, b.cfg, b)
			if st == nil || st.LastCheck == nil {
				t.Fatalf("no verdict: %+v", st)
			}
			c := st.LastCheck
			if checked == nil || *checked != *c {
				t.Errorf("the drill says it made %+v, and the record holds %+v", checked, c)
			}
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

// A probe given up at its clock whose unit could not be confirmed
// stopped is unreachable, said in words: that it did not answer, that
// its unit may run on and where to look — not Go's own error text.
func TestAProbeWhoseStopIsNotConfirmedSaysSoInWords(t *testing.T) {
	old := probeClock
	t.Cleanup(func() { probeClock = old })
	probeClock = 20 * time.Millisecond
	b := restoreBox(t)
	b.hang, b.stopErr = "probe", unit.ErrNotConfirmedGone
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, _, _ := Drill(ctx, b.cfg, b)
	c := st.LastCheck
	if c == nil || c.Class != record.CheckUnreachable ||
		!strings.Contains(c.Detail, "the repository did not answer within 20ms, and its unit could not be confirmed stopped (`systemctl status "+b.spec("probe").Name+"` says whether it runs on)") ||
		strings.Contains(c.Detail, "context deadline exceeded") {
		t.Errorf("%+v", c)
	}
}

// The check reads, and writes nothing that could hold a backup up: no
// lock [M75, the owner], no retrying for one; and the week's group.
func TestTheCheckTakesNoLockAndReadsItsWeeksGroup(t *testing.T) {
	// The group from the clock the drill reads, not a second reading of
	// one here: two either side of Sunday's midnight would disagree.
	old := checkClock
	t.Cleanup(func() { checkClock = old })
	checkClock = func() time.Time { return time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC) } // ISO week 39
	b := restoreBox(t)
	if _, _, err := Drill(context.Background(), b.cfg, b); err != nil {
		t.Fatal(err)
	}
	check := b.spec("repocheck")
	want := []string{"/usr/bin/restic", "check", "--no-lock", "--json", "--read-data-subset=39/52"}
	if !slices.Equal(check.Argv, want) {
		t.Errorf("the check's command:\n%q\nwant\n%q", check.Argv, want)
	}
	// The probe, which uses the cache a killed check leaves its own in,
	// is what removes one a month old: a check works in a temporary cache
	// of its own, and cleans nothing else [measured].
	if probe := b.spec("probe"); !slices.Equal(probe.Argv, []string{"/usr/bin/restic", "cat", "config", "--no-lock", "--cleanup-cache"}) {
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
			st, checked, err := Drill(ctx, b.cfg, b)
			if !errors.Is(err, context.Canceled) {
				t.Errorf("the drill's error: %v", err)
			}
			if checked != nil {
				t.Errorf("an interrupted drill says it made a check: %+v", checked)
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
		"a helper of another version": func(b *box) { b.outcome["check"] = unit.Outcome{Result: "exit-code", ExitStatus: OtherVersionStatus} },
	} {
		t.Run(name, func(t *testing.T) {
			b := restoreBox(t)
			must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
			must(t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{Apps: map[string]*record.App{}, LastCheck: last}))
			set(b)
			// The record holds a check — another drill's, written since the
			// caller last looked, for all the caller can tell — and this
			// drill made none: it says none, and the check on record is not
			// taken for its own.
			_, checked, _ := Drill(context.Background(), b.cfg, b)
			if checked != nil {
				t.Errorf("a drill that made no check says it made %+v", checked)
			}
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
	// It waits for a lock less long than its clock: a lock is said as
	// one, not as a storage that did not answer.
	want := []string{"/usr/bin/restic", "backup", "--quiet", "--json", "--retry-lock", "20m", "--host", "hotserve",
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
	if wait, err := time.ParseDuration(vouchRetryLock); err != nil || wait >= listClock {
		t.Errorf("the record waits %s for a lock, and is given up at %s", vouchRetryLock, listClock)
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
		// restic said it saved it, and named nothing: whether it did is
		// not known.
		"the record with no id": {
			set:   func(b *box) { b.vouch = "" },
			class: record.OK, vouch: true, warn: "blog: snapshot aaaaaaaa is a complete backup, and the record that says so may not have been written: restic exited 0 and its summary names no snapshot",
		},
		// restic exits 3 having saved a snapshot all the same.
		"the record exit 3": {
			set:   func(b *box) { b.outcome["vouch"] = unit.Outcome{Result: "exit-code", ExitStatus: 3} },
			class: record.OK, vouch: true, warn: "blog: snapshot aaaaaaaa is a complete backup, and the record that says so may not have been written: restic exited 3, having saved a snapshot of what it could read",
		},
		"the record locked out": {
			set:   func(b *box) { b.outcome["vouch"] = unit.Outcome{Result: "exit-code", ExitStatus: 11} },
			class: record.OK, vouch: true, warn: "blog: snapshot aaaaaaaa is a complete backup, and the record that says so could not be written into the repository (it stayed locked by something else for 20m, exit 11)",
		},
		// Ended from outside — systemctl stop reaches the bound unit before
		// the command sees its own stop — or by a signal: restic may have
		// saved it first.
		"the record's unit ended from outside": {
			set: func(b *box) {
				b.err["vouch"] = errors.New("hotserve_backup_vouch_blog_x.service: ended from outside before its command finished")
			},
			class: record.OK, vouch: true, warn: "blog: snapshot aaaaaaaa is a complete backup, and the record that says so may not have been written: hotserve_backup_vouch_blog_x.service: ended from outside",
		},
		"the record ended by a signal": {
			set:   func(b *box) { b.outcome["vouch"] = unit.Outcome{Result: "signal"} },
			class: record.OK, vouch: true, warn: "blog: snapshot aaaaaaaa is a complete backup, and the record that says so may not have been written: restic was ended by signal",
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

// A run stopped while the record is being written says so: the record
// may or may not be in the repository, which is not known, and the
// backup itself is sound. One written, or one that failed, before the
// stop reached it is said as what it is.
func TestARecordStoppedWhileItIsWrittenIsSaid(t *testing.T) {
	const stopped = "blog: snapshot aaaaaaaa is a complete backup, and the record that says so may not have been written: the command was stopped while it was being written"
	for name, tc := range map[string]struct {
		hang    bool  // the record's unit runs until it is stopped
		stopErr error // stopping it could not be confirmed
		exit    int   // how it ended where it did not hang
		early   bool  // the stop lands as the app's copies are removed, before the record
		warn    string
		never   string
	}{
		"stopped before it was begun":             {early: true, warn: "blog: snapshot aaaaaaaa is a complete backup, and the record that says so was not written: the command was stopped before it was begun"},
		"stopped while it is written":             {hang: true, warn: stopped},
		"stopped, and not confirmed gone":         {hang: true, stopErr: unit.ErrNotConfirmedGone, warn: stopped},
		"written just before the stop reached it": {never: "the record that says so"},
		"failed just before the stop reached it":  {exit: 1, warn: "blog: snapshot aaaaaaaa is a complete backup, and the record that says so could not be written into the repository (restic failed (exit 1)", never: "stopped while"},
	} {
		t.Run(name, func(t *testing.T) {
			b := newBox(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.hang {
				b.hang = "vouch"
			}
			b.stopErr = tc.stopErr
			if tc.exit != 0 {
				b.outcome["vouch"] = unit.Outcome{Result: "exit-code", ExitStatus: tc.exit}
			}
			// The stop — Ctrl-C, systemctl stop, a shutdown — lands as the
			// record's unit starts; or, early, as the app's last clean unit
			// does, which runs on to the end.
			cleans := 0
			b.before = func(s unit.Spec) {
				if strings.Contains(s.Name, "_clean_blog_") {
					cleans++
				}
				if !tc.early && strings.Contains(s.Name, "_vouch_") || tc.early && cleans == 2 && strings.Contains(s.Name, "_clean_blog_") {
					cancel()
				}
			}
			st, err := Run(ctx, b.cfg, b)
			if !errors.Is(err, context.Canceled) {
				t.Errorf("the run's error: %v", err)
			}
			if st == nil || st.Apps["blog"] == nil || st.Apps["blog"].Class != record.OK {
				t.Fatalf("the backup itself is sound, and is not ok: %+v", st)
			}
			if tc.warn != "" && !strings.Contains(st.Warning, tc.warn) {
				t.Errorf("warning %q\nwant %q", st.Warning, tc.warn)
			}
			if tc.never != "" && strings.Contains(st.Warning, tc.never) {
				t.Errorf("warning %q says %q", st.Warning, tc.never)
			}
			if tc.early && b.started("vouch") {
				t.Errorf("the record was begun after the stop: %s", b.roles())
			}
			if tc.hang && !slices.Contains(b.stopped, b.spec("vouch").Name) {
				t.Errorf("the record's unit was not stopped: %v", b.stopped)
			}
			// What the run wrote says so too.
			on, rerr := record.Read(filepath.Join(b.cfg.StateDir, "status.json"))
			if rerr != nil || on.Warning != st.Warning || on.Error == "" {
				t.Errorf("the record on disk: warning %q, error %q (%v)", on.Warning, on.Error, rerr)
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
		ask      string // --snapshot
		restored string // what is restored, where not the newest
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
		"an older one asked for, and vouched for itself": {
			set: func(b *box) { b.vouches = vouching(snapB, snapA) }, ask: snapB[:8],
			restored: snapB, asked: true,
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
			o.Snapshot = tc.ask
			o.Confirm = func(a RestoreAsk) bool { asked = &a; return true }
			rep, err := Restore(context.Background(), b.cfg, b, o)
			if err != nil {
				t.Fatal(err)
			}
			if want := cmp.Or(tc.restored, snapA); rep.Snapshot.ID != want {
				t.Errorf("restored %.8s, want %.8s", rep.Snapshot.ID, want)
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
		st, _, _ := Drill(bounded(t), b.cfg, b)
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
	// Not answering one app, the repository will not answer the next:
	// the run stops asking, and no unbounded upload is left to hang.
	t.Run("verify, in a run", func(t *testing.T) {
		b := newBox(t)
		two(t, b)
		b.hang = "verify"
		st, _ := Run(bounded(t), b.cfg, b)
		if app := st.Apps["blog"]; app.Class != record.Incomplete || !strings.Contains(app.Detail, "did not answer within") {
			t.Errorf("blog: %+v", app)
		}
		if n := strings.Count(b.roles(), "upload"); n != 1 {
			t.Errorf("uploads after the repository did not answer: %s", b.roles())
		}
		// In the repository's words, not blog's: shop made no snapshot.
		if app := st.Apps["shop"]; app == nil || app.Class != record.NotAttempted || !strings.Contains(app.Detail, "did not answer") || strings.Contains(app.Detail, "was made") {
			t.Errorf("shop: %+v", app)
		}
		if b.started("vouch") {
			t.Error("vouched for a snapshot that was not checked")
		}
	})
	// Given up at the clock and not confirmed gone: the repository did
	// not answer all the same.
	t.Run("history not confirmed gone, in a drill", func(t *testing.T) {
		b := restoreBox(t)
		two(t, b)
		b.hang, b.stopErr = "history", unit.ErrNotConfirmedGone
		_, _, _ = Drill(bounded(t), b.cfg, b)
		if n := strings.Count(b.roles(), "history"); n != 1 {
			t.Errorf("asked %d times: %s", n, b.roles())
		}
	})
	t.Run("size, in a drill", func(t *testing.T) {
		b := restoreBox(t)
		two(t, b)
		b.hang = "size"
		st, _, _ := Drill(bounded(t), b.cfg, b)
		if a := st.Apps["blog"]; a == nil || a.RestoreDrill == nil || !strings.Contains(a.RestoreDrill.Detail, "did not answer within") {
			t.Errorf("blog: %+v", a)
		}
		if n := strings.Count(b.roles(), "size"); n != 1 {
			t.Errorf("asked %d times: %s", n, b.roles())
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
		two(t, b)
		b.hang = "vouch"
		st, _ := Run(bounded(t), b.cfg, b)
		if app := st.Apps["blog"]; app.Class != record.OK || !strings.Contains(st.Warning, "did not answer within") {
			t.Errorf("%+v, warning %q", app, st.Warning)
		}
		if !slices.Contains(b.stopped, b.spec("vouch").Name) {
			t.Errorf("the record's unit was not stopped: %v", b.stopped)
		}
		// A storage that did not answer blog's record will not answer
		// shop's upload, which nothing bounds.
		if n := strings.Count(b.roles(), "upload"); n != 1 {
			t.Errorf("uploads after the repository did not answer: %s", b.roles())
		}
		// Nor blog's own first drill (Copilot on #161): not begun, and no
		// verdict of it on record, so that the next run drills it.
		if b.started("size") || b.started("fetch") {
			t.Errorf("blog's first drill was begun after the repository did not answer: %s", b.roles())
		}
		if app := st.Apps["blog"]; app.RestoreDrill != nil || app.RestoreProven != nil {
			t.Errorf("blog's first drill has a verdict: %+v %+v", app.RestoreDrill, app.RestoreProven)
		}
		if app := st.Apps["shop"]; app == nil || app.Class != record.NotAttempted || !strings.Contains(app.Detail, "did not answer") {
			t.Errorf("shop: %+v", app)
		}
		// The repository answering again, the next run drills blog.
		b.hang, b.specs = "", nil
		st, _ = Run(bounded(t), b.cfg, b)
		if app := st.Apps["blog"]; app == nil || app.RestoreProven == nil || !b.started("fetch") {
			t.Errorf("the next run did not drill blog: %s; %+v", b.roles(), app)
		}
	})
}

// A restore's own reads of the repository have no backstop (the owner,
// 2026-09-30): someone started it and can stop it, and a rebuilt box's
// first listing, from an empty cache, is the longest a box makes. Each
// is stopped by the stop, never by a clock.
func TestARestoresOwnReadsHaveNoBackstop(t *testing.T) {
	old := listClock
	t.Cleanup(func() { listClock = old })
	listClock = 20 * time.Millisecond
	for _, role := range []string{"history", "vouches", "size", "verify"} {
		t.Run(role, func(t *testing.T) {
			b := restoreBox(t)
			b.hang = role
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			b.before = func(s unit.Spec) {
				if strings.Contains(s.Name, "_"+role+"_") {
					time.AfterFunc(300*time.Millisecond, cancel)
				}
			}
			_, err := Restore(ctx, b.cfg, b, inPlace())
			if err == nil || strings.Contains(err.Error(), "did not answer") {
				t.Errorf("the restore's %s: %v", role, err)
			}
			if !slices.Contains(b.stopped, b.spec(role).Name) {
				t.Errorf("the %s unit was not stopped: %v", role, b.stopped)
			}
		})
	}
}

// Interrupted while the repository's records are asked, a restore is
// interrupted: it does not go on to ask its question.
func TestARestoreInterruptedWhileTheRecordsAreAskedAsksNothing(t *testing.T) {
	b := restoreBox(t)
	b.hang = "vouches"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_vouches_") {
			cancel()
		}
	}
	asked := false
	o := inPlace()
	o.Confirm = func(RestoreAsk) bool { asked = true; return false }
	_, err := Restore(ctx, b.cfg, b, o)
	if !errors.Is(err, context.Canceled) || asked {
		t.Errorf("asked %v, error %v", asked, err)
	}
}

// The run's listing of every app is of the apps' snapshots: the records
// of clean runs, one per app per run, are none of its business. (restic
// loads every snapshot file before it filters, so the tag narrows what
// the listing answers, not what it reads.)
func TestTheRunsListingLeavesTheRecordsOut(t *testing.T) {
	b := newBox(t)
	if _, err := Run(context.Background(), b.cfg, b); err != nil {
		t.Fatal(err)
	}
	if got, want := b.spec("listing").Argv, []string{"/usr/bin/restic", "snapshots", "--json", "--no-lock", "--host", "hotserve", "--tag", "hotserve"}; !slices.Equal(got, want) {
		t.Errorf("the listing: %q\nwant %q", got, want)
	}
}

// A check reads the group check_next names — the ISO week's where it
// names none — and its verdict says what the next reads: a clean one, the
// group after; any other, the same group again, a first check included
// (Copilot on #161); no verdict, check_next as it was.
func TestWhatAVerdictDoesToTheNextGroup(t *testing.T) {
	old := checkClock
	t.Cleanup(func() { checkClock = old })
	checkClock = func() time.Time { return time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC) } // ISO week 39
	damaged := func(b *box) {
		b.outcome["repocheck"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
		b.repocheck = `{"message_type":"summary","num_errors":1}`
	}
	for name, tc := range map[string]struct {
		next, reads, after string
		set                func(*box)
	}{
		"told 41, clean":       {next: "41/52", reads: "41/52", after: "42/52", set: func(*box) {}},
		"told 52, clean":       {next: "52/52", reads: "52/52", after: "1/52", set: func(*box) {}},
		"told 41, damaged":     {next: "41/52", reads: "41/52", after: "41/52", set: damaged},
		"told 41, unreachable": {next: "41/52", after: "41/52", set: func(b *box) { b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 1} }},
		"told 41, killed":      {next: "41/52", reads: "41/52", after: "41/52", set: func(b *box) { b.outcome["repocheck"] = unit.Outcome{Result: "signal"} }},
		"first, clean":         {reads: "39/52", after: "40/52", set: func(*box) {}},
		"first, damaged":       {reads: "39/52", after: "39/52", set: damaged},
		"first, unreachable":   {after: "39/52", set: func(b *box) { b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 1} }},
		"first, killed":        {reads: "39/52", after: "39/52", set: func(b *box) { b.outcome["repocheck"] = unit.Outcome{Result: "signal"} }},
		"not a group, clean":   {next: "x", reads: "39/52", after: "40/52", set: func(*box) {}},
		"first, ended from outside": {after: "", set: func(b *box) {
			b.err["repocheck"] = fmt.Errorf("hotserve_backup_repocheck_x.service: %w", unit.ErrEndedFromOutside)
		}},
		"told 41, ended from outside": {next: "41/52", reads: "41/52", after: "41/52", set: func(b *box) {
			b.err["repocheck"] = fmt.Errorf("hotserve_backup_repocheck_x.service: %w", unit.ErrEndedFromOutside)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			b := restoreBox(t)
			must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
			must(t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{Apps: map[string]*record.App{}, CheckNext: tc.next}))
			tc.set(b)
			st, _, _ := Drill(context.Background(), b.cfg, b)
			if tc.reads != "" {
				if argv := b.spec("repocheck").Argv; argv[len(argv)-1] != "--read-data-subset="+tc.reads {
					t.Errorf("the check read %q, want group %s", argv[len(argv)-1], tc.reads)
				}
			}
			on, err := record.Read(filepath.Join(b.cfg.StateDir, "status.json"))
			if err != nil || st.CheckNext != tc.after || on.CheckNext != tc.after {
				t.Errorf("check_next %q (on disk %q, %v), want %q", st.CheckNext, on.CheckNext, err, tc.after)
			}
		})
	}
}

// The case Copilot named, end to end: a box's first check finds damage
// in its week's group; a week later the next check reads that group
// again, not its own week's, and says damaged still; and once a check
// of it is clean, rotation moves on from there.
func TestAFirstCheckThatFindsDamageIsReadAgainTheWeekAfter(t *testing.T) {
	old := checkClock
	t.Cleanup(func() { checkClock = old })
	week := func(n int) func() time.Time { // n weeks after the Sunday of ISO week 39
		return func() time.Time { return time.Date(2026, 9, 27+7*n, 12, 0, 0, 0, time.UTC) }
	}
	b := restoreBox(t)
	read := func(clock func() time.Time, set func()) (string, *record.Status) {
		t.Helper()
		checkClock, b.specs = clock, nil
		set()
		st, _, _ := Drill(context.Background(), b.cfg, b)
		argv := b.spec("repocheck").Argv
		return strings.TrimPrefix(argv[len(argv)-1], "--read-data-subset="), st
	}
	damaged := func() {
		b.outcome["repocheck"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
		b.repocheck = `{"message_type":"summary","num_errors":1}`
	}
	clean := func() {
		delete(b.outcome, "repocheck")
		b.repocheck = `{"message_type":"summary","num_errors":0}`
	}
	if g, st := read(week(0), damaged); g != "39/52" || st.LastCheck.Class != record.CheckDamaged {
		t.Fatalf("week 39: read %s, %+v", g, st.LastCheck)
	}
	if g, st := read(week(1), damaged); g != "39/52" || st.LastCheck.Class != record.CheckDamaged {
		t.Errorf("week 40, after a first check found damage in 39: read %s, %+v", g, st.LastCheck)
	}
	if g, st := read(week(2), clean); g != "39/52" || st.LastCheck.Class != record.CheckClean || st.CheckNext != "40/52" {
		t.Errorf("week 41, the damaged group repaired: read %s, %+v, next %q", g, st.LastCheck, st.CheckNext)
	}
	if g, _ := read(week(3), clean); g != "40/52" {
		t.Errorf("week 42: read %s, want the group after the one read clean, 40/52", g)
	}
}

// Every command carries the group the next check reads, as it carries
// the last check.
func TestARunCarriesTheGroupToReadNext(t *testing.T) {
	b := newBox(t)
	must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
	must(t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{Apps: map[string]*record.App{}, CheckNext: "17/52"}))
	st, err := Run(context.Background(), b.cfg, b)
	if err != nil || st.CheckNext != "17/52" {
		t.Errorf("after a run: %q, %v", st.CheckNext, err)
	}
	on, rerr := record.Read(filepath.Join(b.cfg.StateDir, "status.json"))
	if rerr != nil || on.CheckNext != "17/52" {
		t.Errorf("on disk: %q, %v", on.CheckNext, rerr)
	}
}

// Damage says what else looks like it — a prune run off the box during
// the check, whose packs vanish under a check that takes no lock — and
// that the next check reads the same group again, which settles it;
// whole, however much restic found.
func TestDamageSaysWhatElseLooksLikeIt(t *testing.T) {
	for name, errs := range map[string]int{"one error": 1, "9999 errors in as many packs": 9999} {
		t.Run(name, func(t *testing.T) {
			b := restoreBox(t)
			b.outcome["repocheck"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
			var broken []string
			for i := 0; i < errs && errs > 1; i++ {
				broken = append(broken, fmt.Sprintf("%064x", i))
			}
			raw, _ := json.Marshal(broken)
			b.repocheck = fmt.Sprintf(`{"message_type":"summary","num_errors":%d,"broken_packs":%s}`, errs, raw)
			st, _, _ := Drill(context.Background(), b.cfg, b)
			c := st.LastCheck
			if c == nil || c.Class != record.CheckDamaged || strings.HasSuffix(c.Detail, "…") ||
				!strings.Contains(c.Detail, "a prune run off the box, or the storage failing, during the check looks the same, and the next check reads this group again") ||
				!strings.Contains(c.Detail, "journalctl -u "+b.spec("repocheck").Name) {
				t.Errorf("%+v", c)
			}
		})
	}
}

// A first check's group and its time are one reading of the clock: two
// either side of Sunday's midnight in UTC would record Monday's time
// beside Sunday's week (Copilot on #161).
func TestAChecksGroupAndTimeAreOneReadingOfTheClock(t *testing.T) {
	old := checkClock
	t.Cleanup(func() { checkClock = old })
	readings := []time.Time{
		time.Date(2026, 9, 27, 23, 59, 59, 999_000_000, time.UTC), // Sunday, ISO week 39
		time.Date(2026, 9, 28, 0, 0, 0, 1_000_000, time.UTC),      // Monday, ISO week 40
	}
	checkClock = func() time.Time {
		now := readings[0]
		if len(readings) > 1 {
			readings = readings[1:]
		}
		return now
	}
	b := restoreBox(t)
	st, _, _ := Drill(context.Background(), b.cfg, b)
	c := st.LastCheck
	if c == nil || c.Group != "39/52" || c.Group != checkGroup(c.Time) {
		t.Errorf("a first check read group %q and is recorded at %v, which is week %s", c.Group, c.Time, checkGroup(c.Time))
	}
}

// What every restic unit of a run, a drill and a restore is given, and
// is not. UTC, so that a snapshot and its record are in one zone: forget
// sorts each into days by the zone it was stored in [measured], and a
// Berlin box's 23:30 snapshot and its record, one at +02:00 and the
// other at Z, fell into different days — --keep-daily kept the snapshot
// and removed its record. And restic's own bound on a stuck request, as
// it comes (TestIntegrationResticRetriesAStuckRequestByItself): an
// upload, a fetch and the check have no backstop of ours because restic
// has one (D4).
func TestWhatEveryResticUnitIsGiven(t *testing.T) {
	if s := resticUnit("hotserve_backup_x_000000000000.service", "", "/env", []string{"/usr/bin/restic"}, ""); !slices.Contains(s.Environment, "TZ=UTC") {
		t.Errorf("resticUnit: %q", s.Environment)
	}
	b := restoreBox(t)
	_, err := Run(context.Background(), b.cfg, b)
	must(t, err)
	_, _, err = Drill(context.Background(), b.cfg, b)
	must(t, err)
	_, err = Restore(context.Background(), b.cfg, b, inPlace())
	must(t, err)
	for _, s := range b.specs {
		if len(s.Argv) == 0 || s.Argv[0] != b.cfg.Restic {
			continue
		}
		if !slices.Contains(s.Environment, "TZ=UTC") {
			t.Errorf("%s: %q", s.Name, s.Environment)
		}
		if slices.ContainsFunc(s.Argv, func(a string) bool { return strings.HasPrefix(a, "--stuck-request-timeout") }) {
			t.Errorf("%s sets restic's own bound on a stuck request: %q", s.Name, s.Argv)
		}
	}
}

// A plan that cannot be made — a broken Caddyfile — stops the drill's
// apps, and not the repository's check, which needs no plan.
func TestAPlanThatCannotBeMadeStillChecksTheRepository(t *testing.T) {
	b := restoreBox(t)
	b.outcome["plan"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	st, checked, err := Drill(context.Background(), b.cfg, b)
	if err == nil {
		t.Error("a drill with no plan exited 0")
	}
	if checked == nil || st.LastCheck == nil || *checked != *st.LastCheck || checked.Class != record.CheckClean {
		t.Errorf("checked %+v, record %+v (units %s)", checked, st, b.roles())
	}
	on, rerr := record.Read(filepath.Join(b.cfg.StateDir, "status.json"))
	if rerr != nil || on.LastCheck == nil || on.LastDrill == nil || on.LastDrill.Detail == "" {
		t.Errorf("on disk: %+v, %v", on, rerr)
	}
}

// Damage restic counted is never dropped: stopped, or its unit ended
// from outside, before the repository could be asked again, the check
// is recorded as failed — status unhealthy, the group read again —
// not as no verdict.
func TestDamageCountedAndThenStoppedIsNotLost(t *testing.T) {
	for name, how := range map[string]string{"interrupted": "interrupt", "the probe ended from outside": "outside"} {
		t.Run(name, func(t *testing.T) {
			b := restoreBox(t)
			must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
			must(t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{Apps: map[string]*record.App{}, CheckNext: "41/52",
				LastCheck: &record.Check{Time: time.Date(2026, 9, 20, 3, 30, 0, 0, time.UTC), Group: "40/52", Class: record.CheckClean}}))
			b.outcome["repocheck"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
			b.repocheck = `{"message_type":"summary","num_errors":3,"broken_packs":["3b47"]}`
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			probes := 0
			b.before = func(s unit.Spec) {
				if !strings.Contains(s.Name, "_probe_") {
					return
				}
				if probes++; probes == 2 {
					if how == "interrupt" {
						b.hang = "probe"
						cancel()
					} else {
						b.err["probe"] = fmt.Errorf("%s: %w (start job \"canceled\")", s.Name, unit.ErrEndedFromOutside)
					}
				}
			}
			st, checked, _ := Drill(ctx, b.cfg, b)
			c := st.LastCheck
			if c == nil || c.Class != record.CheckFailed || !strings.Contains(c.Detail, "restic check counted 3 errors") || !strings.Contains(c.Detail, "the next check reads this group again") || checked == nil {
				t.Errorf("verdict %+v, checked %+v", c, checked)
			}
			if st.CheckNext != "41/52" {
				t.Errorf("the group to read next moved on: %q", st.CheckNext)
			}
		})
	}
}

// A check or a probe whose unit is ended from outside — the drill's
// service being stopped, which the manager does to the units bound to
// it, maybe before the drill sees its own stop — found nothing out:
// no verdict, and the last check stands.
func TestACheckEndedFromOutsideIsNoVerdict(t *testing.T) {
	last := &record.Check{Time: time.Date(2026, 9, 20, 3, 30, 0, 0, time.UTC), Group: "38/52", Class: record.CheckClean}
	for _, role := range []string{"probe", "repocheck"} {
		t.Run(role, func(t *testing.T) {
			b := restoreBox(t)
			must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
			must(t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{Apps: map[string]*record.App{}, LastCheck: last}))
			b.err[role] = fmt.Errorf("hotserve_backup_%s_x.service: %w (start job \"canceled\")", role, unit.ErrEndedFromOutside)
			st, checked, _ := Drill(context.Background(), b.cfg, b)
			if checked != nil || st.LastCheck == nil || *st.LastCheck != *last {
				t.Errorf("a check ended from outside came to %+v; the record holds %+v", checked, st.LastCheck)
			}
		})
	}
}

// A drill notes when the first drill that checks ran on this record: a
// check that never comes to a verdict — every drill stopped, week on
// week — turns status unhealthy eight days after, where a fresh
// last_drill would otherwise hide it for good.
func TestADrillNotesWhenChecksBegan(t *testing.T) {
	b := restoreBox(t)
	b.hang = "repocheck"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_repocheck_") {
			cancel()
		}
	}
	st, _, _ := Drill(ctx, b.cfg, b)
	if st.LastCheck != nil || st.CheckSince == nil {
		t.Fatalf("check %+v, since %v", st.LastCheck, st.CheckSince)
	}
	first := *st.CheckSince
	// A later drill keeps the first date; a run carries it.
	b = restoreBox(t)
	must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
	must(t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{Apps: map[string]*record.App{}, CheckSince: &first}))
	st, err := Run(context.Background(), b.cfg, b)
	must(t, err)
	if st.CheckSince == nil || !st.CheckSince.Equal(first) {
		t.Errorf("a run: since %v, want %v", st.CheckSince, first)
	}
	st, _, _ = Drill(context.Background(), b.cfg, b)
	if st.CheckSince == nil || !st.CheckSince.Equal(first) {
		t.Errorf("a drill: since %v, want %v", st.CheckSince, first)
	}
}
