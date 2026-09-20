// Package status says, from the record a run leaves and from what the
// manager says is running, whether the backups are fresh and whether a
// restore would work. It reads nothing itself and needs no privilege:
// the record holds no secret, and is for everyone to read.
package status

import (
	"fmt"
	"sort"
	"time"

	"github.com/smallhoursorg/hotserve/backups/record"
)

const (
	// StaleAfter is how old an app's last complete backup may be: two
	// hourly runs missed, and some grace.
	StaleAfter = 3 * time.Hour
	// ProvenOldAfter is how old a proven restore may be: the weekly
	// drill's period and a day.
	ProvenOldAfter = 8 * 24 * time.Hour
)

const timeFormat = "2006-01-02 15:04 MST"

// Running is one unit of a run, a restore or a drill that the manager
// holds right now.
type Running struct {
	// Role is what the unit does (upload, fetch, …) and App whose data
	// it does it to; a unit of the whole run has no App.
	Role, App string
	// State is the manager's ActiveState. A oneshot is "activating" for
	// as long as its command runs.
	State string
	Since time.Time
}

// Input is what Report is told.
type Input struct {
	Record *record.Status
	Now    time.Time
	// SetUp is when backups were set up, where that is known.
	SetUp time.Time
	// Running are the units of a run, a restore or a drill that the
	// manager holds; Unrecognised the names of those it holds whose
	// name says nothing here; RunningErr why it could not be asked.
	Running      []Running
	Unrecognised []string
	RunningErr   error
}

