# DESIGN — liveswap

This document describes the present: what is built, why, and what it
promises. History lives in git (`git log -- liveswap/DESIGN-liveswap.md`);
the "History" section at the end holds only dated one-liners.
Amendments are for decisions still fresh or contested and get folded
into the body once they settle.

This is the handover brief: why the module exists, its normative
behavior, the architecture decisions and their traps, and what was
deliberately left out.

## Why this module exists

The predecessor stack ran a single VPS with Nomad as orchestrator, a ~490-line Deno
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
| `nomadService` template + SIGUSR1 | `http.reverse_proxy.upstreams.liveswap` atomic socket swap |
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
- Config load and every launch MUST warn when an app's `env_file` is
  world-readable or world-writable (the documented mode is 0640
  root:hotserve; owner and group are not checked), and MUST NOT refuse
  on it: a mode changed between loads is not a reason to fail a
  relaunch. The launch reads the mode from the descriptor it rendered
  the environment through, so it reports the file the unit got. At
  load, an absent file is silent (the launch that needs it fails and
  says so), one the loading account cannot reach is noted at info
  level (`validate` runs unprivileged by design), and any other
  failure to check is warned about as such — never reported as fine.
- Config load MUST warn, once per block, when a `github` or `gitlab`
  `deploy_trust` that some app inherits or names pins an identity but
  no branch (`ref` — on GitLab only beside `ref_type` — `ref_path`,
  `sha`, `workflow_ref`, a `ref_protected` not pinned false, or a
  default-form `subject`), and when a `local` one has no audience.
  Both load — each is the documented minimum — and each is wider than
  it reads: any ref of the identity deploys; a token minted for any
  box trusting the key is accepted. An `environment` as the only
  binding is noted at info level, since it binds only as far as the
  provider's deployment-branch rule for it. A claim that resolved
  empty admits no token and MUST be warned about as that, not as
  width. The generic `oidc` preset is not read: it requires `sub`,
  whose shape is the issuer's.
- The deploy MUST be rejected (409) if one is already running for that
  app; other apps' deploys proceed independently.
- A deploy or rollback naming the version that is running MUST
  relaunch it from its release on disk through the deploy pipeline
  minus fetch and extract (`preparing` runs no `pre_start`): a second
  instance of the installed release under a fresh nonce, soaked,
  promoted, the first drained and stopped — so a changed `env_file`
  value or app definition reaches the app without a new version. A URL
  or push deploy relaunches only for the same artifact: its digest
  (the URL deploy's `sha256` pin; a push's upload, hashed as it is
  staged and recorded as its pin) MUST equal the version's recorded
  pin (`recordedPin`). A request without a digest, a version whose
  record has none (an unpinned pull; a push made before pushes were
  hashed), or a different digest MUST be refused (422) without fetching
  anything to compare, in a message that says how a new value reaches
  the app; a URL deploy whose digest matches MUST still pass the url
  checks a fetch gives it (scheme, `artifact_allowlist`), so a URL
  that could not deploy cannot relaunch either. A rollback to the
  running version needs no digest: it is the same release by
  definition. A relaunch MUST write no deploy
  record — the version's record stays the deploy that put it there,
  with its pin and its times, whether the relaunch succeeded or failed
  — and MUST say what it did in `last_deploy` and the response
  (`note`: `relaunched <version> (same artifact); it reads its env
  file now`). A relaunch never writes under the release: it can only
  run the bytes already there.
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
  (devices/FIFOs), setuid/setgid bits, decompressed content beyond 10×
  `max_artifact_size` — as decompressed stream and as the content the
  entries declare, since a sparse entry expands from no stream, and in
  both readings the budget is the archive's, so one entry may
  legitimately be all of it — more than `max_artifact_entries`
  filesystem objects (files, directories and links, implied parents
  included: the byte cap bounds the stream, not the inodes and blocks
  extraction consumes), and names or link targets over PATH_MAX under
  the release directory or with a component over NAME_MAX. Validation
  is a full pre-pass; nothing is written unless every entry is clean.
  A successful deploy reports the entry count and decompressed size
  and warns past 75% of either cap, so the cliff is visible deploys
  ahead. Extraction goes to a staging dir renamed into place on
  success.
