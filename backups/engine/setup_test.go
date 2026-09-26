package engine

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smallhoursorg/hotserve/backups/envfile"
	"github.com/smallhoursorg/hotserve/backups/record"
	"github.com/smallhoursorg/hotserve/backups/unit"
)

const (
	repoID = "bbd0e899f1b628ed8f158336b70b450ef1c2e86ff7eb636f5e85b5911ca42fe1"
	// What restic 0.18 says [measured, M39/M40]: one line on stdout for
	// a repository it made; on stderr, as --json puts it, for one that
	// was there already; and its config, for a password that opens it.
	initialized = `{"message_type":"initialized","id":"` + repoID + `","repository":"s3:http://e2e-s3:9000/box"}` + "\n"
	alreadyInit = `{"message_type":"exit_error","code":1,"message":"Fatal: create key in repository at s3:http://e2e-s3:9000/box failed: repository master key and config already initialized\n"}` + "\n"
	// The other wording restic has for it, where the backend's Stat of
	// the config answers [measured: a local repository].
	alreadyThere = `{"message_type":"exit_error","code":1,"message":"Fatal: create repository at s3:http://e2e-s3:9000/box failed: Fatal: unable to open repository at s3:http://e2e-s3:9000/box: config file already exists\n"}` + "\n"
	wrongKey     = `{"message_type":"exit_error","code":1,"message":"Fatal: create repository at s3:http://e2e-s3:9000/box failed: Fatal: unable to open repository at s3:http://e2e-s3:9000/box: client.BucketExists: The request signature we calculated does not match the signature you provided. Check your key and signing method.\n"}` + "\n"
	configJSON   = `{"version": 2, "id": "` + repoID + `", "chunker_polynomial": "27ec33083365e9"}` + "\n"
)

// term is the operator: it answers in order, and remembers what it was
// asked and told. An answer that is not there is nobody answering.
type term struct {
	mu      sync.Mutex // Say is called from the clock's goroutine too
	answers []string
	asked   []string // the prompts, in order; "secret:" marks one asked with echo off
	said    []string
	// at runs at every prompt, before the answer: the moment to look at
	// what exists on disk.
	at func(prompt string)
}

func (m *term) Ask(ctx context.Context, prompt string, secret bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if secret {
		prompt = "secret:" + prompt
	}
	m.asked = append(m.asked, prompt)
	if m.at != nil {
		m.at(prompt)
	}
	// An interrupt at the prompt is no answer, as at a real terminal.
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if len(m.answers) == 0 {
		return "", io.EOF
	}
	a := m.answers[0]
	m.answers = m.answers[1:]
	return a, nil
}

func (m *term) Say(line string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.said = append(m.said, line)
}

func (m *term) saidAll() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.said, "\n")
}

func (m *term) askedAll() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.asked, "\n")
}

// setupBox is a box with no credential file, whose init unit makes a
// repository, and whose account is there.
func setupBox(t *testing.T) (*box, *term) {
	t.Helper()
	b := newBox(t)
	must(t, os.Remove(b.cfg.EnvFile))
	b.cfg.EnvFile = filepath.Join(filepath.Dir(b.cfg.EnvFile), "etc", "hotserve-backup", "repository.env")
	b.cfg.OldEnvFile = filepath.Join(filepath.Dir(b.cfg.EnvFile), "..", "hotserve", "backup.env")
	b.initOut, b.probeOut = initialized, configJSON
	// A fresh box: the look for a repository finds none.
	b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 10}
	b.version = 257
	b.haveProgram = func(string) bool { return true }
	b.account = true
	old, oldExists, oldMake, oldClock, oldProbe, oldNote, oldOwn, oldSync := haveProgram, accountExists, makeAccount, setupClock, setupProbeClock, setupNote, ownByRoot, syncDir
	haveProgram = func(p string) bool { return b.haveProgram(p) }
	accountExists = func(string) bool { return b.account }
	makeAccount = func() error { b.accountsMade++; b.account = true; return nil }
	// chown to root is root's to do; here what is asked for is recorded.
	ownByRoot = func(path string) error { b.owned = append(b.owned, path); return nil }
	syncDir = func(path string) error { b.synced = append(b.synced, path); return nil }
	setupClock, setupProbeClock, setupNote = 200*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() {
		haveProgram, accountExists, makeAccount, setupClock, setupProbeClock, setupNote, ownByRoot, syncDir = old, oldExists, oldMake, oldClock, oldProbe, oldNote, oldOwn, oldSync
	})
	return b, &term{answers: []string{"AKIDX", "the-secret", "stored"}}
}

func (b *box) setup(t *testing.T, m *term, repo string) (*SetupReport, error) {
	t.Helper()
	return Setup(context.Background(), b.cfg, b, SetupOptions{Repository: repo, Terminal: m})
}

func envOf(t *testing.T, path string) envfile.Values {
	t.Helper()
	raw, err := os.ReadFile(path)
	must(t, err)
	v, findings := envfile.Parse(raw)
	if len(findings) != 0 {
		t.Fatalf("the file setup wrote is not read as written: %q", findings)
	}
	return v
}

func nothingWritten(t *testing.T, b *box, m *term) {
	t.Helper()
	if _, err := os.Lstat(b.cfg.EnvFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the credential file exists: %v", err)
	}
	if left, _ := filepath.Glob(b.cfg.EnvFile + ".*"); len(left) != 0 {
		t.Fatalf("left beside it: %v", left)
	}
	if len(m.asked) != 0 {
		t.Fatalf("prompts were asked: %q", m.asked)
	}
}

var passwordRe = regexp.MustCompile(`\b([a-z2-7]{52})\b`)

func shownPassword(t *testing.T, m *term) string {
	t.Helper()
	pw := passwordRe.FindStringSubmatch(m.saidAll())
	if pw == nil {
		t.Fatalf("no password was shown:\n%s", m.saidAll())
	}
	return pw[1]
}

