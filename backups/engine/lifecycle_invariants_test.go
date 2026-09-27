package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/hotserve/backups/record"
	"github.com/smallhoursorg/hotserve/backups/unit"
)

// The invariants of unit lifecycle across every command, as tables — the
// rule from 4b: a finding that fits one is a row, never a test of its
// own. Each row is one command on one box; what must never happen is
// asserted beside what must.

const testRepo = "s3:http://e2e-s3:9000/box"

// command is one of the commands a lock holder can be, driven against a
// box of its own kind.
type command struct {
	name string
	box  func(t *testing.T) (*box, *term)
	do   func(t *testing.T, b *box, m *term) error
}

// The three that need a credential file, on a box whose repository
// holds snapshots of blog. A restore --to needs a parent that is
// root's own, which the unit lane has only as root: that row skips
// elsewhere, as the restore tests do.
var runDrillRestore = []command{
	{"a run", plainBox, func(_ *testing.T, b *box, _ *term) error { _, err := Run(context.Background(), b.cfg, b); return err }},
	{"a drill", plainBox, func(_ *testing.T, b *box, _ *term) error { _, err := Drill(context.Background(), b.cfg, b); return err }},
	{"a restore in place", plainBox, func(_ *testing.T, b *box, _ *term) error {
		_, err := Restore(context.Background(), b.cfg, b, inPlace())
		return err
	}},
	{"a restore --to", plainBox, func(t *testing.T, b *box, _ *term) error {
		_, err := Restore(context.Background(), b.cfg, b, RestoreOptions{App: "blog", To: filepath.Join(toParent(t), "out")})
		return err
	}},
}

var setupCommand = command{"setup", setupBox, func(t *testing.T, b *box, m *term) error { _, err := b.setup(t, m, testRepo); return err }}

func plainBox(t *testing.T) (*box, *term) { return restoreBox(t), nil }

// Nothing starts before setup: with no credential file, a run, a drill
// and a restore each refuse naming the path — and the old path, where a
// file from before this version is still there, saying it is not read —
// start no unit and write no record. The unit file's own condition on
// the file (units_test, cmd/hotserve-backup) stands in front of this;
// this is what holds when the file goes between that check and the
// run's own.
func TestNothingStartsBeforeSetup(t *testing.T) {
	note := " is from before this version and is not read: run `sudo hotserve-backup setup <its RESTIC_REPOSITORY>`, which asks for its RESTIC_PASSWORD; then remove it"
	for _, cmd := range runDrillRestore {
		for _, old := range []string{"", ", with the old file beside"} {
			t.Run(cmd.name+old, func(t *testing.T) {
				b, m := cmd.box(t)
				must(t, os.Remove(b.cfg.EnvFile))
				b.cfg.OldEnvFile = filepath.Join(filepath.Dir(b.cfg.EnvFile), "hotserve", "backup.env")
				if old != "" {
					must(t, os.MkdirAll(filepath.Dir(b.cfg.OldEnvFile), 0o755))
					must(t, os.WriteFile(b.cfg.OldEnvFile, []byte("RESTIC_PASSWORD=old\n"), 0o600))
				}
				err := cmd.do(t, b, m)
				if err == nil || !strings.Contains(err.Error(), "backups are not set up: "+b.cfg.EnvFile+" is not there") || strings.Contains(err.Error(), "lstat") {
					t.Fatalf("err = %v", err)
				}
				if strings.Contains(err.Error(), b.cfg.OldEnvFile+note) != (old != "") {
					t.Fatalf("the old path: err = %v", err)
				}
				neverStarted(t, b, m)
			})
		}
	}
}

