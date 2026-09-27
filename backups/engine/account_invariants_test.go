package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The account invariant's lookups, as tables: nobody but the manager
// can be, or act as, the hotserve-backup account — and what says so is
// what the account databases answer, through programs that can be
// slow, fail, or not know. The rows of who the account is are in
// TestAnAccountMadeWrongIsRefusedNotNormalised; these are the rows of
// how it is asked.

// The lookups as they are, kept from before any box stands in for them.
var realAccount, realHolders, realGroupsOf, realGroupNamed, realShadowed, realLookup, realDataOwner, realOwnerOf = account, holders, groupsOf, groupNamed, shadowed, lookup, dataOwner, ownerOf

// given is how long each command line was given to answer, of those
// the last databases was asked.
var given map[string]time.Duration

// said is what a program of the account databases answers.
type said struct {
	out  string
	exit int
	err  error
}

// databases stands answers in for the programs, by their whole command
// line, a conf in for nsswitch.conf, and root in for whoever runs the
// test. A command line no row wrote is not found, and said.
func databases(t *testing.T, conf string, answers map[string]said) {
	t.Helper()
	oldLookup, oldConf, oldRoot, oldReadable := lookup, nsswitchFile, isRoot, readable
	t.Cleanup(func() { lookup, nsswitchFile, isRoot, readable = oldLookup, oldConf, oldRoot, oldReadable })
	isRoot = func() bool { return true }
	readable = func(string) error { return nil }
	given = map[string]time.Duration{}
	nsswitchFile = filepath.Join(t.TempDir(), "nsswitch.conf")
	if conf != "" {
		must(t, os.WriteFile(nsswitchFile, []byte(conf), 0o644))
	}
	lookup = func(_ context.Context, within time.Duration, argv ...string) ([]byte, int, error) {
		given[strings.Join(argv, " ")] = within
		a, ok := answers[strings.Join(argv, " ")]
		if !ok {
			t.Errorf("asked %q, which no row answers", strings.Join(argv, " "))
			return nil, 2, nil
		}
		return []byte(a.out), a.exit, a.err
	}
}

const (
	backupLine = "hotserve-backup:x:995:995::/nonexistent:/usr/sbin/nologin\n"
	aliceLine  = "alice:*:995:995::/home/alice:/bin/bash\n"
	rootLine   = "root:x:0:0:root:/root:/bin/bash\n"
)

