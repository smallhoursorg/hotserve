package status

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/hotserve/backups/record"
)

var now = time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)

const (
	idA = "aaaaaaaa11111111aaaaaaaa11111111aaaaaaaa11111111aaaaaaaa11111111"
	idB = "bbbbbbbb22222222bbbbbbbb22222222bbbbbbbb22222222bbbbbbbb22222222"
)

func ago(d time.Duration) time.Time { return now.Add(-d) }

func at(d time.Duration) *time.Time { t := ago(d); return &t }

// snap is a snapshot made d ago that the listing seen ago held; a nil
// seen is a snapshot no listing has held.
func snap(id string, d time.Duration, seen *time.Time) *record.Snapshot {
	return &record.Snapshot{ID: id, Time: ago(d), Seen: seen}
}

// sound is an app a run backed up ten minutes ago, whose restore was
// proven two days ago, with a listing since that held both.
func sound() *record.App {
	s := snap(idA, 10*time.Minute, at(9*time.Minute))
	return &record.App{
		Class: record.OK, Looked: "/var/lib/liveswap/blog/shared",
		Snapshot: s, LastOK: s, LastSnapshot: s,
		Items:         []record.Item{{Kind: "files", Path: "uploads", OK: true}},
		RestoreProven: &record.Drill{Snapshot: *snap(idB, 50*time.Hour, at(9*time.Minute)), Time: ago(48 * time.Hour)},
	}
}

func of(apps map[string]*record.App) *record.Status {
	return &record.Status{Started: ago(11 * time.Minute), Finished: ago(9 * time.Minute), Apps: apps, Listed: at(9 * time.Minute)}
}

func with(change func(*record.App)) *record.Status {
	a := sound()
	change(a)
	return of(map[string]*record.App{"blog": a, "shop": sound()})
}

