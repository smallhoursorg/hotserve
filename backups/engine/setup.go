package engine

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/smallhoursorg/hotserve/backups/dump"
	"github.com/smallhoursorg/hotserve/backups/envfile"
	"github.com/smallhoursorg/hotserve/backups/plan"
	"github.com/smallhoursorg/hotserve/backups/record"
	"github.com/smallhoursorg/hotserve/backups/unit"
)

// Terminal is the operator at a terminal: what setup asks, with echo
// off for a secret, and what it tells them.
type Terminal interface {
	Ask(ctx context.Context, prompt string, secret bool) (string, error)
	Say(line string)
}

// SetupOptions is what setup is given: the repository, which is not a
// secret and travels as an argument, and the operator.
type SetupOptions struct {
	Repository string
	Terminal   Terminal
}

// SetupReport is what setup did.
type SetupReport struct {
	Account      string // "made" or "present"
	Apps         []string
	New          bool // the repository was made by this setup
	RepositoryID string
	Aside        string   // where the record of the previous repository went, if anywhere
	Swept        []string // files an interrupted setup left, removed
	OldEnvFile   string   // a credential file from before this version, still there
}

// setupClock bounds restic init and the opening of an existing
// repository. restic init answers at once whatever is wrong — a wrong
// key, a host that does not resolve, a port nobody listens on, all
// measured — and after 30 s for a host that swallows packets; opening
// with a wrong password answers at once (exit 12). The clock is for
// what was not measured. setupNote is when a person waiting is told
// what for.
//
// setupProbeClock bounds the look for a repository that comes before
// any password is made: `cat config` with a throwaway password answers
// at once with a right key — exit 10 where there is no repository, 12
// where there is one — and retries for minutes on a bucket not there
// yet, a wrong key, a host that does not resolve [measured]. Those are
// init's to answer, at once, so the look is given up on soon.
var (
	setupClock      = 2 * time.Minute
	setupProbeClock = 10 * time.Second
	setupNote       = 20 * time.Second
)

// What setup checks the box has before it asks anyone for a secret, and
// how it makes the account. Variables so that the flow can be tested
// against a box that lacks them.
var (
	haveProgram = func(path string) bool {
		// Run as hotserve-backup and as the app's own uid, never as root:
		// the execute bit that counts is the one for everyone.
		st, err := os.Stat(path)
		return err == nil && st.Mode().IsRegular() && st.Mode()&0o001 != 0
	}
	// account is the account as the manager itself resolves it — getent,
	// so NSS, where os/user reads /etc/passwd alone and knows no shell:
	// its shell and home, and whether it is there at all. getent's
	// argv is constant; its exit 2 is "not found".
	account = func(ctx context.Context, name string) (passwd, error) {
		out, exit, err := lookup(ctx, lookupClock, getent, "passwd", name)
		if err != nil {
			return passwd{}, err
		}
		switch exit {
		case 0:
			return passwdLine(strings.TrimRight(string(out), "\n"), getent+" passwd "+name)
		case 2:
			return passwd{}, nil
		}
		return passwd{}, fmt.Errorf("%s passwd %s: exit status %d", getent, name, exit)
	}
	// holders is every account that holds a uid. Authorization is by
	// uid: whoever else holds it is the restic process. Two lookups,
	// since neither finds what the other does: the passwd database as
	// getent enumerates it, which finds every holder a source lists —
	// two in one file — and nothing of a directory that does not
	// enumerate, which says so by leaving its accounts out, exit 0; and
	// each source of the passwd line asked for the uid, which a
	// directory answers whether it enumerates or not, with the one
	// account it has there [M65]. A source that does not answer — its
	// daemon down, its module not installed — exits 2, as absence does:
	// what it holds is not known, and nothing local can know it.
	holders = func(ctx context.Context, uid int) ([]string, error) {
		out, exit, err := lookup(ctx, enumerationClock, getent, "passwd")
		if err != nil {
			return nil, err
		}
		if exit != 0 {
			return nil, fmt.Errorf("%s passwd: exit status %d", getent, exit)
		}
		var names []string
		for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
			fields := strings.Split(line, ":")
			if len(fields) == 7 && fields[2] == strconv.Itoa(uid) && !slices.Contains(names, fields[0]) {
				names = append(names, fields[0])
			}
		}
		sources, err := passwdSources()
		if err != nil {
			return nil, err
		}
		for _, source := range sources {
			argv := []string{getent, "-s", source, "passwd", strconv.Itoa(uid)}
			out, exit, err := lookup(ctx, lookupClock, argv...)
			if err != nil {
				return nil, err
			}
			switch exit {
			case 0:
			case 2:
				continue
			default:
				return nil, fmt.Errorf("%s: exit status %d", strings.Join(argv, " "), exit)
			}
			held, err := passwdLine(strings.TrimRight(string(out), "\n"), strings.Join(argv, " "))
			if err != nil {
				return nil, err
			}
			if !slices.Contains(names, held.name) {
				names = append(names, held.name)
			}
		}
		return names, nil
	}
	// groupNamed is a group by its name, as getent resolves it: the
	// hotserve group is the one the apps' env files and directories
	// belong to, whatever the hotserve user's primary group is.
	groupNamed = func(ctx context.Context, name string) (gid int, exists bool, err error) {
		out, exit, err := lookup(ctx, lookupClock, getent, "group", name)
		if err != nil {
			return 0, false, err
		}
		switch exit {
		case 0:
		case 2:
			return 0, false, nil
		default:
			return 0, false, fmt.Errorf("%s group %s: exit status %d", getent, name, exit)
		}
		fields := strings.Split(strings.TrimRight(string(out), "\n"), ":")
		if len(fields) < 3 {
			return 0, false, fmt.Errorf("%s group %s: not a group line: %s", getent, name, record.Text(string(out)))
		}
		gid, err = strconv.Atoi(fields[2])
		if err != nil {
			return 0, false, fmt.Errorf("%s group %s: not a group line: %s", getent, name, record.Text(string(out)))
		}
		return gid, true, nil
	}
	// groupsOf is every group the account is in, its primary among
	// them, as id(1) resolves them: what the manager gives a unit that
	// runs as the account.
	groupsOf = func(ctx context.Context, name string) ([]int, error) {
		out, exit, err := lookup(ctx, lookupClock, idProgram, "-G", name)
		if err != nil {
			return nil, err
		}
		if exit != 0 {
			return nil, fmt.Errorf("%s -G %s: exit status %d", idProgram, name, exit)
		}
		var gids []int
		for _, f := range strings.Fields(string(out)) {
			g, err := strconv.Atoi(f)
			if err != nil {
				return nil, fmt.Errorf("%s -G %s: not a gid: %s", idProgram, name, record.Text(f))
			}
			gids = append(gids, g)
		}
		// An account is in its own group at the least: no group is no
		// answer.
		if len(gids) == 0 {
			return nil, fmt.Errorf("%s -G %s: no group in what it said", idProgram, name)
		}
		return gids, nil
	}
	homeExists  = func(path string) bool { _, err := os.Lstat(path); return err == nil }
	makeAccount = func() error {
		if out, err := exec.Command(useradd[0], useradd[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %w: %s", useraddArgv(), err, record.Text(string(out)))
		}
		return nil
	}
)