func TestAFreshSetupShowsThePasswordBeforeAnythingIsWritten(t *testing.T) {
	b, m := setupBox(t)
	var unitsBeforeStored, tmpAtStored, finalAtStored string
	m.at = func(prompt string) {
		if strings.Contains(prompt, "stored") {
			unitsBeforeStored = b.roles()
			for _, f := range func() []string { l, _ := filepath.Glob(b.cfg.EnvFile + ".*"); return l }() {
				raw, _ := os.ReadFile(f)
				tmpAtStored += string(raw)
			}
			if _, err := os.Lstat(b.cfg.EnvFile); err == nil {
				finalAtStored = "exists"
			}
		}
	}
	var finalAtInit bool
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_init_") {
			_, err := os.Lstat(b.cfg.EnvFile)
			finalAtInit = err == nil
		}
	}
	rep, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err != nil {
		t.Fatalf("%v\nsaid:\n%s", err, m.saidAll())
	}
	if got, want := b.roles(), "plan probe init"; got != want {
		t.Fatalf("units: %s, want %s", got, want)
	}
	if unitsBeforeStored != "plan probe" {
		t.Fatalf("before the password was confirmed stored, units had run: %s", unitsBeforeStored)
	}
	if finalAtInit || finalAtStored != "" {
		t.Fatal("the credential file was in place before the repository had answered")
	}
	pw := shownPassword(t, m)
	// The file the probe read holds a throwaway, never the password
	// that was shown: nothing shown has taken effect before "stored".
	if !strings.Contains(tmpAtStored, "AWS_SECRET_ACCESS_KEY=the-secret") || strings.Contains(tmpAtStored, pw) {
		t.Fatalf("at the stored prompt the temp file held %q", tmpAtStored)
	}
	if !strings.Contains(m.saidAll(), "looking for a repository at s3:http://e2e-s3:9000/box") {
		t.Fatalf("said:\n%s", m.saidAll())
	}
	if got, want := m.askedAll(), "Storage key id (AWS_ACCESS_KEY_ID): \nsecret:Storage secret key (AWS_SECRET_ACCESS_KEY): \nType stored to go on: "; got != want {
		t.Fatalf("asked:\n%s\nwant:\n%s", got, want)
	}
	v := envOf(t, b.cfg.EnvFile)
	want := envfile.Values{"RESTIC_REPOSITORY": "s3:http://e2e-s3:9000/box", "RESTIC_PASSWORD": pw, "AWS_ACCESS_KEY_ID": "AKIDX", "AWS_SECRET_ACCESS_KEY": "the-secret"}
	if len(v) != len(want) {
		t.Fatalf("the file holds %v", v)
	}
	for k, w := range want {
		if v[k] != w {
			t.Fatalf("%s = %q, want %q", k, v[k], w)
		}
	}
	st, err := os.Stat(b.cfg.EnvFile)
	must(t, err)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("the file is %o", st.Mode().Perm())
	}
	if d, err := os.Stat(filepath.Dir(b.cfg.EnvFile)); err != nil || d.Mode().Perm() != 0o755 {
		t.Fatalf("the directory: %v %v", d, err)
	}
	// The directory is made root's whoever made it before, and before
	// anything is written into it.
	if len(b.owned) != 1 || b.owned[0] != filepath.Dir(b.cfg.EnvFile) {
		t.Fatalf("owned by root: %v", b.owned)
	}
	if left, _ := filepath.Glob(b.cfg.EnvFile + ".*"); len(left) != 0 {
		t.Fatalf("left beside it: %v", left)
	}
	// The init unit: the backup account, the credential through the
	// manager, network, no capability, and the temp file — the working
	// file is not touched until the repository has answered.
	s := b.spec("init")
	if s.User != backupUser || !s.Network || len(s.Capabilities) != 0 || s.SameUIDNamespaces || s.CacheDirectory != "hotserve-backup" {
		t.Fatalf("the init unit: %+v", s)
	}
	if p := b.spec("probe"); strings.Join(p.Argv, " ") != "/usr/bin/restic cat config --no-lock" || p.User != backupUser || !p.Network || len(p.Capabilities) != 0 || !strings.HasPrefix(p.EnvironmentFile, b.cfg.EnvFile+".") {
		t.Fatalf("the probe unit: %+v", p)
	}
	if s.EnvironmentFile == b.cfg.EnvFile || !strings.HasPrefix(s.EnvironmentFile, b.cfg.EnvFile+".") {
		t.Fatalf("the init unit reads %q", s.EnvironmentFile)
	}
	if got, want := strings.Join(s.Argv, " "), "/usr/bin/restic init --json"; got != want {
		t.Fatalf("argv %q", got)
	}
	if s.StdoutFile == "" || s.StderrFile == "" {
		t.Fatalf("restic's words are not kept where the run can read them: %+v", s)
	}
	for _, s := range b.specs {
		all := strings.Join(s.Argv, " ") + strings.Join(s.Environment, " ") + s.Description
		if strings.Contains(all, pw) || strings.Contains(all, "the-secret") || strings.Contains(all, "AKIDX") {
			t.Fatalf("a secret reached a unit's argv or environment: %+v", s)
		}
	}
	if !rep.New || rep.RepositoryID != repoID || rep.Account != "present" {
		t.Fatalf("report %+v", rep)
	}
	if !strings.Contains(m.saidAll(), "repository ready: s3:http://e2e-s3:9000/box (new, id bbd0e899)") {
		t.Fatalf("said:\n%s", m.saidAll())
	}
	if !strings.Contains(m.saidAll(), "no app declares a backup yet") && !strings.Contains(m.saidAll(), "blog") {
		t.Fatalf("the plan was not said:\n%s", m.saidAll())
	}
}

func TestNothingIsAskedBeforeThePreflightPasses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(b *box)
		want   string
	}{
		{"restic missing", func(b *box) { b.haveProgram = func(p string) bool { return p != b.cfg.Restic } }, "restic is not installed at /usr/bin/restic: apt install restic"},
		{"sqlite3 missing", func(b *box) { b.haveProgram = func(p string) bool { return p != "/usr/bin/sqlite3" } }, "sqlite3 is not installed at /usr/bin/sqlite3: apt install sqlite3"},
		{"hotserve missing", func(b *box) { b.haveProgram = func(p string) bool { return p != "/usr/bin/hotserve" } }, "hotserve is not installed at /usr/bin/hotserve"},
		{"old systemd", func(b *box) { b.version = 255 }, "systemd 257 or later is needed (Debian 13's); this box has 255"},
		{"the plan fails", func(b *box) {
			b.outcome["plan"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
			b.planErr = "root depends on {$LIVESWAP_ROOT}\x1b[0m: give it a literal value\n"
		}, "could not be turned into a plan (exit 1): root depends on {$LIVESWAP_ROOT} [0m: give it a literal value"},
		{"the plan fails with nothing said", func(b *box) { b.outcome["plan"] = unit.Outcome{Result: "exit-code", ExitStatus: 1} }, "could not be turned into a plan (exit 1); `journalctl -u hotserve_backup_plan_"},
		{"a run holds the lock", func(b *box) {
			unlock, err := lock(filepath.Join(b.cfg.RunDir, "lock"))
			must(b.t, err)
			b.t.Cleanup(unlock)
		}, "another backup run is in progress"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, m := setupBox(t)
			must(t, os.MkdirAll(b.cfg.RunDir, 0o700))
			tc.break_(b)
			_, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			nothingWritten(t, b, m)
			if strings.Contains(b.roles(), "init") {
				t.Fatalf("a repository unit ran: %s", b.roles())
			}
		})
	}
}

