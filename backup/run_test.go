package backup

import (
	"context"
	"errors"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func testApp(name string, state ...StateEntry) App {
	return App{Name: name, Shared: "/var/lib/liveswap/" + name + "/shared", State: state}
}

// deployedApp is an app whose data exists: RunAll skips one that has
// never been deployed, so a test about launching needs a real dir.
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

// RunAll chowns each staging dir to the user the jobs run as, so a
// test that actually runs it needs a user this machine has — the
// hotserve user exists on a box, not in a test container.
func launchOptsHere(t *testing.T, stagingRoot string) LaunchOptions {
	t.Helper()
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	o := launchOpts(stagingRoot)
	o.User = me.Username
	return o
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
		"--property=BindPaths=/var/lib/liveswap/blog/shared",
		"--property=BindPaths=/var/lib/hotserve-backup/blog",
		"--property=Environment=XDG_CACHE_HOME=/var/lib/hotserve-backup/blog/cache",
		"--property=EnvironmentFile=/etc/hotserve/backup.env",
		"--property=PrivateUsers=yes",
		"--property=NoNewPrivileges=yes",
		"--property=CapabilityBoundingSet=",
		"--property=SystemCallFilter=@system-service",
		"--property=MemoryHigh=64M",
		"--property=Environment=GOGC=20",
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

	// The command the unit runs, after the properties.
	rest := args[len(args)-8:]
	want := []string{"/usr/bin/hotserve", "backup", "app", "--name=blog",
		"--shared=/var/lib/liveswap/blog/shared", "--staging=/var/lib/hotserve-backup/blog",
		"sqlite:app.db", "files:uploads"}
	for i := range want {
		if rest[i] != want[i] {
			t.Fatalf("command tail:\n got %v\nwant %v", rest, want)
		}
	}
}

// An app whose state is only files never has its dir opened for
// writing, so it goes in read-only — least privilege per app. A
// database forces the writable bind, because SQLite creates the -shm
// file beside it to read a WAL database at all (proven on the box:
// a read-only mount fails with "unable to open database file").
func TestLaunchArgsBindsTheAppsDataReadOnlyUnlessADatabaseNeedsOpening(t *testing.T) {
	filesOnly := testApp("wiki", StateEntry{Kind: KindFiles, Path: "pages"})
	args := strings.Join(LaunchArgs(filesOnly, launchOpts("/var/lib/hotserve-backup")), "\n")
	if !strings.Contains(args, "--property=BindReadOnlyPaths=/var/lib/liveswap/wiki/shared") {
		t.Errorf("files-only app should get a read-only bind:\n%s", args)
	}
	if strings.Contains(args, "--property=BindPaths=/var/lib/liveswap/wiki/shared") {
		t.Error("files-only app must not get a writable bind")
	}

	withDB := testApp("blog", StateEntry{Kind: KindSQLite, Path: "app.db"})
	args = strings.Join(LaunchArgs(withDB, launchOpts("/var/lib/hotserve-backup")), "\n")
	if !strings.Contains(args, "--property=BindPaths=/var/lib/liveswap/blog/shared") {
		t.Errorf("an app with a database needs its dir writable:\n%s", args)
	}
}

// A repository on this box is invisible to the job unless it is bound
// in — the failure is "repository does not exist", which looks like a
// configuration error rather than a missing mount. A remote
// repository needs no bind at all.
func TestLaunchArgsBindsALocalRepositoryOnly(t *testing.T) {
	app := testApp("blog", StateEntry{Kind: KindSQLite, Path: "app.db"})

	o := launchOpts("/var/lib/hotserve-backup")
	o.RepositoryPath = "/srv/backups"
	if !strings.Contains(strings.Join(LaunchArgs(app, o), "\n"), "--property=BindPaths=/srv/backups") {
		t.Error("a filesystem repository must be in the job's view")
	}

	o.RepositoryPath = ""
	for _, a := range LaunchArgs(app, o) {
		if strings.Contains(a, "BindPaths=/srv") {
			t.Errorf("a remote repository needs no bind: %s", a)
		}
	}
}

func TestRepositoryPath(t *testing.T) {
	for _, tc := range []struct{ repo, want string }{
		{"/srv/backups", "/srv/backups"},
		{"local:/srv/backups/", "/srv/backups"},
		{"s3:s3.example.com/bucket", ""},
		{"b2:bucket:path", ""},
		{"sftp:user@host:/srv/backups", ""},
		{"rest:https://example.com/", ""},
	} {
		got, err := RepositoryPath(tc.repo)
		if err != nil {
			t.Errorf("%q: %v", tc.repo, err)
		}
		if got != tc.want {
			t.Errorf("%q → %q, want %q", tc.repo, got, tc.want)
		}
	}
	// A relative path would be bound nowhere and chowned nowhere, and
	// would resolve against whatever directory each process started
	// in — so it is refused rather than quietly treated as remote.
	for _, repo := range []string{"backups", "./backups", "local:backups"} {
		if _, err := RepositoryPath(repo); err == nil {
			t.Errorf("%q should be refused as a relative path", repo)
		}
	}
}

func TestLocalRepositoryPathFromTheEnvironmentFile(t *testing.T) {
	got, err := LocalRepositoryPath([]string{"RESTIC_PASSWORD=x", "RESTIC_REPOSITORY=/srv/backups"})
	if err != nil || got != "/srv/backups" {
		t.Fatalf("got %q, %v", got, err)
	}
	// The jobs cannot see a file named here: their view holds the
	// app's data, their staging and the repository, nothing else.
	_, err = LocalRepositoryPath([]string{"RESTIC_REPOSITORY_FILE=/etc/hotserve/repo"})
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("RESTIC_REPOSITORY_FILE should be refused, got %v", err)
	}
	_, err = LocalRepositoryPath([]string{"RESTIC_PASSWORD=x"})
	if err == nil || !strings.Contains(err.Error(), "backup init") {
		t.Errorf("a missing repository should say how to set one, got %v", err)
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
	err := RunAll(context.Background(), apps, launchOptsHere(t, root), run, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "shop") {
		t.Fatalf("want an error naming the failed app, got %v", err)
	}
	if strings.Join(launched, ",") != "blog,shop,wiki" {
		t.Errorf("every app must be attempted, in order: %v", launched)
	}
	for _, name := range []string{"blog", "shop", "wiki"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("staging dir for %s: %v", name, err)
		}
	}
}

