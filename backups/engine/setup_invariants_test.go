package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smallhoursorg/hotserve/backups/envfile"
	"github.com/smallhoursorg/hotserve/backups/record"
	"github.com/smallhoursorg/hotserve/backups/unit"
)

// The invariants of setup, each a table: every way a thing can happen
// against everything that must hold of it. A finding that fits a table
// is a row added, never a test of its own.

// Whatever leaves init running leaves it what it needs. Four ways to
// leave it: its clock, an interrupt, a runner that lost sight of it,
// and a stop it could not confirm. Whichever, the unit is not stopped,
// it is recorded for the next lock holder, its staged credential file
// and its run directory stay — the manager opens both in the unit's
// own first moments — and the operator is told to keep the password.
func TestWhateverLeavesInitRunningLeavesItWhatItNeeds(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(b *box, cancel func())
	}{
		{"its clock", func(b *box, _ func()) { b.hang = "init" }},
		{"an interrupt", func(b *box, cancel func()) {
			b.hang = "init"
			b.before = func(s unit.Spec) {
				if strings.Contains(s.Name, "_init_") {
					cancel()
				}
			}
		}},
		{"a runner that lost sight of it", func(b *box, _ func()) {
			b.err["init"] = errors.New("hotserve_backup_init: its state could not be read 10 times running")
		}},
		{"a stop it could not confirm", func(b *box, _ func()) {
			b.err["init"] = fmt.Errorf("%w: hotserve_backup_init: (stopping it: timeout)", unit.ErrNotConfirmedGone)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, m := setupBox(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tc.set(b, cancel)
			_, err := Setup(ctx, b.cfg, b, SetupOptions{Repository: "s3:http://e2e-s3:9000/box", Terminal: m})
			if err == nil || !strings.Contains(err.Error(), "keep the password shown above") {
				t.Fatalf("err = %v", err)
			}
			if len(b.stopped) != 0 {
				t.Fatalf("init was stopped: %v", b.stopped)
			}
			raw, rerr := os.ReadFile(filepath.Join(b.cfg.RunDir, "init-unit"))
			if rerr != nil || !strings.Contains(string(raw), "_init_") {
				t.Fatalf("init is not recorded for the next lock holder: %q %v", raw, rerr)
			}
			if _, err := os.Lstat(envfile.Staged(b.cfg.EnvFile)); err != nil {
				t.Fatal("the staged file init reads its environment from is gone")
			}
			if _, err := os.Lstat(b.cfg.EnvFile); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("the working file was written")
			}
			if runDirs(t, b) != 1 {
				t.Fatal("the run directory init writes its output to is gone")
			}
		})
	}
}