func TestARepositoryTheBoxCannotUseIsRefusedBeforeAnyPrompt(t *testing.T) {
	for _, tc := range []struct{ repo, want string }{
		{"sftp:user@host:/srv/backups", "sftp: is not supported"},
		{"/srv/backups", "a path on this box is not a repository"},
		{"local:/srv/backups", "a path on this box is not a repository"},
		{"rclone:remote:bucket", "rclone: is not supported"},
		{"azure:container:path", "azure: is not a backend Debian's restic has"},
		{"gs:bucket:path", "gs: and swift: are not set up by this command"},
		{"swift:container:/path", "gs: and swift: are not set up by this command"},
		{"s3:http://user:pass@e2e-s3:9000/box", "credentials in the repository URL"},
		{"rest:https://user:pass@host/", "credentials in the repository URL"},
		{"ftp://host/x", `"ftp:" is not a repository restic knows`},
		{"", "no repository was given"},
		{"s3:", "s3: names no bucket"},
		{"s3:http://e2e-s3:9000/box ", "the repository URL has whitespace at an end"},
		{"s3:AKID:secret@s3.example.com/bucket", "credentials in the repository URL"},
		{"rest:user:pass@host:8000/", "credentials in the repository URL"},
		{"s3:http://e2e-s3:9000/b\x01x", "the repository URL cannot go in the file: it holds a control character"},
	} {
		t.Run(tc.repo, func(t *testing.T) {
			b, m := setupBox(t)
			_, err := b.setup(t, m, tc.repo)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			nothingWritten(t, b, m)
			if b.roles() != "" {
				t.Fatalf("units ran: %s", b.roles())
			}
		})
	}
}

// What setup does not ask for, the run still uses: status must not
// warn of it. What setup refuses of the value itself — whitespace at an
// end, a control character — status warns of too.
func TestABackendWrittenByHandIsUsable(t *testing.T) {
	for _, repo := range []string{"gs:bucket:path", "swift:container:/path", "s3:http://h/b", "b2:b:p", "rest:https://h/"} {
		if err := RepositoryUsable(repo, "/etc/x"); err != nil {
			t.Errorf("%s: %v", repo, err)
		}
	}
	for _, repo := range []string{"sftp:h:/p", "local:/x", "/x", "azure:c:p", "rclone:r:b", "s3:http://u:p@h/b", "s3:u:p@h/b", "s3:http://h/b ", " s3:http://h/b", "s3:http://h/b\x01", ""} {
		if err := RepositoryUsable(repo, "/etc/x"); err == nil {
			t.Errorf("%q: usable", repo)
		}
	}
}

// restic init that exited 0 has made the repository with the password
// that was shown: whatever else went wrong after, that password is the
// repository's, and is never said to be dead.
func TestAPasswordInitUsedIsNeverSaidDead(t *testing.T) {
	b, m := setupBox(t)
	b.initOut = "not json at all\n"
	_, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err == nil || strings.Contains(err.Error(), "discard") || !strings.Contains(err.Error(), "restic made the repository and exited 0 but said nothing of it; the password shown above is the repository's: keep it") {
		t.Fatalf("err = %v", err)
	}
	m.asked = nil
	nothingWritten(t, b, m)
}

func TestEachBackendIsAskedForItsOwnVariables(t *testing.T) {
	for _, tc := range []struct {
		repo    string
		prompts string
		keys    []string
	}{
		{"b2:bucket:path", "Storage key id (B2_ACCOUNT_ID): \nsecret:Storage secret key (B2_ACCOUNT_KEY): \nType stored to go on: ", []string{"B2_ACCOUNT_ID", "B2_ACCOUNT_KEY"}},
		{"rest:https://host:8000/", "Storage user name (RESTIC_REST_USERNAME): \nsecret:Storage password (RESTIC_REST_PASSWORD): \nType stored to go on: ", []string{"RESTIC_REST_USERNAME", "RESTIC_REST_PASSWORD"}},
	} {
		t.Run(tc.repo, func(t *testing.T) {
			b, m := setupBox(t)
			if _, err := b.setup(t, m, tc.repo); err != nil {
				t.Fatal(err)
			}
			if m.askedAll() != tc.prompts {
				t.Fatalf("asked:\n%s", m.askedAll())
			}
			v := envOf(t, b.cfg.EnvFile)
			if v[tc.keys[0]] != "AKIDX" || v[tc.keys[1]] != "the-secret" || len(v) != 4 {
				t.Fatalf("the file holds %v", v)
			}
		})
	}
}

func TestThePasswordNotConfirmedStoredWritesNothing(t *testing.T) {
	// A word that is not "stored" gets one more asking; " Stored " is
	// stored — the point is that it was, not the typing.
	for _, ok := range []string{" Stored ", "STORED"} {
		b, m := setupBox(t)
		m.answers = []string{"AKIDX", "the-secret", ok}
		if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err != nil {
			t.Fatalf("%q: %v", ok, err)
		}
	}
	b, m := setupBox(t)
	m.answers = []string{"AKIDX", "the-secret", "y", "stored"}
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err != nil || strings.Count(m.askedAll(), "stored") != 2 || !strings.Contains(m.saidAll(), "that is not stored: type stored to go on") {
		t.Fatalf("err = %v\nasked:\n%s\nsaid:\n%s", err, m.askedAll(), m.saidAll())
	}
	for _, answer := range []string{"y", ""} {
		t.Run(answer, func(t *testing.T) {
			b, m := setupBox(t)
			m.answers = []string{"AKIDX", "the-secret", answer, answer}
			_, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
			if err == nil || !strings.Contains(err.Error(), "not confirmed stored: nothing was written") {
				t.Fatalf("err = %v", err)
			}
			shownPassword(t, m)
			m.asked = nil
			nothingWritten(t, b, m)
			if b.roles() != "plan probe" {
				t.Fatalf("a repository unit ran: %s", b.roles())
			}
		})
	}
	t.Run("nobody answers", func(t *testing.T) {
		b, m := setupBox(t)
		m.answers = []string{"AKIDX"}
		_, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
		if err == nil || !strings.Contains(err.Error(), "nobody answered") {
			t.Fatalf("err = %v", err)
		}
		m.asked = nil
		nothingWritten(t, b, m)
	})
}

func TestAValueTheFileCannotHoldIsAskedAgain(t *testing.T) {
	b, m := setupBox(t)
	m.answers = []string{"AKIDX", "a\x01b", "a\rb", "good", "stored"}
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err != nil {
		t.Fatal(err)
	}
	if v := envOf(t, b.cfg.EnvFile); v["AWS_SECRET_ACCESS_KEY"] != "good" {
		t.Fatalf("the file holds %q", v["AWS_SECRET_ACCESS_KEY"])
	}
	if strings.Count(m.saidAll(), "it holds a control character; again") != 2 {
		t.Fatalf("said:\n%s", m.saidAll())
	}
	// A value the plain form would not carry is written all the same,
	// quoted: a password made elsewhere is what it is.
	b, m = setupBox(t)
	m.answers = []string{" AKIDX ", `"the-secret`, "stored"}
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err != nil {
		t.Fatal(err)
	}
	if v := envOf(t, b.cfg.EnvFile); v["AWS_ACCESS_KEY_ID"] != " AKIDX " || v["AWS_SECRET_ACCESS_KEY"] != `"the-secret` {
		t.Fatalf("the file holds %q", v)
	}
	b, m = setupBox(t)
	m.answers = []string{"", "", "", "x", "stored"}
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err == nil || !strings.Contains(err.Error(), "three times") {
		t.Fatalf("err = %v", err)
	}
	m.asked = nil
	nothingWritten(t, b, m)
}

