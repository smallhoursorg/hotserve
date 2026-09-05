# DESIGN — per-app sandboxing

This document describes the present: what is built, why, and what it
promises. History lives in git (`git log -- liveswap/DESIGN-sandbox.md`);
the "History" section at the end holds only dated one-liners.
Amendments are for decisions still fresh or contested and get folded
into the body once they settle.

Every app unit runs under systemd's own per-unit sandboxing on the
hotserve user's manager: a user namespace, a PID namespace and a
deny-by-default filesystem view. There is one sandbox, every unit gets
it, and no configuration turns it off or widens it. The supported host
is Debian 13 (systemd 257). The implementation is `liveswap/sandbox.go`
(the spec, the base view, the probe) and `sandboxProperties` in
`liveswap/systemd_dbus.go` (the rendered property set); the reasoning
that chose the mechanism is [DESIGN-threat-model.md](../DESIGN-threat-model.md),
"The shared-UID rule".

## Why this feature exists

hotserve's trust model is one box, one trust domain: every app runs as
the `hotserve` user. Without a sandbox any app can read every other
app's files, env files, the TLS private keys and
`/proc/<sibling>/environ`, and can connect to the admin unix socket
(same UID). The realistic attacker is not a malicious tenant — it is a
poisoned transitive dependency (npm's 2025 wave: runtime payloads doing
broad filesystem sweeps for `.env` files, tokens and wallets, then
exfiltrating).

Containers are the industry answer and the product explicitly rejects
them. The sandbox is the container *primitives* without the container
*product*: apps stay files in directories, supervised by hotserve — but
each app's filesystem view contains only its own world, and siblings
are not merely unreadable, they do not exist.

What this stops: lateral movement from one compromised app (sibling
files/env/releases, TLS keys, admin socket, sibling `/proc`, signals).
What this does NOT stop, stated honestly: theft of the compromised
app's own secrets (its env is its env), network exfiltration (netns is
shared by design), resource exhaustion (caps are unset until an app
needs them — #52), and install-time supply-chain attacks (those run in
CI, not on the box).

The mount namespace alone does not close the threat model: the
attacker just described dumps `process.env` first, and a stolen ACME
token lets them issue or alter certificates. `buildEnv` therefore
inherits only an allowlist, pinned by
`TestBuildEnvDoesNotLeakSupervisorSecrets`; the requirement is restated
below as a contract the sandbox path must keep.

## Behaviour specification (normative)

- Sandboxing MUST be unconditional: every unit — deploy, `pre_start`,
  crash relaunch, boot recovery, reattach — gets the one sandbox, and
  there is no configuration surface that turns it off or widens it.
  The host MUST be probed by running a throwaway unit and checking the
  namespaces from inside (the manager silently ignores `PrivatePIDs=`
  on a kernel without PID namespaces, so accepting the property proves
  nothing), and a host that cannot deliver the sandbox MUST fail the
  start with the probe's reason — a weaker sandbox accepted silently is
  the "looks configured, quietly weaker" trap, and no sandbox at all,
  on a shared uid, hands one app every sibling's data and hotserve's
  own keys. The runner MUST refuse a start spec with no sandbox
  (`TestUnitRefusesToRunWithoutASandbox`).
- The sandboxed filesystem view MUST be **deny-by-default**: an empty
  read-only tmpfs replaces the whole filesystem
  (`TemporaryFileSystem=/:ro`) and the only things that exist inside a
  unit are the ones explicitly bound. The guarantee is one sentence —
  *anything not named is absent* — and it holds by construction rather
  than by a list of things to hide being complete and current.
  `ProtectSystem=`, `ProtectHome=` and `InaccessiblePaths=` are NOT
  set: nothing is left for any of them to act on, and "absent" is the
  stronger statement.
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
    `PrivateDevices=` and `PrivatePIDs=`;
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
    no host has all of it (Debian 13 is merged-/usr, so `/bin` and
    `/lib64` are symlinks and there is no `/etc/pki`; a host without
    systemd-resolved has no `/run/systemd/resolve`). The names stay
    even where this distro does not use them, so a `#!` line or an
    interpreter's own ld path resolves to the spelling it was written
    with.
