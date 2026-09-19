// Package backup copies what liveswap apps declare as state to a
// restic repository.
//
// It is a CLI subcommand, never part of the serving process: a
// systemd timer runs `hotserve backup run`, which asks the admin API
// which apps declare `state` and then runs one short-lived, sandboxed
// job per app (`hotserve backup app`). A backup that stalls, leaks or
// crashes therefore cannot touch traffic, and backups still run while
// hotserve is being restarted.
//
// restic is an external binary (Debian ships it) — never a library —
// and every invocation is logged as it is run, so an operator can
// reproduce any step by hand. The one thing this package does that
// restic cannot is make a SQLite database safe to copy: a live
// database is copied with VACUUM INTO into a staging dir first, and
// restic backs up the copy.
package backup

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// DefaultAdminAddress is where the packaged Caddyfile puts the admin
// API (packaging/Caddyfile). A TCP admin API would be reachable by a
// deployed app; the unix socket is not.
const DefaultAdminAddress = "unix//run/hotserve/admin.sock"

// DefaultStagingRoot holds one dir per app for the database copies
// taken before restic runs. Root-owned, 0750 per app; each job binds
// only its own app's dir.
const DefaultStagingRoot = "/var/lib/hotserve-backup"

// Inside an app's staging dir: the copies restic backs up, and
// restic's own cache. They are separate because the backup target is
// the first one — a cache under it would be backed up every hour,
// growing the repository with a copy of itself.
const (
	stagingData  = "data"
	stagingCache = "cache"
)

// StagingData is what restic is pointed at; StagingCache is where
// restic keeps its index cache between runs. Without a writable cache
// restic re-reads the repository index every hour and says so
// ("unable to open cache").
func StagingData(appStaging string) string  { return filepath.Join(appStaging, stagingData) }
func StagingCache(appStaging string) string { return filepath.Join(appStaging, stagingCache) }

// CleanTag marks a clean-run record: a tiny snapshot the job writes
// into the repository once a backup has exited cleanly AND been read
// back holding everything declared. restic writes a snapshot even when
// it exits non-zero (unreadable sources, exit status 3), so "a snapshot
// exists" is not "the backup worked" — and a monitor, or a restore,
// that cannot tell those apart is worse than none.
//
// The record lives in the repository, not on the box: a rebuilt box
// can still tell which of the dead box's snapshots were whole, and a
// record vouches only for runs in its own repository, including after
// `init --force` onto another one. Writing a record is an append,
// which a key that cannot delete can still do.
//
// It carries no `hotserve` tag, so nothing that lists backups — status,
// restore, `snapshots --tag app:<name>` — mistakes it for one.
const CleanTag = "hotserve-clean"

// cleanAppTag and cleanOfTag say which app's run a record is for and
// which snapshot it vouches for.
func cleanAppTag(app string) string { return "clean-app:" + app }
func cleanOfTag(id string) string   { return "clean-of:" + id }

// cleanOf is the id of the snapshot a clean-run record vouches for.
func cleanOf(record Snapshot) string {
	for _, t := range record.Tags {
		if id, ok := strings.CutPrefix(t, "clean-of:"); ok {
			return id
		}
	}
	return ""
}

// cleanRecordArgs writes the record for snapshot id of app. Its content
// comes from a command restic runs itself, so no file is written for it.
func cleanRecordArgs(app, id string) []string {
	return []string{"backup", "--quiet",
		"--tag", CleanTag, "--tag", cleanAppTag(app), "--tag", cleanOfTag(id),
		"--stdin-from-command", "--stdin-filename", CleanTag,
		"--", "echo", id}
}

// cleanRecords splits a snapshot listing into backups and the clean-run
// records among them, keyed by app: the ids each app's records vouch
// for, and the records themselves (for their time and host).
func cleanRecords(snaps []Snapshot) (backups []Snapshot, cleanIDs map[string]map[string]bool, records map[string][]Snapshot) {
	cleanIDs = map[string]map[string]bool{}
	records = map[string][]Snapshot{}
	for _, s := range snaps {
		if !slices.Contains(s.Tags, CleanTag) {
			backups = append(backups, s)
			continue
		}
		var app, of string
		for _, t := range s.Tags {
			if v, ok := strings.CutPrefix(t, "clean-app:"); ok {
				app = v
			}
			if v, ok := strings.CutPrefix(t, "clean-of:"); ok {
				of = v
			}
		}
		if app == "" || of == "" {
			continue
		}
		if cleanIDs[app] == nil {
			cleanIDs[app] = map[string]bool{}
		}
		cleanIDs[app][of] = true
		records[app] = append(records[app], s)
	}
	return backups, cleanIDs, records
}