// useradd makes the account restic runs as: no home, no shell, and
// nothing of its own but the cache the manager makes for it. The
// package's postinstall makes it with the same line, before setup ever
// runs (TestPostinstallMakesTheAccountAsSetupDoes).
var useradd = []string{"/usr/sbin/useradd", "--system", "--no-create-home", "--home-dir", "/nonexistent", "--shell", "/usr/sbin/nologin", backupUser}

const (
	getent    = "/usr/bin/getent"
	idProgram = "/usr/bin/id"
)

// lookupClock bounds one lookup in the account databases. They answer
// at once from a file, and from a directory in the time its own
// timeouts give it; a run holds the lock while it asks, so a directory
// that never answers is given up on, and said.
var lookupClock = 10 * time.Second

// enumerationClock bounds the one lookup that lists a whole database,
// which takes as long as the database is large: on a box joined to a
// directory of tens of thousands of accounts, ten seconds would refuse
// every run (the owner, 2026-09-27: a minute).
var enumerationClock = time.Minute

// readable is whether a file can be opened to be read, by whoever this
// command is; nothing of it is read.
var readable = func(path string) error {
	f, err := os.Open(path) //nolint:gosec // a constant path, opened and closed
	if err != nil {
		return err
	}
	return f.Close()
}

// shadowFile is the shadow database's own file.
var shadowFile = "/etc/shadow"

// lookup runs one program of the account databases — getent, id — with
// a constant argv, and returns what it printed and its exit status. The
// command ends with the context, and at the bound: an error is a
// command that could not be run or did not answer, never an exit
// status, which is the caller's to read.
var lookup = func(ctx context.Context, within time.Duration, argv ...string) (out []byte, exit int, err error) {
	bounded, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	cmd := exec.CommandContext(bounded, argv[0], argv[1:]...) //nolint:gosec // a constant program; the arguments are constants, a uid, or a source's name that matched sourceRe
	// What the command leaves behind holding its pipe is not waited for.
	cmd.WaitDelay = time.Second
	out, err = cmd.Output()
	var ended *exec.ExitError
	switch {
	case err == nil:
		return out, 0, nil
	case ctx.Err() != nil:
		return nil, 0, fmt.Errorf("%s: %w", strings.Join(argv, " "), ctx.Err())
	case bounded.Err() != nil:
		return nil, 0, fmt.Errorf("%s did not answer within %s", strings.Join(argv, " "), within)
	case errors.As(err, &ended) && ended.ExitCode() >= 0:
		return out, ended.ExitCode(), nil
	}
	return nil, 0, fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
}

// nsswitchFile is where the box says which sources answer for the
// account databases.
var nsswitchFile = "/etc/nsswitch.conf"

// passwdSources is the sources of the passwd database, in the order
// the box asks them. No file is glibc's default, files; a file that is
// there and cannot be read leaves the sources unknown, and refuses.
func passwdSources() ([]string, error) {
	raw, err := os.ReadFile(nsswitchFile)
	if errors.Is(err, fs.ErrNotExist) {
		return []string{"files"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("the sources of the passwd database could not be read: %w", err)
	}
	sources, err := sourcesOf(string(raw))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", nsswitchFile, err)
	}
	return sources, nil
}

// sourceRe is a source's name: what becomes libnss_<name>.so, and an
// argument of getent here. The file is root's; a name that could be
// read as an option is refused all the same.
var sourceRe = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// sourcesOf reads the passwd line out of an nsswitch.conf as glibc
// does: the last line for the database counts, "#" begins a comment,
// what stands in brackets is an action on the source before it and no
// source, and a source named twice is one. No line, or none with a
// source on it, is files.
func sourcesOf(conf string) ([]string, error) {
	var sources []string
	for _, line := range strings.Split(conf, "\n") {
		line, _, _ = strings.Cut(line, "#")
		database, rest, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(database) != "passwd" {
			continue
		}
		sources = nil
		for rest = strings.TrimSpace(rest); rest != ""; rest = strings.TrimSpace(rest) {
			if strings.HasPrefix(rest, "[") {
				_, after, closed := strings.Cut(rest, "]")
				if !closed {
					return nil, fmt.Errorf("the passwd line holds an action that is never closed: %s", record.Text(rest))
				}
				rest = after
				continue
			}
			source := rest
			if i := strings.IndexAny(rest, " \t["); i >= 0 {
				source = rest[:i]
			}
			rest = rest[len(source):]
			if !sourceRe.MatchString(source) {
				return nil, fmt.Errorf("the passwd line names a source that is no name: %s", record.Text(source))
			}
			if !slices.Contains(sources, source) {
				sources = append(sources, source)
			}
		}
	}
	if len(sources) == 0 {
		return []string{"files"}, nil
	}
	return sources, nil
}

// errNeedsRoot is a lookup that is root's alone to make.
var errNeedsRoot = errors.New("root's to read")

// isRoot is whether this command is root's: a variable so that a test
// can be either.
var isRoot = func() bool { return os.Geteuid() == 0 }

// shadowed is the account's password as the shadow database holds it:
// the second field of its entry, and whether there is an entry. The
// database is root's to read, and getent asked by anyone else answers
// "not found" for an entry that is there — so anyone else gets
// errNeedsRoot, never that the account has no password.
var shadowed = func(ctx context.Context, name string) (field string, found bool, err error) {
	if !isRoot() {
		return "", false, errNeedsRoot
	}
	out, exit, err := lookup(ctx, lookupClock, getent, "shadow", name)
	if err != nil {
		return "", false, err
	}
	switch exit {
	case 0:
	case 2:
		// "Not found" is believed only of a database that could be
		// read: getent says 2 as well where root could not open it — a
		// security module, a root that is one in name — and an account
		// with a password would pass as one with nothing to log in
		// with. No file at all is no database.
		if err := readable(shadowFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", false, fmt.Errorf("%s could not be read, so whether the %s account has a password is not known: %w", shadowFile, name, err)
		}
		return "", false, nil
	default:
		return "", false, fmt.Errorf("%s shadow %s: exit status %d", getent, name, exit)
	}
	fields := strings.Split(strings.TrimRight(string(out), "\n"), ":")
	if len(fields) < 2 {
		// Never shown: what is on the line may be a password's hash.
		return "", false, fmt.Errorf("%s shadow %s: not a shadow line", getent, name)
	}
	return fields[1], true, nil
}

