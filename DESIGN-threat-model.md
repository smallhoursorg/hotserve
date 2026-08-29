# DESIGN — threat model and process-isolation evaluation

Status: analysis. This document states what hotserve is defending, who
the attacker is, the concrete paths from an entry point to an asset,
and how three candidate isolation/hardening stacks score against those
paths. It is the decision record behind the "per-app sandboxing"
roadmap item; [DESIGN-sandbox.md](liveswap/DESIGN-sandbox.md) remains
authoritative for bubblewrap mechanics, and this document places that
work in the wider attack surface rather than restating it.

Scope: a single Debian/Ubuntu box running `hotserve` (a Caddy
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
   Now the highest-value item reachable via `/proc/<supervisor>/environ`.
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

### Supervisor⇄app and app⇄app boundaries — **currently none**

Every app runs as `hotserve`, in the host's mount, PID, and network
namespaces ([runner_systemd.go](liveswap/runner_systemd.go) — a
transient unit under the user manager gives a cgroup, not a
namespace, and unlike the exec runner's children the apps no longer
sit inside hotserve.service's `PrivateTmp`/`ProtectSystem`). The unit
environment is the user manager's defaults (`XDG_RUNTIME_DIR`,
`INVOCATION_ID`, …) plus an allowlisted slice of hotserve's (`PATH,
HOME, LANG, TZ, LC_*`, [app.go](liveswap/app.go) `inheritedEnv`) —
closing *direct* inheritance of
ACME tokens (and any other supervisor secrets), but not the `/proc` or filesystem routes
to the same values. This is the boundary the whole evaluation exists to
build.

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
| `/proc/<supervisor>/environ` → ACME tokens | ✔ | same UID |
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
> Isolation (A/B or systemd's per-unit sandboxing on the user
> manager, probe-gated on unprivileged userns) is the next milestone.

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

| Attack path (attacker) | Today | A | B | C |
|---|---|---|---|---|
| `/proc/<sup>/environ` (ACME tokens) (T1) | ○ | ● | ● | ● |
| admin socket connect (T1) | ○ | ● | ● | ● |
| TLS private key read (T1) | ○ | ● | ● | ● |
| sibling file read/write (T1) | ○ | ◐¹ | ● | ● |
| sibling `127.0.0.1:$PORT` (T1) | ○ | ○ | ○² | ◐³ |
| sibling PID signal/inspect (T1) | ○ | ◐⁴ | ● | ●⁵ |
| setuid-binary escalation (T1) | ○ | ● | ● | ● |
| fork-bomb / mem exhaust (T1) | ○ | ◐⁶ | ● | ● |
| network exfiltration (T1) | ○ | ○ | ○ | ●⁷ |
| deploy arbitrary code (T2) | ○⁸ | ○⁸ | ○⁸ | ○⁸ |
| version downgrade (T2) | ○ | ○ | ○ | ○ |
| archive CPU/inode exhaust (T3) | ○ | ○ | ◐⁹ | ◐⁹ |
| first-hop→any-https SSRF (T3) | ◐ | ◐ | ◐ | ●⁷ |
| webhook log-amplification (T4) | ○ | ○ | ○ | ○ |
| **supervisor RCE → root? (T5)** | ◐ | ◐ | ◐ | ◐¹⁰ |

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
   but it is the row that demands the most care.

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
  ([liveswap/state.go:16-20](liveswap/state.go) is trusted on relaunch;
  the sandbox-disposition field M8 adds lives there too). Today only
  `releases/`+`shared/` are bound, so it falls outside — but that is an
  accident of the path list, not a stated invariant. Make it normative:
  the app dir *root* MUST NOT be bound writable.
- **Non-isolation hardening is still owed regardless of approach:**
  webhook rate limiting (T4 log-amplification) and the `extract.go`
  entry-count cap (T3). (The Bearer-only / no-shared-secret and
  short-lived-token items are now done — see "Reducing the asset".)

## Recommendation

Ship **B**, phased so each piece stands alone:

1. **A's items first, independently** — `no_new_privs`, per-app UIDs
   (with the ambient-cap clearing test), group-based release access,
   `RLIMIT_NPROC`, the AppArmor profile. This closes the highest-value,
   unconditional rows (supervisor secrets, admin socket, TLS keys,
   setuid) and works on the cheap LXC VPSes the product targets.
2. **Then bubblewrap + `Delegate` cgroups** per DESIGN-sandbox.md, with
   AppArmor as the no-userns fallback rung, engaging on next deploy (the
   path with health-gate fallback) exactly as that doc's rollout section
   specifies. Its engage-on-next-deploy / record-disposition-in-state
   semantics carry over unchanged.

Defer **C** until **egress filtering** or **restart-survival**
specifically earn the polkit/template work — those are C's only
capabilities B cannot reach, and both are one-way doors. When C does
land, it MUST be the root-owned template, never supervisor-shaped
transient units.

Independently of which approach lands, do the three non-isolation
hardening items above — they are cheaper than any of A/B/C and address
rows (T3, T4) that no isolation approach touches.
