package liveswap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
)

// Per-app sandboxing (issue #35) is systemd's own per-unit sandboxing
// on the user-manager runner, in two tiers that follow what the kernel
// gates and what the manager can express (measured on Debian 12/13 and
// Ubuntu 24.04, see the issue):
//
//   - filesystem: a user namespace (PrivateUsers=) plus the mount,
//     device, cgroup, seccomp and capability set. The user namespace
//     is what closes /proc/<pid>/{root,environ,cwd} of every process
//     outside it — commoncap refuses cross-user-namespace ptrace access
//     whatever the uid — so the mount restrictions cannot be walked
//     around via a same-UID neighbour's /proc. Available on every
//     user manager in the support matrix (systemd ≥ 252).
//   - full: filesystem plus a PID namespace (PrivatePIDs=, systemd ≥
//     256): the supervisor, the user manager and every sibling become
//     invisible and unsignalable. Without it a compromised app can
//     still enumerate and signal (kill) same-UID processes — a
//     denial-of-service residual, not a data-access one.
//
// The filesystem view is deny-by-default. An empty read-only tmpfs
// replaces the whole root (TemporaryFileSystem=/) and the only things
// that exist inside a unit are the ones this file names:
// sandboxBaseView, the app's own release and shared dirs, and its
// extra_paths. The guarantee is one sentence — anything not named is
// absent — and it holds by construction rather than by a list of
// things to hide being complete. Nothing has to be derived from the
// running configuration, and nothing ages: a secret that appears on
// the host after a unit started is absent from it for exactly the same
// reason every other path is.
//
// The policy (auto/require/off) is resolved per app at Start against a
// host capability probed once per Start; the tier an instance actually
// got is recorded with its handle in state.json so a relaunch
// reproduces it (engage on the next deploy, never on the upgrade
// relaunch — see liveswap/DESIGN-sandbox.md, rollout semantics).

// sandboxTier is what a unit actually gets.
type sandboxTier int

const (
	sandboxNone       sandboxTier = iota // floor only: non-dumpable supervisor, NoNewPrivileges
	sandboxFilesystem                    // user namespace + mount/device/cgroup/seccomp/caps set
	sandboxFull                          // filesystem + PID namespace
)

func (t sandboxTier) String() string {
	switch t {
	case sandboxFilesystem:
		return "filesystem"
	case sandboxFull:
		return "full"
	default:
		return "none"
	}
}

// parseSandboxTier is the inverse of String, for the persisted
// disposition; anything unknown (including "") is none — the safe
// reading of an older state.json, which relaunches bare.
func parseSandboxTier(s string) sandboxTier {
	switch s {
	case "filesystem":
		return sandboxFilesystem
	case "full":
		return sandboxFull
	default:
		return sandboxNone
	}
}

// Sandbox policy values, as configured.
const (
	sandboxAuto    = "auto"    // best available tier; WARN when it is not full
	sandboxRequire = "require" // full or refuse to start
	sandboxOff     = "off"
)

func validSandboxMode(s string) bool {
	return s == sandboxAuto || s == sandboxRequire || s == sandboxOff
}

// sandboxCapability is what the host can deliver, as probed at Start.
type sandboxCapability struct {
	tier   sandboxTier
	reason string // why tier is below full ("" when full)
}

// resolveSandboxTier applies the configured policy to the host's
// capability. require accepts only the full tier: accepting a weaker
// one silently is the "looks configured, quietly weaker" failure the
// design forbids. warn is non-empty when auto settled below full.
func resolveSandboxTier(mode string, c sandboxCapability) (tier sandboxTier, warn string, err error) {
	switch mode {
	case sandboxOff:
		return sandboxNone, "", nil
	case sandboxRequire:
		if c.tier != sandboxFull {
			return sandboxNone, "", fmt.Errorf("sandbox require: this host cannot deliver the full sandbox (%s); use `sandbox auto` to run with the %s tier, or upgrade the host", c.reason, c.tier)
		}
		return sandboxFull, "", nil
	default: // auto
		if c.tier == sandboxFull {
			return sandboxFull, "", nil
		}
		return c.tier, fmt.Sprintf("sandbox auto: running with the %s tier, not full (%s)", c.tier, c.reason), nil
	}
}