// passwdLine reads one line of the passwd database, as from says it
// was asked for.
func passwdLine(line, from string) (passwd, error) {
	fields := strings.Split(line, ":")
	if len(fields) != 7 {
		return passwd{}, fmt.Errorf("%s: not a passwd line: %s", from, record.Text(line))
	}
	uid, uerr := strconv.Atoi(fields[2])
	gid, gerr := strconv.Atoi(fields[3])
	if uerr != nil || gerr != nil {
		return passwd{}, fmt.Errorf("%s: not a passwd line: %s", from, record.Text(line))
	}
	return passwd{name: fields[0], password: fields[1], uid: uid, gid: gid, home: fields[5], shell: fields[6], exists: true}, nil
}

// passwd is an account as getent gives it: its uid and gid, its home
// and its shell, and whether it is there at all.
type passwd struct {
	name        string
	password    string // the second field: "x" where the shadow database holds it
	uid, gid    int
	home, shell string
	exists      bool
}

func useraddArgv() string { return strings.Join(useradd, " ") }

// accountUsable is what setup asks of an account that is already there:
// a shell nobody can log in with, no password, and no home that exists
// — and that nobody else is it. It is a guard against an account of
// this name that is on the box for another reason, which someone who is
// not root has been given a way to use: setup would otherwise take it
// by its name, and hand that someone the repository. Every restic
// unit runs as it, and whoever can log in as it can read the repository
// credential from a running restic's environment. An account made
// wrongly is refused, never changed: an administrator may have meant
// it, and the message says the two ways to mend it.
func accountUsable(ctx context.Context, acct passwd) error {
	// Two kinds of fault, mended differently. Who the account is: it
	// is what every restic unit runs as, User= by name, and
	// authorization is by uid — root's is root, and any other account
	// holding the uid is the restic process and reads its environment
	// — and by group: the manager gives a unit its account's groups,
	// and in the hotserve group the plan unit and restic read the
	// apps' env files and their 0750 directories, the separation the
	// account exists for; in root's, what root's group reads. (A gid
	// of its own is not asked for: a group reads no process's
	// environment.)
	var identity, lockable, mended []string
	if acct.uid == 0 {
		identity = append(identity, "uid 0 (root)")
	}
	if acct.gid == 0 {
		identity = append(identity, "gid 0 (root)")
	}
	names, err := holders(ctx, acct.uid)
	if err != nil {
		return err
	}
	var others []string
	for _, n := range names {
		if n != backupUser {
			others = append(others, record.Text(n))
		}
	}
	if len(others) > 0 && acct.uid != 0 {
		identity = append(identity, fmt.Sprintf("a uid shared with %s (%d)", strings.Join(others, ", "), acct.uid))
	}
	groups, err := groupsOf(ctx, backupUser)
	if err != nil {
		return err
	}
	if acct.gid != 0 && slices.Contains(groups, 0) {
		identity = append(identity, "root's group among its groups (gid 0)")
	}
	// The group named hotserve — what the apps' env files and their
	// directories belong to — not the hotserve user's primary group,
	// which on a box where that account was made by hand is another.
	gid, there, err := groupNamed(ctx, dataUser)
	if err != nil {
		return err
	}
	if there && gid != 0 && (acct.gid == gid || slices.Contains(groups, gid)) {
		identity = append(identity, fmt.Sprintf("the %s group among its groups (gid %d)", dataUser, gid))
	}
	// What can be locked. The password: the shell refuses whoever logs
	// in with one, and an sshd that serves sftp itself runs no shell —
	// with the password it read a unit's environment out of /proc
	// [M70]. "x" says the shadow database holds it, and no entry there
	// is nothing to log in with; any other field is the password
	// itself, as a directory's "*" is. Locked is "!" or "*" first,
	// which is how useradd --system leaves it.
	//
	// Asked by someone who is not root, the shadow database cannot be
	// read: what else is wrong is said all the same, with that beside
	// it, and an account with nothing else wrong is not called right.
	const unseen = "whether it has a password is root's to read, and was not looked at: sudo hotserve-backup account"
	password, looked := acct.password, true
	if password == "x" {
		field, found, err := shadowed(ctx, backupUser)
		switch {
		case errors.Is(err, errNeedsRoot):
			password, looked = "!", false
		case err != nil:
			return err
		case !found:
			password = "!"
		default:
			password = field
		}
	}
	var withPassword bool
	switch {
	case password == "":
		lockable, withPassword = append(lockable, "no password at all, so that anyone logs in as it"), true
	case !strings.HasPrefix(password, "!") && !strings.HasPrefix(password, "*"):
		lockable, withPassword = append(lockable, "a password that is not locked"), true
	}
	if withPassword {
		mended = append(mended, "the password")
	}
	// The shell, which is what login runs — the paths known to refuse a
	// login, not a name (a copy of bash at /tmp/nologin is a login
	// shell) — and the home.
	switch acct.shell {
	case "/usr/sbin/nologin", "/sbin/nologin", "/bin/false", "/usr/bin/false":
	case "": // no shell set: login gives /bin/sh
		lockable, mended = append(lockable, "a login shell (none set, which is /bin/sh)"), append(mended, "the shell")
	default:
		lockable, mended = append(lockable, fmt.Sprintf("a login shell (%s, not one of /usr/sbin/nologin, /sbin/nologin, /bin/false, /usr/bin/false)", record.Text(acct.shell))), append(mended, "the shell")
	}
	// /nonexistent is the home setup gives; a directory of that name is
	// nobody's home, and refusing it would refuse setup's own account.
	if acct.home != "/nonexistent" && homeExists(acct.home) {
		lockable, mended = append(lockable, fmt.Sprintf("a home directory that exists (%s)", record.Text(acct.home))), append(mended, "the home")
	}
	if len(identity)+len(lockable) == 0 {
		if !looked {
			return fmt.Errorf("the %s account has nothing wrong that can be seen without root; %s", backupUser, unseen)
		}
		return nil
	}
	// The remedy that fits the fault: locking mends the shell and the
	// home, and nothing of who the account is.
	lock := "lock it (usermod --shell /usr/sbin/nologin --home /nonexistent " + backupUser + ")"
	if withPassword {
		lock = "lock it (usermod --lock --shell /usr/sbin/nologin --home /nonexistent " + backupUser + ")"
	}
	remake := "remove it (userdel " + backupUser + ") and make it as setup would (" + useraddArgv() + ")"
	var remedy string
	switch {
	case len(identity) == 0:
		remedy = lock + ", or " + remake
	case len(lockable) == 0:
		remedy = remake + "; nothing short of that changes who the account is"
	default:
		// Said of the faults the account has: the password, the shell,
		// the home.
		what := mended[0]
		if n := len(mended); n > 1 {
			what = strings.Join(mended[:n-1], ", ") + " and " + mended[n-1]
		}
		remedy = remake + ", which mends all of it; to " + lock + " would mend " + what + " alone"
	}
	if !looked {
		remedy += "; " + unseen
	}
	return fmt.Errorf("the %s account exists with %s: every restic unit runs as it, with its groups, and whoever can log in as it — or is it — can read the repository credential from restic's environment; %s",
		backupUser, strings.Join(append(identity, lockable...), " and "), remedy)
}