// A repository that exists is found by the probe, before any password
// is made: the operator is asked for its own, and nothing else.
func TestAnExistingRepositoryIsOpenedWithItsOwnPassword(t *testing.T) {
	b, m := setupBox(t)
	b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
	m.answers = []string{"AKIDX", "the-secret", "its-own-password"}
	rep, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err != nil {
		t.Fatalf("%v\nsaid:\n%s", err, m.saidAll())
	}
	if got, want := b.roles(), "plan probe open"; got != want {
		t.Fatalf("units: %s, want %s", got, want)
	}
	if got, want := m.askedAll(), "Storage key id (AWS_ACCESS_KEY_ID): \nsecret:Storage secret key (AWS_SECRET_ACCESS_KEY): \nsecret:Repository password: "; got != want {
		t.Fatalf("asked:\n%s", got)
	}
	if !strings.Contains(m.saidAll(), "the repository exists; its password is needed") {
		t.Fatalf("said:\n%s", m.saidAll())
	}
	if len(passwordRe.FindAllString(m.saidAll(), -1)) != 0 || strings.Contains(m.saidAll(), "stored") {
		t.Fatalf("a password was made for a repository that exists:\n%s", m.saidAll())
	}
	v := envOf(t, b.cfg.EnvFile)
	if v["RESTIC_PASSWORD"] != "its-own-password" {
		t.Fatalf("the file holds password %q", v["RESTIC_PASSWORD"])
	}
	s := b.spec("open")
	if got, want := strings.Join(s.Argv, " "), "/usr/bin/restic cat config --no-lock"; got != want {
		t.Fatalf("the open unit's argv: %q", got)
	}
	if s.User != backupUser || !s.Network || len(s.Capabilities) != 0 || !strings.HasPrefix(s.EnvironmentFile, b.cfg.EnvFile+".") {
		t.Fatalf("the open unit: %+v", s)
	}
	if rep.New || rep.RepositoryID != repoID || !strings.Contains(m.saidAll(), "repository ready: s3:http://e2e-s3:9000/box (existing, id bbd0e899)") {
		t.Fatalf("report %+v\nsaid:\n%s", rep, m.saidAll())
	}
}

// A look that failed for a reason of the unit's own — a manager that
// could not set it up, a unit that could not be started — is said as
// such, before any password is made.
func TestALookThatFailedOnItsOwnIsSaidBeforeAnyPassword(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(b *box)
		want string
	}{
		{"the manager could not set the unit up", func(b *box) { b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 217} }, "looking for the repository: systemd could not set the unit up (status 217)"},
		{"ended by a signal", func(b *box) { b.outcome["probe"] = unit.Outcome{Result: "signal"} }, "looking for the repository: restic was ended by signal"},
		{"could not be started", func(b *box) { b.err["probe"] = errors.New("starting hotserve_backup_probe: no such user") }, "no such user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, m := setupBox(t)
			tc.set(b)
			_, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if len(passwordRe.FindAllString(m.saidAll(), -1)) != 0 || strings.Contains(b.roles(), "init") {
				t.Fatalf("a password was made, or init ran (%s):\n%s", b.roles(), m.saidAll())
			}
			m.asked = nil
			nothingWritten(t, b, m)
		})
	}
}

// The init unit is the one not recorded for the next lock holder to
// stop: stopped half way it leaves a repository no password opens.
func TestTheInitUnitIsLeftToFinish(t *testing.T) {
	b, m := setupBox(t)
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(b.cfg.RunDir, "units"))
	must(t, err)
	recorded := string(raw)
	if !strings.Contains(recorded, "_plan_") || !strings.Contains(recorded, "_probe_") {
		t.Fatalf("the plan and the look are not recorded: %q", recorded)
	}
	if strings.Contains(recorded, "_init_") {
		t.Fatalf("the init unit is recorded, and the next run would stop it half way: %q", recorded)
	}
	if s := b.spec("init"); s.BindsTo != b.cfg.BindsTo {
		t.Fatalf("the init unit's BindsTo: %q", s.BindsTo)
	}
}

// The file from before this version, still there after a setup, is
// where the credential was reachable from: said, with what to do.
func TestTheOldFileStillThereIsSaid(t *testing.T) {
	b, m := setupBox(t)
	must(t, os.MkdirAll(filepath.Dir(b.cfg.OldEnvFile), 0o755))
	must(t, os.WriteFile(b.cfg.OldEnvFile, []byte("RESTIC_PASSWORD=old\n"), 0o600))
	rep, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err != nil {
		t.Fatal(err)
	}
	if rep.OldEnvFile != b.cfg.OldEnvFile || !strings.Contains(m.saidAll(), b.cfg.OldEnvFile+" is still there, from before this version, and is not read; an administrator's sudoers may reach it: remove it (sudo rm "+b.cfg.OldEnvFile+")") {
		t.Fatalf("report %+v\nsaid:\n%s", rep, m.saidAll())
	}
	if _, err := os.Lstat(b.cfg.OldEnvFile); err != nil {
		t.Fatal("the old file was removed")
	}
	b, m = setupBox(t)
	rep, err = b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err != nil || rep.OldEnvFile != "" || strings.Contains(m.saidAll(), "still there") {
		t.Fatalf("with no old file: %v %+v", err, rep)
	}
}

// Where the probe could not tell — the storage retrying on a wrong key,
// a bucket not there yet — init is what answers; and init may answer
// that the repository exists after all, in either of its wordings, when
// a password has been made and shown: it is said not to be the one.
func TestWhereTheProbeCannotTellInitAnswers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		probe unit.Outcome
	}{
		{"the probe exits 1", unit.Outcome{Result: "exit-code", ExitStatus: 1}},
		{"the probe exits 0 with nothing", unit.Outcome{Result: "success"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, wording := range []string{alreadyInit, alreadyThere} {
				b, m := setupBox(t)
				b.outcome["probe"], b.probeOut = tc.probe, ""
				b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
				// The phrase comes after the URL, which may be long: a message
				// cut for showing must not be the one matched.
				b.initOut, b.initErr = "", strings.ReplaceAll(wording, "s3:http://e2e-s3:9000/box", "s3:https://"+strings.Repeat("a-very-long-host-name.", 12)+"example/bucket")
				b.probeOut = configJSON
				m.answers = []string{"AKIDX", "the-secret", "stored", "its-own-password"}
				rep, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
				if err != nil {
					t.Fatalf("%v\nsaid:\n%s", err, m.saidAll())
				}
				if got, want := b.roles(), "plan probe init open"; got != want {
					t.Fatalf("units: %s, want %s", got, want)
				}
				if !strings.Contains(m.saidAll(), "the repository exists; its password is needed (the one shown above is not it)") {
					t.Fatalf("said:\n%s", m.saidAll())
				}
				if !strings.Contains(m.saidAll(), "the password shown above was never used: discard it") {
					t.Fatalf("the unused password was not said dead on the way out:\n%s", m.saidAll())
				}
				pw := shownPassword(t, m)
				raw, _ := os.ReadFile(b.cfg.EnvFile)
				if strings.Contains(string(raw), pw) || len(passwordRe.FindAllString(m.saidAll(), -1)) != 1 {
					t.Fatalf("the generated password: in the file %v; shown %d times", strings.Contains(string(raw), pw), len(passwordRe.FindAllString(m.saidAll(), -1)))
				}
				if rep.New || rep.RepositoryID != repoID {
					t.Fatalf("report %+v", rep)
				}
			}
		})
	}
	// And a failure after init has said the repository exists is not
	// told to keep that password: init did not make anything with it.
	b, m := setupBox(t)
	b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	b.initOut, b.initErr = "", alreadyInit
	b.outcome["open"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
	m.answers = []string{"AKIDX", "the-secret", "stored", "a", "b", "c"}
	_, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err == nil || !strings.HasSuffix(err.Error(), "the password shown above was never used: discard it") || strings.Contains(err.Error(), "keep the password") {
		t.Fatalf("err = %v", err)
	}
	// A probe that never answers is given up on, and init goes on.
	b, m = setupBox(t)
	b.hang = "probe"
	rep, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err != nil || !rep.New {
		t.Fatalf("%v %+v", err, rep)
	}
	if got, want := b.roles(), "plan probe init"; got != want || len(b.stopped) != 1 || !strings.Contains(b.stopped[0], "_probe_") {
		t.Fatalf("units: %s, stopped %v", got, b.stopped)
	}
}

