package backup

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func testApp(name string, state ...StateEntry) App {
	return App{Name: name, Shared: "/var/lib/liveswap/" + name + "/shared", State: state}
}

// deployedApp is an app whose data exists.
func deployedApp(t *testing.T, name string, state ...StateEntry) App {
	t.Helper()
	shared := filepath.Join(t.TempDir(), name, "shared")
	if err := os.MkdirAll(shared, 0o750); err != nil {
		t.Fatal(err)
	}
	return App{Name: name, Shared: shared, State: state}
}

func launchOpts(stagingRoot string) LaunchOptions {
	return LaunchOptions{
		Self:        "/usr/bin/hotserve",
		StagingRoot: stagingRoot,
		EnvFile:     "/etc/hotserve/backup.env",
		User:        "hotserve",
	}
}

// The sandbox is the whole point of launching the job in its own
// unit, so the properties that constitute it are pinned here: the
// view holds this app's shared dir read-only and this app's staging
// dir writable, and nothing else is named.
func TestLaunchArgsSandboxesEachAppToItsOwnData(t *testing.T) {
	app := testApp("blog",
		StateEntry{Kind: KindSQLite, Path: "app.db"},
		StateEntry{Kind: KindFiles, Path: "uploads"},
	)
	args := LaunchArgs(app, launchOpts("/var/lib/hotserve-backup"))
	joined := strings.Join(args, "\n")

	for _, want := range []string{
		"--unit=hotserve-backup-blog",
		"--property=User=hotserve",
		"--property=TemporaryFileSystem=/:ro",
		// The unit that reaches the network reads the app's data and
		// cannot write it, database or not.
		"--property=BindReadOnlyPaths=-/var/lib/liveswap/blog/shared",
		// systemd makes the job's own dir, owned by the job's user and
		// in its view; the launcher, which is root, never touches it.
		"--property=StateDirectory=hotserve-backup/blog",
		"--property=StateDirectoryMode=0750",
		"--property=Environment=HOME=/var/lib/hotserve-backup/blog",
		"--property=Environment=XDG_CACHE_HOME=/var/lib/hotserve-backup/blog/cache",
		"--property=EnvironmentFile=/etc/hotserve/backup.env",
		"--property=PrivateUsers=yes",
		"--property=NoNewPrivileges=yes",
		"--property=CapabilityBoundingSet=",
		"--property=SystemCallFilter=@system-service",
		"--property=Environment=GOGC=20",
		// The launcher's own Nice does not reach here: a transient unit
		// is started by the system manager, not forked from it, so
		// without this the hourly copy competes with the live apps.
		"--property=Nice=10",
		"--property=IOSchedulingClass=idle",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing property %q", want)
		}
	}
	// The app's own data is the only app data in the view.
	if strings.Contains(joined, "/var/lib/hotserve/") {
		t.Error("hotserve's own state (TLS keys, state.json) must not be in the view")
	}
	if n := strings.Count(joined, "/var/lib/liveswap/"); n != 2 {
		t.Errorf("liveswap paths named %d times, want 2 (the bind and the --shared flag): %v", n, args)
	}

	// The command the unit runs, after the properties — and what the
	// launcher logs, rather than the whole property list.
	want := []string{"/usr/bin/hotserve", "backup", "app", "--phase=upload", "--name=blog",
		"--shared=/var/lib/liveswap/blog/shared", "--staging=/var/lib/hotserve-backup/blog",
		"sqlite:app.db", "files:uploads"}
	if got := jobCommand(args); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("command:\n got %v\nwant %v", got, want)
	}
}

// No limit on how long a job runs or how much memory it may use. A time
// limit makes the first backup of a large uploads dir impossible: a job
// killed part-way leaves the next one to upload everything again
// (measured), so it would restart from nothing every hour, for ever,
// while the repository filled with orphaned data. A memory throttle
// cannot shrink restic's working set, only make it crawl. Hangs are
// bounded by restic's own per-request timeout instead.
func TestLaunchArgsDoNotCapHowLongOrHowLargeABackupMayBe(t *testing.T) {
	app := testApp("blog", StateEntry{Kind: KindFiles, Path: "uploads"})
	for _, a := range LaunchArgs(app, launchOpts("/var/lib/hotserve-backup")) {
		for _, capped := range []string{"RuntimeMaxSec", "MemoryHigh", "MemoryMax", "TimeoutStartSec", "TimeoutSec", "CPUQuota"} {
			if strings.Contains(a, "--property="+capped+"=") {
				t.Errorf("%s would stop a large first backup from ever completing: %s", capped, a)
			}
		}
	}
}