// Every holder of the uid is found: the accounts the box enumerates,
// and the account each source of the passwd line answers with when it
// is asked for the uid — a directory need not enumerate to be asked
// [M65]. What no lookup finds is a row too: a source that does not
// answer says what absence says.
func TestEveryHolderOfTheUidIsFound(t *testing.T) {
	for _, tc := range []struct {
		name    string
		conf    string
		answers map[string]said
		want    []string
		err     string
	}{
		{"enumerated", "passwd: files\n", map[string]said{
			"/usr/bin/getent passwd":              {out: rootLine + "alice:x:995:1000::/home/alice:/bin/bash\n" + backupLine},
			"/usr/bin/getent -s files passwd 995": {out: "alice:x:995:1000::/home/alice:/bin/bash\n"},
		}, []string{"alice", "hotserve-backup"}, ""},
		{"a uid a directory holds and does not enumerate", "passwd: files sss\n", map[string]said{
			"/usr/bin/getent passwd":              {out: rootLine + backupLine},
			"/usr/bin/getent -s files passwd 995": {out: backupLine},
			"/usr/bin/getent -s sss passwd 995":   {out: aliceLine},
		}, []string{"hotserve-backup", "alice"}, ""},
		{"held by the second of three sources", "passwd: files systemd [NOTFOUND=return] ldap\n", map[string]said{
			"/usr/bin/getent passwd":                {out: rootLine + backupLine},
			"/usr/bin/getent -s files passwd 995":   {out: backupLine},
			"/usr/bin/getent -s systemd passwd 995": {exit: 2},
			"/usr/bin/getent -s ldap passwd 995":    {out: "bob:*:995:100::/home/bob:/bin/sh\n"},
		}, []string{"hotserve-backup", "bob"}, ""},
		// What cannot be told from absence: the daemon down, the module
		// not installed. getent says 2 for each [M65].
		{"a source that does not answer", "passwd: files sss\n", map[string]said{
			"/usr/bin/getent passwd":              {out: rootLine + backupLine},
			"/usr/bin/getent -s files passwd 995": {out: backupLine},
			"/usr/bin/getent -s sss passwd 995":   {exit: 2},
		}, []string{"hotserve-backup"}, ""},
		{"the account, answered by enumeration and by its source, is one holder", "passwd: files\n", map[string]said{
			"/usr/bin/getent passwd":              {out: rootLine + backupLine},
			"/usr/bin/getent -s files passwd 995": {out: backupLine},
		}, []string{"hotserve-backup"}, ""},
		{"no nsswitch.conf, which is files", "", map[string]said{
			"/usr/bin/getent passwd":              {out: backupLine},
			"/usr/bin/getent -s files passwd 995": {out: backupLine},
		}, []string{"hotserve-backup"}, ""},

		// A lookup that fails is never an account that passes.
		{"a source that fails", "passwd: files sss\n", map[string]said{
			"/usr/bin/getent passwd":              {out: backupLine},
			"/usr/bin/getent -s files passwd 995": {out: backupLine},
			"/usr/bin/getent -s sss passwd 995":   {exit: 1},
		}, nil, "/usr/bin/getent -s sss passwd 995: exit status 1"},
		{"a source that does not end", "passwd: files sss\n", map[string]said{
			"/usr/bin/getent passwd":              {out: backupLine},
			"/usr/bin/getent -s files passwd 995": {out: backupLine},
			"/usr/bin/getent -s sss passwd 995":   {err: errors.New("/usr/bin/getent -s sss passwd 995 did not answer within 10s")},
		}, nil, "did not answer within 10s"},
		{"a source that answers with no passwd line", "passwd: files sss\n", map[string]said{
			"/usr/bin/getent passwd":              {out: backupLine},
			"/usr/bin/getent -s files passwd 995": {out: backupLine},
			"/usr/bin/getent -s sss passwd 995":   {out: "alice:995\n"},
		}, nil, "/usr/bin/getent -s sss passwd 995: not a passwd line: alice:995"},
		{"an enumeration that fails", "passwd: files\n", map[string]said{
			"/usr/bin/getent passwd": {exit: 1},
		}, nil, "/usr/bin/getent passwd: exit status 1"},
		{"a source whose name is no name", "passwd: files --service=x\n", map[string]said{
			"/usr/bin/getent passwd": {out: backupLine},
		}, nil, "names a source that is no name: --service=x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			databases(t, tc.conf, tc.answers)
			got, err := realHolders(context.Background(), 995)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("holders = %q, err = %v\nwant an error saying %q", got, err, tc.err)
				}
				return
			}
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("holders = %q, err = %v\nwant %q", got, err, tc.want)
			}
		})
	}
	// The listing of the whole database is given a minute (the owner,
	// 2026-09-27): on a box joined to a large directory it is the one
	// lookup that takes as long as the directory is large, and ten
	// seconds would refuse every run there. The lookups by a key are
	// given ten.
	databases(t, "passwd: files sss\n", map[string]said{
		"/usr/bin/getent passwd":              {out: backupLine},
		"/usr/bin/getent -s files passwd 995": {out: backupLine},
		"/usr/bin/getent -s sss passwd 995":   {exit: 2},
	})
	if _, err := realHolders(context.Background(), 995); err != nil {
		t.Fatal(err)
	}
	for command, want := range map[string]time.Duration{
		"/usr/bin/getent passwd":              time.Minute,
		"/usr/bin/getent -s files passwd 995": 10 * time.Second,
		"/usr/bin/getent -s sss passwd 995":   10 * time.Second,
	} {
		if given[command] != want {
			t.Errorf("%s was given %s to answer, want %s", command, given[command], want)
		}
	}

	// An nsswitch.conf that is there and cannot be read refuses: what
	// the sources are is not known.
	databases(t, "", map[string]said{"/usr/bin/getent passwd": {out: backupLine}})
	must(t, os.Mkdir(nsswitchFile, 0o755))
	if got, err := realHolders(context.Background(), 995); err == nil || !strings.Contains(err.Error(), nsswitchFile) {
		t.Fatalf("with nsswitch.conf unreadable: holders = %q, err = %v", got, err)
	}
}