- Failed webhook authentications MUST be throttled in the journal:
  per client address (10 failures logged per minute) and process-wide
  (100 failures logged per minute, every line included), so an
  unauthenticated flood has a bounded journal cost from any number of
  sources. The throttle MUST NOT change the answer: every refusal is
  the same flat 401 whatever the budgets say. A valid token MUST still
  be admitted from a throttled address — the throttle bounds lines,
  never deploys. A source the box cannot consult (discovery, key
  fetch) is charged like any failure but MUST be named in
  the journal once per window per source however spent the budgets
  are and whether or not another source then accepted the token, so
  an issuer outage never leaves the journal quiet.
- `pre_start` (if configured) MUST run to completion in the release dir
  before the new instance starts; non-zero exit aborts the deploy. It
  runs under `deadline`, not under the request: a client that hangs up
  MUST NOT stop it midway (a half-applied migration harms the app's
  data); the health gate then fails the deploy on the cancelled
  request, before any traffic moves.
- The new instance MUST be continuously healthy for `soak` before any
  traffic moves (`soak 0`: the first healthy probe), and MUST be
  stopped and the deploy failed if not healthy within `deadline`. A
  flapping health check resets the soak.
- Any failure before the promote step MUST leave the old version
  serving, untouched. The webhook response MUST be synchronous and MUST
  be a 5xx on failure (CI turns red; `curl --fail-with-body`).
- The cutover MUST NOT trigger a config reload; it is an atomic value
  swap read per-request by the upstream source.
- After cutover the pipeline MUST wait `drain`, then SIGTERM the old
  instance's unit — SIGTERM to its whole cgroup, then SIGKILL after
  `grace`.
- Release GC MUST run only after successful deploys, keep the newest
  `keep` dirs by mtime, and never delete the currently-serving version.
  A failed URL or push deploy's freshly extracted release dir is
  removed (a rollback, or a relaunch of the running version, launches
  an existing release and never deletes it), so its version stays
  retriable — unless the failed instance
  may still be running (the
  dir is kept rather than pulled from under a live process; the next
  launch's sweep stops the stray unit) or the removal itself fails;
  either way release GC (after a later successful deploy) removes it
  once it falls outside the newest `keep`.
- Config reloads MUST NOT restart running apps. Changed app definitions
  apply at the app's next launch: a deploy, a rollback, or a relaunch
  after a crash, a sustained health failure, or a reboot.
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
   atomic socket path, returning `unix/<path>`. This was chosen over (a)
   rewriting config via the admin API — a reload per deploy, config
   drift, races with operator edits — and (b) a bespoke proxy handler —
   reimplementing reverse_proxy badly.

### The reload trap (the one Caddy-specific thing you must not break)

Caddy provisions the NEW config before cleaning up the OLD one on every
reload, and module instances are always rebuilt. Anything owned by a
module instance dies on reload. Therefore all live state — the running
process handle, active socket, deploy mutex, last-deploy record — lives
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
- `activeSocket` is the single atomic the proxy hot path reads: the
  current instance's pinned socket, or nil while nothing serves.
- `ownerLock(app)` (one mutex per app name) makes "stop what nobody
  owns, then prune its socket dirs" one step with respect to recovery:
  the start-time sweep of unconfigured apps holds it across the manager
  sweep and the prune (and a single ownership veto during the sweep
  disables the prune, since ownership is not monotonic), `ensureRunning`
  across the whole of recovery, so a reload adding an app back cannot
  adopt dirs the sweep is removing — and one app's slow recovery holds
  up no other app's.
- The runner's logger sits behind an atomic pointer because the
  per-unit watcher goroutines outlive the config that created them.
- The two seams the start-time sweep reads — `caddyExiting` and
  `appConfigured` — sit behind atomic pointers for the same reason:
  that sweep is deliberately detached, joined by nothing, so a test
  restoring a seam can overlap a sweep still reading it.

### Runner abstraction (`runner.go`)

```go
type runner interface {
	Preflight(spec startSpec) error
	Start(spec startSpec) (handle, error)
	RunOnce(ctx context.Context, spec startSpec) error
	Alive(h handle) bool
	Wait(h handle) <-chan struct{}
	Exit(h handle) string
	Stop(h handle, grace time.Duration) error
	Reattach(st handleState) (handle, bool, error)
	Sweep(app string, keep handle) error
}
```

`Preflight` is the launch's resolution run before any unit exists:
`command[0]` resolves (`resolveInView`, shared with `unitFor`) to a
file inside the sandbox view, and an ELF built for another 64-bit
machine is refused naming the box's architecture and the `runs-on`
to build on (`elf.go`, twenty bytes of header, no dependency). The
deploy runs it for the command and the `pre_start` in phase
`preparing` and reports a refusal as a 422; `unitFor` still resolves
at launch, as the last word before the manager acts.

`Exit` is how the instance's main process ended ("exit status 3",
"killed by signal 9 (killed)"), "" while it runs, and non-empty
whenever `Alive` is false: a runner records the exit before it
declares the handle dead. A failed deploy's response reads it
(`failureDetail`, app.go), so a runner that cannot observe the exit
must still say so in words rather than return "". `Start` and
`RunOnce` report a unit that ran and failed as a typed `exitError`,
which the same detail reads for the `pre_start`'s and a failed start
job's exit.

