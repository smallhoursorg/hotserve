# DESIGN — liveswap

Status: v1 implemented. This document is the handover brief: why the
module exists, its normative behavior, the architecture decisions and
their traps, and what was deliberately left out.

## Why this module exists

The predecessor stack (see `reference/infra-nomad` in the development
repo) ran a single VPS with Nomad as orchestrator, a ~490-line Deno
webhook to download GitHub release tarballs and drive `nomad job run`,
and Caddy configured through a Nomad template that re-rendered
upstreams from the service catalog. It worked, but three systems and a
custom webhook existed to deliver one behavior: *start the new version,
health-check it, move traffic, stop the old one.*

liveswap folds that entire loop into the reverse proxy that was
already there. The webhook, the artifact fetch, the health gate, the
cutover and the process supervision are one Caddy module set. Target
user: indie hackers, solo devs and small businesses deploying from
GitHub/GitLab to one server. Guiding principle: **simple and intuitive,
but powerful and helpful.**

Concept map from the Nomad-era stack:

| Nomad-era piece | liveswap replacement |
|---|---|
| `update { canary = 1, auto_promote }` | deploy pipeline: start → gate → atomic cutover |
| `min_healthy_time = "30s"` | `soak` (default 15s) |
| `auto_revert = true` | failures before promote never move traffic |
| webhook downloads GH release, `nomad job run` | webhook payload carries the artifact URL |
| `nomadService` template + SIGUSR1 | `http.reverse_proxy.upstreams.liveswap` atomic port swap |
| versioned dirs + keep-6 GC | `releases/<version>/` + `keep` GC |
| prestart migration task | `pre_start` |

## Behavior specification (normative)

- The webhook MUST authenticate every request by verifying an
  `Authorization: Bearer <JWT>` against the app's `deploy_trust`
  sources (OIDC against an issuer's public JWKS, or a local public
  key), and MUST NOT reveal whether an app exists to unauthenticated
  callers (unknown apps verify against the global trust sources, then
  404). No shared secret is stored on the box.
- Config load MUST fail if any app resolves to zero `deploy_trust`
  sources.
- The deploy MUST be rejected (409) if one is already running for that
  app; other apps' deploys proceed independently.
- Artifact downloads MUST enforce `max_artifact_size` both via
  Content-Length and the streamed byte count, MUST default to https
  only (enforced on every redirect hop), and MUST match the required
  `artifact_allowlist` — host, optionally with a literal port (no
  wildcards; default-port-only otherwise), a path prefix (mandatory in
  spirit on multi-tenant hosts), and declared query parameter names
  (queries are refused unless every name is vouched for) — first hop
  only. The outgoing URL is constructed from the entry's own config
  bytes; the payload contributes only the canonical-form path suffix
  and vetted query.
- Extraction MUST reject: absolute paths, `..` traversal, symlink and
  hardlink targets resolving outside the archive root, special files
  (devices/FIFOs), setuid/setgid bits, and decompressed content beyond
  10× `max_artifact_size`. Validation is a full pre-pass; nothing is
  written unless every entry is clean. Extraction goes to a staging dir
  renamed into place on success.
- `pre_start` (if configured) MUST run to completion in the release dir
  before the new instance starts; non-zero exit aborts the deploy.
- The new instance MUST be continuously healthy for `soak` before any
  traffic moves, and MUST be stopped and the deploy failed if not
  healthy within `deadline`. A flapping health check resets the soak.
- Any failure before the promote step MUST leave the old version
  serving, untouched. The webhook response MUST be synchronous and MUST
  be a 5xx on failure (CI turns red; `curl --fail`).
- The cutover MUST NOT trigger a config reload; it is an atomic value
  swap read per-request by the upstream source.
- After cutover the pipeline MUST wait `drain`, then SIGTERM the old
  instance's unit — SIGTERM to its whole cgroup, then SIGKILL after
  `grace`.
- Release GC MUST run only after successful deploys, keep the newest
  `keep` dirs by mtime, and never delete the currently-serving version.
  Failed versions' dirs are kept (until GC'd by age) for debugging.
- Config reloads MUST NOT restart running apps. Changed app definitions
  apply on the next deploy.
