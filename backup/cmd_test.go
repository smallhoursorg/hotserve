package backup

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
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
	record := func(_ context.Context, c Cmd) error {
		gotName, gotArgs = c.Name, c.Args
		if len(c.Env) > 0 {
			settingsLeaked = true
		}
		for _, a := range c.Args {
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
	check := restic("cat", "config")
	check.Env = settings
	if err := asJob(view, envDir, record)(context.Background(), check); err != nil {
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

// Ctrl-C, or a check that ran out of time, ends systemd-run — the client.
// The unit is PID 1's, and restic in it goes on retrying for minutes,
// holding the streams this command reads through --pipe: waiting for
// systemd-run to finish is waiting for restic (measured: a wrong storage
// key held init for ten minutes). So here the client does not return
// until the unit has been stopped — and the stop has to come anyway, at
// once, from the context ending.
func TestAsJobStopsTheUnitOfACheckWhoseContextEnded(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	unitStopped := make(chan struct{})
	next := func(ctx context.Context, c Cmd) error {
		mu.Lock()
		calls = append(calls, c.Name+" "+strings.Join(c.Args, " "))
		mu.Unlock()
		if c.Name == "systemctl" {
			if ctx.Err() != nil {
				t.Error("the stop must not be cancelled by the context that just ended")
			}
			close(unitStopped)
			return nil
		}
		<-unitStopped // the client outlives its context for as long as the unit lives
		return errors.New("signal: killed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	x := asJob(jobView{User: "hotserve", Home: checkHome}, t.TempDir(), next)
	done := make(chan error, 1)
	go func() { done <- x(ctx, restic("cat", "config")) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a check whose context ended has not passed")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the check did not end when its context did: nothing stopped the unit it was waiting on")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || !strings.HasPrefix(calls[0], "systemd-run --unit="+checkUnit+" ") || calls[1] != "systemctl stop "+checkUnit+".service" {
		t.Errorf("want the check, in a named unit, then its stop: %q", calls)
	}
}

// A check that runs to its end is not stopped.
func TestAsJobLeavesAFinishedCheckAlone(t *testing.T) {
	var calls []string
	next := func(_ context.Context, c Cmd) error { calls = append(calls, c.Name); return nil }
	x := asJob(jobView{User: "hotserve", Home: checkHome}, t.TempDir(), next)
	if err := x(context.Background(), restic("cat", "config")); err != nil || len(calls) != 1 {
		t.Errorf("want one systemd-run and no stop: %q, %v", calls, err)
	}
}

// `status` is root, and its restic is not: the one restic call it makes
// is a unit, as the jobs' user, in their sandbox, with no app's data in
// its view — and its settings are read by systemd from the settings file
// itself, as a job's are. Nothing of them is in systemd-run's
// environment, and no copy is written anywhere. Its unit has a name of
// its own each time — a monitor and a person asking at once must not
// collide — so that a report whose context ends can stop it, and a name
// no app's unit can have.
func TestStatusRunsResticAsTheJobsDo(t *testing.T) {
	var got Cmd
	next := func(_ context.Context, c Cmd) error {
		got = c
		_, _ = c.Stdout.Write([]byte("[]"))
		return nil
	}
	x := inUnit(jobView{User: "hotserve", EnvFile: "/etc/hotserve/backup.env", Home: statusHome}, next)
	if _, err := Status(context.Background(), []App{testApp("blog", StateEntry{Kind: KindFiles, Path: "uploads"})}, x, "box-1"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got.Args, "\n")
	if got.Name != "systemd-run" || len(got.Env) != 0 {
		t.Fatalf("status ran %s with %d settings in its environment, want systemd-run with none", got.Name, len(got.Env))
	}
	for _, want := range []string{"--property=User=hotserve", "--property=PrivateUsers=yes", "--property=TemporaryFileSystem=/:ro", "--property=StateDirectory=hotserve-backup-status", "--property=EnvironmentFile=/etc/hotserve/backup.env"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q from status's unit:\n%s", want, joined)
		}
	}
	for _, never := range []string{"/var/lib/liveswap", "BindPaths="} {
		if strings.Contains(joined, never) {
			t.Errorf("status's unit must not have %q:\n%s", never, joined)
		}
	}
	unit, ok := strings.CutPrefix(got.Args[0], "--unit=")
	if !ok || !strings.HasPrefix(unit, "hotserve-backup_") {
		t.Errorf("status's unit is named so that it can be stopped, and as no app's unit can be (an app's name has no underscore): %q", got.Args[0])
	}
	if unit == oneOffUnit("restic") {
		t.Errorf("two reports at once need two names, got %q twice", unit)
	}
	if err := x(context.Background(), Cmd{Name: "sh"}); err == nil {
		t.Error("only restic is run this way")
	}
}

// The settings file is systemd's to read. `run` and `restore` are root,
// and hold it to this package's rules without opening it: a unit with
// the jobs' sandbox and nothing in its view is given the file by systemd
// and looks at its own environment.
func TestTheSettingsAreCheckedByAUnitNotByRoot(t *testing.T) {
	envFile := t.TempDir() + "/backup.env"
	if err := os.WriteFile(envFile, []byte("RESTIC_REPOSITORY=/srv/backups\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := LaunchOptions{Self: "/usr/bin/hotserve", EnvFile: envFile, User: "hotserve", StagingRoot: "/var/lib/hotserve-backup"}
	var got Cmd
	refuse := func(_ context.Context, c Cmd) error { got = c; return exited(1) }
	err := checkSettingsInUnit(context.Background(), o, refuse)
	if err == nil || !strings.Contains(err.Error(), envFile) {
		t.Fatalf("a unit that refuses the settings must stop the command, naming the file: %v", err)
	}
	joined := strings.Join(got.Args, "\n")
	for _, want := range []string{"--pipe", "--property=User=hotserve", "--property=EnvironmentFile=" + envFile, "--property=TemporaryFileSystem=/:ro", "/usr/bin/hotserve\nbackup\ncheck-settings"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q from the check's unit:\n%s", want, joined)
		}
	}
	for _, never := range []string{"StateDirectory=", "BindPaths=", "Environment=HOME"} {
		if strings.Contains(joined, never) {
			t.Errorf("the check writes nothing and reads no app's data, so it has no %q:\n%s", never, joined)
		}
	}
	// Never set up: said plainly, and no unit is started to find out.
	o.EnvFile = t.TempDir() + "/absent.env"
	got = Cmd{}
	err = checkSettingsInUnit(context.Background(), o, refuse)
	if err == nil || !strings.Contains(err.Error(), "hotserve backup init") || got.Name != "" {
		t.Fatalf("want an error naming init and no unit, got %v / %+v", err, got)
	}
}

// What the unit checks: its own environment, as systemd read it.
func TestCheckSettings(t *testing.T) {
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	if err := checkSettings(env(map[string]string{"RESTIC_REPOSITORY": "s3:host/bucket", "RESTIC_PASSWORD": "x"})); err != nil {
		t.Errorf("a backend URL with a password is accepted: %v", err)
	}
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"a path":                  {map[string]string{"RESTIC_REPOSITORY": "/srv/backups", "RESTIC_PASSWORD": "x"}, "not a backend URL"},
		"sftp":                    {map[string]string{"RESTIC_REPOSITORY": "sftp:host:/srv", "RESTIC_PASSWORD": "x"}, "sftp is not supported"},
		"a repository file":       {map[string]string{"RESTIC_REPOSITORY_FILE": "/etc/hotserve/repo", "RESTIC_PASSWORD": "x"}, "not supported"},
		"no repository":           {map[string]string{"RESTIC_PASSWORD": "x"}, "backup init"},
		"no password of any kind": {map[string]string{"RESTIC_REPOSITORY": "s3:host/bucket"}, "no restic password"},
	} {
		if err := checkSettings(env(tc.env)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want an error saying %q, got %v", name, tc.want, err)
		}
	}
}

// `backup restic --` is an operator's restic: systemd reads the settings
// for it and runs it as the backups' user — nothing here parses the one
// or switches to the other — outside the jobs' sandbox, because what is
// asked of it (a restore into a directory of the operator's choosing)
// needs the filesystem.
func TestPassthroughIsAUnitSystemdSetsUp(t *testing.T) {
	args := PassthroughArgs("/etc/hotserve/backup.env", "hotserve", []string{"snapshots", "--tag", "app:blog"})
	joined := strings.Join(args, "\n")
	for _, want := range []string{"--pty", "--pipe", "--wait", "--expand-environment=no", "--property=User=hotserve", "--property=EnvironmentFile=/etc/hotserve/backup.env"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q:\n%s", want, joined)
		}
	}
	if tail := args[len(args)-7:]; !slices.Equal(tail, []string{"/bin/sh", "-c", probeScript, "restic", "snapshots", "--tag", "app:blog"}) {
		t.Errorf("restic is looked up on the unit's PATH, with the operator's arguments as they typed them: %q", tail)
	}
	for _, never := range []string{"TemporaryFileSystem", "PrivateUsers"} {
		if strings.Contains(joined, never) {
			t.Errorf("the operator's restic is not sandboxed (%s): it restores where they point it", never)
		}
	}
}

func TestAsJobRunsNothingButRestic(t *testing.T) {
	x := asJob(jobView{User: "hotserve", Home: t.TempDir()}, t.TempDir(), fake(nil, nil))
	if err := x(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "id"}}); err == nil {
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
	x := asJob(jobView{User: "hotserve", Home: scratch}, t.TempDir(),
		func(_ context.Context, c Cmd) error { check = c.Args; return nil })
	if err := x(context.Background(), restic("version")); err != nil {
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
	sharedBind := "--property=BindReadOnlyPaths=-" + shared
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