// The passwd line is read as glibc reads it: the sources in their
// order, the actions in brackets no source, a comment and what follows
// it nothing, a source named twice asked once; no line, as no file, is
// files.
func TestThePasswdLineIsReadAsGlibcReadsIt(t *testing.T) {
	for _, tc := range []struct {
		name, conf string
		want       []string
		err        string
	}{
		{"files", "passwd: files\n", []string{"files"}, ""},
		{"Debian 13's", "passwd:         files systemd\ngroup:          files systemd\n", []string{"files", "systemd"}, ""},
		{"an action between two", "passwd: files sss [NOTFOUND=return] ldap\n", []string{"files", "sss", "ldap"}, ""},
		{"an action of two words", "passwd: files [!UNAVAIL=return SUCCESS=continue] ldap\n", []string{"files", "ldap"}, ""},
		{"an action with spaces inside its brackets", "passwd: files [ NOTFOUND=return ] ldap\n", []string{"files", "ldap"}, ""},
		{"tabs, and a comment after", "passwd:\tfiles\tsss # the directory\n", []string{"files", "sss"}, ""},
		{"a line commented out", "#passwd: ldap\npasswd: files\n", []string{"files"}, ""},
		{"a source twice", "passwd: files sss files\n", []string{"files", "sss"}, ""},
		{"the last line of two wins, as glibc has it", "passwd: files\npasswd: files ldap\n", []string{"files", "ldap"}, ""},
		{"another database's line", "group: files ldap\nshadow: files\n", []string{"files"}, ""},
		{"no source on the line", "passwd:\n", []string{"files"}, ""},
		{"an empty file", "", []string{"files"}, ""},
		{"a digit and an underscore", "passwd: files nis_plus2\n", []string{"files", "nis_plus2"}, ""},
		{"a name that is an option", "passwd: files -s\n", nil, "names a source that is no name: -s"},
		{"a bracket never closed", "passwd: files [NOTFOUND=return ldap\n", nil, "an action that is never closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sourcesOf(tc.conf)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("sources = %q, err = %v\nwant an error saying %q", got, err, tc.err)
				}
				return
			}
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("sources = %q, err = %v\nwant %q", got, err, tc.want)
			}
		})
	}
}

