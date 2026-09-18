package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	ShortID string    `json:"short_id"`
	Time    time.Time `json:"time"`
	Tags    []string  `json:"tags"`
	Paths   []string  `json:"paths"`
}

// AppStatus is one app's line in the report.
type AppStatus struct {
	App       App
	Latest    *Snapshot
	Snapshots int
}

// StaleAfter is when an hourly backup is late enough to be worth
// saying so: two missed runs, not one. A single missed hour is a slow
// upload or a reboot; two is something an operator should look at.
const StaleAfter = 2*time.Hour + 30*time.Minute

// Stale reports whether this app's newest backup is old enough to
// mention. An app that has never been backed up is stale by
// definition — that is the state worth catching, since it is what a
// typo in a `state` path or a never-configured repository looks like.
func (s AppStatus) Stale(now time.Time) bool {
	return s.Latest == nil || now.Sub(s.Latest.Time) > StaleAfter
}

// Status reads every hotserve snapshot in one call and matches them to
// the apps that declare state. Apps come from the running config, so
// an app whose `state` lines were removed drops off the report even
// though its old snapshots remain in the repository.
func Status(ctx context.Context, apps []App, capture Capturer) ([]AppStatus, error) {
	out, err := capture(ctx, "restic", "snapshots", "--json", "--tag", "hotserve")
	if err != nil {
		return nil, fmt.Errorf("reading snapshots: %w", err)
	}
	var snaps []Snapshot
	if err := json.Unmarshal(out, &snaps); err != nil {
		return nil, fmt.Errorf("restic returned a snapshot list this version cannot read: %w", err)
	}
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
		fmt.Fprintln(w, "No app declares state, so nothing is backed up.")
		fmt.Fprintln(w, "Add `state sqlite <file>` or `state files <dir>` to an app block, then reload.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "APP\tDECLARES\tSNAPSHOTS\tLAST BACKUP")
	for _, s := range statuses {
		last := "never"
		if s.Latest != nil {
			last = humanAge(now.Sub(s.Latest.Time)) + "  " + s.Latest.ShortID
		}
		if s.Stale(now) {
			last += "  ⚠"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", s.App.Name, declares(s.App), s.Snapshots, last)
	}
	_ = tw.Flush()
	for _, s := range statuses {
		if s.Stale(now) {
			fmt.Fprintf(w, "\n%s has no current backup. What the last run did:\n    journalctl -u hotserve-backup-%s -n 30\n", s.App.Name, s.App.Name)
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