The one implementation is `systemdRunner` (`runner_systemd.go`, D-Bus
client in `systemd_dbus.go`): every instance is a transient service
unit under the **hotserve user's own systemd manager**
(`user@<uid>.service`), reached over that manager's private socket
(`/run/user/<uid>/systemd/private` — no session bus, no polkit, no
root). There is no other runner: an exec runner would need a page of
init-system emulation for what cgroups do natively, and would tie app
lifetime to the binary's.

What the unit gives us: `KillMode=control-group` makes stop and crash
cleanup mean the whole process tree (npm → node → workers), the
journal captures stdout/stderr under `SyslogIdentifier=hotserve-<app>`,
`Restart=no` keeps the watchdog the sole restart authority,
`NoNewPrivileges` is on, and `LimitCORE=0` on both limits means a
crash writes no core dump of the app (DESIGN-threat-model.md, "Where
`env_file` values sit"). `handleState.Unit` (persisted in
`state.json`) is what `Reattach` looks up after a hotserve restart: a
running unit is adopted with the socket its recorded nonce names —
pinned afresh, so a name that no longer holds a socket refuses the
adoption — and a failed one is
logged, reset and relaunched. On process exit `Destruct` leaves units
running for exactly that reason; only removing an app from the config
stops it.

The runner's invariants are written in its doc comment. The ones the
state machine leans on: a handle is declared dead only on an
*observed* terminal unit state (a D-Bus outage never becomes a phantom
crash and a duplicate instance); `Reattach` reports an unreadable unit
as an error, and recovery retries rather than launching beside it;
`Stop` returns nil only once the unit is observed gone; a start or
pre_start whose D-Bus call failed is reconciled by unit name and, if
even that is impossible, reported as "may still be running" so the
deploy keeps the release; and **the manager is the ledger** — every
unit is named `hotserve-<app>.<version>.<nonce>.service` (the `.`
after the app is the one character an app name cannot contain), and
`Sweep(app, keep)` stops every other unit of the app. Recovery sweeps
after reattach/relaunch, deploys sweep before GC, removing an app
from the config sweeps all of it, and `App.Start` sweeps the units of
any app the config no longer names — so nothing hotserve lost track
of (a failed `state.json` write, an unconfirmed stop, an app renamed
while hotserve was down) survives the next start or the next deploy,
and no release is deleted while the manager still runs something out
of it. There is
deliberately no persisted "leaked releases" ledger: systemd already
knows what is running, and asking it is cheaper and truer than
remembering.

Why the user manager and not the system one: a polkit rule granting
`org.freedesktop.systemd1.manage-units` cannot inspect unit
properties, so it would let a compromised hotserve start a root unit.
Per-app UIDs — the one thing the system manager would buy — are a later
milestone behind a small privileged helper. Isolation between apps
and hotserve comes from systemd's per-unit sandboxing on every unit
(probe-gated at start: a host without unprivileged user namespaces is
refused, and there is no setting that runs an app without one), see
`DESIGN-sandbox.md` and `DESIGN-threat-model.md`.