- `/etc` MUST NOT be bound whole. Binding it would hand every app
  every other app's `env_file` and hotserve's own configuration —
  precisely the derived, ageing hidden set this model exists to
  delete. Naming the dozen entries a runtime actually needs is what
  keeps the guarantee free of exceptions.
- The view therefore MUST NOT contain, without anything having to
  enumerate them: other apps' directories, `/var/lib` outside the
  liveswap root's path to this app's own dirs (hotserve's TLS keys
  live there), `/run/hotserve` (admin socket), `/etc/hotserve` and
  `/etc/liveswap` (env files), `/home`, `/root`, `/opt`, `/srv`, or
  any operator `env_file` wherever it lives. `/run/user` MUST NOT be
  reachable by any route, read-only included: with `PrivateUsers=`
  mapping the app's uid one-to-one, the user manager's private socket
  lets an app ask the manager for a unit with no sandbox at all, and
  connecting to a socket is not a write (`sandboxNeverReachable`).
  `/sys/fs/cgroup` MUST be read-only (`ProtectControlGroups=`): the
  delegated subtree is owned by the app's own UID, so resource caps
  are otherwise rewritable.
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
  Resolution to a path outside the root is unsound: the app's own
  directory is writable by the app, so a symlink placed there cannot
  be distinguished from one the operator meant, and a sibling's
  legitimate external data (`shop/shared -> /mnt/shop-data`) would be
  an acceptable target for `blog` to aim at. **The supported way to
  put an app's data on another disk is a bind mount at the same
  path**: a mount resolves to itself, so it never reaches the check,
  and an app cannot forge one.
- One app's `env_file` MUST NOT sit inside another app's view. The
  deny-by-default view does not close this on its own, because the OS
  base view is bound into every unit. Config load MUST refuse it,
  comparing canonical spellings as well as configured ones. An
  `env_file` inside its OWN app's dirs is a warning, not an error —
  the app receives those variables regardless; the reason to warn is
  that under `shared/` it can rewrite the file and choose its own next
  launch's environment. Pinned by `TestEnvFileMayNotLandInAnotherAppsView`.
- Nothing in `state.json` decides how much isolation a relaunch gets:
  there is no recorded sandbox disposition, and a record written
  before sandboxing existed relaunches sandboxed like everything else.
  Pinned by `TestEveryLaunchIsSandboxed`.
- A reattach MUST verify rather than trust: a recorded unit that is
  running without `PrivateUsers=`/`PrivatePIDs=` was not started by
  this hotserve (a development build, or another same-uid process) and
  is refused, logged at ERROR, and replaced by a sandboxed relaunch
  through the ordinary sweep (`TestSystemdRunnerReattach`). That
  catches an honest stale unit, not a same-uid process starting units
  on purpose — that process can give a unit any property set and is
  inside the trust domain already.
- A command that resolves outside the view MUST be refused at launch
  with a message that says where a runtime has to live (ship it in
  the release, or install it under `/usr`), not left to fail as a bare
  `203/EXEC`: `exec.LookPath` runs in the supervisor's view, so a
  runtime installed outside `/usr` (`/opt/node/bin/node`, an nvm or
  asdf shim under a home directory) resolves for the supervisor and
  then does not exist inside the unit
  (`TestUnitForRefusesCommandOutsideTheView`).
- `/run/systemd/resolve` MUST stay reachable (read-only) when it
  exists: on systemd-resolved hosts `/etc/resolv.conf` is a symlink
  into that directory, and since nothing else under `/run` is in the
  view the link would otherwise dangle — all DNS inside the sandbox
  failing, silently. Local unix sockets (`/run/postgresql`,
  `/run/mysqld`, …) are deliberately not reachable: a same-box
  database is reached over TCP loopback.