// extraPath is a host path the app may see in addition to its own
// directories (`extra_path <path> [rw]`).
type extraPath struct {
	path string
	rw   bool
	// source is what actually gets bound at path: the same thing,
	// unless path is a symlink, in which case resolveBindSources fills
	// in what it resolved to. The app still sees it at path — the name
	// it was configured with and the name everything else refers to.
	source string
}

// bindPath is one of the app's own directories bound back into its
// view: dest is where the app sees it (and what WorkingDirectory,
// HOME and the {release_dir}/{shared_dir} placeholders name), source
// is what is mounted there. They differ only when the operator has
// pointed one of those directories somewhere else with a symlink —
// binding the resolved path at the resolved path would put the
// directory somewhere the app has no reason to look, and its own
// spelling would be gone with the tmpfs that masks the root.
type bindPath struct {
	dest   string
	source string
}

// sandboxBaseView is everything an app sees of the host besides its
// own directories: the parts of the OS a program needs in order to
// execute at all. Under TemporaryFileSystem=/ this list, plus the
// app's own dirs and its extra_paths, *is* the view — anything not
// named here is absent, not merely unreadable.
//
// /etc is named entry by entry rather than bound whole. Binding /etc
// would hand every app every other app's env_file and hotserve's own
// configuration, which is precisely the derived, ageing hidden set
// this model exists to delete; naming the dozen entries a runtime
// actually needs keeps the guarantee free of exceptions.
//
// Every entry is bound with IgnoreENOENT: the list spans four distros
// and no single one has all of it (a merged-/usr host has no real
// /lib64, Debian has no /etc/pki, a host without systemd-resolved has
// no /run/systemd/resolve). A missing entry must not fail the unit.
var sandboxBaseView = []string{
	"/usr",
	// The usrmerge aliases: symlinks into /usr on modern distros, real
	// directories on older ones. Bound by name so a #! line, a
	// hard-coded /bin/sh or an interpreter's own ld path resolves to
	// the spelling it was written with.
	"/bin", "/sbin", "/lib", "/lib64", "/lib32", "/libx32",
	// The TLS trust store — the certificate directories themselves, not
	// the trees that contain them. /etc/ssl also holds /etc/ssl/private,
	// which nothing in an app needs: it is 0700 root:root on every cell
	// of the matrix, so binding /etc/ssl disclosed nothing today, but a
	// base view should name what apps need rather than rely on the
	// permissions of what it sweeps up. Recursive, so the hashed
	// symlinks under certs/ resolve into the /usr bound above.
	"/etc/ssl/certs", "/etc/ssl/openssl.cnf",
	"/etc/ca-certificates", "/etc/pki/tls/certs",
	// Name resolution and user lookup: getaddrinfo reads all four, and
	// without them every DNS lookup and every getpwuid fails.
	"/etc/resolv.conf", "/etc/hosts", "/etc/hostname", "/etc/nsswitch.conf",
	"/etc/passwd", "/etc/group",
	"/etc/localtime", "/etc/alternatives",
	// The dynamic linker's cache and configuration: without ld.so.cache
	// every dynamically-linked binary falls back to a search that
	// misses the distro's multiarch directories.
	"/etc/ld.so.cache", "/etc/ld.so.conf", "/etc/ld.so.conf.d",
	// systemd-resolved hosts symlink /etc/resolv.conf into here; if the
	// target is missing the link dangles and DNS fails silently on the
	// most common distro in the matrix.
	"/run/systemd/resolve",
}