// A lookup that fails refuses: an exit status that is neither "found"
// nor "not found", an answer that is no line of the database, a
// program that did not run — each is an error, in the lookup's own
// words, and never an account taken as found, as absent, or as right.
func TestALookupThatFailsRefuses(t *testing.T) {
	ctx := context.Background()
	ask := map[string]func() error{
		"/usr/bin/getent passwd hotserve-backup": func() error {
			acct, err := realAccount(ctx, backupUser)
			if err == nil && !acct.exists {
				return errors.New("taken as not there")
			}
			return err
		},
		"/usr/bin/getent group hotserve": func() error {
			_, there, err := realGroupNamed(ctx, dataUser)
			if err == nil && !there {
				return errors.New("taken as not there")
			}
			return err
		},
		"/usr/bin/id -G hotserve-backup": func() error { _, err := realGroupsOf(ctx, backupUser); return err },
		"/usr/bin/getent shadow hotserve-backup": func() error {
			_, found, err := realShadowed(ctx, backupUser)
			if err == nil && !found {
				return errors.New("taken as not there")
			}
			return err
		},
	}
	for _, tc := range []struct {
		command string
		name    string
		said    said
		want    string
	}{
		{"/usr/bin/getent passwd hotserve-backup", "exit 1", said{exit: 1}, "/usr/bin/getent passwd hotserve-backup: exit status 1"},
		{"/usr/bin/getent passwd hotserve-backup", "five fields", said{out: "hotserve-backup:x:995:995:/nonexistent\n"}, "not a passwd line"},
		{"/usr/bin/getent passwd hotserve-backup", "a uid that is no number", said{out: "hotserve-backup:x:nine:995::/nonexistent:/usr/sbin/nologin\n"}, "not a passwd line"},
		{"/usr/bin/getent passwd hotserve-backup", "nothing, and exit 0", said{}, "not a passwd line"},
		{"/usr/bin/getent passwd hotserve-backup", "did not run", said{err: errors.New("fork/exec /usr/bin/getent: no such file or directory")}, "no such file or directory"},
		{"/usr/bin/getent group hotserve", "exit 1", said{exit: 1}, "/usr/bin/getent group hotserve: exit status 1"},
		{"/usr/bin/getent group hotserve", "two fields", said{out: "hotserve:x\n"}, "not a group line"},
		{"/usr/bin/getent group hotserve", "a gid that is no number", said{out: "hotserve:x:many:\n"}, "not a group line"},
		{"/usr/bin/id -G hotserve-backup", "exit 1", said{exit: 1}, "/usr/bin/id -G hotserve-backup: exit status 1"},
		{"/usr/bin/id -G hotserve-backup", "a gid that is no number", said{out: "995 wheel\n"}, "not a gid"},
		{"/usr/bin/id -G hotserve-backup", "no group at all", said{out: "\n"}, "no group"},
		{"/usr/bin/getent shadow hotserve-backup", "exit 1", said{exit: 1}, "/usr/bin/getent shadow hotserve-backup: exit status 1"},
		{"/usr/bin/getent shadow hotserve-backup", "one field", said{out: "hotserve-backup\n"}, "not a shadow line"},
		{"/usr/bin/getent shadow hotserve-backup", "did not answer", said{err: errors.New("/usr/bin/getent shadow hotserve-backup did not answer within 10s")}, "did not answer within 10s"},
	} {
		t.Run(tc.command+", "+tc.name, func(t *testing.T) {
			databases(t, "passwd: files\n", map[string]said{tc.command: tc.said})
			if err := ask[tc.command](); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v\nwant it to say %q", err, tc.want)
			}
		})
	}

	// "Not found" is an answer, and the one exit status that means it.
	databases(t, "passwd: files\n", map[string]said{
		"/usr/bin/getent passwd hotserve-backup": {exit: 2},
		"/usr/bin/getent group hotserve":         {exit: 2},
		"/usr/bin/getent shadow hotserve-backup": {exit: 2},
	})
	if acct, err := realAccount(ctx, backupUser); err != nil || acct.exists {
		t.Fatalf("an account that is not there: %+v, %v", acct, err)
	}
	if _, there, err := realGroupNamed(ctx, dataUser); err != nil || there {
		t.Fatalf("a group that is not there: %v, %v", there, err)
	}
	if _, found, err := realShadowed(ctx, backupUser); err != nil || found {
		t.Fatalf("a shadow entry that is not there: %v, %v", found, err)
	}
	// And "not found" of the shadow database is believed only where
	// the database could be read: getent says 2 as well where root
	// could not open it — a security module, a root that is one in
	// name — and an account with a password would pass as one with
	// nothing to log in with. No file at all is no database.
	readable = func(string) error { return fs.ErrPermission }
	if _, found, err := realShadowed(ctx, backupUser); err == nil || found || !strings.Contains(err.Error(), "/etc/shadow could not be read, so whether the hotserve-backup account has a password is not known") {
		t.Fatalf("a shadow database that could not be read: found = %v, err = %v", found, err)
	}
	readable = func(string) error { return fs.ErrNotExist }
	if _, found, err := realShadowed(ctx, backupUser); err != nil || found {
		t.Fatalf("no shadow database at all: found = %v, err = %v", found, err)
	}
	readable = func(string) error { return nil }
	// The shadow database is root's to read: asked by anyone else it
	// answers "not found" for an entry that is there, so anyone else is
	// told that it could not be asked.
	isRoot = func() bool { return false }
	if _, _, err := realShadowed(ctx, backupUser); !errors.Is(err, errNeedsRoot) {
		t.Fatalf("the shadow database asked without root: err = %v", err)
	}
}

