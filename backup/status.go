package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// Capturer runs a command and returns its stdout. Separate from
// Runner because only the reporting path needs the output; the
// backup path deliberately lets restic's own output through to the
// journal instead.
type Capturer func(ctx context.Context, name string, args ...string) ([]byte, error)

// Snapshot is the part of `restic snapshots --json` this reads.
type Snapshot struct {
	ID       string    `json:"id"`
	ShortID  string    `json:"short_id"`
	Time     time.Time `json:"time"`
	Hostname string    `json:"hostname"`
	Tags     []string  `json:"tags"`
	Paths    []string  `json:"paths"`
}

// AppStatus is one app's line in the report.
type AppStatus struct {
	App       App
	Latest    *Snapshot
	Snapshots int
	// LastSuccess is when a job on this box last finished cleanly, from
	// its record in the repository (CleanTag). Zero when none has: a
	// repository can hold snapshots from runs that ended in failure.
	LastSuccess time.Time
	// Running is whether this app's job is running right now. A first
	// backup of a large uploads dir can take hours, and without this the
	// report would show only "never", which reads as something broken.
	Running bool
	// NeverThere lists the declared files paths that no clean run has
	// found yet: an app's uploads dir before the first upload — or a
	// typo, which looks exactly the same to the job. Only the report can
	// tell them apart, by saying so to someone who knows which it is.
	NeverThere []string
}

// StaleAfter is when an hourly backup is late enough to be worth
// saying so: two missed runs, not one. A single missed hour is a slow
// upload or a reboot; two is something an operator should look at.
const StaleAfter = 2*time.Hour + 30*time.Minute

// Stale reports whether this app's newest backup is old enough to
// mention. An app that has never been backed up is stale by
// definition — that is the state worth catching, since it is what a
// typo in a `state` path or a never-configured repository looks like.
// Freshness is measured from the last run that finished cleanly,
// not from the newest snapshot: restic writes a snapshot even when it
// exits non-zero (unreadable sources), so an app whose every run
// fails part-way would otherwise look healthy for ever.
func (s AppStatus) Stale(now time.Time) bool {
	// The snapshots are the backup: an app with none in this repository
	// is stale, whatever else the listing holds.
	if s.Latest == nil {
		return true
	}
	// And no clean run from this box recorded in this repository is the
	// same answer. Falling back to the newest snapshot here would restore
	// the very failure the records exist to catch: restic writes a
	// snapshot and still exits non-zero, so a box whose every run fails
	// part-way would show a fresh snapshot every hour and stay green for
	// ever. A box that has never finished a run has no evidence that its
	// backups work — that includes a rebuilt one, until its first run.
	if s.LastSuccess.IsZero() {
		return true
	}
	return now.Sub(s.LastSuccess) > StaleAfter
}

// Status reads every hotserve snapshot in one call and matches them to
// the apps that declare state. Apps come from the running config, so
// an app whose `state` lines were removed drops off the report even
// though its old snapshots remain in the repository.
//
// host is this box's hostname: freshness is this box's runs, not those
// of another box writing to the same repository.
func Status(ctx context.Context, apps []App, capture Capturer, stagingRoot, host string) ([]AppStatus, error) {
	// --no-lock: status runs as root, and a lock it wrote into a
	// repository on this box would be root's — one the jobs could not
	// clear if status were killed before removing it. Listing snapshots
	// needs no lock. Two --tag flags are an OR: the backups, and the
	// clean-run records.
	out, err := capture(ctx, "restic", "snapshots", "--no-lock", "--json", "--tag", "hotserve", "--tag", CleanTag)
	if err != nil {
		return nil, fmt.Errorf("reading snapshots: %w", err)
	}
	var listed []Snapshot
	if err := json.Unmarshal(out, &listed); err != nil {
		return nil, fmt.Errorf("restic returned a snapshot list this version cannot read: %w", err)
	}
	snaps, _, records := cleanRecords(listed)
	byApp := map[string][]Snapshot{}
	for _, s := range snaps {
		for _, t := range s.Tags {
			if name, ok := strings.CutPrefix(t, "app:"); ok {
				byApp[name] = append(byApp[name], s)
			}
		}
	}
	statuses := make([]AppStatus, 0, len(apps))
	for _, app := range apps {
		st := AppStatus{App: app, Snapshots: len(byApp[app.Name])}
		for _, r := range records[app.Name] {
			if r.Hostname == host && r.Time.After(st.LastSuccess) {
				st.LastSuccess = r.Time
			}
		}
		// The seen list is written by a clean run on this box; until
		// there has been one it says nothing yet.
		if _, err := os.Stat(SeenPaths(filepath.Join(stagingRoot, app.Name))); err == nil {
			seen := readSeen(SeenPaths(filepath.Join(stagingRoot, app.Name)))
			for _, rel := range app.Files() {
				if !seen[filepath.Clean(rel)] {
					st.NeverThere = append(st.NeverThere, filepath.Clean(rel))
				}
			}
		}
		for i, s := range byApp[app.Name] {
			if st.Latest == nil || s.Time.After(st.Latest.Time) {
				st.Latest = &byApp[app.Name][i]
			}
		}
		statuses = append(statuses, st)
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].App.Name < statuses[j].App.Name })
	return statuses, nil
}

