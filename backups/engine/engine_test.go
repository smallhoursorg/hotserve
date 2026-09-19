package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/smallhoursorg/hotserve/backups/dump"
	"github.com/smallhoursorg/hotserve/backups/record"
	"github.com/smallhoursorg/hotserve/backups/unit"
)

const snapA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// box is a liveswap root with one deployed app, the engine's
// directories, and a scripted Runner standing in for the manager. What
// the real manager does with a Spec is the unit package's integration
// suite; what is tested here is which units a run starts, with what,
// in what order, and what it makes of how they end.
type box struct {
	t       *testing.T
	cfg     Config
	root    string
	plan    string // what the plan unit prints
	specs   []unit.Spec
	stopped []string
	// how each role ends; the default is a clean run of everything
	outcome map[string]unit.Outcome
	err     map[string]error
	dump    func(paths []string) []dump.Result
	summary string
	ls      func(parent string) string
	// before runs as a unit is about to start: the moment the manager
	// would resolve its bind sources.
	before func(unit.Spec)
	// mount points, and the directory each was made from
	mounted    map[string]string
	unmounted  []string
	leftMounts []string
}

var roleRe = regexp.MustCompile(`^hotserve_backup_([a-z]+)[0-9]*_`)

func newBox(t *testing.T) *box {
	t.Helper()
	dir := t.TempDir()
	b := &box{t: t, root: filepath.Join(dir, "liveswap"), outcome: map[string]unit.Outcome{}, err: map[string]error{}}
	b.cfg = Config{
		Caddyfile: "/etc/hotserve/Caddyfile", ConfigDir: "/etc/hotserve",
		EnvFile: filepath.Join(dir, "backup.env"), StateDir: filepath.Join(dir, "state"), RunDir: filepath.Join(dir, "run"),
		Self: "/usr/bin/hotserve-backup", Restic: "/usr/bin/restic",
	}
	must(t, os.WriteFile(b.cfg.EnvFile, []byte("RESTIC_PASSWORD=the-secret\n"), 0o600))
	must(t, os.MkdirAll(filepath.Join(b.root, "blog", "shared", "uploads"), 0o755))
	b.plan = fmt.Sprintf(`{"root":%q,"apps":{"blog":{"sqlite":["app.db"],"files":["uploads"]}}}`, b.root)
	b.dump = func(paths []string) []dump.Result {
		out := make([]dump.Result, len(paths))
		for i, p := range paths {
			out[i] = dump.Result{Path: p, Class: dump.OK, Bytes: 4096}
		}
		return out
	}
	b.summary = `{"message_type":"summary","snapshot_id":"` + snapA + `"}`
	b.ls = func(parent string) string {
		switch parent {
		case "/backup/blog/sqlite":
			return `{"struct_type":"snapshot"}` + "\n" + `{"struct_type":"node","path":"/backup/blog/sqlite/app.db","type":"file","size":4096}`
		case "/backup/blog/files":
			return `{"struct_type":"node","path":"/backup/blog/files/uploads","type":"dir"}`
		}
		return ""
	}
	old, oldMount, oldUnmount, oldUnder := dataOwner, bindMount, unmountDetach, mountsUnder
	dataOwner = func() (int, int, error) { return os.Getuid(), os.Getgid(), nil }
	// mount(2) needs a privilege this lane does not have, and what the
	// kernel does with it is the integration suite's to show. Here it is
	// enough to know what was asked for: which directory, at the moment
	// of asking, onto which mount point.
	b.mounted = map[string]string{}
	bindMount = func(source, target string) error {
		was, err := os.Readlink(source)
		b.mounted[target] = was
		return err
	}
	unmountDetach = func(target string) error { b.unmounted = append(b.unmounted, target); return nil }
	mountsUnder = func(string) ([]string, error) { return b.leftMounts, nil }
	t.Cleanup(func() { dataOwner, bindMount, unmountDetach, mountsUnder = old, oldMount, oldUnmount, oldUnder })
	return b
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (b *box) Stop(name string) error { b.stopped = append(b.stopped, name); return nil }

func (b *box) Run(_ context.Context, s unit.Spec) (unit.Outcome, error) {
	b.specs = append(b.specs, s)
	if b.before != nil {
		b.before(s)
	}
	m := roleRe.FindStringSubmatch(s.Name)
	if m == nil {
		b.t.Fatalf("unit name %q", s.Name)
	}
	role := m[1]
	if err := b.err[role]; err != nil {
		return unit.Outcome{}, err
	}
	write := func(body string) {
		if s.StdoutFile != "" {
			must(b.t, os.WriteFile(s.StdoutFile, []byte(body), 0o600))
		}
	}
	switch role {
	case "plan":
		write(b.plan)
	case "dump":
		var decl struct {
			SQLite []string `json:"sqlite"`
		}
		for _, bind := range s.Binds {
			if bind.Dest == "/plan.json" {
				raw, err := os.ReadFile(bind.Source)
				must(b.t, err)
				must(b.t, json.Unmarshal(raw, &decl))
			}
		}
		raw, _ := json.Marshal(b.dump(decl.SQLite))
		write(string(raw))
	case "upload":
		write(b.summary)
	case "verify":
		write(b.ls(s.Argv[len(s.Argv)-1]))
	}
	if o, ok := b.outcome[role]; ok {
		return o, nil
	}
	return unit.Outcome{Result: "success"}, nil
}

func (b *box) roles() string {
	var r []string
	for _, s := range b.specs {
		r = append(r, roleRe.FindStringSubmatch(s.Name)[1])
	}
	return strings.Join(r, " ")
}

func (b *box) spec(role string) unit.Spec {
	for _, s := range b.specs {
		if roleRe.FindStringSubmatch(s.Name)[1] == role {
			return s
		}
	}
	b.t.Fatalf("no %s unit was started (started: %s)", role, b.roles())
	return unit.Spec{}
}

func TestACleanRun(t *testing.T) {
	b := newBox(t)
	st, err := Run(context.Background(), b.cfg, b)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := b.roles(), "plan clean dump upload verify verify clean"; got != want {
		t.Fatalf("units, in order: %s\nwant:            %s", got, want)
	}
	app := st.Apps["blog"]
	if app.Class != record.OK || app.Snapshot == nil || app.Snapshot.ID != snapA || app.LastOK == nil {
		t.Fatalf("%+v", app)
	}
	if app.Looked != filepath.Join(b.root, "blog", "shared") {
		t.Errorf("the record does not say where it looked: %q", app.Looked)
	}
	again, err := record.Read(filepath.Join(b.cfg.StateDir, "status.json"))
	if err != nil || again.Apps["blog"].Class != record.OK {
		t.Fatalf("the record on disk: %+v, %v", again, err)
	}
	if left, _ := os.ReadDir(b.cfg.RunDir); len(left) != 2 { // the lock and the unit list
		t.Errorf("left in the run dir: %v", left)
	}
}

// Who holds what. The unit that parses the app's bytes has no network
// and no credential; the unit with the credential is another account
// and sees one app, read-only, at paths that do not depend on the root.
func TestWhatEachUnitIsGiven(t *testing.T) {
	b := newBox(t)
	if _, err := Run(context.Background(), b.cfg, b); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"plan", "dump", "clean"} {
		s := b.spec(role)
		if s.Network || s.EnvironmentFile != "" || len(s.Capabilities) != 0 || !s.SameUIDNamespaces || s.AsRoot {
			t.Errorf("%s: network=%v envfile=%q caps=%v namespaces=%v root=%v", role, s.Network, s.EnvironmentFile, s.Capabilities, s.SameUIDNamespaces, s.AsRoot)
		}
	}
	if s := b.spec("plan"); s.User != backupUser {
		t.Errorf("the plan unit runs as %q: the config dir holds env files the hotserve group can read", s.User)
	}
	if s := b.spec("dump"); s.User != dataUser {
		t.Errorf("the dump unit runs as %q: only the data user may open a live database", s.User)
	}
	up := b.spec("upload")
	if up.User != backupUser || !up.Network || up.EnvironmentFile != b.cfg.EnvFile || up.SameUIDNamespaces || up.AsRoot ||
		len(up.Capabilities) != 1 || up.Capabilities[0] != unit.CapDACReadSearch {
		t.Errorf("upload: %+v", up)
	}
	shared := filepath.Join(b.root, "blog", "shared")
	var dests []string
	for _, bind := range up.Binds {
		if bind.Writable {
			t.Errorf("the upload unit can write %s", bind.Source)
		}
		if bind.Source == shared {
			t.Errorf("the upload unit is given the whole shared dir, though only uploads is declared")
		}
		dests = append(dests, bind.Dest)
	}
	if got := strings.Join(dests, " "); got != "/backup/blog/sqlite /backup/blog/plan.json /backup/blog/files/uploads" {
		t.Errorf("the upload unit's view: %s", got)
	}
	for _, s := range b.specs {
		if s.Environment != nil && strings.Contains(strings.Join(s.Environment, " "), "the-secret") {
			t.Errorf("%s: the engine read the credential file", s.Name)
		}
	}
}