// A lookup is bounded, and ends with its command: a program of the
// account databases that never answers — a directory that is not
// there — is ended at the bound and said, where it would hold the run
// lock for as long as it liked; and a command that is stopped ends the
// program with it, which is an interrupt and no verdict.
func TestALookupIsBoundedAndEndsWithItsCommand(t *testing.T) {
	if _, err := os.Stat("/bin/sleep"); err != nil {
		t.Skip("no /bin/sleep here")
	}
	began := time.Now()
	_, _, err := realLookup(context.Background(), 300*time.Millisecond, "/bin/sleep", "5")
	if err == nil || !strings.Contains(err.Error(), "/bin/sleep 5 did not answer within 300ms") || errors.Is(err, context.Canceled) {
		t.Fatalf("a lookup that never answers: err = %v", err)
	}
	if took := time.Since(began); took > 3*time.Second {
		t.Fatalf("a lookup bounded at 300ms took %s", took)
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	began = time.Now()
	_, _, err = realLookup(ctx, time.Minute, "/bin/sleep", "5")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a lookup whose command was stopped: err = %v", err)
	}
	if took := time.Since(began); took > 3*time.Second {
		t.Fatalf("a lookup whose command was stopped at 100ms took %s", took)
	}

	// What it answers otherwise: what was printed, and the exit status,
	// which is the caller's to read and no error.
	out, exit, err := realLookup(context.Background(), time.Minute, "/bin/sh", "-c", "echo found; exit 2")
	if err != nil || exit != 2 || string(out) != "found\n" {
		t.Fatalf("out = %q, exit = %d, err = %v", out, exit, err)
	}
	if _, _, err := realLookup(context.Background(), time.Minute, "/nonexistent/getent", "passwd"); err == nil {
		t.Fatal("a program that is not there answered")
	}
}

// The context a command runs on reaches every lookup of the account
// check: setup's, and the one a run, a restore and a drill begin with.
func TestTheAccountCheckRunsOnItsCommandsContext(t *testing.T) {
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "this command's")
	b := restoreBox(t)
	var asked []string
	see := func(what string, c context.Context) {
		if c.Value(key{}) != "this command's" {
			t.Errorf("%s was asked on a context that is not the command's", what)
		}
		asked = append(asked, what)
	}
	account = func(c context.Context, _ string) (passwd, error) {
		see("account", c)
		return passwd{name: backupUser, password: "x", shell: "/usr/sbin/nologin", home: "/nonexistent", uid: 995, gid: 995, exists: true}, nil
	}
	holders = func(c context.Context, _ int) ([]string, error) { see("holders", c); return []string{backupUser}, nil }
	groupsOf = func(c context.Context, _ string) ([]int, error) { see("groups", c); return []int{995}, nil }
	groupNamed = func(c context.Context, _ string) (int, bool, error) { see("group", c); return 1000, true, nil }
	shadowed = func(c context.Context, _ string) (string, bool, error) { see("shadow", c); return "!", true, nil }
	if _, err := Drill(ctx, b.cfg, b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"account", "holders", "groups", "group", "shadow"} {
		if !slices.Contains(asked, want) {
			t.Errorf("a drill never asked %s: %q", want, asked)
		}
	}
	// And the account is one answer: looked up once, as the command
	// begins, and what a fetch gives its directory to is the account
	// that was looked at then — not a second lookup's, which a
	// directory may answer otherwise.
	if n := strings.Count(strings.Join(asked, " "), "account"); n != 1 {
		t.Errorf("the account was looked up %d times in one drill: %q", n, asked)
	}
	if !b.started("fetch") {
		t.Fatal("the drill fetched nothing: the row proves nothing")
	}
	if len(b.owned4) == 0 || b.owned4[0].uid != 995 || b.owned4[0].name != backupUser {
		t.Errorf("the fetch's directory was given to %+v, not to the account the check looked at", b.owned4)
	}
}

