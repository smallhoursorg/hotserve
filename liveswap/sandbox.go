package liveswap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
)

// Per-app sandboxing (issue #35) is systemd's own per-unit sandboxing
// on the user-manager runner: a user namespace (PrivateUsers=) plus a
// PID namespace (PrivatePIDs=, systemd 256+) on top of the mount,
// device, cgroup, seccomp and capability set.
//
// The user namespace is what closes /proc/<pid>/{root,environ,cwd} of
// every process outside it — commoncap refuses cross-user-namespace
// ptrace access whatever the uid — so the mount restrictions cannot be
// walked around via a same-UID neighbour's /proc. The PID namespace
// closes what is left: the supervisor, the user manager and every
// sibling become invisible and unsignalable.
//
// One tier, because the support matrix is one release: Debian 13 ships
// systemd 257 and does not restrict unprivileged user namespaces. A
// host that cannot deliver it is refused rather than given a lesser
// one: `sandbox on` fails the start, naming what the host lacks and
// `sandbox off` as the way to run without it. (An earlier "filesystem"
// tier — the user
// namespace without the PID namespace — was what Debian 12 and Ubuntu
// 24.04 could manage. It was briefly kept as a value state.json might
// already hold, until the release history was checked: v0.1.0 is the
// only tag and predates sandboxing entirely, so no released hotserve
// ever wrote it and there was no migration to protect.)
//
// The filesystem view is deny-by-default. An empty read-only tmpfs
// replaces the whole root (TemporaryFileSystem=/) and the only things
// that exist inside a unit are the ones this file names:
// sandboxBaseView and the app's own release and shared dirs. Nothing
// widens it. The guarantee is one sentence — anything not named is
// absent — and it holds by construction rather than by a list of
// things to hide being complete. Nothing has to be derived from the
// running configuration, and nothing ages: a secret that appears on
// the host after a unit started is absent from it for exactly the same
// reason every other path is.
//
// The policy (on/off) is resolved per app at Start against a host
// capability measured once per manager connection (not per config
// load: the measurement costs a whole unit — see
// userManagerClient.sandboxCapability); the tier an instance actually
// got is recorded with its handle in state.json so a relaunch
// reproduces it (engage on the next deploy, never on the upgrade
// relaunch — see liveswap/DESIGN-sandbox.md, rollout semantics).
//
// Because there is one tier and no lesser one to fall back to, the
// measurement decides whether hotserve starts at all rather than which
// tier it picks: a host that cannot deliver it fails Start with the
// probe's reason attached. That makes the sandbox an availability
// dependency, which is the deliberate trade — a supervisor that
// quietly ran every app with no isolation because the kernel changed
// its mind is the worse outcome. `sandbox off` is the escape, and
// every refusal names it.

// sandboxTier is what a unit actually gets.
type sandboxTier int

const (
	sandboxNone sandboxTier = iota // floor only: non-dumpable supervisor, NoNewPrivileges
	sandboxFull                    // user namespace + mount set + PID namespace; the one tier a host is probed for
)

func (t sandboxTier) String() string {
	switch t {
	case sandboxNone:
		return "none"
	case sandboxFull:
		return "full"
	default:
		// A tier this build does not define. state() persists whatever
		// this returns, so rendering it as "none" would let an
		// out-of-range value be written as a legitimate record and
		// relaunch a sandboxed app bare — fail-open, in the one enum
		// whose corruption is meant to fail closed. This spelling is
		// not a record validSandboxTierRecord accepts, so it does.
		return fmt.Sprintf("sandboxTier(%d)", int(t))
	}
}

