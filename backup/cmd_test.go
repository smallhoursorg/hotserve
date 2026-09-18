package backup

import (
	"strings"
	"testing"
)

func TestParseEntries(t *testing.T) {
	got, err := parseEntries([]string{"sqlite:app.db", "files:uploads", "sqlite:data/sessions.db"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []StateEntry{
		{Kind: KindSQLite, Path: "app.db"},
		{Kind: KindFiles, Path: "uploads"},
		{Kind: KindSQLite, Path: "data/sessions.db"},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A path may legitimately contain a colon, so only the first one
// separates kind from path.
func TestParseEntriesSplitsOnTheFirstColonOnly(t *testing.T) {
	got, err := parseEntries([]string{"files:odd:name/dir"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got[0].Path != "odd:name/dir" {
		t.Fatalf("path = %q", got[0].Path)
	}
}

func TestParseEntriesRejections(t *testing.T) {
	for _, tc := range []struct{ name, arg, want string }{
		{"no colon", "app.db", "<kind>:<path>"},
		{"empty path", "sqlite:", "<kind>:<path>"},
		{"unknown kind", "postgres:db", "unknown state kind"},
		{"kind only", "files", "<kind>:<path>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseEntries([]string{tc.arg})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// What `run` launches must be what `app` accepts: the two halves of
// this command talk to each other across a systemd unit, so a change
// to either vocabulary has to fail here.
func TestLaunchArgsRoundTripThroughParseEntries(t *testing.T) {
	app := testApp("blog",
		StateEntry{Kind: KindSQLite, Path: "app.db"},
		StateEntry{Kind: KindFiles, Path: "uploads"},
	)
	args := LaunchArgs(app, launchOpts("/var/lib/hotserve-backup"))
	var positional []string
	for _, a := range args {
		if strings.Contains(a, ":") && !strings.HasPrefix(a, "--") {
			positional = append(positional, a)
		}
	}
	got, err := parseEntries(positional)
	if err != nil {
		t.Fatalf("the job cannot parse what the launcher emits: %v", err)
	}
	if len(got) != len(app.State) {
		t.Fatalf("got %d entries, want %d", len(got), len(app.State))
	}
	for i := range got {
		if got[i] != app.State[i] {
			t.Fatalf("entry %d = %+v, want %+v", i, got[i], app.State[i])
		}
	}
}