// sandboxNeverBound are the names an extra_path may never take.
//
// With a deny-by-default view this list is not what makes the sandbox
// hold — nothing is in a unit's view unless something names it, so a
// path missing here is still absent. What the list does is refuse the
// names that, once named, would give back what the sandbox just took.
//
// "Once named" is the whole of it, and it is why these literals are
// not the last word: a mandatory bind that RESOLVES onto a protected
// path names it just as surely as an operator typing it. See
// protectedFromBinds, which is what the checks actually use — this
// list is its static floor, canonicalised and extended there with the
// directories this hotserve is really pointed at.
//
// The sharpest case is /run/user: an extra_path there returns the user
// manager's private socket, and since PrivateUsers= maps the app's uid
// one-to-one, the app could then ask the manager to start a unit of
// its own with no sandbox at all. Read-only does not help — connecting
// to a unix socket is not a filesystem write. `sandbox off` is the
// escape hatch for a workload that genuinely needs one of these.
var sandboxNeverBound = append(append([]string{}, sandboxHotservePaths...),
	// The user manager's private socket and session bus, and the home
	// directories of every same-UID neighbour.
	"/home", "/root", "/run/user",
	// The kernel interfaces and scratch space the unit gets its own
	// copies of (PrivateTmp=, PrivateDevices=, the API VFS): a bind
	// here would hand back the host's, nested into the unit's own.
	"/tmp", "/var/tmp",
	"/dev", "/sys", "/proc",
)

// sandboxHotservePaths is the supervisor's own state on a packaged
// install: TLS keys and certificates, the admin socket's directory,
// the documented env-file directories. It is the subset of
// sandboxNeverBound that is a mistake to put the liveswap *root*
// inside of, not merely to name in an extra_path.
var sandboxHotservePaths = []string{
	"/var/lib/hotserve", "/run/hotserve", "/etc/hotserve", "/etc/liveswap",
}

// sandboxSpec is what the runner needs to render the sandbox
// properties of one unit. Paths are real host paths; nothing is
// remapped.
type sandboxSpec struct {
	tier sandboxTier
	// root is the liveswap root. It is not masked as such any more —
	// the whole filesystem is — but it is still what the containment
	// checks measure against: a bind that reaches under it is one app
	// reaching into another's data.
	root string
	// writable are this app's own directories bound back at their real
	// paths, read-write (BindPaths=): the release being started and
	// the shared dir. Its state.json, its tmp/ (the upload staging dir
	// — a running instance must never be able to rewrite the next
	// version's tarball) and its other releases are simply not named,
	// so they do not exist inside the unit.
	writable []bindPath
	extra    []extraPath
	// appDir is this app's own directory under the root, and appName
	// the name it was configured with: a writable bind that resolves
	// inside the root must stay inside *this* app's directory, or one
	// app would be binding another's data.
	appDir  string
	appName string
}