// say prints one line of progress for the operator — to their terminal
// when they ran the command, to the journal when the timer did.
//
// Nothing checks the write, and this is the only place that decides
// so: a backup cannot do anything useful about a terminal that will
// not take a line, and a command whose real work succeeded must not
// report failure because its last println did not land.
func say(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format+"\n", a...)
}

// unquote drops the quotes around a value in a --credentials-file.
func unquote(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// Backends are the restic backends a repository may be. A repository
// is always a URL for one of them, never a path on this box.
//
// Each takes its credentials as settings, which is the only way a
// credential reaches a job: the sandbox holds the environment file's
// values and no file of the operator's. That is what leaves out the two
// restic backends that run another program: sftp, whose ssh reads its
// key and known_hosts from the user's home and /etc/ssh, and rclone,
// which reads a config file from the user's home. Neither place is in a
// job's view.
var Backends = []string{"s3", "b2", "rest", "azure", "gs", "swift"}

// CheckRepository refuses anything that is not a backend URL.
func CheckRepository(repo string) error {
	if scheme, rest, ok := strings.Cut(repo, ":"); ok && rest != "" && slices.Contains(Backends, scheme) {
		return nil
	}
	if strings.HasPrefix(repo, "sftp:") {
		return fmt.Errorf("repository %q: sftp is not supported — ssh takes its key and known_hosts from files, and the backup jobs run in a sandbox that holds only the settings init writes; use a backend whose credentials are settings (%s:)", repo, strings.Join(Backends, ":, "))
	}
	if strings.HasPrefix(repo, "rclone:") {
		return fmt.Errorf("repository %q: rclone is not supported — it reads its remotes from a config file in the user's home, and the backup jobs run in a sandbox that holds only the settings init writes; use a backend restic reaches itself (%s:)", repo, strings.Join(Backends, ":, "))
	}
	return fmt.Errorf("repository %q is not a backend URL: it must start with one of %s: — a path on this box is not supported", repo, strings.Join(Backends, ":, "))
}

// checkSettings holds the repository settings to this package's rules,
// reading them from an environment — a unit's, which systemd built from
// the settings file. Nothing here reads that file: its format is
// systemd's (quotes, escapes, continuation lines), and a second reader
// of it is a second opinion about what it says.
//
// RESTIC_REPOSITORY_FILE is refused: the jobs' sandbox cannot see the
// file it points at.
func checkSettings(env func(string) string) error {
	if env("RESTIC_REPOSITORY_FILE") != "" {
		return fmt.Errorf("RESTIC_REPOSITORY_FILE is not supported: the backup jobs run in a sandbox that cannot see it — put RESTIC_REPOSITORY in the environment file instead")
	}
	if err := requireResticEnv(env); err != nil {
		return err
	}
	return CheckRepository(env("RESTIC_REPOSITORY"))
}

// requireSettingsFile says so plainly when backups were never set up;
// systemd's own words for a missing EnvironmentFile= say nothing about
// how to make one. It looks, and does not read.
func requireSettingsFile(path string) error {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("backups are not configured on this box: %s does not exist — `hotserve backup init <repository>` writes it", path)
	}
	return nil
}

// DefaultLiveswapRoot repeats liveswap's own default because the
// admin API serves the config as it was loaded, not as it was
// provisioned: an operator who never wrote `root` leaves the field
// empty there. Kept in sync by TestDefaultRootMatchesLiveswap.
const DefaultLiveswapRoot = "/var/lib/liveswap"

// State kinds, as liveswap serializes them. Not imported from
// liveswap: this package depends on that module's *config JSON*, a
// contract the admin API already publishes, and not on its code.
const (
	KindSQLite = "sqlite"
	KindFiles  = "files"
)