// Nothing an operator or an app chose is in any command line: systemd
// never expands it there, and it is not there to expand.
func TestDeclaredStringsNeverReachArgv(t *testing.T) {
	b := newBox(t)
	// (Braces are refused long before this: a placeholder. What is left
	// is what systemd itself would expand.)
	b.root = filepath.Join(filepath.Dir(b.root), "live $RESTIC_PASSWORD swap")
	evil := "up $RESTIC_PASSWORD $HOME %h"
	must(t, os.MkdirAll(filepath.Join(b.root, "blog", "shared", evil), 0o755))
	b.plan = fmt.Sprintf(`{"root":%q,"apps":{"blog":{"sqlite":["$DB.db"],"files":[%q]}}}`, b.root, evil)
	b.ls = func(string) string { return "" }
	if _, err := Run(context.Background(), b.cfg, b); err != nil {
		t.Fatal(err)
	}
	for _, s := range b.specs {
		for _, arg := range s.Argv {
			if strings.ContainsAny(arg, "$%") || strings.Contains(arg, "swap") {
				t.Errorf("%s: argv holds %q", s.Name, arg)
			}
		}
	}
}

func TestADatabaseInsideAFilesPathIsMaskedThere(t *testing.T) {
	b := newBox(t)
	b.plan = fmt.Sprintf(`{"root":%q,"apps":{"blog":{"sqlite":["data/app.db"],"files":["."]}}}`, b.root)
	b.ls = func(string) string { return "" }
	if _, err := Run(context.Background(), b.cfg, b); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(b.spec("upload").Masked, " ")
	want := "/backup/blog/files/data/app.db /backup/blog/files/data/app.db-wal /backup/blog/files/data/app.db-shm /backup/blog/files/data/app.db-journal"
	if got != want {
		t.Fatalf("masked: %s\nwant:   %s", got, want)
	}
}