// resolveBindSources resolves every path this unit will bind and
// re-checks it, immediately before the unit is created — the last
// moment before systemd follows these paths, and the only check that
// sees what they point at *now*.
//
// The mandatory binds need this as much as extra_path does, and are
// the easier target: `shared` and the release dirs live under the
// shared hotserve UID, and appDirs.ensure's MkdirAll succeeds on a
// symlink. An app running bare — `sandbox off`, a host stuck at the
// none tier, or simply an app that has not had its first sandboxed
// deploy yet, which is the documented rollout — can therefore replace
// its own `shared` with a link to /run/user/<uid> and have BindPaths=
// mount the user manager's private socket inside the sandbox on the
// next launch, while the status endpoint reports it as sandboxed.
//
// Resolution is allowed and useful (an operator may legitimately put
// `shared` on another disk); what is refused is a resolution that
// lands somewhere the sandbox exists to keep out. Paths that do not
// resolve — an extra_path for a socket directory that appears later —
// are left as written, having already passed the same checks
// lexically at config load.
//
// Residual, stated rather than papered over: this is a check on a
// pathname, and between it and the manager following that pathname a
// process sharing the hotserve UID can swap what it points at. During
// the documented bare-to-sandbox rollout the old bare instance is
// still running (a deploy stops it only once the new one is healthy),
// so that process may be the very app being sandboxed. Nothing a
// supervisor can do to a pathname closes this while it and its apps
// share a UID — the check has to hold an object, or the app has to
// stop being the same principal. It is the shared-UID rule again (see
// DESIGN-threat-model.md), and per-app UIDs are what closes it; a
// sandboxed app cannot reach the mount points at all, so the exposure
// is bounded to apps that are already running unsandboxed on a box
// the threat model treats as one trust domain until then.
func (s *sandboxSpec) resolveBindSources() error {
	// Compare canonical against canonical: a resolved bind source is
	// canonical, so a lexical root would miss a sibling reached through
	// a link when the root itself is one (/srv/liveswap ->
	// /mnt/liveswap: a planted shared -> /mnt/liveswap/shop/shared
	// resolves *outside* the lexical root and the sibling check would
	// never fire). Unresolvable falls back to the lexical value.
	rootC := s.root
	if c, err := filepath.EvalSymlinks(s.root); err == nil {
		rootC = c
	}
	// The expected app directory is derived from the canonical root and
	// the configured app name, NOT from what appDir itself resolves to.
	// Resolving appDir would let the alias vouch for itself: a bare app
	// that replaces <root>/blog with a link to <root>/shop would make
	// shop's directory the expected base, and both of blog's mandatory
	// binds would then match it exactly — handing the sibling's release
	// and shared data to the new sandbox.
	appC := filepath.Join(rootC, s.appName)
	own := func(dest, resolved string) error {
		// Inside the root, the only difference a mandatory bind may
		// have from its configured path is one an ancestor's symlink
		// explains — the root being a link. Resolving to some *other*
		// place under the root is the app pointing at data that is not
		// the directory we meant to bind: its own app dir or releases/
		// (whose recursive bind would carry state.json, the upload
		// staging dir and every other release in under `shared`), or a
		// sibling's. Outside the root entirely — `shared` moved to
		// another disk — is legitimate, subject to the checks below.
		rel, err := filepath.Rel(s.appDir, dest)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%q is not inside the app's own directory %s", dest, s.appDir)
		}
		expected := filepath.Join(appC, rel)
		if resolved == expected {
			// It IS the directory it names, reached through an alias on
			// the root itself. Nothing further to check, and in
			// particular NOT closedPath: validateSandboxRoot
			// deliberately allows a root under /tmp, /var/tmp or /home
			// (the binds nest into the unit's own private copies, and
			// the integration lane runs from /var/tmp), so refusing here
			// would make a symlinked root fail every launch while the
			// same root spelled directly works.
			return nil
		}
		if overlaps(resolved, s.root) || overlaps(resolved, rootC) {
			return fmt.Errorf("%q resolves to %q, which is not the directory it names and overlaps the liveswap root %s (expected %q, or a path outside the root entirely)", dest, resolved, s.root, expected)
		}
		// Outside the root: `shared` legitimately moved to another disk.
		// Here the deny list does apply — this is a path the operator
		// (or a bare app's planted symlink) chose, not one the root's
		// own spelling implies.
		return closedPath(resolved)
	}
	for i, b := range s.writable {
		resolved, err := filepath.EvalSymlinks(b.dest)
		if err != nil {
			return fmt.Errorf("resolving %q before binding it into the sandbox: %w", b.dest, err)
		}
		if resolved != b.dest {
			if err := own(b.dest, resolved); err != nil {
				return fmt.Errorf("refusing to launch sandboxed: %w", err)
			}
			// Only the source moves: the app must still find its
			// release and data where its command line, HOME and
			// placeholders say they are.
			s.writable[i].source = resolved
		}
	}
	for i, e := range s.extra {
		resolved, err := filepath.EvalSymlinks(e.path)
		if err != nil || resolved == e.path {
			continue // absent (checked as written at config load), or itself
		}
		if err := checkExtraPathContainment(resolved, s.root, rootC); err != nil {
			return fmt.Errorf("refusing to launch sandboxed: extra_path %q resolves to %q: %w", e.path, resolved, err)
		}
		s.extra[i].source = resolved
	}
	return nil
}

