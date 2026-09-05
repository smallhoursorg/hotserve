# DESIGN — threat model

This document describes the present: what is built, why, and what it
promises. History lives in git (`git log -- DESIGN-threat-model.md`);
the "History" section at the end holds only dated one-liners.
Amendments are for decisions still fresh or contested and get folded
into the body once they settle.

This document states what hotserve is defending, who the attacker is,
the concrete paths from an entry point to an asset, and how the shipped
isolation closes or bounds each one.
[liveswap/DESIGN-sandbox.md](liveswap/DESIGN-sandbox.md) is authoritative
for the sandbox's behaviour specification; this document places that
work in the wider attack surface rather than restating it.

Scope: a single Debian 13 box running `hotserve` (a Caddy distribution)
as the `hotserve` system user, supervising deployed apps as transient
systemd units under the hotserve user's own service manager via
liveswap's `systemdRunner`
([liveswap/runner_systemd.go](liveswap/runner_systemd.go)). Multi-node,
Windows, and macOS-as-a-server are out of scope by product design.

## Assets (what an attacker is after), ranked

1. **ACME DNS tokens** — in the supervisor env; issue/alter certs. The
   highest-value item on the box. The `/proc/<supervisor>/environ`
   route is closed by the non-dumpable supervisor and by the app-side
   namespaces ("The shared-UID rule"). Still on disk wherever Caddy
   persists config (`/var/lib/hotserve/caddy/autosave.json`) — a
   filesystem route, closed by the sandbox view.
2. **TLS private keys** — `/var/lib/hotserve/caddy/**`, mode `0750`
   owned by `hotserve`.
3. **Admin API socket** — `/run/hotserve/admin.sock`
   ([packaging/Caddyfile:18](packaging/Caddyfile)); reconfigures the
   whole server. Gated on being the `hotserve` user, not on the network.
4. **Per-app secrets** — an app's own env vars / `env_file`
   (`/etc/hotserve/*.env`). Legitimately reachable by that app; the
   goal is to keep them from *siblings*.
5. **Sibling app data** — `/var/lib/liveswap/<app>/{releases,shared,state.json}`.
6. **System integrity** — root, persistence, other system services.
7. **Availability** — serving traffic and the deploy pipeline.

There is no deploy secret on the box. Deploys are authenticated by a
verified JWT (CI OIDC or a local public key); the verifier holds only
public material — see "Reducing the asset".

## Trust boundaries and entry points

All line references are against the working tree at time of writing.

### Webhook endpoint — `liveswap/handler.go`

The only application-level authenticated entry point. Auth is a
verified JWT in `Authorization: Bearer` (see "Reducing the asset"):
`deploy_trust` sources verify the token's signature and standard claims
against public material, then a claim allowlist. Auth happens **before**
existence is revealed: an unknown app name is verified against the
*global* trust sources so callers cannot enumerate app names
([handler.go:76-110](liveswap/handler.go)). Config load refuses an app
that resolves to zero trust sources ([liveswap.go](liveswap/liveswap.go)).
Bearer is the only transport, which Caddy redacts from access logs.

Properties that matter to the model:

- **GET and POST both sit behind the token.** GET returns full status,
  POST deploys, all else 405 ([handler.go:101-109](liveswap/handler.go)).
  The status endpoint is authenticated — not public.
- **No rate limiting anywhere on the auth path.** No throttle, lockout,
  or backoff. Token *forgery* is infeasible (no private key), so this is
  not a guessing oracle — but each failure logs at Warn with `app`+`remote`
  ([handler.go:90-95](liveswap/handler.go)), an unauthenticated
  log-amplification / disk-fill primitive, and every attempt costs a
  JWT/JWKS verification. Body is capped at 64 KiB → 413
  ([handler.go:37,133-140](liveswap/handler.go)); `deployMu.TryLock()`
  → 409 serializes deploys ([app.go:258-260](liveswap/app.go)) but does
  nothing for auth attempts.
- **Path routing is `path.Base(path.Clean(...))`**
  ([handler.go:77](liveswap/handler.go)): `/anything/deep/myapp` targets
  `myapp`. A naive `path /deploy/*` site matcher does not constrain the
  app name; the operator's matcher is the only constraint.
