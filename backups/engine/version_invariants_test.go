package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smallhoursorg/hotserve/backups/record"
	"github.com/smallhoursorg/hotserve/backups/unit"
)

// A command and the helpers it starts are one version, or the helper
// does nothing. An upgrade of the package leaves a run under way alone
// (the owner, 2026-09-27): its upload, which is restic's, finishes —
// stopped, it was sent again whole — and the run goes on as the
// program it was started as. The helpers it starts from then on are
// started by path, and are the new version's: read strictly, an answer
// of another shape is refused, and an answer of the same shape that
// means something else is what nothing would see. So each helper is
// told which program started it, by that program's own hash, and one
// that is another does nothing and says so with a status of its own.
// The helper that removes plaintext is the exception: what it is asked
// is to empty one directory, and copies left on a disk for an hour are
// the worse of the two.

const thisRun = "0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f"

func sameVersion(t *testing.T) {
	t.Helper()
	old := whichProgram
	whichProgram = func() (string, error) { return thisRun, nil }
	t.Cleanup(func() { whichProgram = old })
}

// Every unit a command starts from its own program is told which
// program that is, and no other unit is: restic is nobody's version.
func TestEveryHelperIsToldWhichProgramStartedIt(t *testing.T) {
	for _, cmd := range append(runDrillRestore, setupCommand) {
		t.Run(cmd.name, func(t *testing.T) {
			sameVersion(t)
			b, m := cmd.box(t)
			if err := cmd.do(t, b, m); err != nil {
				t.Fatal(err)
			}
			told := 0
			for _, s := range b.specs {
				own := len(s.Argv) > 0 && s.Argv[0] == b.cfg.Self
				var said []string
				for _, e := range s.Environment {
					if strings.HasPrefix(e, RunIdentityEnv+"=") {
						said = append(said, e)
					}
				}
				switch {
				case own && (len(said) != 1 || said[0] != RunIdentityEnv+"="+thisRun):
					t.Errorf("%s, started from %s, is told %q", s.Name, s.Argv[0], said)
				case !own && len(said) != 0:
					t.Errorf("%s, which runs %s, is told %q", s.Name, s.Argv[0], said)
				case own:
					told++
				}
			}
			if told == 0 {
				t.Fatal("no unit of the command's own program ran: the row proves nothing")
			}
		})
	}
}

// A program that cannot say which it is starts nothing.
func TestACommandThatCannotReadItselfStartsNothing(t *testing.T) {
	for _, cmd := range runDrillRestore {
		t.Run(cmd.name, func(t *testing.T) {
			old := whichProgram
			whichProgram = func() (string, error) { return "", fmt.Errorf("open /proc/self/exe: permission denied") }
			t.Cleanup(func() { whichProgram = old })
			b, m := cmd.box(t)
			err := cmd.do(t, b, m)
			if err == nil || !strings.Contains(err.Error(), "which version of hotserve-backup this is could not be read: open /proc/self/exe: permission denied") {
				t.Fatalf("err = %v", err)
			}
			if len(b.specs) != 0 {
				t.Fatalf("units were started: %s", b.roles())
			}
		})
	}
}

// What the program is: the hash of the file it was started from, as
// the kernel still holds it, whatever is at its path by now.
func TestWhichProgramThisIs(t *testing.T) {
	self, err := os.Executable()
	must(t, err)
	raw, err := os.ReadFile(self)
	must(t, err)
	sum := sha256.Sum256(raw)
	got, err := whichProgram()
	if err != nil || got != hex.EncodeToString(sum[:]) {
		t.Fatalf("whichProgram = %q, %v; the file is %s", got, err, hex.EncodeToString(sum[:]))
	}
}

const upgraded = "the package was upgraded while this command was under way: its %s helper is another version's, and did nothing; the same command, run again, is of one version"

var anotherVersions = unit.Outcome{Result: "exit-code", ExitStatus: OtherVersionStatus}