// closedPath refuses a path that overlaps anything an app may never be
// given — in either direction. Being *inside* a protected path is the
// obvious case; *containing* one is just as bad, because these binds
// are recursive: `shared -> /run` would carry /run/user (the manager's
// socket) and /run/hotserve (the admin socket) in with it, and
// `shared -> /etc` every configured env file.
func closedPath(p string) error {
	for _, c := range protectedFromBinds() {
		if overlaps(p, c) {
			return fmt.Errorf("%q overlaps %s, which no app may be given", p, c)
		}
	}
	// The base view is the one part of the host every unit shares, so a
	// bind source inside it is readable by every OTHER app whether or
	// not this app binds it — `shared -> /usr/local/app-data` would put
	// this app's data somewhere all its siblings can read, silently and
	// with the status endpoint still reporting the tier as applied. The
	// app-to-app boundary is exactly what the sandbox exists to build,
	// so this is a configuration error, not a choice.
	for _, b := range sandboxBaseView {
		if overlaps(p, b) {
			return fmt.Errorf("%q overlaps %s, which is bound read-only into EVERY app's sandbox — data placed there is readable by every other app; put it somewhere outside the base view", p, b)
		}
	}
	return nil
}

// protectedFromBinds is what a bind may never name: sandboxNeverBound
// plus this supervisor's *actual* state directories, each in both its
// configured and its canonical spelling.
//
// The static list is not sufficient here, and this is the one place the
// distinction matters. A path missing from it is still absent from
// every unit — nothing binds it — so for the VIEW the list is advisory.
// But a mandatory bind that resolves onto such a path *names* it, and
// naming is exactly what puts something in the view. Two ways that bit:
// `/var/lib/hotserve -> /srv/hotserve` made the lexical entry miss a
// planted `shared -> /srv/hotserve`, and a hotserve pointed at a
// non-default XDG data dir has its keys somewhere no literal mentions
// at all. Both hand the supervisor's TLS keys to an app that plants one
// symlink while running bare.
//
// Resolved on each call rather than cached: this runs at most twice per
// launch (only for a bind whose source moved), and the answer must be
// what the filesystem says now, not what it said at config load — the
// same reason resolveBindSources exists at all.
func protectedFromBinds() []string {
	out := make([]string, 0, len(sandboxNeverBound)+8)
	add := func(p string) {
		if p == "" {
			return
		}
		if !filepath.IsAbs(p) {
			abs, err := filepath.Abs(p)
			if err != nil {
				return
			}
			p = abs
		}
		p = filepath.Clean(p)
		out = append(out, p)
		if c, err := filepath.EvalSymlinks(p); err == nil && c != p {
			out = append(out, c)
		}
	}
	for _, p := range sandboxNeverBound {
		add(p)
	}
	// Caddy's data and config dirs follow XDG_DATA_HOME/XDG_CONFIG_HOME,
	// so the TLS keys and the config autosave are not always where the
	// packaged layout puts them.
	add(caddy.AppDataDir())
	add(caddy.AppConfigDir())
	// systemd sets RUNTIME_DIRECTORY for RuntimeDirectory= (a
	// colon-separated list when there is more than one): the admin
	// socket, as the manager actually made it.
	for _, d := range filepath.SplitList(os.Getenv("RUNTIME_DIRECTORY")) {
		add(d)
	}
	return out
}

// overlaps reports whether two paths are the same or either contains
// the other. Containment in one direction hides a path; in the other
// it carries it along.
func overlaps(a, b string) bool { return pathWithin(a, b) || pathWithin(b, a) }

// sandboxSpecFor is the sandbox of one instance of this app at the
// given tier: its release being started and its shared dir writable,
// its extra paths, and the base view. Everything else on the host —
// every other app, every other release, hotserve's own state — is
// absent because nothing names it. nil for none.
func (s *appSpec) sandboxSpecFor(releaseDir string, tier sandboxTier) *sandboxSpec {
	if tier == sandboxNone {
		return nil
	}
	return &sandboxSpec{
		tier:     tier,
		root:     s.dirs.root,
		appDir:   s.dirs.app,
		appName:  s.name,
		writable: []bindPath{{dest: releaseDir, source: releaseDir}, {dest: s.dirs.shared, source: s.dirs.shared}},
		// Copied: resolveBindSources rewrites entries to what they
		// resolve to, and the spec must not mutate the app's config.
		extra: append([]extraPath{}, s.extraPaths...),
	}
}