// Every unit a command starts is bound to the command's own service,
// when it is one — the manager then ends them if the command is killed,
// however it dies [M8] — and to nothing when it is a shell's: from a
// shell there is no service, and the signal handler and the next lock
// holder's sweep do that work. Three are never bound. Setup's init
// unit, left to finish making the repository whatever becomes of
// setup. And the two that remove plaintext, clean and unstage: a
// service that is being stopped has a stop job queued, and the manager
// refuses to start a unit bound to it ("transaction is destructive"
// [M62]), so bound, the copies a stopped run made stayed until the
// next run; they hold no credential and no network, and are over in
// moments.
func TestEveryUnitOfACommandIsBoundToItsService(t *testing.T) {
	for _, cmd := range append(runDrillRestore, setupCommand) {
		for _, own := range []string{"hotserve-backup.service", ""} {
			t.Run(cmd.name+" bound to "+own, func(t *testing.T) {
				b, m := cmd.box(t)
				b.cfg.BindsTo = own
				if err := cmd.do(t, b, m); err != nil {
					t.Fatal(err)
				}
				if len(b.specs) == 0 {
					t.Fatal("no unit ran: the row proves nothing")
				}
				unbound := 0
				for _, s := range b.specs {
					want := own
					if strings.Contains(s.Name, "_init_") || strings.Contains(s.Name, "_clean_") || strings.Contains(s.Name, "_unstage_") {
						want = ""
						unbound++
					}
					if s.BindsTo != want {
						t.Errorf("%s: BindsTo = %q, want %q", s.Name, s.BindsTo, want)
					}
				}
				if unbound == len(b.specs) {
					t.Fatal("no unit of the command is one that is bound: the row proves nothing")
				}
			})
		}
	}
}

// One lock across every command: whoever holds it, whoever comes says
// so, naming the holder, and starts nothing, asks nothing, and writes
// no record. Setup included — an operator at its prompts holds the
// lock for as long as the prompts wait, and a run that comes due says
// so and does nothing.
func TestOneLockAcrossCommands(t *testing.T) {
	for _, cmd := range append(runDrillRestore, setupCommand) {
		t.Run(cmd.name, func(t *testing.T) {
			b, m := cmd.box(t)
			must(t, os.MkdirAll(b.cfg.RunDir, 0o700))
			unlock, err := lock(filepath.Join(b.cfg.RunDir, "lock"))
			must(t, err)
			defer unlock()
			err = cmd.do(t, b, m)
			if !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), "pid ") {
				t.Fatalf("err = %v", err)
			}
			neverStarted(t, b, m)
			if m != nil {
				if _, err := os.Lstat(filepath.Dir(b.cfg.EnvFile)); err == nil {
					t.Fatal("setup made the credential directory while another command held the lock")
				}
			}
		})
	}
}

// neverStarted: no unit, no record, and nobody asked anything.
func neverStarted(t *testing.T, b *box, m *term) {
	t.Helper()
	if len(b.specs) != 0 {
		t.Fatalf("units were started: %s", b.roles())
	}
	if _, err := os.Lstat(filepath.Join(b.cfg.StateDir, "status.json")); err == nil {
		t.Fatal("a record was written")
	}
	if m != nil && len(m.asked) != 0 {
		t.Fatalf("asked %q", m.asked)
	}
}

