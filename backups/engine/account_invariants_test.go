package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// The account's lookups, as tables: the account every restic unit
// runs as is hotserve's own, and what says so is what the account
// database answers, through a program that can be slow, fail, or not
// know. The rows of which account is accepted are in
// TestAnAccountHotserveDidNotMakeIsRefused; these are the rows of how
// it is asked.

// The lookups as they are, kept from before any box stands in for them.
var realAccount, realLookup, realDataOwner, realOwnerOf, realMakeAccount = account, lookup, dataOwner, ownerOf, makeAccount

// said is what a program of the account databases answers.
type said struct {
	out  string
	exit int
	err  error
}

// databases stands answers in for the programs, by their whole command
// line. A command line no row wrote is not found, and said.
func databases(t *testing.T, answers map[string]said) {
	t.Helper()
	oldLookup := lookup
	t.Cleanup(func() { lookup = oldLookup })
	lookup = func(_ context.Context, _ time.Duration, argv ...string) ([]byte, int, error) {
		a, ok := answers[strings.Join(argv, " ")]
		if !ok {
			t.Errorf("asked %q, which no row answers", strings.Join(argv, " "))
			return nil, 2, nil
		}
		return []byte(a.out), a.exit, a.err
	}
}

// A lookup that fails refuses: an exit status that is neither "found"
// nor "not found", an answer that is no line of the database, a
// program that did not run — each is an error, in the lookup's own
// words, and never an account taken as found, as absent, or as right.
func TestALookupThatFailsRefuses(t *testing.T) {
	const cmd = "/usr/bin/getent passwd hotserve-backup"
	for _, tc := range []struct {
		name string
		said said
		want string
	}{
		{"exit 1", said{exit: 1}, cmd + ": exit status 1"},
		{"five fields", said{out: "hotserve-backup:x:995:995:/nonexistent\n"}, "not a passwd line"},
		{"a uid that is no number", said{out: "hotserve-backup:x:nine:995:made-by-hotserve:/nonexistent:/usr/sbin/nologin\n"}, "not a passwd line"},
		{"nothing, and exit 0", said{}, "not a passwd line"},
		{"did not run", said{err: errors.New("fork/exec /usr/bin/getent: no such file or directory")}, "no such file or directory"},
		{"did not answer", said{err: errors.New(cmd + " did not answer within 10s")}, "did not answer within 10s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			databases(t, map[string]said{cmd: tc.said})
			acct, err := realAccount(context.Background(), backupUser)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("account = %+v, err = %v\nwant it to say %q", acct, err, tc.want)
			}
		})
	}
	// "Not found" is an answer, and the one exit status that means it;
	// and the mark is read from the comment field.
	databases(t, map[string]said{cmd: {exit: 2}})
	if acct, err := realAccount(context.Background(), backupUser); err != nil || acct.exists {
		t.Fatalf("an account that is not there: %+v, %v", acct, err)
	}
	databases(t, map[string]said{cmd: {out: "hotserve-backup:x:995:995:made-by-hotserve:/nonexistent:/usr/sbin/nologin\n"}})
	if acct, err := realAccount(context.Background(), backupUser); err != nil || acct.comment != accountMark || acct.uid != 995 {
		t.Fatalf("hotserve's account: %+v, %v", acct, err)
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

// Making the account ends with setup's command: an interrupt — Ctrl-C
// at the terminal setup runs at — ends a useradd that does not return
// (a directory it asks that is not there), and is no account made. It
// has no bound of its own: setup is attended, someone is there to stop
// it (D4), and a clock would be a new limit on a box whose directory is
// slow.
func TestMakingTheAccountEndsWithItsCommand(t *testing.T) {
	if _, err := os.Stat("/bin/sleep"); err != nil {
		t.Skip("no /bin/sleep here")
	}
	old := useradd
	useradd = []string{"/bin/sleep", "5"}
	t.Cleanup(func() { useradd = old })
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	began := time.Now()
	err := realMakeAccount(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a useradd whose command was stopped: err = %v", err)
	}
	if took := time.Since(began); took > 3*time.Second {
		t.Fatalf("a useradd whose command was stopped at 100ms took %s", took)
	}
}

// The context a command runs on reaches the account's lookup, and the
// account is one answer: looked up once, as the command begins, and
// what a fetch gives its directory to is the account that was looked
// at then — not a second lookup's, which a directory may answer
// otherwise.
func TestTheAccountCheckRunsOnItsCommandsContext(t *testing.T) {
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "this command's")
	b := restoreBox(t)
	asked := 0
	account = func(c context.Context, _ string) (passwd, error) {
		if c.Value(key{}) != "this command's" {
			t.Error("the account was asked on a context that is not the command's")
		}
		asked++
		return passwd{name: backupUser, comment: accountMark, uid: 995, gid: 995, exists: true}, nil
	}
	if _, _, err := Drill(ctx, b.cfg, b); err != nil {
		t.Fatal(err)
	}
	if asked != 1 {
		t.Errorf("the account was looked up %d times in one drill", asked)
	}
	if !b.started("fetch") {
		t.Fatal("the drill fetched nothing: the row proves nothing")
	}
	if len(b.owned4) == 0 || b.owned4[0].uid != 995 || b.owned4[0].name != backupUser {
		t.Errorf("the fetch's directory was given to %+v, not to the account the check looked at", b.owned4)
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
	databases(t, map[string]said{
		"/usr/bin/getent passwd hotserve": {out: "hotserve:*:5151:5152::/var/lib/hotserve:/usr/sbin/nologin\n"},
	})
	uid, gid, err := realDataOwner(context.Background())
	if err != nil || uid != 5151 || gid != 5152 {
		t.Fatalf("the data user's ids = %d, %d, %v; the lookup said 5151, 5152", uid, gid, err)
	}
	databases(t, map[string]said{"/usr/bin/getent passwd hotserve": {exit: 2}})
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