// Every lock holder waits for an init left running, then sweeps what
// it was left: setup, a run and a drill alike, on a box with a
// credential file and on one with none but the staged file — a first
// setup interrupted mid-init leaves exactly that.
func TestEveryLockHolderWaitsForAnInitLeftRunningThenSweeps(t *testing.T) {
	const left = "hotserve_backup_init_0123456789ab.service"
	holders := []struct {
		name string
		hold func(b *box) error
	}{
		{"setup", func(b *box) error {
			_, err := b.setup(b.t, &term{answers: []string{"AKIDX", "the-secret", "stored"}}, "s3:http://e2e-s3:9000/box")
			return err
		}},
		{"a run", func(b *box) error { _, err := Run(context.Background(), b.cfg, b); return err }},
		{"a drill", func(b *box) error { _, err := Drill(context.Background(), b.cfg, b); return err }},
	}
	for _, box := range []string{"with a credential file", "with the staged file alone"} {
		for _, h := range holders {
			t.Run(box+", "+h.name, func(t *testing.T) {
				b, _ := setupBox(t)
				must(t, os.MkdirAll(b.cfg.RunDir, 0o700))
				must(t, os.WriteFile(filepath.Join(b.cfg.RunDir, "init-unit"), []byte(left+"\n"), 0o600))
				must(t, os.MkdirAll(filepath.Join(b.cfg.RunDir, "0123456789ab"), 0o700))
				must(t, os.WriteFile(filepath.Join(b.cfg.RunDir, "0123456789ab", "init.err"), nil, 0o600))
				must(t, os.MkdirAll(filepath.Dir(b.cfg.EnvFile), 0o755))
				staged := envfile.Staged(b.cfg.EnvFile)
				must(t, envfile.Write(staged, []envfile.Pair{{Key: "RESTIC_REPOSITORY", Value: "s3:http://e2e-s3:9000/box"}, {Key: "RESTIC_PASSWORD", Value: "shown"}}))
				if box == "with a credential file" {
					must(t, envfile.Write(b.cfg.EnvFile, []envfile.Pair{{Key: "RESTIC_REPOSITORY", Value: "s3:http://e2e-s3:9000/box"}, {Key: "RESTIC_PASSWORD", Value: "x"}}))
				}
				err := h.hold(b)
				if len(b.waited) != 1 || b.waited[0] != left {
					t.Fatalf("%s did not wait for the init left running: waited %v (err %v)", h.name, b.waited, err)
				}
				if _, err := os.Lstat(staged); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s left the staged file, and its password, behind (err %v)", h.name, err)
				}
				if _, err := os.Lstat(filepath.Join(b.cfg.RunDir, "0123456789ab")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s left the run directory behind", h.name)
				}
				if _, err := os.Lstat(filepath.Join(b.cfg.RunDir, "init-unit")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s left the marker after the wait", h.name)
				}
				if box == "with the staged file alone" && h.name != "setup" && (err == nil || !strings.Contains(err.Error(), "backups are not set up")) {
					t.Fatalf("%s on a box with no credential file: %v", h.name, err)
				}
			})
		}
	}
}