// AccountReady is accountReady for the `account` command: what the
// package's postinstall asks, so that its warning is setup's own words,
// and what an administrator asks after mending the account.
func AccountReady(ctx context.Context) error { _, err := accountReady(ctx); return err }

// accountReady is what a run, a restore and a drill ask of the account
// restic runs as, before any unit: that it is there, and that nobody
// else can be it — what setup asks of one that exists, held at every
// run, since it is the run that puts the credential in that account's
// environment. Not there: a box with a credential file written by
// hand and no account, whose units would end 217/USER.
//
// The account it returns is the one that was looked at: what a fetch
// gives its directory to, with no second lookup for a directory to
// answer otherwise.
func accountReady(ctx context.Context) (passwd, error) {
	acct, err := account(ctx, backupUser)
	if err != nil {
		return passwd{}, err
	}
	if !acct.exists {
		return passwd{}, fmt.Errorf("the %s account is not there: sudo hotserve-backup setup <repository> makes it (%s)", backupUser, useraddArgv())
	}
	return acct, accountUsable(ctx, acct)
}

// programs are what the units run, and where. A unit whose command is
// not there ends 203/EXEC, which says nothing of what to install: setup
// and a run each refuse first, in the same words.
func programsInstalled(cfg Config) error {
	for _, p := range []struct{ name, path, fix string }{
		{"restic", cfg.Restic, ": apt install restic"},
		{"sqlite3", dump.Program(), ": apt install sqlite3"},
		{"hotserve", plan.Program(), ""},
		{"hotserve-backup", cfg.Self, ", where the units run it"},
	} {
		if !haveProgram(p.path) {
			return fmt.Errorf("%s is not installed at %s%s", p.name, p.path, p.fix)
		}
	}
	return nil
}

// errByHand marks a backend the run can use and this command does not
// set up: the file is written by hand.
var errByHand = errors.New("not set up by this command")

// credential is one thing a backend takes from the environment.
type credential struct {
	label, variable string
	secret          bool
}

// backendOf says what the repository's backend needs typed, or why the
// box cannot use it. Only backends that take their credentials from the
// environment are usable at all [handover]; of those, azure: is not in
// Debian's restic [measured], and gs: and swift: take a credentials
// file or a dozen variables, which this command does not ask for.
func backendOf(repo, envFile string) ([]credential, error) {
	scheme, rest, ok := strings.Cut(repo, ":")
	switch {
	case repo == "":
		return nil, errors.New("no repository was given")
	case !ok || strings.HasPrefix(repo, "/") || strings.HasPrefix(repo, ".") || scheme == "local":
		return nil, errors.New("a path on this box is not a repository: a backup must survive losing the box")
	}
	switch scheme {
	case "sftp":
		return nil, errors.New("sftp: is not supported: ssh takes its key and known_hosts from a home directory, which no backup unit has")
	case "rclone":
		return nil, errors.New("rclone: is not supported: rclone takes its remotes from a config file, which no backup unit has")
	case "azure":
		return nil, errors.New("azure: is not a backend Debian's restic has")
	case "gs", "swift", "rest":
		return nil, fmt.Errorf("rest:, gs: and swift: are %w: write %s by hand (see the README)", errByHand, envFile)
	case "s3":
		if rest == "" {
			return nil, fmt.Errorf("%s: names no bucket", scheme)
		}
		// restic takes the host with or without a scheme (s3:host/bucket):
		// what is looked at is the authority either way, not what
		// url.Parse makes of "user:pass@host" with no scheme before it.
		authority := strings.TrimPrefix(rest, "//") // s3://host/bucket: restic's other form
		if i := strings.Index(rest, "://"); i >= 0 {
			authority = rest[i+3:]
		}
		authority, _, _ = strings.Cut(authority, "/")
		if strings.Contains(authority, "@") {
			return nil, errors.New("credentials in the repository URL are on the command line and in the shell's history; give the URL without them, and type them when asked")
		}
		return []credential{{"Storage key id", "AWS_ACCESS_KEY_ID", false}, {"Storage secret key", "AWS_SECRET_ACCESS_KEY", true}}, nil
	case "b2":
		if rest == "" {
			return nil, errors.New("b2: names no bucket")
		}
		return []credential{{"Storage key id", "B2_ACCOUNT_ID", false}, {"Storage secret key", "B2_ACCOUNT_KEY", true}}, nil
	}
	return nil, fmt.Errorf("%q is not a repository restic knows (s3: or b2:)", scheme+":")
}

// RepositoryUsable says why the box could not use the repository the
// credential file names, or nil: what status says of a hand-edited
// file. A backend setup does not ask for, the run still uses.
func RepositoryUsable(repo, envFile string) error {
	if err := repositoryValue(repo); err != nil {
		return err
	}
	_, err := backendOf(repo, envFile)
	if errors.Is(err, errByHand) {
		return nil
	}
	return err
}

// repositoryValue is what setup refuses of the URL as a value, before
// it looks at it as a URL: what a prompt would refuse of any value,
// and whitespace at an end, which the manager trims of a plain value
// and keeps of a quoted one.
func repositoryValue(repo string) error {
	if strings.TrimSpace(repo) != repo {
		return errors.New("the repository URL has whitespace at an end")
	}
	if err := envfile.Refuse(repo); err != nil {
		return fmt.Errorf("the repository URL cannot go in the file: %w", err)
	}
	return nil
}

// OldEnvFileNote is what a run and status add when the credential file
// is missing and one from an earlier version of this branch is there:
// it is not read, and setup is what to run. Empty otherwise.
func OldEnvFileNote(cfg Config) string {
	if _, err := os.Lstat(cfg.OldEnvFile); cfg.OldEnvFile == "" || err != nil {
		return ""
	}
	return fmt.Sprintf(" (%s is from before this version and is not read: run `sudo hotserve-backup setup <its RESTIC_REPOSITORY>`, which asks for its RESTIC_PASSWORD; then remove it)", cfg.OldEnvFile)
}

// setup is one setup under way: the run whose lock it holds, the
// operator, the repository, the file being written beside the working
// one, and what has become of the password shown, where one was.
type setup struct {
	*run
	term Terminal
	repo string
	// staged is the file beside the working one, which the units here
	// read, and which takes the working one's place at the end.
	staged string
	// password is the one made for a new repository: shown once, and
	// kept through a key typed wrong — it has taken effect nowhere
	// until init has made the repository with it. Empty until shown.
	password string
	// initRan is whether restic init has been started with password
	// at all: from then on no failure proves it unused, and it is never
	// said to be. made is more: the repository has that password for
	// certain — init made it, or it opened one an earlier init made.
	initRan, made bool
	// initLeft is whether init was left running — by its clock or an
	// interrupt — and so still has the staged file and the run
	// directory to read: the manager opens both in the unit's own first
	// moments, which the end of this command may come before. Both are
	// then left for it, and swept by the next lock holder after its wait.
	initLeft bool
}