func TestNoDataYetAndDataGone(t *testing.T) {
	b := newBox(t)
	must(t, os.RemoveAll(filepath.Join(b.root, "blog")))
	st, err := Run(context.Background(), b.cfg, b)
	if err != nil {
		t.Fatal(err)
	}
	if app := st.Apps["blog"]; app.Class != record.Pending || !strings.Contains(app.Detail, b.root) {
		t.Fatalf("an app never deployed and never backed up: %+v", app)
	}
	if got := b.roles(); got != "plan" {
		t.Fatalf("units started for an app with no data: %s", got)
	}

	// Deployed, backed up, and then the data goes.
	must(t, os.MkdirAll(filepath.Join(b.root, "blog", "shared", "uploads"), 0o755))
	if st, err = Run(context.Background(), b.cfg, b); err != nil || st.Apps["blog"].Class != record.OK {
		t.Fatalf("%+v, %v", st.Apps["blog"], err)
	}
	must(t, os.RemoveAll(filepath.Join(b.root, "blog")))
	st, err = Run(context.Background(), b.cfg, b)
	if err != nil {
		t.Fatal(err)
	}
	app := st.Apps["blog"]
	if app.Class != record.DataMissing || !strings.Contains(app.Detail, snapA[:8]) || app.LastOK == nil {
		t.Fatalf("data that was backed up and is now gone must never read as not-yet-deployed: %+v", app)
	}
}

// Only "no such file" means the data is absent.
func TestAnErrorLookingAtTheDataIsNotItsAbsence(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root is refused nothing")
	}
	b := newBox(t)
	must(t, os.Chmod(filepath.Join(b.root, "blog"), 0o000))
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(b.root, "blog"), 0o755) })
	st, err := Run(context.Background(), b.cfg, b)
	if err != nil {
		t.Fatal(err)
	}
	if app := st.Apps["blog"]; app.Class != record.Failed || !strings.Contains(app.Detail, "permission denied") {
		t.Fatalf("%+v", app)
	}
}

