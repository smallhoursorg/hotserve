package engine

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
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
	haveProgram   = func(path string) bool { _, err := os.Stat(path); return err == nil }
	accountExists = func(name string) bool { _, err := user.Lookup(name); return err == nil }
	makeAccount   = func() error {
		if out, err := exec.Command(useradd[0], useradd[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %w: %s", useraddArgv(), err, record.Text(string(out)))
		}
		return nil
	}
)

// useradd makes the account restic runs as: no home, no shell, and
// nothing of its own but the cache the manager makes for it.
var useradd = []string{"/usr/sbin/useradd", "--system", "--no-create-home", "--home-dir", "/nonexistent", "--shell", "/usr/sbin/nologin", backupUser}

func useraddArgv() string { return strings.Join(useradd, " ") }

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
	case "gs", "swift":
		return nil, fmt.Errorf("gs: and swift: are %w: write %s by hand (see the README)", errByHand, envFile)
	case "s3", "rest":
		if rest == "" {
			return nil, fmt.Errorf("%s: names no bucket", scheme)
		}
		// restic takes the host with or without a scheme (s3:host/bucket):
		// what is looked at is the authority either way, not what
		// url.Parse makes of "user:pass@host" with no scheme before it.
		authority := rest
		if i := strings.Index(rest, "://"); i >= 0 {
			authority = rest[i+3:]
		}
		authority, _, _ = strings.Cut(authority, "/")
		if strings.Contains(authority, "@") {
			return nil, errors.New("credentials in the repository URL are on the command line and in the shell's history; give the URL without them, and type them when asked")
		}
		if scheme == "rest" {
			return []credential{{"Storage user name", "RESTIC_REST_USERNAME", false}, {"Storage password", "RESTIC_REST_PASSWORD", true}}, nil
		}
		return []credential{{"Storage key id", "AWS_ACCESS_KEY_ID", false}, {"Storage secret key", "AWS_SECRET_ACCESS_KEY", true}}, nil
	case "b2":
		if rest == "" {
			return nil, errors.New("b2: names no bucket")
		}
		return []credential{{"Storage key id", "B2_ACCOUNT_ID", false}, {"Storage secret key", "B2_ACCOUNT_KEY", true}}, nil
	}
	return nil, fmt.Errorf("%q is not a repository restic knows (s3:, b2: or rest:)", scheme+":")
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
	for _, p := range []struct{ name, path, fix string }{
		{"restic", cfg.Restic, ": apt install restic"},
		{"sqlite3", dump.Program(), ": apt install sqlite3"},
		{"hotserve", plan.Program(), ""},
	} {
		if !haveProgram(p.path) {
			return nil, fmt.Errorf("%s is not installed at %s%s", p.name, p.path, p.fix)
		}
	}
	if m, ok := r.(interface {
		ManagerVersion(context.Context) (int, error)
	}); ok {
		v, err := m.ManagerVersion(ctx)
		if err != nil {
			return nil, err
		}
		if v < 257 {
			return nil, fmt.Errorf("systemd 257 or later is needed (Debian 13's); this box has %d", v)
		}
	}
	rep := &SetupReport{Account: "present"}
	if !accountExists(backupUser) {
		if err := makeAccount(); err != nil {
			return nil, fmt.Errorf("making the %s account: %w", backupUser, err)
		}
		rep.Account = "made"
	}
	term.Say("account " + backupUser + ": " + rep.Account)

	x, end, err := open(cfg, r)
	if x == nil {
		return nil, err
	}
	defer end()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(cfg.EnvFile)
	// Anyone may see that the file is there and when it was written —
	// that is how status tells a fresh setup from one that never ran —
	// and nobody but root what is in it.
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // holds one root-only file
		return nil, err
	}
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // said again past the caller's umask
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		// What a setup that did not live to remove them leaves, and
		// nothing else: a copy an operator keeps beside the file under
		// another name is theirs.
		if !envfile.IsLeftover(e.Name(), filepath.Base(cfg.EnvFile)) {
			continue
		}
		f := filepath.Join(dir, e.Name())
		if err := os.Remove(f); err != nil {
			return nil, fmt.Errorf("a file an interrupted setup left could not be removed: %w", err)
		}
		term.Say("removed a file an interrupted setup left: " + f)
		rep.Swept = append(rep.Swept, f)
	}
	// The file before, if any: its repository decides what becomes of
	// the record. Root's own file, written by this command alone.
	var oldRepository string
	hadOld := false
	if raw, err := os.ReadFile(cfg.EnvFile); err == nil {
		v, _ := envfile.Parse(raw)
		oldRepository, hadOld = v["RESTIC_REPOSITORY"], true
	}

	p, err := x.planWith(ctx, filepath.Join(x.dir, "plan.err"))
	if err != nil {
		return nil, err
	}
	rep.Apps = p.Names()
	if len(rep.Apps) == 0 {
		term.Say("no app declares a backup yet: a run would back nothing up")
	} else {
		term.Say(fmt.Sprintf("a run would back up %s, under %s", record.Clean(strings.Join(rep.Apps, ", ")), record.Text(p.Root)))
	}

	// The file is written beside the working one, under a name the
	// manager reads for the units here, and takes the working one's
	// place only once the repository has answered. Whatever ends this
	// command before that, the working file is as it was and the temp
	// file goes with it. Until a password is made it holds a throwaway
	// one: what the look for a repository needs, and nothing that takes
	// effect anywhere.
	tmp := cfg.EnvFile + "." + x.nonce
	defer os.Remove(tmp) //nolint:errcheck // gone already once it was put in place
	throwaway, err := newPassword()
	if err != nil {
		return nil, err
	}
	// password is the one made for a new repository: shown once, and
	// kept through a key typed wrong — it has taken effect nowhere
	// until init has made the repository with it.
	var password string
	exists := false
	pairs := []envfile.Pair{{Key: "RESTIC_REPOSITORY", Value: o.Repository}, {Key: "RESTIC_PASSWORD", Value: throwaway}}
	for attempt := 1; ; attempt++ {
		pairs = pairs[:2]
		for _, c := range creds {
			v, err := ask(ctx, term, c.label+" ("+c.variable+"): ", c.secret)
			if err != nil {
				return nil, x.setupErr(err, password)
			}
			pairs = append(pairs, envfile.Pair{Key: c.variable, Value: v})
		}
		pairs[1].Value = throwaway
		if err := envfile.Write(tmp, pairs); err != nil {
			return nil, err
		}
		// Before any password is made: is there a repository already?
		// With a right key restic says at once — none (10), or one this
		// password does not open (12) — and a rebuilt box is then asked
		// for the password it has, not shown one it does not need.
		// Where the look cannot tell, init answers, and says why.
		term.Say(fmt.Sprintf("looking for a repository at %s (up to %s)", o.Repository, setupProbeClock))
		o0, _, err := x.repository(ctx, term, o.Repository, "probe", tmp, setupProbeClock, cfg.Restic, "cat", "config", "--no-lock")
		// Only two answers say the look could not tell: restic's exit 1
		// — the storage retrying — and the clock. Anything else is a
		// fault of the unit's own, said as such before any password is
		// made: a manager that could not set it up, a unit that could
		// not be started or seen gone.
		if err != nil && !errors.Is(err, errDidNotAnswer) {
			return nil, x.setupErr(err, password)
		}
		if err == nil && !lookAnswered(o0) {
			detail, _ := resticFailure(o0)
			return nil, x.setupErr(errors.New("looking for the repository: "+detail), password)
		}
		switch {
		case err == nil && o0.Result == "exit-code" && o0.ExitStatus == 12:
			exists = true
			if password != "" {
				term.Say("the repository exists; its password is needed (the one shown above is not it)")
			} else {
				term.Say("the repository exists; its password is needed")
			}
		case err == nil && o0.Result == "exit-code" && o0.ExitStatus == 10:
			// None: one is made below.
		case password != "":
			term.Say(fmt.Sprintf("no repository answered within %s: a bucket not made yet, a wrong key and a wrong host look alike here; a new repository will be made, and the password shown above still applies", setupProbeClock))
		default:
			term.Say(fmt.Sprintf("no repository answered within %s: a bucket not made yet, a wrong key and a wrong host look alike here; a new repository will be made, and if that fails the password shown next was never used", setupProbeClock))
		}
		if exists {
			break
		}
		if password == "" {
			if password, err = newPassword(); err != nil {
				return nil, err
			}
			term.Say("Repository password (new): " + password)
			term.Say("Store it off the box now, with the repository URL, " + o.Repository + ", and the storage key: with those three, `restic -r <url>` reads every backup from any machine; without the password nothing can.")
			if err := x.confirmStored(ctx, term); err != nil {
				return nil, x.setupErr(err, password)
			}
		}
		pairs[1].Value = password
		if err := envfile.Write(tmp, pairs); err != nil {
			return nil, err
		}
		o1, message, err := x.repository(ctx, term, o.Repository, "init", tmp, setupClock, cfg.Restic, "init", "--json")
		if err != nil {
			return nil, x.setupErr(err, password)
		}
		if o1.OK() {
			if rep.RepositoryID = initializedID(filepath.Join(x.dir, "init.out")); rep.RepositoryID == "" {
				// Exit 0 is a repository made, with the password that was
				// shown: whatever restic did not say, that password is now
				// the repository's, and is never said to be dead.
				return nil, errors.New("restic made the repository and exited 0 but said nothing of it; the password shown above is the repository's: keep it, and run setup again to open it")
			}
			rep.New = true
			break
		}
		if o1.Result == "exit-code" && o1.ExitStatus == 1 && repositoryExists(message) {
			// The look could not tell, and init can: the repository has a
			// password already, and the one just shown is not it.
			term.Say("the repository exists; its password is needed (the one shown above is not it)")
			exists = true
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
		return nil, x.setupErr(failure, password)
	}
	if exists {
		for attempt := 1; ; attempt++ {
			own, err := ask(ctx, term, "Repository password: ", true)
			if err != nil {
				return nil, x.setupErr(err, password)
			}
			pairs[1].Value = own
			if err := envfile.Write(tmp, pairs); err != nil {
				return nil, err
			}
			o2, message, err := x.repository(ctx, term, o.Repository, "open", tmp, setupClock, cfg.Restic, "cat", "config", "--no-lock")
			if err != nil {
				return nil, x.setupErr(err, password)
			}
			if o2.OK() {
				if rep.RepositoryID = configID(filepath.Join(x.dir, "open.out")); rep.RepositoryID == "" {
					return nil, x.setupErr(errors.New("restic exited 0 but said nothing of the repository's id"), password)
				}
				break
			}
			if o2.Result == "exit-code" && o2.ExitStatus == 12 {
				if attempt < 3 {
					term.Say("this password cannot open the repository (exit 12): again")
					continue
				}
				return nil, x.setupErr(errors.New("this password cannot open the repository (exit 12)"), password)
			}
			if o2.Result == "exit-code" && o2.ExitStatus == 1 {
				return nil, x.setupErr(errors.New("restic could not open the repository (exit 1): "+record.Text(message)), password)
			}
			detail, _ := resticFailure(o2)
			return nil, x.setupErr(errors.New(detail), password)
		}
	}
	// The record speaks of the repository it was written against. With
	// another one now in use — or none known, or one this setup made —
	// it is put aside, so that the next run drills what it backs up
	// into the repository it is now using. Aside first, then the file:
	// whatever ends setup between the two leaves no credential file with
	// a record of another repository beside it (a record aside with the
	// old file in place is a fresh start, which the next run mends); a
	// file that could not be put in place gets its record back.
	statusPath := filepath.Join(cfg.StateDir, "status.json")
	why := ""
	if _, err := os.Lstat(statusPath); err == nil {
		switch {
		case rep.New:
			why = "the repository was made by this setup, so it holds none of the record's snapshots"
		case !hadOld:
			why = "no credential file was there to tie it to this repository"
		case oldRepository != o.Repository:
			why = "the previous credential file named another repository"
		}
	}
	if why != "" {
		rep.Aside = statusPath + ".aside-" + time.Now().UTC().Format("20060102T150405Z")
		if err := os.Rename(statusPath, rep.Aside); err != nil {
			return nil, err
		}
	}
	if err := commit(tmp, cfg.EnvFile); err != nil {
		if why != "" {
			if back := os.Rename(rep.Aside, statusPath); back != nil {
				return nil, fmt.Errorf("%w; and the record, put aside first, could not be put back from %s: %w", err, rep.Aside, back)
			}
			rep.Aside = ""
		}
		return nil, err
	}
	if why != "" {
		term.Say(fmt.Sprintf("the record was put aside (%s): %s; the next run drills what it backs up", why, rep.Aside))
	}
	kind := "existing"
	if rep.New {
		kind = "new"
	}
	term.Say(fmt.Sprintf("repository ready: %s (%s, id %s)", o.Repository, kind, short(rep.RepositoryID)))
	if password != "" && !rep.New {
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

// confirmStored has the operator type "stored" — a word that means
// something, not "y" — with one more asking for a word that is not
// it; anything then is a no.
func (x *run) confirmStored(ctx context.Context, term Terminal) error {
	for i := 0; i < 2; i++ {
		said, err := term.Ask(ctx, "Type stored to go on: ", false)
		if err != nil {
			return fmt.Errorf("nobody answered: %w", err)
		}
		if strings.EqualFold(strings.TrimSpace(said), "stored") {
			return nil
		}
		if i == 0 {
			term.Say("that is not stored: type stored to go on, or anything else to stop")
		}
	}
	return errors.New("the password was not confirmed stored: nothing was written")
}

// setupErr is an error in the operator's words: an interrupt is said
// as one, and a password that was shown and never used is said to be
// dead.
func (x *run) setupErr(err error, password string) error {
	// A unit that could not be seen gone may still be making the
	// repository with that password: nothing is said of it but the
	// runner's own words.
	if errors.Is(err, unit.ErrNotConfirmedGone) {
		return err
	}
	dead := ""
	if password != "" {
		dead = "; the password shown above was never used: discard it"
	}
	if errors.Is(err, context.Canceled) {
		return interrupted("interrupted: nothing has been written" + dead)
	}
	if dead != "" {
		return fmt.Errorf("%w%s", err, dead)
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
func ask(ctx context.Context, term Terminal, prompt string, secret bool) (string, error) {
	for i := 0; i < 3; i++ {
		v, err := term.Ask(ctx, prompt, secret)
		if err != nil {
			return "", fmt.Errorf("nobody answered: %w", err)
		}
		why := envfile.Refuse(v)
		if why == nil {
			return v, nil
		}
		term.Say(fmt.Sprintf("that cannot go in the file: %v; again", why))
	}
	return "", errors.New("no usable value was given in three times")
}

// lookAnswered says whether the look ended as a look can: none there
// (10), one there (12), or restic unable to tell (1; and 0, which a
// throwaway password cannot earn, is taken the same way). Anything
// else is the unit's own failure.
func lookAnswered(o unit.Outcome) bool {
	if o.OK() {
		return true
	}
	return o.Result == "exit-code" && (o.ExitStatus == 1 || o.ExitStatus == 10 || o.ExitStatus == 12)
}

// commit puts the file in place: envfile.Commit, a variable so that a
// test can look at the moment it happens.
var commit = envfile.Commit

// errDidNotAnswer is a unit stopped at its clock.
var errDidNotAnswer = errors.New("the repository did not answer")

// repository runs one restic command against the repository as the
// backup account, with the credential file the manager reads, under a
// clock; a person waiting is told what for. It returns what restic
// said on stderr, cleaned — the message of its exit_error line where it
// wrote one — for the caller to match and show.
//
// The init unit is the one unit here that is not recorded for the next
// lock holder to stop: stopped half way it leaves a repository with a
// config and no key, which no password opens, where left to its few
// seconds it makes the repository with the password that was shown —
// what the next setup then asks for. It holds the credential in its
// environment as every restic unit does, for as long as init takes.
func (x *run) repository(ctx context.Context, term Terminal, repo, role, envFile string, within time.Duration, argv ...string) (unit.Outcome, string, error) {
	clock, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	note := time.AfterFunc(setupNote, func() {
		term.Say("still waiting for " + repo + " (Ctrl-C is safe: nothing has been written)")
	})
	defer note.Stop()
	errFile := filepath.Join(x.dir, role+".err")
	start := x.start
	if role == "init" {
		start = func(ctx context.Context, s unit.Spec) (unit.Outcome, error) {
			s.BindsTo = x.cfg.BindsTo
			return x.r.Run(ctx, s)
		}
	}
	o, err := start(clock, unit.Spec{
		Name: x.name(role, ""), Description: "hotserve backup: " + role + " the repository",
		Argv: argv,
		User: backupUser, Network: true, EnvironmentFile: envFile,
		Environment:    []string{"RESTIC_CACHE_DIR=/var/cache/hotserve-backup", "HOME=/nonexistent"},
		CacheDirectory: "hotserve-backup",
		StdoutFile:     filepath.Join(x.dir, role+".out"), StderrFile: errFile,
	})
	if err != nil {
		// The clock: the unit was stopped and seen gone — unless the
		// runner says it could not confirm that, which is then the error,
		// in its own words.
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, unit.ErrNotConfirmedGone) {
			return o, "", fmt.Errorf("%w within %s; the unit was stopped", errDidNotAnswer, within)
		}
		return o, "", err
	}
	return o, resticMessage(errFile), nil
}

// repositoryExists reads restic's word for a repository that is there
// already, out of an init that exited 1. Two wordings, one condition
// [measured]: "config file already exists" where the backend's Stat
// of the config answers (a local path, S3 proper), "repository master
// key and config already initialized" where it does not (the rclone
// S3 fixture) and init goes on to find the config as it makes the key.
func repositoryExists(message string) bool {
	return strings.Contains(message, "already exists") || strings.Contains(message, "already initialized")
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