// validSandboxTierRecord accepts the values state.json may legitimately
// hold for an instance's tier. "" is the important one: it is a record
// written before sandboxing existed, and reading it as none is the
// documented rollout contract — such an app relaunches bare and engages
// on its next deploy.
//
// Anything else non-empty is corruption or tampering, and reading THAT
// as none would relaunch the app with no sandbox at all, silently: a
// one-character typo (`"ful"`) would cost an app its isolation while
// the status endpoint honestly reported `none`. A syntactically corrupt
// state.json is already a permanent recovery error ("never silently
// reset", app.go); a semantically corrupt one is the same class of
// problem and gets the same answer, rather than the opposite one.
func validSandboxTierRecord(s string) error {
	switch s {
	case "", sandboxNone.String(), sandboxFull.String():
		return nil
	}
	return fmt.Errorf("recorded sandbox tier %q is not %q or %q (or empty, for a record written before sandboxing): refusing to relaunch, because reading it as %q would silently drop this app's sandbox",
		s, sandboxNone, sandboxFull, sandboxNone)
}

// parseSandboxTier is the inverse of String, for the persisted
// disposition. Anything unknown (including "") reads as none; callers
// that LAUNCH from a record must have validated it with
// validSandboxTierRecord first, so that only ever means the legacy
// empty value. Callers that merely describe a running unit (the status
// endpoint, Reattach) can take the lenient reading.
func parseSandboxTier(s string) sandboxTier {
	switch s {
	case "full":
		return sandboxFull
	default:
		return sandboxNone
	}
}

// Sandbox policy values, as configured. Two, not three: an `auto` that
// silently ran an app with no isolation on a host that could not
// deliver the full sandbox was the "looks configured, quietly weaker"
// failure this design refuses everywhere else. There is one supported
// tier, so the policy is whether to insist on it.
const (
	sandboxOn  = "on" // the full sandbox, or refuse to start
	sandboxOff = "off"
)

func validSandboxMode(s string) bool {
	return s == sandboxOn || s == sandboxOff
}

// sandboxModeError validates a configured mode, naming the two
// spellings this policy replaced rather than lumping them into the
// generic message: `auto` and `require` are all over the design docs
// and may well sit in an operator's working config. Nothing released
// ever accepted either — v0.1.0 is the only tag and predates
// sandboxing entirely — so this is a clear message on a config that
// was never valid, not a migration. "" is the default Provision fills
// in.
func sandboxModeError(mode string) error {
	if mode == "" || validSandboxMode(mode) {
		return nil
	}
	switch mode {
	case "auto":
		return fmt.Errorf("sandbox %q has been removed: on a host that could not deliver the sandbox it ran apps with none at all, which is the %q it was named against. Use %q (the default), which refuses to start instead, or %q to run without one deliberately",
			"auto", "looks configured, quietly weaker", sandboxOn, sandboxOff)
	case "require":
		return fmt.Errorf("sandbox %q is now spelled %q, and is the default", "require", sandboxOn)
	}
	return fmt.Errorf("sandbox must be %q or %q, got %q", sandboxOn, sandboxOff, mode)
}

// sandboxCapability is what the host can deliver. Measured by starting
// a real unit, so it is cached against the manager connection rather
// than taken again on every config load.
type sandboxCapability struct {
	tier   sandboxTier
	reason string // why tier is below full ("" when full)
}