// FormatStatus writes the report. One line per app, with what it
// declares and when it was last copied, because the two questions an
// operator has are "is everything covered?" and "is it current?".
func FormatStatus(w io.Writer, statuses []AppStatus, now time.Time) {
	if len(statuses) == 0 {
		say(w, "No app declares state, so nothing is backed up.")
		say(w, "Add `state sqlite <file>` or `state files <dir>` to an app block, then reload.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	say(tw, "APP\tDECLARES\tSNAPSHOTS\tLAST BACKUP")
	for _, s := range statuses {
		last := "never"
		if s.Latest != nil {
			last = humanAge(now.Sub(s.Latest.Time)) + "  " + s.Latest.ShortID
		}
		// A snapshot newer than the last clean run means the runs
		// since then have been failing: show that rather than the
		// snapshot's age, which would read as a healthy backup.
		switch {
		case s.Latest != nil && s.LastSuccess.IsZero():
			last += "  (no clean run on this box)"
		case s.Latest != nil && !s.LastSuccess.IsZero() && s.Latest.Time.After(s.LastSuccess.Add(time.Minute)):
			last += "  (last clean run " + humanAge(now.Sub(s.LastSuccess)) + ")"
		}
		switch {
		case s.Running && s.Latest == nil:
			last += "  (first backup running now)"
		case s.Running:
			last += "  (backing up now)"
		}
		if s.Stale(now) {
			last += "  ⚠"
		}
		say(tw, "%s\t%s\t%d\t%s", s.App.Name, declares(s.App), s.Snapshots, last)
	}
	_ = tw.Flush()
	for _, s := range statuses {
		switch {
		case s.Stale(now) && s.Running:
			say(w, "\n%s has no current backup yet; one is running now. Its progress:\n    journalctl -u hotserve-backup-%s -f", s.App.Name, s.App.Name)
		case s.Stale(now):
			say(w, "\n%s has no current backup. What the last run did:\n    journalctl -u hotserve-backup-%s -n 30", s.App.Name, s.App.Name)
		}
		// Not a failure, and not counted by --check: an app may declare
		// where its data will go before it has any. But a typo in the
		// path looks exactly like that for ever, so it is said here.
		for _, rel := range s.NeverThere {
			say(w, "\n%s: %s has not existed at any backup yet — the app has not created it, or `state files %s` has the path wrong (%s)", s.App.Name, rel, rel, filepath.Join(s.App.Shared, rel))
		}
	}
}

func declares(a App) string {
	dbs, files := len(a.Databases()), len(a.Files())
	parts := make([]string, 0, 2)
	if dbs > 0 {
		parts = append(parts, fmt.Sprintf("%d database%s", dbs, plural(dbs)))
	}
	if files > 0 {
		parts = append(parts, fmt.Sprintf("%d path%s", files, plural(files)))
	}
	if len(parts) == 0 {
		return "nothing"
	}
	return strings.Join(parts, ", ")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// humanAge is deliberately coarse: the question is whether a backup is
// current, not when precisely it ran.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}