- **Payload:** three fields only — `url`, `version`, `auth_header`
  ([app.go:112-116](liveswap/app.go)); unknown JSON silently ignored (no
  `DisallowUnknownFields`). `version` is
  `^[A-Za-z0-9_-][A-Za-z0-9._-]{0,63}$` (no leading dot, so never
  `.`/`..` or a release-GC bookkeeping name), double-sanitized before
  touching the filesystem
  ([liveswap.go:65,77-79,85-87](liveswap/liveswap.go)). `auth_header`
  is only control-char-checked ([handler.go:164-169](liveswap/handler.go));
  its contents are attacker-chosen and forwarded to the allowlisted host.
- **Response leaks (all post-auth):** the 500 path returns raw
  `err.Error()` plus the full status snapshot
  ([handler.go:254-258](liveswap/handler.go)) — filesystem paths, tar
  entry names, the operator's allowlist echoed verbatim
  ([allowlist.go:279-281,363,379](liveswap/allowlist.go)). The status
  snapshot exposes the app's **port and PID** and watchdog cause/failure
  state ([app.go:519-543](liveswap/app.go),
  [watchdog.go:234-254](liveswap/watchdog.go)). Artifact URLs *are*
  redacted before logs/errors ([download.go:158-163](liveswap/download.go)),
  so query signatures do not leak.
- **Replay / downgrade is bounded.** The bearer JWT is short-lived
  (`exp`), so a captured request is replayable only within that window.
  Versions are immutable: re-deploying an existing version (running or
  not) is refused 422, so replaying an *older* deploy's payload does not
  downgrade the app. A token-holder can still downgrade deliberately
  via `?rollback=<version>` — an explicit, audited operation
  (`deployed_by` + a `source:rollback` log), not a silent replay
  side-effect.

### Artifact fetching — `liveswap/download.go` + `allowlist.go`

The allowlist is **mandatory** — config load fails without one
([liveswap.go:397](liveswap/liveswap.go)); no any-origin mode. Pinning
([allowlist.go:382-449](liveswap/allowlist.go)) rebuilds the outgoing
URL so scheme is constant, host/port/path-prefix come from *config
bytes*, and only the path suffix + query come from the payload — the
request can never contribute host bytes, with two fail-closed re-checks
(port [allowlist.go:407](liveswap/allowlist.go), query
[allowlist.go:418](liveswap/allowlist.go)) and a prefix-boundary guard
([allowlist.go:433](liveswap/allowlist.go)). Canonicalization
([allowlist.go:180-227](liveswap/allowlist.go)) and query gating
([allowlist.go:238-284](liveswap/allowlist.go)) are thorough and
closed-by-default.

`https` only unless `allow_insecure_http`
([download.go:41-49](liveswap/download.go)), re-enforced on **every
redirect hop** ([download.go:142-152](liveswap/download.go)) — closing
Go's default https→http downgrade. `auth_header` is stripped by stdlib
on cross-host redirects but **not** same-host
([download.go:77-82](liveswap/download.go)). Size: Content-Length
pre-check plus streaming `LimitReader`, default 100 MB
([download.go:94-114](liveswap/download.go),
[liveswap.go:332-333](liveswap/liveswap.go)).