// Report is what status prints, and whether the last run is recent and
// every app that has anything to back up has a fresh complete backup
// and a restore proven lately.
//
// Freshness is the last complete backup's, whatever is in progress and
// whatever a later, incomplete snapshot holds; and a snapshot that a
// listing of the repository since did not hold is not a backup.
func Report(in Input) (lines []string, healthy bool) {
	st, now, running, runningErr := in.Record, in.Now, in.Running, in.RunningErr
	healthy = true
	say := func(format string, a ...any) { lines = append(lines, fmt.Sprintf(format, a...)) }
	bad := func(format string, a ...any) { healthy = false; say(format, a...) }
	when := func(t time.Time) string { return t.UTC().Format(timeFormat) }

	// short is a snapshot's id as it is printed. The record is root's,
	// and still nothing of it reaches a terminal unlooked at.
	short := func(id string) string { return fmt.Sprintf("%.8s", record.Text(id)) }

	// The run itself is aged, not only each backup: a record whose apps
	// are all pending, or that has none, would otherwise read "nothing
	// to back up yet" for as long as no run replaces it — a timer that
	// died weeks ago included.
	switch {
	case st.Started.IsZero() && !in.SetUp.IsZero() && now.Sub(in.SetUp) > StaleAfter:
		bad("no run yet, and backups were set up %s, more than %d hours ago", when(in.SetUp), int(StaleAfter.Hours()))
	case st.Started.IsZero():
		say("no run yet: pending first run")
	default:
		say("last run %s", when(st.Started))
		if len(st.Apps) == 0 && st.Error == "" {
			say("no app declares a backup; nothing is backed up")
		}
		if last := st.Finished; now.Sub(last) > StaleAfter {
			bad("no run has finished since %s, more than %d hours ago: what follows is as of then", when(last), int(StaleAfter.Hours()))
		}
	}
	if d := st.LastDrill; d != nil && d.Detail != "" {
		bad("the last drill, %s, could not begin: %s", when(d.Time), record.Text(d.Detail))
	} else if d != nil {
		say("last drill %s", when(d.Time))
	}
	// One listing unanswered is the run's warning, below. Unanswered for
	// longer than a backup may be old, it is what would keep a snapshot
	// pruned off the box looking like the last good backup.
	if u := st.Unlisted; u != nil && now.Sub(*u) > StaleAfter {
		bad("the repository has not been listed since %s, more than %d hours ago: whether it still holds the snapshots below is not known", when(*u), int(StaleAfter.Hours()))
	} else if u != nil {
		say("the repository has not been listed since %s", when(*u))
	}
	if st.Error != "" {
		bad("the last run ended early: %s", record.Text(st.Error))
	}
	if st.Warning != "" {
		say("warning: %s", record.Text(st.Warning))
	}

	// gone: a listing was answered after the snapshot was last seen.
	gone := func(s *record.Snapshot) bool {
		return st.Listed != nil && (s.Seen == nil || s.Seen.Before(*st.Listed))
	}
	lastSeen := func(s *record.Snapshot) string {
		if s.Seen == nil {
			return "no listing has held it"
		}
		return "last seen there " + when(*s.Seen)
	}

	names := make([]string, 0, len(st.Apps))
	for n := range st.Apps {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		app, n := st.Apps[name], record.Text(name)
		if app == nil {
			continue
		}
		at := record.Text(app.Looked)
		if app.Class == record.Pending {
			say("%s: pending first run: no data at %s yet", n, at)
			continue
		}
		switch {
		case app.Class == record.OK && app.LastOK != nil:
			say("%s: ok: last complete backup %s, snapshot %s (data at %s)", n, when(app.LastOK.Time), short(app.LastOK.ID), at)
		case app.Class == record.OK && app.Snapshot != nil:
			// A restore backs up what it is about to restore over, and that
			// is never the last complete backup.
			say("%s: ok: snapshot %s, made by a restore of what it then restored over (data at %s)", n, short(app.Snapshot.ID), at)
		case app.Class == record.NotRun:
			say("%s: not run: %s (data at %s)", n, record.Text(app.Detail), at)
		default:
			// The last run did not back this app up, however fresh the
			// run before it.
			bad("%s: %s: %s (data at %s)", n, record.Text(string(app.Class)), record.Text(app.Detail), at)
		}
		switch {
		case app.LastOK == nil:
			bad("%s: no complete backup yet", n)
		case gone(app.LastOK):
			bad("%s: snapshot %s is no longer in the repository (%s): it was the last complete backup, made %s", n, short(app.LastOK.ID), lastSeen(app.LastOK), when(app.LastOK.Time))
		case now.Sub(app.LastOK.Time) > StaleAfter:
			bad("%s: stale: the last complete backup is from %s, more than %d hours ago, snapshot %s", n, when(app.LastOK.Time), int(StaleAfter.Hours()), short(app.LastOK.ID))
		case app.Class != record.OK:
			say("%s: last complete backup %s, snapshot %s", n, when(app.LastOK.Time), short(app.LastOK.ID))
		}
		if app.LastSnapshot == nil {
			continue // nothing in the repository to prove a restore of
		}
		if p := app.RestoreProven; p != nil {
			line := fmt.Sprintf("%s: restore last proven: %s, snapshot %s", n, when(p.Time), short(p.Snapshot.ID))
			if now.Sub(p.Time) > ProvenOldAfter {
				bad("%s: old, more than %d days ago; `sudo hotserve-backup drill` proves one now", line, int(ProvenOldAfter.Hours()/24))
			} else {
				say("%s", line)
			}
			// Pruned since, off the box: what was proven was proven.
			if gone(&p.Snapshot) {
				say("%s: snapshot %s is no longer in the repository (%s); the restore proven was of it", n, short(p.Snapshot.ID), lastSeen(&p.Snapshot))
			}
		}
		// The newest snapshot is the one a restore reaches for: a drill of
		// it that proved nothing is not made good by an older proof.
		switch d := app.RestoreDrill; {
		case d != nil:
			bad("%s: restore not proven: %s (snapshot %s, %s); `sudo hotserve-backup drill` tries again", n, record.Text(d.Detail), short(d.Snapshot.ID), when(d.Time))
		case app.RestoreProven == nil:
			bad("%s: restore not proven: no drill of it has run; `sudo hotserve-backup drill` proves it", n)
		}
	}

	// Only the units of a run are looked for: the process that starts
	// them, between two of them, is not one.
	switch {
	case runningErr != nil:
		say("could not ask systemd what is running: %s", record.Text(runningErr.Error()))
	case len(running) == 0 && len(in.Unrecognised) == 0:
		say("no unit of a run, a restore or a drill is running")
	}
	for _, r := range running {
		what := record.Text(r.Role)
		if r.App != "" {
			what += " of " + record.Text(r.App)
		}
		// A oneshot is "activating" for as long as its command runs; any
		// other state is said as the manager says it.
		if r.State != "activating" {
			what += " (" + record.Text(r.State) + ")"
		}
		// No time: the unit ended while the manager was being asked.
		if r.Since.IsZero() {
			say("running: %s", what)
		} else {
			say("running: %s, since %s", what, when(r.Since))
		}
	}
	for _, name := range in.Unrecognised {
		say("running: %s", record.Text(name))
	}
	return lines, healthy
}