// SQLite creates the -shm file beside a WAL database to read it at all
// (measured: a read-only mount fails with "unable to open database
// file"), so whatever copies a database has the app's data writable.
// That is a unit of its own, with nothing else: no network, and no
// settings file, so no repository credentials. The unit that has both —
// restic's — has the data read-only. Everything else about the two
// sandboxes is the same.
func TestTheUnitThatCanWriteAnAppsDataCanReachNothing(t *testing.T) {
	app := testApp("blog", StateEntry{Kind: KindSQLite, Path: "app.db"}, StateEntry{Kind: KindFiles, Path: "uploads"})
	o := launchOpts("/var/lib/hotserve-backup")
	stage, upload := StageArgs(app, o), LaunchArgs(app, o)
	props := func(args []string) []string {
		var out []string
		for _, a := range args {
			if strings.HasPrefix(a, "--property=") || strings.HasPrefix(a, "--unit=") {
				out = append(out, a)
			}
		}
		return out
	}
	const (
		writable = "--property=BindPaths=-/var/lib/liveswap/blog/shared"
		readOnly = "--property=BindReadOnlyPaths=-/var/lib/liveswap/blog/shared"
		noNet    = "--property=PrivateNetwork=yes"
		settings = "--property=EnvironmentFile=/etc/hotserve/backup.env"
	)
	s, u := props(stage), props(upload)
	for _, want := range []string{writable, noNet} {
		if !slices.Contains(s, want) {
			t.Errorf("the staging unit lacks %s", want)
		}
	}
	for _, a := range s {
		if strings.Contains(a, "EnvironmentFile") {
			t.Errorf("the staging unit must hold no repository settings: %s", a)
		}
	}
	for _, want := range []string{readOnly, settings} {
		if !slices.Contains(u, want) {
			t.Errorf("the upload unit lacks %s", want)
		}
	}
	for _, never := range []string{writable, noNet} {
		if slices.Contains(u, never) {
			t.Errorf("the upload unit must not have %s", never)
		}
	}
	drop := func(all []string, these ...string) []string {
		return slices.DeleteFunc(slices.Clone(all), func(a string) bool { return slices.Contains(these, a) })
	}
	if rest, want := drop(s, writable, noNet), drop(u, readOnly, settings); !slices.Equal(rest, want) {
		t.Errorf("the two units differ by more than the data's bind, the network and the settings:\nstage  %v\nupload %v", rest, want)
	}
	if !slices.Contains(s, "--unit=hotserve-backup-blog") {
		t.Errorf("one unit name per app keeps a copy, an upload and a restore from ever overlapping: %v", s)
	}
	if got := jobCommand(stage); !slices.Equal(got[:4], []string{"/usr/bin/hotserve", "backup", "app", "--phase=stage"}) {
		t.Errorf("the staging unit runs %v", got)
	}
}

// An app with no database has nothing to copy, so it has no staging
// step: one unit, its data read-only.
func TestAnAppWithoutADatabaseHasNoStagingStep(t *testing.T) {
	var launched []string
	x := func(_ context.Context, c Cmd) error {
		for _, a := range c.Args {
			if phase, ok := strings.CutPrefix(a, "--phase="); ok {
				launched = append(launched, phase)
			}
		}
		return nil
	}
	apps := []App{
		deployedApp(t, "blog", StateEntry{Kind: KindSQLite, Path: "app.db"}, StateEntry{Kind: KindFiles, Path: "uploads"}),
		deployedApp(t, "wiki", StateEntry{Kind: KindFiles, Path: "pages"}),
	}
	var log strings.Builder
	if err := RunAll(context.Background(), apps, launchOpts(t.TempDir()), x, &log); err != nil {
		t.Fatal(err)
	}
	if strings.Join(launched, ",") != "stage,upload,upload" {
		t.Errorf("want blog copied then uploaded, and wiki uploaded: %v", launched)
	}
	if !strings.Contains(log.String(), "blog: copying its databases") || !strings.Contains(log.String(), "with no network and no repository settings") || strings.Contains(log.String(), "wiki: copying") {
		t.Errorf("the log must say which step has what:\n%s", log.String())
	}
}