// A helper that is another version's ends the command, and is said.
func TestAHelperOfAnotherVersionEndsTheCommand(t *testing.T) {
	two := func(b *box) {
		must(b.t, os.MkdirAll(filepath.Join(b.root, "shop", "shared", "uploads"), 0o755))
		b.plan = fmt.Sprintf(`{"root":%q,"apps":{"blog":{"sqlite":["app.db"],"files":["uploads"]},"shop":{"sqlite":["app.db"],"files":["uploads"]}}}`, b.root)
	}
	t.Run("the plan, of a run", func(t *testing.T) {
		sameVersion(t)
		b := newBox(t)
		b.outcome["plan"] = anotherVersions
		st, err := Run(context.Background(), b.cfg, b)
		want := fmt.Sprintf(upgraded, "plan")
		if err == nil || !strings.Contains(err.Error(), want) || st == nil || !strings.Contains(st.Error, want) {
			t.Fatalf("err = %v, record %+v", err, st)
		}
		if strings.Contains(err.Error(), "could not be turned into a plan") || strings.Contains(err.Error(), "journalctl") {
			t.Errorf("a helper that did nothing is said to have failed at the Caddyfile: %v", err)
		}
		if got := b.roles(); got != "plan" {
			t.Fatalf("units started: %s", got)
		}
	})
	t.Run("the plan, of a drill", func(t *testing.T) {
		sameVersion(t)
		b := restoreBox(t)
		b.outcome["plan"] = anotherVersions
		st, err := Drill(context.Background(), b.cfg, b)
		want := fmt.Sprintf(upgraded, "plan")
		if err == nil || st == nil || st.LastDrill == nil || !strings.Contains(st.LastDrill.Detail, want) {
			t.Fatalf("err = %v, record %+v", err, st)
		}
	})
	t.Run("the plan, of a restore", func(t *testing.T) {
		sameVersion(t)
		b := restoreBox(t)
		b.outcome["plan"] = anotherVersions
		_, err := Restore(context.Background(), b.cfg, b, inPlace())
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf(upgraded, "plan")) {
			t.Fatalf("err = %v", err)
		}
		if got := b.roles(); got != "plan" {
			t.Fatalf("units started: %s", got)
		}
	})
	t.Run("the dump, of the first of two apps", func(t *testing.T) {
		sameVersion(t)
		b := newBox(t)
		two(b)
		b.outcome["dump"] = anotherVersions
		st, err := Run(context.Background(), b.cfg, b)
		if err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf(upgraded, "dump")
		blog, shop := st.Apps["blog"], st.Apps["shop"]
		if blog == nil || blog.Class != record.Failed || !strings.Contains(blog.Detail, want) {
			t.Fatalf("blog: %+v", blog)
		}
		if shop == nil || shop.Class != record.NotAttempted || !strings.Contains(shop.Detail, want) {
			t.Fatalf("shop, which no helper of this version would have served either: %+v", shop)
		}
		// Nothing was uploaded of an app whose databases nobody copied,
		// and what was staged is removed all the same, by the one helper
		// that works for any version.
		if b.started("upload") || b.started("verify") {
			t.Fatalf("units started: %s", b.roles())
		}
		if !b.started("clean") {
			t.Fatalf("the plaintext was left: %s", b.roles())
		}
		if n := strings.Count(" "+b.roles()+" ", " dump "); n != 1 {
			t.Fatalf("the dump helper was started %d times: %s", n, b.roles())
		}
	})
	t.Run("the check, of a drill of two apps", func(t *testing.T) {
		sameVersion(t)
		b := restoreBox(t)
		two(b)
		b.outcome["check"] = anotherVersions
		st, err := Drill(context.Background(), b.cfg, b)
		want := fmt.Sprintf(upgraded, "check")
		if err != nil || st.Apps["blog"] == nil || st.Apps["blog"].RestoreDrill == nil || !strings.Contains(st.Apps["blog"].RestoreDrill.Detail, want) {
			t.Fatalf("err = %v, blog %+v", err, st.Apps["blog"])
		}
		if st.Apps["shop"] == nil || st.Apps["shop"].RestoreDrill == nil || !strings.Contains(st.Apps["shop"].RestoreDrill.Detail, want) {
			t.Fatalf("shop: %+v", st.Apps["shop"])
		}
		if n := strings.Count(" "+b.roles()+" ", " fetch "); n != 1 {
			t.Fatalf("a second app was fetched for a helper that would do nothing: %s", b.roles())
		}
		if !b.started("unstage") {
			t.Fatalf("what was fetched was left: %s", b.roles())
		}
	})
	t.Run("the install, of a restore into place", func(t *testing.T) {
		sameVersion(t)
		b := restoreBox(t)
		b.outcome["install"] = anotherVersions
		_, err := Restore(context.Background(), b.cfg, b, inPlace())
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf(upgraded, "install")) {
			t.Fatalf("err = %v", err)
		}
		// It did nothing, and is not said to have begun.
		if strings.Contains(err.Error(), "partly restored") || strings.Contains(err.Error(), "no usable answer") {
			t.Errorf("a helper that did nothing is said to have begun: %v", err)
		}
	})
	// The one that removes plaintext is asked whatever the version, and
	// its status is read as any unit's: 75 of it is a clean that failed
	// — here the one that empties staging before the dump — and never
	// taken for an upgrade.
	t.Run("the clean, which is every version's", func(t *testing.T) {
		sameVersion(t)
		b := newBox(t)
		b.outcome["clean"] = anotherVersions
		st, err := Run(context.Background(), b.cfg, b)
		if err != nil {
			t.Fatal(err)
		}
		if blog := st.Apps["blog"]; blog == nil || strings.Contains(blog.Detail, "was upgraded") || !strings.Contains(blog.Detail, "emptying staging before the run: exit 75") {
			t.Fatalf("blog: %+v", blog)
		}
	})
}