// warnSandboxTier logs, at every launch, when an app that wants a
// sandbox is getting less than the full one — the design's "prominent
// WARN at every spawn", so a degraded host is never a quiet fact.
func warnSandboxTier(c collaborators, spec *appSpec, tier sandboxTier) {
	if spec.sandboxMode == sandboxOff || tier == sandboxFull {
		return
	}
	c.logger.Warn("launching without the full sandbox",
		zap.String("tier", tier.String()),
		zap.String("residual", sandboxResidual(tier)))
}

// sandboxResidual states what a tier leaves open, for the WARN.
func sandboxResidual(tier sandboxTier) string {
	switch tier {
	case sandboxFilesystem:
		return "same-UID processes (hotserve, the user manager, sibling apps) remain visible and signalable; files, /proc and the admin and manager sockets are closed"
	default:
		return "no per-unit sandbox: the app shares the hotserve UID's files, sockets and processes (non-dumpable supervisor and NoNewPrivileges only)"
	}
}

// probeSandboxSpec is the spec the capability probe runs with: the
// tier under test with the same deny-by-default view as a real app
// unit, and nothing of its own bound into it. The probe needs no root
// and no per-config state — the view is a policy, not a derivation,
// so there is nothing about this host's configuration for it to
// reproduce.
func probeSandboxSpec(tier sandboxTier) *sandboxSpec {
	return &sandboxSpec{tier: tier}
}

// sandboxProbeCommand is the command the capability probe runs inside
// a unit built with probeSandboxSpec. It checks the namespaces are in
// effect — not merely that the manager accepted the properties: the
// manager ignores PrivatePIDs= on a kernel without PID namespaces, and
// a user namespace the kernel or an LSM (Ubuntu's AppArmor userns
// restriction) refuses fails the unit outright. In a user namespace
// the process's own uid_map covers a single id, not the 2^32-1 of the
// initial namespace; in a PID namespace the executed process is PID 1.
func sandboxProbeCommand(tier sandboxTier) []string {
	// The echo lands in the journal under the probe unit's identifier
	// (hotserve-sandbox-probe), so a degraded host can be diagnosed
	// from `journalctl -t hotserve-sandbox-probe`. systemd applies its
	// own escaping to ExecStart arguments before the shell sees them:
	// "$$" becomes "$" (systemd.service, "Command lines"), so the
	// shell's own pid is spelled "$$$$" here; "$n" inside a word is
	// left alone.
	script := `read _ _ n < /proc/self/uid_map; echo "sandbox probe (` + tier.String() + `): uid_map range $n, pid $$$$"; [ "$n" != 4294967295 ]`
	if tier == sandboxFull {
		script += ` && [ "$$$$" = 1 ]`
	}
	return []string{"/bin/sh", "-c", script}
}

// sandboxProbeTimeout bounds one probe unit: a shell that exits at
// once, plus the manager's namespace setup.
const sandboxProbeTimeout = 30 * time.Second

// probeSandboxCapability finds the best tier the host delivers, from
// the top down: full needs a manager ≥ 256 (PrivatePIDs= is otherwise
// an unknown property) and a kernel that honours it; filesystem needs
// the user namespace. Each candidate is proven by running
// sandboxProbeCommand inside a unit built with that tier — a unit the
// manager refuses (a kernel or LSM that denies the namespaces), or a
// probe that exits non-zero (a property silently ignored), fails the
// candidate with the reason kept for the WARN and the require error.
//
// The probe also proves the base view: /bin/sh has to exist inside the
// unit for the script to run at all, so a host whose runtime the base
// view fails to name cannot report a tier it does not have.
func probeSandboxCapability(r runner, managerVersion int) sandboxCapability {
	var reasons []string
	try := func(tier sandboxTier) bool {
		ctx, cancel := context.WithTimeout(context.Background(), sandboxProbeTimeout)
		defer cancel()
		err := r.RunOnce(ctx, startSpec{
			app:     "sandbox-probe",
			version: tier.String(),
			command: sandboxProbeCommand(tier),
			dir:     "/",
			env:     []string{"PATH=/usr/bin:/bin"},
			grace:   5 * time.Second,
			sandbox: probeSandboxSpec(tier),
			probe:   true,
		})
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("%s tier: %v", tier, err))
			return false
		}
		return true
	}
	switch {
	case managerVersion >= 256:
		if try(sandboxFull) {
			return sandboxCapability{tier: sandboxFull}
		}
	case managerVersion == 0:
		reasons = append(reasons, "full tier: the user manager's systemd version is unknown, PrivatePIDs= needs 256")
	default:
		reasons = append(reasons, fmt.Sprintf("full tier: the user manager is systemd %d, PrivatePIDs= needs 256", managerVersion))
	}
	if try(sandboxFilesystem) {
		return sandboxCapability{tier: sandboxFilesystem, reason: strings.Join(reasons, "; ")}
	}
	return sandboxCapability{tier: sandboxNone, reason: strings.Join(reasons, "; ")}
}