- The app environment MUST be scrubbed: an allowlist of inherited
  variables (`PATH`, `LANG`, `TZ`, `LC_*`; `HOME` is set to the app's
  shared dir, never inherited), then `env_file`, then inline `env`,
  then the `PORT`/`HOST` contract — never a blanket `os.Environ()`
  inheritance, which would leak ACME tokens (and any other supervisor
  secrets) into every app. The sandbox path MUST NOT regress this.
  `XDG_RUNTIME_DIR` and `DBUS_SESSION_BUS_ADDRESS`, which the manager
  hands every unit, are unset (`UnsetEnvironment=`): both point into a
  `/run/user` that is not in the view.
- Nothing in the app's environment may name a path outside its view.
  `HOME` MUST DEFAULT to a writable in-sandbox path (the app's shared
  dir): inherited it points at `/var/lib/hotserve`, and every runtime
  that touches `$HOME` (npm, corepack, pip) would ENOENT. It is applied
  before `env_file` and inline `env`, so an operator can still point it
  elsewhere — deliberately, since an app may want its cache on another
  bound path. An override that lands OUTSIDE the view is not refused
  (the operator asked for it, and a config-load check cannot see a view
  that is built per launch) but MUST be reported at launch
  (`homeOutsideView`), or the app fails later with an ENOENT that
  names no cause. `XDG_DATA_HOME` and `XDG_CONFIG_HOME` MUST NOT be
  inherited: leaving them unset makes every runtime derive them from
  `$HOME`, which is already correct. They are not in `envAllowlist`,
  which is what enforces it. Pinned by
  `TestSandboxedEnvNamesNoPathOutsideTheView`.
- The app MUST run inside a new user namespace (`PrivateUsers=yes`,
  set explicitly — the mount options do not imply it): this is the
  load-bearing property for the filesystem rows — see
  DESIGN-threat-model.md, "The shared-UID rule": under a shared UID,
  every mount restriction above is void without a namespace the
  kernel's ptrace check honours, and the user namespace is the one it
  does. The app MUST additionally run inside a new PID namespace
  (`PrivatePIDs=yes`): the supervisor, the user manager and every
  sibling become invisible and unsignalable, and systemd's
  in-namespace stub init reaps orphaned grandchildren. A manager that
  cannot deliver it is refused at start.
- The unit's cgroup is the lifetime boundary (`KillMode=control-group`):
  nothing outlives the unit. A unit has no controlling terminal.
- Privilege and system-call surface, every unit: `NoNewPrivileges=`,
  `CapabilityBoundingSet=` (empty), `RestrictNamespaces=` (empty set —
  an app cannot create further namespaces), `RestrictRealtime=`,
  `RestrictSUIDSGID=`, `LockPersonality=`, `ProtectKernelTunables=`,
  `ProtectKernelModules=`, `ProtectKernelLogs=`,
  `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK`
  (netlink read-only for `getifaddrs()`, which runtimes call at
  startup), and `SystemCallFilter=@system-service` with
  `SystemCallErrorNumber=EPERM` — systemd's curated allowlist, adopted
  wholesale, which is the bar the non-goals below set for any seccomp
  filter. Pinned by `TestSandboxPropertiesEveryUnit`.
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
- The view a unit gets is fixed at its start. Nothing changes the view
  today, and this is not a security asymmetry: a secret declared after
  a unit started is absent from every view, old units included,
  because nothing ever bound it.
- The status endpoint reports no sandbox field: there is one sandbox
  and every unit has it, so there is nothing to report. The lanes
  assert the sandbox from inside the unit, which is the only honest
  assertion hook.

## Config surface

There is none. The view is exactly `sandboxBaseView` plus the app's
own release and `shared/` dirs, and nothing in the Caddyfile widens or
disables it; a config naming `sandbox` fails to load
(`TestRetiredSandboxConfigIsRejected`). Raw unit directives are
deliberately not exposed — hand-rolled flag soup is the "looks
configured, quietly weaker" failure mode. See "`extra_path` — rules
for when it returns" for the one widening mechanism that has been
designed and deferred.

## Mechanism