// A storage that refuses the key, or cannot be reached, is a mistake
// in what was typed: the key is asked for again, up to three times,
// and the one password shown stands — shown once, stored once, and
// the password of the repository that is made in the end.
func TestAKeyTheStorageRefusesIsAskedForAgain(t *testing.T) {
	b, m := setupBox(t)
	b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	b.initOut, b.initErr = "", wrongKey
	tries := 0
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_init_") {
			if tries++; tries == 2 {
				// The right key this time.
				b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 10}
				delete(b.outcome, "init")
				b.initOut = initialized
			}
		}
	}
	m.answers = []string{"AKIDX", "wrong", "stored", "AKIDX", "the-secret"}
	rep, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err != nil {
		t.Fatalf("%v\nsaid:\n%s", err, m.saidAll())
	}
	if got, want := b.roles(), "plan probe init probe init"; got != want {
		t.Fatalf("units: %s, want %s", got, want)
	}
	if got, want := m.askedAll(), "Storage key id (AWS_ACCESS_KEY_ID): \nsecret:Storage secret key (AWS_SECRET_ACCESS_KEY): \nType stored to go on: \nStorage key id (AWS_ACCESS_KEY_ID): \nsecret:Storage secret key (AWS_SECRET_ACCESS_KEY): "; got != want {
		t.Fatalf("asked:\n%s", got)
	}
	pw := shownPassword(t, m)
	if len(passwordRe.FindAllString(m.saidAll(), -1)) != 1 {
		t.Fatalf("the password was shown more than once:\n%s", m.saidAll())
	}
	for _, want := range []string{
		"no repository answered within 100ms: a bucket not made yet, a wrong key and a wrong host look alike here; a new repository will be made, and if that fails the password shown next was never used",
		"restic could not make or open the repository (exit 1): Fatal: create repository at",
		"the storage refused the key, or could not be reached: the key id and secret again (the password shown above still applies)",
	} {
		if !strings.Contains(m.saidAll(), want) {
			t.Fatalf("said lacks %q:\n%s", want, m.saidAll())
		}
	}
	if v := envOf(t, b.cfg.EnvFile); v["RESTIC_PASSWORD"] != pw || v["AWS_SECRET_ACCESS_KEY"] != "the-secret" || !rep.New {
		t.Fatalf("the file holds %v; report %+v", v, rep)
	}
	// Three refusals: the end, and the password shown is said to be
	// dead.
	b, m = setupBox(t)
	b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	b.initOut, b.initErr = "", wrongKey
	m.answers = []string{"AKIDX", "wrong", "stored", "AKIDX", "wrong", "AKIDX", "wrong"}
	_, err = b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err == nil || !strings.HasSuffix(err.Error(), "keep the password shown above: restic init ran with it, and may have made the repository; run setup again, which looks first and asks for it if the repository is there") || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("err = %v", err)
	}
	if got, want := b.roles(), "plan probe init probe init probe init"; got != want {
		t.Fatalf("units: %s, want %s", got, want)
	}
	m.asked = nil
	nothingWritten(t, b, m)
	// An interrupt after the password was shown says so too.
	b, m = setupBox(t)
	ctx, cancel := context.WithCancel(context.Background())
	b.hang = "init"
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_init_") {
			cancel()
		}
	}
	_, err = Setup(ctx, b.cfg, b, SetupOptions{Repository: "s3:http://e2e-s3:9000/box", Terminal: m})
	if !errors.Is(err, context.Canceled) || err.Error() != "interrupted: nothing has been written; keep the password shown above: restic init ran with it, and may have made the repository; run setup again, which looks first and asks for it if the repository is there" {
		t.Fatalf("err = %v", err)
	}
	// Before init has started — at the stored prompt, say — the
	// password has taken effect nowhere and is said to be dead.
	b, m = setupBox(t)
	ctx, cancel = context.WithCancel(context.Background())
	m.answers = []string{"AKIDX", "the-secret"}
	m.at = func(prompt string) {
		if strings.Contains(prompt, "stored") {
			cancel()
		}
	}
	_, err = Setup(ctx, b.cfg, b, SetupOptions{Repository: "s3:http://e2e-s3:9000/box", Terminal: m})
	if !errors.Is(err, context.Canceled) || err.Error() != "interrupted: nothing has been written; the password shown above was never used: discard it" {
		t.Fatalf("err = %v", err)
	}
	// Unless the unit could not be seen gone: init may still be making
	// the repository with that password, so it is not called dead, and
	// the runner's own words are the error.
	b, m = setupBox(t)
	ctx, cancel = context.WithCancel(context.Background())
	b.hang, b.stopErr = "init", unit.ErrNotConfirmedGone
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_init_") {
			cancel()
		}
	}
	_, err = Setup(ctx, b.cfg, b, SetupOptions{Repository: "s3:http://e2e-s3:9000/box", Terminal: m})
	if !errors.Is(err, unit.ErrNotConfirmedGone) || strings.Contains(err.Error(), "discard it") || strings.Contains(err.Error(), "nothing has been written") {
		t.Fatalf("err = %v", err)
	}
	// Nor is a look that could not be seen gone passed over.
	b, m = setupBox(t)
	b.hang, b.stopErr = "probe", unit.ErrNotConfirmedGone
	_, err = b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if !errors.Is(err, unit.ErrNotConfirmedGone) || strings.Contains(b.roles(), "init") {
		t.Fatalf("err = %v after %s", err, b.roles())
	}
}

