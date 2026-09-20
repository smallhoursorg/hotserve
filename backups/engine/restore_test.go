package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/hotserve/backups/record"
	"github.com/smallhoursorg/hotserve/backups/unit"
)

const snapB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// restoreBox is a box whose repository holds two snapshots of blog, the
// newer of them snapA.
func restoreBox(t *testing.T) *box {
	t.Helper()
	b := newBox(t)
	b.history = fmt.Sprintf(`[{"id":%q,"time":"2026-09-01T00:00:00Z"},{"id":%q,"time":"2026-09-02T00:00:00Z"}]`, snapB, snapA)
	return b
}

func (b *box) started(role string) bool {
	return strings.Contains(" "+b.roles()+" ", " "+role+" ")
}

func inPlace() RestoreOptions { return RestoreOptions{App: "blog"} }

func TestACleanRestore(t *testing.T) {
	b := restoreBox(t)
	var asked RestoreAsk
	o := inPlace()
	o.Confirm = func(a RestoreAsk) bool {
		// Asked once the snapshot is known, and before anything else.
		if got := b.roles(); got != "plan history" {
			t.Errorf("units started before the question: %s", got)
		}
		asked = a
		return true
	}
	rep, err := Restore(context.Background(), b.cfg, b, o)
	if err != nil {
		t.Fatal(err)
	}
	// The app is backed up first; what was fetched is removed first and
	// last; the hand-over sits between the fetch and whatever reads it.
	if got, want := b.roles(), "plan history clean dump upload verify clean unstage size fetch handover install unstage"; got != want {
		t.Fatalf("units, in order: %s\nwant:            %s", got, want)
	}
	shared := filepath.Join(b.root, "blog", "shared")
	if asked.App != "blog" || asked.Snapshot.ID != snapA || asked.Into != shared || !asked.PreBackup {
		t.Errorf("what was asked: %+v", asked)
	}
	if rep.Snapshot.ID != snapA || rep.PreBackup == nil || rep.Into != shared || len(rep.Items) != 2 || !rep.Items[0].OK || !rep.Items[1].OK {
		t.Fatalf("%+v", rep)
	}
	if left, _ := os.ReadDir(b.cfg.RunDir); len(left) != 2 { // the lock and the unit list
		t.Errorf("left in the run dir: %v", left)
	}
}

// §1.3 of the plan, as a test. The unit with the credential has no
// capability and writes one empty directory; the unit that is root can
// chown and can read closed directories, has no network, and sees that
// directory and nothing else;
// the unit that parses the snapshot's bytes and writes the app's data is
// the app's own user, with no network and no credential.
func TestWhatEachRestoreUnitIsGiven(t *testing.T) {
	b := restoreBox(t)
	o := inPlace()
	o.NoPreBackup = true
	if _, err := Restore(context.Background(), b.cfg, b, o); err != nil {
		t.Fatal(err)
	}
	restaging := filepath.Join(b.cfg.StateDir, "restore")

	fetch := b.spec("fetch")
	if fetch.User != backupUser || fetch.AsRoot || len(fetch.Capabilities) != 0 || fetch.SameUIDNamespaces || !fetch.Network || fetch.EnvironmentFile != b.cfg.EnvFile {
		t.Errorf("fetch: %+v", fetch)
	}
	// A user namespace would turn every chown restic tries into an error
	// it does not overlook, and a capability is what would let a
	// snapshot's owners and modes land as they are.
	argv := strings.Join(fetch.Argv, " ")
	if !strings.Contains(argv, " restore ") || !strings.Contains(argv, snapA+":/backup/blog") || !strings.Contains(argv, "--json") {
		t.Errorf("fetch argv: %s", argv)
	}
	if strings.Contains(argv, "--include") || strings.Contains(argv, "latest") {
		t.Errorf("fetch selects by pattern, which matches nothing with exit 0, or by 'latest': %s", argv)
	}
	writable := 0
	for _, bind := range fetch.Binds {
		if bind.Writable {
			writable++
			if !strings.HasPrefix(bind.Source, restaging+"/") {
				t.Errorf("the unit with the credential can write %s", bind.Source)
			}
		}
		if strings.HasPrefix(bind.Source, b.root) || strings.HasPrefix(bind.Source, b.cfg.RunDir) {
			t.Errorf("the unit with the credential is shown the app's data: %+v", bind)
		}
	}
	if writable != 1 {
		t.Errorf("fetch has %d writable binds, want the restore staging dir alone", writable)
	}

	hand := b.spec("handover")
	if !hand.AsRoot || hand.User != "" || hand.Network || hand.EnvironmentFile != "" || len(hand.Capabilities) != 2 || hand.Capabilities[0] != unit.CapChown || hand.Capabilities[1] != unit.CapDACReadSearch {
		t.Errorf("handover: %+v", hand)
	}
	if len(hand.Binds) != 1 || !hand.Binds[0].Writable || !strings.HasPrefix(hand.Binds[0].Source, restaging+"/") {
		t.Errorf("the root unit sees more than what was fetched: %+v", hand.Binds)
	}
	// By name, resolved on this box: what makes a restore independent of
	// the uid the snapshot was made under.
	if got := strings.Join(hand.Argv, " "); !strings.Contains(got, dataUser) || strings.ContainsAny(got, "0123456789") {
		t.Errorf("handover argv names the owner by number, or not at all: %s", got)
	}

	inst := b.spec("install")
	if inst.User != dataUser || inst.AsRoot || !inst.SameUIDNamespaces || inst.Network || inst.EnvironmentFile != "" || len(inst.Capabilities) != 0 {
		t.Errorf("install: %+v", inst)
	}
	shared := filepath.Join(b.root, "blog", "shared")
	var target *unit.Bind
	for i, bind := range inst.Binds {
		switch {
		case strings.HasPrefix(bind.Source, restaging+"/"):
			// Writable: the fetched tree is scratch, and a copy closed to
			// its owner is opened to be read.
			if strings.HasPrefix(bind.Source, b.root) {
				t.Errorf("install is shown the live data as what was fetched: %+v", bind)
			}
		case bind.Writable:
			target = &inst.Binds[i]
		}
	}
	// By a mount point of the run's own, made from the directory that was
	// opened: never by a name the app, or the server, can re-aim.
	if target == nil || target.Source == shared || !strings.HasPrefix(target.Source, b.cfg.RunDir+"/") || b.mounted[target.Source] != shared {
		t.Fatalf("install's target: %+v (mounted: %v)", target, b.mounted)
	}
	for _, s := range b.specs {
		if strings.Contains(strings.Join(s.Environment, " "), "the-secret") {
			t.Errorf("%s: the engine read the credential file", s.Name)
		}
	}
}

