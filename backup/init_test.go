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

// fakeRestic answers the questions init asks through the capturer:
// does the repository exist, which snapshot did the probe write, and
// what happens when it tries to remove it. The forget goes through
// the capturer because its OUTPUT is the evidence — a refusal by the
// storage reads differently from a network error, and only one of
// those means the backups are safe from the box.
type fakeRestic struct {
	calls       []call
	repoMissing bool
	forgetOut   string
	forgetErr   error
}

func (f *fakeRestic) capture(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, call{name, args})
	switch {
	case len(args) > 1 && args[0] == "cat" && args[1] == "config":
		if f.repoMissing {
			return nil, errors.New("Fatal: repository does not exist")
		}
		return []byte("{}"), nil
	case len(args) > 0 && args[0] == "snapshots":
		return []byte(`[{"short_id":"probe123","time":"2026-09-18T08:00:00Z","tags":["` + DeleteProbeTag + `"]}]`), nil
	case len(args) > 0 && args[0] == "forget":
		return []byte(f.forgetOut), f.forgetErr
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

func probeSnapshots() Capturer { return (&fakeRestic{}).capture }

func missingRepo() Capturer { return (&fakeRestic{repoMissing: true}).capture }

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
	if err := Init(context.Background(), o, rec.run, probeSnapshots(), &out); err != nil {
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
	if err := Init(context.Background(), o, (&recorder{}).run, missingRepo(), &out); err != nil {
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

// Overwriting the env file loses the password, and with it every
// existing backup.
func TestInitRefusesToOverwriteWithoutForce(t *testing.T) {
	o := initOpts(t)
	if err := os.WriteFile(o.EnvFile, []byte("RESTIC_PASSWORD=existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	err := Init(context.Background(), o, rec.run, probeSnapshots(), io.Discard)
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

// An existing repository is the normal case when rebuilding a box, so
// init must not try to create it again.
func TestInitCreatesTheRepositoryOnlyWhenMissing(t *testing.T) {
	rec := &recorder{}
	if err := Init(context.Background(), initOpts(t), rec.run, probeSnapshots(), io.Discard); err != nil {
		t.Fatalf("init: %v", err)
	}
	_ = rec
	for _, c := range rec.calls {
		if len(c.args) > 0 && c.args[0] == "init" {
			t.Fatalf("repository exists (cat config succeeded); init should not have run: %+v", rec.calls)
		}
	}

	// A repository that is not there yet: the existence check is a
	// capture (its failure is the answer, not a fault), so that is
	// where "does not exist" has to come from.
	rec = &recorder{}
	fresh := initOpts(t)
	fresh.Password = "" // a new repository: init makes the password
	if err := Init(context.Background(), fresh, rec.run, missingRepo(), io.Discard); err != nil {
		t.Fatalf("init: %v", err)
	}
	var sawInit bool
	for _, c := range rec.calls {
		if len(c.args) > 0 && c.args[0] == "init" {
			sawInit = true
		}
	}
	if !sawInit {
		t.Fatalf("a missing repository should have been created: %+v", rec.calls)
	}
}

// The promise is that a compromised box cannot erase its own backups,
// so init proves it by trying: a key that CAN delete gets a warning.
func TestInitWarnsWhenTheKeyCanDelete(t *testing.T) {
	rec := &recorder{}
	fake := &fakeRestic{} // forget succeeds: the key can delete
	var out strings.Builder
	if err := Init(context.Background(), initOpts(t), rec.run, fake.capture, &out); err != nil {
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

func TestInitReportsAnAppendOnlyKey(t *testing.T) {
	fake := &fakeRestic{
		forgetOut: "Fatal: AccessDenied: the key is not permitted to delete",
		forgetErr: errors.New("exit status 1"),
	}
	var out strings.Builder
	if err := Init(context.Background(), initOpts(t), (&recorder{}).run, fake.capture, &out); err != nil {
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeRestic{forgetOut: tc.out, forgetErr: errors.New("exit status 1")}
			var out strings.Builder
			if err := Init(context.Background(), initOpts(t), (&recorder{}).run, fake.capture, &out); err != nil {
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
	err := Init(context.Background(), initOpts(t), rec.run, probeSnapshots(), io.Discard)
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
		err := Init(context.Background(), o, (&recorder{}).run, probeSnapshots(), io.Discard)
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
	err := Init(context.Background(), o, (&recorder{}).run, probeSnapshots(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "KEY=VALUE") {
		t.Fatalf("want a KEY=VALUE error, got %v", err)
	}
}

// init runs as root, so a repository on this box comes out root-owned
// and the jobs (another user) cannot read it: "open …/keys:
// permission denied", every hour, for ever.
func TestInitHandsALocalRepositoryToTheJobsUser(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "keys"), 0o700); err != nil {
		t.Fatal(err)
	}
	o := initOpts(t)
	o.Repository = repo
	o.User = me.Username
	var out strings.Builder
	if err := Init(context.Background(), o, (&recorder{}).run, probeSnapshots(), &out); err != nil {
		t.Fatalf("init: %v", err)
	}
	if !strings.Contains(out.String(), "belongs to "+me.Username) {
		t.Errorf("init should say it handed the repository over:\n%s", out.String())
	}

	// A remote repository has nothing on this box to hand over.
	o.Repository = "s3:s3.example.com/bucket"
	o.Force = true
	out.Reset()
	if err := Init(context.Background(), o, (&recorder{}).run, probeSnapshots(), &out); err != nil {
		t.Fatalf("init: %v", err)
	}
	if strings.Contains(out.String(), "belongs to") {
		t.Errorf("a remote repository needs no chown:\n%s", out.String())
	}
}

func TestChownTreeNamesAnUnknownUser(t *testing.T) {
	err := chownTree(t.TempDir(), "no-such-user-here")
	if err == nil || !strings.Contains(err.Error(), "no-such-user-here") {
		t.Fatalf("want an error naming the user, got %v", err)
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