// resolveSandboxTier applies the configured policy to the host's
// capability: the full tier, or a refusal naming what the host lacks
// and the one setting that runs without it. The error reaches the
// operator through App.Start, so it must carry the remedy — this is
// the message someone reads when the server will not come up.
func resolveSandboxTier(mode string, c sandboxCapability) (sandboxTier, error) {
	if mode == sandboxOff {
		return sandboxNone, nil
	}
	if c.tier != sandboxFull {
		return sandboxNone, fmt.Errorf("sandbox on: this host cannot deliver the full sandbox (%s); fix the host, or set `sandbox off` to run this app with no sandbox at all", c.reason)
	}
	return sandboxFull, nil
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
// app's own dirs, *is* the view — anything not
// named here is absent, not merely unreadable.
//
// /etc is named entry by entry rather than bound whole. Binding /etc
// would hand every app every other app's env_file and hotserve's own
// configuration, which is precisely the derived, ageing hidden set
// this model exists to delete; naming the dozen entries a runtime
// actually needs keeps the guarantee free of exceptions.
//
// Every entry is bound with IgnoreENOENT: no host has all of it
// (Debian 13 is merged-/usr, so /bin and /lib64 are symlinks rather
// than real directories and there is no /etc/pki; a host without
// systemd-resolved has no /run/systemd/resolve). A missing entry must
// not fail the unit. The names stay even where this distro does not
// use them: they are bound by name so a #! line or an interpreter's
// own ld path resolves to the spelling it was written with, and an
// app's tarball is not obliged to know which spelling its host uses.
var sandboxBaseView = []string{
	"/usr",
	// The usrmerge aliases: symlinks into /usr on Debian 13.
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

// sandboxNeverReachable is what nothing an app runs may hold, by any
// route: the user manager's private socket and session bus (with
// PrivateUsers= mapping the app's uid one-to-one, reaching it lets the
// app ask the manager for a unit with no sandbox at all — read-only is
// no defence, since connecting to a socket is not a write) and the
// kernel interfaces the unit gets its own.
var sandboxNeverReachable = []string{"/run/user", "/dev", "/sys", "/proc"}

// sandboxHotservePaths is the supervisor's own state on a packaged
// install: TLS keys and certificates, the admin socket's directory,
// the documented env-file directories: a mistake to put the liveswap
// *root* inside of, and never reachable as a bind source.
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
// The mandatory binds need this, and are
// the easier target: `shared` and the release dirs live under the
// shared hotserve UID, and appDirs.ensure's MkdirAll succeeds on a
// symlink. An app running bare — `sandbox off`, a host stuck at the
// none tier, or simply an app that has not had its first sandboxed
// deploy yet, which is the documented rollout — can therefore replace
// its own `shared` with a link to /run/user/<uid> and have BindPaths=
// mount the user manager's private socket inside the sandbox on the
// next launch, while the status endpoint reports it as sandboxed.
//
// A mandatory bind must therefore resolve to the directory it NAMES.
// The only difference permitted is one an alias on the liveswap root
// itself explains; anything else is refused, including targets that
// look innocent. That is not conservatism, it is the only line that
// can be drawn: the app's own directory is writable by the app, so a
// symlink placed there cannot be distinguished from one the operator
// meant. `shop/shared -> /mnt/shop-data` being a legitimate layout is
// exactly what made /mnt/shop-data an acceptable target for a bare
// `blog` to aim at, and no list of other apps' data and env_file
// locations can close that — it would be incomplete by construction
// and would go stale, which is the derived set this model deletes.
//
// An operator putting an app's data on another disk uses a bind mount
// at the same path (`mount --bind`, or an fstab entry). A mount
// resolves to itself, so it never reaches this check at all — and an
// app cannot forge one.
//
// Paths that do not resolve are left as written, having already passed
// the same checks lexically at config load.
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
		// The only difference a mandatory bind may have from its
		// configured path is one an alias on the root explains.
		// Anywhere else is the app pointing at data that is not the
		// directory we meant to bind: its own app dir or releases/
		// (whose recursive bind would carry state.json, the upload
		// staging dir and every other release in under `shared`), a
		// sibling's, or somewhere outside the root entirely — which
		// used to be permitted as "moved to another disk" and was the
		// hole that let one app alias another's external data.
		rel, err := filepath.Rel(s.appDir, dest)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%q is not inside the app's own directory %s", dest, s.appDir)
		}
		expected := filepath.Join(appC, rel)
		if resolved != expected {
			return fmt.Errorf("%q resolves to %q, which is not the directory it names (expected %q). A mandatory bind may differ from its configured path only by an alias on the liveswap root itself; to put an app's data on another disk, bind-mount it at %q instead of symlinking — a mount resolves to itself, and an app cannot forge one", dest, resolved, expected, dest)
		}
		// It IS the directory it names. The root was vetted at config
		// load, but rootC is read from the live filesystem every launch,
		// so re-check what it actually points at now.
		return refusedAsBindSource(resolved)
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
	return nil
}