- On Caddy start, each app with recorded state MUST be relaunched (or
  reattached, if the runner supports it) and published as soon as the
  process is up — the health gate is a deploy gate, not a boot gate.
- Deploy secrets, `auth_header` values, and artifact URL query strings
  MUST NOT be logged.

## Architecture

Three cooperating modules, flat package `liveswap`:

1. **`liveswap`** (`liveswap.go`) — a `caddy.App`, registered as a
   Caddyfile global option. Owns app definitions, defaults
   (Provision), invariants (Validate), boot recovery (Start), and pool
   reference lifecycle (Cleanup).
2. **`http.handlers.liveswap_webhook`** (`handler.go`) — resolves the
   app module via `ctx.App("liveswap")`; authenticates; `POST /<app>`
   deploys (synchronous), `GET /<app>` reports status. The app name is
   the last path segment so the directive works under any mount path.
3. **`http.reverse_proxy.upstreams.liveswap`** (`upstreams.go`) —
   implements `reverseproxy.UpstreamSource`. `GetUpstreams` reads an
   atomic port; returning `127.0.0.1:<port>`. This was chosen over (a)
   rewriting config via the admin API — a reload per deploy, config
   drift, races with operator edits — and (b) a bespoke proxy handler —
   reimplementing reverse_proxy badly.

### The reload trap (the one Caddy-specific thing you must not break)

Caddy provisions the NEW config before cleaning up the OLD one on every
reload, and module instances are always rebuilt. Anything owned by a
module instance dies on reload. Therefore all live state — the running
process handle, active port, deploy mutex, last-deploy record — lives
in `managedApp` objects stored in a package-level `caddy.UsagePool`
keyed by app name (`liveswap.go`). Provision takes a pool reference;
Cleanup releases it; the refcount never reaches zero across a reload,
so `Destruct` (which stops the child) only runs at real shutdown or
when an app is removed from the config. The integration suite pins this
with a reload-keeps-PID test; the e2e suite reloads via the admin API
mid-traffic.

Corollary: `App.Stop()` is deliberately a no-op — on reload the old
config's Stop runs *after* the new config serves, and stopping children
there would defeat the pool.

### Concurrency model

- `managedApp.specMu` guards the spec and collaborator wiring
  (re-wired each reload); deploys take a `snapshot()` once at start, so
  a mid-deploy reload never tears the config.
- `deployMu.TryLock` serializes deploys per app; 409 instead of queue.
- `mu` guards current instance/phase/last-deploy.
- `activePort` is the single atomic the proxy hot path reads.
- The runner's logger sits behind an atomic pointer because the
  per-unit watcher goroutines outlive the config that created them.

### Runner abstraction (`runner.go`)

```go
type runner interface {
	Start(spec startSpec) (handle, error)
	RunOnce(ctx context.Context, spec startSpec) error
	Alive(h handle) bool
	Wait(h handle) <-chan struct{}
	Stop(h handle, grace time.Duration) error
	Reattach(st handleState) (handle, bool)
}
```

The one implementation is `systemdRunner` (`runner_systemd.go`, D-Bus
client in `systemd_dbus.go`): every instance is a transient service
unit under the **hotserve user's own systemd manager**
(`user@<uid>.service`), reached over that manager's private socket
(`/run/user/<uid>/systemd/private` — no session bus, no polkit, no
root). v1 shipped an exec runner (apps as child processes in their own
process group); it was replaced, not fixed, once it needed a page of
init-system emulation for what cgroups do natively, and it took the
"apps restart with the binary" trade-off with it.

What the unit gives us: `KillMode=control-group` makes stop and crash
cleanup mean the whole process tree (npm → node → workers), the
journal captures stdout/stderr under `SyslogIdentifier=hotserve-<app>`,
`Restart=no` keeps the watchdog the sole restart authority, and
`NoNewPrivileges` is on. `handleState.Unit` (persisted in
`state.json`) is what `Reattach` looks up after a hotserve restart: a
running unit is adopted with its port from state, a failed one is
logged, reset and relaunched. On process exit `Destruct` leaves units
running for exactly that reason; only removing an app from the config
stops it.