// restic restores nothing with exit 0 when what it was asked for
// matches nothing: exit 0 is not a restore.
func TestAFetchThatFetchedNothingIsNotARestore(t *testing.T) {
	for name, summary := range map[string]string{
		"nothing restored":    `{"message_type":"summary","total_files":0,"files_restored":0}`,
		"fewer than it holds": `{"message_type":"summary","total_files":4,"files_restored":3}`,
		"no summary":          ``,
		"not a summary":       `{"message_type":"status","files_restored":4,"total_files":4}`,
	} {
		t.Run(name, func(t *testing.T) {
			b := restoreBox(t)
			b.fetch = summary
			o := inPlace()
			o.NoPreBackup = true
			rep, err := Restore(context.Background(), b.cfg, b, o)
			if err == nil {
				t.Fatalf("restic exited 0 having said %q, and the restore is reported as done: %+v, %v", summary, rep, err)
			}
			if b.started("install") || b.started("handover") {
				t.Errorf("went on after a fetch that fetched nothing: %s", b.roles())
			}
			if got := b.roles(); !strings.HasSuffix(got, " unstage") {
				t.Errorf("what was fetched was not removed: %s", got)
			}
		})
	}
}

func TestHowAFetchEnds(t *testing.T) {
	for exit, want := range map[int]string{
		10: "no repository",
		11: "locked",
		12: "password is wrong",
		1:  "out of room", // of a restore, 1 is first of all this fetch's own
		3:  "exit 3",
	} {
		t.Run(fmt.Sprint(exit), func(t *testing.T) {
			b := restoreBox(t)
			b.outcome["fetch"] = unit.Outcome{Result: "exit-code", ExitStatus: exit}
			o := inPlace()
			o.NoPreBackup = true
			_, err := Restore(context.Background(), b.cfg, b, o)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("fetch exit %d: %v, want %q", exit, err, want)
			}
			if b.started("handover") || b.started("install") {
				t.Errorf("went on after a failed fetch: %s", b.roles())
			}
			if got := b.roles(); !strings.HasSuffix(got, " unstage") {
				t.Errorf("what was fetched was not removed: %s", got)
			}
		})
	}
}

// Plaintext never outlives a restore: not one that was interrupted
// either, whose context is already cancelled when the cleaning starts.
func TestWhatWasFetchedIsRemovedWhenARestoreIsInterrupted(t *testing.T) {
	b := restoreBox(t)
	ctx, cancel := context.WithCancel(context.Background())
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_handover_") {
			cancel()
		}
	}
	o := inPlace()
	o.NoPreBackup = true
	_, err := Restore(ctx, b.cfg, b, o)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	if b.started("install") {
		t.Errorf("installed after the interrupt: %s", b.roles())
	}
	if got := b.roles(); !strings.HasSuffix(got, " unstage") {
		t.Errorf("what was fetched was not removed: %s", got)
	}
}

func TestNothingIsStartedForAnAppOrASnapshotThatIsNotOne(t *testing.T) {
	for _, o := range []RestoreOptions{
		{App: ""}, {App: "Blog"}, {App: "../shop"}, {App: "blog shop"}, {App: "${RESTIC_PASSWORD}"},
		{App: "blog", Snapshot: "latest"}, {App: "blog", Snapshot: "${RESTIC_PASSWORD}"},
		{App: "blog", Snapshot: "--target=/etc"}, {App: "blog", Snapshot: snapA[:8] + " /etc"}, {App: "blog", Snapshot: strings.ToUpper(snapA)},
		{App: "blog", To: "relative/dir"},
	} {
		b := restoreBox(t)
		if _, err := Restore(context.Background(), b.cfg, b, o); err == nil {
			t.Errorf("%+v: %v", o, err)
		}
		if len(b.specs) != 0 {
			t.Errorf("%+v: units were started: %s", o, b.roles())
		}
	}
}