// Setup makes the box ready to back up into one repository: the
// account, the directories, and the credential file — written whole and
// put in place only once the repository has answered. Everything that
// can be known to fail is checked before anyone is asked for a secret;
// the password of a new repository is shown, and confirmed stored,
// before it takes effect anywhere.
func Setup(ctx context.Context, cfg Config, r Runner, o SetupOptions) (*SetupReport, error) {
	term := o.Terminal
	creds, err := backendOf(o.Repository, cfg.EnvFile)
	if err != nil {
		return nil, err
	}
	// The one value not typed at a prompt: refused here for what a
	// prompt would refuse, before anyone is asked for anything.
	if err := repositoryValue(o.Repository); err != nil {
		return nil, err
	}
	if err := programsInstalled(cfg); err != nil {
		return nil, err
	}
	if v, err := r.ManagerVersion(ctx); err != nil {
		return nil, err
	} else if v < 257 {
		return nil, fmt.Errorf("systemd 257 or later is needed (Debian 13's); this box has %d", v)
	}
	rep := &SetupReport{Account: "present"}
	acct, err := account(ctx, backupUser)
	if err != nil {
		return nil, err
	}
	if acct.exists {
		if err := accountUsable(ctx, acct); err != nil {
			return nil, err
		}
	} else {
		if err := makeAccount(); err != nil {
			return nil, fmt.Errorf("making the %s account: %w", backupUser, err)
		}
		rep.Account = "made"
	}
	term.Say("account " + backupUser + ": " + rep.Account)

	x, end, err := open(ctx, cfg, r, term.Say)
	if x == nil {
		return nil, err
	}
	defer end()
	s := &setup{run: x, term: term, repo: o.Repository, staged: envfile.Staged(cfg.EnvFile)}
	if err != nil {
		return nil, s.err(err)
	}
	dir := filepath.Dir(cfg.EnvFile)
	// Anyone may see that the file is there and when it was written —
	// that is how status tells a fresh setup from one that never ran —
	// and nobody but root what is in it: root's own directory, whoever
	// made it before (a directory's writer may rename or remove what is
	// in it), and not a link, which would put the file somewhere else.
	if st, err := os.Lstat(dir); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a link, and the credential file has to be in a directory of root's own", dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // holds one root-only file
		return nil, err
	}
	if err := ownByRoot(dir); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // said again past the caller's umask
		return nil, err
	}
	// What a setup that did not live to remove them left was swept by
	// open, after the wait for its init, and said.
	rep.Swept = x.swept
	// Whether there was a file before: with none, no run could have
	// written the record against this repository. What it names is not
	// read — the record's repository is known by its id, below.
	hadOld := false
	if _, err := os.Lstat(cfg.EnvFile); err == nil {
		hadOld = true
	}

	p, err := x.planWith(ctx, filepath.Join(x.dir, "plan.err"))
	if err != nil {
		return nil, s.err(err)
	}
	rep.Apps = p.Names()
	if len(rep.Apps) == 0 {
		term.Say("no app declares a backup yet: a run would back nothing up")
	} else {
		term.Say(fmt.Sprintf("a run would back up %s, under %s", record.Clean(strings.Join(rep.Apps, ", ")), record.Text(p.Root)))
	}

	// The file is written beside the working one, staged under a name
	// the manager reads for the units here, and takes the working one's
	// place only once the repository has answered. Whatever ends this
	// command before that, the working file is as it was and the staged
	// file goes with it — unless init was left running to read it, in
	// which case the next setup removes it. Until a password is made it
	// holds a throwaway one: what the look for a repository needs, and
	// nothing that takes effect anywhere.
	defer func() {
		if !s.initLeft {
			os.Remove(s.staged) //nolint:errcheck,gosec // gone already once it was put in place
		}
	}()
	throwaway, err := newPassword()
	if err != nil {
		return nil, err
	}
	exists := false
	// A repository that is there has a password of its own, which is
	// asked for below; one shown here is not it, and init, where it
	// ran, made nothing with it.
	needsOwn := func() {
		exists, s.initRan = true, false
		msg := "the repository exists; its password is needed"
		if s.password != "" {
			msg += " (the one shown above is not it)"
		}
		term.Say(msg)
	}
	pairs := []envfile.Pair{{Key: "RESTIC_REPOSITORY", Value: o.Repository}, {Key: "RESTIC_PASSWORD"}}
	for attempt := 1; ; attempt++ {
		// Whether an earlier attempt's init ran with the shown password:
		// a repository found there, by the look or by this attempt's
		// init, may be one that init made.
		ranBefore := s.initRan
		pairs = pairs[:2]
		for _, c := range creds {
			v, err := s.ask(ctx, c.label+" ("+c.variable+"): ", c.secret)
			if err != nil {
				return nil, s.err(err)
			}
			pairs = append(pairs, envfile.Pair{Key: c.variable, Value: v})
		}
		pairs[1].Value = throwaway
		if err := writeEnv(s.staged, pairs); err != nil {
			return nil, s.err(err)
		}
		// Before any password is made: is there a repository already?
		// With a right key restic says at once — none (10), or one this
		// password does not open (12) — and a rebuilt box is then asked
		// for the password it has, not shown one it does not need.
		// Where the look cannot tell, init answers, and says why.
		term.Say(fmt.Sprintf("looking for a repository at %s (up to %s)", o.Repository, setupProbeClock))
		found, err := s.look(ctx)
		if err != nil {
			return nil, s.err(err)
		}
		switch {
		case found == lookFound && ranBefore:
			// A repository is there, and an earlier attempt's init ran
			// with the password shown: the throwaway not opening it says
			// nothing of that one. Tried first — a repository it opens is
			// one this setup made.
			pairs[1].Value = s.password
			if err := writeEnv(s.staged, pairs); err != nil {
				return nil, s.err(err)
			}
			if err := s.tryShown(ctx, rep, needsOwn); err != nil {
				return nil, s.err(err)
			}
		case found == lookFound:
			needsOwn()
		case found == lookNone:
			// None: one is made below.
		case s.password != "":
			term.Say(fmt.Sprintf("no repository answered within %s: a bucket not made yet, a wrong key and a wrong host look alike here; a new repository will be made, and the password shown above still applies", setupProbeClock))
		default:
			term.Say(fmt.Sprintf("no repository answered within %s: a bucket not made yet, a wrong key and a wrong host look alike here; a new repository will be made with the password shown next", setupProbeClock))
		}
		if exists || rep.New {
			break
		}
		if s.password == "" {
			if s.password, err = newPassword(); err != nil {
				return nil, err
			}
			term.Say("Repository password (new): " + s.password)
			term.Say("Store it off the box now, with the repository URL, " + o.Repository + ", and the storage key: with those three, `restic -r <url>` reads every backup from any machine; without the password nothing can.")
			if err := s.confirmStored(ctx); err != nil {
				return nil, s.err(err)
			}
		}
		pairs[1].Value = s.password
		if err := writeEnv(s.staged, pairs); err != nil {
			return nil, s.err(err)
		}
		s.initRan = true
		o1, message, err := s.makeRepository(ctx)
		if err != nil {
			return nil, s.err(err)
		}
		if o1.OK() {
			// Exit 0 is a repository made, with the password that was
			// shown: whatever restic did not say, that password is now
			// the repository's.
			s.made = true
			if rep.RepositoryID = initializedID(filepath.Join(x.dir, "init.out")); rep.RepositoryID == "" {
				return nil, s.err(errors.New("restic made the repository and exited 0 but said nothing of its id; run setup again, which opens it"))
			}
			rep.New = true
			break
		}
		if o1.Result == "exit-code" && o1.ExitStatus == 1 && repositoryExists(message) {
			// The look could not tell, and init can: the repository has a
			// password already. This init made nothing with the one
			// shown; an earlier attempt's may have, and is tried first,
			// as above. Otherwise it is unused again, whatever ends the
			// opening that follows.
			if ranBefore {
				if err := s.tryShown(ctx, rep, needsOwn); err != nil {
					return nil, s.err(err)
				}
			} else {
				needsOwn()
			}
			break
		}
		var failure error
		if o1.Result == "exit-code" && o1.ExitStatus == 1 {
			failure = errors.New("restic could not make or open the repository (exit 1): " + record.Text(message))
			// What the storage says of the key, the host or the bucket is
			// a mistake in what was typed, as often as not: the key is
			// asked for again. The password stands: it has taken effect
			// nowhere.
			if attempt < 3 {
				term.Say(failure.Error())
				term.Say("the storage refused the key, or could not be reached: the key id and secret again (the password shown above still applies)")
				continue
			}
		} else {
			detail, _ := resticFailure(o1)
			failure = errors.New(detail)
		}
		return nil, s.err(failure)
	}
	if exists {
		for attempt := 1; ; attempt++ {
			own, err := s.ask(ctx, "Repository password: ", true)
			if err != nil {
				return nil, s.err(err)
			}
			pairs[1].Value = own
			if err := writeEnv(s.staged, pairs); err != nil {
				return nil, s.err(err)
			}
			id, opened, err := s.openRepository(ctx)
			if err != nil {
				return nil, s.err(err)
			}
			if opened {
				rep.RepositoryID = id
				break
			}
			if attempt < 3 {
				term.Say("this password cannot open the repository (exit 12): again")
				continue
			}
			return nil, s.err(errors.New("this password cannot open the repository (exit 12)"))
		}
	}
	// The record speaks of the repository it was written against,
	// known by its id: the one setup keeps beside it (`repository-id`)
	// for the file it puts in place. A URL says only where — a bucket
	// emptied and made again is another repository at the same one,
	// and one reached by another URL is the same. With another
	// repository now in use — or none known, or one this setup made —
	// the record is put aside, so that the next run drills what it
	// backs up into the repository it is now using. Aside first, then
	// the file: whatever ends setup between the two leaves no credential
	// file with a record of another repository beside it (a record aside
	// with the old file in place is a fresh start, which the next run
	// mends); a file that could not be put in place gets its record
	// back.
	statusPath := filepath.Join(cfg.StateDir, "status.json")
	idFile := filepath.Join(cfg.StateDir, "repository-id")
	prevID := ""
	if raw, err := os.ReadFile(idFile); err == nil { //nolint:gosec // root's own file under the state directory
		prevID = strings.TrimSpace(string(raw))
	}
	why := ""
	if _, err := os.Lstat(statusPath); err == nil {
		switch {
		case rep.New:
			why = "the repository was made by this setup, so it holds none of the record's snapshots"
		case !hadOld:
			why = "no credential file was there to tie it to this repository"
		case prevID == "":
			why = "no setup recorded which repository it was written against"
		case prevID != rep.RepositoryID:
			why = fmt.Sprintf("it was written against another repository (id %s)", short(prevID))
		}
	}
	// On the disk before the file is — the two live in different
	// directories, and a power cut must not keep the new file and lose
	// the aside — and so is a put-back. Whatever fails, the two files
	// are left agreeing: the record comes back while the old file is
	// still in place, and stays aside once the new one is.
	putBack := func(cause error) error {
		if back := os.Rename(rep.Aside, statusPath); back != nil {
			return fmt.Errorf("%w; and the record, put aside first, could not be put back from %s: %w", cause, rep.Aside, back)
		}
		rep.Aside = ""
		if sync := syncDir(cfg.StateDir); sync != nil {
			return fmt.Errorf("%w; and the record, put back, could not be synced: %w", cause, sync)
		}
		return cause
	}
	if why != "" {
		// Named by the time and this setup's nonce, so that no aside
		// replaces another: two in one second, or a clock set back.
		rep.Aside = statusPath + ".aside-" + time.Now().UTC().Format("20060102T150405Z") + "-" + s.nonce
		if err := os.Rename(statusPath, rep.Aside); err != nil {
			return nil, s.err(err)
		}
		if err := syncDir(cfg.StateDir); err != nil {
			return nil, s.err(putBack(err))
		}
	}
	if err := commit(s.staged, cfg.EnvFile); err != nil {
		if why != "" {
			return nil, s.err(putBack(err))
		}
		return nil, s.err(err)
	}
	// The file on the disk first, then the id: a power cut must never
	// leave the new id beside the old file, or a record the next run
	// writes against the old repository would pass for the new one's
	// — the old id beside the new file, at worst, which puts the record
	// aside once more, a fresh start the next run mends. From here the
	// file is in place for every reader, and stays; what fails after is
	// said with the record's whereabouts, and the password's fate from
	// what setup knows.
	inPlace := "the credential file is in place"
	if rep.Aside != "" {
		inPlace += "; the record was put aside: " + rep.Aside
	}
	if err := syncDir(dir); err != nil {
		return nil, s.err(fmt.Errorf("%s; its directory could not be synced to the disk, and the repository's id was not recorded beside the record, so the next setup puts the record aside: %w", inPlace, err))
	}
	if err := writeID(idFile, rep.RepositoryID); err != nil {
		return nil, s.err(fmt.Errorf("%s; the repository's id could not be kept beside the record, so the next setup puts the record aside: %w", inPlace, err))
	}
	if err := syncDir(cfg.StateDir); err != nil {
		return nil, s.err(fmt.Errorf("%s; the state directory could not be synced to the disk: %w", inPlace, err))
	}
	if why != "" {
		term.Say(fmt.Sprintf("the record was put aside (%s): %s; the next run drills what it backs up", why, rep.Aside))
	}
	kind := "existing"
	if rep.New {
		kind = "new"
	}
	term.Say(fmt.Sprintf("repository ready: %s (%s, id %s)", o.Repository, kind, short(rep.RepositoryID)))
	if s.password != "" && !rep.New {
		term.Say("the password shown above was never used: discard it")
	}
	// The file from before this version, if it is still there, is
	// where the credential was reachable from: said, not removed — it
	// is root's, and an operator may want its contents once more.
	if _, err := os.Lstat(cfg.OldEnvFile); cfg.OldEnvFile != "" && err == nil {
		rep.OldEnvFile = cfg.OldEnvFile
		term.Say(fmt.Sprintf("%s is still there, from before this version, and is not read; an administrator's sudoers may reach it: remove it (sudo rm %s)", cfg.OldEnvFile, cfg.OldEnvFile))
	}
	return rep, nil
}