// What setup refuses, a run refuses, in setup's words and before any
// unit: a missing restic, sqlite3 or hotserve (a unit whose command is
// not there ends 203/EXEC, which says nothing of what to install), and
// the account restic runs as — not there, or one someone else can be
// (Copilot on #153: the credential reaches restic's environment on
// every run, not at setup, so the run is where the check has to hold).
// One check, in the one place a run, a restore and a drill share. A run
// records the refusal as the run's own error and a drill as one that
// could not begin, so that status fails with the unit rather than
// reporting the run before as ok for three hours; a restore says it
// at the terminal, as every refusal of a restore is.
func TestARunRefusesWhatSetupRefusesWithSetupsWords(t *testing.T) {
	for _, p := range []struct {
		name string
		set  func(b *box)
		want string
	}{
		{"restic", func(b *box) { b.haveProgram = func(path string) bool { return path != "/usr/bin/restic" } }, "restic is not installed at /usr/bin/restic: apt install restic"},
		{"sqlite3", func(b *box) { b.haveProgram = func(path string) bool { return path != "/usr/bin/sqlite3" } }, "sqlite3 is not installed at /usr/bin/sqlite3: apt install sqlite3"},
		{"hotserve", func(b *box) { b.haveProgram = func(path string) bool { return path != "/usr/bin/hotserve" } }, "hotserve is not installed at /usr/bin/hotserve"},
		{"hotserve-backup", func(b *box) { b.haveProgram = func(path string) bool { return path != "/usr/bin/hotserve-backup" } }, "hotserve-backup is not installed at /usr/bin/hotserve-backup, where the units run it"},
		{"the account", func(b *box) { b.account = false }, "the hotserve-backup account is not there: sudo hotserve-backup setup <repository> makes it (" + useraddArgv() + ")"},
		{"a login shell", func(b *box) { b.shell = "/bin/bash" }, "the hotserve-backup account exists with a login shell (/bin/bash, not one of /usr/sbin/nologin, /sbin/nologin, /bin/false, /usr/bin/false)"},
		{"a shared uid", func(b *box) { b.holders = []string{"alice", "hotserve-backup"} }, "a uid shared with alice (995)"},
		{"the hotserve group", func(b *box) { b.groups = []int{995, b.hotserveGid} }, "the hotserve group among its groups"},
		{"a password", func(b *box) { b.shadow = "$y$j9T$abc" }, "a password that is not locked"},
	} {
		for _, cmd := range runDrillRestore {
			t.Run(cmd.name+" without "+p.name, func(t *testing.T) {
				b, m := cmd.box(t)
				p.set(b)
				err := cmd.do(t, b, m)
				if err == nil || !strings.Contains(err.Error(), p.want) {
					t.Fatalf("err = %v", err)
				}
				if len(b.specs) != 0 {
					t.Fatalf("units were started: %s", b.roles())
				}
				st, rerr := record.Read(filepath.Join(b.cfg.StateDir, "status.json"))
				switch cmd.name {
				case "a run":
					if rerr != nil || !strings.Contains(st.Error, p.want) {
						t.Fatalf("the run's record: %+v, %v", st, rerr)
					}
				case "a drill":
					if rerr != nil || st.LastDrill == nil || !strings.Contains(st.LastDrill.Detail, p.want) {
						t.Fatalf("the drill's record: %+v, %v", st, rerr)
					}
				default:
					if _, err := os.Lstat(filepath.Join(b.cfg.StateDir, "status.json")); err == nil {
						t.Fatal("a restore wrote a record")
					}
				}
			})
		}
	}
}