// A password shown on a first attempt and not used — the second
// attempt's look found the repository — is said not to be the one, and
// dead, on the way out.
func TestAPasswordShownAndNotUsedIsSaidDeadOnSuccessToo(t *testing.T) {
	b, m := setupBox(t)
	b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	b.initOut, b.initErr = "", wrongKey
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_init_") {
			b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
		}
	}
	m.answers = []string{"AKIDX", "wrong", "stored", "AKIDX", "the-secret", "its-own-password"}
	rep, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err != nil || rep.New || b.roles() != "plan probe init probe open" {
		t.Fatalf("%v %+v %s", err, rep, b.roles())
	}
	for _, want := range []string{"the repository exists; its password is needed (the one shown above is not it)", "the password shown above was never used: discard it"} {
		if !strings.Contains(m.saidAll(), want) {
			t.Fatalf("said lacks %q:\n%s", want, m.saidAll())
		}
	}
}

// A wrong password for a repository that exists is asked for again, up
// to three times.
func TestAWrongRepositoryPasswordIsAskedForAgain(t *testing.T) {
	b, m := setupBox(t)
	b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
	b.outcome["open"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
	opens := 0
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_open_") {
			if opens++; opens == 2 {
				delete(b.outcome, "open")
			}
		}
	}
	m.answers = []string{"AKIDX", "the-secret", "not-the-password", "its-own-password"}
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err != nil {
		t.Fatalf("%v\nsaid:\n%s", err, m.saidAll())
	}
	if got, want := b.roles(), "plan probe open open"; got != want || !strings.Contains(m.saidAll(), "this password cannot open the repository (exit 12): again") {
		t.Fatalf("units: %s\nsaid:\n%s", got, m.saidAll())
	}
	if v := envOf(t, b.cfg.EnvFile); v["RESTIC_PASSWORD"] != "its-own-password" {
		t.Fatalf("the file holds %v", v)
	}
	b, m = setupBox(t)
	b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
	b.outcome["open"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
	m.answers = []string{"AKIDX", "the-secret", "a", "b", "c"}
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err == nil || !strings.Contains(err.Error(), "this password cannot open the repository (exit 12)") || b.roles() != "plan probe open open open" {
		t.Fatalf("err = %v, units %s", err, b.roles())
	}
}

// A mistake over a working setup leaves the working file byte for byte,
// with no copy of a credential beside it and no unit running.
func TestAMistakeLeavesAWorkingSetupAsItWas(t *testing.T) {
	const working = "RESTIC_REPOSITORY=s3:http://e2e-s3:9000/box\nRESTIC_PASSWORD=old\nAWS_ACCESS_KEY_ID=A\nAWS_SECRET_ACCESS_KEY=B\n"
	for _, tc := range []struct {
		name   string
		break_ func(b *box, m *term)
		want   string
	}{
		{"wrong key", func(b *box, m *term) {
			b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
			b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
			b.initOut, b.initErr = "", wrongKey
			m.answers = []string{"AKIDX", "wrong", "stored", "AKIDX", "wrong", "AKIDX", "wrong"}
		}, "restic could not make or open the repository (exit 1): Fatal: create repository at"},
		{"wrong password for an existing repository", func(b *box, m *term) {
			b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
			b.outcome["open"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
			b.probeOut = ""
			m.answers = []string{"AKIDX", "the-secret", "a", "b", "c"}
		}, "this password cannot open the repository (exit 12)"},
		{"the repository gone between the probe and the opening", func(b *box, m *term) {
			b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
			b.outcome["open"] = unit.Outcome{Result: "exit-code", ExitStatus: 10}
			b.probeOut = ""
			m.answers = []string{"AKIDX", "the-secret", "pw"}
		}, "there is no repository at the configured location (exit 10)"},
		{"the storage refusing the opening, in restic's words", func(b *box, m *term) {
			b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
			b.outcome["open"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
			b.probeOut, b.openErr = "", "Fatal: unable to open repository: AccessDenied: keys/ is not yours to read\n"
			m.answers = []string{"AKIDX", "the-secret", "pw"}
		}, "restic could not open the repository (exit 1): Fatal: unable to open repository: AccessDenied: keys/ is not yours to read"},
		{"init exits 0 and names no repository", func(b *box, m *term) { b.initOut = "" }, "restic made the repository and exited 0 but said nothing of it; the password shown above is the repository's: keep it"},
		{"init ended by a signal", func(b *box, m *term) {
			b.outcome["init"] = unit.Outcome{Result: "signal"}
		}, "restic was ended by signal"},
		{"the manager could not set the unit up", func(b *box, m *term) {
			b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 217}
		}, "systemd could not set the unit up (status 217)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, m := setupBox(t)
			must(t, os.MkdirAll(filepath.Dir(b.cfg.EnvFile), 0o755))
			must(t, os.WriteFile(b.cfg.EnvFile, []byte(working), 0o600))
			tc.break_(b, m)
			_, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q\nsaid:\n%s", err, tc.want, m.saidAll())
			}
			raw, _ := os.ReadFile(b.cfg.EnvFile)
			if string(raw) != working {
				t.Fatalf("the working file was changed:\n%s", raw)
			}
			if left, _ := filepath.Glob(b.cfg.EnvFile + ".*"); len(left) != 0 {
				t.Fatalf("a copy of a credential was left: %v", left)
			}
			if entries, _ := os.ReadDir(filepath.Dir(b.cfg.EnvFile)); len(entries) != 1 {
				t.Fatalf("left in the directory: %v", entries)
			}
			if strings.Contains(err.Error(), "no repository") && tc.name == "wrong key" {
				t.Fatalf("an exit 1 was called no repository: %v", err)
			}
		})
	}
}

func TestARepositoryThatDoesNotAnswerIsGivenUpOnAndTheUnitStopped(t *testing.T) {
	b, m := setupBox(t)
	b.hang = "init"
	_, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err == nil || !strings.Contains(err.Error(), "the repository did not answer within") {
		t.Fatalf("err = %v", err)
	}
	if len(b.stopped) != 1 || !strings.Contains(b.stopped[0], "_init_") {
		t.Fatalf("stopped: %v", b.stopped)
	}
	m.asked = nil
	nothingWritten(t, b, m)
	// What a person waiting on init is told: Ctrl-C stops a unit that
	// may have made the repository with the shown password — never
	// "nothing has been written", which is the look's and the opening's.
	if !strings.Contains(m.saidAll(), "still waiting for s3:http://e2e-s3:9000/box (Ctrl-C stops it; restic init may have made the repository with the password shown: keep it)") || strings.Contains(m.saidAll(), "nothing has been written") {
		t.Fatalf("said:\n%s", m.saidAll())
	}
	b, m = setupBox(t)
	b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
	b.hang = "open"
	m.answers = []string{"AKIDX", "the-secret", "its-own-password"}
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err == nil || !strings.Contains(m.saidAll(), "still waiting for s3:http://e2e-s3:9000/box (Ctrl-C is safe: nothing has been written)") {
		t.Fatalf("err %v\nsaid:\n%s", err, m.saidAll())
	}
	// A unit the runner could not see gone is not said to have been
	// stopped: the runner's own words are the error.
	b, m = setupBox(t)
	b.hang, b.stopErr = "init", unit.ErrNotConfirmedGone
	_, err = b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err == nil || strings.Contains(err.Error(), "the unit was stopped") || !errors.Is(err, unit.ErrNotConfirmedGone) {
		t.Fatalf("err = %v", err)
	}
}

