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
// holder's sweep do that work. The one exception is setup's init unit,
// left to finish making the repository whatever becomes of setup.
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
				for _, s := range b.specs {
					want := own
					if strings.Contains(s.Name, "_init_") {
						want = ""
					}
					if s.BindsTo != want {
						t.Errorf("%s: BindsTo = %q, want %q", s.Name, s.BindsTo, want)
					}
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
	usermod := "usermod --shell /usr/sbin/nologin --home /nonexistent hotserve-backup"
	me := 1000 // the hotserve data user's uid on this box
	for _, tc := range []struct {
		name, shell, home string
		homeThere         bool
		uid, gid          int
		refused           []string // in the error; nil means accepted
	}{
		{"as setup makes it", "/usr/sbin/nologin", "/nonexistent", false, 995, 995, nil},
		{"false for a shell", "/bin/false", "/nonexistent", false, 995, 995, nil},
		{"the other paths", "/sbin/nologin", "/nonexistent", false, 995, 995, nil},
		// The shell is what login runs: only the paths that are known
		// to refuse a login are taken, not a name (a copy of bash at
		// /tmp/nologin is a login shell).
		{"a shell named nologin elsewhere", "/tmp/nologin", "/nonexistent", false, 995, 995, []string{"a login shell (/tmp/nologin, not one of /usr/sbin/nologin, /sbin/nologin, /bin/false, /usr/bin/false)"}},
		{"a home named and not there", "/usr/sbin/nologin", "/home/hotserve-backup", false, 995, 995, nil},
		// /nonexistent is the home setup gives, and a directory of that
		// name is nobody's home: not refused, or setup's own account
		// would be, and the usermod the message names would change nothing.
		{"/nonexistent, which exists on this box", "/usr/sbin/nologin", "/nonexistent", true, 995, 995, nil},
		{"a login shell", "/bin/bash", "/nonexistent", false, 995, 995, []string{"a login shell (/bin/bash, not one of /usr/sbin/nologin, /sbin/nologin, /bin/false, /usr/bin/false)"}},
		{"no shell at all, which login reads as /bin/sh", "", "/nonexistent", false, 995, 995, []string{"a login shell (none set, which is /bin/sh)"}},
		{"a home that exists", "/usr/sbin/nologin", "/home/hotserve-backup", true, 995, 995, []string{"a home directory that exists (/home/hotserve-backup)"}},
		{"both", "/bin/sh", "/var/lib/hotserve-backup", true, 995, 995, []string{"a login shell (/bin/sh, not one of", "a home directory that exists (/var/lib/hotserve-backup)"}},
		// The account is what every restic unit runs as: root, or the
		// hotserve data user, would run restic with the credential as
		// root or as the apps' own uid — the two the design keeps it
		// away from.
		{"uid 0", "/usr/sbin/nologin", "/nonexistent", false, 0, 995, []string{"uid 0 (root)"}},
		{"gid 0", "/usr/sbin/nologin", "/nonexistent", false, 995, 0, []string{"gid 0 (root)"}},
		// Authorization is by uid: whoever else holds it is the restic
		// process, and reads its environment. The hotserve user is one
		// such account; any other is the same.
		{"the hotserve user's uid", "/usr/sbin/nologin", "/nonexistent", false, me, 995, []string{"a uid shared with hotserve (1000)"}},
		{"a uid shared with two accounts", "/usr/sbin/nologin", "/nonexistent", false, 1001, 995, []string{"a uid shared with alice, bob (1001)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, m := setupBox(t)
			b.shell, b.home, b.homeThere, b.uid, b.gid = tc.shell, tc.home, tc.homeThere, tc.uid, tc.gid
			switch tc.uid {
			case me:
				b.holders = []string{"hotserve", "hotserve-backup"}
			case 1001:
				b.holders = []string{"alice", "hotserve-backup", "bob"}
			}
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
			for _, w := range append(tc.refused, "the hotserve-backup account", usermod, useraddArgv()) {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err = %v\nwant it to say %q", err, w)
				}
			}
			if b.accountsMade != 0 || b.shell != tc.shell || b.home != tc.home {
				t.Fatalf("the account was changed: made %d, shell %q, home %q", b.accountsMade, b.shell, b.home)
			}
			neverStarted(t, b, m)
			if _, err := os.Lstat(filepath.Dir(b.cfg.EnvFile)); err == nil {
				t.Fatal("the credential directory was made before the account was found wrong")
			}
		})
	}
}
