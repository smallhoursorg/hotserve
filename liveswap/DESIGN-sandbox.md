# DESIGN — per-app sandboxing (M8)

Status: shipped (2026-08-30, #35 phase 1) as systemd's own per-unit
sandboxing on the user-manager runner. **Narrowed 2026-09-01** to a
single probe-gated tier, *full* (user namespace + PID namespace + the
mount/device/cgroup/seccomp/caps set), when the support matrix became
Debian 13 alone — see "Amendment: one host, one tier" below, which
supersedes every two-tier statement in this document. Resource caps
are phase 2. This document was the implementation brief
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

- Sandboxing MUST default to on (`sandbox auto`), probed at start by
  running a throwaway unit and checking the namespaces from inside
  (the manager silently ignores `PrivatePIDs=` on a kernel without PID
  namespaces, so accepting the property proves nothing). It MUST be
  configurable globally and per app: `auto` (sandbox where the host
  delivers it, with a prominent WARN at start and at every spawn that
  runs below *full*), `require` (start fails unless the *full* tier is
  available — a weaker tier accepted silently is the "looks
  configured, quietly weaker" trap), `off`.
- The sandboxed filesystem view MUST be **deny-by-default**: an empty
  read-only tmpfs replaces the whole filesystem
  (`TemporaryFileSystem=/:ro`) and the only things that exist inside a
  unit are the ones explicitly bound. The guarantee is one sentence —
  *anything not named is absent* — and it holds by construction rather
  than by a list of things to hide being complete and current.
  *Amended 2026-08-31 (#35):* this replaces the earlier model, which
  left the host readable (`ProtectSystem=strict`) and masked a set
  derived from the running configuration (`InaccessiblePaths=` over
  hotserve's own paths plus every app's `env_file`). That set could be
  incomplete, and — because a unit's mount namespace is built once at
  start and never rebuilt — it aged: an `env_file` belonging to an app
  added later was merely read-only inside older siblings' views. There
  is no set now, so there is nothing to keep current.
  `ProtectSystem=` and `ProtectHome=` are NOT set: nothing is left for
  either to act on, and "absent" is the stronger statement.
- What a unit's view MUST contain, at REAL host paths (no remapping —
  `current` symlinks, state paths and operator debugging all assume
  real paths):
  - the app's release dir being started (rw) and its shared dir (rw),
    and nothing else of the liveswap root. Its `state.json`, its
    `tmp/` (the upload staging dir — a running instance must never be
    able to rewrite the next version's tarball), its `current` symlink
    and its other releases are simply never named, so they do not
    exist inside the unit;
  - a private `/tmp` and `/var/tmp` (`PrivateTmp=`), a minimal `/dev`
    (`PrivateDevices=`) and the API VFS. `MountAPIVFS=` is NOT set:
    `/proc`, `/sys` and `/dev` are mounted inside the tmpfs by
    `PrivateDevices=` (and, at the *full* tier, `PrivatePIDs=`) —
    measured on 252, 255, 257 and 259;
  - a **base view** of the OS, bound read-only, named entry by entry
    in `sandboxBaseView` (liveswap/sandbox.go): `/usr` and the usrmerge
    aliases (`/bin`, `/sbin`, `/lib*`), the TLS trust store
    (`/etc/ssl/certs`, `/etc/ssl/openssl.cnf`, `/etc/ca-certificates`,
    `/etc/pki/tls/certs` — the certificate directories themselves, never
    the `/etc/ssl` tree that also holds `/etc/ssl/private`), name and user
    resolution (`/etc/resolv.conf`, `/etc/hosts`, `/etc/hostname`,
    `/etc/nsswitch.conf`, `/etc/passwd`, `/etc/group`), `/etc/localtime`,
    `/etc/alternatives`, the linker's cache and configuration
    (`/etc/ld.so.cache`, `/etc/ld.so.conf`, `/etc/ld.so.conf.d`), and
    `/run/systemd/resolve`. Every entry is bound with `IgnoreENOENT`:
    the list spans four distros and none has all of it (Ubuntu 24.04
    and 26.04 ship no `/etc/localtime`, Debian no `/etc/pki`, a merged
    host no real `/lib64`);
  - each configured `extra_path` (ro by default, rw when declared).
- `/etc` MUST NOT be bound whole. Binding it would hand every app
  every other app's `env_file` and hotserve's own configuration —
  precisely the derived, ageing hidden set this model exists to
  delete. Naming the dozen entries a runtime actually needs is what
  keeps the guarantee free of exceptions.
- The view therefore MUST NOT contain, without anything having to
  enumerate them: other apps' directories, `/var/lib` outside the
  liveswap root's path to this app's own dirs (hotserve's TLS keys
  live there), `/run/hotserve` (admin socket), `/run/user/<uid>` (the
  user manager's private socket and session bus), `/etc/hotserve` and
  `/etc/liveswap` (env files), `/home`, `/root`, `/opt`, `/srv`, or
  any operator `env_file` wherever it lives. `/sys/fs/cgroup` MUST be
  read-only (`ProtectControlGroups=`): the delegated subtree is owned
  by the app's own UID, so resource caps are otherwise rewritable.
- `BindPaths=` and `BindReadOnlyPaths=` are two views of ONE list in
  the manager, and setting either to an empty array RESETS that list
  rather than adding nothing. An empty `BindPaths=` emitted after the
  base view therefore takes `/usr` and `/bin` away again and the unit
  fails `203/EXEC` — measured, and exactly what the capability probe
  (which has no directories of its own) would otherwise do. Empty list
  properties MUST NOT be emitted.
- An app's mandatory binds — the release being started and `shared/` —
  MUST resolve to the directories they name, and a launch whose bind
  source resolves anywhere else MUST be refused. The only permitted
  difference is one an alias on the liveswap root itself explains.
  *Amended 2026-08-31 (#35):* resolution to a path outside the root
  was previously allowed as "the operator moved `shared` to another
  disk", subject to deny lists. That is unsound: the app's own
  directory is writable by the app, so a symlink placed there cannot
  be distinguished from one the operator meant, and a sibling's
  legitimate external data (`shop/shared -> /mnt/shop-data`) was
  therefore an acceptable target for a bare `blog` to aim at.
  Enumerating other apps' data and `env_file` locations cannot close
  it — incomplete by construction, and stale by the same mechanism as
  the hidden set this design deleted. **The supported way to put an
  app's data on another disk is a bind mount at the same path**: a
  mount resolves to itself, so it never reaches the check, and an app
  cannot forge one.
- One app's `env_file` MUST NOT sit inside another app's view. The
  deny-by-default view does not close this on its own, because both
  routes are things the operator named: another app's `extra_path`
  covering the directory, and the OS base view, which is bound into
  every unit. Config load MUST refuse both, comparing canonical
  spellings as well as configured ones. An `env_file` inside its OWN
  app's dirs is a warning, not an error — the app receives those
  variables regardless; the reason to warn is that under `shared/` it
  can rewrite the file and choose its own next launch's environment.
  Pinned by `TestEnvFileMayNotLandInAnotherAppsView`.
- A recorded sandbox tier that is neither empty nor a tier name MUST
  fail the relaunch rather than read as `none`. Empty is the legacy
  pre-sandbox record and correctly relaunches bare; anything else is
  corruption, and a syntactically corrupt `state.json` is already a
  permanent recovery error. Pinned by
  `TestCorruptRecordedTierFailsClosed`.
- A command that resolves outside the view MUST be refused at launch
  with a message naming `extra_path`, not left to fail as a bare
  `203/EXEC`: `exec.LookPath` runs in the supervisor's view, so a
  runtime installed outside `/usr` (`/opt/node/bin/node`, an nvm or
  asdf shim under a home directory) resolves for the supervisor and
  then does not exist inside the unit.
- `/run/systemd/resolve` MUST stay reachable (read-only) when it
  exists: on systemd-resolved hosts (every Ubuntu in the support
  matrix) `/etc/resolv.conf` is a symlink into that directory, and
  since nothing else under `/run` is in the view the link would
  otherwise dangle — all DNS inside the sandbox failing, silently, on
  the most common distro. Local
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
- Nothing in the app's environment may name a path outside its view.
  `HOME` MUST DEFAULT to a writable in-sandbox path (the app's shared
  dir): inherited it points at `/var/lib/hotserve`, and every runtime
  that touches `$HOME` (npm, corepack, pip) would ENOENT. It is applied
  before `env_file` and inline `env`, so an operator can still point it
  elsewhere — deliberately, since an app may want its cache on another
  bound path. An override that lands OUTSIDE the view is not refused
  (the operator asked for it, and a config-load check cannot see a view
  that is built per launch) but MUST be reported at launch, or the app
  fails later with an ENOENT that names no cause.
  *Amended 2026-09-01: this bullet said "MUST be set", which the
  implementation never did — the override is intentional.*
  `XDG_DATA_HOME` and `XDG_CONFIG_HOME` MUST NOT be inherited —
  *amended 2026-08-31, this bullet previously said they must be set*.
  Leaving them unset is what satisfies the rule: the XDG base-directory
  spec then makes every runtime derive them from `$HOME`, which is
  already correct, so there are two fewer values to keep right. They
  are not in `envAllowlist`, which is what enforces it.
  Pinned by `TestSandboxedEnvNamesNoPathOutsideTheView`.
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
- The view a unit gets is fixed at its start: an `extra_path` added by
  a later reload does not appear inside instances already running, and
  the operator MUST be told to redeploy them. This is the same "engage
  on the next deploy" rule the tier follows. *Amended 2026-08-31:* it
  is no longer a **security** asymmetry. Under the previous
  hidden-set model a secret declared after a unit started stayed
  readable inside it; under the deny-by-default view that secret is
  absent from every view, old units included, because nothing ever
  bound it. What ages now is only what an operator asked to be let
  IN, which fails safe.
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
   app-by-app, watching `"sandbox"` in each app's status (the shipped
   field is a tier string — `"full"`, `"filesystem"` or `"none"` — not
   a boolean; a boolean cannot express the two tiers).
4. Only after every app is proven, optionally tighten to `require`.

Nothing here is destructive: the release being started and `shared/`
are bind-mounted at their real paths and everything else is simply
absent from the unit's view — `state.json` included, which the
supervisor still reads and writes as itself; nothing on disk is
deleted or rewritten. hotserve itself is never sandboxed (certs and
admin are untouched), and rollback is reinstalling the prior package
and restarting. The failure mode this section exists to prevent is a
**correlated, no-fallback availability outage at upgrade time**, not
data loss.

## Testing acceptance criteria

**Every promise above is asserted somewhere, and a promise that is not
asserted is not a promise.** Three review rounds on this feature found
the same class of defect — one app reaching another's data — by
inspection, one instance at a time, because the model's guarantees
lived in prose while the tests checked the diff. `liveswap/sandbox_promises_test.go`
carries the ones whose only other statement was prose, and its header
maps every other normative bullet to the test that pins it. Adding a
MUST here means adding its assertion in the same change.

- Unit: the unit-property builder's table tests (the base view,
  extra_path ro/rw, real-path invariants, and above all that the set
  of bind destinations IS the view — nothing named that should not be)
  against the fake D-Bus connection; fallback ladder with a fake
  manager version
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
- install-test: smoke stage 2 asserts `"sandbox": "full"` in the
  status response on the systemd ≥ 256 cells (Debian 13, Ubuntu
  26.04) — this is where Ubuntu's AppArmor user-namespace policy is
  proven for `systemd --user` per-release, per-arch, under the real
  unit — and `"sandbox": "filesystem"` with the WARN in the journal on
  the older cells (Debian 12, Ubuntu 24.04). Both cells additionally
  assert the view from inside the unit: hotserve's state, sockets and
  a sibling's files absent, `/etc` only the base-view entries, and a
  runnable OS present so "absent" cannot mean "empty unit".
- soak: full churn with sandbox on; RSS/goroutine/fd deltas quantify
  the (expected ~zero) overhead as a measured claim.
- Upgrade contract: a test proves that an app whose recorded running
  instance is bare relaunches **bare** after a supervisor restart even
  when config now says `sandbox auto`, and only becomes sandboxed on
  its next deploy (the "engage on next deploy, not on the upgrade
  relaunch" rule). This asserts the `state.json` sandbox-disposition
  field is honored on relaunch — the invariant that keeps an upgrade
  from being a fleet-wide no-fallback restart into sandboxes.
- Negative test once during development: widen the view (add a bind
  for /etc/hotserve) and confirm the e2e isolation scenario fails.
  Under a deny-by-default view the mutation to make is *adding* a
  name, not removing a mask.
- Mind the failure *signature*: under a deny-by-default view a
  runtime that probes a path nobody named gets **ENOENT** ("no such
  file or directory"), not EACCES or EROFS. That is the opposite of
  the masking model, where the path existed and the denial was a
  permission error, and it is why a missing runtime surfaces as
  `203/EXEC` rather than a permission complaint. Any journal grep in
  packaging/test/smoke.sh that scans for denial strings must be scoped
  to the stage it guards: sandboxed runtimes legitimately probe absent
  paths at startup, and the isolation assertions intentionally
  generate such lines.

## Non-goals (M8)

- Resource limits (memory/CPU) — *no longer a non-goal*: unit
  properties (`MemoryMax=`, `TasksMax=`, `CPUQuota=`) come with the
  mechanism, once `/sys/fs/cgroup` is read-only in the unit (above).
  Defaults and the config surface are decided in #35.
- Network egress control — kernel sandboxes cannot scope by hostname;
  document Deno/Node permission flags as the per-runtime option.
  *Discharged 2026-09-01:* liveswap/README.md, "Runtime permissions",
  with the layering stated (the sandbox is the kernel-enforced ceiling
  decided by the Caddyfile; the runtime's `--allow-*` flags are the
  app narrowing itself inside it, enforced in-process and therefore
  not a substitute). Measured on Deno 2.8.3: `--allow-net=<addr:port>`
  is sufficient for `deno serve` to bind and refuses every other
  address, which also closes the sibling-localhost residual named in
  DESIGN-threat-model.md for apps that adopt it. liveswap stays
  runtime-agnostic — no `deno` app type, no flag synthesis, no
  parsing of `command`.
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

- ~~Exact ro system-path set: bind the conventional list (`/usr`,
  `/bin`, `/sbin`, `/lib*`, `/etc`) vs `--ro-bind / /` + masks. Lean:
  explicit allowlist — masking-the-world inverts the failure mode to
  "quietly exposed".~~ **DECIDED 2026-08-31 (#35): explicit allowlist**,
  as the lean said, and for the reason it gave — the masking model did
  fail exactly that way in review. `/etc` is named entry by entry
  rather than bound whole, because binding it would reintroduce the
  per-config hidden set for everything under it. See `sandboxBaseView`
  and the behaviour spec above.
- Global `sandbox require` default in the `liveswap` block (fleet
  policy vs per-app)? Lean: yes, per-app overrides a global default;
  cheap to add at config level.
- ~~Operator env_files outside `/etc/hotserve`: masked dir covers the
  documented location only…~~ **DISSOLVED 2026-08-31 (#35).** With a
  deny-by-default view an `env_file` is absent from every unit
  wherever it lives — there is no location to chase and no directory
  to mask. The one place one can still land inside a view is the app's
  own directories, which are the only host paths bound writable; that
  is warned about at config load (`warnEnvFileInView`), because under
  `shared/` an app can rewrite the file and so choose the environment
  of its own next launch. An app's own environment being reachable is
  a stated non-goal to defend against.
- Landlock as a same-config fallback backend where userns is
  unavailable: keep the config surface compatible, defer the backend.

## Amendment: `extra_path` deferred (2026-09-02)

`extra_path` is **not in the shipped feature**. Every reference to it
below describes a design that was written, reviewed and then lifted out
before merge; the shipped view is exactly `sandboxBaseView` plus the
app's own release and `shared/` dirs, and nothing widens it.

**Why it came out.** Six review rounds over the sandbox produced one
class of real defect, and every instance of it was `extra_path`:

- an optional bind (`IgnoreENOENT`) was believed to defer a missing
  source; it drops it, so the app served permanently blind to a path it
  declared;
- a writable `extra_path` aliasing over a second one let a compromised
  app repoint the second at any directory on the box between launches —
  a sibling's `env_file` included — and every containment check still
  passed, because the planted target was a real path that simply was
  not the one the operator named;
- the `{env.*}` expansion every other path option gets was missing;
- and the cross-app `env_file` checks exist *because* an `extra_path`
  can cover another app's secrets, which is where the remaining
  findings clustered.

That is not a coincidence. `extra_path` is the only mechanism that
*adds* to a deny-by-default view, it is operator-controlled, and it has
to hold simultaneously against symlinks, TOCTOU, cross-app containment
and the base view. It is the one part of this design that inverts the
model, and it deserves to be designed and reviewed on its own rather
than as a clause of a feature this size.

**What replaces it, for now:** `sandbox off` for the app that needs
more — per app, so the rest of the fleet keeps its isolation, and no
worse than that app had before per-app sandboxing existed. Persistent
data goes in `shared/`; runtimes go under `/usr`, which the base view
binds.

**What to keep when it returns.** The rules below were paid for and
should not be rediscovered: a bind must BE the directory it names
(equality, not resolve-then-check — a mount resolves to itself and an
app cannot forge one); overlap is symmetric; the base view may not be
named at all, read-only or writable; a missing source must fail the
unit rather than be skipped; and the cross-app `env_file` check must
compare canonical *and* configured spellings, resolving the deepest
existing ancestor for a path whose leaf does not exist yet.

## Amendment: one host, one tier (2026-09-01)

The support matrix narrowed to **Debian 13** (systemd 257). Where this
document says "two tiers", "the best tier", "*filesystem*" or names
Debian 12 / Ubuntu, read the following instead. Nothing in the
property set, the deny-by-default view, the config surface or the
rollout semantics changes.

**One tier is probed for.** `probeSandboxCapability` has a single
candidate, *full*. A host either delivers both namespaces or reports
`none`: `auto` launches bare with a WARN naming the residual,
`require` refuses to start. There is deliberately no fallback rung —
on a matrix of one, a second tier would be a path no supported host
takes and no lane tests.

**`filesystem` survives as a record, not a capability.** An instance
started before this change may have `"sandbox":"filesystem"` in
`state.json`. `validSandboxTierRecord` still accepts it and
`sandboxProperties` still renders it, so such an instance relaunches
at the tier it actually has. Reading it as `none` would silently drop
a running app's isolation; reading it as `full` would silently claim
one it never got. Both are the failure this document exists to
prevent, so the value stays until every instance has redeployed.

**The AppArmor profile is gone.** Debian's kernel does not restrict
unprivileged user namespaces, so the profile granting `userns` to
hotserve's user manager, and the wrapper it attached to by path, are
removed. The residual they carried — the manager's children inheriting
that permission, so an app under `sandbox off` could create user
namespaces the distro default would refuse — goes with them.

**The probe stays, and this is the load-bearing decision.** It was
never a proxy for the systemd version, which is why removing version
detection did not remove it: a container, an LXC VPS, or a kernel
built without user namespaces presents a supported manager and still
fails the unit. The probe is what keeps `"sandbox":"full"` a
measurement rather than an inference from `/etc/os-release`.

**Accepted costs**, both stated so they are not rediscovered as bugs:

- A Debian 12 host that upgrades hotserve drops from *filesystem* to
  `none` — not to a lesser sandbox, to no sandbox. It is loud (WARN at
  every launch, `"sandbox":"none"` in status, `require` refusing to
  start) but it is a real reduction, and it is what dropping support
  means.
- CI no longer exercises a userns-restricted kernel. GitHub's runners
  boot an Ubuntu kernel, and the profile was what let the Debian cells
  prove the sandbox under a real restriction; CI now sets
  `kernel.apparmor_restrict_unprivileged_userns=0` to make the runner
  behave like a Debian host. Readmitting Ubuntu would mean restoring
  the profile, the wrapper, and that coverage together — the
  measurements justifying all three are kept above rather than
  deleted.