The runner's invariants are written in its doc comment; the two that
matter most to the state machine are that a handle is declared dead
only on an *observed* terminal unit state (a D-Bus outage never
becomes a phantom crash and a duplicate instance), and that `Stop`
returns nil only once the unit is observed gone — any other outcome
is an error, on which the deploy pipeline keeps the release on disk
and skips GC.

Why the user manager and not the system one: a polkit rule granting
`org.freedesktop.systemd1.manage-units` cannot inspect unit
properties, so it would let a compromised hotserve start a root unit;
Ubuntu 22.04's polkit 0.105 cannot even filter by unit name. Per-app
UIDs — the one thing the system manager would buy — are a later
milestone behind a small privileged helper. Isolation between apps
and hotserve comes from per-unit sandboxing (probe-gated, needs
unprivileged user namespaces) or bubblewrap, see `DESIGN-sandbox.md`
and `DESIGN-threat-model.md`.

Packaging consequences: `libpam-systemd` (pam_systemd hands the user
manager its `XDG_RUNTIME_DIR`) and `dbus` are hard dependencies;
postinstall enables lingering and drops in `Wants=/After=user@<uid>`
on `hotserve.service`; preremove stops the user manager (and with it
every app) on real removal only. Provision fails loudly if the
manager is unreachable — there is deliberately no fallback runner.

### File-by-file

| File | Concern |
|---|---|
| `liveswap.go` | App module, config structs + defaults, Validate, pool wiring |
| `app.go` | `managedApp` state machine, Deploy pipeline, recovery, env building |
| `caddyfile.go` | all Caddyfile parsing (global option, directive, upstreams); NO defaults here — Provision owns them |
| `handler.go` | webhook auth, payload validation, status endpoint |
| `upstreams.go` | dynamic upstream source (the cutover read side) |
| `runner.go` / `runner_systemd.go` / `systemd_dbus.go` / `port.go` | runner interface + the systemd transient-unit implementation + its D-Bus client + port helpers |
| `download.go` | capped, redacted artifact download; `releaseFetcher` (download+extract orchestration) |
| `extract.go` | validate-then-extract hardened tar handling |
| `health.go` | prober with soak/deadline arithmetic on an injected clock |
| `watchdog.go` | continuous crash/health supervision: per-app loop, restart budget, backoff |
| `state.go` | `state.json` atomic persistence, keep-N GC |
| `deploytrust.go` | deploy-auth: OIDC + local-key JWT verification |
| `clock.go` | `Now()`/`Sleep()` clock seam |

## Security posture

- Keyless webhook auth; CI never gets SSH, and no shared secret lives
  on the box. Deploys carry a short-lived JWT verified against public
  material only — CI OIDC (issuer JWKS + a claim allowlist) or a local
  public key. Verification uses vetted libraries (`go-oidc`,
  `go-jose`), never hand-rolled JWT crypto.
- Tar hardening is a pure-Go port of the predecessor webhook's
  `validateArchive`, extended with a decompression-ratio cap and
  setuid stripping, and unit-tested against crafted malicious archives.
- Apps bind 127.0.0.1 only; `PORT`/`HOST` are injected.
- `Authorization` is dropped by Go's HTTP client on cross-host
  redirects — exactly right for GitHub's asset→S3 redirect.