// A drill that could not begin records why — a program not there, a
// unit that would not stop — but an interrupt is no verdict: stopped
// while it waits, the drill leaves the last drill's time and detail as
// they were, as it leaves the app an interrupt lands on. And what open
// had to say — a record it could not read and replaced — reaches the
// record it writes.
func TestADrillStoppedWhileItWaitsIsNoVerdict(t *testing.T) {
	b := restoreBox(t)
	last := &record.Drill{Time: time.Date(2026, 9, 20, 3, 30, 0, 0, time.UTC), Detail: ""}
	must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
	must(t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{LastDrill: last, Apps: map[string]*record.App{}}))
	must(t, os.MkdirAll(b.cfg.RunDir, 0o700))
	must(t, os.WriteFile(filepath.Join(b.cfg.RunDir, "init-unit"), []byte("hotserve_backup_init_0123456789ab.service\n"), 0o600))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Drill(ctx, b.cfg, b); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	st, err := record.Read(filepath.Join(b.cfg.StateDir, "status.json"))
	must(t, err)
	if st.LastDrill == nil || !st.LastDrill.Time.Equal(last.Time) || st.LastDrill.Detail != "" {
		t.Fatalf("the last drill's verdict was replaced by an interrupt: %+v", st.LastDrill)
	}

	// Stopped during the plan unit, the same: the plan that could not be
	// read is the interrupt's doing, and no verdict either.
	b = restoreBox(t)
	must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
	must(t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{LastDrill: last, Apps: map[string]*record.App{}}))
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_plan_") {
			cancel()
		}
	}
	b.err["plan"] = context.Canceled
	if _, err := Drill(ctx, b.cfg, b); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if !b.started("plan") {
		t.Fatal("the plan unit never started: the row proves nothing")
	}
	st, err = record.Read(filepath.Join(b.cfg.StateDir, "status.json"))
	must(t, err)
	if st.LastDrill == nil || !st.LastDrill.Time.Equal(last.Time) || st.LastDrill.Detail != "" {
		t.Fatalf("the last drill's verdict was replaced by an interrupt during the plan: %+v", st.LastDrill)
	}

	// A record that could not be read is said by the drill that could
	// not begin, beside why it could not.
	b = restoreBox(t)
	b.haveProgram = func(path string) bool { return path != "/usr/bin/restic" }
	must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
	must(t, os.WriteFile(filepath.Join(b.cfg.StateDir, "status.json"), []byte("not json"), 0o644))
	if _, err := Drill(context.Background(), b.cfg, b); err == nil {
		t.Fatal("a drill without restic began")
	}
	st, err = record.Read(filepath.Join(b.cfg.StateDir, "status.json"))
	must(t, err)
	if st.LastDrill == nil || !strings.Contains(st.LastDrill.Detail, "restic is not installed") || !strings.Contains(st.Warning, "could not be read and was replaced") {
		t.Fatalf("the record: last drill %+v, warning %q", st.LastDrill, st.Warning)
	}
}

// The account restic runs as is one useradd line in two places: setup's,
// and the package's postinstall, which makes it before setup ever runs
// (the owner, 2026-09-26). The script holds setup's argv verbatim, so
// that the two cannot drift.
func TestPostinstallMakesTheAccountAsSetupDoes(t *testing.T) {
	line := "useradd " + strings.Join(useradd[1:], " ")
	// And the e2e fixture that makes it by hand, and the README that
	// says how: every copy of the line, or one drifts.
	for _, f := range []string{"../../packaging/postinstall.sh", "../../e2e/backup/lib.sh", "../README.md"} {
		raw, err := os.ReadFile(f)
		must(t, err)
		if !strings.Contains(string(raw), line) {
			t.Fatalf("%s does not make the account as setup does:\nwant a line holding %q", f, line)
		}
	}
}