`unitFor` (liveswap/runner_systemd.go) builds the transient unit:
`resolveBindSources` resolves and re-checks every bind source at the
last moment before the manager acts (closing the planted-symlink
case), `homeOutsideView` reports an operator `HOME` the view cannot
satisfy, and `sandboxProperties` (liveswap/systemd_dbus.go) renders
the property set above. `unitFor` refuses a spec without a sandbox.

**The probe.** Before the first unit on a manager connection,
`probeSandboxCapability` starts a throwaway unit with the same property
set whose script reads `/proc/self/uid_map` and its own pid, and passes
only if the uid range is not the host's and the pid is 1 — both
namespaces engaged. One attempt, one retry on timeout only (the shape a
capable host under boot load produces; hotserve.service carries no
`Restart=`, so without it one slow attempt keeps a capable box down
until someone restarts it by hand), 30 s each. The probe also proves
the base view: `/bin/sh` has to exist inside the unit for the script
to run at all. Its output lands in `journalctl -t hotserve-sandbox-probe`,
which the refusal message names. The verdict is cached per manager
connection (`userManagerClient.sandboxCapability`); a failed verdict
is never cached.

The probe stays a measurement rather than a version check because the
host still gets a vote: a container, an LXC VPS, a kernel built
without user namespaces, or an LSM that refuses them all fail the unit
on a manager whose version says it should have worked. CI sets
`kernel.apparmor_restrict_unprivileged_userns=0` on GitHub's Ubuntu
kernel so the runner behaves like a Debian host; no lane exercises a
restricted kernel, and the probe is what stands between such a host
and a silent loss of isolation.

There is no cache invalidation, and this is deliberate: app units are
`Type=simple`, whose start job completes once the manager has forked —
before namespace setup and before exec — so a namespace failure
(`226/NAMESPACE`, when a kernel or LSM change withdraws the namespaces
under a live connection) arrives asynchronously through the unit
watcher, where no synchronous hook can see it.
`TestIntegrationSystemdSandboxedUnitFailsAfterItsStartJobSucceeds` pins
the systemd semantics so the invalidation is not reinvented.

**Packaging.** `postinstall.sh` enables linger for the hotserve user so
its manager runs without a session. Nothing else: there is no runtime
dependency and no profile to install.

## Availability dependency

The sandbox is an availability dependency of the whole server: on a
host that cannot sandbox, hotserve does not come up at all — admin
socket and proxy included. There is no graceful degrade for host
incapability (a container, an LXC VPS, a locked-down kernel), because
the setting that would run apps without one hands a bare app every
sibling's data and hotserve's keys on the shared uid. The refusal
message names where the probe's output is and says that no setting
runs without one. The accepted cost: unprivileged LXC containers from
budget providers cannot run hotserve, and a workload the sandbox
cannot host has nowhere to run.

The governing asymmetry an operator should know:

- On a **deploy**, a failed health gate is safe — the new instance is
  discarded and the previous one keeps serving. A view mistake (a DNS
  break, a scrubbed env var an app leaned on) fails here, harmlessly.
- On a **relaunch** — boot recovery, a unit stopped behind hotserve's
  back, every watchdog restart — there is no previous instance to fall
  back to. The view is the same fixed one every launch of that app
  gets, so a mistake surfaces on the next deploy or relaunch alike;
  what never happens is the view changing under a running app.

Before restarting into a new box or binary, establish that the host
can deliver the sandbox. A reload is the safe place to find out — a
config that cannot activate leaves the running one serving, where a
restart does not. `hotserve validate` does not measure the host (it
never starts a unit). The verdict is cached per manager connection, so
a reload measures the host only the first time after hotserve dialled
the manager; a namespace policy changed underneath a running hotserve
(a sysctl, an LSM policy load) is not seen until the next restart.
There is deliberately no uncached preflight: the cache is what keeps a
throwaway unit off Caddy's reload path.

Nothing here is destructive: the release and `shared/` are
bind-mounted at their real paths and everything else is simply absent
from the unit's view — `state.json` included, which the supervisor
still reads and writes as itself. hotserve itself is never sandboxed.