func TestWhatStatusSays(t *testing.T) {
	for name, tc := range map[string]struct {
		st         *record.Status
		setUp      time.Time
		running    []Running
		unknown    []string
		runningErr error
		healthy    bool
		says       []string
		never      []string
	}{
		"no run yet": {
			st: &record.Status{Apps: map[string]*record.App{}}, healthy: true,
			says: []string{"pending first run"}, never: []string{"stale", "not proven"},
		},
		"declared, not deployed yet": {
			st: with(func(a *record.App) {
				*a = record.App{Class: record.Pending, Looked: "/var/lib/liveswap/blog/shared"}
			}), healthy: true,
			says:  []string{"blog: pending", "/var/lib/liveswap/blog/shared"},
			never: []string{"blog: stale", "blog: restore not proven"},
		},
		"fresh and proven": {
			st: with(func(*record.App) {}), healthy: true,
			says: []string{
				"blog: ok", "2026-09-20 14:50 UTC", "snapshot aaaaaaaa",
				"blog: restore last proven: 2026-09-18 15:00 UTC, snapshot bbbbbbbb",
				"/var/lib/liveswap/blog/shared",
			},
			never: []string{"stale", ": old", "no longer in the repository", idA},
		},
		"a minute short of stale": {
			st:      with(func(a *record.App) { a.LastOK = snap(idA, StaleAfter-time.Minute, at(9*time.Minute)) }),
			healthy: true, never: []string{"stale"},
		},
		"a minute past stale": {
			st:   with(func(a *record.App) { a.LastOK = snap(idA, StaleAfter+time.Minute, at(9*time.Minute)) }),
			says: []string{"blog: stale", "2026-09-20 11:59 UTC"}, never: []string{"shop: stale"},
		},
		"freshness is the last complete backup's, not the last snapshot's": {
			st: with(func(a *record.App) {
				a.Class, a.Detail = record.Incomplete, "restic could not read everything (exit 3)"
				a.LastOK = snap(idB, 30*time.Hour, at(9*time.Minute))
			}),
			says:  []string{"blog: incomplete", "exit 3", "blog: stale", "2026-09-19 09:00 UTC"},
			never: []string{"blog: ok"},
		},
		"a run that did not reach the app says so, and judges it by its last ok": {
			st: with(func(a *record.App) {
				a.Class, a.Snapshot, a.Items = record.NotRun, nil, nil
			}), healthy: true,
			says: []string{"blog: not run"}, never: []string{"blog: ok", "blog: stale"},
		},
		"never a complete backup": {
			st: with(func(a *record.App) {
				a.Class, a.Detail = record.Failed, "the repository's password is wrong (exit 12)"
				a.Snapshot, a.LastOK, a.LastSnapshot, a.RestoreProven = nil, nil, nil, nil
			}),
			says: []string{"blog: failed", "password is wrong", "blog: no complete backup yet"}, never: []string{"pending"},
		},
		"never proven, and why": {
			st: with(func(a *record.App) {
				a.RestoreProven = nil
				a.RestoreDrill = &record.Drill{Snapshot: *snap(idA, time.Hour, nil), Time: ago(time.Hour), Detail: "app.db: the copy is damaged"}
			}),
			says:  []string{"blog: restore not proven", "app.db: the copy is damaged", "hotserve-backup drill"},
			never: []string{"blog: restore last proven"},
		},
		"no drill at all yet": {
			st:   with(func(a *record.App) { a.RestoreProven = nil }),
			says: []string{"blog: restore not proven", "hotserve-backup drill"},
		},
		"proven a minute short of old": {
			st:      with(func(a *record.App) { a.RestoreProven.Time = ago(ProvenOldAfter - time.Minute) }),
			healthy: true, never: []string{": old"},
		},
		"proven a minute past old": {
			st:   with(func(a *record.App) { a.RestoreProven.Time = ago(ProvenOldAfter + time.Minute) }),
			says: []string{"blog: restore last proven: 2026-09-12 14:59 UTC", ": old"},
		},
		// The newest snapshot is the one a restore would reach for: a drill
		// of it that proved nothing is not made good by an older proof.
		"a later drill that proved nothing, beside what was proven": {
			st: with(func(a *record.App) {
				a.RestoreDrill = &record.Drill{Snapshot: *snap(idA, time.Hour, nil), Time: ago(time.Hour), Detail: "out of room"}
			}),
			says: []string{"blog: restore last proven", "blog: restore not proven", "out of room"},
		},
		"the last complete backup is gone from the repository": {
			st:    with(func(a *record.App) { a.LastOK = snap(idA, 70*time.Minute, at(69*time.Minute)) }),
			says:  []string{"blog: snapshot aaaaaaaa is no longer in the repository", "2026-09-20 13:51 UTC"},
			never: []string{"shop: snapshot"},
		},
		"a snapshot no listing ever held": {
			st:   with(func(a *record.App) { a.LastOK = snap(idA, 70*time.Minute, nil) }),
			says: []string{"blog: snapshot aaaaaaaa is no longer in the repository"},
		},
		"no listing yet says nothing of vanishing": {
			st: func() *record.Status {
				st := with(func(a *record.App) { a.LastOK = snap(idA, 10*time.Minute, nil) })
				st.Listed = nil
				st.Apps["shop"].LastOK = snap(idA, 10*time.Minute, nil)
				st.Apps["blog"].RestoreProven.Snapshot.Seen, st.Apps["shop"].RestoreProven.Snapshot.Seen = nil, nil
				return st
			}(), healthy: true, never: []string{"no longer in the repository"},
		},
		"a listing that failed since says nothing of vanishing": {
			st: func() *record.Status {
				st := with(func(*record.App) {})
				st.Listed = at(3 * time.Hour) // every Seen is newer
				return st
			}(), healthy: true, never: []string{"no longer in the repository"},
		},
		"a proven snapshot since pruned is said, and is still a proof": {
			st:      with(func(a *record.App) { a.RestoreProven.Snapshot.Seen = at(30 * time.Hour) }),
			healthy: true,
			says:    []string{"blog: restore last proven", "snapshot bbbbbbbb is no longer in the repository"},
		},
		"data missing": {
			st: with(func(a *record.App) {
				a.Class, a.Detail, a.Snapshot = record.DataMissing, "/var/lib/liveswap/blog/shared is gone, and was backed up before", nil
			}),
			says: []string{"blog: data missing", "was backed up before"}, never: []string{"pending"},
		},
		"a unit three hours in is running, not failed": {
			st:      with(func(*record.App) {}),
			running: []Running{{Role: "upload", App: "blog", State: "activating", Since: ago(3 * time.Hour)}},
			healthy: true,
			says:    []string{"running: upload of blog, since 2026-09-20 12:00 UTC"},
			never:   []string{"failed", "no unit of"},
		},
		"a unit of the whole run": {
			st:      with(func(*record.App) {}),
			running: []Running{{Role: "plan", State: "activating", Since: ago(time.Minute)}},
			healthy: true, says: []string{"running: plan, since 2026-09-20 14:59 UTC"},
		},
		"the manager could not be asked": {
			st: with(func(*record.App) {}), runningErr: errors.New("dial unix /run/dbus/system_bus_socket: no such file"),
			healthy: true,
			says:    []string{"could not ask systemd what is running", "system_bus_socket"},
			never:   []string{"no unit of"},
		},
		"nothing running": {
			st: with(func(*record.App) {}), healthy: true, says: []string{"no unit of a run, a restore or a drill is running"},
		},
		// Nothing else ages a record whose apps are all pending, or that
		// has none: a timer that died three weeks ago would read as "no
		// data yet", exit 0, for good.
		"the last run is weeks old and everything was pending": {
			st: func() *record.Status {
				st := of(map[string]*record.App{"blog": {Class: record.Pending, Looked: "/var/lib/liveswap/blog/shared"}})
				st.Started, st.Finished = ago(21*24*time.Hour), ago(21*24*time.Hour-time.Minute)
				return st
			}(),
			says: []string{"no run has finished since 2026-08-30 15:01 UTC"},
		},
		"the last run is weeks old and no app declared a backup": {
			st:   &record.Status{Started: ago(21 * 24 * time.Hour), Finished: ago(21 * 24 * time.Hour), Apps: map[string]*record.App{}},
			says: []string{"no app declares a backup", "no run has finished since"},
		},
		"no app declares a backup, as of a run just now": {
			st:      &record.Status{Started: ago(2 * time.Minute), Finished: ago(time.Minute), Apps: map[string]*record.App{}},
			healthy: true, says: []string{"no app declares a backup"}, never: []string{"no run has finished"},
		},
		"set up four hours ago, and still no run": {
			st: &record.Status{Apps: map[string]*record.App{}}, setUp: ago(4 * time.Hour),
			says: []string{"no run yet", "set up 2026-09-20 11:00 UTC"}, never: []string{"pending first run"},
		},
		"set up ten minutes ago": {
			st: &record.Status{Apps: map[string]*record.App{}}, setUp: ago(10 * time.Minute), healthy: true,
			says: []string{"pending first run"},
		},
		// A drill before any run writes a record with apps and no run.
		"a drill came first": {
			st: func() *record.Status {
				st := with(func(*record.App) {})
				st.Started, st.Finished = time.Time{}, time.Time{}
				return st
			}(), healthy: true,
			says: []string{"no run yet"}, never: []string{"0001"},
		},
		"a unit that ended while it was being asked about has no time": {
			st:      with(func(*record.App) {}),
			running: []Running{{Role: "upload", App: "blog", State: "activating"}},
			healthy: true, says: []string{"running: upload of blog"}, never: []string{"0001", "since"},
		},
		"a unit whose name says nothing here is still running": {
			st: with(func(*record.App) {}), unknown: []string{"hotserve_backup_newrole_x_y.service"}, healthy: true,
			says: []string{"running: hotserve_backup_newrole_x_y.service"}, never: []string{"no unit of"},
		},
		"a drill that could not begin": {
			st: func() *record.Status {
				st := with(func(*record.App) {})
				st.LastDrill = &record.Drill{Time: ago(time.Hour), Detail: "the Caddyfile could not be turned into a plan (exit 1)"}
				return st
			}(),
			says: []string{"the last drill, 2026-09-20 14:00 UTC, could not begin: the Caddyfile could not be turned into a plan"},
		},
		"when a drill last ran": {
			st: func() *record.Status {
				st := with(func(*record.App) {})
				st.LastDrill = &record.Drill{Time: ago(time.Hour)}
				return st
			}(), healthy: true, says: []string{"last drill 2026-09-20 14:00 UTC"},
		},
		// A listing that fails once is a warning; one that has failed for
		// longer than a backup may be old is what keeps a snapshot pruned
		// off the box looking like the last good backup.
		"the repository has gone unlisted for an hour": {
			st: func() *record.Status {
				st := with(func(*record.App) {})
				st.Unlisted = at(time.Hour)
				return st
			}(), healthy: true, says: []string{"the repository has not been listed since 2026-09-20 14:00 UTC"},
		},
		"the repository has gone unlisted for four hours": {
			st: func() *record.Status {
				st := with(func(*record.App) {})
				st.Unlisted = at(4 * time.Hour)
				return st
			}(), says: []string{"the repository has not been listed since 2026-09-20 11:00 UTC", "more than 3 hours"},
		},
		// A restore backs an app up first, and that is never "the last
		// complete backup": ok, with no last ok.
		"backed up by a restore, and never by a run": {
			st: with(func(a *record.App) {
				a.LastOK, a.RestoreProven = nil, nil
			}),
			says:  []string{"blog: ok: snapshot aaaaaaaa", "made by a restore", "blog: no complete backup yet"},
			never: []string{"blog: ok:  ("},
		},
		"not attempted": {
			st: with(func(a *record.App) {
				a.Class, a.Detail, a.Snapshot = record.NotAttempted, "the repository's password is wrong (exit 12)", nil
			}),
			says: []string{"blog: not attempted: the repository's password is wrong"},
		},
		"a run that ended early, and a warning": {
			st: func() *record.Status {
				st := with(func(*record.App) {})
				st.Error, st.Warning = "the Caddyfile could not be turned into a plan (exit 1)", "the last record could not be read"
				return st
			}(),
			says: []string{"the last run ended early: the Caddyfile could not be turned into a plan", "warning: the last record could not be read"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			lines, healthy := Report(Input{Record: tc.st, Now: now, SetUp: tc.setUp, Running: tc.running, Unrecognised: tc.unknown, RunningErr: tc.runningErr})
			out := strings.Join(lines, "\n")
			if healthy != tc.healthy {
				t.Errorf("healthy = %v, want %v:\n%s", healthy, tc.healthy, out)
			}
			for _, want := range tc.says {
				if !strings.Contains(out, want) {
					t.Errorf("does not say %q:\n%s", want, out)
				}
			}
			for _, not := range tc.never {
				if strings.Contains(out, not) {
					t.Errorf("says %q:\n%s", not, out)
				}
			}
		})
	}
}

// What an app wrote reaches a record through record.Text; a record
// written by hand, or by a later engine, has been through nothing.
func TestAReportHoldsNoControlCharacters(t *testing.T) {
	st := with(func(a *record.App) {
		a.Class, a.Detail = "fail\x1b[2Jed", "\x1b[2Jgone"
		a.Looked = "/var/lib/liveswap/blog/\x1b]0;x\x07shared"
		a.LastOK = &record.Snapshot{ID: "\x1b[2Jaaaa", Time: ago(time.Minute)}
		a.RestoreProven.Snapshot.ID = "\x1b[2Jbbbb"
	})
	st.Apps["sh\x1bop"] = sound()
	lines, _ := Report(Input{Record: st, Now: now})
	if len(lines) == 0 {
		t.Fatal("nothing was said")
	}
	for _, l := range lines {
		if strings.ContainsAny(l, "\x1b\x07\n") {
			t.Fatalf("a control character reached the terminal: %q", l)
		}
	}
}