// leaveInit is init left running, whichever way: the staged file it
// reads and the run directory it writes to stay, since the manager
// opens both in the unit's own first moments, which this end may come
// before. The next lock holder waits for it, then sweeps both.
func (s *setup) leaveInit() { s.initLeft, s.keepDir = true, true }

// tryShown opens the repository with the password shown — the staged
// file holds it — after an earlier attempt's init ran with it: one it
// opens is one this setup made; else the repository's own is needed.
func (s *setup) tryShown(ctx context.Context, rep *SetupReport, needsOwn func()) error {
	id, opened, err := s.openRepository(ctx)
	if err != nil {
		return err
	}
	if opened {
		s.term.Say("the repository exists, and the password shown above opens it: it was made by an earlier attempt of this setup")
		rep.RepositoryID, rep.New, s.made = id, true, true
		return nil
	}
	needsOwn()
	return nil
}

// confirmStored has the operator type "stored" — a word that means
// something, not "y" — with one more asking for a word that is not
// it; anything then is a no.
func (s *setup) confirmStored(ctx context.Context) error {
	for i := 0; i < 2; i++ {
		said, err := s.term.Ask(ctx, "Type stored to go on: ", false)
		if err != nil {
			return fmt.Errorf("nobody answered: %w", err)
		}
		if strings.EqualFold(strings.TrimSpace(said), "stored") {
			return nil
		}
		if i == 0 {
			s.term.Say("that is not stored: type stored to go on, or anything else to stop")
		}
	}
	return errors.New("the password was not confirmed stored: nothing was written")
}