func TestHowAnUploadEnds(t *testing.T) {
	for name, tc := range map[string]struct {
		outcome unit.Outcome
		summary string
		class   record.Class
		detail  string
		wide    bool
	}{
		"exit 0 and no snapshot id":   {unit.Outcome{Result: "success"}, `{"message_type":"summary"}`, record.Failed, "names no snapshot", false},
		"exit 0 and a bad id":         {unit.Outcome{Result: "success"}, `{"message_type":"summary","snapshot_id":"latest"}`, record.Failed, "names no snapshot", false},
		"exit 3 with a snapshot":      {unit.Outcome{Result: "exit-code", ExitStatus: 3}, "", record.Incomplete, "exit 3", false},
		"no repository":               {unit.Outcome{Result: "exit-code", ExitStatus: 10}, `{}`, record.Failed, "no repository", true},
		"locked":                      {unit.Outcome{Result: "exit-code", ExitStatus: 11}, `{}`, record.Failed, "stayed locked", true},
		"wrong password":              {unit.Outcome{Result: "exit-code", ExitStatus: 12}, `{}`, record.Failed, "password is wrong", true},
		"exit 1 is not no-repository": {unit.Outcome{Result: "exit-code", ExitStatus: 1}, `{}`, record.Failed, "could not be reached, or refused the key", true},
		"killed":                      {unit.Outcome{Result: "oom-kill", ExitStatus: 9}, `{}`, record.Failed, "ended by oom-kill", false},
	} {
		t.Run(name, func(t *testing.T) {
			b := newBox(t)
			must(t, os.MkdirAll(filepath.Join(b.root, "shop", "shared"), 0o755))
			b.plan = fmt.Sprintf(`{"root":%q,"apps":{"blog":{"files":["uploads"]},"shop":{"files":["."]}}}`, b.root)
			b.outcome["upload"] = tc.outcome
			if tc.summary != "" {
				b.summary = tc.summary
			}
			st, err := Run(context.Background(), b.cfg, b)
			if err != nil {
				t.Fatal(err)
			}
			blog, shop := st.Apps["blog"], st.Apps["shop"]
			if blog.Class != tc.class || !strings.Contains(blog.Detail, tc.detail) {
				t.Fatalf("blog: %+v", blog)
			}
			if strings.Contains(blog.Detail, "no repository") && tc.outcome.ExitStatus != 10 {
				t.Fatalf("called a missing repository: %s", blog.Detail)
			}
			if tc.wide != (shop.Class == record.NotAttempted) {
				t.Fatalf("shop after blog's failure: %+v", shop)
			}
			if !strings.HasSuffix(b.roles(), "clean") {
				t.Fatalf("staging was not emptied after the failure: %s", b.roles())
			}
		})
	}
}

// restic leaves a vanished path out with exit 0, so the listing is what
// finds it.
func TestADeclaredPathMissingFromTheSnapshotIsNotOK(t *testing.T) {
	b := newBox(t)
	b.ls = func(parent string) string {
		if parent == "/backup/blog/sqlite" {
			return `{"struct_type":"node","path":"/backup/blog/sqlite/app.db","type":"file","size":0}`
		}
		return `{"struct_type":"node","path":"/backup/blog/files/other","type":"dir"}`
	}
	st, err := Run(context.Background(), b.cfg, b)
	if err != nil {
		t.Fatal(err)
	}
	app := st.Apps["blog"]
	if app.Class != record.Incomplete || app.LastOK != nil {
		t.Fatalf("%+v", app)
	}
	for _, it := range app.Items {
		if it.OK {
			t.Errorf("%s %q is reported as in the snapshot", it.Kind, it.Path)
		}
	}
}

func TestOneDatabaseFailingDoesNotStopTheRest(t *testing.T) {
	b := newBox(t)
	b.dump = func(paths []string) []dump.Result {
		return []dump.Result{{Path: "app.db", Class: dump.NeverOpened, Detail: "killed"}}
	}
	b.ls = func(string) string { return `{"struct_type":"node","path":"/backup/blog/files/uploads","type":"dir"}` }
	st, err := Run(context.Background(), b.cfg, b)
	if err != nil {
		t.Fatal(err)
	}
	app := st.Apps["blog"]
	if app.Class != record.Incomplete || app.Snapshot == nil || !strings.Contains(app.Detail, "never opened") {
		t.Fatalf("the files are still worth uploading, and the run is not ok: %+v", app)
	}
}

