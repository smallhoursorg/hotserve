package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// fakeRestic answers the questions init asks through the capturer:
// does the repository exist, which snapshot did the probe write, and
// what happens when it tries to remove it. The forget goes through
// the capturer because its OUTPUT is the evidence — a refusal by the
// storage reads differently from a network error, and only one of
// those means the backups are safe from the box.
type fakeRestic struct {
	calls       []call
	repoMissing bool
	// refuses is what the storage does with the delete: when set, the
	// probe snapshot is still listed afterwards. What restic prints and
	// returns for forget is separate — forgetOut/forgetErr — because
	// the two do not agree: restic exits 0 over a refused delete.
	refuses   bool
	forgetOut string
	forgetErr error
	// listFailsAfterForget makes the look afterwards fail, so the
	// outcome cannot be known.
	listFailsAfterForget bool
	forgotten            bool
	// wrongPassword: the repository exists and this password does not
	// open it. initFails: `restic init` fails for a reason other than
	// the repository already being there (a key that may not create
	// the bucket, a typo'd one).
	wrongPassword bool
	initFails     string
	// stderr and env are those of the command being answered.
	stderr io.Writer
	env    []string
	// bounded records whether `cat config` was asked against a clock.
	bounded bool
}

// exited is a command's exit status, as os/exec reports one.
type exited int

func (e exited) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exited) ExitCode() int { return int(e) }

// opens reports whether the password a command was given opens this
// repository. One that init made up never does: it is 32 random bytes,
// and the repository was set up with something else.
func (f *fakeRestic) opens() bool {
	for _, kv := range f.env {
		if pw, ok := strings.CutPrefix(kv, "RESTIC_PASSWORD="); ok {
			made, _ := regexp.MatchString(`^[A-Za-z0-9_-]{43}$`, pw)
			return !f.wrongPassword && !made
		}
	}
	return false
}

// exec is this repository as an Exec: capture answers every command
// whose stdout is read, and do gets the rest.
func (f *fakeRestic) exec(do does) Exec {
	return func(ctx context.Context, c Cmd) error {
		f.stderr, f.env = c.Stderr, c.Env
		return fake(do, f.capture)(ctx, c)
	}
}

// restic 0.18.0's own words, captured from real runs (see
// alreadyInitialized).
const (
	resticInitExistsS3      = "Fatal: create key in repository at s3:http://e2e-s3:8333/made/other failed: repository master key and config already initialized\n"
	resticWrongPasswordSays = "Fatal: wrong password or no key found\n"
)

func (f *fakeRestic) capture(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, call{name, args})
	switch {
	case len(args) > 0 && args[0] == "init":
		switch {
		case f.initFails != "":
			return []byte(f.initFails), exited(1)
		case f.repoMissing:
			f.repoMissing = false
			return []byte("created restic repository 1a67cc52d5\n"), nil
		}
		return []byte(resticInitExistsS3), exited(1)
	case len(args) > 1 && args[0] == "cat" && args[1] == "config":
		_, f.bounded = ctx.Deadline()
		switch {
		case f.repoMissing || f.initFails != "":
			return []byte("Fatal: repository does not exist: unable to open config file\n"), exited(resticNoRepository)
		case !f.opens():
			return []byte(resticWrongPasswordSays), exited(resticWrongPassword)
		}
		return []byte("{}"), nil
	case len(args) > 0 && args[0] == "snapshots":
		if f.forgotten && f.listFailsAfterForget {
			return nil, errors.New("Fatal: unable to open repository: dial tcp: i/o timeout")
		}
		if f.forgotten && !f.refuses {
			return []byte(`[]`), nil
		}
		return []byte(`[{"short_id":"probe123","time":"2026-09-18T08:00:00Z","tags":["` + DeleteProbeTag + `"]}]`), nil
	case len(args) > 0 && args[0] == "forget":
		f.forgotten = true
		// Streams as restic really uses them (measured): what forget
		// says goes to stderr, stdout stays empty — whatever its exit
		// status.
		if f.stderr != nil {
			_, _ = io.WriteString(f.stderr, f.forgetOut)
		}
		return nil, f.forgetErr
	}
	return nil, nil
}