// A power cut at any point of the commit leaves a state the next setup
// reads rightly. Only a directory's sync makes its writes durable: the
// durable state at any moment is each directory as it was at its last
// sync. For each such state a run then writes a record against the
// repository the durable credential file names, and a setup onto the
// new repository must put that record aside whenever the file named
// the old one — never keep a record written against another
// repository. Where the durable state is whole, the record is kept.
func TestAPowerCutAtAnyPointOfTheCommitLeavesAStateTheNextSetupReads(t *testing.T) {
	const oldRepo, newRepo = "s3:http://e2e-s3:9000/old", "s3:http://e2e-s3:9000/box"
	const oldID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	type dirState map[string]string // file name → content, one directory, files alone
	snap := func(dir string) dirState {
		st := dirState{}
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if !e.IsDir() {
				raw, _ := os.ReadFile(filepath.Join(dir, e.Name()))
				st[e.Name()] = string(raw)
			}
		}
		return st
	}
	type durable struct{ state, etc dirState }
	key := func(d durable) string { return fmt.Sprintf("%v|%v", d.state, d.etc) }

	// The capture: a setup from the old repository to a new one, with a
	// record beside the old id, every hook noting the durable state
	// before and after it.
	b, m := setupBox(t)
	etc := filepath.Dir(b.cfg.EnvFile)
	must(t, os.MkdirAll(etc, 0o755))
	must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
	must(t, envfile.Write(b.cfg.EnvFile, []envfile.Pair{{Key: "RESTIC_REPOSITORY", Value: oldRepo}, {Key: "RESTIC_PASSWORD", Value: "old"}}))
	must(t, os.WriteFile(filepath.Join(b.cfg.StateDir, "repository-id"), []byte(oldID+"\n"), 0o644))
	must(t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{Apps: map[string]*record.App{"blog": {Class: record.OK}}}))
	last := durable{snap(b.cfg.StateDir), snap(etc)}
	var states []durable
	seen := map[string]bool{}
	note := func() {
		if !seen[key(last)] {
			seen[key(last)] = true
			states = append(states, last)
		}
	}
	note()
	oldCommit, oldWrite := commit, writeID
	t.Cleanup(func() { commit, writeID = oldCommit, oldWrite })
	commit = func(from, to string) error { note(); err := oldCommit(from, to); note(); return err }
	writeID = func(path, id string) error { note(); err := oldWrite(path, id); note(); return err }
	syncDir = func(path string) error {
		note()
		switch path {
		case b.cfg.StateDir:
			last = durable{snap(b.cfg.StateDir), last.etc}
		case etc:
			last = durable{last.state, snap(etc)}
		}
		note()
		return nil
	}
	if _, err := b.setup(t, m, newRepo); err != nil {
		t.Fatal(err)
	}
	note()
	if len(states) < 4 {
		t.Fatalf("only %d durable states were seen", len(states))
	}

	// Each durable state on a fresh box: a run's record, then a setup
	// onto the new repository, which it opens with its own password.
	for i, d := range states {
		t.Run(fmt.Sprintf("state %d", i), func(t *testing.T) {
			b, m := setupBox(t)
			etc := filepath.Dir(b.cfg.EnvFile)
			must(t, os.MkdirAll(etc, 0o755))
			must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
			for name, content := range d.etc {
				must(t, os.WriteFile(filepath.Join(etc, name), []byte(content), 0o600))
			}
			for name, content := range d.state {
				must(t, os.WriteFile(filepath.Join(b.cfg.StateDir, name), []byte(content), 0o644))
			}
			fileNames := "" // the repository the durable credential file names, if any
			if raw, err := os.ReadFile(b.cfg.EnvFile); err == nil {
				v, _ := envfile.Parse(raw)
				fileNames = v["RESTIC_REPOSITORY"]
			}
			// A run writes its record against the repository the file names.
			must(t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{Apps: map[string]*record.App{"blog": {Class: record.OK}}}))
			b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
			m.answers = []string{"AKIDX", "the-secret", "its-own-password"}
			rep, err := b.setup(t, m, newRepo)
			if err != nil {
				t.Fatalf("%v\nsaid:\n%s", err, m.saidAll())
			}
			whole := fileNames == newRepo && strings.TrimSpace(d.state["repository-id"]) == repoID
			switch {
			case fileNames != newRepo && rep.Aside == "":
				t.Fatalf("the record was written against %q and kept for %s\ndurable: %v\nsaid:\n%s", fileNames, newRepo, d, m.saidAll())
			case whole && rep.Aside != "":
				t.Fatalf("a whole state had its record put aside: %s\nsaid:\n%s", rep.Aside, m.saidAll())
			}
			if left, _ := filepath.Glob(b.cfg.EnvFile + ".*"); len(left) != 0 {
				t.Fatalf("a leftover was not swept: %v", left)
			}
		})
	}
}