func TestADumpUnitThatLiesIsNotBelieved(t *testing.T) {
	b := newBox(t)
	b.dump = func([]string) []dump.Result { return []dump.Result{{Path: "other.db", Class: dump.OK}} }
	b.ls = func(string) string { return `{"struct_type":"node","path":"/backup/blog/files/uploads","type":"dir"}` }
	st, err := Run(context.Background(), b.cfg, b)
	if err != nil {
		t.Fatal(err)
	}
	if it := st.Apps["blog"].Items[0]; it.OK || !strings.Contains(it.Detail, "other.db") {
		t.Fatalf("%+v", it)
	}
}

func TestAPlanThatCannotBeMadeKeepsWhatWasKnown(t *testing.T) {
	b := newBox(t)
	if _, err := Run(context.Background(), b.cfg, b); err != nil {
		t.Fatal(err)
	}
	b.specs = nil
	b.outcome["plan"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	st, err := Run(context.Background(), b.cfg, b)
	if err == nil || st.Error == "" {
		t.Fatalf("want the run to fail, got %+v, %v", st, err)
	}
	if b.roles() != "plan" {
		t.Fatalf("units after a failed plan: %s", b.roles())
	}
	if app := st.Apps["blog"]; app == nil || app.LastOK == nil {
		t.Fatalf("the last known state of blog was dropped: %+v", st.Apps)
	}

	// And a plan root would not accept is not acted on, whatever the
	// unit that made it says it checked.
	b.outcome = map[string]unit.Outcome{}
	b.plan = `{"root":"/var/lib/liveswap","apps":{"../etc":{"files":["."]}}}`
	if _, err := Run(context.Background(), b.cfg, b); err == nil || !strings.Contains(err.Error(), "is not one liveswap accepts") {
		t.Fatalf("%v", err)
	}
}

func TestOneRunAtATimeAndLeftoversAreStoppedByName(t *testing.T) {
	b := newBox(t)
	must(t, os.MkdirAll(b.cfg.RunDir, 0o755))
	unlock, err := lock(filepath.Join(b.cfg.RunDir, "lock"))
	must(t, err)
	if _, err := Run(context.Background(), b.cfg, b); !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), "pid ") {
		t.Fatalf("a second run: %v", err)
	}
	if len(b.specs) != 0 {
		t.Fatalf("the second run started units: %s", b.roles())
	}
	unlock()

	left := "hotserve_backup_upload_blog_0123456789ab.service"
	must(t, os.WriteFile(filepath.Join(b.cfg.RunDir, "units"), []byte(left+"\n"), 0o600))
	if _, err := Run(context.Background(), b.cfg, b); err != nil {
		t.Fatal(err)
	}
	if len(b.stopped) != 1 || b.stopped[0] != left {
		t.Fatalf("stopped: %v", b.stopped)
	}
	raw, _ := os.ReadFile(filepath.Join(b.cfg.RunDir, "units"))
	if strings.Contains(string(raw), left) || len(strings.Fields(string(raw))) != len(b.specs) {
		t.Fatalf("the unit list after the run:\n%s", raw)
	}
}

func TestNotSetUp(t *testing.T) {
	b := newBox(t)
	must(t, os.Remove(b.cfg.EnvFile))
	if _, err := Run(context.Background(), b.cfg, b); err == nil || !strings.Contains(err.Error(), "not set up") {
		t.Fatalf("%v", err)
	}
	if len(b.specs) != 0 {
		t.Fatalf("units were started: %s", b.roles())
	}
}