// StateEntry is one `state <kind> <path>` declaration of one app.
type StateEntry struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}

// App is one app worth backing up: the paths its job needs, already
// resolved, so the job itself needs no access to the admin API (it
// could not reach it anyway — the socket is outside its sandbox).
type App struct {
	Name   string
	Shared string
	State  []StateEntry
}

// Databases and Files split the declarations by kind, keeping the
// order they were declared in.
func (a App) Databases() []string { return a.pathsOfKind(KindSQLite) }
func (a App) Files() []string     { return a.pathsOfKind(KindFiles) }

func (a App) pathsOfKind(kind string) []string {
	var out []string
	for _, e := range a.State {
		if e.Kind == kind {
			out = append(out, e.Path)
		}
	}
	return out
}

// sharedPath resolves a declared path against the app's shared dir,
// and refuses one that does not stay inside it. liveswap validates
// this at config load; it is checked again here because this process
// reads the config over a socket, and a path that escapes would make
// a backup job read something the app never declared.
func sharedPath(shared, rel string) (string, error) {
	clean := filepath.Clean(rel)
	if !filepath.IsLocal(clean) || clean == "." {
		return "", fmt.Errorf("state path %q must stay inside the app's shared dir", rel)
	}
	return filepath.Join(shared, clean), nil
}

// requireResticEnv fails before anything runs when the repository or
// its password is missing, naming the file the packaging puts them
// in. Without it restic's own error arrives a staging copy later and
// says nothing about where the settings belong.
func requireResticEnv(env func(string) string) error {
	if env("RESTIC_REPOSITORY") == "" {
		return fmt.Errorf("RESTIC_REPOSITORY is not set: put it in /etc/hotserve/backup.env (root-owned, 0600) — `hotserve backup init` writes that file")
	}
	pwSet := env("RESTIC_PASSWORD") != "" ||
		env("RESTIC_PASSWORD_FILE") != "" ||
		env("RESTIC_PASSWORD_COMMAND") != ""
	if !pwSet {
		return fmt.Errorf("no restic password is set: RESTIC_PASSWORD, RESTIC_PASSWORD_FILE or RESTIC_PASSWORD_COMMAND belongs in /etc/hotserve/backup.env")
	}
	return nil
}

// sqliteURI is the read-only connection string for the live database.
// Opening read-only says what the job is for: it reads the app's
// data, and cannot leave a hot journal behind in the app's directory.
//
// The path is escaped rather than concatenated: `?`, `#` and `%` are
// legal in a filename and meaningful in a URI, so `data/a?b.db` would
// otherwise open `data/a` with a stray parameter — or fail — instead
// of the database the operator declared.
func sqliteURI(path string) string {
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	return u.String()
}

// vacuumInto is the SQL that takes the consistent copy. The
// destination is a SQL string literal, so a quote in the path is
// doubled; liveswap's own validation does not forbid one.
func vacuumInto(dst string) string {
	return "VACUUM INTO '" + strings.ReplaceAll(dst, "'", "''") + "'"
}

func lookupEnv(key string) string { return os.Getenv(key) }

// CredentialsFile reads init's --credentials-file: the provider's keys
// as KEY=VALUE lines, in a file the operator wrote, so that they are not
// in shell history or in /proc/*/cmdline while init runs. The format is
// this command's own — KEY=VALUE, # comments and blank lines, a value's
// surrounding quotes dropped — and what init then writes for systemd is
// held to envFileSafe, so systemd reads back exactly what was checked.
func CredentialsFile(path string) ([]string, error) {
	body, err := os.ReadFile(path) //nolint:gosec // the operator's own root-only file, named on their command line
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("--credentials-file %s does not exist: it is a file you write, holding the provider's keys as KEY=VALUE lines, one per line", path)
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var env []string
	for line := range strings.SplitSeq(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s: %q is not a KEY=VALUE line", path, line)
		}
		env = append(env, key+"="+unquote(value))
	}
	if len(env) == 0 {
		return nil, fmt.Errorf("--credentials-file %s is empty: it should hold the provider's keys, like AWS_ACCESS_KEY_ID=… and AWS_SECRET_ACCESS_KEY=… (the repository and its password are not set here — they are the argument to init and --password-file)", path)
	}
	return env, nil
}