// A copy that fails is that app's failure: nothing is uploaded for it —
// the staged copies are last hour's, or none — and the next app still
// gets its backup.
func TestAFailedCopyUploadsNothingForThatApp(t *testing.T) {
	var launched []string
	x := func(_ context.Context, c Cmd) error {
		joined := strings.Join(c.Args, " ")
		launched = append(launched, joined[strings.Index(joined, "--phase="):strings.Index(joined, " --shared=")])
		if strings.Contains(joined, "--phase=stage --name=blog") {
			return errors.New("exit status 1")
		}
		return nil
	}
	apps := []App{
		deployedApp(t, "blog", StateEntry{Kind: KindSQLite, Path: "app.db"}),
		deployedApp(t, "shop", StateEntry{Kind: KindSQLite, Path: "app.db"}),
	}
	err := RunAll(context.Background(), apps, launchOpts(t.TempDir()), x, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "blog") || strings.Contains(err.Error(), "shop") {
		t.Fatalf("want an error naming blog alone, got %v", err)
	}
	if got := strings.Join(launched, " | "); got != "--phase=stage --name=blog | --phase=stage --name=shop | --phase=upload --name=shop" {
		t.Errorf("launched: %s", got)
	}
}

// A repository is a backend URL, never a path on this box.
func TestCheckRepository(t *testing.T) {
	for _, repo := range []string{
		"s3:s3.example.com/bucket", "b2:bucket:path",
		"rest:https://example.com/", "azure:container:/", "gs:bucket:/", "swift:container:/",
	} {
		if err := CheckRepository(repo); err != nil {
			t.Errorf("%q: %v", repo, err)
		}
	}
	// sftp and rclone are restic backends, each refused with its own
	// reason: what they need is in files, and a job's sandbox holds none.
	if err := CheckRepository("sftp:user@host:/srv/backups"); err == nil || !strings.Contains(err.Error(), "sftp is not supported") {
		t.Errorf("sftp must be refused, saying why, got %v", err)
	}
	if err := CheckRepository("rclone:remote:path"); err == nil || !strings.Contains(err.Error(), "rclone is not supported") {
		t.Errorf("rclone must be refused, saying why, got %v", err)
	}
	for _, repo := range []string{
		"/srv/backups", "local:/srv/backups", "backups", "./backups", "C:/backups", "s3:", "", "ftp:host/x",
	} {
		if err := CheckRepository(repo); err == nil || !strings.Contains(err.Error(), "not a backend URL") {
			t.Errorf("%q must be refused as not a backend URL, got %v", repo, err)
		}
	}
}

// Nothing but the app's data and the unit's own dir is bound into a
// job: the repository is reached over the network.
func TestLaunchArgsBindNothingButTheAppAndItsStaging(t *testing.T) {
	app := testApp("blog", StateEntry{Kind: KindSQLite, Path: "app.db"})
	o := launchOpts("/var/lib/hotserve-backup")
	for _, a := range LaunchArgs(app, o) {
		if !strings.Contains(a, "BindPaths=") || strings.Contains(a, "BindReadOnlyPaths=/usr ") {
			continue
		}
		if a != "--property=BindPaths=/var/lib/hotserve-backup/blog" && a != "--property=BindPaths="+app.Shared {
			t.Errorf("unexpected bind: %s", a)
		}
	}
}

func TestLaunchArgsPassesEveryDeclaration(t *testing.T) {
	app := testApp("shop",
		StateEntry{Kind: KindSQLite, Path: "app.db"},
		StateEntry{Kind: KindFiles, Path: "uploads"},
		StateEntry{Kind: KindSQLite, Path: "data/sessions.db"},
	)
	args := LaunchArgs(app, launchOpts("/var/lib/hotserve-backup"))
	joined := strings.Join(args, " ")
	for _, want := range []string{"sqlite:app.db", "sqlite:data/sessions.db", "files:uploads"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %v", want, args)
		}
	}
}