// refusedAsBindSource refuses a resolved mandatory bind source that
// reaches something no app may hold, in either direction — being
// *inside* a protected path is the obvious case, *containing* one just
// as bad, since these binds are recursive.
//
// /tmp, /var/tmp and /home are deliberately NOT here: a liveswap root
// may legitimately live under one of them and every bind beneath it
// would then "overlap" it. A unit gets its own /tmp and /home anyway,
// so nothing of the host's leaks through them.
func refusedAsBindSource(p string) error {
	for _, c := range supervisorPaths() {
		if overlaps(p, c) {
			return fmt.Errorf("%q overlaps %s, which holds hotserve's own keys, sockets or config", p, c)
		}
	}
	for _, c := range sandboxNeverReachable {
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

// supervisorPaths is hotserve's own state, each entry in both its
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
func supervisorPaths() []string {
	out := make([]string, 0, len(sandboxHotservePaths)+8)
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
	for _, p := range sandboxHotservePaths {
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
// plus the base view. Everything else on the host —
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
	}
}

// warnSandboxTier logs, at every launch, when an app that wants a
// sandbox is getting less than the full one — the design's "prominent
// WARN at every spawn".
//
// With `sandbox on` refusing the start outright, the one way here is a
// relaunch reproducing an instance recorded bare (the rollout
// contract: sandboxing engages on the next deploy, never on the
// upgrade relaunch). That app is running unsandboxed while its config
// asks for a sandbox, which is exactly the state worth saying out loud
// every time.
func warnSandboxTier(c collaborators, spec *appSpec, tier sandboxTier) {
	if spec.sandboxMode == sandboxOff || tier == sandboxFull {
		return
	}
	c.logger.Warn("launching without the full sandbox",
		zap.String("tier", tier.String()),
		zap.String("residual", sandboxResidualNone))
}

// sandboxResidualNone states what the absence of a sandbox leaves
// open, for the WARN. There is one tier below full and it is none, so
// this is the only residual there is to name.
const sandboxResidualNone = "no per-unit sandbox: the app shares the hotserve UID's files, sockets and processes (non-dumpable supervisor and NoNewPrivileges only)"

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
// a user namespace the kernel or an LSM refuses fails the unit
// outright. In a user namespace the process's own uid_map covers a
// single id, not the 2^32-1 of the initial namespace; in a PID
// namespace the executed process is PID 1.
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

// probeSandboxCapability reports whether the host delivers the full
// sandbox. There is one supported tier, so there is one candidate: the
// support matrix is Debian 13 (systemd 257), where PrivatePIDs= exists
// and the kernel does not restrict unprivileged user namespaces.
//
// It stays a measurement rather than a version check because the host
// still gets a vote: a container or LXC VPS, a kernel built without
// user namespaces, or an LSM that refuses them all fail the unit on a
// manager whose version says it should have worked. A unit the manager
// refuses, or a probe that exits non-zero (a property silently
// ignored), fails with the reason kept for the WARN and the require
// error.
//
// The probe also proves the base view: /bin/sh has to exist inside the
// unit for the script to run at all, so a host whose runtime the base
// view fails to name cannot report a tier it does not have.
func probeSandboxCapability(r runner) sandboxCapability {
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
	if try(sandboxFull) {
		return sandboxCapability{tier: sandboxFull}
	}
	reasons = append(reasons, "the supported host is Debian 13 (systemd 257); PrivatePIDs= needs systemd 256 and both namespaces need a kernel that permits them")
	return sandboxCapability{tier: sandboxNone, reason: strings.Join(reasons, "; ")}
}

// effectiveSandbox is an app's policy after the global default and
// Provision's own default are applied — the same resolution buildSpec
// does. Reading the raw per-app field instead treats `sandbox off` set
// globally, which leaves every app's own field empty, as if every app
// were sandboxed.
func (a *App) effectiveSandbox(cfg *AppConfig) string {
	if cfg != nil && cfg.Sandbox != "" {
		return cfg.Sandbox
	}
	if a.Sandbox != "" {
		return a.Sandbox
	}
	return sandboxOn
}

// anySandboxed reports whether any app will actually get a view. The
// checks that exist only because a view is built — the base-view
// overlap rule below — must not fire when none is.
func (a *App) anySandboxed() bool {
	for _, cfg := range a.Apps {
		if cfg != nil && a.effectiveSandbox(cfg) != sandboxOff {
			return true
		}
	}
	return false
}

// validateSandboxRoot rejects a liveswap root inside hotserve's own
// state: the root holds the apps' own data, and putting it inside the
// supervisor's TLS keys or env files is a configuration mistake worth
// failing on, sandbox or no sandbox.
//
// A root under /tmp, /var/tmp or /home is deliberately NOT refused:
// systemd creates the mount points for
// TemporaryFileSystem= and BindPaths= inside the namespace, so a root
// under /tmp, /var/tmp or /home is masked with everything else and the
// app's own dirs bind back into it exactly as they do under /var/lib.
// The real-systemd integration lane runs with a /var/tmp root and
// asserts the full tier. Refusing those would turn an odd-but-working
// setup into a server that will not start.
func validateSandboxRoot(root string, sandboxed bool) error {
	// Both spellings: the check is lexical, so `/srv/liveswap ->
	// /var/lib/hotserve/apps` would otherwise walk straight past it.
	// An unresolvable root (not created yet, which is the normal case
	// at first config load) falls back to the lexical value.
	roots := []string{root}
	if c, err := filepath.EvalSymlinks(root); err == nil && c != root {
		roots = append(roots, c)
	}
	supervisor := supervisorPaths()
	for _, r := range roots {
		for _, h := range supervisor {
			if pathWithin(r, h) {
				return fmt.Errorf("root %q is inside %s, which holds hotserve's own keys, sockets and env files; use a root outside it (the default is /var/lib/liveswap)", root, h)
			}
		}
		// The base-view rules below exist only because a view is built.
		// With every app effectively `sandbox off` nothing binds /usr,
		// so refusing a root under it would reject a configuration that
		// was valid before per-app sandboxing existed — and `sandbox
		// off`, the documented way out, could not rescue it, because
		// config load fails first. The supervisor-state rule above is
		// unconditional: a root inside hotserve's keys is a mistake
		// with or without a sandbox.
		if !sandboxed {
			continue
		}
		// A root inside the base view would put EVERY app's state and
		// every app's data under a tree bound read-only into every
		// other app's sandbox — the app-to-app boundary gone wholesale,
		// which is worse than the single-app case closedPath refuses.
		for _, b := range sandboxBaseView {
			if pathWithin(r, b) {
				return fmt.Errorf("root %q is inside %s, which is bound read-only into every app's sandbox — every app's data would be readable by every other; use a root outside the base view (the default is /var/lib/liveswap)", root, b)
			}
			// Overlap is symmetric here as it is for binds: a root that
			// CONTAINS a base-view entry is the same exposure reached
			// from the other side, because an app's directory is derived
			// from the root and its name. `root /etc/ssl` passes the
			// check above, and then an app called `certs` puts its
			// releases and shared dir at /etc/ssl/certs — an entry every
			// sandbox binds read-only and recursively.
			if pathWithin(b, r) {
				return fmt.Errorf("root %q contains %s, which is bound read-only into every app's sandbox — an app whose name lands on that path would have its releases and shared dir readable by every other app; use a root that neither sits inside nor contains a base-view entry (the default is /var/lib/liveswap)", root, b)
			}
		}
	}
	return nil
}

// homeOutsideView reports an operator-set HOME that the unit's view
// does not contain. buildEnv applies the sandbox HOME (the shared dir)
// BEFORE env_file and inline env so an operator can point it elsewhere
// — inside the release dir, say — which is deliberate. What is not
// deliberate is doing it by accident: a HOME that is absent inside the
// unit surfaces as an ENOENT from whichever runtime touches $HOME
// first, naming no cause. The last assignment wins, as it does for
// systemd's Environment=.
func homeOutsideView(env []string, s *sandboxSpec) (string, bool) {
	if s == nil || s.tier == sandboxNone {
		return "", false // no view to be outside of
	}
	home := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "HOME="); ok {
			home = v
		}
	}
	if home == "" {
		return "", false
	}
	// Resolved, because the view is recognised by its bind sources and
	// a symlink would walk straight past a lexical comparison — the
	// same reason resolveBindSources canonicalises before checking.
	resolved := home
	if c, err := filepath.EvalSymlinks(home); err == nil {
		resolved = c
	}
	if s.inView(resolved) {
		return "", false
	}
	return home, true
}