// err is an error ending setup, in the operator's words: an interrupt
// is said as one, and what became of the password shown, where one
// was, is said with it — nothing here returns an error after the
// showing but through this.
func (s *setup) err(err error) error {
	fate := ""
	switch {
	case s.password != "" && s.made:
		fate = "; the password shown above is the repository's: keep it"
	case s.password != "" && s.initRan:
		// Started, init may have written some or all of a repository
		// before it failed or was left running: nothing here proves the
		// password unused, so it is kept.
		fate = "; keep the password shown above: restic init ran with it, and may have made the repository; run setup again, which looks first and asks for it if the repository is there"
	case s.password != "":
		fate = "; the password shown above was never used: discard it"
	}
	if errors.Is(err, context.Canceled) {
		return interrupted("interrupted: nothing has been written" + fate)
	}
	if fate != "" {
		return fmt.Errorf("%w%s", err, fate)
	}
	return err
}

// interrupted is context.Canceled in the operator's words: it is that
// error to errors.Is, and says nothing of contexts.
type interrupted string

func (e interrupted) Error() string      { return string(e) }
func (interrupted) Is(target error) bool { return target == context.Canceled }

// ask asks up to three times for a value the file can hold, and says
// each time why it cannot.
func (s *setup) ask(ctx context.Context, prompt string, secret bool) (string, error) {
	for i := 0; i < 3; i++ {
		v, err := s.term.Ask(ctx, prompt, secret)
		if err != nil {
			return "", fmt.Errorf("nobody answered: %w", err)
		}
		why := envfile.Refuse(v)
		if why == nil {
			return v, nil
		}
		s.term.Say(fmt.Sprintf("that cannot go in the file: %v; again", why))
	}
	return "", errors.New("no usable value was given in three times")
}

// commit puts the file in place — the rename alone; its directory is
// synced by the caller, which has to know which of the two failed —
// and writeEnv writes the file beside it. Variables so that a test
// can look at the moment each happens, and make it fail.
var (
	commit   = os.Rename
	writeEnv = envfile.Write
)

// ownByRoot makes a directory root's, and syncDir puts a directory's
// entries on the disk; variables so that the flow can be tested by an
// account that is not root.
var (
	ownByRoot = func(path string) error { return os.Lchown(path, 0, 0) }
	syncDir   = envfile.SyncDir
	// writeID keeps the repository's id beside the record: whole, on
	// the disk before it returns, and for everyone to read whatever the
	// caller's umask, as the record is — nothing a password guards.
	writeID = func(path, id string) error {
		tmp := path + ".new"
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644) //nolint:gosec // an id, beside status.json
		if err != nil {
			return err
		}
		write := func() error {
			if _, err := f.WriteString(id + "\n"); err != nil {
				return err
			}
			if err := f.Chmod(0o644); err != nil {
				return err
			}
			return f.Sync()
		}
		if err := write(); err != nil {
			f.Close()      //nolint:errcheck,gosec // the write's error is the one
			os.Remove(tmp) //nolint:errcheck,gosec // as above
			return err
		}
		if err := f.Close(); err != nil {
			os.Remove(tmp) //nolint:errcheck,gosec // the close's error is the one
			return err
		}
		return os.Rename(tmp, path)
	}
)

// lookResult is what the look for a repository found.
type lookResult int

const (
	lookUnsure lookResult = iota // restic could not tell, or the clock: init decides
	lookNone                     // no repository there (exit 10)
	lookFound                    // one there, which the throwaway does not open (exit 12)
)

// look asks whether a repository is there, with the throwaway password,
// under its own short clock. Only two answers of the unit say the look
// could not tell — restic's exit 1 (the storage retrying) and the
// clock — and those fall through to init; any other failure is the
// unit's own, and ends setup before any password is made.
func (s *setup) look(ctx context.Context) (lookResult, error) {
	o, _, err := s.repository(ctx, "probe", setupProbeClock, s.cfg.Restic, "cat", "config", "--no-lock")
	if err != nil {
		if errors.Is(err, errDidNotAnswer) {
			return lookUnsure, nil
		}
		return lookUnsure, err
	}
	switch {
	case o.Result == "exit-code" && o.ExitStatus == 12:
		return lookFound, nil
	case o.Result == "exit-code" && o.ExitStatus == 10:
		return lookNone, nil
	case o.OK() || o.Result == "exit-code" && o.ExitStatus == 1:
		return lookUnsure, nil
	}
	detail, _ := resticFailure(o)
	return lookUnsure, errors.New("looking for the repository: " + detail)
}

// makeRepository runs restic init. This is the one unit here that is
// never stopped: stopped half way it leaves a repository with a config
// and no key, which no password opens, where left to its few seconds it
// makes the repository with the password that was shown. Its clock, and
// an interrupt, end the waiting, not the unit: it is left running,
// recorded for the next lock holder to wait for, and said so.
func (s *setup) makeRepository(ctx context.Context) (unit.Outcome, string, error) {
	return s.repository(ctx, "init", setupClock, s.cfg.Restic, "init", "--json")
}