// Every exit after the password was shown says what became of it, in
// one of three ways, from what setup knows: made, ran, or never used.
// The made and the never-used are certain; "ran" is the rest.
func TestEveryExitAfterTheShowingSaysOneOfThreeFates(t *testing.T) {
	const (
		made = "the password shown above is the repository's: keep it"
		ran  = "keep the password shown above: restic init ran with it"
		dead = "the password shown above was never used: discard it"
	)
	seedRecord := func(b *box) {
		must(b.t, os.MkdirAll(b.cfg.StateDir, 0o755))
		must(b.t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{Apps: map[string]*record.App{"blog": {Class: record.OK}}}))
	}
	existsOwn := func(b *box, m *term) { // the look could not tell, init says exists, the operator's own password opens it
		b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
		b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
		b.initOut, b.initErr = "", alreadyInit
		m.answers = []string{"AKIDX", "the-secret", "stored", "its-own-password"}
	}
	for _, tc := range []struct {
		name string
		set  func(b *box, m *term)
		want string
	}{
		{"init made it; the credential directory's sync fails", func(b *box, m *term) {
			syncDir = func(path string) error {
				if path == filepath.Dir(b.cfg.EnvFile) {
					return errors.New("EIO on the credential directory")
				}
				return nil
			}
		}, made},
		{"init made it; the id cannot be written", func(b *box, m *term) {
			writeID = func(string, string) error { return errors.New("EIO on the id") }
		}, made},
		{"init made it; the state directory's sync after the id fails", func(b *box, m *term) {
			syncs := 0
			syncDir = func(path string) error {
				if path == b.cfg.StateDir {
					if syncs++; syncs == 1 {
						return errors.New("EIO on the state directory")
					}
				}
				return nil
			}
		}, made},
		{"init made it; the aside's sync fails before the commit", func(b *box, m *term) {
			seedRecord(b)
			syncDir = func(string) error { return errors.New("EIO on the state directory") }
		}, made},
		{"init made it; the commit fails", func(b *box, m *term) {
			commit = func(string, string) error { return errors.New("EIO on the rename") }
		}, made},
		{"init was left running at its clock", func(b *box, m *term) { b.hang = "init" }, ran},
		{"init failed three times on the key", func(b *box, m *term) {
			b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
			b.initOut, b.initErr = "", wrongKey
			m.answers = []string{"AKIDX", "wrong", "stored", "AKIDX", "wrong", "AKIDX", "wrong"}
		}, ran},
		{"it existed, its own password opened it; the credential directory's sync fails", func(b *box, m *term) {
			existsOwn(b, m)
			syncDir = func(path string) error {
				if path == filepath.Dir(b.cfg.EnvFile) {
					return errors.New("EIO on the credential directory")
				}
				return nil
			}
		}, dead},
		{"it existed, its own password opened it; the id cannot be written", func(b *box, m *term) {
			existsOwn(b, m)
			writeID = func(string, string) error { return errors.New("EIO on the id") }
		}, dead},
		{"it existed, its own password opened it; the commit fails", func(b *box, m *term) {
			existsOwn(b, m)
			commit = func(string, string) error { return errors.New("EIO on the rename") }
		}, dead},
		{"the write of the password before init fails", func(b *box, m *term) {
			writes := 0
			writeEnv = func(path string, pairs []envfile.Pair) error {
				if writes++; writes == 2 {
					return errors.New("ENOSPC on the credential directory")
				}
				return envfile.Write(path, pairs)
			}
		}, dead},
		{"the opening's write fails after init said the repository exists", func(b *box, m *term) {
			existsOwn(b, m)
			writes := 0
			writeEnv = func(path string, pairs []envfile.Pair) error {
				if writes++; writes == 3 {
					return errors.New("ENOSPC on the credential directory")
				}
				return envfile.Write(path, pairs)
			}
		}, dead},
		{"init exited 0 and said nothing of the id", func(b *box, m *term) { b.initOut = "not json at all\n" }, made},
		{"the shown password opened what an earlier init made; the commit fails", func(b *box, m *term) {
			b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
			b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
			b.initOut, b.initErr = "", wrongKey
			inits := 0
			b.before = func(s unit.Spec) {
				if strings.Contains(s.Name, "_init_") {
					if inits++; inits == 1 {
						b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
					}
				}
			}
			m.answers = []string{"AKIDX", "wrong", "stored", "AKIDX", "the-secret"}
			commit = func(string, string) error { return errors.New("EIO on the rename") }
		}, made},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, m := setupBox(t)
			oldCommit, oldWrite := commit, writeEnv
			t.Cleanup(func() { commit, writeEnv = oldCommit, oldWrite })
			tc.set(b, m)
			_, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q\nsaid:\n%s", err, tc.want, m.saidAll())
			}
			if strings.Count(err.Error(), "the password shown above") != 1 {
				t.Fatalf("the password's fate is said more than once: %v", err)
			}
		})
	}
}

// runDirs counts the run directories under RunDir.
func runDirs(t *testing.T, b *box) int {
	t.Helper()
	entries, _ := os.ReadDir(b.cfg.RunDir)
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			n++
		}
	}
	return n
}
