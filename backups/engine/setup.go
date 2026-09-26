package engine

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
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
}

// setupClock bounds each of setup's two restic units. restic init
// answers at once whatever is wrong — a wrong key, a host that does not
// resolve, a port nobody listens on, all measured — and after 30 s for
// a host that swallows packets; a probe of an existing repository with
// a wrong password answers at once (exit 12). The clock is for what
// was not measured. setupNote is when a person waiting is told what
// for.
var (
	setupClock = 2 * time.Minute
	setupNote  = 20 * time.Second
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
		if u, err := url.Parse(rest); err == nil && u.User != nil {
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
	_, err := backendOf(repo, envFile)
	if errors.Is(err, errByHand) {
		return nil
	}
	return err
}

// OldEnvFileNote is what a run and status add when the credential file
// is missing and one from an earlier version of this branch is there:
// it is not read, and setup is what to run. Empty otherwise.
func OldEnvFileNote(cfg Config) string {
	if _, err := os.Lstat(cfg.OldEnvFile); cfg.OldEnvFile == "" || err != nil {
		return ""
	}
	return fmt.Sprintf(" (%s is from before this version and is not read: run `hotserve-backup setup`)", cfg.OldEnvFile)
}

// tempRe is the shape of the file setup writes beside the working one
// (repository.env.<nonce>), and of the file envfile.Write makes on the
// way to it (.repository.env.<nonce>-<n>): what a setup that did not
// live to remove them leaves, and all the sweep touches — a copy an
// operator keeps beside the file under another name is theirs.
func tempRe(envFile string) *regexp.Regexp {
	return regexp.MustCompile(`^\.?` + regexp.QuoteMeta(filepath.Base(envFile)) + `\.[0-9a-f]{12}(-[0-9]+)?$`)
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
	if strings.TrimSpace(o.Repository) != o.Repository {
		return nil, errors.New("the repository URL has whitespace at an end")
	}
	if err := envfile.Refuse(o.Repository); err != nil {
		return nil, fmt.Errorf("the repository URL cannot go in the file: %w", err)
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
	shape := tempRe(cfg.EnvFile)
	for _, e := range entries {
		if !shape.MatchString(e.Name()) {
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

	pairs := []envfile.Pair{{Key: "RESTIC_REPOSITORY", Value: o.Repository}, {Key: "RESTIC_PASSWORD", Value: ""}}
	for _, c := range creds {
		v, err := ask(ctx, term, c.label+" ("+c.variable+"): ", c.secret)
		if err != nil {
			return nil, err
		}
		pairs = append(pairs, envfile.Pair{Key: c.variable, Value: v})
	}
	password, err := newPassword()
	if err != nil {
		return nil, err
	}
	term.Say("Repository password (new): " + password)
	term.Say("Store it off the box now: without it no backup can be read.")
	stored, err := term.Ask(ctx, "Type stored to go on: ", false)
	if err != nil {
		return nil, fmt.Errorf("nobody answered: %w", err)
	}
	if stored != "stored" {
		return nil, errors.New("the password was not confirmed stored: nothing was written")
	}
	pairs[1].Value = password

	// The file is written beside the working one, under a name the
	// manager reads for the two units here, and takes the working one's
	// place only once the repository has answered. Whatever ends this
	// command before that, the working file is as it was and the temp
	// file goes with it.
	tmp := cfg.EnvFile + "." + x.nonce
	defer os.Remove(tmp) //nolint:errcheck // gone already once it was put in place
	if err := envfile.Write(tmp, pairs); err != nil {
		return nil, err
	}
	o1, message, err := x.repository(ctx, term, o.Repository, "init", tmp, cfg.Restic, "init", "--json")
	if err != nil {
		return nil, err
	}
	switch {
	case o1.OK():
		if rep.RepositoryID = initializedID(filepath.Join(x.dir, "init.out")); rep.RepositoryID == "" {
			return nil, errors.New("restic exited 0 but said nothing of a repository")
		}
		rep.New = true
	case o1.Result == "exit-code" && o1.ExitStatus == 1 && repositoryExists(message):
		// A rebuilt box, or a bucket reused: the repository has a
		// password already, and the one just shown is not it.
		term.Say("the repository exists; its password is needed (the one shown above is not it)")
		own, err := ask(ctx, term, "Repository password: ", true)
		if err != nil {
			return nil, err
		}
		pairs[1].Value = own
		if err := envfile.Write(tmp, pairs); err != nil {
			return nil, err
		}
		o2, _, err := x.repository(ctx, term, o.Repository, "probe", tmp, cfg.Restic, "cat", "config", "--no-lock")
		if err != nil {
			return nil, err
		}
		if !o2.OK() {
			if o2.Result == "exit-code" && o2.ExitStatus == 12 {
				return nil, errors.New("this password cannot open the repository (exit 12)")
			}
			detail, _ := resticFailure(o2)
			return nil, errors.New(detail)
		}
		if rep.RepositoryID = configID(filepath.Join(x.dir, "probe.out")); rep.RepositoryID == "" {
			return nil, errors.New("restic exited 0 but said nothing of the repository's id")
		}
	case o1.Result == "exit-code" && o1.ExitStatus == 1:
		return nil, errors.New("restic could not make or open the repository (exit 1): " + record.Text(message))
	default:
		detail, _ := resticFailure(o1)
		return nil, errors.New(detail)
	}

	if err := envfile.Commit(tmp, cfg.EnvFile); err != nil {
		return nil, err
	}
	// The record speaks of the repository it was written against. With
	// another one now in use it is put aside, so that the next run
	// drills what it backs up into the repository it is now using —
	// after the file is in place, so that a file that could not be put
	// in place keeps the record it goes with.
	statusPath := filepath.Join(cfg.StateDir, "status.json")
	if _, err := os.Lstat(statusPath); err == nil && (!hadOld || oldRepository != o.Repository) {
		rep.Aside = statusPath + ".aside-" + time.Now().UTC().Format("20060102T150405Z")
		if err := os.Rename(statusPath, rep.Aside); err != nil {
			return nil, err
		}
		term.Say("the record of the previous repository was put aside: " + rep.Aside)
	}
	kind := "existing"
	if rep.New {
		kind = "new"
	}
	term.Say(fmt.Sprintf("repository ready: %s (%s, id %s)", o.Repository, kind, short(rep.RepositoryID)))
	return rep, nil
}

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

// repository runs one restic command against the repository as the
// backup account, with the credential file the manager reads, under
// the clock; a person waiting is told what for. It returns what restic
// said on stderr, cleaned — the message of its exit_error line where it
// wrote one — for the caller to match and show.
func (x *run) repository(ctx context.Context, term Terminal, repo, role, envFile string, argv ...string) (unit.Outcome, string, error) {
	clock, cancel := context.WithTimeout(ctx, setupClock)
	defer cancel()
	note := time.AfterFunc(setupNote, func() {
		term.Say("still waiting for " + repo + " (Ctrl-C is safe: nothing has been written)")
	})
	defer note.Stop()
	errFile := filepath.Join(x.dir, role+".err")
	o, err := x.start(clock, unit.Spec{
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
			return o, "", fmt.Errorf("the repository did not answer within %s; the unit was stopped", setupClock)
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
