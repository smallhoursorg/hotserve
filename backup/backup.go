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
	"fmt"
	"net/url"
	"os"
	"path/filepath"
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

// unquote mirrors what systemd does to an EnvironmentFile= value.
func unquote(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// RepositoryPath returns the directory a filesystem repository lives
// in, or "" for a remote one (s3:, b2:, sftp:, rest: …). A local
// repository has to be bound into each job's view; a remote one needs
// nothing but the network the sandbox already allows.
//
// A relative path is refused rather than quietly treated as remote:
// it would be bound nowhere, chowned nowhere, and resolved against
// whatever directory each process happened to start in.
func RepositoryPath(repo string) (string, error) {
	v := strings.TrimPrefix(repo, "local:")
	if strings.HasPrefix(v, "/") {
		return filepath.Clean(v), nil
	}
	// A backend prefix (scheme before any slash) is remote; anything
	// else is a path, and paths have to be absolute.
	if scheme, _, ok := strings.Cut(v, ":"); ok && !strings.Contains(scheme, "/") && scheme != "" && !strings.HasPrefix(repo, "local:") {
		return "", nil
	}
	return "", fmt.Errorf("repository %q must be an absolute path (a directory on this box) or a backend URL like s3:…, b2:… or sftp:…", repo)
}

// LocalRepositoryPath is RepositoryPath for the settings systemd
// hands the jobs, plus the rule that this file is where a repository
// is named: RESTIC_REPOSITORY_FILE would point at a file the jobs
// cannot see inside their view, so it is refused rather than left to
// fail hourly.
func LocalRepositoryPath(env []string) (string, error) {
	var repo string
	for _, kv := range env {
		if strings.HasPrefix(kv, "RESTIC_REPOSITORY_FILE=") {
			return "", fmt.Errorf("RESTIC_REPOSITORY_FILE is not supported: the backup jobs run in a sandbox that cannot see it — put RESTIC_REPOSITORY in the environment file instead")
		}
		if v, ok := strings.CutPrefix(kv, "RESTIC_REPOSITORY="); ok {
			repo = v
		}
	}
	if repo == "" {
		return "", fmt.Errorf("no RESTIC_REPOSITORY in the environment file: `hotserve backup init <repository>` writes it")
	}
	return RepositoryPath(repo)
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

// LoadEnvFile reads the repository settings systemd hands the jobs, so
// a command an operator runs by hand (status, prune) reaches the same
// repository without them exporting anything. Deliberately not a
// shell: KEY=VALUE, # comments and blank lines, nothing else.
func LoadEnvFile(path string) ([]string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("backups are not configured on this box: %s does not exist — `hotserve backup init <repository>` writes it", path)
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
		// systemd's EnvironmentFile= strips surrounding quotes before
		// handing the value to the job. Reading the same file any
		// other way would give this process a different repository
		// path and a different password than the jobs get — the
		// launcher would then look for a repository named `"/srv/x"`,
		// find no leading slash, and never bind it into the view.
		env = append(env, key+"="+unquote(value))
	}
	if len(env) == 0 {
		return nil, fmt.Errorf("%s is empty: it should set RESTIC_REPOSITORY and RESTIC_PASSWORD", path)
	}
	return env, nil
}