// A declared path is the app's own to replace, and the upload unit
// reads any file it is shown. A link is refused wherever it sits, and
// the manager is never given the app's name for anything: only mount
// points of the run's own. (That a mount made from a pin cannot be
// re-aimed is the kernel's doing, and the integration suite's to show.)
func TestALinkIsNeverFollowedAndNothingIsBoundByName(t *testing.T) {
	b := newBox(t)
	sibling := filepath.Join(b.root, "shop", "shared")
	must(t, os.MkdirAll(sibling, 0o755))
	must(t, os.WriteFile(filepath.Join(sibling, "orders.txt"), []byte("the sibling's"), 0o600))
	shared := filepath.Join(b.root, "blog", "shared")
	must(t, os.Symlink(sibling, filepath.Join(shared, "linked")))
	must(t, os.MkdirAll(filepath.Join(shared, "media"), 0o755))
	must(t, os.Symlink(sibling, filepath.Join(shared, "media", "deep")))
	b.plan = fmt.Sprintf(`{"root":%q,"apps":{"blog":{"files":["uploads","linked","media/deep/x"]}}}`, b.root)
	b.ls = func(string) string { return `{"struct_type":"node","path":"/backup/blog/files/uploads","type":"dir"}` }

	st, err := Run(context.Background(), b.cfg, b)
	if err != nil {
		t.Fatal(err)
	}
	up := b.spec("upload")
	for _, bind := range up.Binds {
		if strings.Contains(bind.Dest, "linked") || strings.Contains(bind.Dest, "deep") {
			t.Errorf("a path through a link was bound: %+v", bind)
		}
		if strings.HasPrefix(bind.Dest, "/backup/blog/files") {
			// By a mount point of the run's own, never by a name the app
			// can re-aim between the look and the bind.
			if !strings.HasPrefix(bind.Source, b.cfg.RunDir+"/") {
				t.Errorf("a declared path is bound by name: %+v", bind)
			}
			if got, want := b.mounted[bind.Source], filepath.Join(shared, "uploads"); got != want {
				t.Errorf("the mount point was made from %q, want %q", got, want)
			}
		}
	}
	app := st.Apps["blog"]
	if app.Class != record.Incomplete {
		t.Fatalf("%+v", app)
	}
	for _, it := range app.Items[1:] {
		if it.OK || !strings.Contains(it.Detail, "symbolic link") {
			t.Errorf("%q: %+v", it.Path, it)
		}
	}
}

func TestASharedDirReachedThroughALinkIsRefused(t *testing.T) {
	b := newBox(t)
	shared := filepath.Join(b.root, "blog", "shared")
	must(t, os.Rename(shared, shared+".real"))
	must(t, os.Symlink(filepath.Join(b.root, "shop", "shared"), shared))
	st, err := Run(context.Background(), b.cfg, b)
	if err != nil {
		t.Fatal(err)
	}
	if app := st.Apps["blog"]; app.Class != record.Failed || !strings.Contains(app.Detail, "symbolic link") {
		t.Fatalf("%+v", app)
	}
	if b.roles() != "plan" {
		t.Fatalf("units were started for it: %s", b.roles())
	}
}

// The liveswap root itself may be an alias; liveswap allows that, and
// it is the operator's to set.
func TestASymlinkedRootIsFollowed(t *testing.T) {
	b := newBox(t)
	alias := filepath.Join(filepath.Dir(b.root), "alias")
	must(t, os.Symlink(b.root, alias))
	b.plan = strings.Replace(b.plan, fmt.Sprintf("%q", b.root), fmt.Sprintf("%q", alias), 1)
	st, err := Run(context.Background(), b.cfg, b)
	if err != nil || st.Apps["blog"].Class != record.OK {
		t.Fatalf("%+v, %v", st.Apps["blog"], err)
	}
}

func TestEveryMountIsTakenAwayAndLeftoversAreSwept(t *testing.T) {
	b := newBox(t)
	left := filepath.Join(b.cfg.RunDir, "deadbeef0000", "mount-1")
	must(t, os.MkdirAll(left, 0o700))
	b.leftMounts = []string{left}
	if _, err := Run(context.Background(), b.cfg, b); err != nil {
		t.Fatal(err)
	}
	if len(b.unmounted) == 0 || b.unmounted[0] != left {
		t.Fatalf("the earlier run's mount was not taken away first: %v", b.unmounted)
	}
	for target := range b.mounted {
		found := false
		for _, u := range b.unmounted {
			found = found || u == target
		}
		if !found {
			t.Errorf("%s was mounted and never unmounted", target)
		}
	}
	if _, err := os.Stat(filepath.Dir(left)); err == nil {
		t.Errorf("the earlier run's directory is still there")
	}
}