// One app's failure must not cost the others their backup, and the
// run must still end in a non-zero exit so the timer's status is red.
func TestRunAllContinuesAfterOneFailureAndReportsIt(t *testing.T) {
	root := t.TempDir()
	apps := []App{
		deployedApp(t, "blog", StateEntry{Kind: KindFiles, Path: "uploads"}),
		deployedApp(t, "shop", StateEntry{Kind: KindFiles, Path: "uploads"}),
		deployedApp(t, "wiki", StateEntry{Kind: KindFiles, Path: "uploads"}),
	}
	var launched []string
	run := func(_ context.Context, _ string, args ...string) error {
		for _, a := range args {
			if name, ok := strings.CutPrefix(a, "--name="); ok {
				launched = append(launched, name)
				if name == "shop" {
					return errors.New("unit failed")
				}
			}
		}
		return nil
	}
	err := RunAll(context.Background(), apps, launchOpts(root), fake(run, nil), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "shop") {
		t.Fatalf("want an error naming the failed app, got %v", err)
	}
	if strings.Join(launched, ",") != "blog,shop,wiki" {
		t.Errorf("every app must be attempted, in order: %v", launched)
	}
	// The launcher is root, and what is under the staging root is
	// written by jobs: it makes nothing there. systemd makes each job's
	// dir (StateDirectory=).
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("the launcher made something under the staging root: %v", entries)
	}
}

