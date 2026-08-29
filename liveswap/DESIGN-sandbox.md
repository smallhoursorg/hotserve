# DESIGN — per-app sandboxing (M8)

Status: proposed. This document is the implementation brief for
sandboxing deployed apps with bubblewrap: the threat model, the
normative behavior, the architecture decisions and their traps, and
what is deliberately left out.

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

- Sandboxing MUST default to on (`sandbox auto`) when bubblewrap and
  user namespaces are available, and MUST be per-app configurable:
  `auto` (sandbox if available, else run bare with a prominent WARN at
  provision and at every spawn), `require` (provision fails if the
  sandbox cannot engage), `off`.
- The sandboxed filesystem view MUST contain, at their REAL host paths
  (no remapping — `current` symlinks, state paths and operator
  debugging all assume real paths):
  - the app's release dir (rw), shared dir (rw), and the release
    being started;
  - the app's own tmp dir (`appDirs.tmp`) bind-mounted as `/tmp` (rw):
    disk-backed like today, per-app private, no tmpfs RAM surprise;
  - read-only system paths: `/usr`, `/bin`, `/sbin`, `/lib*`, `/etc`,
    plus a fresh `--proc /proc` and minimal `--dev /dev`;
  - each configured `extra_path` (ro by default, rw when declared).
- The view MUST NOT contain: other apps' directories, `/var/lib`
  outside the app's own subtree (TLS keys live there), `/run/hotserve`
  (admin socket), and `/etc/hotserve` (env files — masked with a
  tmpfs over the ro `/etc` bind; apps never legitimately read their
  env *file*, they receive env *variables*).
- `/run` MUST NOT be bound wholesale — the admin socket is masked by
  absence, which is the strong form of masking. But
  `/run/systemd/resolve` MUST be bound ro when it exists: on
  systemd-resolved hosts (every Ubuntu in the support matrix)
  `/etc/resolv.conf` is a symlink into that directory, and without the
  bind it dangles — all DNS inside the sandbox fails, silently, on the
  most common distro. Local unix sockets (`/run/postgresql`,
  `/run/mysqld`, …) are deliberately absent; `extra_path` is the
  documented recipe (see config surface).
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
- The app MUST run inside a new PID namespace with bubblewrap's
  reaper as in-namespace init (NOT `--as-pid-1`): siblings are
  invisible and unsignalable, and orphaned grandchildren are reaped.
- `--die-with-parent` MUST be set (supersedes Pdeathsig for the
  sandboxed path) and `--new-session` MUST be set (CVE-2017-5226
  class: terminal injection).
- The network namespace MUST be shared: the app binds 127.0.0.1:$PORT
  and hotserve proxies to it, unchanged.
- `pre_start` MUST run under the same sandbox as the app it precedes
  (a migration that writes where the app cannot read is a bug caught
  at deploy time, not 3am).
- Stop semantics MUST remain: SIGTERM to the process group, `grace`,
  then SIGKILL — with namespace teardown as the backstop (killing the
  in-namespace init kills everything in the namespace; no survivor is
  possible). The existing escalation tests define the contract.
- The status endpoint MUST report whether the running instance is
  sandboxed (`"sandboxed": true|false`) — observability for operators
  and the assertion hook for smoke tests.
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

Raw bubblewrap arguments are deliberately NOT exposed. The
declarations are the stable contract; bubblewrap is the current
backend. The same declarations must be implementable by Landlock (the
no-dependency fallback candidate) and by the v2 systemd runner's
sandbox directives — locking users to `--bind` flags would foreclose
both. This mirrors how Flatpak and systemd treat their sandbox
plumbing, and prevents the "looks configured, quietly weaker" failure
mode of hand-rolled flag soup.

## Architecture

### Spawn path (runner_systemd.go)

> Note (systemd runner): apps are now transient systemd units, so the
> spawn is `ExecStart=` on the unit and bwrap becomes its argv prefix;
> the process-group discussion below is moot — the unit's cgroup is
> what Stop signals, bwrap or not. On hosts with unprivileged user
> namespaces, systemd's own per-unit sandboxing (`ProtectProc=`,
> `InaccessiblePaths=`, `ProtectSystem=strict`) may cover much of this
> without bwrap; reconciling the two is the next design pass.

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

A hotserve binary upgrade triggers the second path for every app at
once (postinstall `try-restart` → supervisor restart → `--die-with-parent`
kills the children → relaunch from `state.json`). Therefore:

- Enabling sandboxing MUST NOT force an already-running app into a
  sandbox on the upgrade's restart relaunch. Sandbox engagement for an
  existing app MUST ride its **next deploy** (the path that has the
  fallback). Concretely: an app whose recorded running instance is
  bare relaunches **bare** after a supervisor restart even when config
  now says `sandbox auto`; the new isolation takes effect on the next
  cutover, where the health gate protects it. Record the sandbox
  disposition of the running instance in `state.json` so a relaunch
  reproduces what was actually running, not what config now wants.
- `auto`'s graceful degrade covers **host incapability only** (no
  bwrap / userns denied → bare + WARN). It does NOT cover a per-app
  misconfiguration on a capable host — a missing `extra_path` still
  fails the app. The doc must not imply `auto` is a safety net for
  profile mistakes; the deploy-path fallback is that net.
- A global `sandbox require` is the one setting that can take down the
  **whole server** rather than one app: `require` fails *provision*, so
  on a host without working userns (LXC VPS, locked-down kernel)
  hotserve does not come up at all — admin socket and proxy included.
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

- Unit: `buildBwrapArgs` table tests (paths, masking, extra_path
  ro/rw, real-path invariants); fallback ladder with a fake prober
  (auto-degrade warns, require fails provision).
- Integration: real bwrap in the dev container (compose `security_opt`
  to permit userns under Docker's default seccomp; skip-guard when
  unavailable) — spawn, group-SIGTERM reaches the app, escalation,
  no orphans after SIGKILL.
- e2e: bwrap installed in the e2e image + `security_opt` on
  e2e-hotserve; ALL existing scenarios pass sandboxed (the
  zero-downtime suite doubles as sandbox-compat proof); one new
  scenario deploys a probe app that attempts to read a sibling's
  release dir, a sibling's env file path, and the admin socket, and
  asserts every attempt fails. The probe app MUST also assert the
  positive contract: a DNS lookup and an outbound HTTP fetch succeed
  (no current test app makes any outbound call, so the resolv.conf
  trap would otherwise ship silently), `$HOME` is writable, and the
  scrubbed env does NOT contain a seeded supervisor secret (e.g. a
  test ACME token).
- install-test: smoke stage 2 asserts `"sandboxed": true` in the
  status response on every distro cell — this is where Ubuntu's
  AppArmor userns policy is proven per-release, per-arch, under the
  real systemd unit.
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

- Resource limits (memory/CPU) — the v2 systemd-transient-units
  runner tier; bubblewrap has no cgroup story.
- Network egress control — kernel sandboxes cannot scope by hostname;
  document Deno/Node permission flags as the per-runtime option.
- Per-app UIDs — the systemd runner; bubblewrap's mount/PID
  namespaces deliver the file/process isolation without them.
- seccomp filtering — Flatpak ships a curated, battle-tested filter;
  hand-rolling one here is exactly the "looks configured, quietly
  weaker" trap this document warns about. Revisit only if a vetted
  filter can be adopted wholesale.
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
