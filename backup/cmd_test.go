package backup

import (
	"context"
	"os"
	"path/filepath"
	"slices"
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

// init exists to answer one question: will the hourly backups work?
// The hourly job gets the settings file and nothing else, so a
// credential that lives only in the operator's shell — AWS_PROFILE, a
// key exported for a one-off, a credentials file under their HOME —
// must not be able to make the check pass. Every command a person runs
// themselves keeps their environment, because a proxy or a locale is
// theirs to set.
func TestCommandEnvSealsInitFromTheOperatorsShell(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "only-in-the-shell")
	t.Setenv("HOME", "/root")
	settings := []string{"RESTIC_REPOSITORY=s3:example/bucket", "RESTIC_PASSWORD=from-init"}
	ctx := context.WithValue(context.Background(), envKey{}, settings)

	if !slices.Contains(commandEnv(ctx), "AWS_ACCESS_KEY_ID=only-in-the-shell") {
		t.Error("a command the operator runs by hand should keep their own environment")
	}

	env := commandEnv(context.WithValue(ctx, sealKey{}, "/tmp/scratch-home"))
	for _, kv := range env {
		if strings.HasPrefix(kv, "AWS_ACCESS_KEY_ID=") {
			t.Errorf("init borrowed a credential from the shell: %q — the jobs will not have it", kv)
		}
		if kv == "HOME=/root" {
			t.Error("init read root's home: a credentials file there would pass a check the jobs then fail")
		}
	}
	for _, want := range []string{
		"HOME=/tmp/scratch-home",
		"XDG_CACHE_HOME=/tmp/scratch-home/cache",
		"RESTIC_REPOSITORY=s3:example/bucket",
		"RESTIC_PASSWORD=from-init",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("sealed environment is missing %q: %v", want, env)
		}
	}
	// restic has to be findable, or every check fails for the wrong
	// reason — the jobs get systemd's default PATH, so this does too.
	var path string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	if path != jobPath {
		t.Errorf("PATH = %q, want the jobs' own %q", path, jobPath)
	}
}

// Setting PATH in the child's environment does not decide which binary
// runs: exec resolves the name against THIS process's PATH and only
// then hands over the environment. A hand-built restic earlier on
// root's PATH would otherwise create and check the repository at init
// while every hourly run used the packaged one.
func TestSealedRunsTheBinaryTheJobsWillRun(t *testing.T) {
	// sh stands in for restic: it is on the jobs' PATH everywhere this
	// runs, which restic is not (it is a Recommends, and the test image
	// for this package has no need of it).
	want, err := lookPathIn(jobPath, "sh")
	if err != nil {
		t.Skipf("nothing to resolve against here: %v", err)
	}
	// The operator's own PATH, with something of theirs first.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sh"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	var ran []string
	record := func(_ context.Context, name string, _ ...string) error {
		ran = append(ran, name)
		return nil
	}
	run, _ := sealed(dir, record, nil)
	if err := run(context.Background(), "sh", "-c", "true"); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 1 || ran[0] != want {
		t.Fatalf("init ran %q, want %q: the program has to be resolved on the jobs' PATH, or init checks the repository with one binary and the timer uses another", ran, want)
	}
}