func (f *fakeRestic) forgot(id string) bool {
	for _, c := range f.calls {
		joined := strings.Join(c.args, " ")
		if strings.HasPrefix(joined, "forget") && strings.Contains(joined, id) {
			return true
		}
	}
	return false
}

func probeSnapshots() *fakeRestic { return &fakeRestic{} }

func missingRepo() *fakeRestic { return &fakeRestic{repoMissing: true} }

func initOpts(t *testing.T) InitOptions {
	t.Helper()
	// A password the operator already has: the repository in these
	// fixtures exists, and init refuses to invent a second password
	// for a repository it can see (that password could never read
	// what is already in it).
	return InitOptions{
		Repository: "s3:s3.example.com/bucket",
		EnvFile:    filepath.Join(t.TempDir(), "backup.env"),
		Password:   "the-password-from-the-password-manager",
	}
}

// The password protects every backup and the env file is the only
// copy on the box, so its mode is part of the feature.
func TestInitWritesARootOnlyEnvFile(t *testing.T) {
	o := initOpts(t)
	o.Extra = []string{"AWS_SECRET_ACCESS_KEY=s3cret", "AWS_ACCESS_KEY_ID=keyid"}
	rec := &recorder{}
	var out strings.Builder
	if err := Init(context.Background(), o, probeSnapshots().exec(rec.run), &out); err != nil {
		t.Fatalf("init: %v", err)
	}
	info, err := os.Stat(o.EnvFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("env file mode = %o, want 600", got)
	}
	body, err := os.ReadFile(o.EnvFile)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{
		"RESTIC_REPOSITORY=s3:s3.example.com/bucket",
		"RESTIC_PASSWORD=",
		"AWS_ACCESS_KEY_ID=keyid",
		"AWS_SECRET_ACCESS_KEY=s3cret",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("env file missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(out.String(), "save this somewhere safe") {
		t.Errorf("a password the operator supplied is not news to them:\n%s", out.String())
	}
}

// A new repository's password exists nowhere else, so init has to show
// it — and what it shows must be what the file holds, or the operator
// saves a password that opens nothing.
func TestInitShowsAGeneratedPasswordOnce(t *testing.T) {
	o := initOpts(t)
	o.Password = "" // a new repository
	var out strings.Builder
	if err := Init(context.Background(), o, missingRepo().exec((&recorder{}).run), &out); err != nil {
		t.Fatalf("init: %v", err)
	}
	if !strings.Contains(out.String(), "save this somewhere safe") {
		t.Fatalf("the generated password must be shown:\n%s", out.String())
	}
	body, err := os.ReadFile(o.EnvFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if pw, ok := strings.CutPrefix(line, "RESTIC_PASSWORD="); ok {
			if !strings.Contains(out.String(), pw) {
				t.Error("the password printed is not the password written")
			}
			return
		}
	}
	t.Error("no password in the environment file")
}

// Stopped while `restic init` runs, the repository may already be there,
// made with a password only this process has: it is shown before init
// gives up, since nothing later will show it.
func TestInitShowsTheGeneratedPasswordWhenStoppedWhileCreating(t *testing.T) {
	o := initOpts(t)
	o.Password = ""
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var password string
	x := func(_ context.Context, c Cmd) error {
		if c.Name != "restic" || c.Args[0] != "init" {
			t.Errorf("nothing runs after an init that was stopped: %s %v", c.Name, c.Args)
			return nil
		}
		for _, kv := range c.Env {
			if pw, ok := strings.CutPrefix(kv, "RESTIC_PASSWORD="); ok {
				password = pw
			}
		}
		cancel()
		return errors.New("signal: killed")
	}
	var out strings.Builder
	err := Init(ctx, o, x, &out)
	if err == nil || !strings.Contains(err.Error(), "stopped while creating") {
		t.Fatalf("want init to say it was stopped, got %v", err)
	}
	if password == "" || !strings.Contains(out.String(), password) {
		t.Errorf("the password the repository may have been made with must be shown:\n%s", out.String())
	}
	if _, err := os.Stat(o.EnvFile); err == nil {
		t.Error("nothing is scheduled by an init that was stopped")
	}
}

// A key that can read but not write gets through `restic cat config`
// and fails on the first real backup. Installing the settings anyway
// would arm the hourly timer to fail for ever, and the retry would
// then refuse to overwrite the file it just left behind.
func TestInitWritesNothingWhenTheCredentialsCannotBackUp(t *testing.T) {
	o := initOpts(t)
	rec := &recorder{failIf: func(_ string, args []string) error {
		if len(args) > 0 && args[0] == "backup" {
			return errors.New("Fatal: unable to save snapshot: AccessDenied")
		}
		return nil
	}}
	err := Init(context.Background(), o, probeSnapshots().exec(rec.run), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "was not written") {
		t.Fatalf("want a failure saying nothing was installed, got %v", err)
	}
	if _, statErr := os.Stat(o.EnvFile); !os.IsNotExist(statErr) {
		t.Errorf("no environment file should exist after a failed probe: %v", statErr)
	}
}

// Pointing a working box at a new repository that turns out to be
// unwritable must not tell the operator their backups are off: the old
// settings are still there and the timer is still using them. Saying
// "nothing is scheduled" here would be a lie at the worst moment.
func TestInitSaysTheOldSettingsStandWhenForcedInitFails(t *testing.T) {
	o := initOpts(t)
	o.Force = true
	if err := os.WriteFile(o.EnvFile, []byte("RESTIC_REPOSITORY=s3:s3.example.com/old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{failIf: func(_ string, args []string) error {
		if len(args) > 0 && args[0] == "backup" {
			return errors.New("Fatal: unable to save snapshot: AccessDenied")
		}
		return nil
	}}
	err := Init(context.Background(), o, probeSnapshots().exec(rec.run), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "unchanged") {
		t.Fatalf("want a failure saying the old settings stand, got %v", err)
	}
	if strings.Contains(err.Error(), "nothing is scheduled") {
		t.Error("the box is still backing up to the old repository; saying otherwise sends the operator looking in the wrong place")
	}
	body, readErr := os.ReadFile(o.EnvFile)
	if readErr != nil || !strings.Contains(string(body), "s3.example.com/old") {
		t.Errorf("the working settings must survive a failed --force: %q, %v", body, readErr)
	}
}

// Provider settings keep the order they were given: systemd takes the
// last assignment of a key, so re-ordering them here would hand the
// jobs credentials that init never tested.
func TestInitKeepsTheOrderOfProviderSettings(t *testing.T) {
	o := initOpts(t)
	o.Extra = []string{"AWS_ACCESS_KEY_ID=superseded", "AWS_ACCESS_KEY_ID=current"}
	if err := Init(context.Background(), o, probeSnapshots().exec((&recorder{}).run), io.Discard); err != nil {
		t.Fatalf("init: %v", err)
	}
	body, err := os.ReadFile(o.EnvFile)
	if err != nil {
		t.Fatal(err)
	}
	first := strings.Index(string(body), "AWS_ACCESS_KEY_ID=superseded")
	last := strings.Index(string(body), "AWS_ACCESS_KEY_ID=current")
	if first < 0 || last < 0 || last < first {
		t.Errorf("the last setting given must be the last one written:\n%s", body)
	}
}

// Overwriting the env file loses the password, and with it every
// existing backup.
func TestInitRefusesToOverwriteWithoutForce(t *testing.T) {
	o := initOpts(t)
	if err := os.WriteFile(o.EnvFile, []byte("RESTIC_PASSWORD=existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	err := Init(context.Background(), o, probeSnapshots().exec(rec.run), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("want a refusal naming --force, got %v", err)
	}
	if len(rec.calls) != 0 {
		t.Errorf("nothing should have run: %+v", rec.calls)
	}
	body, _ := os.ReadFile(o.EnvFile)
	if !strings.Contains(string(body), "existing") {
		t.Error("the existing file must be left alone")
	}
}

// An existing repository is the normal case when rebuilding a box:
// init must open it, not create a second one — and report which of the
// two it did.
func TestInitCreatesOrOpensTheRepository(t *testing.T) {
	var out strings.Builder
	if err := Init(context.Background(), initOpts(t), probeSnapshots().exec((&recorder{}).run), &out); err != nil {
		t.Fatalf("init on an existing repository: %v", err)
	}
	if !strings.Contains(out.String(), "repository ready") || strings.Contains(out.String(), "created a new repository") {
		t.Errorf("an existing repository is opened, not created:\n%s", out.String())
	}

	fresh := initOpts(t)
	fresh.Password = "" // a new repository: init makes the password
	out.Reset()
	if err := Init(context.Background(), fresh, missingRepo().exec((&recorder{}).run), &out); err != nil {
		t.Fatalf("init on a missing repository: %v", err)
	}
	if !strings.Contains(out.String(), "created a new repository") {
		t.Errorf("a missing repository should have been created:\n%s", out.String())
	}
}

// `restic init` exits 1 whether a repository is already there or one
// could not be made. Which it was is read from `cat config`'s exit
// status — and that call is made against a clock, because asked of a
// bucket that does not exist it retries for many minutes (measured
// against a real S3 server: still retrying when stopped at eight), and a
// typo in a bucket name must not look like a stuck box.
func TestInitReportsItsOwnFailureWhereNoRepositoryIs(t *testing.T) {
	fake := &fakeRestic{initFails: "Fatal: create repository at s3:https://s3.example.com/typo failed: Access Denied.\n"}
	o := initOpts(t)
	err := Init(context.Background(), o, fake.exec((&recorder{}).run), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "cannot create a repository") || !strings.Contains(err.Error(), "Access Denied") {
		t.Fatalf("want init's own failure, quoted, got %v", err)
	}
	if !fake.bounded {
		t.Error("cat config was asked with no deadline: on a missing bucket it retries for many minutes")
	}
	if _, statErr := os.Stat(o.EnvFile); !os.IsNotExist(statErr) {
		t.Error("no settings may be written for a repository that was never made")
	}
}

// What decides is restic's exit status, which it documents, and never
// its wording, which differs with the backend and the version: here
// every answer says the same unhelpful thing.
func TestRepositoryStateReadsTheExitStatusNotTheWords(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want repoState
	}{
		"opens":                      {nil, repositoryOpen},
		"12: wrong password":         {exited(resticWrongPassword), repositoryLocked},
		"10: no repository there":    {exited(resticNoRepository), repositoryAbsent},
		"1: anything else":           {exited(1), repositoryAbsent},
		"stopped before it answered": {context.DeadlineExceeded, repositoryAbsent},
		"could not be started":       {errors.New("restic is not installed"), repositoryAbsent},
	} {
		t.Run(name, func(t *testing.T) {
			x := fake(nil, func(context.Context, string, ...string) ([]byte, error) {
				return []byte("Fatal: something went wrong\n"), tc.err
			})
			if got, _ := repositoryState(context.Background(), x); got != tc.want {
				t.Errorf("state = %v, want %v", got, tc.want)
			}
		})
	}
}

// A rebuilt box, at a terminal: the repository already exists, so the
// password init would have invented is dropped and the real one asked
// for — and it is that one, checked against the repository, that goes
// into the settings. Nothing is created with the invented one.
func TestInitAsksForTheExistingRepositorysPassword(t *testing.T) {
	o := initOpts(t)
	o.Password = "" // nothing given: init would invent one
	asked := 0
	o.AskPassword = func() (string, error) { asked++; return "the-one-saved-at-setup", nil }
	var out strings.Builder
	if err := Init(context.Background(), o, probeSnapshots().exec((&recorder{}).run), &out); err != nil {
		t.Fatalf("init: %v", err)
	}
	if asked != 1 {
		t.Fatalf("asked %d times, want once", asked)
	}
	body, err := os.ReadFile(o.EnvFile)
	if err != nil || !strings.Contains(string(body), "RESTIC_PASSWORD=the-one-saved-at-setup\n") {
		t.Errorf("the settings must hold the password that was asked for: %q, %v", body, err)
	}
	if strings.Contains(out.String(), "save this somewhere safe") {
		t.Error("an existing repository's password is not news: no invented one may be printed")
	}

	// And a new repository is never asked about: its password is made
	// here, strong, and shown once.
	fresh := initOpts(t)
	fresh.Password = ""
	fresh.AskPassword = func() (string, error) {
		t.Error("asked for a password of a repository that did not exist")
		return "", nil
	}
	if err := Init(context.Background(), fresh, missingRepo().exec((&recorder{}).run), io.Discard); err != nil {
		t.Fatalf("init on a new repository: %v", err)
	}
}

// The asked-for password is checked like any other: a wrong one is
// said plainly, and nothing is written.
func TestInitRefusesAWrongAskedForPassword(t *testing.T) {
	o := initOpts(t)
	o.Password = ""
	o.AskPassword = func() (string, error) { return "not-it", nil }
	err := Init(context.Background(), o, (&fakeRestic{wrongPassword: true}).exec((&recorder{}).run), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "cannot open it") {
		t.Fatalf("want a wrong-password failure, got %v", err)
	}
	if _, statErr := os.Stat(o.EnvFile); !os.IsNotExist(statErr) {
		t.Error("no settings may be written with a password that does not open the repository")
	}
}

// Existing repository, wrong password: said plainly, and quickly.
func TestInitSaysWhenThePasswordCannotOpenTheRepository(t *testing.T) {
	fake := &fakeRestic{wrongPassword: true}
	err := Init(context.Background(), initOpts(t), fake.exec((&recorder{}).run), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "cannot open it") || !strings.Contains(err.Error(), "wrong password") {
		t.Fatalf("want a wrong-password failure quoting restic, got %v", err)
	}
}

// The promise is that a compromised box cannot erase its own backups,
// so init proves it by trying: a key that CAN delete gets a warning.
func TestInitWarnsWhenTheKeyCanDelete(t *testing.T) {
	rec := &recorder{}
	fake := &fakeRestic{} // forget succeeds: the key can delete
	var out strings.Builder
	if err := Init(context.Background(), initOpts(t), fake.exec(rec.run), &out); err != nil {
		t.Fatalf("init: %v", err)
	}
	if !strings.Contains(out.String(), "WARNING: these credentials can delete backups") {
		t.Fatalf("a deletable repository must warn:\n%s", out.String())
	}
	var probed bool
	for _, c := range rec.calls {
		joined := strings.Join(c.args, " ")
		if strings.Contains(joined, "backup") && strings.Contains(joined, DeleteProbeTag) {
			probed = true
		}
	}
	if !probed {
		t.Errorf("the check must write a probe snapshot: %+v", rec.calls)
	}
	// By id, never by tag: `restic forget --tag x` refuses without a
	// policy, which would read as a storage that denied the delete.
	if !fake.forgot("probe123") {
		t.Errorf("the probe snapshot must be removed by id: %+v", fake.calls)
	}
	for _, c := range fake.calls {
		joined := strings.Join(c.args, " ")
		if strings.HasPrefix(joined, "forget") && strings.Contains(joined, "--tag") {
			t.Errorf("forget by tag always fails without a policy, so it proves nothing: %v", c.args)
		}
	}
}

// restic's own output against a real append-only server, captured
// verbatim from restic 0.18.0 and restic-rest-server --append-only
// (Debian 13) — ON STDERR, with stdout empty and exit status 0. The
// fake above delivers these words on the stream restic really uses.
// Re-capture when upgrading.
const resticRefusedForget = "Remove(<snapshot/a4adfea30f>) failed: unexpected HTTP response (403): 403 Forbidden\n" +
	"unable to remove snapshot/a4adfea30faac63d6109a2343dc75340f06d0db6f5dd31c9a1e50a42505d75fe from the repository\n"

func TestInitJudgesTheDeleteByTheRepositoryNotTheExitStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		fake *fakeRestic
		want string // what init must say
		not  string // and must not
	}{
		{"refused, as restic really reports it: exit 0 and a 403",
			&fakeRestic{refuses: true, forgetOut: resticRefusedForget},
			"delete refused by the storage", "WARNING"},
		{"deleted: the snapshot is gone",
			&fakeRestic{},
			"WARNING: these credentials can delete backups", "delete refused"},
		{"still there, exit 0, no refusal in the output: not proof of anything",
			&fakeRestic{refuses: true, forgetOut: "some unrelated message"},
			"could not finish", "delete refused"},
		{"cannot look afterwards",
			&fakeRestic{listFailsAfterForget: true},
			"could not finish", "delete refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			if err := Init(context.Background(), initOpts(t), tc.fake.exec((&recorder{}).run), &out); err != nil {
				t.Fatalf("init: %v", err)
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("want %q:\n%s", tc.want, out.String())
			}
			if strings.Contains(out.String(), tc.not) {
				t.Errorf("must not say %q:\n%s", tc.not, out.String())
			}
		})
	}
}