## Testing acceptance criteria

**Every promise above is asserted somewhere, and a promise that is not
asserted is not a promise.** Three review rounds on this feature found
the same class of defect — one app reaching another's data — by
inspection, one instance at a time, because the model's guarantees
lived in prose while the tests checked the diff.
`liveswap/sandbox_promises_test.go` carries the ones whose only other
statement was prose, and its header maps every other normative bullet
to the test that pins it. Adding a MUST here means adding its
assertion in the same change.

- Unit: the unit-property builder's table tests (the base view,
  real-path invariants, and above all that the set of bind
  destinations IS the view — nothing named that should not be) against
  the fake D-Bus connection; the host-refusal pins (an incapable host
  fails Start with the probe's reason; a start spec with no sandbox is
  refused by the runner).
- Integration: real systemd in the dev-systemd container — the
  property set is read back from the transient unit, SIGTERM reaches
  the app inside its PID namespace, escalation, no orphans after
  SIGKILL. Once during development, the same run with `PrivatePIDs=`
  removed made the `/proc/<manager-pid>/root` assertion below fail,
  proving the PID namespace is the load-bearing piece.
- e2e: ALL scenarios pass sandboxed (the zero-downtime suite doubles as
  sandbox-compat proof); one scenario deploys a probe app that
  attempts to read a sibling's release dir, a sibling's env file path,
  the admin socket, the user manager's private socket,
  `/proc/<hotserve-pid>/environ` and `/proc/<user-manager-pid>/root`,
  and asserts every attempt fails and that `/proc` lists only
  in-namespace PIDs. The probe app also asserts the positive contract:
  a DNS lookup and an outbound HTTP fetch succeed (the resolv.conf trap
  would otherwise ship silently), `$HOME` is writable, and the scrubbed
  env does NOT contain a seeded supervisor secret (a test ACME token).
- install-test: the smoke asserts the sandbox from inside the unit on
  Debian 13: pid 1 and a single-id `uid_map` (both namespaces),
  hotserve's state, sockets and a sibling's files absent, `/etc` only
  the base-view entries, and a runnable OS present so "absent" cannot
  mean "empty unit".
- soak: full churn; RSS/goroutine/fd deltas quantify the (expected
  ~zero) overhead as a measured claim.
- Every launch is sandboxed: `TestEveryLaunchIsSandboxed` asserts the
  specs the runner receives for a deploy, its `pre_start`, and a
  relaunch from a record (including one with no sandbox field) all
  carry the app's sandbox, and `TestUnitRefusesToRunWithoutASandbox`
  that the runner refuses a spec without one.
- Negative test once during development: widen the view (add a bind
  for `/etc/hotserve`) and confirm the e2e isolation scenario fails.
  Under a deny-by-default view the mutation to make is *adding* a
  name, not removing a mask.