// validateExtraPath enforces what an extra_path may name: an absolute,
// clean path that is neither inside the liveswap root (whose other
// contents are exactly what the sandbox exists to keep out; the app's
// own directories are already in its view) nor one of the names that
// would give back what the sandbox took. The refusal names the reason
// so an operator can tell a typo from a boundary.
func validateExtraPath(p, root string) error {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return fmt.Errorf("extra_path %q must be an absolute, clean path", p)
	}
	if p == "/" {
		return errors.New("extra_path / would expose the whole host")
	}
	// The root's own canonical spelling, for the containment checks: a
	// resolved extra_path is canonical, so comparing it only against a
	// lexical root would miss a sibling reached the other way round
	// (with /srv/liveswap -> /mnt/liveswap, `extra_path
	// /mnt/liveswap/shop/shared` already resolves to itself and would
	// pass, then bind the sibling back). This is the same
	// canonicalisation resolveBindSources does for mandatory binds.
	rootC := root
	if c, err := filepath.EvalSymlinks(root); err == nil {
		rootC = c
	}
	// Follow symlinks before the containment checks: they are lexical,
	// and BindPaths= binds what the path resolves to, so a link such as
	// /srv/db-socket -> /run/user/<uid> would otherwise walk straight
	// past them and hand back the very socket they exist to protect.
	// A path that does not resolve (not created yet — /run/postgresql
	// before postgres starts) is checked as written; and a link swapped
	// between config load and unit start is not covered here, which
	// needs write access outside every sandbox to arrange.
	if resolved, err := filepath.EvalSymlinks(p); err == nil && resolved != p {
		if err := checkExtraPathContainment(resolved, root, rootC); err != nil {
			return fmt.Errorf("extra_path %q resolves to %q: %w", p, resolved, err)
		}
	}
	return checkExtraPathContainment(p, root, rootC)
}

// checkExtraPathContainment is the lexical half of validateExtraPath,
// applied to the path as written and to what it resolves to. root and
// rootC are the liveswap root's configured and canonical spellings;
// either one reaching the path is a refusal.
func checkExtraPathContainment(p, root, rootC string) error {
	// Overlap in either direction: an extra_path *inside* the root
	// exposes another app's data, and one *containing* the root carries
	// the whole tree in, since the binds are recursive.
	if overlaps(p, root) || overlaps(p, rootC) {
		return fmt.Errorf("extra_path %q overlaps the liveswap root %s, which no app sees beyond its own release and shared dirs", p, root)
	}
	for _, c := range protectedFromBinds() {
		if overlaps(p, c) {
			return fmt.Errorf("extra_path %q overlaps %s, which no app may be given — binding it back would undo the sandbox for this app; use `sandbox off` if the app genuinely needs it", p, c)
		}
	}
	return nil
}