Packaging consequences: `libpam-systemd` (pam_systemd hands the user
manager its `XDG_RUNTIME_DIR`) and `dbus` are hard dependencies;
postinstall enables lingering and drops in `Wants=/After=user@<uid>`
on `hotserve.service`; preremove stops the user manager (and with it
every app) on real removal only. Start fails loudly if the manager is
unreachable — there is deliberately no fallback runner; the check is
in Start rather than Provision so `hotserve validate` (which
provisions and cleans up without starting) works as any user.

Config commit is transactional in the way Caddy's activation demands:
Provision only takes pool references, Start installs the definition
on the pooled app, and a candidate Caddy rejects after our Start
(another app's Start failing) has its definition rolled back by its
own Cleanup — last-writer semantics, so a config replaced by a
successful reload is untouched. Destruct stops a removed app's units
only when another *started* config is live; a rejected candidate,
`validate`, and process exit leave them to whoever is or will be
serving them. Boot recovery is owned by the config that started it,
retried with backoff for anything not explicitly permanent, and
cancelled/joined by that config's Cleanup.

### Layering

The package is flat, so the compiler cannot report a dependency
pointing the wrong way: an import cycle is unreachable and every file
sees every other. Nothing enforces the order below — it is written
down because #60 found eight edges running backwards, and the only
reason they were found at all was somebody building this map by hand.

| Layer | Files |
|---|---|
| 0 | `clock.go`, `names.go` |
| 1 | `appdirs.go`, `ownfile.go`, `socket.go` (+`_linux`/`_other`), `extract.go`, `allowlist.go`, `health.go` |
| 2 | `runner.go`, `sandbox.go`, `download.go`, `state.go` |
| 3 | `runner_systemd.go`, `systemd_dbus.go` |
| 4 | `app.go`, `watchdog.go`, `sweep.go` |
| 5 | `liveswap.go`, `handler.go`, `upstreams.go`, `caddyfile.go`, `deploytoken_cmd.go` |

A file may use a lower layer or its own. Sideways is fine: a layer is a
concern, and a concern is sometimes split across files — `appdirs.go`
and `socket.go` are one path vocabulary; `runner.go` and `sandbox.go`
hand each other value types.

Six edges run upward on purpose. They are listed here so a seventh has
to justify itself rather than blend in:

| Edge | Why it is right |
|---|---|
| `app.go` → `liveswap.go` (`caddyExiting`, `liveStartedApps`) | process-lifecycle facts live with the module that owns the pool; `Destruct` reads them to tell a shutdown from an app being removed |
| `sweep.go` → `liveswap.go` (`appPool`, `poolKey`, `caddyExiting`) | the pool is the ledger the sweep asks what is still configured |
| `download.go` → `app.go` (`appSpec`, `deployRequest`, `validationError`) | `app.go` holds the `fetcher` interface and `download.go` implements it, so the call points down; only the value types it is handed point back |
| `sandbox.go` → `app.go` (`appSpec`) | `sandboxSpecFor` is a method on `*appSpec` — methods live with the concern, not the receiver |
| `sandbox.go` → `liveswap.go` (`App`) | `validateEnvFileIsolation` is a whole-config `Validate` rule: checking one app's `env_file` against every other app's dirs needs the whole config |
| `systemd_dbus.go` → `liveswap.go` (`managerClient`) | the interface is declared where it is consumed and asserted where it is implemented — Go's normal direction |

A machine-checked version of this table was built and rejected (#75):
resolving "which file declares this name" from the syntax tree alone
takes a guess at every construct that shares a shape with another —
map keys against struct fields, generic instantiations against
collections — and six review rounds each found another. Doing it
properly needs the type checker, which is a real dependency for a
property no operator ever sees. The trigger for revisiting is
backwards edges reappearing *despite* this table.

### File-by-file

| File | Concern |
|---|---|
| `liveswap.go` | App module, config structs + defaults, Validate, pool wiring |
| `names.go` | the app-name and version alphabets, and the two helpers over them; shared by every layer |
| `app.go` | `managedApp` state machine, Deploy pipeline, recovery, env building |
| `appdirs.go` | `appDirs`: the on-disk layout for one app. Pure path arithmetic — no state, no lock |
| `ownfile.go` | how a file hotserve keeps in an app dir is written and read — temp file + rename; no link followed at the name, no FIFO held, size-bounded — shared by the deploy records and `state.json` |
| `caddyfile.go` | all Caddyfile parsing (global option, directive, upstreams) but the `deploy_trust` block, which is `deploytrust.Parse`; NO defaults here — Provision owns them, reading a zero field as unset, so a value below an option's minimum (0 included) is refused here, named; `soak` and `deploy_log_lines`, whose 0 is a setting, are pointers |
| `handler.go` | the webhook: payload validation, deploy dispatch, the status endpoint, and every body's filter (`RespondJSON`); authentication is `deploytrust`'s |
| `deploytrust/` | leaf package: deploy auth — the `deploy_trust` grammar (`Parse`), its sources and verifiers (OIDC + local-key JWT verification), the webhook preamble (`Limiter.Authenticate`) and what a failed attempt costs the journal (`Limiter`, on an injected clock). Imports nothing of liveswap's; the box webhook authenticates through it too. `trusttest/` beside it mints tokens and runs an issuer for tests |
| `internal/dispenser/` | leaf package: the one Caddyfile rule the parsers here and `deploytrust.Parse` apply alike — a repeated subdirective is refused — in one place so the two cannot drift |
| `upstreams.go` | dynamic upstream source (the cutover read side) |
| `runner.go` / `runner_systemd.go` / `systemd_dbus.go` | runner interface + the systemd transient-unit implementation + its D-Bus client |
| `sweep.go` | `App.Start`'s reconciliation against the manager: stop the units, and prune the dirs, of apps no loaded config names. Module-layer (the pool is the ledger), driving the runner |
| `socket.go` | the per-instance socket: nonce, path checks, stale-socket pruning, the unix dialer |
| `sandbox.go` | the per-unit sandbox: view spec, base view, bind-source checks, capability probe (see DESIGN-sandbox.md) |
| `harden/` | leaf package: the supervisor goes non-dumpable at init, before `os` |
| `allowlist.go` | `artifact_allowlist` parsing, canonicalisation and URL pinning |
| `download.go` | capped, redacted artifact download; `releaseFetcher` (download+extract orchestration) |
| `extract.go` | validate-then-extract hardened tar handling |
| `health.go` | prober with soak/deadline arithmetic on an injected clock |
| `watchdog.go` | continuous crash/health supervision: per-app loop, restart budget, backoff |
| `state.go` | `state.json` atomic persistence, keep-N GC |
| `deploytoken_cmd.go` | the `hotserve deploy-keygen` / `deploy-token` subcommands |
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
- Apps listen on a per-instance unix socket in their own `run/<nonce>/`
  dir; `SOCKET` is injected. Nothing hotserve runs listens on a port,
  so there is no sibling port to reach. The socket and the unit share
  a nonce, which is what `state.json` records; a record whose nonce is
  not the unit's, or whose socket is not there to dial, is treated as
  nothing recorded. hotserve never dials the socket by its
  app-writable name: the first connect verifies the inode
  (`O_NOFOLLOW`, `S_IFSOCK` only), hard-links it to
  `<app>/proxy/<nonce>.sock` — outside every view — and proxy and
  prober dial that from then on; retiring an instance unlinks it. So
  bind once per instance. The unit removes its own
  socket when it stops (`ExecStopPost=`); a confirmed sweep prunes any
  left behind. The health probe is `GET <health_path>` with
  `Host: localhost`.
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

Every lane but the first runs against **real systemd** (a privileged
container with the host cgroup namespace); only unit tests use a fake
D-Bus connection. Tests are written from the invariants, adversarially
where the real system can be made to misbehave — a fake encodes the
author's beliefs about systemd, and only the real thing corrects them.

1. Unit (`make test`, race, goleak): the deploy state machine (happy
   path; pre_start abort; health abort keeps old serving; 409;
   same-version relaunch — a matching pin, a push of the same bytes, a
   rollback onto the running version — and its refusals — no digest
   on either side, a different digest — with the version's record
   untouched either way and a failed relaunch leaving the old
   instance serving; GC gated on a confirmed sweep); recovery
   (reattach, relaunch, no-state sweep, transient vs permanent
   classification, retry loop on the fake clock, refusal of a foreign
   unit name in state.json, Cleanup joining recovery); Destruct's
   live-config gate and rollback of a rejected candidate's definition;
   the runner over a scripted manager (unit properties and naming,
   watcher terminal states and transport-error immunity, PID backfill,
   Stop/RunOnce outcomes, reconcile of an ambiguous start, concurrent
   guarded sweeps, unknown-app sweep judged against the live pool);
   watchdog crash/health/throttle/unconfirmed-stop paths; env
   precedence, placeholders and key validation; malicious-tar table;
   download caps/allowlist/redaction; prober soak/flap/deadline;
   parser and Validate tables.
2. Fuzz (`make fuzz`, weekly in CI; seed corpora run in `make test`):
   artifact extraction, path containment, URL redaction — and, for the
   runner: unit-name round-trip (a name parses back to exactly its app
   and never matches a sibling; anything invalid is refused),
   env_file keys (every key handed to a unit is one systemd accepts),
   and the D-Bus value parsers (no overflowed budgets, no "infinity"
   sentinel, no panics).
3. Integration (`make test-integration`, caddytest under a real user
   manager): webhook deploy v1 → proxied body; v2 cutover; broken v3 →
   500 and v2 still serving; reload keeps the app PID; keep-N; OIDC,
   push and rollback deploys; watchdog restart and throttle recovery.
   The runner's own suite: a leader+worker tree is gone after Stop and
   after a leader crash; SIGKILL lands after grace, not before; exit
   statuses as systemd reports them; a second runner adopts a live
   unit and never a failed one; Sweep stops strays; a manager
   SIGSTOPped past a poll timeout is not a crash.
4. e2e (`make e2e`, the packaged unit under systemd as `User=hotserve`,
   laid out by `packaging/postinstall.sh`): module-linked build guard;
   auth; 5xx-before-first-deploy; **continuous curl loop through the
   v2 cutover with zero non-200s**; broken-v3 containment; 409;
   **admin-API reload under traffic with unchanged app PID**; status;
   watchdog after crash and after health failure; push and rollback.
   Then, inside the container: the app is a unit under `user@<uid>`
   with `Restart=no`; `systemctl restart hotserve` keeps the same app
   PID (reattach), and so do hotserve's own deaths — SIGKILL, exit 2,
   an OOM kill — which the unit undoes on its own, while a start
   hotserve refuses stays down; stopping a version takes its whole
   process tree; a crash leaves no failed unit; a unit gone behind
   hotserve's back is relaunched on start; removing the app via reload
   stops its units and re-adding it relaunches; an app removed while
   hotserve was down is swept on the next start; app output is in the
   journal. Finally the recovery suite from the runner's side.
5. Package (`make install-test`, the built `.deb` on debian:13 — the
   support matrix, on both arches): Depends resolution (`libpam-systemd`,
   `dbus`), user manager active with lingering, a deploy under the
   shipped unit as a transient unit with its output and `LimitNOFILE`
   in the journal and `LimitCORE=0` on the unit and inside it,
   `hotserve validate` as root and as hotserve leaving
   the running app alone, the app keeping its PID across the upgrade
   restart, and removal taking the apps, drop-ins and linger flag.

## Watchdog

Design summary (full operator docs in README.md "Watchdog"):

- One goroutine per pooled `managedApp` (`watchdog.go`), started at
  first Start, torn down in `Destruct` **before** the child is
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
  relaunch path is `launchVersion`, shared with boot recovery: the
  record pins the **version**, and command, env and sandbox render
  from the spec as it is now, so an app definition edited since the
  deploy reaches the next crash restart — the same rule
  `watchdogDisabled` follows for the same event, and the same exposure
  a reload has always had. The record deliberately holds no launch
  disposition: reproducing a stale instance faithfully would close the
  one way an operator has to fix a crash-looping app without a
  deploy, make "every unit is sandboxed" a property of a file rather
  than of the code, and write env secrets to disk. What makes the
  drift survivable is visibility, not fidelity — status reports the
  argv the manager holds for the running unit (`ExecStart`, read back
  at adopt and at reattach, never persisted), so a definition that has
  drifted from what is running is visible rather than inferred from an
  app that changed behaviour at a restart.
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

## Accepted residuals

Findings scored by the repository's triage rule
(`.claude/skills/triage/SKILL.md`) and accepted; the scores are
severity × occurrence × detection, then the fix's size and risk.

| Failure | Where | S | O | D | Product | Fix | Why accepted |
|---|---|---|---|---|---|---|---|
| A relaunch of the running version starts a second instance of the same release in the same release dir, bound read-write (`sandbox.go`), for the overlap — `soak`, then `drain` and the old instance's stop. An app that keeps mutable state in its release dir rather than under `HOME` (its shared dir) may see its two instances contend there. | `deployLocked`, relaunch | 3 | 5 | 3 | 45 | size 3, risk 2 (a copy per instance, or a stop-then-start branch) | Scores to fix; accepted by decision (#234, 2026-10-16). Both fixes cost what the feature exists to avoid: stop-then-start drops every request for the launch time, which the watchdog's restart already does and a deploy never does; a copy per instance doubles the disk of each relaunch and makes two paths to one release (`{release_dir}`, status, GC). The shared dir is already shared by both versions in every deploy, so an app's state there meets the same overlap today; the release dir is the app's code, and nothing of hotserve's writes under it during a relaunch. An app that conflicts fails the health gate or crashes in the watchdog's view, red and named; the silent case needs state the app chose to keep where every deploy already discards it. |
| A relaunch matches the request's digest against the version's record, not the release's bytes (`recordedPin`'s known limit: a version release GC pruned, deployed again, whose record write then failed). A request carrying the old digest then relaunches the bytes on disk. | `relaunchRefusal` | 3 | 1 | 3 | 9 | size 3 (a pin kept with the release) | The relaunch introduces no bytes: whatever the match decides, what runs is the release that was already serving. Closing it is the same fix the known limit names for rollbacks. |
| A client that hangs up during `pre_start` does not stop it: the migration runs to its end, then the deploy starts the new instance and the health gate fails it on the cancelled request (phase `soaking`, `deploy canceled while waiting for health`). The new instance runs its startup for that moment, the release is removed, and a retry runs `pre_start` again, as it does after any gate failure that follows a `pre_start`. | `deployLocked`, after `pre_start` | 3 | 3 | 1 | 9 | size 2, risk 2 (a request check before the start); size 3, risk 3 (run the rest of the deploy detached) | Rule 2 holds (it ends red, the cause named); under rule 3 the product is under 20 and neither fix is size 1, risk 1. The shape is the owner's decision (#295, 2026-10-17): the gate aborts the deploy as it does for any client that left. The old instance serves throughout, and a migration that cannot run twice already meets the same retry after any gate failure. |
| A `pre_start` that exits within milliseconds of `deadline` can still read as timed out: `jobResult` prefers a job result already received when the context ends, but not one whose signal is still in flight. The deploy fails red naming the deadline, and the release of a migration that ran is removed. | `jobResult` | 2 | 3 | 1 | 6 | size 3 (a second completion source: a oneshot's transient unit is unloaded on success, so no `Result` is left to ask for) | Rule 2 holds (red, named); under rule 3 the product is under 20. A migration that ends on its deadline has used its whole budget; raising `deadline` is the remedy. |
| A Debian 13 host that passes the tutorial's step 2 (apt's `systemd (>= 257)` floor, `systemd-detect-virt --container`) but whose kernel or container refuses the sandbox's namespaces installs and serves the starter config, and first fails at step 3: `hotserve init`'s reload is liveswap's first start. That reload is refused, the probe's error names the cause, and the starter keeps serving; the box's repository, signer and workflow carry over to a fixed or new host. | `probeSandboxCapability`, from `App.Start` | 2 | 5 | 1 | 10 | size 3, risk 1 (a sandbox-check subcommand that step 2 runs as the hotserve user) | Rule 2 holds (it ends red and named); under rule 3 the product is under 20 and the fix is not size 1, risk 1 (#274, 2026-10-17; #225 scored the same case at occurrence 3, but a host is the operator's choice, so it scores as input). The probe needs the hotserve user and its lingering manager, which the package's postinst creates, so the earliest any check could run is the end of `apt install`, minutes before step 3, with the same refusal and the same fix. A check in postinst could only warn, since a failed postinst leaves the package half-configured: one line in apt's output, after up to two 30-second probe budgets inside dpkg. And liveswap with no apps runs no probe (`len(a.managed) > 0` in `App.Start`), so the starter config cannot carry one. |