// The probe's content comes from a command restic runs, so this
// process — root — writes no file for it anywhere the backup user can
// reach first.
func TestTheDeleteProbeWritesNoFile(t *testing.T) {
	rec := &recorder{}
	if err := Init(context.Background(), initOpts(t), probeSnapshots().exec(rec.run), io.Discard); err != nil {
		t.Fatalf("init: %v", err)
	}
	for _, c := range rec.calls {
		joined := strings.Join(c.args, " ")
		if strings.HasPrefix(joined, "backup") && strings.Contains(joined, DeleteProbeTag) {
			if !strings.Contains(joined, "--stdin-from-command") {
				t.Errorf("the probe snapshot must not be of a file root wrote: %v", c.args)
			}
			return
		}
	}
	t.Error("no probe backup was run")
}

func TestInitReportsAnAppendOnlyKey(t *testing.T) {
	fake := &fakeRestic{
		refuses:   true,
		forgetOut: "Fatal: AccessDenied: the key is not permitted to delete",
		forgetErr: errors.New("exit status 1"),
	}
	var out strings.Builder
	if err := Init(context.Background(), initOpts(t), fake.exec((&recorder{}).run), &out); err != nil {
		t.Fatalf("a key that refuses deletes is the good case, not an error: %v", err)
	}
	if !strings.Contains(out.String(), "delete refused by the storage") {
		t.Fatalf("want the append-only confirmation:\n%s", out.String())
	}
	if strings.Contains(out.String(), "WARNING") {
		t.Error("an append-only key must not warn")
	}
}