// The same contract as a failed launch: a per-app staging problem is
// that app's failure, not the run's.
func TestRunAllContinuesWhenOneAppsStagingDirCannotBeMade(t *testing.T) {
	root := t.TempDir()
	// A regular file where blog's staging dir belongs.
	if err := os.WriteFile(filepath.Join(root, "blog"), []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}
	apps := []App{
		deployedApp(t, "blog", StateEntry{Kind: KindFiles, Path: "uploads"}),
		deployedApp(t, "shop", StateEntry{Kind: KindFiles, Path: "uploads"}),
	}
	var launched []string
	run := func(_ context.Context, _ string, args ...string) error {
		for _, a := range args {
			if name, ok := strings.CutPrefix(a, "--name="); ok {
				launched = append(launched, name)
			}
		}
		return nil
	}
	var log strings.Builder
	err := RunAll(context.Background(), apps, launchOptsHere(t, root), run, &log)
	if err == nil || !strings.Contains(err.Error(), "blog") {
		t.Fatalf("want an error naming blog, got %v", err)
	}
	if strings.Join(launched, ",") != "shop" {
		t.Errorf("shop should still have been backed up: %v", launched)
	}
	if !strings.Contains(log.String(), "blog: FAILED") {
		t.Errorf("the run should name the failed app: %q", log.String())
	}
}

// An app can declare state before it has ever been deployed. That is
// not a failure: it would otherwise turn the hourly timer red until
// the first deploy.
func TestRunAllSkipsAnAppThatHasNeverBeenDeployed(t *testing.T) {
	apps := []App{
		testApp("never-deployed", StateEntry{Kind: KindFiles, Path: "uploads"}),
		deployedApp(t, "blog", StateEntry{Kind: KindFiles, Path: "uploads"}),
	}
	var launched []string
	run := func(_ context.Context, _ string, args ...string) error {
		for _, a := range args {
			if name, ok := strings.CutPrefix(a, "--name="); ok {
				launched = append(launched, name)
			}
		}
		return nil
	}
	var log strings.Builder
	if err := RunAll(context.Background(), apps, launchOptsHere(t, t.TempDir()), run, &log); err != nil {
		t.Fatalf("an undeployed app is not a failure: %v", err)
	}
	if strings.Join(launched, ",") != "blog" {
		t.Errorf("only the deployed app should run: %v", launched)
	}
	if !strings.Contains(log.String(), "never-deployed: no data yet") {
		t.Errorf("the run should say why it skipped: %q", log.String())
	}
}

func TestRunAllWithNothingDeclaredIsNotAnError(t *testing.T) {
	var log strings.Builder
	run := func(context.Context, string, ...string) error {
		t.Error("nothing should be launched")
		return nil
	}
	if err := RunAll(context.Background(), nil, launchOptsHere(t, t.TempDir()), run, &log); err != nil {
		t.Fatalf("want success, got %v", err)
	}
	if !strings.Contains(log.String(), "nothing to back up") {
		t.Errorf("the run should say why it did nothing: %q", log.String())
	}
}
