# DESIGN — per-app sandboxing (M8)

Status: shipped (2026-08-30, #35 phase 1) as systemd's own per-unit
sandboxing on the user-manager runner, in two probe-gated tiers —
*filesystem* (user namespace + the mount/device/cgroup/seccomp/caps
set; systemd ≥ 252, every cell of the support matrix) and *full*
(filesystem + PID namespace; systemd ≥ 256: Debian 13, Ubuntu 26.04).
Resource caps are phase 2. This document was the implementation brief
for bubblewrap; what remains normative is the threat model, the
behaviour specification (amended below where the measured mechanism
differs), the config surface, the rollout/upgrade semantics and the
testing criteria. The bubblewrap mechanics ("Spawn path", "Signal
trap", "Probe and fallback ladder", "Packaging") are historical. The
implementation is `liveswap/sandbox.go` (tiers, policy, spec, probe)
and `sandboxProperties` in `liveswap/systemd_dbus.go`; the measured
basis is the 2026-08-30 spike comment on #35 and
[DESIGN-threat-model.md](../DESIGN-threat-model.md), "The shared-UID
rule".

## Why this feature exists

hotserve's v1 trust model is one box, one trust domain: every app runs
as the `hotserve` user, so any app can read every other app's files,
env files, the TLS private keys, and `/proc/<sibling>/environ`, and
can connect to the admin unix socket (same UID). The realistic
attacker is not a malicious tenant — it is a poisoned transitive
dependency (npm's 2025 wave: runtime payloads doing broad filesystem
sweeps for `.env` files, tokens and wallets, then exfiltrating).

Containers are the industry answer and the product explicitly rejects
them. Bubblewrap is the container *primitives* without the container
*product*: a ~50KB launcher from the Flatpak project that builds
mount/PID/user namespaces around a plain process. Apps stay files in
directories, supervised by hotserve — but each app's filesystem view
contains only its own world, and siblings are not merely unreadable,
they do not exist.

What this stops: lateral movement from one compromised app (sibling
files/env/releases, TLS keys, admin socket, sibling /proc, signals).
What this does NOT stop, stated honestly: theft of the compromised
app's own secrets (its env is its env), network exfiltration (netns is
shared by design), resource exhaustion (no cgroups — that is the v2
systemd runner tier), and install-time supply-chain attacks (those run
in CI, not on the box).

The mount namespace alone does not close the threat model. `buildEnv`
originally seeded every app's environment with hotserve's own
(`os.Environ()`) — which under the packaged unit includes any ACME DNS
tokens (and, before deploy auth went keyless, the deploy secret too).
The attacker we just described dumps `process.env` first; a stolen ACME
token lets them issue or alter certificates. That leak is FIXED ahead
of M8: `buildEnv` now inherits only an allowlist (PATH, HOME, LANG, TZ,
LC_*), pinned by `TestBuildEnvDoesNotLeakSupervisorSecrets`. (Deploy
auth is now asymmetric — no shared secret lives on the box at all; see
[DESIGN-threat-model.md](../DESIGN-threat-model.md).) The normative
requirement below stands as the contract the sandbox path must keep.

## Behavior specification (normative)

- Sandboxing MUST default to on (`sandbox auto`): the best tier the
  user manager and kernel deliver, probed at start by running a
  throwaway unit and checking the namespaces from inside (the manager
  silently ignores `PrivatePIDs=` on a kernel without PID namespaces,
  so accepting the property proves nothing). It MUST be configurable
  globally and per app: `auto` (best tier, with a prominent WARN at
  start and at every spawn that runs below *full*), `require` (start
  fails unless the *full* tier is available — a weaker tier accepted
  silently is the "looks configured, quietly weaker" trap), `off`.
- The sandboxed filesystem view MUST contain, at their REAL host paths
  (no remapping — `current` symlinks, state paths and operator
  debugging all assume real paths):
  - the app's release dir (rw), shared dir (rw), and the release
    being started;
  - a private `/tmp` (`PrivateTmp=`: a directory under the host's
    `/tmp`, removed when the unit stops). *Amended 2026-08-30:* the
    app's own `tmp/` (`appDirs.tmp`) is NOT bound — it is the upload
    staging dir, and a running instance that could see it could
    rewrite the next version's tarball before extraction.
  - read-only system paths (`ProtectSystem=strict`: `/usr`, `/etc`,
    `/boot`, `/efi` read-only, everything else as declared), plus a
    fresh `/proc` for the unit's PID namespace (`PrivatePIDs=`) and a
    minimal `/dev` (`PrivateDevices=`);
  - each configured `extra_path` (ro by default, rw when declared).
- The view MUST NOT contain: other apps' directories, `/var/lib`
  outside the app's own subtree (TLS keys live there), `/run/hotserve`
  (admin socket), `/run/user/<uid>` (the user manager's private
  socket and session bus; `ProtectHome=tmpfs` covers it, a tmpfs so
  an `extra_path` under `/home` can still be bound in), and
  `/etc/hotserve` (env files; apps never legitimately read their env
  *file*, they receive env *variables*). *Amended 2026-08-30:* the
  liveswap root is replaced by an empty read-only tmpfs
  (`TemporaryFileSystem=<root>:ro`) with the app's release and shared
  dirs bound back (`BindPaths=`); `ReadWritePaths=` nested under an
  `InaccessiblePaths=` parent does not re-open the child (measured),
  so that idiom is not used. hotserve's own paths are
  `InaccessiblePaths=` with the `-` prefix. `/sys/fs/cgroup` MUST be
  read-only (`ProtectControlGroups=`): the delegated subtree is owned
  by the app's own UID, so resource caps are otherwise rewritable.
- `/run/systemd/resolve` MUST stay reachable (read-only) when it
  exists: on systemd-resolved hosts (every Ubuntu in the support
  matrix) `/etc/resolv.conf` is a symlink into that directory, and if
  the masking of `/run` takes it away the link dangles — all DNS
  inside the sandbox fails, silently, on the most common distro. Local
  unix sockets (`/run/postgresql`, `/run/mysqld`, …) are deliberately
  not reachable unless declared; `extra_path` is the documented recipe
  (see config surface).
- The app environment MUST be scrubbed (already implemented, for bare
  and sandboxed spawns alike): an allowlist of inherited variables
  (`PATH`, `HOME`, `LANG`, `TZ`, `LC_*`), then env_file, then inline
  `env`, then the `PORT`/`HOST` contract — never a blanket
  `os.Environ()` inheritance, which would leak ACME tokens (and any
  other supervisor secrets) into every app. The sandbox path MUST NOT
  regress this.
- `HOME`, `XDG_DATA_HOME` and `XDG_CONFIG_HOME` MUST be set to a
  writable in-sandbox path (the app's shared dir, or tmp). Inherited
  they point at `/var/lib/hotserve` — outside the view — and every
  runtime that touches `$HOME` (npm, corepack, pip) would ENOENT.
- The app MUST run inside a new user namespace (`PrivateUsers=yes`,
  set explicitly — systemd 252 does not imply it): this is the
  load-bearing property for the filesystem rows — see
  DESIGN-threat-model.md, "The shared-UID rule": under a shared UID,
  every mount restriction above is void without a namespace the
  kernel's ptrace check honours, and the user namespace is the one it
  does, on every cell of the matrix. On systemd ≥ 256 the app MUST
  additionally run inside a new PID namespace (`PrivatePIDs=yes`, the
  *full* tier): the supervisor, the user manager and every sibling
  become invisible and unsignalable, and systemd's in-namespace stub
  init reaps orphaned grandchildren. Below 256 signals and process
  visibility remain open (DoS class) and MUST be warned about at
  every launch.
- The unit's cgroup is the lifetime boundary (`KillMode=control-group`,
  already in force for every unit): nothing outlives the unit, which
  is what `--die-with-parent` provided under bubblewrap. A unit has no
  controlling terminal, which is what `--new-session` guarded against
  (CVE-2017-5226 class).
- Privilege and system-call surface: `NoNewPrivileges=yes` (already
  in force), `CapabilityBoundingSet=` (empty),
  `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK` (netlink
  read-only for `getifaddrs()`, which runtimes call at startup),
  `ProtectKernelTunables=`, and `SystemCallFilter=@system-service` —
  systemd's curated allowlist, adopted wholesale, which is the bar the
  non-goals below set for any seccomp filter.
- The network namespace MUST be shared: the app binds 127.0.0.1:$PORT
  and hotserve proxies to it, unchanged.
- `pre_start` MUST run under the same sandbox as the app it precedes
  (a migration that writes where the app cannot read is a bug caught
  at deploy time, not 3am).
- Stop semantics MUST remain: SIGTERM to the whole cgroup, `grace`
  (`TimeoutStopSec=`), then SIGKILL — with PID-namespace teardown as
  the backstop (killing the in-namespace init kills everything in the
  namespace; no survivor is possible). The existing escalation tests
  define the contract.
- The status endpoint MUST report the tier the running instance has
  (`"sandbox": "full" | "filesystem" | "none"`) — observability for
  operators and the assertion hook for smoke tests.
- When a deploy fails its health gate and the app is sandboxed, the
  webhook error body SHOULD hint at `extra_path` (the "file/socket
  exists on the host but not in the sandbox" ENOENT is the one support
  question this feature will generate — and the socket variant, a
  same-box database at `/run/postgresql`, is the likelier of the two).

## Config surface (semantic, backend-agnostic)

```caddyfile
app blog {
	command node server.js
	sandbox auto              # auto (default) | require | off
	extra_path /opt/geoip     # ro by default; repeatable
	extra_path /var/cache/blog rw
	extra_path /run/postgresql  # the canonical recipe: same-box DB
	                            # over its unix socket (ro suffices —
	                            # connecting writes to the socket, not
	                            # the directory)
}
```

Raw sandbox plumbing is deliberately NOT exposed — neither bubblewrap
arguments (the backend this document was written for) nor systemd
unit directives (the backend chosen since; see the status note at the
top). The declarations are the stable contract: `extra_path` becomes
`BindReadOnlyPaths=`/`BindPaths=` on the unit, and the same
declarations would remain implementable by Landlock or bubblewrap.
Locking users to one backend's flags would foreclose the others. This
mirrors how Flatpak and systemd treat their sandbox plumbing, and
prevents the "looks configured, quietly weaker" failure mode of
hand-rolled flag soup.

## Architecture

### Spawn path (runner_systemd.go)

> Note (systemd runner): apps are now transient systemd units, so the
> spawn is `ExecStart=` on the unit and bwrap becomes its argv prefix;
> the process-group discussion below is moot — the unit's cgroup is
> what Stop signals, bwrap or not. systemd's own per-unit sandboxing
> (`ProtectSystem=strict`, `InaccessiblePaths=`, `ProtectProc=`) is
> **not** an alternative: under the shared UID a mount sandbox holds
> only inside a PID namespace (`/proc/<any same-UID pid>/root` walks
> the host otherwise — the user manager is always such a pid), and
> systemd delivers one only via `PrivatePIDs=` on ≥ 256; `hidepid`
> hides other users' processes, of which there are none. See
> DESIGN-threat-model.md, "The shared-UID rule". On systemd ≥ 256
> `PrivatePIDs=` provides that namespace and the unit properties
> become the whole mechanism — bwrap is dropped, not layered; below
> 256 `auto` will run floor-only with a WARN. `/sys/fs/cgroup` must be read-only
> in the unit (`ProtectControlGroups=`): the delegated subtree is
> writable by the app's own UID otherwise. Until then apps run with
> **no** sandbox beyond the non-dumpable supervisor: they no longer
> inherit hotserve.service's PrivateTmp / ProtectSystem, and their
> environment is the user manager's defaults plus the allowlisted
> slice.

`startSpec` gains a `sandbox *sandboxSpec` (nil = bare). The exec
runner, when the spec is non-nil, prepends the bwrap argv produced by
a pure function:

    buildBwrapArgs(spec sandboxSpec) []string

That function is the unit-test surface: table-driven tests over dirs,
extra_paths, and masking, with no bubblewrap needed. The process
handle, output piping, reaper goroutine and state persistence are
unchanged — bwrap is simply the direct child.

The argv MUST include an explicit `--chdir` to the release dir rather
than relying on `cmd.Dir` cwd preservation — deterministic beats
inherited. When the probed bwrap supports it (≥ 0.8.0; Debian 12 yes,
Ubuntu 22.04's 0.6.1 no), add `--disable-userns`: the app cannot then
create nested namespaces, closing off the kernel's largest
unprivileged attack surface at zero cost. Feature-detect via the
probe, never by version-string parsing.

### Signal trap (the one thing you must not break)

This is not just a verify-it item — the current plumbing actively
conflicts with bwrap. `setProcessGroup` (runner_unix.go) sets
`Setpgid: true`, which would make bwrap a process-group leader; bwrap's
`--new-session` then calls `setsid()`, which fails with EPERM for a
group leader. Sandboxed spawns MUST drop `Setpgid` and let bwrap's
setsid create the session: the new pgid equals the bwrap PID, children
inside the PID namespace inherit that process group, and process
groups are signalable across PID namespaces from the owning namespace
— so `signalGroup(bwrapPID, SIGTERM)` still reaches the app directly.
Verify with a real integration test, not by reading man pages. The
SIGKILL escalation gains a stronger guarantee than today: PID-ns
teardown makes orphan survival structurally impossible.

### Probe and fallback ladder

At provision, once per config load: locate `bwrap` in PATH and run a
self-test (`bwrap --ro-bind / / true` class) to prove userns actually
works on this kernel/host (LXC-based VPSes and locked-down kernels
fail here, not at first deploy). Cache the verdict; `auto` degrades
with a WARN, `require` fails provision with a message naming the
probe's error. The probe result feeds the status endpoint.

### Packaging

`Depends: bubblewrap` (deb) / `depends` (apk). NEVER vendor the
binary: (a) distro security updates for a security-critical C tool,
(b) Ubuntu 24.04+ restricts unprivileged userns via an AppArmor
exception keyed to the distro's `/usr/bin/bwrap` path — a bundled
copy at another path is not covered and would break exactly there.
Raw-binary (non-package) installs rely on the `auto` probe.

## Rollout and upgrade semantics (normative)

This feature ships as a hotserve binary upgrade to boxes that are
already serving production traffic, so how sandboxing *engages on an
existing fleet* is part of the contract, not an operator's problem to
discover at 3am. The governing asymmetry:

- On a **deploy**, a failed health gate is safe — the new instance is
  discarded and the previous instance keeps serving (zero downtime,
  automatic fallback). A wrong sandbox profile (a missing `extra_path`
  for a DB socket, a DNS break, a scrubbed env var an app leaned on)
  fails *here*, harmlessly.
- On a **restart / boot relaunch from `state.json`** — and equally on
  a **watchdog restart** (crash/health, `watchdog.go`), which is the
  same `launchVersion` path — there is no previous instance to fall
  back to. A sandboxed relaunch that fails leaves the app simply
  **down, with nothing to roll back to**, and a watchdog restart that
  newly engages a sandbox would burn the restart budget on a profile
  mistake. Both are why the sandbox-disposition rule below applies to
  every relaunch, not just boot.

Under the systemd runner a hotserve restart or upgrade no longer
relaunches apps at all — units survive and are reattached — so the
second path is reached by a reboot, a unit stopped behind hotserve's
back, and every watchdog restart. Those are not rare, and a watchdog
restart is exactly when an app is already in trouble. Therefore:

- Enabling sandboxing MUST NOT force an already-running app into a
  sandbox on the upgrade's restart relaunch. Sandbox engagement for an
  existing app MUST ride its **next deploy** (the path that has the
  fallback). Concretely: an app whose recorded running instance is
  bare relaunches **bare** after a supervisor restart even when config
  now says `sandbox auto`; the new isolation takes effect on the next
  cutover, where the health gate protects it. Record the sandbox
  disposition of the running instance in `state.json` so a relaunch
  reproduces what was actually running, not what config now wants.
- `auto`'s graceful degrade covers **host incapability only** (a user
  manager below systemd 256, or a kernel that forbids the namespaces
  → floor-only + WARN). It does NOT cover a per-app
  misconfiguration on a capable host — a missing `extra_path` still
  fails the app. The doc must not imply `auto` is a safety net for
  profile mistakes; the deploy-path fallback is that net.
- A global `sandbox require` is the one setting that can take down the
  **whole server** rather than one app: `require` fails *provision*, so
  on a host that cannot sandbox (systemd < 256, an LXC VPS or
  locked-down kernel without the namespaces) hotserve does not come up
  at all — admin socket and proxy included.
  `require` MUST therefore be reachable only after `auto` has been
  proven per-app on that host, and this hazard MUST be documented at
  the config surface, not just here.

The supported operator rollout, which the product docs MUST describe:
1. Upgrade the binary with sandboxing not yet engaged; absorb the one
   unavoidable supervisor-restart blip (a binary swap is not
   zero-downtime — only deploys are) and confirm apps run healthy
   *bare* on the new binary. One variable at a time.
2. Pre-declare each app's `extra_path` needs and set it to `sandbox
   auto`, applied via **reload** (a no-op for a live app — it does not
   restart), so nothing changes yet.
3. Let the sandbox engage on that app's **next deploy**, where a broken
   profile fails safe and surfaces the `extra_path` hint. Roll
   app-by-app, watching `"sandboxed": true`.
4. Only after every app is proven, optionally tighten to `require`.

Nothing here is destructive: releases, `shared/`, and `state.json` are
bind-mounted at real paths (visibility is restricted, nothing is
deleted or rewritten), hotserve itself is never sandboxed (certs and
admin are untouched), and rollback is reinstalling the prior package
and restarting. The failure mode this section exists to prevent is a
**correlated, no-fallback availability outage at upgrade time**, not
data loss.

## Testing acceptance criteria

- Unit: the unit-property builder's table tests (paths, masking,
  extra_path ro/rw, real-path invariants) against the fake D-Bus
  connection; fallback ladder with a fake manager version
  (auto-degrade warns, require fails provision).
- Integration: real systemd ≥ 256 in the dev-systemd container — the
  property set is read back from the transient unit, SIGTERM reaches
  the app inside its PID namespace, escalation, no orphans after
  SIGKILL; and once during development, the same run with
  `PrivatePIDs=` removed MUST make the `/proc/<manager-pid>/root`
  assertion below fail, proving the PID namespace is the load-bearing
  piece.
- e2e: ALL existing scenarios pass sandboxed (the zero-downtime suite
  doubles as sandbox-compat proof); one new scenario deploys a probe
  app that attempts to read a sibling's release dir, a sibling's env
  file path, the admin socket, the user manager's private socket,
  `/proc/<hotserve-pid>/environ` and `/proc/<user-manager-pid>/root`,
  and asserts every attempt fails and that `/proc` lists only
  in-namespace PIDs. The probe app MUST also assert the
  positive contract: a DNS lookup and an outbound HTTP fetch succeed
  (no current test app makes any outbound call, so the resolv.conf
  trap would otherwise ship silently), `$HOME` is writable, and the
  scrubbed env does NOT contain a seeded supervisor secret (e.g. a
  test ACME token).
- install-test: smoke stage 2 asserts `"sandboxed": true` in the
  status response on the systemd ≥ 256 cells (Debian 13, Ubuntu
  26.04) — this is where Ubuntu's AppArmor user-namespace policy is
  proven for `systemd --user` per-release, per-arch, under the real
  unit — and `"sandboxed": false` with the WARN in the journal on the
  older cells (Debian 12, Ubuntu 24.04).
- soak: full churn with sandbox on; RSS/goroutine/fd deltas quantify
  the (expected ~zero) overhead as a measured claim.
- Upgrade contract: a test proves that an app whose recorded running
  instance is bare relaunches **bare** after a supervisor restart even
  when config now says `sandbox auto`, and only becomes sandboxed on
  its next deploy (the "engage on next deploy, not on the upgrade
  relaunch" rule). This asserts the `state.json` sandbox-disposition
  field is honored on relaunch — the invariant that keeps an upgrade
  from being a fleet-wide no-fallback restart into sandboxes.
- Negative test once during development: break the masking (expose
  /etc/hotserve) and confirm the e2e isolation scenario fails.
- Watch the install-test's journal greps (`permission denied`,
  `read-only file system` in packaging/test/smoke.sh): sandboxed
  runtimes legitimately probe paths that are now masked, and any
  isolation assertion added to the smoke stage intentionally
  *generates* denial lines — scope the greps to the stages they guard
  so they keep catching real regressions without tripping on the
  sandbox working as designed.

## Non-goals (M8)

- Resource limits (memory/CPU) — *no longer a non-goal*: unit
  properties (`MemoryMax=`, `TasksMax=`, `CPUQuota=`) come with the
  mechanism, once `/sys/fs/cgroup` is read-only in the unit (above).
  Defaults and the config surface are decided in #35.
- Network egress control — kernel sandboxes cannot scope by hostname;
  document Deno/Node permission flags as the per-runtime option.
- Per-app UIDs — a later milestone behind a root-owned template or a
  minimal privileged helper; the unit's mount/PID namespaces deliver
  the file/process isolation without them.
- Hand-rolled seccomp filtering — exactly the "looks configured,
  quietly weaker" trap this document warns about. systemd's curated
  `@system-service` set is a vetted filter adopted wholesale, which is
  why it appears in the specification above.
- Protecting an app from itself — its own env and its own database
  are legitimately reachable by definition.
- macOS/Windows sandboxing — `auto` is a documented no-op off-Linux
  (dev machines); servers are Linux.

## Open questions (with leans)

- Exact ro system-path set: bind the conventional list (`/usr`,
  `/bin`, `/sbin`, `/lib*`, `/etc`) vs `--ro-bind / /` + masks. Lean:
  explicit allowlist — masking-the-world inverts the failure mode to
  "quietly exposed".
- Global `sandbox require` default in the `liveswap` block (fleet
  policy vs per-app)? Lean: yes, per-app overrides a global default;
  cheap to add at config level.
- Operator env_files outside `/etc/hotserve`: masked dir covers the
  documented location only — and the docs currently disagree with
  themselves (liveswap.go's example says `/etc/liveswap/blog.env`,
  packaging/Caddyfile says `/etc/hotserve/myapp.env`). Lean: unify the
  documentation on `/etc/hotserve`, mask both dirs, and WARN at
  provision when an app's `env_file` path would be readable inside its
  own sandbox view; do not chase arbitrary paths in M8.
- Landlock as a same-config fallback backend where userns is
  unavailable: keep the config surface compatible, defer the backend.