// "Your backups cannot be deleted" is a security claim, so it must
// rest on the storage refusing — not on any failure at all. A network
// error or a stale lock is an unknown answer, and says so.
func TestInitDoesNotMistakeAFailureForProtection(t *testing.T) {
	for _, tc := range []struct{ name, out string }{
		{"network", "Fatal: unable to open repository: dial tcp: i/o timeout"},
		{"stale lock", "Fatal: repository is already locked by PID 123"},
		{"corrupt", "Fatal: load index: invalid data returned"},
		// A status number with no refusal wording beside it. Matching
		// bare "403" would turn any of these into "your backups cannot
		// be deleted" — a security claim resting on a PID.
		{"stale lock held by pid 403", "Fatal: repository is already locked by PID 403 on box by root"},
		{"a duration", "Fatal: unable to remove snapshot: timeout after 403ms"},
		{"a pack id", "Fatal: pack 4031a9c2 not found in index"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeRestic{refuses: true, forgetOut: tc.out, forgetErr: errors.New("exit status 1")}
			var out strings.Builder
			if err := Init(context.Background(), initOpts(t), fake.exec((&recorder{}).run), &out); err != nil {
				t.Fatalf("init: %v", err)
			}
			if strings.Contains(out.String(), "delete refused by the storage") {
				t.Errorf("a %s failure is not proof the backups are protected:\n%s", tc.name, out.String())
			}
			if !strings.Contains(out.String(), "could not finish") {
				t.Errorf("an indeterminate probe must say so:\n%s", out.String())
			}
		})
	}
}