func TestAnInterruptDuringInitStopsTheUnitAndWritesNothing(t *testing.T) {
	b, m := setupBox(t)
	ctx, cancel := context.WithCancel(context.Background())
	b.hang = "init"
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_init_") {
			cancel()
		}
	}
	_, err := Setup(ctx, b.cfg, b, SetupOptions{Repository: "s3:http://e2e-s3:9000/box", Terminal: m})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if len(b.stopped) != 1 {
		t.Fatalf("stopped: %v", b.stopped)
	}
	m.asked = nil
	nothingWritten(t, b, m)
}

func TestTheRecordIsPutAsideWithAChangeOfRepository(t *testing.T) {
	for _, tc := range []struct {
		name  string
		old   string // the file before, or none
		aside bool
	}{
		{"another repository", "RESTIC_REPOSITORY=s3:http://e2e-s3:9000/other\nRESTIC_PASSWORD=x\n", true},
		{"the same repository", "RESTIC_REPOSITORY=s3:http://e2e-s3:9000/box\nRESTIC_PASSWORD=x\n", false},
		{"the same, quoted", "RESTIC_REPOSITORY=\"s3:http://e2e-s3:9000/box\"\nRESTIC_PASSWORD=x\n", false},
		{"no file before", "", true},
		{"the same URL, re-made", "RESTIC_REPOSITORY=s3:http://e2e-s3:9000/box\nRESTIC_PASSWORD=x\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, m := setupBox(t)
			if tc.name != "the same URL, re-made" {
				// The repository is there: the look finds it, and the
				// record it goes with is kept.
				b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
				m.answers = []string{"AKIDX", "the-secret", "its-own-password"}
			}
			if tc.old != "" {
				must(t, os.MkdirAll(filepath.Dir(b.cfg.EnvFile), 0o755))
				must(t, os.WriteFile(b.cfg.EnvFile, []byte(tc.old), 0o600))
			}
			must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
			must(t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{Apps: map[string]*record.App{"blog": {Class: record.OK}}}))
			rep, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
			if err != nil {
				t.Fatal(err)
			}
			st, err := record.Read(filepath.Join(b.cfg.StateDir, "status.json"))
			must(t, err)
			asides, _ := filepath.Glob(filepath.Join(b.cfg.StateDir, "status.json.aside-*"))
			if tc.aside {
				why := "the previous credential file named another repository"
				switch {
				case tc.old == "":
					why = "no credential file was there to tie it to this repository"
				case tc.name == "the same URL, re-made":
					why = "the repository was made by this setup, so it holds none of the record's snapshots"
				}
				if len(st.Apps) != 0 || len(asides) != 1 || rep.Aside != asides[0] || !strings.Contains(m.saidAll(), "the record was put aside ("+why+"): "+asides[0]+"; the next run drills what it backs up") {
					t.Fatalf("the record was kept: %v %v %+v\n%s", st.Apps, asides, rep, m.saidAll())
				}
				if st, err := os.Stat(asides[0]); err != nil || st.Mode().Perm() != 0o644 {
					t.Fatalf("the aside: %v %v", st, err)
				}
			} else if len(st.Apps) != 1 || len(asides) != 0 || rep.Aside != "" {
				t.Fatalf("the record was put aside: %v %v", st.Apps, asides)
			}
		})
	}
	t.Run("no record", func(t *testing.T) {
		b, m := setupBox(t)
		if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err != nil {
			t.Fatal(err)
		}
		if asides, _ := filepath.Glob(filepath.Join(b.cfg.StateDir, "status.json*")); len(asides) != 0 {
			t.Fatalf("a record appeared: %v", asides)
		}
	})
}

// A file that could not be put in place keeps the record it goes
// with: the box is still on the old repository.
func TestARecordIsPutAsideOnlyOnceTheFileIsInPlace(t *testing.T) {
	b, m := setupBox(t)
	must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
	must(t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{Apps: map[string]*record.App{"blog": {Class: record.OK}}}))
	// A directory where the file has to go: the rename over it fails.
	must(t, os.MkdirAll(b.cfg.EnvFile, 0o755))
	_, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err == nil || b.roles() != "plan probe init" {
		t.Fatalf("setup with a directory in the file's place: %v after %s", err, b.roles())
	}
	st, err := record.Read(filepath.Join(b.cfg.StateDir, "status.json"))
	must(t, err)
	if asides, _ := filepath.Glob(filepath.Join(b.cfg.StateDir, "status.json.aside-*")); len(st.Apps) != 1 || len(asides) != 0 {
		t.Fatalf("the record was put aside: %v %v", st.Apps, asides)
	}
}

// A credential directory that is a link leads the root-only file
// somewhere else: refused, before anything is written.
func TestACredentialDirectoryThatIsALinkIsRefused(t *testing.T) {
	b, m := setupBox(t)
	dir := filepath.Dir(b.cfg.EnvFile)
	must(t, os.MkdirAll(filepath.Dir(dir), 0o755))
	elsewhere := t.TempDir()
	must(t, os.Symlink(elsewhere, dir))
	_, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err == nil || !strings.Contains(err.Error(), dir+" is a link, and the credential file has to be in a directory of root's own") {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 || len(m.asked) != 0 {
		t.Fatalf("written through the link: %v; asked %v", entries, m.asked)
	}
}

// The record is put aside before the file takes its place, and put
// back if the file cannot: whatever ends setup between the two leaves
// no credential file with a record of another repository beside it.
// The aside is on the disk before the file is, and so is a put-back.
func TestTheRecordIsAsideBeforeTheFileIsInPlace(t *testing.T) {
	b, m := setupBox(t)
	must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
	statusPath := filepath.Join(b.cfg.StateDir, "status.json")
	must(t, record.Write(statusPath, &record.Status{Apps: map[string]*record.App{"blog": {Class: record.OK}}}))
	var recordAtCommit string
	oldCommit := commit
	commit = func(from, to string) error {
		if _, err := os.Lstat(statusPath); err == nil {
			recordAtCommit = "in place"
		} else {
			recordAtCommit = "aside"
		}
		return oldCommit(from, to)
	}
	t.Cleanup(func() { commit = oldCommit })
	var syncedAtCommit int
	inner := commit
	commit = func(from, to string) error {
		syncedAtCommit = len(b.synced)
		return inner(from, to)
	}
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err != nil {
		t.Fatal(err)
	}
	if recordAtCommit != "aside" {
		t.Fatalf("at the moment the file took its place the record was %s", recordAtCommit)
	}
	if syncedAtCommit != 1 || b.synced[0] != b.cfg.StateDir {
		t.Fatalf("the state directory was not put on the disk before the file was committed: synced %v", b.synced)
	}
	// And a put-back is synced too.
	b, m = setupBox(t)
	must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
	must(t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{Apps: map[string]*record.App{"blog": {Class: record.OK}}}))
	must(t, os.MkdirAll(b.cfg.EnvFile, 0o755))
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err == nil || len(b.synced) != 2 {
		t.Fatalf("err %v, synced %v", err, b.synced)
	}
}

