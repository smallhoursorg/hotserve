# DESIGN — threat model and process-isolation evaluation

Status: analysis. This document states what hotserve is defending, who
the attacker is, the concrete paths from an entry point to an asset,
and how three candidate isolation/hardening stacks score against those
paths. It is the decision record behind the "per-app sandboxing"
roadmap item; [DESIGN-sandbox.md](liveswap/DESIGN-sandbox.md) remains
authoritative for the sandbox's behaviour spec, config surface and
rollout semantics (its bubblewrap mechanics are superseded — see "The
shared-UID rule"), and this document places that work in the wider
attack surface rather than restating it.

Scope: a single Debian 13 box (the support matrix; narrowed from
Debian 12/13 + Ubuntu 24.04/26.04 on 2026-09-01 — see "Amendment" at
the end of the Recommendation) running `hotserve` (a Caddy
distribution) as the `hotserve` system user, supervising deployed apps
as transient systemd units under the hotserve user's own service
manager via liveswap's `systemdRunner`
([liveswap/runner_systemd.go](liveswap/runner_systemd.go)). Multi-node,
Windows, and macOS-as-a-server are out of scope by product design.

> **Update — the deploy secret has been removed from the box.** Deploy
> auth is now asymmetric (`deploy_trust`, see "Reducing the asset"
> below): the supervisor holds only public material, so the former
> top asset — a symmetric `LIVESWAP_SECRET` readable via
> `/proc/<supervisor>/environ` — no longer exists. ACME DNS tokens and
> TLS keys remain, so the isolation work below still stands; the
> ranking is left intact as the pre-change record, with the deploy
> secret struck through.

## Assets (what an attacker is after), ranked

1. ~~**`LIVESWAP_SECRET`** — the deploy secret.~~ **Removed.** Deploys
   are now authenticated by a verified JWT (CI OIDC or a local public
   key); there is no shared deploy secret on the box. A stolen token is
   short-lived and audience/claim-scoped, and the verifier holds only
   public keys.
2. **ACME DNS tokens** — in the supervisor env; issue/alter certs.
   The highest-value item on the box. Was reachable via
   `/proc/<supervisor>/environ`; closed by the non-dumpable supervisor
   ("The shared-UID rule"). Still on disk wherever Caddy persists
   config (`/var/lib/hotserve/caddy/autosave.json`) — a filesystem route.
3. **TLS private keys** — `/var/lib/hotserve/caddy/**`, mode `0750`
   owned by `hotserve`.
4. **Admin API socket** — `/run/hotserve/admin.sock`
   ([packaging/Caddyfile:18](packaging/Caddyfile)); reconfigures the
   whole server. Gated on being the `hotserve` user, not on the network.
5. **Per-app secrets** — an app's own env vars / `env_file`
   (`/etc/hotserve/*.env`). Legitimately reachable by that app; the
   goal is to keep them from *siblings*.
6. **Sibling app data** — `/var/lib/liveswap/<app>/{releases,shared,state.json}`.
7. **System integrity** — root, persistence, other system services.
8. **Availability** — serving traffic and the deploy pipeline.

## Trust boundaries and entry points

All line references are against the working tree at time of writing.

### Webhook endpoint — `liveswap/handler.go`

The only application-level authenticated entry point. Auth is a
verified JWT in `Authorization: Bearer` (see "Reducing the asset"):
`deploy_trust` sources verify the token's signature and standard claims
against public material, then a claim allowlist. Auth happens **before**
existence is revealed: an unknown app name is verified against the
*global* trust sources so callers cannot enumerate app names
([handler.go:81-103](liveswap/handler.go)). Config load refuses an app
that resolves to zero trust sources ([liveswap.go](liveswap/liveswap.go)).
The `X-Liveswap-Secret` custom header is retired — Bearer only, which
Caddy redacts from access logs.

Properties that matter to the model:

- **GET and POST both sit behind the secret.** GET returns full status,
  POST deploys, all else 405 ([handler.go:105-113](liveswap/handler.go)).
  The status endpoint is authenticated — not public.
- **No rate limiting anywhere on the auth path.** No throttle, lockout,
  or backoff. Token *forgery* is infeasible (no private key), so this is
  not a guessing oracle — but each failure logs at Warn with `app`+`remote`
  ([handler.go:95-96](liveswap/handler.go)), an unauthenticated
  log-amplification / disk-fill primitive, and every attempt costs a
  JWT/JWKS verification. Body is
  capped at 64 KiB → 413 ([handler.go:42,121-128](liveswap/handler.go));
  `deployMu.TryLock()` → 409 serializes deploys
  ([app.go:258-260](liveswap/app.go)) but does nothing for auth attempts.
- **Path routing is `path.Base(path.Clean(...))`**
  ([handler.go:82](liveswap/handler.go)): `/anything/deep/myapp` targets
  `myapp`. A naive `path /deploy/*` site matcher does not constrain the
  app name; the operator's matcher is the only constraint.
- **Payload:** three fields only — `url`, `version`, `auth_header`
  ([app.go:94-98](liveswap/app.go)); unknown JSON silently ignored (no
  `DisallowUnknownFields`). `version` is `^[A-Za-z0-9._-]{1,64}$`,
  not `.`/`..`, double-sanitized before touching the filesystem
  ([liveswap.go:50,63-65,72-74](liveswap/liveswap.go)). `auth_header`
  is only control-char-checked ([handler.go:141-143](liveswap/handler.go));
  its contents are attacker-chosen and forwarded to the allowlisted host.
- **Response leaks (all post-auth):** the 500 path returns raw
  `err.Error()` plus the full status snapshot
  ([handler.go:159-162](liveswap/handler.go)) — filesystem paths, tar
  entry names, the operator's allowlist echoed verbatim
  ([allowlist.go:279-281,363,379](liveswap/allowlist.go)). The status
  snapshot exposes the app's **port and PID** and watchdog cause/failure
  state ([app.go:519-543](liveswap/app.go),
  [watchdog.go:234-254](liveswap/watchdog.go)). Artifact URLs *are*
  redacted before logs/errors ([download.go:158-163](liveswap/download.go)),
  so query signatures do not leak — handled well.
- **Replay / downgrade — now bounded.** The bearer JWT is short-lived
  (`exp`), so a captured request is replayable only within that window,
  not indefinitely. And versions are immutable: re-deploying an existing
  version (running or not) is refused 422, so replaying an *older*
  deploy's payload no longer downgrades the app. A token-holder can
  still downgrade deliberately via `?rollback=<version>` — but that is an
  explicit, audited operation (`deployed_by` + a `source:rollback` log),
  not a silent replay side-effect.
- **Header-in-logs footgun — fixed.** The custom `X-Liveswap-Secret`
  header (logged in plaintext, unlike the redacted `Authorization`) has
  been removed; Bearer is the only transport. (Deploy tokens are also
  short-lived, so an access-log leak is far less valuable than a leaked
  long-lived secret would have been.)

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
closed-by-default. This is a genuinely strong design.

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
  Flagged for the sandbox review, not asserted as exploitable.
- **No entry-count or path-length cap** independent of the byte budget:
  a billion 1-byte entries fits in 1 GB → CPU/inode exhaustion. The
  archive is read **twice**, so the byte cap permits 2× the work.
- `+x` survives extraction — intended (the artifact ships the app
  binary), but it is the point where artifact bytes become code.

### Reverse proxy, admin socket, penaltybox

The starter config exposes only `:80` returning a static string; the
entire liveswap/webhook block ships commented out
([packaging/Caddyfile:23-65](packaging/Caddyfile)) — a fresh install
has no deploy endpoint. Admin is off TCP, on
`unix//run/hotserve/admin.sock` ([packaging/Caddyfile:18](packaging/Caddyfile)),
`RuntimeDirectoryMode=0750` owned by the service user
([packaging/hotserve.service:26-29](packaging/hotserve.service)) — so
admin access is gated on *being the `hotserve` user*, which every
deployed app already is (stated honestly at
[packaging/Caddyfile:13-15](packaging/Caddyfile)). The recommended
webhook deployment ([Caddyfile:63-65](packaging/Caddyfile)) is a public
TLS vhost with only the shared secret and no rate limiting in front.

**penaltybox** is a response-phase rate-limit-hint enforcer; it touches
untrusted input only narrowly (the client key defaults to
`{http.vars.client_ip}`, deferring XFF trust to `trusted_proxies`;
[penaltybox/penaltybox.go:34-37,109-114](penaltybox/penaltybox.go)). A
misconfigured `trusted_proxies` turns client bytes into store keys,
bounded by `max_keys` (default 100 000, idle-eviction). The origin's
hint header is strict-parsed and fails open. It is a minor surface:
denial-of-protection under bad config, not injection or exhaustion.

### Supervisor⇄app and app⇄app boundaries — **built (2026-08-31, #35)**

> **Status:** this section described the state before per-app
> sandboxing shipped, when a transient unit gave a cgroup and nothing
> else. It is kept for the reasoning that follows; what it says is
> *missing* is now in place. Every app unit runs in its own user
> namespace (and, on systemd ≥ 256, its own PID namespace) with a
> deny-by-default filesystem view — see "The shared-UID rule" below
> and the Recommendation at the end. The network namespace is still
> shared, by design.

Every app runs as `hotserve`. Before #35 that meant the host's mount,
PID and network namespaces ([runner_systemd.go](liveswap/runner_systemd.go)
— a transient unit under the user manager gives a cgroup, not a
namespace, and unlike the exec runner's children the apps no longer
sit inside hotserve.service's `PrivateTmp`/`ProtectSystem`). The unit
environment is the user manager's defaults (`XDG_RUNTIME_DIR`,
`INVOCATION_ID`, …) plus an allowlisted slice of hotserve's (`PATH,
HOME, LANG, TZ, LC_*`, [app.go](liveswap/app.go) `inheritedEnv`) —
closing *direct* inheritance of
ACME tokens (and any other supervisor secrets); the `/proc` route is
closed by the non-dumpable supervisor ("The shared-UID rule" below);
the filesystem routes were what remained. This is the boundary the
whole evaluation existed to build, and #35 phase 1 builds it.

### Install-time — `packaging/postinstall.sh`

Runs as root at install; creates the `hotserve` user, chowns
`/var/lib/{hotserve,liveswap}`. Supply-chain attacks on hotserve's own
build land here, but they run in CI, not from a deployed app — out of
scope for the runtime model, in scope for release signing (roadmap).

## Attacker profiles (ranked by likelihood)

- **T1 — Poisoned dependency running *inside* a deployed app.** The
  primary threat, per the npm 2025 wave: a transitive dependency's
  runtime payload sweeps the filesystem for `.env`/tokens and
  exfiltrates. Already has code execution as `hotserve`. This is the
  attacker per-app isolation must stop.
- **T2 — Stolen deploy token / compromised CI identity.** With the
  symmetric secret retired, this is now a short-lived, claim-scoped
  token (or control of the CI identity that mints one). Can still
  deploy arbitrary code and downgrade versions within
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
  reach root?) — which is where the polkit trap below lives.

## Enumerated attack paths (today, no isolation)

For **T1**, the reachable-today set — the boundary matrix:

| Path | Reachable today | Mechanism |
|---|---|---|
| `/proc/<supervisor>/environ` → ACME tokens | ✘ closed | same UID, but hotserve is non-dumpable (see "The shared-UID rule") |
| connect admin socket → reconfigure server | ✔ | dir `0750`, same UID |
| read TLS private keys | ✔ | same UID, `/var/lib/hotserve` |
| read/write sibling releases, `shared`, `state.json` | ✔ | same UID |
| read sibling `env_file` (`/etc/hotserve/*.env`) | ✔ | operator-set mode, same UID |
| talk to sibling `127.0.0.1:$PORT` directly | ✔ | shared netns |
| signal / `/proc`-inspect sibling processes | ✔ | shared PID ns |
| exec setuid binary (`sudo`/`pkexec` class) | ✔ | no `no_new_privs` |
| fork-bomb / memory exhaust the box | ✔ | no cgroup/rlimit |
| network exfiltration | ✔ | shared netns, no egress policy |

For **T2**: deploy-arbitrary-code (contained only by the allowlist);
version-downgrade. For **T3**: archive-borne CPU/inode exhaustion; the
link-TOCTOU shape (unproven); first-hop→any-https SSRF. For **T4**:
log-amplification disk-fill (online *token forgery* is infeasible —
see below). For **T5**: total, by definition — the containment
question is root-vs-not-root.

## The shared-UID rule (normative for every approach)

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
(An earlier version of this section said "regardless of user
namespace"; the 2026-08-30 spike on #35 measured otherwise — bare:
`/proc/<manager>/root` open; `PrivateUsers=yes` alone: denied.) Three
consequences, and they decide the mechanism:

1. **No mount sandbox without a namespace the ptrace check honours.**
   A sandboxed app that can `ptrace`-read any same-UID PID outside its
   sandbox opens `/proc/<that-pid>/root/…` and walks the host
   filesystem. The user manager (host root, same UID, always running)
   is a permanent such target; every sibling app is another.
   `ProtectSystem=strict`, `InaccessiblePaths=`, a bubblewrap
   `--ro-bind` view are all void without one. `ProtectProc=invisible`
   does not substitute: `hidepid` hides other *users'* processes, and
   there are no other users here. Two namespaces do, and they close
   different things:
   - a **user namespace** (`PrivateUsers=yes`) closes the `/proc`
     reads — `root`, `environ`, `cwd`, `mem`, `fd` — of every process
     outside it, so the mount restrictions hold. Signals still
     deliver: `kill` checks uids, not namespaces. Available on every
     user manager in the support matrix (explicit on 252; implied by
     the mount options from 253).
   - a **PID namespace** (`PrivatePIDs=yes`, systemd ≥ 256) makes
     those processes invisible and unsignalable, and gives the unit
     an in-namespace init. Debian 13 and Ubuntu 26.04.

   **Shipped (#35 phase 1, 2026-08-30):** two tiers, probe-gated at
   Start. *filesystem* — user namespace plus the mount, device,
   cgroup, seccomp and capability set — on every cell; *full* —
   filesystem plus the PID namespace — where the manager is ≥ 256.
   Below 256 the residual is DoS-class (a compromised app can
   enumerate and `kill` same-UID processes; hotserve.service and the
   watchdog restart what it kills), not data access, and is warned
   about at every launch; `sandbox require` accepts only *full*.
   Bubblewrap is not carried as a second mechanism. Ubuntu 22.04 is
   dropped from the matrix. Ubuntu 24.04+ restricts unprivileged user
   namespaces to unconfined processes (measured on CI's Ubuntu-kernel
   runner: probe exit 226, tier none); the package ships an AppArmor
   profile granting `userns` to hotserve's user manager alone,
   attached by path to the wrapper `user@<uid>.service` is started
   through (`AppArmorProfile=` cannot be used: for an unprivileged unit
   newer AppArmor converts it into a stack with `unconfined`, which
   stays restricted — seen on Ubuntu 26.04). Residual: that
   permission is inherited by the manager's children, so an app run
   with `sandbox off` on such a host may create user namespaces where
   the distro default would refuse — sandboxed apps cannot
   (`RestrictNamespaces=`).

   **Superseded 2026-09-01 (matrix narrowed to Debian 13):** the
   second tier and the AppArmor profile are gone. There is one tier,
   *full*, and one candidate in the probe; a host that cannot deliver
   both namespaces gets `none` with a WARN, and `sandbox require`
   refuses to start. The paragraph above is kept because the
   measurements behind it — that a user namespace alone closes the
   `/proc` routes, that AppArmor path-attachment is the only way to
   grant `userns` to one manager — are what the current design rests
   on, and because they are the evidence for readmitting Ubuntu should
   that ever be wanted. Consequence accepted: a Debian 12 host that
   upgrades hotserve drops from *filesystem* to `none` rather than
   degrading gracefully. That is what dropping support means, and it
   is loud (WARN at every launch, `"sandbox":"none"` in status) rather
   than silent.
2. **A non-dumpable supervisor is the floor on every host.**
   `prctl(PR_SET_DUMPABLE, 0)` makes hotserve's `/proc` entries require
   `CAP_SYS_PTRACE`, which apps under `NoNewPrivileges` never hold —
   with or without user namespaces. **Shipped:** `liveswap/harden`, a
   leaf package whose `init` runs right after `syscall`'s — before
   `os`, `fmt`, Caddy and every package depending on them; only
   `syscall`-closure-only leaves that sort earlier can precede it, and
   those cannot touch `/proc` (measured with `GODEBUG=inittrace=1`:
   the 17th of 460 initializers on hotserve, two such leaves ahead of
   it; `TestInitRunsBeforeOS` pins `syscall < harden < os` in any
   binary that runs it) — so any binary
   importing liveswap (hotserve or an xcaddy build) is non-dumpable
   before `main`; a failure is fatal. Pinned by a unit test and by the
   real-systemd e2e suite (scenario 12). It closes the
   `/proc/<supervisor>/environ` and `/proc/<supervisor>/root` routes
   only; TLS keys on disk, the admin socket and sibling files still
   need the mount namespace.
   *Residual:* app units outlive supervisor restarts, so a same-UID
   app already running can race the interval between `execve` and
   that `init` — the Go runtime's start-up plus `syscall`'s own
   dependencies, well under a millisecond — and read the new
   supervisor's environment. On the support matrix that is a *read*
   race: Yama's default `kernel.yama.ptrace_scope=1` forbids a
   non-descendant from `PTRACE_ATTACH`/`PTRACE_SEIZE` at any time
   (Yama gates `PTRACE_MODE_ATTACH`, which the dumpable flag does not
   govern), so an app cannot seize the supervisor in the window; on a
   host set to `ptrace_scope=0` an attach made in the window would
   survive `PR_SET_DUMPABLE=0` and amount to persistent supervisor
   compromise — such hosts are outside this model. Only the kernel
   closes the window, and only from the app's side: an app unit in
   its own user namespace cannot read the supervisor's `/proc` at all
   (the cross-namespace refusal above is not gated on the dumpable
   flag), and one in its own PID namespace cannot see the
   supervisor's PID — a namespace on `hotserve.service` would not
   help, because a parent PID namespace sees its children's processes.
   With #35 phase 1 the window is closed for every sandboxed app on
   every cell of the matrix (the *filesystem* tier suffices); it
   stands only for apps running with `sandbox off` or on a host where
   the probe found no usable user namespace — accepted and stated
   here rather than in the README's one-line claim.
*Residual the sandbox cannot close by itself:* every path a unit binds
is checked by name, and between that check and the manager following
it, any process sharing the hotserve UID can swap what it points at.
During the documented bare-to-sandbox rollout the old bare instance is
still running — a deploy stops it only once the new one is healthy —
so that process can be the very app being sandboxed. hotserve resolves
and re-checks each bind source at unit creation, the last moment
before the manager acts, which closes the planted-symlink case and
leaves only this race; no pathname check can close the race itself
while the supervisor and its apps are the same principal. A sandboxed
app cannot reach the mount points at all, so the exposure is bounded
to apps already running unsandboxed on a box this model treats as one
trust domain. Per-app UIDs are what closes it.

*A unit's view is a policy, not a snapshot.* **Closed 2026-08-31
(#35).** This row used to record the opposite. systemd builds a unit's
mount namespace at start and hotserve never rebuilds it under a
running app (reloads leave instances alone by design), so while the
view was *the host, minus a set of paths derived from the running
configuration*, that set aged: an `env_file` belonging to an app added
later was merely read-only inside older siblings' sandboxes rather
than absent, and a path created after a unit started was not masked in
it at all. The sibling-secret row therefore held only for instances
launched after the secret was declared.

The view is now deny-by-default — `TemporaryFileSystem=/:ro` plus an
explicit base view, the app's own two directories, and its declared
its own directories — so nothing is derived and nothing ages. A secret
declared tomorrow is absent from a unit started yesterday for exactly
the same reason every other path is: nothing ever bound it. What is
still fixed at a unit's start is the tier and the set of paths an
operator asked to be let IN, both of which fail safe and both of which
a redeploy refreshes. Measured on all four cells of the support matrix
(systemd 252/255/257/259): inside a unit, `/etc` holds only the named
base-view entries this host actually has — a dozen or so — and
`/var/lib` holds the liveswap root alone.

*What a bare app leaves behind, the sandbox keeps.* The sandbox
restricts **reachability**; it cannot un-copy. `shared/` is the one
directory that survives every deploy and is bound read-write into the
new sandbox, so anything a bare instance put there — `sandbox off`, a
host at the `none` tier, or simply any app before its first sandboxed
deploy, which is the documented rollout — is inside the sandboxed
app's view afterwards, legitimately, with every check passing because
the bind resolves to exactly the directory it names. As the shared uid
a bare app can `cp` the supervisor's TLS keys, a sibling's release or
`shared` contents, or any readable `env_file` into its own `shared/`.
Worse, it can `link(2)` them: the shared uid owns those files, so the
hardlink is a live view of the inode, and an operator who later
rotates a secret **in place** publishes the new value into the sandbox
too — only replace-by-rename breaks the link. The operational
consequence, stated in liveswap/README.md's rollout section: sandboxing
an app that may have been compromised while bare is not containment;
clear its `shared/` and rotate anything it could read first.

*The recorded tier is app-writable.* A relaunch reproduces the tier
from `state.json`, which lives under the shared uid and outside every
sandboxed view — but a *bare* app can write any app's `state.json` and
pin it to `none` across every supervisor restart, boot recovery and
watchdog relaunch, with the status endpoint then honestly reporting
`none`. It cannot redirect `CurrentVersion` out of the app
(`versionPathComponent`, the `os.Stat` on the release dir, and
`unitBelongsTo` each refuse). The signal is the WARN every such launch
emits; the fix is per-app UIDs.

*The capability probe runs under the shared uid.* It starts a real
transient unit for up to 30s per start, so any process holding that uid
can interfere with it: under `auto` a failed probe degrades every app
to `none` with a WARN, under `require` it fails the whole server start.
The tier is therefore not solely a property of the host.

3. **Resource caps need a read-only cgroupfs inside the sandbox.** The
   cgroup subtree under `user@<uid>.service` is delegated to — owned
   by — the hotserve UID, so `MemoryMax=`/`TasksMax=` on a user-manager
   unit is a limit the app can rewrite in `/sys/fs/cgroup` (or escape
   by migrating its PIDs) unless that tree is read-only in its view.
   Runtimes only ever *read* it.

## Reducing the asset (deploy auth) — shipped

The two mitigation axes are orthogonal: *isolate the runtime* (the
three approaches below) and *shrink the prize*. This axis is done.

The former `LIVESWAP_SECRET` was bad on two counts: it was **symmetric**
(the verifier stored the same value it checked, so the prize physically
sat in the supervisor's env, reachable via `/proc`), and it was a
**long-lived bearer** (theft was permanent, replay unbounded). The fix
is asymmetric verification — the box holds only public material:

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

- **Asset #1 is gone from the box.** `/proc/<supervisor>/environ` no
  longer yields a deploy credential; the top asset is now ACME tokens.
- **T2 shrinks:** a stolen token is short-lived and claim-scoped, not a
  permanent deploy-anything key. The compromise surface moves from
  "a secret on the box" to "the CI identity that mints tokens."
- **T4's online guessing disappears:** forging a token needs the
  issuer's or operator's private key; there is nothing to brute-force.
  (Rate limiting is still owed for the log-amplification vector.)
- **Replay/downgrade is bounded** by the token's `exp` (minutes),
  where the old bearer secret was replayable indefinitely.

What it does **not** do: ACME tokens and TLS keys still live on the box
(the `/proc` and filesystem routes remain), so the isolation approaches
below are still required. Auth redesign shrinks the prize; it does not
isolate the runtime.

## The three candidate approaches

All three sit behind the existing `runner` interface
([liveswap/runner.go:12](liveswap/runner.go)), whose comment already
anticipates a systemd-backed implementation slotting in without touching
the state machine. The isolation dimension is added to `startSpec` as a
**backend-agnostic** field (paths, uid, namespace flags), not as a bag
of one backend's flags — so the approaches are swappable, not welded in.

### A — "UID + hardening" (no user namespaces required)

- `no_new_privs` on every spawned child (via a `setpriv` wrapper, or
  free with the ambient-cap plumbing below) — kills the entire
  setuid-binary escalation row.
- Static per-app UIDs (`hotserve-app-<name>`), created at provision.
  **Trap (test-pinned invariant):** the supervisor needs
  `CAP_SETUID`/`CAP_SETGID`, and ambient capabilities survive
  `setuid()`+`execve()` — so the ambient set MUST be cleared before
  exec, or every app inherits `CAP_SETUID` and can `setuid(0)`. This is
  security-critical hand-authored code and must carry a test in the
  spirit of `TestBuildEnvDoesNotLeakSupervisorSecrets`.
- Group-based release access: extraction stays `hotserve`, apps read via
  a shared group with `0750` release dirs (no `CAP_CHOWN` needed;
  `extract.go` already strips setuid/setgid so the tree is safe to share
  read-only).
- `RLIMIT_NPROC` (per-UID) → fork-bomb bound even before cgroups.
- A **shipped static AppArmor profile** (Debian/Ubuntu ship AppArmor
  enabled) applied via `aa_change_onexec`, denying `@{PROC}/*/environ`,
  `/etc/hotserve/**`, `/run/hotserve/**`, `/var/lib/hotserve/**`.
  Works **without** user namespaces — including on LXC VPSes.

Closes the supervisor-secret, admin-socket, TLS-key, and setuid rows
**unconditionally** (the only approach that does so on userns-denied
hosts). Does **not** isolate sibling files by path (UID + group + one
static profile is supervisor-vs-app, not per-app), sibling ports, or
sibling PIDs beyond what per-UID `ProtectProc`-style hiding gives.

### B — "Canonical exec-runner stack" (A + namespaces + cgroups)

A, plus:

- **bubblewrap** per [DESIGN-sandbox.md](liveswap/DESIGN-sandbox.md):
  mount + PID namespaces, allowlist filesystem view at real host paths,
  `--die-with-parent`, `--new-session`, the `Setpgid`-vs-`setsid` EPERM
  conflict resolved as that doc specifies. Hides `/proc/<supervisor>`,
  the admin socket, sibling files and PIDs by **absence**.
- **`Delegate=yes` cgroup subtree** on `hotserve.service`: the
  supervisor writes `memory.max`/`pids.max`/`cpu.max` into per-app
  sub-cgroups itself — unprivileged within its delegated subtree, on
  every matrix cell (cgroup v2 everywhere ≥ 22.04). It must move itself
  into a leaf sub-cgroup first (no-processes-in-inner-nodes rule).
- AppArmor (from A) doubles as the **no-userns fallback rung**: where
  the userns probe fails, `auto` degrades to bare+AppArmor+UID with a
  WARN, exactly the ladder DESIGN-sandbox.md specifies.

Closes every T1 filesystem/proc/PID row and adds resource limits.
Remaining open: **sibling `127.0.0.1:$PORT`** (netns is shared by
design) and **network egress** — both explicit non-goals, the port gap
addressable later by a unix-socket upstream contract.

### C — "systemd template-unit runner"

> **Status (2026-08-29): what shipped is neither B nor C.** liveswap
> now runs every app as a transient unit on the **hotserve user's own
> manager** (`user@<uid>.service`, private socket, no polkit, no
> grant of any kind). The escalation trap this section warns about is
> real and was the reason for that choice: it exists only when the
> supervisor holds a `manage-units` grant on the *system* manager,
> and hotserve holds none — it can create units only as itself, under
> `NoNewPrivileges`, so a supervisor RCE gains nothing it did not
> already have (T5 unchanged: `◐`). What C alone can buy — per-app
> `User=`, `IPAddressDeny=` egress filtering — remains future work
> and, if wanted, must arrive as a root-owned template or a minimal
> privileged helper, never as a system-manager transient grant.
> Restart-survival (`Reattach`) shipped with the runner. The
> "unprivileged compose" testability row below is stale: `make e2e`
> now runs hotserve under real systemd in a privileged container.
> Isolation is the next milestone, and "The shared-UID rule" above
> settles its mechanism: systemd's own per-unit sandboxing on the
> user manager, which holds only with `PrivatePIDs=` (systemd ≥ 256)
> — so it is probe-gated: full on Debian 13 / Ubuntu 26.04, floor-only
> with a WARN on Debian 12 / Ubuntu 24.04. The property set is C's,
> minus `User=` and the polkit/template machinery:
> `PrivatePIDs=`, `ProtectSystem=strict` + `ReadWritePaths=`,
> `PrivateTmp=`, `InaccessiblePaths=`, `ProtectControlGroups=` (so
> caps are real), `SystemCallFilter=@system-service`, resource caps.
> No bubblewrap. Still to prove on both cells, inside the *user*
> manager: that the set applies to a transient unit, and that
> Ubuntu's AppArmor user-namespace restriction does not block
> `systemd --user` (bwrap had a dedicated profile; systemd does not).

A packaged `hotserve-app@.service` template with **root-owned**
properties: per-app `User=`, `ProtectSystem=strict`,
`ProtectProc=invisible`, `PrivateTmp`, `SystemCallFilter=@system-service`
(a curated, adopt-wholesale seccomp filter — the bar DESIGN-sandbox.md
sets), `IPAddressDeny=`/`SocketBindDeny=` (**egress filtering — C-only**),
and `PrivatePIDs=yes` which **engages on systemd ≥ 256 and is silently
ignored below** (unit-file unknown-directive degradation; matrix:
22.04=249, D12=252, 24.04=255, D13=257, 26.04≥257). The supervisor is
restricted to `start`/`stop` of that template — polkit **can** match
unit *names*, and because nothing is transient the properties are not
attacker-suppliable.

Why the *template*, not `systemd-run` transient units: polkit is blind
to unit *properties*, so a `StartTransientUnit` grant that the
supervisor can shape is effectively root-equivalent (a unit can name
`User=root`, any `ExecStart=`) — the escalation trap that makes a naive
systemd runner *worse* than today for T5. The template moves that policy
into root-owned files. Per-deploy variation (port/env) flows via
drop-ins or `EnvironmentFile`, not via attacker-influenced properties.

Restart-survival (`Reattach` across a supervisor restart,
`handleState.Unit` already reserved in
[liveswap/runner.go:56-59](liveswap/runner.go)) is **explicitly a
separate later milestone** — it is the riskiest state-machine work and
is not required for the isolation win.

Note: C breaks the fast dev/test loop — `make e2e` is unprivileged
compose with no systemd — so it needs the install-test lane (already
real systemd, [Makefile:112-127](Makefile)) as its proving ground. This
supersedes DESIGN-liveswap.md's stale "needs a Linux-VM test lane
outside compose" note.

## Scoring against the threat model

`●` closed · `◐` partial · `○` open · `—` n/a. "userns-denied host" =
LXC VPS / locked-down kernel.

"Shipped" is #35 phase 1 (2026-08-30): the systemd-native tiers on
the user-manager runner; *full* on systemd ≥ 256, *filesystem* below.

| Attack path (attacker) | Before #35 | Shipped | A | B | C |
|---|---|---|---|---|---|
| `/proc/<sup>/environ` (ACME tokens) (T1) | ●¹¹ | ● | ● | ● | ● |
| admin socket connect (T1) | ○ | ● | ● | ● | ● |
| TLS private key read (T1) | ○ | ● | ● | ● | ● |
| sibling file read/write (T1) | ○ | ● | ◐¹ | ● | ● |
| sibling `127.0.0.1:$PORT` (T1) | ○ | ○² | ○ | ○² | ◐³ |
| sibling PID signal/inspect (T1) | ○ | ●/◐¹² | ◐⁴ | ● | ●⁵ |
| setuid-binary escalation (T1) | ○ | ● | ● | ● | ● |
| fork-bomb / mem exhaust (T1) | ○ | ○¹³ | ◐⁶ | ● | ● |
| network exfiltration (T1) | ○ | ○ | ○ | ○ | ●⁷ |
| deploy arbitrary code (T2) | ○⁸ | ○⁸ | ○⁸ | ○⁸ | ○⁸ |
| version downgrade (T2) | ○ | ○ | ○ | ○ | ○ |
| archive CPU/inode exhaust (T3) | ○ | ○ | ○ | ◐⁹ | ◐⁹ |
| first-hop→any-https SSRF (T3) | ◐ | ◐ | ◐ | ◐ | ●⁷ |
| webhook log-amplification (T4) | ○ | ○ | ○ | ○ | ○ |
| **supervisor RCE → root? (T5)** | ◐ | ◐ | ◐ | ◐ | ◐¹⁰ |

1. Supervisor-vs-app via one static profile + group perms; not per-app
   path isolation. 2. Netns shared by design; future unix-socket
   upstream contract closes it. 3. `SocketBindDeny=`/`RestrictAddress`
   can fence localhost binds, partially. 4. Per-UID `ProtectProc`-style
   hiding only. 5. Real per-unit PID isolation only on systemd ≥ 256
   (`PrivatePIDs=`); below that, `ProtectProc=invisible` + per-UID.
   6. `RLIMIT_NPROC` only, no memory cap. 7. Egress/IP filtering is
   C-only. 8. Contained by `artifact_allowlist`, not the runtime —
   orthogonal to isolation. 9. `Delegate` cgroup `pids.max` bounds inode
   pressure indirectly; the missing entry-count cap in `extract.go` is
   the real fix and is isolation-independent. 10. C is the only approach
   that can make T5 *worse* (polkit root-escalation) if built as
   transient units — the template design avoids it, hence `◐` not `○`,
   but it is the row that demands the most care. 11. Closed by the
   non-dumpable supervisor (shared-UID rule, item 2) independently of
   any approach; the "Before #35" column otherwise predates isolation.
   12. `●` on the *full* tier (systemd ≥ 256, PID namespace); `◐` on
   the *filesystem* tier — `/proc` inspection is closed by the user
   namespace, signals are not (DoS class, warned at every launch).
   13. Deferred to #35 phase 2: `ProtectControlGroups=` already makes
   the cgroup tree read-only inside the unit, so `MemoryMax=`/
   `TasksMax=`/`CPUQuota=` will be real when they land.

Cost / lock-in rows:

| | A | B | C |
|---|---|---|---|
| Works on userns-denied host | ● | ◐ (degrades to A) | ● |
| New privileged code authored | ambient-cap clearing | + bwrap argv | polkit + template policy |
| New runtime dependency | none | `bubblewrap` | none |
| Testable in `make e2e` (unprivileged compose) | ● | ● | ○ (install-test only) |
| Reversible (swap behind `runner`) | ● | ● | ◐ (egress/restart features are one-way) |

## Residual risks common to all three

- **An app's own secrets and its own database are reachable by
  definition** — not a bug, stated so operators do not expect otherwise.
- **Install-time supply chain** (hotserve's own build) is a CI concern;
  release signing is the roadmap answer, not a runtime control.
- **T5 is contained, not prevented** — a supervisor RCE holds every
  asset short of root under all three; only C changes the root question,
  and only if built as a locked template (never transient units).
- **Sibling localhost ports** stay reachable under A and B (shared
  netns). The clean fix is making the app→hotserve contract a unix
  socket in the app's own dir (Caddy `reverse_proxy` speaks `unix/`),
  which also enables a per-app netns later. Decide it explicitly rather
  than inheriting the gap.
- **`state.json` must stay outside any writable sandbox view**
  ([liveswap/state.go](liveswap/state.go) is trusted on relaunch; the
  recorded sandbox tier lives there too). Normative and shipped: the
  whole filesystem is replaced by an empty read-only tmpfs in the
  unit's view (`TemporaryFileSystem=/:ro`) and only the release being
  started, `shared/` and the OS base view are bound back — the app dir root, `state.json`, `tmp/` (the upload
  staging dir: a running instance must not be able to rewrite the next
  version's tarball) and the other releases do not exist inside, along
  with everything else on the host. `sandboxSpecFor` in
  liveswap/sandbox.go is the single place that list is built;
  `TestSandboxSpecFor` and `TestSandboxViewIsExactlyWhatIsNamed` pin
  it — the latter asserts the rendered set of bind destinations IS the
  view, so an accidental widening fails there.
- **Non-isolation hardening is still owed regardless of approach:**
  webhook rate limiting (T4 log-amplification) and the `extract.go`
  entry-count cap (T3). (The Bearer-only / no-shared-secret and
  short-lived-token items are now done — see "Reducing the asset".)

## Recommendation

**Decided (2026-08-29) and shipped (2026-08-30, #35 phase 1):
systemd's own per-unit sandboxing on the user-manager runner, in two
probe-gated tiers** — narrowed to one tier on 2026-09-01, see the
Amendment below. That is C's property set without C's privilege —
`PrivateUsers=` for the user namespace that closes cross-process
`/proc` reads (the shared-UID rule as corrected), `PrivatePIDs=` for
the PID namespace that closes visibility and signals where the manager
is ≥ 256, a deny-by-default filesystem view (`TemporaryFileSystem=/:ro`
plus `BindReadOnlyPaths=` for a named OS base view and `BindPaths=`
for the app's own two directories — amended 2026-08-31 from
`ProtectSystem=strict` plus a derived `InaccessiblePaths=` set, which
could be incomplete and went stale between deploys), `PrivateTmp=`,
`ProtectControlGroups=` so resource caps will be real, and the curated
`SystemCallFilter=@system-service` — issued as transient-unit
properties by a supervisor that holds no grant, so a supervisor RCE
still gains nothing (T5 unchanged). *full* on Debian 13 / Ubuntu
26.04; *filesystem* on Debian 12 / Ubuntu 24.04 with a WARN at every
launch naming the residual. Bubblewrap is dropped rather than carried
as a second mechanism; per-app UIDs, egress filtering and the
root-owned template stay later milestones, and if they land they MUST
be the template or a minimal privileged helper, never
supervisor-shaped transient units on the system manager.
DESIGN-sandbox.md's behaviour spec, config surface and rollout
semantics (engage on next deploy, record the tier in `state.json`,
`auto`/`require`/`off`) are what shipped; its bwrap mechanics did not.
Resource caps are #35 phase 2.

*Superseded (kept for the record):* the earlier recommendation was B
— A's items first (`no_new_privs`, per-app UIDs, group-based release
access, `RLIMIT_NPROC`, an AppArmor profile), then bubblewrap +
`Delegate` cgroups with AppArmor as the no-userns rung, deferring C
until egress filtering or restart-survival earned it. Restart-survival
arrived without C (the user-manager runner, #34), the non-dumpable
floor is shipped, and the shared-UID rule made bubblewrap's PID
namespace the load-bearing piece — which systemd now provides itself.

Independently of which approach lands, do the three non-isolation
hardening items above — they are cheaper than any of A/B/C and address
rows (T3, T4) that no isolation approach touches.


### Amendment (2026-09-01): one host, one tier

The support matrix is Debian 13 alone. Two things follow, and neither
changes the property set above:

- **One tier.** `PrivatePIDs=` exists on every supported manager
  (systemd 257), so *filesystem* is no longer probed for or offered.
  It survives only as a value `state.json` may already hold, so an
  instance recorded at that tier relaunches faithfully instead of
  being silently upgraded or silently dropped
  (`validSandboxTierRecord` in [liveswap/sandbox.go](liveswap/sandbox.go)).
- **No AppArmor profile.** Debian's kernel does not restrict
  unprivileged user namespaces, so the profile and the user-manager
  wrapper it attached to are removed along with the privilege they
  carried — the residual noted above (an app under `sandbox off`
  inheriting `userns` from the manager) is gone with them.

What deliberately does **not** change: the probe. It was never a proxy
for the systemd version — it is what catches a container, an LXC VPS
or a kernel built without user namespaces, all of which can present a
supported manager version and still refuse the unit. Deleting it would
turn a measurement into a claim.

The cost is CI fidelity: GitHub's runners boot an Ubuntu kernel, and
the profile was what let the Debian cells prove the sandbox under a
real userns restriction. With it gone, CI sets
`kernel.apparmor_restrict_unprivileged_userns=0` on the runner to make
it behave like a Debian host, and no lane exercises a restricted
kernel any more. The probe is the only thing standing between such a
host and a silent loss of isolation, which is the second reason it
stays.