// pathWithin reports whether p equals dir or lies beneath it.
func pathWithin(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

// inView reports whether p exists inside a unit built from this spec:
// under one of the app's own directories, or in the base view.
// Everything else on the host is absent, so this is
// the whole of the view — the same two sources sandboxProperties
// renders, and the reason it can be answered without asking systemd.
func (s *sandboxSpec) inView(p string) bool {
	// Destination AND source. The caller resolves symlinks before
	// asking, and a resolved path is canonical, so a bind whose source
	// differs from its destination — a symlinked liveswap root — is
	// only recognisable by its
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
		// Both spellings of both sides, as validateEnvFileIsolation
		// does: comparing lexically alone misses an env_file that is a
		// link into the app's own shared dir, and misses the canonical
		// spelling of one under a symlinked liveswap root. A warning
		// that a symlink walks past is worse than none — it reads as
		// "checked, and fine".
		within := func(dir string) bool {
			for _, f := range absAndCanonical(spec.envFile) {
				for _, d := range absAndCanonical(dir) {
					if pathWithin(f, d) {
						return true
					}
				}
			}
			return false
		}
		f := spec.envFile
		switch {
		case within(spec.dirs.shared):
			logger.Warn("env_file is inside the app's shared dir, which the app can read and rewrite inside its sandbox",
				zap.String("app", name), zap.String("env_file", f),
				zap.String("fix", "move it outside the liveswap root (the documented location is /etc/hotserve), where no app can see it"))
		case within(spec.dirs.releases):
			logger.Warn("env_file is inside the app's release dir, so the app can read the file as well as receive its variables",
				zap.String("app", name), zap.String("env_file", f),
				zap.String("fix", "move it outside the liveswap root (the documented location is /etc/hotserve), where no app can see it"))
		}
	}
}