- `artifact_allowlist` is required — there is no "deploy from
  anywhere" mode. Host+path entries pin the tenant on shared hosts
  (a bare `github.com` would admit anyone's artifacts), and the
  request host is rebuilt from the config's own string after the
  match, so the attacker-steerable part of a deploy URL is a path
  suffix under an operator-pinned origin.
- Nothing secret is logged: URL query strings are redacted, and the
  deploy token arrives via `Authorization: Bearer` (Caddy-redacted).

## Testing acceptance criteria (all implemented)

1. Unit (`make test`, race): state machine happy path; pre_start abort;
   health abort keeps old serving; 409; same-version rejection; GC;
   recovery (relaunch + reattach + no-op); env precedence and
   placeholders; malicious-tar table; download caps/allowlist/redaction;
   prober soak/flap/deadline/liveness; parser tables incl. the
   "empty block leaves defaults to Provision" rule; Validate table.
2. Integration (`make test-integration`, caddytest, real processes):
   webhook deploy v1 → proxied body; v2 cutover; broken v3 → 500 and v2
   still serving; **reload via admin API keeps the app PID**; keep-N
   after a subsequent successful deploy.
3. e2e (`make e2e`, compose, xcaddy-built binary): module-linked build
   guard; auth; 5xx-before-first-deploy; v1 deploy; **continuous curl
   loop through the v2 cutover with zero non-200s**; broken-v3
   containment under traffic; 409 mid-deploy; **admin-API reload under
   traffic with zero non-200s and unchanged app PID**; status endpoint.

## Watchdog (added after v1)

The continuous watchdog — originally the first v1 non-goal below —
shipped as a follow-up. Design summary (full operator docs in
README.md "Watchdog"):

- One goroutine per pooled `managedApp` (`watchdog.go`), started at
  first Provision, torn down in `Destruct` **before** the child is
  stopped — that ordering is the invariant that makes a mid-restart
  watchdog unable to orphan a freshly started process. Reloads never
  touch the goroutine; it re-snapshots the spec every cycle.
- Triggers: unit exit (the runner's per-unit watcher, exposed as
  `runner.Wait`; a nil channel degrades to `Alive` polling) or
  `watchdog_failures` consecutive failed probes. A health verdict on
  an instance that is dead by restart time is recorded as a crash —
  which signal noticed first is only probe interval vs. unit poll. `watchdog_grace` re-arms after every start; crashes count
  even during grace.
- Restarts take `deployMu` (TryLock, yielding to deploys) and re-check
  instance identity — promote swaps `current` before stopping the old
  handle, so a deploy-stopped handle never reads as a crash. The
  relaunch path is `launchVersion`, shared with boot recovery: it
  reproduces the recorded instance, never re-reads config-level launch
  policy.
- Pacing is fixed: 1s exponential backoff, 60s cap, ±20% jitter,
  reset only after sustained health. The budget
  (`watchdog_restarts`/`watchdog_window`, shared by crash and health
  triggers) is a rate limiter, not a give-up point — Nomad's
  `mode="delay"`, chosen over hard-fail so an app killed by a
  transient incident recovers unattended once the incident ends. A
  full window throttles (dead instances are unrouted during the wait)
  until the oldest restart slides out; a successful deploy resets the
  budget immediately. The rate bound is also the DoS bound on induced
  health failures. Alerting on a sustained restart loop is deferred
  to the events/metrics milestone.
- The watchdog is the **single restart authority**: the systemd
  runner creates every unit with `Restart=no`, or systemd and
  liveswap would fight over the same failure.

## Non-goals (v1)

- Post-promote *auto*-revert. Rollback is an explicit operation —
  `POST /<app>?rollback=<version>` relaunches an on-disk release. (The
  continuous health watchdog, originally part of this
  non-goal, has since shipped — see "Watchdog" above. It restarts the
  same version; it still never auto-reverts.)
- Multi-node or any cluster awareness.
- Resource limits — now one unit property away (`MemoryMax=`,
  `TasksMax=`, `CPUQuota=`; the user manager delegates those
  controllers); not yet exposed in config.
- Prometheus metrics (deploys_total, duration) — v1.1 candidate.
- Deploy queueing; 409 + CI retry is the queue.
- Windows.
- Streaming (NDJSON) deploy progress; the response is buffered JSON so
  `curl --fail` semantics stay honest.

## Open questions for v2 (with leans)

1. ~~**systemd runner**~~ — shipped, and as the only runner rather than
   an opt-in: see "Runner abstraction". The test lane it needed is a
   privileged systemd container, which Docker Desktop runs fine.
2. **Deploy events** — emit Caddy events (deploy_succeeded/_failed) so
   users can wire notifications. Lean: yes, cheap and composable.
3. **Admin endpoints** (`caddy liveswap status/rollback`) alongside the
   webhook. Lean: later; the webhook + status JSON covers CI and cron.
4. **Metrics.** Lean: add with events.
5. **Config-change restarts** — an explicit "apply now" admin action
   instead of waiting for the next deploy. Lean: keep next-deploy
   semantics; add an admin endpoint if users ask.