**The documented, real gap:** the host allowlist governs the **first
hop only** — `CheckRedirect` deliberately does not re-check the host
per hop, because the GitHub→S3 redirect is load-bearing
([download.go:128-135](liveswap/download.go)). So an allowlisted first
host can redirect the fetch to *any* https host — LAN, internal, an
https metadata endpoint. "https-only" is a partial SSRF barrier (it
stops plain-http metadata endpoints, not an attacker's https target).
Reaching it requires a valid deploy token **and** an allowlisted first hop.
Secondary: a malicious host can trickle bytes under the cap (only a
30 s `ResponseHeaderTimeout`, [download.go:136-141](liveswap/download.go))
to hold the per-app deploy lock open — DoS-of-deploys, not of serving.

### Tar extraction — `liveswap/extract.go`

Two-pass (validate-all, then write — no partial residue,
[extract.go:26-36](liveswap/extract.go)). Traversal via stdlib
`filepath.IsLocal` in both passes
([extract.go:82,108,165-177](liveswap/extract.go)). Symlink/hardlink
targets must resolve inside the archive root
([extract.go:88-96,183-196](liveswap/extract.go)). Modes: `Perm()|0600`
strips setuid/setgid/sticky structurally
([extract.go:123](liveswap/extract.go)); dirs forced `0750`. Devices/
FIFOs rejected. Decompression bomb capped at `max_artifact_size × 10`
over the *decompressed* stream ([extract.go:17,52](liveswap/extract.go)).

Residual items for the model:

- **Link TOCTOU shape (unproven, worth review):** validation is
  *symbolic* (string resolution); writing
  ([extract.go:133-146](liveswap/extract.go)) does `os.Symlink` then
  later `os.Link`/`os.OpenFile` under `destDir` with no `openat`-style
  re-check after intermediate symlinks exist on disk. Each entry name
  passes `IsLocal`, but nothing resolves the on-disk path *through*
  an earlier-written symlink. Blast radius is bounded (targets must stay
  symbolically under root; extraction is into a hidden staging dir
  `os.Rename`d on success, [download.go:196-215](liveswap/download.go)).
  Not asserted as exploitable.
- **No entry-count or path-length cap** independent of the byte budget:
  a billion 1-byte entries fits in 1 GB → CPU/inode exhaustion. The
  archive is read **twice**, so the byte cap permits 2× the work.
- `+x` survives extraction — intended (the artifact ships the app
  binary), but it is the point where artifact bytes become code.

### Reverse proxy, admin socket, penaltybox

The starter config exposes only `:80` returning a static string; the
entire liveswap/webhook block ships commented out
([packaging/Caddyfile:26-79](packaging/Caddyfile)) — a fresh install
has no deploy endpoint. Admin is off TCP, on
`unix//run/hotserve/admin.sock` ([packaging/Caddyfile:18](packaging/Caddyfile)),
`RuntimeDirectoryMode=0750` owned by the service user
([packaging/hotserve.service:26-29](packaging/hotserve.service)) — so
admin access is gated on *being the `hotserve` user*. Every deployed
app is that user, but a sandboxed app cannot reach `/run/hotserve` at
all (the path is not in its view), so the gate holds against apps and
only hotserve itself can connect. The example webhook deployment
([Caddyfile:26-40](packaging/Caddyfile)) is a public TLS vhost with
`deploy_trust` and no rate limiting in front.

**penaltybox** is a response-phase rate-limit-hint enforcer; it touches
untrusted input only narrowly (the client key defaults to
`{http.vars.client_ip}`, deferring XFF trust to `trusted_proxies`;
[penaltybox/penaltybox.go:34-37,109-114](penaltybox/penaltybox.go)). A
misconfigured `trusted_proxies` turns client bytes into store keys,
bounded by `max_keys` (default 100 000, idle-eviction). The origin's
hint header is strict-parsed and fails open. It is a minor surface:
denial-of-protection under bad config, not injection or exhaustion.

### Supervisor⇄app and app⇄app boundaries

Every app runs as `hotserve`, in its own user namespace and its own PID
namespace, with a deny-by-default filesystem view — see "The shared-UID
rule" and "The shipped mechanism". The network namespace is shared, by
design. The unit environment is the user manager's defaults
(`INVOCATION_ID`, …; `XDG_RUNTIME_DIR` and `DBUS_SESSION_BUS_ADDRESS`
are unset because `/run/user` is not in the view) plus an allowlisted
slice of hotserve's (`PATH, HOME, LANG, TZ, LC_*`,
[app.go](liveswap/app.go) `inheritedEnv`) — closing *direct*
inheritance of ACME tokens and any other supervisor secret
(`TestBuildEnvDoesNotLeakSupervisorSecrets`). The `/proc` route is
closed twice over (non-dumpable supervisor; cross-namespace refusal);
the filesystem routes are closed by absence.

### Install-time — `packaging/postinstall.sh`

Runs as root at install; creates the `hotserve` user, chowns
`/var/lib/{hotserve,liveswap}`, enables linger so the hotserve user
manager runs without a session. Supply-chain attacks on hotserve's own
build land here, but they run in CI, not from a deployed app — out of
scope for the runtime model, in scope for release signing (roadmap).

## Attacker profiles (ranked by likelihood)

- **T1 — Poisoned dependency running *inside* a deployed app.** The
  primary threat, per the npm 2025 wave: a transitive dependency's
  runtime payload sweeps the filesystem for `.env`/tokens and
  exfiltrates. Already has code execution as `hotserve`. This is the
  attacker per-app isolation exists to stop.