// absAndCanonical returns a path made absolute and cleaned, plus its
// resolved form when that differs and resolves at all. Containment
// checks are lexical, so both spellings have to be compared or a
// symlink walks straight past them.
func absAndCanonical(p string) []string {
	if p == "" {
		return nil
	}
	if !filepath.IsAbs(p) {
		a, err := filepath.Abs(p)
		if err != nil {
			return nil
		}
		p = a
	}
	p = filepath.Clean(p)
	out := []string{p}
	if c := canonicalDeepest(p); c != p {
		out = append(out, c)
	}
	return out
}

// canonicalDeepest is EvalSymlinks for a path whose leaf may not exist
// yet: it resolves the deepest ancestor that does and re-appends the
// rest unchanged, falling back to p when nothing resolves.
//
// EvalSymlinks fails outright on a missing final component, and an
// env_file is explicitly allowed to be absent until the first deploy —
// so resolving the whole path would silently give back only the
// lexical spelling for exactly the configurations that need checking.
// With `/safe/link -> /var/lib/liveswap/blog/shared` and
// `env_file /safe/link/shop.env` not yet created, blog's own bind
// exposes that file the moment it appears, while a lexical comparison
// sees two unrelated strings. The link is in the PARENT, which is why the
// deepest existing ancestor is the thing to resolve.
func canonicalDeepest(p string) string {
	rest := ""
	for cur := p; ; {
		if c, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(c, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p // reached the root without resolving anything
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// validateEnvFileIsolation refuses a configuration in which one app's
// env_file lands inside ANOTHER app's view. An app's own env file being
// reachable is a stated non-goal to defend against — it receives those
// variables anyway — but a sibling's is the secret this whole feature
// exists to keep apart, and the documented guarantee says such a file
// is absent from every other app's view.
//
// Two routes make that false, neither of which the deny-by-default view
// closes on its own, because both are things the operator NAMED:
//
//   - the base view, which is bound read-only into every unit, so an
//     env_file under /usr or a named /etc entry is readable by every
//     app on the box;
//   - another app's own directories, which sandboxSpecFor binds into
//     its unit — `shared/` read-WRITE. An env file parked in a
//     neighbour's shared dir is readable and rewritable by that
//     neighbour, which is the same exposure by a shorter route.
//
// A hard error rather than a warning: the alternative is documenting
// that env files are only usually isolated. Validate runs before a
// reload is committed, so a configuration rejected here leaves the
// previous one serving.
//
// Every branch is judged against the EFFECTIVE policy, not the raw
// per-app field: `sandbox off` set globally leaves every app's own
// setting empty, and reading that as "not off" would reject a working
// config over views that are not being built.
func validateEnvFileIsolation(a *App) error {
	// The same resolution buildSpec does: the app's own setting, else
	// the global default, else auto (what Provision fills in).
	effective := func(cfg *AppConfig) string { return a.effectiveSandbox(cfg) }
	sandboxed := false
	for _, cfg := range a.Apps {
		if cfg != nil && effective(cfg) != sandboxOff {
			sandboxed = true
			break
		}
	}
	if !sandboxed {
		// No unit gets a view, so there is nothing for an env file to
		// land in: every app already sees the whole filesystem as the
		// hotserve uid, and refusing the config here would block a
		// working one over an exposure sandboxing did not create.
		return nil
	}
	for name, cfg := range a.Apps {
		if cfg == nil || cfg.EnvFile == "" {
			continue
		}
		for _, f := range absAndCanonical(cfg.EnvFile) {
			for _, b := range sandboxBaseView {
				if pathWithin(f, b) {
					return fmt.Errorf("app %s: env_file %q is inside %s, which is bound read-only into EVERY app's sandbox — every other app on this box could read it; keep env files outside the base view (the documented location is /etc/hotserve, which no app may name)", name, cfg.EnvFile, b)
				}
			}
			for other, ocfg := range a.Apps {
				// An app running unsandboxed reads the whole filesystem
				// as the hotserve uid; refusing this config would not
				// close anything for it.
				if other == name || ocfg == nil || effective(ocfg) == sandboxOff {
					continue
				}
				// The neighbour's own bound directories. Its release
				// dirs are read-only in its unit and its shared dir is
				// read-write, so this is the one route on which the
				// file can also be REWRITTEN by another app.
				od := newAppDirs(a.Root, other)
				for _, d := range []struct{ path, what, rw string }{
					{od.shared, "shared dir", "read and rewrite"},
					{od.releases, "release dirs", "read"},
				} {
					// Both spellings: the env file is compared
					// canonically, so a lexical
					// app dir alone would miss it whenever the root is a
					// symlink (/srv/liveswap -> /mnt/liveswap, an env
					// file configured as /mnt/liveswap/blog/shared/...).
					for _, dp := range absAndCanonical(d.path) {
						if pathWithin(f, dp) {
							return fmt.Errorf("app %s: env_file %q is inside app %s's %s %q, which is bound into %s's own sandbox — %s could %s it; move the env file outside the liveswap root (the documented location is /etc/hotserve)", name, cfg.EnvFile, other, d.what, d.path, other, other, d.rw)
						}
					}
				}
			}
		}
	}
	return nil
}
