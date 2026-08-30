package liveswap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
}

// sandboxHiddenFloor is the packaged install's layout: hotserve's own
// state (TLS keys, certificates), the admin socket's directory, and the
// documented env-file directories. It is a floor, not the whole set —
// hiddenPaths adds what this process actually uses, because none of
// these paths is fixed by anything but convention.
var sandboxHiddenFloor = []string{
	"/var/lib/hotserve",
	"/run/hotserve",
	"/etc/hotserve",
	"/etc/liveswap",
}

// hiddenPaths is what no app may see, for this running configuration:
// the floor above plus the directories this hotserve was actually
// pointed at — Caddy's data and config dirs (TLS keys and the config
// autosave, which follow XDG_DATA_HOME/XDG_CONFIG_HOME), the systemd
// runtime directory (RuntimeDirectory=hotserve — the admin socket),
// and every app's env_file, each hidden as the file it is so an
// operator's `/etc/blog/blog.env` does not require hiding all of
// `/etc/blog`. An app never legitimately reads its env *file*; it
// receives env *variables*. Rendered as InaccessiblePaths= with the
// "-" prefix, so a path absent on this host is not an error.
//
// Deterministic order: the floor, then the runtime dirs, then env
// files sorted — specEqual compares specs by their rendering.
func (a *App) hiddenPaths() []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p == "" || !filepath.IsAbs(p) || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, p := range sandboxHiddenFloor {
		add(p)
	}
	add(caddy.AppDataDir())
	add(caddy.AppConfigDir())
	// systemd sets RUNTIME_DIRECTORY for RuntimeDirectory= (a
	// colon-separated list when there is more than one).
	for _, d := range filepath.SplitList(os.Getenv("RUNTIME_DIRECTORY")) {
		add(d)
	}
	var envFiles []string
	for _, cfg := range a.Apps {
		if cfg.EnvFile != "" {
			envFiles = append(envFiles, cfg.EnvFile)
		}
	}
	sort.Strings(envFiles)
	for _, f := range envFiles {
		add(f)
	}
	return out
}

// sandboxClosedPrefixes are the paths the sandbox options take away by
// themselves: ProtectHome=tmpfs (/home, /root, /run/user — the user
// manager's private socket and session bus), PrivateTmp= (/tmp,
// /var/tmp), PrivateDevices= (/dev), ProtectControlGroups= and
// ProtectKernelTunables= (/sys), PrivatePIDs=/MountAPIVFS (/proc).
//
// An extra_path may not name anything within them. BindPaths= nests
// *into* those overmounts — that is exactly why the app's own dirs can
// be bound back under the tmpfs that replaces the liveswap root — so a
// bind here would hand back precisely what the option closed, while
// the status endpoint still reported the tier as applied. The sharpest
// case: `extra_path /run/user/<uid>` returns the manager's private
// socket, and since PrivateUsers= maps the app's uid one-to-one, the
// app could then ask the manager to start a unit of its own with no
// sandbox at all. Read-only does not help: connecting to a unix socket
// is not a filesystem write. `sandbox off` is the escape hatch for a
// workload that genuinely needs one of these.
var sandboxClosedPrefixes = []string{
	"/home", "/root", "/run/user",
	"/tmp", "/var/tmp",
	"/dev", "/sys", "/proc",
}

// sandboxSpec is what the runner needs to render the sandbox
// properties of one unit. Paths are real host paths; nothing is
// remapped.
type sandboxSpec struct {
	tier sandboxTier
	// root is the liveswap root: an empty read-only tmpfs replaces it
	// in the unit's view (TemporaryFileSystem=), so sibling apps, this
	// app's state.json, its tmp/ (the upload staging dir — a running
	// instance must never be able to rewrite the next version's
	// tarball) and its other releases do not exist there.
	root string
	// writable are this app's own directories bound back at their real
	// paths, read-write (BindPaths=): the release being started and
	// the shared dir.
	writable []string
	extra    []extraPath
	// hidden is what no app may see on this host (App.hiddenPaths).
	hidden []string
}

// sandboxSpecFor is the sandbox of one instance of this app at the
// given tier: its release being started and its shared dir writable,
// its extra paths, everything else under the root gone. nil for none.
func (s *appSpec) sandboxSpecFor(releaseDir string, tier sandboxTier) *sandboxSpec {
	if tier == sandboxNone {
		return nil
	}
	return &sandboxSpec{
		tier:     tier,
		root:     s.dirs.root,
		writable: []string{releaseDir, s.dirs.shared},
		extra:    s.extraPaths,
		hidden:   s.sandboxHidden,
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
// tier under test over the same root and hidden set as a real app
// unit, nothing exposed.
func probeSandboxSpec(tier sandboxTier, root string, hidden []string) *sandboxSpec {
	return &sandboxSpec{tier: tier, root: root, hidden: hidden}
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
func probeSandboxCapability(r runner, managerVersion int, root string, hidden []string) sandboxCapability {
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
			sandbox: probeSandboxSpec(tier, root, hidden),
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
// contents are exactly what the sandbox exists to hide; the app's own
// directories are already in its view), nor inside the hidden set, nor
// inside anything the sandbox options close by themselves — a bind
// under any of those re-opens what the sandbox just shut, which is the
// "looks configured, quietly weaker" failure this feature must not
// have. The refusal names the reason so an operator can tell a typo
// from a boundary.
func validateExtraPath(p, root string, hidden []string) error {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return fmt.Errorf("extra_path %q must be an absolute, clean path", p)
	}
	if p == "/" {
		return errors.New("extra_path / would expose the whole host")
	}
	if pathWithin(p, root) {
		return fmt.Errorf("extra_path %q is inside the liveswap root %s, which the sandbox replaces with an empty tmpfs; an app already sees its own release and shared dirs", p, root)
	}
	for _, h := range hidden {
		if pathWithin(p, h) {
			return fmt.Errorf("extra_path %q is inside %s, which is hidden from every app (hotserve's own keys, sockets and env files)", p, h)
		}
	}
	for _, c := range sandboxClosedPrefixes {
		if pathWithin(p, c) {
			return fmt.Errorf("extra_path %q is inside %s, which the sandbox itself closes — binding it back would undo the sandbox for this app; use `sandbox off` if the app genuinely needs it", p, c)
		}
	}
	return nil
}

// validateSandboxRoot rejects a liveswap root inside a hidden path:
// the inaccessible overmount would sit above the root, no app
// directory could be bound back into view, and the root holds the
// apps' own data — putting it inside hotserve's private state is a
// configuration mistake worth failing on, sandbox or no sandbox.
func validateSandboxRoot(root string, hidden []string) error {
	for _, h := range hidden {
		if pathWithin(root, h) {
			return fmt.Errorf("root %q is inside %s, which the sandbox hides from every app; use a root outside it (the default is /var/lib/liveswap)", root, h)
		}
	}
	return nil
}

// A root inside one of sandboxClosedPrefixes is deliberately NOT
// refused: systemd creates the mount point for TemporaryFileSystem=
// and BindPaths= inside the namespace, so a root under /tmp or
// /var/tmp is masked and the app's own dirs bind back into it exactly
// as they do under /var/lib — the real-systemd integration lane runs
// with a /var/tmp root and asserts the full tier. Only hotserve's own
// state is a mistake worth failing on.

// pathWithin reports whether p equals dir or lies beneath it.
func pathWithin(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}