func TestASnapshotThatIsNotTheAppsIsRefusedBeforeAnythingIsFetched(t *testing.T) {
	const other = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	b := restoreBox(t)
	o := inPlace()
	o.Snapshot = other
	_, err := Restore(context.Background(), b.cfg, b, o)
	if err == nil || !strings.Contains(err.Error(), "not a snapshot of blog") {
		t.Fatalf("%v", err)
	}
	if got := b.roles(); got != "plan history" {
		t.Errorf("units started: %s", got)
	}

	// An app nothing declares, and an app with no snapshot.
	b = restoreBox(t)
	if _, err := Restore(context.Background(), b.cfg, b, RestoreOptions{App: "ghost"}); err == nil || !strings.Contains(err.Error(), "ghost") || b.started("fetch") {
		t.Errorf("an undeclared app: %v (%s)", err, b.roles())
	}
	b = restoreBox(t)
	b.history = "[]"
	if _, err := Restore(context.Background(), b.cfg, b, inPlace()); err == nil || !strings.Contains(err.Error(), "no snapshot") || b.started("fetch") || b.started("upload") {
		t.Errorf("no snapshot: %v (%s)", err, b.roles())
	}
	// A shortened id that is the start of one of the app's is that one.
	b = restoreBox(t)
	o = inPlace()
	o.Snapshot, o.NoPreBackup = snapB[:8], true
	rep, err := Restore(context.Background(), b.cfg, b, o)
	if err != nil || rep.Snapshot.ID != snapB || !strings.Contains(strings.Join(b.spec("fetch").Argv, " "), snapB+":") {
		t.Errorf("a short id: %+v, %v", rep, err)
	}
}

func TestNothingHappensBeforeTheAnswerAndNothingAfterANo(t *testing.T) {
	b := restoreBox(t)
	o := inPlace()
	o.Confirm = func(RestoreAsk) bool { return false }
	if _, err := Restore(context.Background(), b.cfg, b, o); !errors.Is(err, ErrDeclined) {
		t.Fatalf("%v", err)
	}
	if got := b.roles(); got != "plan history" {
		t.Errorf("units started around a no: %s", got)
	}
}

// The backup made first is what lets a restore be undone; one that did
// not end ok stops a restore that was not told to go without.
func TestABackupFirstThatFailsStopsTheRestore(t *testing.T) {
	b := restoreBox(t)
	b.uploadExit = map[string]int{"blog": 12}
	_, err := Restore(context.Background(), b.cfg, b, inPlace())
	if err == nil || !strings.Contains(err.Error(), "--no-pre-backup") || !strings.Contains(err.Error(), "password is wrong") {
		t.Fatalf("%v", err)
	}
	if b.started("fetch") || b.started("install") {
		t.Errorf("restored over data that could not be backed up first: %s", b.roles())
	}

	b = restoreBox(t)
	b.uploadExit = map[string]int{"blog": 12}
	o := inPlace()
	o.NoPreBackup = true
	if _, err := Restore(context.Background(), b.cfg, b, o); err != nil {
		t.Fatal(err)
	}
	if b.started("upload") || b.started("dump") {
		t.Errorf("told to skip it, a backup was made first all the same: %s", b.roles())
	}

	// A rebuilt box: no data dir is nothing to back up and nothing to
	// overwrite, not a failed backup.
	b = restoreBox(t)
	must(t, os.RemoveAll(filepath.Join(b.root, "blog")))
	rep, err := Restore(context.Background(), b.cfg, b, inPlace())
	if err != nil || rep.PreBackup != nil || b.started("upload") || !b.started("install") {
		t.Errorf("with no data dir: %+v, %v (%s)", rep, err, b.roles())
	}
}