// What each failure between the aside and the commit leaves: the two
// files agree, whichever step failed.
func TestAFailureBetweenTheAsideAndTheCommitLeavesTheTwoFilesAgreeing(t *testing.T) {
	seed := func(t *testing.T) (*box, *term, string) {
		b, m := setupBox(t)
		must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
		statusPath := filepath.Join(b.cfg.StateDir, "status.json")
		must(t, record.Write(statusPath, &record.Status{Apps: map[string]*record.App{"blog": {Class: record.OK}}}))
		return b, m, statusPath
	}
	recordInPlace := func(t *testing.T, statusPath string) {
		t.Helper()
		st, err := record.Read(statusPath)
		must(t, err)
		if asides, _ := filepath.Glob(statusPath + ".aside-*"); len(st.Apps) != 1 || len(asides) != 0 {
			t.Fatalf("the record: %v, asides %v", st.Apps, asides)
		}
	}
	// The sync after the aside fails: the record is put back, and that
	// put-back is synced, before the error is returned.
	b, m, statusPath := seed(t)
	calls := 0
	syncDir = func(path string) error {
		calls++
		b.synced = append(b.synced, path)
		if calls == 1 {
			return errors.New("EIO on the state directory")
		}
		return nil
	}
	_, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err == nil || !strings.Contains(err.Error(), "EIO on the state directory") {
		t.Fatalf("err = %v", err)
	}
	recordInPlace(t, statusPath)
	if _, err := os.Lstat(b.cfg.EnvFile); err == nil {
		t.Fatal("the credential file was put in place after the aside could not be synced")
	}
	if len(b.synced) != 2 {
		t.Fatalf("the put-back was not synced: %v", b.synced)
	}
	// The credential's rename fails: the record is put back.
	b, m, statusPath = seed(t)
	must(t, os.MkdirAll(b.cfg.EnvFile, 0o755))
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err == nil {
		t.Fatal("setup succeeded with a directory in the file's place")
	}
	recordInPlace(t, statusPath)
	// The credential directory's sync fails after the rename: the file
	// is in place, so the record stays aside — the two agree — and the
	// error says what state was left.
	b, m, statusPath = seed(t)
	calls = 0
	syncDir = func(path string) error {
		calls++
		b.synced = append(b.synced, path)
		if path == filepath.Dir(b.cfg.EnvFile) {
			return errors.New("EIO on the credential directory")
		}
		return nil
	}
	_, err = b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err == nil || !strings.Contains(err.Error(), "EIO on the credential directory") || !strings.Contains(err.Error(), "the credential file is in place") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Lstat(b.cfg.EnvFile); err != nil {
		t.Fatal("the credential file is not in place")
	}
	st, err := record.Read(statusPath)
	must(t, err)
	if asides, _ := filepath.Glob(statusPath + ".aside-*"); len(st.Apps) != 0 || len(asides) != 1 {
		t.Fatalf("the record was put back under a file already in place: %v, asides %v", st.Apps, asides)
	}
}

func TestTheAccountIsMadeWhenMissingAndLeftAloneWhenNot(t *testing.T) {
	b, m := setupBox(t)
	b.account = false
	rep, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err != nil {
		t.Fatal(err)
	}
	if b.accountsMade != 1 || rep.Account != "made" || !strings.Contains(m.saidAll(), "account hotserve-backup: made") {
		t.Fatalf("made %d, report %+v\n%s", b.accountsMade, rep, m.saidAll())
	}
	b, m = setupBox(t)
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err != nil {
		t.Fatal(err)
	}
	if b.accountsMade != 0 || !strings.Contains(m.saidAll(), "account hotserve-backup: present") {
		t.Fatalf("made %d\n%s", b.accountsMade, m.saidAll())
	}
	if !strings.Contains(useraddArgv(), "useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin hotserve-backup") {
		t.Fatalf("useradd: %s", useraddArgv())
	}
}

func TestALeftoverTempFileIsRemovedFirstAndSaid(t *testing.T) {
	b, m := setupBox(t)
	dir := filepath.Dir(b.cfg.EnvFile)
	must(t, os.MkdirAll(dir, 0o755))
	// The two shapes a setup that did not live to the end leaves: the
	// file the units read, and the one envfile.Write makes on the way
	// to it. What an operator keeps beside the file is theirs.
	stale := b.cfg.EnvFile + ".0123456789ab"
	dotted := filepath.Join(dir, ".repository.env.0123456789ab-4207310592")
	kept := []string{b.cfg.EnvFile + ".bak", b.cfg.EnvFile + ".old", filepath.Join(dir, "repository.env.gs"), filepath.Join(dir, "notes.txt")}
	for _, f := range append([]string{stale, dotted}, kept...) {
		must(t, os.WriteFile(f, []byte("RESTIC_PASSWORD=leaked\n"), 0o600))
	}
	rep, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{stale, dotted} {
		if _, err := os.Lstat(f); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the leftover %s is still there", f)
		}
	}
	for _, f := range kept {
		if _, err := os.Lstat(f); err != nil {
			t.Errorf("the operator's %s was removed", f)
		}
	}
	if len(rep.Swept) != 2 || !strings.Contains(m.saidAll(), "removed a file an interrupted setup left: "+stale) || !strings.Contains(m.saidAll(), "removed a file an interrupted setup left: "+dotted) {
		t.Fatalf("report %+v\n%s", rep, m.saidAll())
	}
}

func TestThePlanIsSaidAndAPlanWithNoAppGoesOn(t *testing.T) {
	b, m := setupBox(t)
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.saidAll(), "a run would back up blog, under "+b.root) {
		t.Fatalf("said:\n%s", m.saidAll())
	}
	b, m = setupBox(t)
	b.plan = `{"root":"/var/lib/liveswap","apps":{}}`
	rep, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Apps) != 0 || !strings.Contains(m.saidAll(), "no app declares a backup yet: a run would back nothing up") {
		t.Fatalf("report %+v\n%s", rep, m.saidAll())
	}
}

func TestBeginNamesTheOldPathWhenItIsThereAndTheNewIsNot(t *testing.T) {
	b, _ := setupBox(t)
	must(t, os.MkdirAll(filepath.Dir(b.cfg.OldEnvFile), 0o755))
	must(t, os.WriteFile(b.cfg.OldEnvFile, []byte("RESTIC_PASSWORD=old\n"), 0o600))
	_, err := Run(context.Background(), b.cfg, b)
	if err == nil || !strings.Contains(err.Error(), "backups are not set up: "+b.cfg.EnvFile+" is not there") || !strings.Contains(err.Error(), b.cfg.OldEnvFile+" is from before this version and is not read: run `sudo hotserve-backup setup <its RESTIC_REPOSITORY>`, which asks for its RESTIC_PASSWORD; then remove it") || strings.Contains(err.Error(), "lstat") {
		t.Fatalf("err = %v", err)
	}
	if b.roles() != "" {
		t.Fatalf("units ran: %s", b.roles())
	}
	must(t, os.Remove(b.cfg.OldEnvFile))
	_, err = Run(context.Background(), b.cfg, b)
	if err == nil || strings.Contains(err.Error(), "before this version") {
		t.Fatalf("err = %v", err)
	}
}