// An existing hotserve-backup account is not trusted by name: made by
// hand with a login shell or a real home, whoever can log in as it can
// read the repository credential from a running restic's environment.
// Setup refuses it, naming what is wrong and the two ways to mend it,
// and changes nothing — normalising an account an administrator made
// on purpose is not setup's to do (Copilot round 11 on #152; the owner,
// deferred to 4c).
func TestAnAccountMadeWrongIsRefusedNotNormalised(t *testing.T) {
	const (
		lock   = "lock it (usermod --shell /usr/sbin/nologin --home /nonexistent hotserve-backup)"
		locked = "lock it (usermod --lock --shell /usr/sbin/nologin --home /nonexistent hotserve-backup)"
		remake = "remove it (userdel hotserve-backup) and make it as setup would"
		shells = ", not one of /usr/sbin/nologin, /sbin/nologin, /bin/false, /usr/bin/false)"
	)
	me := 1000 // the hotserve data user's uid, and the gid of the group named hotserve
	ok := acct{shell: "/usr/sbin/nologin", home: "/nonexistent", uid: 995, gid: 995, passwordField: "x", shadow: "!"}
	with := func(f func(a *acct)) acct { a := ok; f(&a); return a }
	for _, tc := range []struct {
		name    string
		a       acct
		refused []string // in the error; nil means accepted
		remedy  string   // the one the fault is mended by; the other must not be named alone
	}{
		{"as setup makes it", ok, nil, ""},
		{"false for a shell", with(func(a *acct) { a.shell = "/bin/false" }), nil, ""},
		{"the other paths", with(func(a *acct) { a.shell = "/sbin/nologin" }), nil, ""},
		{"a home named and not there", with(func(a *acct) { a.home = "/home/hotserve-backup" }), nil, ""},
		// /nonexistent is the home setup gives, and a directory of that
		// name is nobody's home: not refused, or setup's own account
		// would be, and the usermod the message names would change nothing.
		{"/nonexistent, which exists on this box", with(func(a *acct) { a.homeThere = true }), nil, ""},

		// What a usermod mends: the shell, which is what login runs —
		// only the paths known to refuse a login, not a name (a copy of
		// bash at /tmp/nologin is a login shell) — and the home.
		{"a login shell", with(func(a *acct) { a.shell = "/bin/bash" }), []string{"a login shell (/bin/bash" + shells}, lock},
		{"a shell named nologin elsewhere", with(func(a *acct) { a.shell = "/tmp/nologin" }), []string{"a login shell (/tmp/nologin" + shells}, lock},
		{"no shell at all, which login reads as /bin/sh", with(func(a *acct) { a.shell = "" }), []string{"a login shell (none set, which is /bin/sh)"}, lock},
		{"a home that exists", with(func(a *acct) { a.home, a.homeThere = "/home/hotserve-backup", true }), []string{"a home directory that exists (/home/hotserve-backup)"}, lock},
		{"both", with(func(a *acct) { a.shell, a.home, a.homeThere = "/bin/sh", "/var/lib/hotserve-backup", true }), []string{"a login shell (/bin/sh, not one of", "a home directory that exists (/var/lib/hotserve-backup)"}, lock},

		// A password: the shell refuses whoever logs in with it, and an
		// sshd that serves sftp itself runs no shell [M70]. Locked is
		// what useradd --system leaves, and what is asked for.
		{"a password locked, as useradd leaves it", with(func(a *acct) { a.shadow = "!" }), nil, ""},
		{"a password set and then locked", with(func(a *acct) { a.shadow = "!$y$j9T$abc" }), nil, ""},
		{"no login by password", with(func(a *acct) { a.shadow = "*" }), nil, ""},
		{"no shadow entry: nothing to log in with", with(func(a *acct) { a.noShadow = true }), nil, ""},
		// A directory's account: its passwd line holds the mark, and the
		// password, if it has one, is the directory's to know.
		{"a mark in the passwd line itself", with(func(a *acct) { a.passwordField = "*"; a.shadow = "$y$never-asked-for" }), nil, ""},
		{"a password", with(func(a *acct) { a.shadow = "$y$j9T$abc" }), []string{"a password that is not locked"}, locked},
		{"an empty password", with(func(a *acct) { a.shadow = "" }), []string{"no password at all, so that anyone logs in as it"}, locked},
		{"a password in the passwd line itself", with(func(a *acct) { a.passwordField = "$1$old" }), []string{"a password that is not locked"}, locked},
		{"an empty password field in the passwd line", with(func(a *acct) { a.passwordField = "" }), []string{"no password at all, so that anyone logs in as it"}, locked},
		{"a password and a login shell", with(func(a *acct) { a.shadow, a.shell = "$y$j9T$abc", "/bin/bash" }), []string{"a password that is not locked", "a login shell (/bin/bash"}, locked},

		// What no usermod of the shell mends: who the account is. It is
		// what every restic unit runs as, User= by name, and
		// authorization is by uid — whoever else holds it is the restic
		// process — and by group: the manager gives a unit its
		// account's groups, and in the hotserve group the plan unit and
		// restic read the apps' env files and their 0750 directories,
		// the separation the account exists for.
		{"uid 0", with(func(a *acct) { a.uid = 0 }), []string{"uid 0 (root)"}, remake},
		{"gid 0", with(func(a *acct) { a.gid = 0; a.groups = []int{0} }), []string{"gid 0 (root)"}, remake},
		{"the hotserve user's uid", with(func(a *acct) { a.uid = me; a.holders = []string{"hotserve", "hotserve-backup"} }), []string{"a uid shared with hotserve (1000)"}, remake},
		{"a uid shared with two accounts", with(func(a *acct) { a.uid = 1001; a.holders = []string{"alice", "hotserve-backup", "bob"} }), []string{"a uid shared with alice, bob (1001)"}, remake},
		{"root's group among its groups", with(func(a *acct) { a.groups = []int{995, 0} }), []string{"root's group among its groups (gid 0)"}, remake},
		{"the hotserve group among its groups", with(func(a *acct) { a.groups = []int{995, me} }), []string{"the hotserve group among its groups (gid 1000)"}, remake},
		{"the hotserve group as its own", with(func(a *acct) { a.gid = me; a.groups = []int{me} }), []string{"the hotserve group among its groups (gid 1000)"}, remake},
		// The group is the one named hotserve, which the apps' env files
		// and directories belong to — not whatever the hotserve user's
		// primary group is: an account made by hand may have another
		// (the package smoke found the difference).
		{"the hotserve group, the hotserve user's primary group being another", with(func(a *acct) { a.groups = []int{995, me}; a.dataGid = 2000 }), []string{"the hotserve group among its groups (gid 1000)"}, remake},
		{"the hotserve user's primary group, which is not the hotserve group", with(func(a *acct) { a.groups = []int{995, 2000}; a.dataGid = 2000 }), nil, ""},

		// Both kinds: both remedies, each said of what it mends.
		{"a login shell and a shared uid", with(func(a *acct) { a.shell = "/bin/bash"; a.holders = []string{"alice", "hotserve-backup"} }), []string{"a login shell (/bin/bash", "a uid shared with alice (995)", remake}, remake},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, m := setupBox(t)
			b.shell, b.home, b.homeThere, b.uid, b.gid = tc.a.shell, tc.a.home, tc.a.homeThere, tc.a.uid, tc.a.gid
			b.passwordField, b.shadow, b.noShadow = tc.a.passwordField, tc.a.shadow, tc.a.noShadow
			if tc.a.holders != nil {
				b.holders = tc.a.holders
			}
			if tc.a.groups != nil {
				b.groups = tc.a.groups
			}
			dataGid := me
			if tc.a.dataGid != 0 {
				dataGid = tc.a.dataGid
			}
			dataOwner = func() (int, int, error) { return me, dataGid, nil } // restored by the box's cleanup
			rep, err := b.setup(t, m, testRepo)
			if tc.refused == nil {
				if err != nil || rep.Account != "present" {
					t.Fatalf("accepted account refused: %+v, %v", rep, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("not refused: %+v", rep)
			}
			for _, w := range append(tc.refused, "the hotserve-backup account", tc.remedy) {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err = %v\nwant it to say %q", err, w)
				}
			}
			// A remedy that does not mend the fault is not named: a
			// usermod of the shell changes nothing of who the account is.
			if tc.remedy == remake && len(tc.refused) == 1 && strings.Contains(err.Error(), "usermod") {
				t.Errorf("err = %v\nnames a usermod for a fault it does not mend", err)
			}
			if tc.remedy == remake && !strings.Contains(err.Error(), useraddArgv()) {
				t.Errorf("err = %v\nwant the useradd line", err)
			}
			if b.accountsMade != 0 || b.shell != tc.a.shell || b.home != tc.a.home {
				t.Fatalf("the account was changed: made %d, shell %q, home %q", b.accountsMade, b.shell, b.home)
			}
			neverStarted(t, b, m)
			if _, err := os.Lstat(filepath.Dir(b.cfg.EnvFile)); err == nil {
				t.Fatal("the credential directory was made before the account was found wrong")
			}
		})
	}
}

// acct is an account as a row of the table has it.
type acct struct {
	shell, home string
	homeThere   bool
	uid, gid    int
	holders     []string
	groups      []int
	dataGid     int // the hotserve user's primary gid, where it is not the hotserve group's
	// The second field of its passwd line, its password as the shadow
	// database holds it, and whether that database holds it at all.
	passwordField, shadow string
	noShadow              bool
}