- Mind the failure *signature*: under a deny-by-default view a
  runtime that probes a path nobody named gets **ENOENT** ("no such
  file or directory"), not EACCES or EROFS. It is why a missing
  runtime surfaces as `203/EXEC` rather than a permission complaint.
  Any journal grep in packaging/test/smoke.sh that scans for denial
  strings must be scoped to the stage it guards: sandboxed runtimes
  legitimately probe absent paths at startup, and the isolation
  assertions intentionally generate such lines.

## Non-goals

- Resource limits (`MemoryMax=`, `TasksMax=`, `CPUQuota=`) — real the
  moment they are set, because `/sys/fs/cgroup` is read-only in the
  unit; unset by design until an app needs bounding (#52).
- Network egress control — kernel sandboxes cannot scope by hostname.
  The sandbox is the kernel-enforced ceiling; a runtime's own
  `--allow-*` flags are the app narrowing itself inside it, enforced
  in-process and therefore not a substitute — see liveswap/README.md,
  "Runtime permissions". liveswap stays runtime-agnostic: no app
  types, no flag synthesis, no parsing of `command`.
- Per-app UIDs — a later milestone behind a root-owned template or a
  minimal privileged helper; the unit's namespaces deliver the
  file/process isolation without them.
- Hand-rolled seccomp filtering — exactly the "looks configured,
  quietly weaker" trap. systemd's curated `@system-service` set is a
  vetted filter adopted wholesale.
- Protecting an app from itself — its own env and its own database
  are legitimately reachable by definition.
- macOS/Windows sandboxing — servers are Linux; elsewhere the probe
  refuses to start, which is the documented behaviour, not a no-op.

## `extra_path` — rules for when it returns

`extra_path` — a per-app declaration binding a host path into the view
(ro by default, rw when declared) — was designed, reviewed and lifted
out of #40 before merge. It is the only mechanism that *adds* to a
deny-by-default view, it is operator-controlled, and it has to hold
simultaneously against symlinks, TOCTOU, cross-app containment and the
base view; every real defect across six review rounds was in it. It
returns when a running app needs a path outside its own dirs, designed
and reviewed on its own. Until then: persistent data goes in `shared/`;
runtimes go under `/usr` or ship in the release; a same-box database
is reached over TCP loopback.

The rules below were paid for and should not be rediscovered: a bind
must BE the directory it names (equality, not resolve-then-check — a
mount resolves to itself and an app cannot forge one); overlap is
symmetric; the base view may not be named at all, read-only or
writable; a missing source must fail the unit rather than be skipped
(an `IgnoreENOENT` bind does not defer a missing source, it drops it,
and the app serves permanently blind to a path it declared); a
writable `extra_path` aliasing over a second one must be refused (it
let a compromised app repoint the second at any directory on the box
between launches, a sibling's `env_file` included, with every
containment check passing because the planted target was a real path
that simply was not the one the operator named); `{env.*}` expansion
applies like every other path option; and the cross-app `env_file`
check must compare canonical *and* configured spellings, resolving the
deepest existing ancestor for a path whose leaf does not exist yet.

## History

Dated one-liners; the full text of each is in git.

- 2026-08 — Design brief written for bubblewrap as the mechanism (M8):
  spawn path, signal trap, probe-and-fallback ladder, `Depends:
  bubblewrap`. Never shipped.
- 2026-08-30 (#38, `6a080d0`) — The shared-UID rule and the spike
  measurement that a user namespace closes cross-process `/proc` reads
  made systemd's own namespaces the mechanism; bubblewrap dropped, not
  layered.
- 2026-08-31 (#40 branch) — Deny-by-default view replaces
  `ProtectSystem=strict` + a derived `InaccessiblePaths=` set (which
  could be incomplete and aged between deploys); `/etc` named entry by
  entry; bind sources must be the directories they name.
- 2026-09-01 (`3446a53` on the #40 branch; tier code removed in #47
  `ce97f37`) — Debian 13 only: one tier, AppArmor profile and
  user-manager wrapper deleted, CI sets
  `apparmor_restrict_unprivileged_userns=0`.
- 2026-09-02 (#40 branch, `446774e`/`719c898`) — `extra_path` lifted
  out before merge; rules kept above.
- 2026-09-02 (#40 merged, `6e8dfa2`) — Sandbox shipped: user + PID
  namespaces, one tier, `sandbox auto|require|off`, a recorded tier in
  `state.json` with "engage on next deploy, never on a relaunch", and
  a four-step operator rollout ladder.
- 2026-09-03 (#48 `5e36764`) — Capability measured once per manager
  connection, not per config load; failure never cached.
- 2026-09-03 (#49 `20e61c1`) — `auto`/`require` collapsed to `on|off`
  (`auto` ran apps bare with a WARN on an incapable host — the trap
  this document names); #48's cache invalidation deleted as
  unworkable (`Type=simple` semantics, pinned above).
- 2026-09-04 (#51 `24ff8e9`) — `sandbox off`, the recorded tier, the
  status field and the rollout ladder removed; the runner refuses a
  spec without a sandbox; reattach verifies the live unit's
  namespaces. `off` was per-app in syntax only: on a shared uid a bare
  app read every sibling's data and hotserve's keys.
