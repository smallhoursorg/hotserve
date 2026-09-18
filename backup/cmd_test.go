package backup

import (
	"bytes"
	"context"
	"os"
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
// through EnvironmentFile= exactly as the jobs get them. Any
// difference — root's shell environment, root's PATH, root's HOME,
// root owning what restic creates — is a check that passes at init
// and a backup that fails every hour after.
func TestAsJobRunsResticAsTheJobDoes(t *testing.T) {
	envDir := t.TempDir()
	view := jobView{User: "hotserve", Home: "/var/lib/hotserve-backup/.init-x"}
	settings := []string{"RESTIC_REPOSITORY=s3:s3.example.com/bucket", "RESTIC_PASSWORD=from-init"}

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
	for _, want := range []string{"--wait", "--pipe", "--expand-environment=no", "--property=User=hotserve"} {
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
		scratch = checkHome
	)
	app := testApp("blog", StateEntry{Kind: KindFiles, Path: "uploads"})
	opts := launchOpts("/var/lib/hotserve-backup")
	job := LaunchArgs(app, opts)

	var check []string
	run, _ := asJob(jobView{User: "hotserve", Home: scratch}, t.TempDir(),
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
	// Two differences by design. The job reads an app's data, and init's
	// checks read none.
	sharedBind := "--property=BindReadOnlyPaths=" + shared
	if !slices.Contains(j, sharedBind) {
		t.Fatalf("the job should bind the app's data read-only: %v", j)
	}
	j = slices.DeleteFunc(j, func(a string) bool { return a == sharedBind })
	// And systemd makes each its one writable directory where that kind
	// belongs: a job's under /var/lib, kept from run to run; a check's
	// on tmpfs, kept only until init removes it. Neither is a path the
	// launcher, which is root, makes or binds itself.
	take := func(all []string, want ...string) []string {
		for _, w := range want {
			if !slices.Contains(all, w) {
				t.Fatalf("missing %s in %v", w, all)
			}
			all = slices.DeleteFunc(all, func(a string) bool { return a == w })
		}
		return all
	}
	j = take(j, "--property=StateDirectory=hotserve-backup/blog", "--property=StateDirectoryMode=0750")
	c = take(c, "--property=RuntimeDirectory=hotserve-backup-check", "--property=RuntimeDirectoryMode=0700", "--property=RuntimeDirectoryPreserve=yes")
	for _, a := range append(slices.Clone(j), c...) {
		if strings.Contains(a, "BindPaths=") {
			t.Errorf("no unit's own directory is bound by path any more: %s", a)
		}
	}
	if strings.Join(j, "\n") != strings.Join(c, "\n") {
		t.Fatalf("the job and init's checks run in different sandboxes:\njob:\n%s\n\ncheck:\n%s", strings.Join(j, "\n"), strings.Join(c, "\n"))
	}
	if len(j) < 25 {
		t.Fatalf("only %d properties compared; the comparison would be vacuous", len(j))
	}
}

// captureQuiet against a real process, not a fake: stdout is what the
// caller parses (JSON, for `restic snapshots --json`), so stderr must
// never be mixed into it on success — but a caller that asks for stderr
// must get it whatever the exit status, because restic exits 0 over a
// refused delete and says so on stderr alone.
func TestCaptureQuietKeepsTheStreamsApart(t *testing.T) {
	capture := captureQuiet()
	script := func(exit string) []string {
		return []string{"-c", `printf '[{"ok":true}]'; printf 'Remove(<snapshot/x>) failed: 403 Forbidden' >&2; exit ` + exit}
	}

	var stderr bytes.Buffer
	out, err := capture(withStderr(context.Background(), &stderr), "sh", script("0")...)
	if err != nil {
		t.Fatalf("exit 0: %v", err)
	}
	if string(out) != `[{"ok":true}]` {
		t.Errorf("stdout must stay clean for parsing on success, got %q", out)
	}
	if !strings.Contains(stderr.String(), "403 Forbidden") {
		t.Errorf("the caller asked for stderr and did not get it on exit 0: %q", stderr.String())
	}

	// Nobody asked: stderr is not in the result on success.
	out, _ = capture(context.Background(), "sh", script("0")...)
	if strings.Contains(string(out), "Forbidden") {
		t.Errorf("stderr leaked into stdout: %q", out)
	}

	// On failure the result carries both.
	out, err = capture(context.Background(), "sh", script("1")...)
	if err == nil || !strings.Contains(string(out), "403 Forbidden") {
		t.Errorf("a failure should return stderr with stdout: %q, %v", out, err)
	}
}

// init keeps the repository password in this directory while its checks
// run, so it is used only as a real directory that is root's and that
// nobody else can enter — whatever an earlier run, or someone else,
// left there.
func TestRequireRootOnlyDir(t *testing.T) {
	open := t.TempDir()
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := requireRootOnlyDir(open); err == nil || !strings.Contains(err.Error(), "only root can enter") {
		t.Errorf("a directory others can enter must be refused, got %v", err)
	}

	closed := t.TempDir()
	if err := os.Chmod(closed, 0o700); err != nil {
		t.Fatal(err)
	}
	link := closed + "-link"
	if err := os.Symlink(closed, link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(link) })
	if err := requireRootOnlyDir(link); err == nil {
		t.Error("a link to a directory is not that directory, and must be refused")
	}

	// 0700 is enough only when root owns it.
	err := requireRootOnlyDir(closed)
	if os.Geteuid() == 0 && err != nil {
		t.Errorf("root's own 0700 directory must be accepted, got %v", err)
	}
	if os.Geteuid() != 0 && err == nil {
		t.Error("a 0700 directory that is not root's must be refused")
	}

	if err := requireRootOnlyDir(closed + "-missing"); err == nil {
		t.Error("a missing directory must be an error")
	}
}

// systemd 257 says "activating", and `is-active` exits 3, for a
// Type=oneshot unit whose command is still running (measured on Debian
// 13). A job is such a unit, so that is what a running backup looks
// like: read as "not running", a restore is never told to wait and
// `status` never says a backup is under way.
func TestARunningJobIsAnActivatingUnit(t *testing.T) {
	for state, want := range map[string]bool{
		"activating": true, "active": true, "deactivating": true, "reloading": true,
		"inactive": false, "failed": false, "": false, "unknown": false,
	} {
		if got := unitStateIsRunning(state); got != want {
			t.Errorf("unitStateIsRunning(%q) = %v, want %v", state, got, want)
		}
	}
}

// A restore puts back what an app's block declares, so an app that
// declares nothing — or is not there — is told what comes first.
func TestFindApp(t *testing.T) {
	apps := []App{{Name: "blog"}, {Name: "shop"}}
	if a, err := findApp(apps, "shop"); err != nil || a.Name != "shop" {
		t.Errorf("findApp(shop) = %+v, %v", a, err)
	}
	_, err := findApp(apps, "wiki")
	if err == nil || !strings.Contains(err.Error(), "blog, shop") || !strings.Contains(err.Error(), "the block comes first") {
		t.Errorf("want the apps that do declare state, and what to do, got %v", err)
	}
	_, err = findApp(nil, "wiki")
	if err == nil || !strings.Contains(err.Error(), "no app declares state") || !strings.Contains(err.Error(), "reload") {
		t.Errorf("want an error saying nothing declares state, got %v", err)
	}
}