// What the install unit says came through the snapshot's bytes. It is
// believed where it is an answer in words this side knows, about a plan
// that is a valid declaration.
func TestAnInstallUnitThatLiesIsNotBelieved(t *testing.T) {
	for name, answer := range map[string]string{
		"nothing":               ``,
		"not json":              `restored!`,
		"an unknown field":      `{"plan":{"files":["uploads"]},"items":[{"kind":"files","path":"uploads","class":"ok"}],"also":1}`,
		"an unknown class":      `{"plan":{"files":["uploads"]},"items":[{"kind":"files","path":"uploads","class":"fine"}]}`,
		"an item not in a plan": `{"plan":{"files":["uploads"]},"items":[{"kind":"files","path":"elsewhere","class":"ok"}]}`,
		"a plan item unsaid":    `{"plan":{"sqlite":["app.db"],"files":["uploads"]},"items":[{"kind":"files","path":"uploads","class":"ok"}]}`,
		"a plan that leaves":    `{"plan":{"files":["../../shop/shared"]},"items":[{"kind":"files","path":"../../shop/shared","class":"ok"}]}`,
		"an absolute plan":      `{"plan":{"files":["/etc"]},"items":[{"kind":"files","path":"/etc","class":"ok"}]}`,
		"an empty plan":         `{"plan":{},"items":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			b := restoreBox(t)
			b.install = answer
			o := inPlace()
			o.NoPreBackup = true
			rep, err := Restore(context.Background(), b.cfg, b, o)
			if err == nil {
				t.Fatalf("believed: %+v", rep)
			}
			if got := b.roles(); !strings.HasSuffix(got, " unstage") {
				t.Errorf("what was fetched was not removed: %s", got)
			}
		})
	}

	// Its words are cleaned before they are kept or printed.
	b := restoreBox(t)
	b.install = `{"plan":{"sqlite":["app.db"]},"items":[{"kind":"sqlite","path":"app.db","class":"damaged","detail":"row 1 missing\u001b[2J` + strings.Repeat("x", 500) + `"}]}`
	o := inPlace()
	o.NoPreBackup = true
	_, err := Restore(context.Background(), b.cfg, b, o)
	if err == nil || !strings.Contains(err.Error(), "app.db") || !strings.Contains(err.Error(), "damaged") || strings.ContainsRune(err.Error(), 0x1b) || len(err.Error()) > 700 {
		t.Fatalf("%q", err)
	}
}

func TestARestoreIsNotAimedByALink(t *testing.T) {
	b := restoreBox(t)
	sibling := filepath.Join(b.root, "shop", "shared")
	must(t, os.MkdirAll(sibling, 0o755))
	shared := filepath.Join(b.root, "blog", "shared")
	must(t, os.RemoveAll(shared))
	must(t, os.Symlink(sibling, shared))
	o := inPlace()
	o.NoPreBackup = true
	_, err := Restore(context.Background(), b.cfg, b, o)
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("%v", err)
	}
	if b.started("fetch") || b.started("install") {
		t.Errorf("units started for a shared dir that is a link: %s", b.roles())
	}

	// Re-aimed between the look and the unit's start: the unit is shown
	// what was opened.
	b = restoreBox(t)
	shared = filepath.Join(b.root, "blog", "shared")
	must(t, os.MkdirAll(sibling, 0o755))
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_install_") {
			must(t, os.Rename(shared, shared+".away"))
			must(t, os.Symlink(sibling, shared))
		}
	}
	if _, err := Restore(context.Background(), b.cfg, b, o); err != nil {
		t.Fatal(err)
	}
	for _, bind := range b.spec("install").Binds {
		if bind.Dest == "/target" && (bind.Source == shared || b.mounted[bind.Source] != shared) {
			t.Errorf("install's target is bound by name: %+v (mounted from %q)", bind, b.mounted[bind.Source])
		}
	}
}

func TestToADirectory(t *testing.T) {
	b := restoreBox(t)
	to := filepath.Join(t.TempDir(), "up $RESTIC_PASSWORD %h")
	asked := false
	o := RestoreOptions{App: "blog", To: to, Confirm: func(RestoreAsk) bool { asked = true; return true }}
	rep, err := Restore(context.Background(), b.cfg, b, o)
	if err != nil || rep.Into != to || rep.PreBackup != nil {
		t.Fatalf("%+v, %v", rep, err)
	}
	if asked || b.started("upload") {
		t.Errorf("asked=%v, units: %s — nothing is overwritten, so nothing is asked and nothing backed up", asked, b.roles())
	}
	st, err := os.Stat(to)
	if err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		t.Errorf("the directory: %v, %v", st, err)
	}
	// Its parent may be anyone's, so it is bound as an app's data is: by
	// a mount point of the run's own, made from the directory that was
	// made — never by the name the operator gave.
	for _, bind := range b.spec("extract").Binds {
		if bind.Dest == "/target" && (bind.Source == to || !strings.HasPrefix(bind.Source, b.cfg.RunDir+"/") || b.mounted[bind.Source] != to) {
			t.Errorf("--to is bound by name: %+v (mounted from %q)", bind, b.mounted[bind.Source])
		}
	}
	for _, s := range b.specs {
		for _, arg := range s.Argv {
			if strings.Contains(arg, "RESTIC_PASSWORD") || strings.Contains(arg, to) {
				t.Errorf("%s: the operator's path is on a command line: %q", s.Name, s.Argv)
			}
		}
	}
	// The live data is not in any unit's view.
	for _, s := range b.specs[2:] {
		for _, bind := range s.Binds {
			if strings.HasPrefix(bind.Source, b.root) || strings.HasPrefix(b.mounted[bind.Source], b.root) {
				t.Errorf("%s is shown the live data: %+v", s.Name, bind)
			}
		}
	}

	b = restoreBox(t)
	if _, err := Restore(context.Background(), b.cfg, b, RestoreOptions{App: "blog", To: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "exists") || b.started("fetch") {
		t.Errorf("a directory that exists: %v (%s)", err, b.roles())
	}
}

func TestOneRestoreOrRunAtATime(t *testing.T) {
	b := restoreBox(t)
	must(t, os.MkdirAll(b.cfg.RunDir, 0o755))
	unlock, err := lock(filepath.Join(b.cfg.RunDir, "lock"))
	must(t, err)
	defer unlock()
	if _, err := Restore(context.Background(), b.cfg, b, inPlace()); !errors.Is(err, ErrBusy) || len(b.specs) != 0 {
		t.Fatalf("a restore during a run: %v (%s)", err, b.roles())
	}
	if _, err := Drill(context.Background(), b.cfg, b); !errors.Is(err, ErrBusy) || len(b.specs) != 0 {
		t.Fatalf("a drill during a run: %v (%s)", err, b.roles())
	}
}

func TestADrillProvesAndInstallsNothing(t *testing.T) {
	b := restoreBox(t)
	st, err := Drill(context.Background(), b.cfg, b)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := b.roles(), "plan history unstage size fetch handover check unstage"; got != want {
		t.Fatalf("units, in order: %s\nwant:            %s", got, want)
	}
	for _, s := range b.specs {
		for _, bind := range s.Binds {
			if strings.HasPrefix(bind.Source, b.root) || strings.HasPrefix(b.mounted[bind.Source], b.root) {
				t.Errorf("%s: a drill is shown the live data: %+v", s.Name, bind)
			}
		}
	}
	app := st.Apps["blog"]
	if app == nil || app.RestoreProven == nil || app.RestoreProven.Snapshot.ID != snapA || app.RestoreDrill != nil {
		t.Fatalf("%+v", app)
	}
	again, err := record.Read(filepath.Join(b.cfg.StateDir, "status.json"))
	if err != nil || again.Apps["blog"].RestoreProven == nil {
		t.Fatalf("the record on disk: %+v, %v", again, err)
	}
}

func TestADrillThatProvesNothingKeepsWhatWasProven(t *testing.T) {
	made := &record.Snapshot{ID: snapB, Time: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	proven := &record.Drill{Snapshot: *made, Time: made.Time.Add(time.Hour)}
	for name, breakIt := range map[string]func(*box){
		"a damaged copy": func(b *box) {
			b.install = `{"plan":{"sqlite":["app.db"]},"items":[{"kind":"sqlite","path":"app.db","class":"damaged","detail":"row 1 missing from index i"}]}`
		},
		"nothing fetched": func(b *box) { b.fetch = `{"message_type":"summary","total_files":0,"files_restored":0}` },
		"wrong password":  func(b *box) { b.outcome["fetch"] = unit.Outcome{Result: "exit-code", ExitStatus: 12} },
		"a lying unit":    func(b *box) { b.install = `{"plan":{"files":["uploads"]},"items":[]}` },
	} {
		t.Run(name, func(t *testing.T) {
			b := restoreBox(t)
			must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
			must(t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{Apps: map[string]*record.App{
				"blog": {Class: record.OK, LastOK: made, LastSnapshot: made, RestoreProven: proven},
			}}))
			breakIt(b)
			st, err := Drill(context.Background(), b.cfg, b)
			if err != nil {
				t.Fatal(err)
			}
			app := st.Apps["blog"]
			if app == nil || app.RestoreProven == nil || app.RestoreProven.Snapshot.ID != snapB {
				t.Fatalf("what was proven is gone, or the snapshot that failed is called proven: %+v", app)
			}
			if app.RestoreDrill == nil || app.RestoreDrill.Snapshot.ID != snapA || app.RestoreDrill.Detail == "" {
				t.Fatalf("the record does not say which snapshot failed its drill, and why: %+v", app.RestoreDrill)
			}
			// What the last backup run found stays what it found.
			if app.Class != record.OK || app.LastOK == nil {
				t.Errorf("a drill rewrote the backup's own result: %+v", app)
			}
			if got := b.roles(); !strings.HasSuffix(got, " unstage") {
				t.Errorf("what was fetched was not removed: %s", got)
			}
		})
	}
}

func TestTheFirstBackupProvesItsOwnRestoreAndLaterRunsKeepIt(t *testing.T) {
	b := restoreBox(t)
	st, err := Run(context.Background(), b.cfg, b)
	if err != nil {
		t.Fatal(err)
	}
	if !b.started("fetch") || !b.started("check") || b.started("install") {
		t.Fatalf("a first backup: %s", b.roles())
	}
	if app := st.Apps["blog"]; app.RestoreProven == nil || app.RestoreProven.Snapshot.ID != snapA {
		t.Fatalf("%+v", app)
	}
	b.specs = nil
	if st, err = Run(context.Background(), b.cfg, b); err != nil {
		t.Fatal(err)
	}
	if b.started("fetch") {
		t.Errorf("an hourly run drilled again: %s", b.roles())
	}
	if app := st.Apps["blog"]; app.RestoreProven == nil || app.RestoreProven.Snapshot.ID != snapA {
		t.Fatalf("a backup run unproved the restore: %+v", app)
	}
}

// What a restore backs up first is what it is about to restore over —
// the damage, as often as not. It is there to be restored by name; it is
// never what "the newest" means, to a second try at a restore that
// failed or to a drill.
func TestWhatARestoreBackedUpFirstIsNeverTheNewest(t *testing.T) {
	const pre = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	history := fmt.Sprintf(`[{"id":%q,"time":"2026-09-01T00:00:00Z","tags":["hotserve","app:blog"]},{"id":%q,"time":"2026-09-02T00:00:00Z","tags":["hotserve","app:blog","pre-restore"]}]`, snapB, pre)

	b := restoreBox(t)
	b.history = history
	rep, err := Restore(context.Background(), b.cfg, b, inPlace())
	if err != nil || rep.Snapshot.ID != snapB {
		t.Fatalf("the default: %+v, %v", rep, err)
	}
	// The backup this restore makes first says what it is; a run's does not.
	if argv := strings.Join(b.spec("upload").Argv, " "); !strings.Contains(argv, "--tag pre-restore") {
		t.Errorf("the pre-restore upload: %s", argv)
	}
	b = restoreBox(t)
	if _, err := Run(context.Background(), b.cfg, b); err != nil {
		t.Fatal(err)
	}
	if argv := strings.Join(b.spec("upload").Argv, " "); strings.Contains(argv, "pre-restore") {
		t.Errorf("a run's upload: %s", argv)
	}

	// By name it is restored like any other.
	b = restoreBox(t)
	b.history = history
	o := inPlace()
	o.Snapshot, o.NoPreBackup = pre[:8], true
	if rep, err := Restore(context.Background(), b.cfg, b, o); err != nil || rep.Snapshot.ID != pre {
		t.Fatalf("by name: %+v, %v", rep, err)
	}

	b = restoreBox(t)
	b.history = history
	st, err := Drill(context.Background(), b.cfg, b)
	if err != nil || st.Apps["blog"].RestoreProven == nil || st.Apps["blog"].RestoreProven.Snapshot.ID != snapB {
		t.Fatalf("a drill proved %+v, %v", st.Apps["blog"], err)
	}
}

// The backup made first is named also when the restore then fails: it is
// what undoes whatever was done.
func TestAFailedRestoreStillNamesTheBackupItMadeFirst(t *testing.T) {
	b := restoreBox(t)
	b.outcome["fetch"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	rep, err := Restore(context.Background(), b.cfg, b, inPlace())
	if err == nil || rep == nil || rep.PreBackup == nil || rep.PreBackup.ID != snapA || len(rep.Items) != 0 {
		t.Fatalf("%+v, %v", rep, err)
	}
}

func TestWhatAFailedInstallLeftIsSaidForWhatItIs(t *testing.T) {
	for answer, want := range map[string]string{
		// Refused at the checks: nothing was begun.
		`{"plan":{"sqlite":["app.db"],"files":["uploads"]},"items":[{"kind":"sqlite","path":"app.db","class":"damaged","detail":"x"},{"kind":"files","path":"uploads","class":"held back"}]}`: "nothing was changed",
		// Failed half way.
		`{"plan":{"sqlite":["app.db"],"files":["uploads"]},"changed":true,"items":[{"kind":"sqlite","path":"app.db","class":"ok"},{"kind":"files","path":"uploads","class":"failed","detail":"no space left on device"}]}`: "partly restored",
		// A unit that holds everything back and blames nothing restored nothing.
		`{"plan":{"files":["uploads"]},"items":[{"kind":"files","path":"uploads","class":"held back"}]}`: "held every item back",
	} {
		b := restoreBox(t)
		b.install = answer
		o := inPlace()
		o.NoPreBackup = true
		rep, err := Restore(context.Background(), b.cfg, b, o)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s:\n%v, want %q", answer, err, want)
		}
		if strings.Contains(want, "partly") && (rep == nil || len(rep.Items) != 2 || !rep.Items[0].OK || strings.Contains(err.Error(), "nothing was changed")) {
			t.Errorf("a restore that began: %+v, %v", rep, err)
		}
	}
}

func TestADrillOfOneAppFailingIsThatAppsOwnAndAnInterruptIsNoVerdict(t *testing.T) {
	b := restoreBox(t)
	b.plan = fmt.Sprintf(`{"root":%q,"apps":{"blog":{"files":["uploads"]},"shop":{"files":["uploads"]}}}`, b.root)
	b.install = `{"plan":{"files":["uploads"]},"items":[{"kind":"files","path":"uploads","class":"ok"}]}`
	failBlog := true
	b.before = func(s unit.Spec) {
		// Exit 1 of a fetch is a full disk before it is anything else:
		// the app after it is drilled all the same.
		if strings.Contains(s.Name, "_fetch_blog_") && failBlog {
			b.outcome["fetch"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
		} else {
			delete(b.outcome, "fetch")
		}
	}
	st, err := Drill(context.Background(), b.cfg, b)
	if err != nil || st.Apps["blog"].RestoreDrill == nil || st.Apps["shop"] == nil || st.Apps["shop"].RestoreProven == nil {
		t.Fatalf("blog %+v shop %+v, %v", st.Apps["blog"], st.Apps["shop"], err)
	}

	// Interrupted: nothing was found out, and nothing is written over
	// what the last drill found.
	b = restoreBox(t)
	ctx, cancel := context.WithCancel(context.Background())
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_fetch_") {
			cancel()
		}
	}
	st, err = Drill(ctx, b.cfg, b)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	if app := st.Apps["blog"]; app != nil && app.RestoreDrill != nil {
		t.Fatalf("an interrupt is recorded as a drill that failed: %+v", app.RestoreDrill)
	}
	// And an app that has left the plan has nothing left to prove.
	b = restoreBox(t)
	must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
	must(t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{Apps: map[string]*record.App{
		"gone": {Class: record.OK, RestoreDrill: &record.Drill{Detail: "could not be asked"}},
	}}))
	if st, err = Drill(context.Background(), b.cfg, b); err != nil || st.Apps["gone"].RestoreDrill != nil {
		t.Fatalf("%+v, %v", st.Apps["gone"], err)
	}
}

// A fetch is the whole app in plaintext under the state dir. One that
// cannot fit is refused before it is begun, in a restore and in a drill;
// and an answer about no snapshot — restic's, with exit 0, for an id it
// does not hold — is not an answer.
func TestAFetchThatCannotFitIsNotBegun(t *testing.T) {
	for name, tc := range map[string]struct {
		size string
		free uint64
		want string
	}{
		"larger than what is free": {`{"total_size":83886080,"total_file_count":9,"snapshots_count":1}`, 8 << 20, "no room"},
		"no snapshot was counted":  {`{"total_size":0,"snapshots_count":0}`, 1 << 30, "did not say how large"},
		"nothing was said":         {``, 1 << 30, "did not say how large"},
		"no size in it":            {`{"snapshots_count":1}`, 1 << 30, "did not say how large"},
	} {
		t.Run(name, func(t *testing.T) {
			b := restoreBox(t)
			b.size, b.free = tc.size, tc.free
			o := inPlace()
			o.NoPreBackup = true
			_, err := Restore(context.Background(), b.cfg, b, o)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%v, want %q", err, tc.want)
			}
			if name == "larger than what is free" && (!strings.Contains(err.Error(), "80 MiB") || !strings.Contains(err.Error(), "8 MiB") || !strings.Contains(err.Error(), b.cfg.StateDir)) {
				t.Errorf("what is needed, what there is and where: %v", err)
			}
			if b.started("fetch") || b.started("install") {
				t.Errorf("fetched all the same: %s", b.roles())
			}
		})
	}
	// On one filesystem an install writes the app a second time while the
	// fetch is still there: twice the snapshot, exactly, is room; less is
	// not, though the fetch alone would fit — and a drill, which installs
	// nothing, needs the fetch alone.
	b := restoreBox(t)
	b.size, b.free, b.oneDisk = `{"total_size":41943040,"snapshots_count":1}`, 41943040+1<<20, true
	o := inPlace()
	o.NoPreBackup = true
	if _, err := Restore(context.Background(), b.cfg, b, o); err == nil || !strings.Contains(err.Error(), "twice") || b.started("fetch") {
		t.Fatalf("fits once, not twice: %v (%s)", err, b.roles())
	}
	b = restoreBox(t)
	b.size, b.free, b.oneDisk = `{"total_size":41943040,"snapshots_count":1}`, 41943040+1<<20, true
	if st, err := Drill(context.Background(), b.cfg, b); err != nil || st.Apps["blog"].RestoreProven == nil {
		t.Fatalf("a drill where the fetch alone fits: %+v, %v", st.Apps["blog"], err)
	}
	b = restoreBox(t)
	b.size, b.free, b.oneDisk = `{"total_size":41943040,"snapshots_count":1}`, 2*41943040, true
	if _, err := Restore(context.Background(), b.cfg, b, o); err != nil {
		t.Fatalf("exactly twice: %v", err)
	}
	// On two filesystems each needs the snapshot; exactly what is free is
	// room. And the unit that asks holds the credential and sees nothing
	// of the app.
	b = restoreBox(t)
	b.size, b.free = `{"total_size":4096,"snapshots_count":1}`, 4096
	if _, err := Restore(context.Background(), b.cfg, b, o); err != nil {
		t.Fatal(err)
	}
	size := b.spec("size")
	if size.User != backupUser || len(size.Capabilities) != 0 || len(size.Binds) != 0 || !strings.Contains(strings.Join(size.Argv, " "), "--mode restore-size "+snapA) {
		t.Errorf("size: %+v", size)
	}

	// In a drill it is that app's own: the record says why, what was
	// proven stays, and the next app is drilled.
	b = restoreBox(t)
	b.plan = fmt.Sprintf(`{"root":%q,"apps":{"blog":{"files":["uploads"]},"shop":{"files":["uploads"]}}}`, b.root)
	b.install = `{"plan":{"files":["uploads"]},"items":[{"kind":"files","path":"uploads","class":"ok"}]}`
	b.before = func(s unit.Spec) {
		b.free = 1 << 30
		if strings.Contains(s.Name, "_size_blog_") {
			b.free = 1
		}
	}
	st, err := Drill(context.Background(), b.cfg, b)
	if err != nil || st.Apps["blog"].RestoreDrill == nil || !strings.Contains(st.Apps["blog"].RestoreDrill.Detail, "no room") || st.Apps["shop"].RestoreProven == nil {
		t.Fatalf("blog %+v shop %+v, %v", st.Apps["blog"], st.Apps["shop"], err)
	}
}

// The question promises a backup first only where one will be made: not
// on a rebuilt box with nothing there, and not when asked to skip it.
func TestTheQuestionSaysWhetherABackupComesFirst(t *testing.T) {
	ask := func(b *box, o RestoreOptions) RestoreAsk {
		var got RestoreAsk
		o.Confirm = func(a RestoreAsk) bool { got = a; return false }
		if _, err := Restore(context.Background(), b.cfg, b, o); !errors.Is(err, ErrDeclined) {
			t.Fatalf("%v", err)
		}
		return got
	}
	b := restoreBox(t)
	must(t, os.RemoveAll(filepath.Join(b.root, "blog")))
	if a := ask(b, inPlace()); a.PreBackup {
		t.Errorf("with no data dir the question promises a backup first: %+v", a)
	}
	b = restoreBox(t)
	o := inPlace()
	o.NoPreBackup = true
	if a := ask(b, o); a.PreBackup {
		t.Errorf("asked to skip it, the question promises a backup first: %+v", a)
	}
}

// What a restore's sweep could not empty of another app is said.
func TestARestoreSaysWhatItCouldNotSweep(t *testing.T) {
	b := restoreBox(t)
	left := filepath.Join(b.cfg.StateDir, "restore", "shop")
	must(t, os.MkdirAll(left, 0o700))
	must(t, os.WriteFile(filepath.Join(left, "plan.json"), []byte("{}"), 0o600))
	b.failUnstage = "shop"
	o := inPlace()
	o.NoPreBackup = true
	rep, err := Restore(context.Background(), b.cfg, b, o)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rep.Warning, "shop") || !strings.Contains(rep.Warning, left) {
		t.Fatalf("the warning: %q", rep.Warning)
	}
}

// A drill inside a run is for an app's first good backup, once: an app
// whose drill failed is the weekly drill's to try again, not every hour's
// to fetch in full. And a drill's repository-wide failure stops the run
// as an upload's would.
func TestTheRunsOwnDrillHappensOnceAndItsWideFailureStopsTheRun(t *testing.T) {
	b := restoreBox(t)
	must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
	must(t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{Apps: map[string]*record.App{
		"blog": {Class: record.OK, RestoreDrill: &record.Drill{Detail: "damaged last week"}},
	}}))
	if _, err := Run(context.Background(), b.cfg, b); err != nil {
		t.Fatal(err)
	}
	if b.started("fetch") {
		t.Errorf("an app whose drill failed was fetched again by an hourly run: %s", b.roles())
	}

	b = restoreBox(t)
	b.plan = fmt.Sprintf(`{"root":%q,"apps":{"blog":{"sqlite":["app.db"],"files":["uploads"]},"shop":{"sqlite":["app.db"],"files":["uploads"]}}}`, b.root)
	must(t, os.MkdirAll(filepath.Join(b.root, "shop", "shared", "uploads"), 0o755))
	b.outcome["fetch"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
	st, err := Run(context.Background(), b.cfg, b)
	if err != nil {
		t.Fatal(err)
	}
	if st.Apps["blog"].RestoreDrill == nil || st.Apps["shop"].Class != record.NotAttempted {
		t.Fatalf("blog %+v shop %+v", st.Apps["blog"], st.Apps["shop"])
	}
}

// The backup a restore makes first is written under the restore's own
// time, with every other app as the last run left it.
func TestThePreBackupIsRecordedUnderTheRestoresOwnTime(t *testing.T) {
	b := restoreBox(t)
	must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
	old := time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC)
	must(t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{Started: old, Finished: old, Apps: map[string]*record.App{
		"shop": {Class: record.Incomplete, Detail: "as the last run left it"},
	}}))
	before := time.Now().UTC().Add(-time.Second)
	if _, err := Restore(context.Background(), b.cfg, b, inPlace()); err != nil {
		t.Fatal(err)
	}
	st, err := record.Read(filepath.Join(b.cfg.StateDir, "status.json"))
	must(t, err)
	if st.Started.Before(before) || st.Finished.Before(st.Started) {
		t.Errorf("the record's time is the last run's: %v–%v", st.Started, st.Finished)
	}
	if st.Apps["blog"] == nil || st.Apps["blog"].Class != record.OK || st.Apps["shop"] == nil || st.Apps["shop"].Class != record.Incomplete {
		t.Errorf("the apps: blog %+v shop %+v", st.Apps["blog"], st.Apps["shop"])
	}
}

// A run says how large its first drill is before fetching, and leaves
// one above the limit to the drill: recorded as not proven, and why.
func TestARunLeavesALargeFirstDrillToTheDrill(t *testing.T) {
	b := restoreBox(t)
	b.size = fmt.Sprintf(`{"total_size":%d,"snapshots_count":1}`, firstDrillLimit+1)
	st, err := Run(context.Background(), b.cfg, b)
	if err != nil {
		t.Fatal(err)
	}
	app := st.Apps["blog"]
	if b.started("fetch") || app.RestoreProven != nil || app.RestoreDrill == nil || !strings.Contains(app.RestoreDrill.Detail, "hotserve-backup drill") || !strings.Contains(app.RestoreDrill.Detail, "1025 MiB") {
		t.Fatalf("%s; %+v", b.roles(), app.RestoreDrill)
	}
	// The drill itself has no such limit, and proves it (given the room).
	b.specs, b.free = nil, 4<<30
	if st, err = Drill(context.Background(), b.cfg, b); err != nil || st.Apps["blog"].RestoreProven == nil || st.Apps["blog"].RestoreDrill != nil {
		t.Fatalf("the drill: %+v, %v", st.Apps["blog"], err)
	}
}

// The snapshot a restore takes may be one a run ended incomplete on,
// which no check at install can tell: when the record says a later or
// earlier snapshot is the last ok one, the question and the report say
// so and name it.
func TestARestoreSaysWhenTheSnapshotIsNotTheLastOK(t *testing.T) {
	b := restoreBox(t)
	must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
	must(t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{Apps: map[string]*record.App{
		"blog": {Class: record.Incomplete, LastOK: &record.Snapshot{ID: snapB}, LastSnapshot: &record.Snapshot{ID: snapA}},
	}}))
	var asked RestoreAsk
	o := inPlace()
	o.NoPreBackup = true
	o.Confirm = func(a RestoreAsk) bool { asked = a; return true }
	rep, err := Restore(context.Background(), b.cfg, b, o)
	if err != nil {
		t.Fatal(err)
	}
	if asked.LastOK == nil || asked.LastOK.ID != snapB || rep.LastOK == nil || rep.LastOK.ID != snapB {
		t.Fatalf("asked %+v, report %+v", asked.LastOK, rep.LastOK)
	}
	// Restoring that one, nothing is said.
	o.Snapshot = snapB[:8]
	if rep, err := Restore(context.Background(), b.cfg, b, o); err != nil || rep.LastOK != nil || asked.LastOK != nil {
		t.Fatalf("the last ok one itself: %+v, %v", rep.LastOK, err)
	}
	// And with no record — a rebuilt box — nothing can be said.
	must(t, os.Remove(filepath.Join(b.cfg.StateDir, "status.json")))
	o.Snapshot = ""
	if rep, err := Restore(context.Background(), b.cfg, b, o); err != nil || rep.LastOK != nil {
		t.Fatalf("no record: %+v, %v", rep.LastOK, err)
	}
}