## Non-goals

- Post-promote *auto*-revert. Rollback is an explicit operation —
  `POST /<app>?rollback=<version>` relaunches an on-disk release. The
  watchdog restarts the same version; it never auto-reverts.
- Multi-node or any cluster awareness.
- Resource limits — one unit property away (`MemoryMax=`, `TasksMax=`,
  `CPUQuota=`; the sandbox makes cgroupfs read-only so they hold);
  unset by design until an app needs them (#71).
- Prometheus metrics (deploys_total, duration).
- Deploy queueing; 409 + CI retry is the queue.
- Windows.
- Streaming (NDJSON) deploy progress; the response is buffered JSON so
  `curl --fail-with-body` semantics stay honest.

## Open questions (with leans)

1. **Deploy events** — emit Caddy events (deploy_succeeded/_failed) so
   users can wire notifications. Lean: yes, cheap and composable.
2. **Admin endpoints** (`caddy liveswap status/rollback`) alongside the
   webhook. Lean: later; the webhook + status JSON covers CI and cron.
3. **Metrics.** Lean: add with events.
4. **Config-change restarts** — an explicit "apply now" admin action
   instead of waiting for the next launch. A rollback to the running
   version, or a deploy of it with the same artifact, is that action
   through the webhook (Behavior specification); the lean is to add
   no admin endpoint beside it.

## History

Dated one-liners; the full text of each is in git.

- 2026-08-17 (v0.1.0) — v1 shipped with an exec runner: apps as child
  processes in their own process group, restarting with the binary.
  The continuous watchdog was a stated non-goal.
- 2026-08-18 (#28) — Watchdog shipped as a follow-up, on by default.
- 2026-08-28 (#29) — Shared deploy secret replaced by `deploy_trust`
  (OIDC + local key).
- 2026-08-29 (#34, `17f1a04`) — Exec runner replaced, not fixed, by the
  systemd transient-unit runner on the hotserve user's manager; units
  survive a hotserve restart (`Reattach`). The test lane it needed is
  a privileged systemd container.
- 2026-09-02 (#40) — Every unit sandboxed; see DESIGN-sandbox.md.
- 2026-09-09 (#52) — A relaunch renders from the current spec: written
  down as the rule rather than the record growing a launch
  disposition, and status now reports the unit's own `ExecStart` so
  the drift is visible. `pid` left `state.json` with it.
- 2026-10-10 (#188) — Deploy auth became the leaf package
  `deploytrust/`: the `deploy_trust` grammar, sources, verifiers, the
  limiter and the webhook preamble, which now returns who the token is
  (`Identity`, the claims included) or a `Refusal` the caller writes
  through its own filter. The box webhook authenticates through it on
  the same limiter. `NewRedactor` and `RespondJSON` give it the
  response filter. One visible change: an app name carrying a control
  byte or invalid UTF-8 is Go-quoted in the two auth journal lines, as
  a refusal already was. Nothing else changed.
- 2026-10-16 (#234) — A deploy or rollback to the running version
  relaunches it from its release on disk (same artifact only, by
  digest; a push's upload is hashed and recorded as its pin from here
  on) instead of being refused; the two instances share the release
  dir for the overlap, recorded as an accepted residual.
