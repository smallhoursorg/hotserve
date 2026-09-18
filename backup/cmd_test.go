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

// init's checks are not an imitation of the hourly job: every restic
// command is a unit with the job's own sandbox, run as the job's user,
// with restic looked up on the unit's PATH and the settings arriving
// through EnvironmentFile= exactly as the jobs get them. Each way init
// used to differ — root's shell environment, root's PATH, root's HOME,
// root owning what restic created — was a check that passed at init
// and a backup that failed every hour after.
func TestAsJobRunsResticAsTheJobDoes(t *testing.T) {
	envDir := t.TempDir()
	view := jobView{User: "hotserve", Home: "/var/lib/hotserve-backup/.init-x", RepositoryPath: "/srv/backups"}
	settings := []string{"RESTIC_REPOSITORY=/srv/backups", "RESTIC_PASSWORD=from-init"}

	var gotName string
	var gotArgs []string
	var envDuring string
	var settingsLeaked bool
	record := func(ctx context.Context, name string, args ...string) error {
		gotName, gotArgs = name, args
		if s, _ := ctx.Value(envKey{}).([]string); len(s) > 0 {
			settingsLeaked = true
		}
		for _, a := range args {
			if f, ok := strings.CutPrefix(a, "--property=EnvironmentFile="); ok {
				info, err := os.Stat(f)
				if err != nil {
					t.Fatalf("the settings file must exist while the unit runs: %v", err)
				}
				if info.Mode().Perm() != 0o600 {
					t.Errorf("settings file mode = %o, want 600: it holds the repository password", info.Mode().Perm())
				}
				body, _ := os.ReadFile(f)
				envDuring = string(body)
			}
		}
		return nil
	}
	run, _ := asJob(view, envDir, record, nil)
	ctx := context.WithValue(context.Background(), envKey{}, settings)
	if err := run(ctx, "restic", "cat", "config"); err != nil {
		t.Fatal(err)
	}

	if gotName != "systemd-run" {
		t.Fatalf("ran %q, want systemd-run: the check has to be a unit like the job", gotName)
	}
	joined := strings.Join(gotArgs, "\n")
	for _, want := range []string{"--wait", "--pipe", "--expand-environment=no", "--property=User=hotserve", "--property=BindPaths=/srv/backups"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q from the check's unit:\n%s", want, joined)
		}
	}
	// restic is found inside the unit, on its PATH — not resolved here
	// (exec would use root's PATH) or by systemd-run (which uses the
	// caller's).
	tail := gotArgs[len(gotArgs)-6:]
	if want := []string{"/bin/sh", "-c", probeScript, "restic", "cat", "config"}; strings.Join(tail, " ") != strings.Join(want, " ") {
		t.Errorf("command = %q, want %q", tail, want)
	}
	if envDuring != renderEnvFile(settings) {
		t.Errorf("the check read different settings from the ones the jobs will get:\n%s", envDuring)
	}
	if settingsLeaked {
		t.Error("the password was also handed to systemd-run's own environment; the file is the only place it belongs")
	}
	if left, _ := os.ReadDir(envDir); len(left) != 0 {
		t.Errorf("a settings file was left behind: %v", left)
	}
}

func TestAsJobRunsNothingButRestic(t *testing.T) {
	run, _ := asJob(jobView{User: "hotserve", Home: t.TempDir()}, t.TempDir(),
		func(context.Context, string, ...string) error { return nil }, nil)
	if err := run(context.Background(), "sh", "-c", "id"); err == nil {
		t.Fatal("init's checks run restic and nothing else")
	}
}

// One sandbox, two users of it. If the hourly job's unit and init's
// checks ever built their property lists separately, every property
// one had and the other lacked would be a check that passed and a
// backup that failed. This compares the hardening both actually get.
func TestTheJobAndInitsChecksShareOneSandbox(t *testing.T) {
	const (
		staging = "/var/lib/hotserve-backup/blog"
		shared  = "/var/lib/liveswap/blog/shared"
		scratch = "/run/hotserve-backup/init-x"
		repo    = "/srv/backups"
	)
	app := testApp("blog", StateEntry{Kind: KindFiles, Path: "uploads"})
	opts := launchOpts("/var/lib/hotserve-backup")
	opts.RepositoryPath = repo
	job := LaunchArgs(app, opts)

	var check []string
	run, _ := asJob(jobView{User: "hotserve", Home: scratch, RepositoryPath: repo}, t.TempDir(),
		func(_ context.Context, _ string, args ...string) error { check = args; return nil }, nil)
	if err := run(context.Background(), "restic", "version"); err != nil {
		t.Fatal(err)
	}
	// Every property, binds included, with only the paths that name
	// *this* unit's own directories replaced — so a bind that one side
	// gained and the other did not (a writable /etc, say) is a
	// difference this test sees, not one it filters out.
	props := func(args []string, home string) []string {
		var out []string
		for _, a := range args {
			if !strings.HasPrefix(a, "--property=") {
				continue
			}
			if strings.HasPrefix(a, "--property=EnvironmentFile=") {
				a = "--property=EnvironmentFile=<settings>"
			}
			out = append(out, strings.ReplaceAll(a, home, "<home>"))
		}
		return out
	}
	j, c := props(job, staging), props(check, scratch)
	// The one difference by design: the job reads an app's data, and
	// init's checks read none.
	sharedBind := "--property=BindReadOnlyPaths=" + shared
	if !slices.Contains(j, sharedBind) {
		t.Fatalf("the job should bind the app's data read-only: %v", j)
	}
	j = slices.DeleteFunc(j, func(a string) bool { return a == sharedBind })
	if strings.Join(j, "\n") != strings.Join(c, "\n") {
		t.Fatalf("the job and init's checks run in different sandboxes:\njob:\n%s\n\ncheck:\n%s", strings.Join(j, "\n"), strings.Join(c, "\n"))
	}
	if len(j) < 25 {
		t.Fatalf("only %d properties compared; the comparison would be vacuous", len(j))
	}
}

// `run` mounts the repository writable into every job, so it mounts
// only a real one: not a directory a hand edit pointed the settings at,
// and not the empty mountpoint a disk that failed to mount leaves.
func TestRunBindsOnlyARealRepository(t *testing.T) {
	repo := t.TempDir()
	mustWrite(t, filepath.Join(repo, "config"), "restic")
	got, err := repositoryToBind([]string{"RESTIC_REPOSITORY=" + repo})
	if err != nil || got != repo {
		t.Errorf("a restic repository should be bound: %q, %v", got, err)
	}
	if _, err := repositoryToBind([]string{"RESTIC_REPOSITORY=" + t.TempDir()}); err == nil {
		t.Error("an empty mountpoint must not be bound into the jobs")
	}
	home := t.TempDir()
	mustWrite(t, filepath.Join(home, ".ssh", "id_ed25519"), "key")
	if _, err := repositoryToBind([]string{"RESTIC_REPOSITORY=" + home}); err == nil {
		t.Error("a directory with other things in it must not be bound into the jobs")
	}
	if got, err := repositoryToBind([]string{"RESTIC_REPOSITORY=s3:s3.example.com/bucket"}); err != nil || got != "" {
		t.Errorf("a remote repository needs no bind: %q, %v", got, err)
	}
}