// Asked by someone who is not root — an administrator after mending
// the account — the check says what it can see, and never more than
// it saw: a fault that is there is said, with what could not be looked
// at beside it, and an account with none to see is not called right.
func TestTheAccountAskedWithoutRoot(t *testing.T) {
	const unseen = "whether it has a password is root's to read, and was not looked at: sudo hotserve-backup account"
	b := restoreBox(t)
	b.shadowErr = errNeedsRoot
	b.shell = "/bin/bash"
	err := AccountReady(context.Background())
	if err == nil || !strings.Contains(err.Error(), "the hotserve-backup account exists with a login shell (/bin/bash") || !strings.Contains(err.Error(), unseen) {
		t.Fatalf("with a login shell: err = %v", err)
	}
	b.shell = "/usr/sbin/nologin"
	err = AccountReady(context.Background())
	if err == nil || !strings.Contains(err.Error(), unseen) || strings.Contains(err.Error(), "exists with") {
		t.Fatalf("with nothing wrong that can be seen: err = %v", err)
	}
	// Any other failure of that lookup refuses as every lookup's does.
	b.shadowErr = errors.New("/usr/bin/getent shadow hotserve-backup: exit status 1")
	b.shell = "/bin/bash"
	if err := AccountReady(context.Background()); err == nil || !strings.Contains(err.Error(), "exit status 1") || strings.Contains(err.Error(), "login shell") {
		t.Fatalf("with a lookup that failed: err = %v", err)
	}
}

// The account is one answer: what a fetch gives its directory to is the
// account the check looked at, by the lookup the check made — not what
// /etc/passwd alone holds, which knows nothing of an account a
// directory holds.
func TestTheAccountIsOneAnswer(t *testing.T) {
	if uid, gid := realOwnerOf(passwd{name: backupUser, uid: 4242, gid: 4243, exists: true}); uid != 4242 || gid != 4243 {
		t.Fatalf("the account's ids = %d, %d; the lookup said 4242, 4243", uid, gid)
	}
	// The data user the same: the account the box resolves, a
	// directory's if it is one, as postinstall found it with getent.
	databases(t, "passwd: files\n", map[string]said{
		"/usr/bin/getent passwd hotserve": {out: "hotserve:*:5151:5152::/var/lib/hotserve:/usr/sbin/nologin\n"},
	})
	uid, gid, err := realDataOwner(context.Background())
	if err != nil || uid != 5151 || gid != 5152 {
		t.Fatalf("the data user's ids = %d, %d, %v; the lookup said 5151, 5152", uid, gid, err)
	}
	databases(t, "passwd: files\n", map[string]said{"/usr/bin/getent passwd hotserve": {exit: 2}})
	if _, _, err := realDataOwner(context.Background()); err == nil || !strings.Contains(err.Error(), "the hotserve account is not there") {
		t.Fatalf("with no data user: err = %v", err)
	}
}

// A lookup that failed stops the command whether or not anything
// would have used its answer: a run whose plan has no app reaches no
// caller of the data user's ids, and ended ok beside a lookup that
// had not answered.
func TestAFailedLookupStopsACommandThatWouldNotHaveUsedIt(t *testing.T) {
	b := newBox(t)
	b.plan = fmt.Sprintf(`{"root":%q,"apps":{}}`, b.root)
	dataOwner = func(context.Context) (int, int, error) {
		return 0, 0, errors.New("/usr/bin/getent passwd hotserve did not answer within 10s")
	}
	st, err := Run(context.Background(), b.cfg, b)
	if err == nil || !strings.Contains(err.Error(), "did not answer within 10s") {
		t.Fatalf("a run with no app, the data user's lookup having failed: err = %v, record %+v", err, st)
	}
	if len(b.specs) != 0 {
		t.Fatalf("units were started: %s", b.roles())
	}
}