// The other side of matching words, never a bare number: what real
// stores send when they refuse must still read as a refusal.
func TestDeniedByRecognisesRealRefusals(t *testing.T) {
	for _, text := range []string{
		"s3.removeObject: 403 Forbidden",
		"Access Denied.",
		"b2_delete_file_version: 401: unauthorized",
		"This request is not authorized to perform this operation.",
	} {
		if !deniedBy(text) {
			t.Errorf("%q is a storage refusing, and must count as one", text)
		}
	}
	// Every repository is an object store reached over the network, so a
	// filesystem's words are never the storage's: they are this box's —
	// restic's cache, a temp dir — and a probe snapshot that is still
	// there beside one of them is not evidence of anything.
	for _, text := range []string{
		"unable to open cache: mkdir /var/lib/hotserve-backup/x/cache: permission denied",
		"open /tmp/restic-temp-pack-1: read-only file system",
		"operation not permitted",
	} {
		if deniedBy(text) {
			t.Errorf("%q is this box talking, not the storage refusing a delete", text)
		}
	}
}

// A repository that cannot even be written to is a setup failure, and
// must not be reported as "append-only".
func TestInitFailsWhenTheProbeSnapshotCannotBeWritten(t *testing.T) {
	rec := &recorder{}
	rec.failIf = func(_ string, args []string) error {
		if len(args) > 1 && args[0] == "backup" {
			return errors.New("AccessDenied")
		}
		return nil
	}
	err := Init(context.Background(), initOpts(t), probeSnapshots().exec(rec.run), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "not writable") {
		t.Fatalf("want a not-writable error, got %v", err)
	}
}