// openRepository opens the repository with the password the file holds
// (restic cat config): the repository's id and true, or false where the
// password does not open it (exit 12); anything else is an error, in
// restic's words where it has any.
func (s *setup) openRepository(ctx context.Context) (id string, opened bool, err error) {
	o, message, err := s.repository(ctx, "open", setupClock, s.cfg.Restic, "cat", "config", "--no-lock")
	if err != nil {
		return "", false, err
	}
	switch {
	case o.OK():
		if id = configID(filepath.Join(s.dir, "open.out")); id == "" {
			return "", false, errors.New("restic exited 0 but said nothing of the repository's id")
		}
		return id, true, nil
	case o.Result == "exit-code" && o.ExitStatus == 12:
		return "", false, nil
	case o.Result == "exit-code" && o.ExitStatus == 1:
		return "", false, errors.New("restic could not open the repository (exit 1): " + record.Text(message))
	}
	detail, _ := resticFailure(o)
	return "", false, errors.New(detail)
}

// errDidNotAnswer is a unit given up on at its clock.
var errDidNotAnswer = errors.New("the repository did not answer")

// repository runs one restic command against the repository as the
// backup account, with the credential file the manager reads, under a
// clock; a person waiting is told what for. It returns what restic
// said on stderr, cleaned — the message of its exit_error line where it
// wrote one — for the caller to match and show.
//
// The look and the opening only read, and are stopped at their clock
// and on an interrupt. Init is not (see makeRepository): the clock and
// an interrupt end the waiting, the unit runs on, and the error names
// it.
func (s *setup) repository(ctx context.Context, role string, within time.Duration, argv ...string) (unit.Outcome, string, error) {
	clock, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	// A person waiting is told what for, and what Ctrl-C would do:
	// nothing, for the look and the opening, which only read; for init,
	// leave a unit running that may be making the repository with the
	// password shown. Said under the terminal's own lock, and waited
	// for if it is mid-sentence when the unit ends.
	noteDone := make(chan struct{})
	note := time.AfterFunc(setupNote, func() {
		defer close(noteDone)
		if role == "init" {
			s.term.Say("still waiting for " + s.repo + " (Ctrl-C leaves restic init running, and the next setup waits for it; it may be making the repository with the password shown: keep it)")
			return
		}
		s.term.Say("still waiting for " + s.repo + " (Ctrl-C is safe: nothing has been written)")
	})
	defer func() {
		if !note.Stop() {
			<-noteDone
		}
	}()
	errFile := filepath.Join(s.dir, role+".err")
	spec := unit.Spec{
		Name: s.name(role, ""), Description: "hotserve backup: " + role + " the repository",
		Argv: argv,
		User: backupUser, Network: true, EnvironmentFile: s.staged,
		Environment:    []string{"RESTIC_CACHE_DIR=/var/cache/hotserve-backup", "HOME=/nonexistent"},
		CacheDirectory: "hotserve-backup",
		StdoutFile:     filepath.Join(s.dir, role+".out"), StderrFile: errFile,
	}
	if role != "init" {
		o, err := s.start(clock, spec)
		if err != nil {
			if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, unit.ErrNotConfirmedGone) {
				return o, "", fmt.Errorf("%w within %s; the unit was stopped", errDidNotAnswer, within)
			}
			return o, "", err
		}
		return o, resticMessage(errFile), nil
	}
	// Init: recorded for the next lock holder to wait for, not to stop
	// (the record goes once it was seen to end), and run on a context
	// nothing here cancels. The clock and an interrupt end this wait alone.
	marker := filepath.Join(s.cfg.RunDir, "init-unit")
	if err := os.WriteFile(marker, []byte(spec.Name+"\n"), 0o600); err != nil {
		return unit.Outcome{}, "", err
	}
	// Not bound to setup's own service, where it has one: BindsTo=
	// would have the manager stop it as setup ends.
	type ended struct {
		o   unit.Outcome
		err error
	}
	done := make(chan ended, 1)
	go func() {
		o, err := s.r.Run(context.WithoutCancel(ctx), spec)
		if err == nil {
			// Seen to its end. A runner error is a unit lost sight
			// of — the request may have reached the manager all the
			// same — and the record stays for the next lock holder,
			// whose wait treats a unit that is not there as ended.
			_ = os.Remove(marker)
		}
		done <- ended{o, err}
	}()
	select {
	case e := <-done:
		if e.err != nil {
			// Lost sight of, and recorded still: left what it needs.
			s.leaveInit()
			return e.o, "", e.err
		}
		return e.o, resticMessage(errFile), nil
	case <-clock.Done():
		s.leaveInit()
		if ctx.Err() != nil {
			return unit.Outcome{}, "", ctx.Err()
		}
		return unit.Outcome{}, "", fmt.Errorf("%w within %s; restic init is left running as %s, and the next run or setup waits for it (if it must be ended: systemctl stop %s)", errDidNotAnswer, within, spec.Name, spec.Name)
	}
}

// repositoryExists reads restic's word for a repository that is there
// already, out of an init that exited 1: two wordings, one condition
// [measured] — "config file already exists" where the backend's Stat
// of the config answers (a local path, S3 proper), "repository master
// key and config already initialized" where it does not (the rclone S3
// fixture) and init goes on to find the config as it makes the key.
// Exactly those, so that a storage's own "already exists" about some
// other thing is not taken for it.
func repositoryExists(message string) bool {
	return strings.Contains(message, "config file already exists") || strings.Contains(message, "repository master key and config already initialized")
}

// resticMessage is what restic said on stderr: the message of its
// exit_error line when --json gave it one, else the text as it is —
// cleaned, but whole: the phrase that says a repository exists comes
// after its URL, however long that is. What is shown is cut by Text.
func resticMessage(file string) string {
	var m struct {
		Message string `json:"message"`
	}
	if jsonLine(file, "exit_error", &m) {
		return record.Clean(m.Message)
	}
	raw, _ := os.ReadFile(file) //nolint:gosec // written by the manager into root's own run dir
	return record.Clean(string(raw))
}

// initializedID is the id of the repository `restic init --json` made,
// from its one line [measured].
func initializedID(file string) string {
	var m struct {
		ID string `json:"id"`
	}
	if jsonLine(file, "initialized", &m) && snapshotRe.MatchString(m.ID) {
		return m.ID
	}
	return ""
}

// configID is the repository's id from `restic cat config` [measured].
func configID(file string) string {
	raw, err := os.ReadFile(file) //nolint:gosec // written by the manager into root's own run dir
	if err != nil {
		return ""
	}
	var c struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &c) != nil || !snapshotRe.MatchString(c.ID) {
		return ""
	}
	return c.ID
}

// newPassword is 32 random bytes, in an alphabet a person can store and
// type back: base32, lower case, no padding.
func newPassword() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)), nil
}