// systemd makes each job's directory, and makes them under /var/lib: a
// staging root anywhere else would leave every job without one.
func TestCheckStagingRoot(t *testing.T) {
	for _, ok := range []string{"/var/lib/hotserve-backup", "/var/lib/hotserve-backup/", "/var/lib/x/y"} {
		if err := checkStagingRoot(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"/srv/staging", "/var/lib", "/var/lib/", "/var/lib/../../etc", "staging", ""} {
		if err := checkStagingRoot(bad); err == nil || !strings.Contains(err.Error(), "under /var/lib") {
			t.Errorf("%q must be refused, got %v", bad, err)
		}
	}
	for _, p := range homeProperties("/srv/staging/blog") {
		if strings.Contains(p, "Directory=") {
			t.Errorf("a home systemd cannot make gets no directory property: %s", p)
		}
	}
	if props := homeProperties(""); len(props) != 0 {
		t.Errorf("a unit with no directory of its own gets nothing: %v", props)
	}
}

// An app can declare state before it has ever been deployed. That is
// not a failure: it would otherwise turn the hourly timer red until
// the first deploy. It is the job that says so, with its exit status:
// the launcher looks at no app's data, because its unit cannot see every
// place that data may be (under /home, ProtectHome= shows it "no such
// file" for a dir that is there).
func TestRunAllSkipsAnAppWhoseJobSaysItHasNoData(t *testing.T) {
	apps := []App{
		testApp("never-deployed", StateEntry{Kind: KindSQLite, Path: "app.db"}),
		testApp("blog", StateEntry{Kind: KindFiles, Path: "uploads"}),
	}
	var launched []string
	run := func(_ context.Context, _ string, args ...string) error {
		for _, a := range args {
			if name, ok := strings.CutPrefix(a, "--name="); ok {
				launched = append(launched, name)
				if name == "never-deployed" {
					return exited(exitNoData)
				}
			}
		}
		return nil
	}
	var log strings.Builder
	if err := RunAll(context.Background(), apps, launchOpts(t.TempDir()), fake(run, nil), &log); err != nil {
		t.Fatalf("an undeployed app is not a failure: %v", err)
	}
	// The copy has no network: its "no data" is followed by the upload,
	// which can ask the repository whether that is news, and whose answer
	// is the one the run takes.
	if strings.Join(launched, ",") != "never-deployed,never-deployed,blog" {
		t.Errorf("want the undeployed app's copy, then its upload, then blog: %v", launched)
	}
	if !strings.Contains(log.String(), "never-deployed: nothing to back up yet, skipping") || strings.Contains(log.String(), "never-deployed: ok") || strings.Contains(log.String(), "FAILED") {
		t.Errorf("the run should say why it skipped, and not that it backed anything up: %q", log.String())
	}
}

// Stopping a run — the timer's unit, or Ctrl-C — stops the job it is
// waiting on, which is a unit of its own that nothing else would stop;
// and it stops no other. The next app's unit name may be an operator's
// restore, so with the context over nothing is started and nothing is
// stopped.
func TestRunAllStopsTheJobItIsWaitingOnAndNoOther(t *testing.T) {
	apps := []App{
		testApp("blog", StateEntry{Kind: KindFiles, Path: "uploads"}),
		testApp("shop", StateEntry{Kind: KindFiles, Path: "uploads"}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var calls []string
	unitStopped := make(chan struct{})
	x := func(ctx context.Context, c Cmd) error {
		mu.Lock()
		calls = append(calls, c.Name+" "+c.Args[len(c.Args)-1])
		mu.Unlock()
		if c.Name == "systemctl" {
			close(unitStopped)
			return nil
		}
		cancel() // stopped while blog's job runs
		<-unitStopped
		return errors.New("signal: killed")
	}
	var log strings.Builder
	err := RunAll(ctx, apps, launchOpts(t.TempDir()), x, &log)
	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("a run that was stopped says so: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || calls[1] != "systemctl hotserve-backup-blog.service" {
		t.Errorf("want blog's job, then its stop, and nothing for shop: %q", calls)
	}
	if strings.Contains(log.String(), "FAILED") {
		t.Errorf("a job stopped with its run did not fail: %q", log.String())
	}
}

// systemd expands $VAR in a unit's command from the unit's environment,
// which for these units holds the repository password and the storage
// key — and the command's arguments carry what the admin API said an app
// declares. Every systemd-run this package makes turns that off.
func TestNoUnitsCommandIsExpandedBySystemd(t *testing.T) {
	app := testApp("blog", StateEntry{Kind: KindSQLite, Path: "app.db"}, StateEntry{Kind: KindFiles, Path: "${RESTIC_PASSWORD}"})
	o := launchOpts("/var/lib/hotserve-backup")
	var made []string
	record := func(_ context.Context, c Cmd) error {
		if c.Name == "systemd-run" {
			made = c.Args
		}
		return nil
	}
	if err := ensureShared(context.Background(), filepath.Join(t.TempDir(), "not-there"), "hotserve", record); err != nil {
		t.Fatal(err)
	}
	for name, argv := range map[string][]string{
		"the copy":           StageArgs(app, o),
		"the upload":         LaunchArgs(app, o),
		"a restore":          RestoreArgs(app, o, "s1full", true),
		"the settings check": settingsCheckArgs(o.Self, o.User, o.EnvFile),
		"restic in a unit":   resticInUnit(jobView{User: o.User, EnvFile: o.EnvFile}, []string{"snapshots"}),
		"backup restic --":   PassthroughArgs(o.EnvFile, o.User, []string{"snapshots"}),
		"making shared/":     made,
	} {
		if !slices.Contains(argv, "--expand-environment=no") {
			t.Errorf("%s: systemd would expand $VAR in its command from the settings: %v", name, argv)
		}
	}
}

// init's checks run in one named unit, and an app may be called `check`.
func TestInitsCheckUnitIsNoAppsUnit(t *testing.T) {
	if !appNameRe.MatchString("check") {
		t.Fatal("setup: `check` is a name an app may have")
	}
	if rest, ok := strings.CutPrefix(checkUnit, "hotserve-backup-"); ok && appNameRe.MatchString(rest) {
		t.Errorf("%s is the unit of an app called %q: init stopping its check would stop that app's backup, or its restore", checkUnit, rest)
	}
}

func TestRunAllWithNothingDeclaredIsNotAnError(t *testing.T) {
	var log strings.Builder
	run := func(context.Context, string, ...string) error {
		t.Error("nothing should be launched")
		return nil
	}
	if err := RunAll(context.Background(), nil, launchOpts(t.TempDir()), fake(run, nil), &log); err != nil {
		t.Fatalf("want success, got %v", err)
	}
	if !strings.Contains(log.String(), "nothing to back up") {
		t.Errorf("the run should say why it did nothing: %q", log.String())
	}
}

// `run blog` backs up blog, and a name that is no app's is said to be
// one rather than quietly meaning "every app".
func TestSelectApps(t *testing.T) {
	apps := []App{testApp("blog", StateEntry{Kind: KindFiles, Path: "uploads"}), testApp("shop", StateEntry{Kind: KindFiles, Path: "uploads"})}
	if got, err := selectApps(apps, nil); err != nil || len(got) != 2 {
		t.Errorf("no names is every app: %v, %v", got, err)
	}
	if got, err := selectApps(apps, []string{"shop"}); err != nil || len(got) != 1 || got[0].Name != "shop" {
		t.Errorf("a name is that app: %v, %v", got, err)
	}
	if _, err := selectApps(apps, []string{"blgo"}); err == nil || !strings.Contains(err.Error(), "blog, shop") {
		t.Errorf("a name that is no app's is refused, naming the apps there are: %v", err)
	}
}