// systemd takes the last assignment in an environment file, so a
// second RESTIC_PASSWORD would leave the jobs using one password
// while the operator saves the one this command printed — and a
// second RESTIC_REPOSITORY would send the backups somewhere init
// never checked.
func TestInitRefusesToLetCredentialsRedefineItsOwnSettings(t *testing.T) {
	for _, kv := range []string{
		"RESTIC_PASSWORD=sneaky",
		"RESTIC_PASSWORD_FILE=/tmp/other",
		"RESTIC_PASSWORD_COMMAND=cat /tmp/other",
		"RESTIC_REPOSITORY=s3:elsewhere/bucket",
		"RESTIC_REPOSITORY_FILE=/tmp/repo",
	} {
		o := initOpts(t)
		o.Extra = []string{"AWS_ACCESS_KEY_ID=keyid", kv}
		err := Init(context.Background(), o, probeSnapshots().exec((&recorder{}).run), io.Discard)
		if err == nil {
			t.Errorf("%s should be refused", kv)
			continue
		}
		if _, statErr := os.Stat(o.EnvFile); statErr == nil {
			t.Errorf("%s: nothing should have been written", kv)
		}
	}
}

func TestInitRejectsMalformedCredentials(t *testing.T) {
	o := initOpts(t)
	o.Extra = []string{"AWS_ACCESS_KEY_ID"}
	err := Init(context.Background(), o, probeSnapshots().exec((&recorder{}).run), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "KEY=VALUE") {
		t.Fatalf("want a KEY=VALUE error, got %v", err)
	}
}

// A repository is a backend URL; init refuses a path on this box before
// it does anything else.
func TestInitRefusesAPath(t *testing.T) {
	for _, repo := range []string{"/srv/backups", "local:/srv/backups", "backups"} {
		o := InitOptions{Repository: repo, EnvFile: filepath.Join(t.TempDir(), "backup.env"), Password: "p"}
		var calls []string
		run := func(_ context.Context, name string, args ...string) error { calls = append(calls, name); return nil }
		capture := func(_ context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, name)
			return nil, nil
		}
		err := Init(context.Background(), o, fake(run, capture), io.Discard)
		if err == nil || !strings.Contains(err.Error(), "not a backend URL") {
			t.Errorf("%q: want a refusal naming backend URLs, got %v", repo, err)
		}
		if len(calls) != 0 {
			t.Errorf("%q: nothing may run before the refusal, ran %v", repo, calls)
		}
	}
}

func TestNewPasswordIsLongAndUnique(t *testing.T) {
	a, err := newPassword()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := newPassword()
	if a == b {
		t.Fatal("two passwords came out the same")
	}
	if len(a) < 40 {
		t.Fatalf("password is %d characters, want a 256-bit one", len(a))
	}
}