- **T2 — Stolen deploy token / compromised CI identity.** A short-lived,
  claim-scoped token (or control of the CI identity that mints one). Can
  deploy arbitrary code and roll back versions within
  `artifact_allowlist` for the token's lifetime; containment is the
  allowlist and the claim scope, not the runtime.
- **T3 — Malicious/compromised artifact host** within the allowlist
  pin. Controls tarball bytes, status, redirect targets (any https
  host), timing. Faces `extract.go` and the first-hop SSRF gap.
- **T4 — Unauthenticated network attacker** on the public webhook/proxy.
  Faces the token gate — forgery needs a private key, so there is no
  guessing oracle; the realer wins are log-amplification, the CPU cost of
  JWT/JWKS verification (no rate limit), and any pre-auth proxy/Caddy
  surface.
- **T5 — RCE in the supervisor itself** (a Caddy or liveswap bug). Low
  probability, catastrophic: it *is* the `hotserve` user, so it already
  holds every asset short of root. No app-isolation design prevents
  this; the design question is only how much *worse* it can get (does it
  reach root?) — which is why the supervisor holds no grant (see "The
  shipped mechanism").

For **T2**: deploy-arbitrary-code (contained only by the allowlist);
deliberate rollback. For **T3**: archive-borne CPU/inode exhaustion; the
link-TOCTOU shape (unproven); first-hop→any-https SSRF. For **T4**:
log-amplification disk-fill (online *token forgery* is infeasible).
For **T5**: total, by definition — the containment question is
root-vs-not-root.

## The shared-UID rule

Everything liveswap touches runs as one UID: hotserve, the hotserve
user's `systemd --user` manager, and every app. The kernel gates
`/proc/<pid>/{environ,root,cwd,mem,fd,maps}` on
`ptrace_may_access(PTRACE_MODE_READ)`, which any same-UID caller passes
while the target is dumpable — regardless of the caller's mount
namespace or seccomp filter, **but not across user namespaces**: after
the uid and dumpable checks the LSM hook runs, and commoncap's
`cap_ptrace_access_check` refuses a caller whose `user_ns` differs
from the target's unless the caller holds `CAP_SYS_PTRACE` *in the
target's namespace*, which an app in a child namespace never does.
(Measured on the 2026-08-30 spike — bare: `/proc/<manager>/root` open;
`PrivateUsers=yes` alone: denied.) Three consequences, and they decide
the mechanism:

1. **No mount sandbox without a namespace the ptrace check honours.**
   A sandboxed app that can `ptrace`-read any same-UID PID outside its
   sandbox opens `/proc/<that-pid>/root/…` and walks the host
   filesystem. The user manager (host root, same UID, always running)
   is a permanent such target; every sibling app is another.
   `ProtectSystem=strict`, `InaccessiblePaths=`, any bind-mounted view
   are all void without one. `ProtectProc=invisible` does not
   substitute: `hidepid` hides other *users'* processes, and there are
   no other users here. Two namespaces do, and they close different
   things:
   - a **user namespace** (`PrivateUsers=yes`) closes the `/proc`
     reads — `root`, `environ`, `cwd`, `mem`, `fd` — of every process
     outside it, so the mount restrictions hold. Signals still
     deliver: `kill` checks uids, not namespaces.
   - a **PID namespace** (`PrivatePIDs=yes`) makes those processes
     invisible and unsignalable, and gives the unit an in-namespace
     init.

   Every app unit gets both. There is no lesser configuration and no
   opt-out: a host that cannot deliver both namespaces fails the start
   rather than running any app bare (`unitFor` refuses a spec without
   a sandbox). A unit hotserve did not start with them — a stale one
   from a development build, or another same-uid process's — is
   refused at reattach and replaced by a sandboxed relaunch, so "every
   unit" holds for adopted units too. That check is for an honest
   stale unit; a same-uid process that starts units on purpose can
   give one any property set, and is the trust domain, not a boundary
   the check could hold.

   **Corollary: an opt-out is impossible on a shared uid.** A per-app
   "sandbox off" reads as per-app; it is not — a bare app reads every
   sibling's data and hotserve's keys. The rule that makes the user
   namespace load-bearing is the rule that makes any app outside a
   namespace every app's problem.

2. **A non-dumpable supervisor is the floor.**
   `prctl(PR_SET_DUMPABLE, 0)` makes hotserve's `/proc` entries require
   `CAP_SYS_PTRACE`, which apps under `NoNewPrivileges` never hold —
   with or without user namespaces. `liveswap/harden` is a leaf package
   whose `init` runs right after `syscall`'s — before `os`, `fmt`,
   Caddy and every package depending on them; only
   `syscall`-closure-only leaves that sort earlier can precede it, and
   those cannot touch `/proc` (measured with `GODEBUG=inittrace=1`:
   the 17th of 460 initializers on hotserve, two such leaves ahead of
   it; `TestInitRunsBeforeOS` pins `syscall < harden < os` in any
   binary that runs it) — so any binary importing liveswap (hotserve or
   an xcaddy build) is non-dumpable before `main`; a failure is fatal.
   Pinned by a unit test and by the real-systemd e2e suite (scenario
   11). It closes the `/proc/<supervisor>/environ` and
   `/proc/<supervisor>/root` routes only; TLS keys on disk, the admin
   socket and sibling files need the mount namespace.

   *Residual, recorded because `liveswap/harden` cites it:* app units
   outlive supervisor restarts, so a same-UID app could in principle
   race the interval between `execve` and that `init` — well under a
   millisecond — and read the new supervisor's environment. On Debian
   13 that is a *read* race: Yama's default
   `kernel.yama.ptrace_scope=1` forbids a non-descendant from
   `PTRACE_ATTACH`/`PTRACE_SEIZE` at any time (Yama gates
   `PTRACE_MODE_ATTACH`, which the dumpable flag does not govern), so
   an app cannot seize the supervisor in the window; a host set to
   `ptrace_scope=0` is outside this model. The window is closed from
   the app's side by the namespaces above — an app in its own user
   namespace cannot read the supervisor's `/proc` at all, and one in
   its own PID namespace cannot see the supervisor's PID — and since
   every app is sandboxed it stands for nothing on a running box.

   *The capability probe runs under the shared uid.* It starts a real
   transient unit, bounded at 30 s, so any process holding that uid can
   interfere with it. A failed probe fails the whole server start, so
   this is an availability attack on the supervisor, not a way to
   weaken an app: interference cannot produce a running hotserve with
   a lesser sandbox. The verdict is cached per manager connection
   (`userManagerClient.sandboxCapability`,
   [liveswap.go:577](liveswap/liveswap.go)), which narrows the window,
   and a failed verdict is deliberately NOT cached, so interference
   costs the next start rather than pinning a verdict until the manager
   restarts. The remedy is to remove the interfering process, which
   runs as the hotserve uid and is therefore either hotserve's own app
   or something already in the trust domain.

3. **Resource caps need a read-only cgroupfs inside the sandbox.** The
   cgroup subtree under `user@<uid>.service` is delegated to — owned
   by — the hotserve UID, so `MemoryMax=`/`TasksMax=` on a user-manager
   unit is a limit the app can rewrite in `/sys/fs/cgroup` (or escape
   by migrating its PIDs) unless that tree is read-only in its view.
   Runtimes only ever *read* it. `ProtectControlGroups=` is set on
   every unit, so caps are real the moment they are set; none is set
   today — the trigger is an app that needs bounding (#52).

## The shipped mechanism

systemd's own per-unit sandboxing on the user-manager runner, issued as
transient-unit properties by a supervisor that holds no grant.

**Why the user manager and not the system one.** polkit is blind to
unit *properties*, so a `StartTransientUnit` grant on the *system*
manager that the supervisor can shape is root-equivalent (a unit can
name `User=root`, any `ExecStart=`) — a naive systemd runner would
make T5 *worse*. hotserve instead talks to the hotserve user's own
manager over its private socket (`/run/user/<uid>/systemd/private`):
no session bus, no polkit, no grant of any kind. It can create units
only as itself, under `NoNewPrivileges`, so a supervisor RCE gains
nothing it did not already have (T5 unchanged). What only the system
manager can buy — per-app `User=`, `IPAddressDeny=` egress filtering —
stays future work and, if wanted, must arrive as a root-owned template
or a minimal privileged helper, never as a system-manager transient
grant.

**The property set** (`sandboxProperties`,
[liveswap/systemd_dbus.go](liveswap/systemd_dbus.go)), on every unit:

- Namespaces: `PrivateUsers=yes`, `PrivatePIDs=yes`, `PrivateTmp=yes`,
  `PrivateDevices=yes`; `RestrictNamespaces=` (empty set — an app
  cannot create further namespaces).
- View: `TemporaryFileSystem=/:ro` replaces the whole filesystem with
  an empty read-only tmpfs; `BindReadOnlyPaths=` puts back a named OS
  base view and `BindPaths=` the app's release and `shared/` dirs.
  Nothing else exists inside. There is no `InaccessiblePaths=` and no
  `ProtectSystem=` because there is nothing left for either to act on
  — an unnamed path is absent, not merely unreadable. The view is
  deny-by-default, so nothing is derived from the running
  configuration and nothing ages: a secret declared tomorrow is absent
  from a unit started yesterday for the same reason every other path
  is.
- Kernel and privilege surface: `ProtectControlGroups=`,
  `ProtectKernelTunables=`, `ProtectKernelModules=`,
  `ProtectKernelLogs=`, `RestrictRealtime=`, `RestrictSUIDSGID=`,
  `LockPersonality=`, `NoNewPrivileges=`, `CapabilityBoundingSet=`
  (empty), `SystemCallFilter=@system-service` with
  `SystemCallErrorNumber=EPERM`, `RestrictAddressFamilies=AF_INET
  AF_INET6 AF_UNIX AF_NETLINK` (netlink stays read-only for
  `getifaddrs()`, which Node and Go frameworks call at startup).
- Lifecycle: `Restart=no` (the liveswap watchdog is the sole
  restarter), `KillMode=control-group`,
  `UnsetEnvironment=XDG_RUNTIME_DIR DBUS_SESSION_BUS_ADDRESS`.

**The probe stays.** Before the first unit on a manager connection,
hotserve starts a real transient unit with this property set and reads
back from inside it that both namespaces engaged. The probe is not a
proxy for the systemd version — it is what catches a container, an LXC
VPS or a kernel built without user namespaces, all of which can present
a supported manager version and still refuse the unit. CI sets
`kernel.apparmor_restrict_unprivileged_userns=0` on GitHub's Ubuntu
kernel to make the runner behave like a Debian host, so no lane
exercises a restricted kernel; the probe is the only thing standing
between such a host and a silent loss of isolation. Deleting it would
turn a measurement into a claim.

**What it does not do**, by design: the network namespace is shared
(sibling `127.0.0.1:$PORT` reachable; egress open); there are no
per-app UIDs; resource caps are unset. Each is in "Residual risks".

## Reducing the asset (deploy auth)

The two mitigation axes are orthogonal: *isolate the runtime* (above)
and *shrink the prize*. A symmetric, long-lived deploy secret would be
bad on two counts: the verifier would store the same value it checks
(so the prize physically sits in the supervisor's env), and theft would
be permanent. hotserve uses asymmetric verification — the box holds
only public material:

- **OIDC (CI, primary):** the box verifies a per-run token against the
  provider's public JWKS and a claim allowlist (`deploy_trust github |
  gitlab | oidc`). Nothing high-value on the box, nothing stored in CI;
  the token is minted per run, short-lived, and scoped to
  `repository`/`ref`/`environment` claims.
- **Local key (non-CI / fallback):** the box trusts a public key
  (`deploy_trust local`); the operator mints tokens with the private
  half (`hotserve deploy-token`). The signing key never touches the box.

Implementation: `liveswap/deploytrust.go` (verification via the vetted
`go-oidc`/`go-jose`, never hand-rolled). Effects on the model:

- **No deploy credential on the box.** `/proc/<supervisor>/environ`
  would yield ACME tokens, not a deploy key; the top asset is ACME
  tokens.
- **T2 is bounded:** a stolen token is short-lived and claim-scoped, not
  a permanent deploy-anything key. The compromise surface is "the CI
  identity that mints tokens", not "a secret on the box".
- **T4 has no online guessing:** forging a token needs the issuer's or
  operator's private key; there is nothing to brute-force. (Rate
  limiting is still owed for the log-amplification vector.)
- **Replay is bounded** by the token's `exp` (minutes).

What it does **not** do: ACME tokens and TLS keys still live on the box,
so the sandbox is still required. Auth design shrinks the prize; it
does not isolate the runtime.

## Residual risks

- **An app's own secrets and its own database are reachable by
  definition** — not a bug, stated so operators do not expect otherwise.
- **Install-time supply chain** (hotserve's own build) is a CI concern;
  release signing is the roadmap answer, not a runtime control.
- **T5 is contained, not prevented** — a supervisor RCE holds every
  asset short of root. It does not reach root because the supervisor
  holds no grant; keep it that way.
- **Sibling localhost ports** stay reachable (shared netns). The clean
  fix is making the app→hotserve contract a unix socket in the app's
  own dir (Caddy `reverse_proxy` speaks `unix/`), which also enables a
  per-app netns later. It changes the app contract from `$PORT` to a
  socket path, so it is its own milestone.
- **Network egress** is open for every app. A runtime whose permission
  model gates the network (Deno's `--allow-net`) can close it from
  inside — see liveswap/README.md "Runtime permissions"; Node's
  `--permission` model does not cover network I/O. The runtime-agnostic
  answer is the unix-socket contract above plus `PrivateNetwork=`.
- **Resource exhaustion** — a runaway app can starve its siblings and
  Caddy (fork bomb, memory leak). `ProtectControlGroups=` makes
  `MemoryMax=`/`TasksMax=`/`CPUQuota=` real the moment they are set;
  nothing sets them until an app needs bounding (#52).
- **`state.json` must stay outside any writable sandbox view**
  ([liveswap/state.go](liveswap/state.go) is trusted on relaunch for
  the version and the unit). Normative and shipped: only the release
  being started, `shared/` and the OS base view are bound into the
  unit — the app dir root, `state.json`, `tmp/` (the upload staging
  dir: a running instance must not be able to rewrite the next
  version's tarball) and the other releases do not exist inside.
  `sandboxSpecFor` in liveswap/sandbox.go is the single place that
  list is built; `TestSandboxSpecFor` and
  `TestSandboxViewIsExactlyWhatIsNamed` pin it — the latter asserts the
  rendered set of bind destinations IS the view, so an accidental
  widening fails there.
- **Non-isolation hardening still owed:** webhook rate limiting (T4
  log-amplification) and the `extract.go` entry-count cap (T3).

## History

Dated one-liners; the full text of each is in git.

- 2026-08 — Threat model written as a decision record comparing three
  isolation stacks (A: UID + hardening + AppArmor; B: bubblewrap +
  delegated cgroups; C: root-owned systemd template). B was recommended.
- 2026-08-28 (#29, `465e846`) — Shared deploy secret (`LIVESWAP_SECRET`,
  `X-Liveswap-Secret` header) replaced by `deploy_trust` (OIDC + local
  key). The former top asset left the box.
- 2026-08-29 (#34, `17f1a04`) — Apps run as transient units on the
  hotserve user's own manager: neither B nor C. Restart-survival
  (`Reattach`) shipped with it.
- 2026-08-30 (#38, `6a080d0`) — Non-dumpable supervisor; the shared-UID
  rule recorded, corrected by the spike measurement that a user
  namespace *does* close cross-process `/proc` reads. That measurement
  made systemd's own namespaces the mechanism.
- 2026-08-30 (#40 branch; merged 2026-09-02 as `6e8dfa2`) — Sandbox
  built: two probe-gated tiers (*filesystem* on systemd < 256, *full*
  ≥ 256), deny-by-default view (replacing `ProtectSystem=strict` + a
  derived `InaccessiblePaths=` set that could go stale — the "view is a
  policy" residual). Ubuntu needed an AppArmor profile attached by path
  to a user-manager wrapper.
- 2026-09-01 (`3446a53` on the #40 branch; tier code removed in #47
  `ce97f37`) — Debian 13 only; one tier; AppArmor profile and wrapper
  deleted; CI sets `apparmor_restrict_unprivileged_userns=0`.
- 2026-09-03 (#48 `5e36764`) — Probe measured once per manager
  connection; failure never cached.
- 2026-09-03 (#49 `20e61c1`) — `sandbox auto`/`require` collapsed to
  `on|off`.
- 2026-09-04 (#51 `24ff8e9`) — `sandbox off`, the recorded tier and the
  status field removed; the runner refuses a spec without a sandbox;
  reattach verifies the live unit's namespaces. Closed by construction:
  the bind-source TOCTOU race (no process outside a sandbox shares the
  uid but hotserve), the app-writable recorded tier (no record), and
  "what a bare app leaves behind in `shared/`" (nothing runs bare). The
  pre-#40 "reachable today" attack-path table described bare apps and
  is history with them.