// validateSandboxRoot rejects a liveswap root inside hotserve's own
// state: the root holds the apps' own data, and putting it inside the
// supervisor's TLS keys or env files is a configuration mistake worth
// failing on, sandbox or no sandbox.
//
// A root inside one of the *other* sandboxNeverBound entries is
// deliberately NOT refused: systemd creates the mount points for
// TemporaryFileSystem= and BindPaths= inside the namespace, so a root
// under /tmp, /var/tmp or /home is masked with everything else and the
// app's own dirs bind back into it exactly as they do under /var/lib.
// The real-systemd integration lane runs with a /var/tmp root and
// asserts the full tier. Refusing those would turn an odd-but-working
// setup into a server that will not start.
func validateSandboxRoot(root string) error {
	// Both spellings: the check is lexical, so `/srv/liveswap ->
	// /var/lib/hotserve/apps` would otherwise walk straight past it.
	// An unresolvable root (not created yet, which is the normal case
	// at first config load) falls back to the lexical value.
	roots := []string{root}
	if c, err := filepath.EvalSymlinks(root); err == nil && c != root {
		roots = append(roots, c)
	}
	for _, r := range roots {
		for _, h := range sandboxHotservePaths {
			if pathWithin(r, h) {
				return fmt.Errorf("root %q is inside %s, which holds hotserve's own keys, sockets and env files; use a root outside it (the default is /var/lib/liveswap)", root, h)
			}
		}
		// A root inside the base view would put EVERY app's state and
		// every app's data under a tree bound read-only into every
		// other app's sandbox — the app-to-app boundary gone wholesale,
		// which is worse than the single-app case closedPath refuses.
		for _, b := range sandboxBaseView {
			if pathWithin(r, b) {
				return fmt.Errorf("root %q is inside %s, which is bound read-only into every app's sandbox — every app's data would be readable by every other; use a root outside the base view (the default is /var/lib/liveswap)", root, b)
			}
		}
	}
	return nil
}

// pathWithin reports whether p equals dir or lies beneath it.
func pathWithin(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

// inView reports whether p exists inside a unit built from this spec:
// under one of the app's own directories, in the base view, or named
// by an extra_path. Everything else on the host is absent, so this is
// the whole of the view — the same three sources sandboxProperties
// renders, and the reason it can be answered without asking systemd.
func (s *sandboxSpec) inView(p string) bool {
	// Destination AND source. The caller resolves symlinks before
	// asking, and a resolved path is canonical, so a bind whose source
	// differs from its destination — a symlinked liveswap root, or an
	// extra_path that resolves elsewhere — is only recognisable by its
	// source. Testing destinations alone would refuse `./server` under
	// a root like /srv/liveswap -> /mnt/liveswap on every launch.
	both := func(dest, source string) bool {
		if source == "" {
			source = dest
		}
		return pathWithin(p, dest) || pathWithin(p, source)
	}
	for _, b := range s.writable {
		if both(b.dest, b.source) {
			return true
		}
	}
	for _, e := range s.extra {
		if both(e.path, e.source) {
			return true
		}
	}
	for _, base := range sandboxBaseView {
		if pathWithin(p, base) {
			return true
		}
	}
	return false
}

// warnEnvFileInView warns about the one place an env_file can still
// land inside a sandbox view: the app's own directories, which are the
// only host paths bound writable. An app's own environment is
// legitimately reachable — protecting an app from itself is a stated
// non-goal — but the *file* is a different thing from the variables.
// Under `shared/` the app can rewrite it and so choose the environment
// of its own next launch, which outlives the compromise that arranged
// it. Everywhere else an env_file is simply absent from every view,
// which is why this is the whole of what used to be a derived,
// per-config hidden set.
func warnEnvFileInView(logger *zap.Logger, specs map[string]*appSpec) {
	if logger == nil {
		return
	}
	for name, spec := range specs {
		if spec.envFile == "" || spec.sandboxMode == sandboxOff {
			continue
		}
		f := spec.envFile
		if !filepath.IsAbs(f) {
			abs, err := filepath.Abs(f)
			if err != nil {
				continue
			}
			f = abs
		}
		f = filepath.Clean(f)
		switch {
		case pathWithin(f, spec.dirs.shared):
			logger.Warn("env_file is inside the app's shared dir, which the app can read and rewrite inside its sandbox",
				zap.String("app", name), zap.String("env_file", f),
				zap.String("fix", "move it outside the liveswap root (the documented location is /etc/hotserve), where no app can see it"))
		case pathWithin(f, spec.dirs.releases):
			logger.Warn("env_file is inside the app's release dir, so the app can read the file as well as receive its variables",
				zap.String("app", name), zap.String("env_file", f),
				zap.String("fix", "move it outside the liveswap root (the documented location is /etc/hotserve), where no app can see it"))
		}
	}
}
