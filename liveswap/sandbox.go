package liveswap

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

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

// sandboxHiddenPaths are never inside an app's view: hotserve's own
// state (TLS keys, certificates), the admin socket, and the env-file
// directories. InaccessiblePaths= with the "-" prefix, so a path that
// does not exist on this host is not an error.
var sandboxHiddenPaths = []string{
	"/var/lib/hotserve",
	"/run/hotserve",
	"/etc/hotserve",
	"/etc/liveswap",
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
// tier under test and the root, nothing exposed.
func probeSandboxSpec(tier sandboxTier, root string) *sandboxSpec {
	return &sandboxSpec{tier: tier, root: root}
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
func probeSandboxCapability(r runner, managerVersion int, root string) sandboxCapability {
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
			sandbox: probeSandboxSpec(tier, root),
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
// clean path that is not inside the hidden set or the liveswap root
// (the root's other contents are exactly what the sandbox exists to
// hide; an app's own directories are already in its view).
func validateExtraPath(p, root string) error {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return fmt.Errorf("extra_path %q must be an absolute, clean path", p)
	}
	if p == "/" {
		return errors.New("extra_path / would expose the whole host")
	}
	for _, h := range append([]string{root}, sandboxHiddenPaths...) {
		if pathWithin(p, h) {
			return fmt.Errorf("extra_path %q is inside %s, which is hidden from every app", p, h)
		}
	}
	return nil
}

// validateSandboxRoot rejects a liveswap root inside a hidden path:
// the hidden overmount would sit above the root and no app directory
// could be bound back into view.
func validateSandboxRoot(root string) error {
	for _, h := range sandboxHiddenPaths {
		if pathWithin(root, h) {
			return fmt.Errorf("root %q is inside %s, which the sandbox hides from every app; use a root outside it (the default is /var/lib/liveswap)", root, h)
		}
	}
	return nil
}

// pathWithin reports whether p equals dir or lies beneath it.
func pathWithin(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}
