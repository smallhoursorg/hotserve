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
  key), and its answer MUST NOT reveal whether an app exists to
  unauthenticated callers (unknown apps verify against the global trust
  sources, then 404; a refused token gets the same 401 either way). Its
  timing can, within a bound (DESIGN-threat-model.md, "Webhook
  endpoint", the residual; owed: #306): a token is checked against
  each source a name resolves to in turn, the signature before the
  issuer, so an OIDC source refuses a forged token after a local
  signature check when it holds the token's kid, and, when it does
  not, after a fetch of its issuer's keys that it makes at most once a
  minute, whatever issuer the token names. A name that does not exist
  goes to the global sources: it is refused at once when they hold no
  OIDC source, and otherwise pays the same on their own key sets.
  The fetch window is per app, not shared across apps that trust the
  same issuer. No shared secret is stored on the box.
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
- A deploy, push or rollback MUST be refused (500) before it reads or
  writes anything through the app's dirs when the app dir, or a dir
  it makes (`releases/`, `shared/`, `tmp/`, `run/`, `proxy/`), is a
  link, is not a directory, or cannot be checked for a link
  (`writeRefusal`): through `<root>/blog -> shop` it would stage in
  shop's `tmp/` and extract into shop's `releases/`. The refusal's error
  names the path as the app spells it, never what a link resolves to;
  the status the response carries still reads through the link
  (Accepted residuals). An alias on the liveswap root itself is no
  refusal.
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
  request, before any traffic moves. Its unit carries `deadline` as
  its own `TimeoutStartSec=`, so it ends on time when no hotserve is
  watching it. A hotserve stop or restart MUST NOT stop it midway
  either: the manager runs it on past hotserve, and every sweep but
  an app's removal leaves a running `pre_start` unit alone and
  reports itself unconfirmed, naming it, so recovery, the watchdog
  and the next deploy start and delete nothing until it has ended
  (recovery retrying on its backoff).
- The new instance MUST be continuously healthy for `soak` before any
  traffic moves (`soak 0`: the first healthy probe; refused at load
  with `health_path off`, where nothing is probed), and MUST be
  stopped and the deploy failed if not healthy within `deadline`. A
  flapping health check resets the soak. The gate is judged once per
  `health_interval`, so it ends within `deadline` plus one
  `health_interval` and two `health_timeout`s; a soak that falls due
  on the first tick past `deadline` still passes if that tick's probe
  does.
- Any failure before the promote step MUST leave the old version
  serving, untouched. The webhook response MUST be synchronous and MUST
  carry a 5xx on failure: as its status (CI turns red; `curl
  --fail-with-body`), or, for a client that sent `Accept:
  application/x-ndjson`, as the last line's `http_status`. That client
  gets the deploy streamed — a line per phase, then the single
  response's body with its code as `http_status` — and the status line
  is 200 once the first line has gone out.
- The cutover MUST NOT trigger a config reload; it is an atomic value
  swap read per-request by the upstream source.
- After cutover the pipeline MUST wait `drain`, then SIGTERM the old
  instance's unit — SIGTERM to its whole cgroup, then SIGKILL after
  `grace`.
- Release GC MUST run only after successful deploys and keep the
  `keep` most recently deployed releases, the one the deploy promoted
  first — so the serving version is never deleted and counts toward
  `keep` — and the one it replaced next, the way back, by what served
  rather than by its record (a rollback whose record write was lost
  leaves that version's record at an older deploy). A release's
  deploy time MUST be one no app can write: its deploy record's write
  time (`deploys/`, outside every view; the version's latest deploy or
  rollback), never the release dir's own mtime, which the app running
  from that dir can set to anything. A release whose latest deploy
  failed (a failed rollback to it) comes after every release whose
  latest deploy succeeded: that deploy never served, and takes no
  place from one that did. A release with no record that proves itself
  one (deployed before records existed, or its record write failed)
  comes after every recorded release, ordered among its kind by its
  dir's mtime, and the status MUST name it in `ordered_by_mtime`: that
  mtime is the app's to set, so it decides nothing among recorded
  releases, and such a release is the first GC candidate after the two
  held by rank, and a rollback target while it is on disk. A version's
  first deploy writes its record as the deploy returns, so from the
  cutover until then the status names the running version in
  `ordered_by_mtime` too. `available_versions` lists the releases in the
  same order, the running version first — it is what the next deploy
  keeps beside the new one — so the rollback targets an operator sees
  are the ones the next GC keeps. GC MUST delete no release, and log
  why, when the app dir or `releases/` is not the directory it names
  (`linkAt`, as for the deploy's refusal above: through a link it
  would delete another app's releases) or the records dir cannot be
  read (refused the same way: the whole order would be the app's to
  set). A release whose
  own record is there but cannot be read (permissions, I/O, not a
  regular file — not the same as none) MUST be held: neither deleted
  nor counted toward `keep`, and named each GC, while the rest are
  pruned by their records. Crash-orphaned `.extract-*` staging dirs
  are removed whenever GC runs, since no order decides them. A dir
  under `releases/` whose name is not a version sorts after every
  release and holds no `keep` place, so it is removed whenever GC
  prunes. Nothing could deploy it or roll back to it, and the status
  does not list it, so it never takes the place of a version the
  status lists. A failed URL or push deploy's freshly extracted release dir is
  removed (a rollback, or a relaunch of the running version, launches
  an existing release and never deletes it), so its version stays
  retriable — unless the failed instance
  may still be running (the
  dir is kept rather than pulled from under a live process; the next
  launch's sweep stops the stray unit) or the removal itself fails;
  either way release GC (after a later successful deploy) removes it
  once it falls outside the newest `keep`.
- A URL or push deploy cut short by hotserve stopping (a crash, a
  restart, an upgrade) MUST leave its version deployable, and MUST
  NOT leave a release a rollback could launch without its
  `pre_start`. Before it extracts, the deploy writes
  `<app>/deploying.json` (its version and start time; outside every
  view, through `ownfile.go`), and removes it once its record is
  written, whatever it came to; a rollback or a relaunch writes none.
  A marker found by the next recovery or the next deploy, each after
  a confirmed sweep, is a deploy no hotserve finished: if it names
  the version `state.json` names or the instance kept, that deploy
  reached the cutover, and the marker is only removed; if the
  version's record carries the marker's start time, that deploy was
  recorded and the marker is stale, only removed; otherwise the
  release is removed (refused through a link, `linkAt`), a failed
  record names the restart, and then the marker goes. A removal that
  fails keeps the marker for the next try. A marker that does not
  prove itself one (it does not parse, or names no version) is
  removed and acted on for nothing.
- Config reloads MUST NOT restart running apps. Changed app definitions
  apply at the app's next launch: a deploy, a rollback, or a relaunch
  after a crash, a sustained health failure, or a reboot.
- A reload that removes an app MUST leave none of its units running:
  a deploy of it in flight is waited for, then everything the manager
  holds for the app is stopped, and the removed app takes no deploy
  after.
- On Caddy start, each app with recorded state MUST be relaunched (or
  reattached, if the runner supports it) and published as soon as the
  process is up — the health gate is a deploy gate, not a boot gate.
- Every relaunch — a restart's recovery, a watchdog restart, a
  rollback — MUST run the deploy's pre-flight on the spec it starts,
  and a refusal (a release built for another machine; a command not
  there, or outside the sandbox view) MUST end it once, red and
  named, never in a loop: a rollback is refused as a deploy is (422),
  recovery gives up, and the watchdog drops the instance — nothing
  current, nothing routed — and parks. The journal says so once, and
  the status (`watchdog.last_failure`) until a launch replaces it.
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
  `Destruct`, removing the app, takes it with a blocking `Lock` and
  keeps it: a reload stops the old config's servers without waiting
  for their requests, so a deploy of the removed app can still be in
  flight, and past Destruct's sweep it would start a unit nothing
  supervises, routes or stops. The wait is the deploy's remaining
  run. Once the artifact is on the box the app's own settings bound
  it — `deadline` for a `pre_start` and again for the health gate
  (plus one `health_interval` and two `health_timeout`s), `drain`,
  and each stop's `grace` — with the runner's fixed allowances (30 s
  for the start job, 30 s past a stop's grace); a download's or a
  push upload's transfer has no bound of hotserve's own, only the
  artifact host's or the client's pace and the server's read
  timeouts. The removing reload's cleanup, and so its response, waits
  that long, and says so in the journal when it has to wait. Kept, the
  lock leaves the removed app no further deploy: a request the old
  config still serves gets a 409.
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
to build on, and a 32-bit ELF naming the architecture to build for,
since units allow the native system-call ABI only (`elf.go`, twenty
bytes of header, no dependency). The
deploy runs it for the command and the `pre_start` in phase
`preparing` and reports a refusal as a 422; every relaunch
(`launchVersion`: recovery and the watchdog) runs it for the command
after its sweep, and a refusal there is permanent; `unitFor` still
resolves at launch, as the last word before the manager acts.

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
`Restart=no` keeps the watchdog the sole restart authority, the
command is set as `ExecStartEx` with `no-env-expand` so its argv execs
byte for byte (systemd substitutes no `$VAR` into it, and no shell is
in the way either), `NoNewPrivileges` is on, and `LimitCORE=0` on both limits means a
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
`Sweep(app, keep)` stops every other unit of the app but a running
`pre_start`, which it leaves to finish and names, unconfirmed.
Recovery sweeps after reattach/relaunch, deploys sweep before a
`pre_start`, a start and GC, removing an app from the config stops
all of it (`StopAll`, sparing nothing), and `App.Start` stops the
units of any app the config no longer names — so nothing hotserve lost track
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
only when another *started* config is live, and only once a deploy of
the app in flight has ended (Concurrency model); a rejected candidate,
`validate`, and process exit leave them to whoever is or will be
serving them, and wait for nothing. Boot recovery is owned by the config that started it,
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
| 1 | `appdirs.go`, `ownfile.go`, `socket.go` (+`_linux`/`_other`), `extract.go`, `elf.go`, `allowlist.go`, `health.go`, `journal.go` |
| 2 | `runner.go`, `sandbox.go`, `download.go`, `state.go` |
| 3 | `runner_systemd.go`, `systemd_dbus.go` |
| 4 | `app.go`, `deploys.go`, `redact.go`, `watchdog.go`, `sweep.go` |
| 5 | `liveswap.go`, `handler.go`, `upstreams.go`, `caddyfile.go`, `trustwarn.go`, `deploytoken_cmd.go` |

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
| `appdirs.go` | `appDirs`: the on-disk layout for one app — path arithmetic, and the one check that a dir of it is the directory it names (`linkAt`, by Lstat), which every place hotserve writes or deletes in an app dir runs. No state, no lock |
| `ownfile.go` | how a file hotserve keeps in an app dir is written and read — temp file + rename; no link followed at the name, no FIFO held, size-bounded — shared by the deploy records, `state.json` and the deploy marker |
| `caddyfile.go` | all Caddyfile parsing (global option, directive, upstreams) but the `deploy_trust` block, which is `deploytrust.Parse`; NO defaults here — Provision owns them, reading a zero field as unset, so a value below an option's minimum (0 included) is refused here, named; `soak` and `deploy_log_lines`, whose 0 is a setting, are pointers |
| `handler.go` | the webhook: payload validation, deploy dispatch, the status endpoint, and every body's filter (`RespondJSON`); authentication is `deploytrust`'s |
| `redact.go` | the response filter (`Redactor`) that `RespondJSON` applies: known `env_file` values in their encoded forms, a safe list, shape rules, entropy; the four rules every writer of a body holds head the file |
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
| `elf.go` | the pre-flight's ELF check: a release's command built for another machine, or 32-bit, is refused before any unit exists. It also declares `preflightError`, the type every pre-flight refusal shares, reported as a 422 |
| `health.go` | prober with soak/deadline arithmetic on an injected clock |
| `journal.go` | the journal tail a failed deploy's response carries: `journalctl` run with argument lists built here, its output bounded |
| `watchdog.go` | continuous crash/health supervision: per-app loop, restart budget, backoff, the wait for a deploy holding the lock |
| `state.go` | `state.json` atomic persistence; the release order (deploy time) that keep-N GC and `available_versions` share |
| `deploying.go` | the deploy marker, `<app>/deploying.json`: a URL or push deploy in flight, so the next start or deploy removes the release of one hotserve stopped during, and records it failed |
| `deploys.go` | the deploy record store, `<app>/deploys/<version>.json`: written once through the filter, served back only through it, pruned with the releases; its four rules head the file |
| `trustwarn.go` | the load-time pass that asks `deploytrust.WarnUnboundSource` about each `deploy_trust` block once, the global ones only when some app inherits them |
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
  left behind, but never through a link: if the app dir, `run/` or
  `proxy/` is one (`<root>/blog -> shop`), is not a directory, or
  cannot be checked for a link, the sweep logs it and prunes nothing.
  The health probe is `GET <health_path>` with `Host: localhost`.
- `Authorization` (`auth_header`) follows a redirect only while the
  hop's host is the first hop's or a subdomain of it, and is dropped
  for good at the first hop outside that domain (Go's
  `shouldCopyHeaderOnRedirect`) — exactly right for GitHub's asset→S3
  redirect, which leaves github.com's domain.
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
   unit name in state.json, Cleanup joining recovery); the pre-flight
   on every relaunch path, on the spec it starts, and a refusal ending
   each once; Destruct's
   live-config gate, its wait for a deploy in flight (nothing left
   running whatever the deploy came to, no wait on exit), and rollback
   of a rejected candidate's definition;
   the runner over a scripted manager (unit properties and naming,
   watcher terminal states and transport-error immunity, PID backfill,
   Stop/RunOnce outcomes, reconcile of an ambiguous start, concurrent
   guarded sweeps, unknown-app sweep judged against the live pool);
   watchdog crash/health/throttle/unconfirmed-stop paths, and the
   ledger of a restart meeting a deploy over a dead instance (one
   crash Warn however long the deploy, the instance back on the fake
   clock's same instant after a failed deploy, a nudge ending no
   step), with every lock holder waking the watchdog as it lets go; env
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
   push and rollback deploys; watchdog restart and throttle recovery;
   a relaunch of a release built for another machine refused once by
   the watchdog and by recovery, with no unit started; a reload that
   removes an app mid-deploy leaves no unit of it running.
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
- Restarts take `deployMu` and re-check instance identity — promote
  swaps `current` before stopping the old handle, so a deploy-stopped
  handle never reads as a crash. A restart that finds the lock held
  waits for its holder to let go rather than re-entering supervision
  per backoff step: every holder but the watchdog lets go through
  `unlockDeploy`, which pokes `wdNotify`. A promoted deploy has
  installed its own instance; after any other end the restart goes at
  once. A deploy that ends after the failure was found and leaves the
  instance current (`endDeploy`, counted in `deploysEnded`) also ends
  the backoff step, so a dead instance is back about a second after a
  failed deploy rather than at the end of a step of up to 72 s; a
  throttle, the budget's, still stands, a config nudge ends no step,
  and the step is never refunded (refunding it made a 0.8 s loop,
  #150). While it waits for the lock the state stays `backoff`. The
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
- Never giving up is for failures a retry can fix. A relaunch its
  pre-flight refuses (the release is for another machine; its command
  is not there or not in the view) is not one: the watchdog drops the
  instance (`dropIf`: nothing current, nothing routed), records the
  refusal as `last_failure`, logs it once and parks, as recovery gives
  up on the same refusal. A deploy, or a reload's recovery once the
  cause is fixed, launches the app again; `state.json` keeps the
  version, so a restart meets the same refusal.
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
| A request cancelled before or during `pre_start` does not stop it: the migration runs to its end, then the deploy starts the new instance and the health gate fails it on the cancelled request (phase `soaking`, `deploy canceled while waiting for health`). The new instance runs its startup for that moment, the release is removed, and a retry runs `pre_start` again, as it does after any gate failure that follows a `pre_start`. | `deployLocked`, after `pre_start` | 3 | 3 | 1 | 9 | size 2, risk 1 (a request check before the start); size 3, risk 3 (run the rest of the deploy detached) | Rule 2 holds (it ends red, the cause named); under rule 3 the product is under 20 and neither fix is size 1, risk 1. The shape is the owner's decision (#295, 2026-10-17): the gate aborts the deploy as it does for any client that left. The old instance serves throughout, and a migration that cannot run twice already meets the same retry after any gate failure. |
| A watchdog restart whose command cannot be looked up at that moment for a reason a retry would fix — its runtime under `/usr` (`/usr/bin/node`) missing while its package is removed and reinstalled, or the lookup failing with an I/O error or a passing `EACCES` (`resolveInView` reports every `exec.LookPath` error as a refusal) — is refused at the pre-flight as permanent: the app stays down, unrouted, until a deploy or a reload launches it. | `handleFailure`, a pre-flight refusal | 4 | 3 | 1 | 12 | size 2, risk 2 (retry a lookup refusal under the budget, a branch on the refusal's kind) | Rule 2 holds (it ends red and named: the journal and status carry the lookup's error); under rule 3 the product is under 20 and the fix is not size 1, risk 1. An upgrade never opens the missing-runtime window (dpkg replaces a file by renaming over it), and restart recovery has always given up on the same refusal. |
| A health-failure restart stops the instance — unhealthy, but serving — before its relaunch's pre-flight runs, so a release made unrunnable meanwhile (a reload's edited `command`, an executable the app rewrote) is refused only once nothing serves: the app is down until a deploy or a reload launches it. | `handleFailure`, the stop before `launchVersion` | 4 | 4 | 1 | 16 | size 3, risk 2 (prepare the launch and run its pre-flight before the stop) | Rule 2 holds (it ends red and named: the refusal in the journal and in `last_failure`); under rule 3 the product is under 20 and the fix is not size 1, risk 1. The order predates the pre-flight on relaunch: before it, the same stop was followed by a relaunch that failed at every step. |
| A reload that removes an app waits for a deploy of it in flight (`Destruct`), and every config change after it waits too: a caller with a deadline of its own (`systemctl reload`, the box applier) can give up first though the reload then completes, and a push upload from a client that stalls without closing holds it until the connection ends, since no bound of hotserve's covers a transfer. | `Destruct`, the blocking `Lock` | 4 | 3 | 1 | 12 | size 3 (`Destruct` cancels the deploy's transfer; or a time bound on uploads, a new limit) | The shape is the owner's decision (#297): the wait is what keeps a removed app's deploy from starting a unit nothing stops. It is named in the journal; under rule 3 the product is under 20. A bound on uploads could fail a legitimate deploy and wants its own sizing. |
| A recovery that gives up records its reason (`recover`) after `ensureRunning` has let go of `deployMu`: a deploy that succeeds in that moment can be followed by the stale reason in `last_failure`, beside a running instance, until the watchdog's first healthy probe of it or the next launch. | `recover`, `setLastFailure` | 2 | 2 | 3 | 12 | size 2 (record the reason at each permanent return, under the lock) | Under rule 3 the product is under 20. It takes a deploy landing within the microseconds between the two, and status shows the instance running beside the reason. |
| A deploy request the old config was still serving when a reload removed its app, reaching the deploy lock after `Destruct` took it, gets a 409 ("a deploy is already in progress") rather than the answer for an app that is gone. | `Destruct`, the lock kept | 2 | 2 | 3 | 12 | size 2 (a removed flag with its own refusal) | Under rule 3 the product is under 20. Only a request already inside the old server as it shut down can meet it (a slow authentication, an HTTP/2 stream opened before the GOAWAY), and the retry the 409 invites reaches the new config, which answers for the app truthfully. |
| A watchdog restart that waits for a deploy holding the lock keeps the budget slot it claimed before the wait: a wait longer than `watchdog_window` lets the slot slide out before the restart happens, so that restart goes uncounted and a crash loop right after gets one restart more than the budget. | `handleFailure`, the wait for the lock | 2 | 2 | 3 | 12 | size 2 (re-stamp the slot once the lock is taken) | Under rule 3 the product is under 20. It takes a deploy longer than the window (ten minutes by default) over a dead instance, and costs one restart per such deploy. |
| A `pre_start` that exits within milliseconds of `deadline` can still read as timed out: `jobResult` prefers a job result already received when the context ends, but not one whose signal is still in flight. The deploy fails red naming the deadline, and the release of a migration that ran is removed. | `jobResult` | 2 | 3 | 1 | 6 | size 3 (a second completion source: a oneshot's transient unit is unloaded on success, so no `Result` is left to ask for) | Rule 2 holds (red, named); under rule 3 the product is under 20. A migration that ends on its deadline has used its whole budget; raising `deadline` is the remedy. |
| A Debian 13 host that passes the tutorial's step 2 (apt's `systemd (>= 257)` floor, `systemd-detect-virt --container`) but whose kernel or container refuses the sandbox's namespaces installs and serves the starter config, and first fails at step 3: `hotserve init`'s reload is liveswap's first start. That reload is refused, the probe's error names the cause, and the starter keeps serving; the box's repository, signer and workflow carry over to a fixed or new host. | `probeSandboxCapability`, from `App.Start` | 2 | 5 | 1 | 10 | size 3, risk 1 (a sandbox-check subcommand that step 2 runs as the hotserve user) | Rule 2 holds (it ends red and named); under rule 3 the product is under 20 and the fix is not size 1, risk 1 (#274, 2026-10-17; #225 scored the same case at occurrence 3, but a host is the operator's choice, so it scores as input). The probe needs the hotserve user and its lingering manager, which the package's postinst creates, so the earliest any check could run is the end of `apt install`, minutes before step 3, with the same refusal and the same fix. A check in postinst could only warn, since a failed postinst leaves the package half-configured: one line in apt's output, after up to two 30-second probe budgets inside dpkg. And liveswap with no apps runs no probe (`len(a.managed) > 0` in `App.Start`), so the starter config cannot carry one. |
| Releases with no deploy record that proves itself one (deployed before records existed, or whose record write failed) sort after every recorded release, ordered among themselves by their dirs' mtimes. A compromised instance running from one of them can set its own dir's mtime, keep its release, and have another unrecorded one pruned first. No recorded release is affected, nor the two GC holds by rank (`TestReleaseOrderLedger`, "the fallback's limit"). | `newestFirst`, state.go | 2 | 2 | 3 | 12 | size 3 (a birth-time key, with its own fallback where a filesystem reports none) | Rule 2 holds. Under rule 3 the product is under 20 and the fix is not size 1, risk 1. What moves is only which of the releases already first in line for pruning goes first, and each is named in the status (`ordered_by_mtime`) and in the GC line (`placed_by`). It needs two unrecorded releases on disk past the held pair plus a compromised app running from one of them. On a box with records since its first deploy that means two failed record writes as well (occurrence 1). Sorting these releases last is the owner's decision (#299, 2026-10-18): it leaves no app-writable input deciding a prune among recorded releases. |
| A deploy that finds no instance in memory (the watchdog dropped one whose relaunch was refused for good, `dropIf`, or recovery has not launched the app) gives GC no replaced version to hold by rank. If the version that last served came in by a rollback whose record write was lost, its record is at an older deploy, and with `keep 2` that GC can prune it. | `deployLocked` → `gcReleases`, `previous` | 3 | 2 | 3 | 18 | size 2, risk 2 (fall back to `state.json`'s last-served version) | Under rule 3 the product is under 20 and the fix is not size 1, risk 1. It takes two independent faults: the lost record write, and no instance in memory at the next deploy. With `keep 3` or more, that version stays among those kept unless newer ones were deployed since. The GC line names what it prunes (`placed_by`). |
| `max_token_ttl {env.NAME}` in a `deploy_trust local` block is refused at config load (`invalid max_token_ttl "{env.NAME}"`), though the README says `{env.*}` placeholders are resolved at config load: the duration is parsed when the Caddyfile is, before placeholders are. | `deploytrust.Parse` | 2 | 5 | 1 | 10 | size 2, risk 2 (a string field, resolved in `ResolvePlaceholders` and parsed in `Build`) | Rule 2 holds (refused and named, before anything runs); under rule 3 the product is under 20 and the fix is not size 1, risk 1 (#296, 2026-10-17). Every liveswap duration directive (`health_interval`, `deadline`, …) parses the same way, so this one matches them; the Caddyfile's own `{$NAME}`, substituted when the file is read, works for all of them. |
| An app dir, or a dir it makes, that is a link to a sibling's (`<root>/blog -> shop`, `blog/proxy -> ../shop/proxy`) is still followed outside a deploy. blog's status lists shop's releases as its own available versions, and so does the status a refused deploy's response carries. blog's recovery reads shop's `state.json`; it and the watchdog's relaunch make and remove a run dir in shop's `run/` and are refused at launch for a linked app dir, `releases/`, `shared/` or `run/`, recovery retrying, backing off to once a minute. A linked `proxy/` launches: blog's pins land in shop's `proxy/`, where shop's sweep removes them, so blog, not shop, goes unreachable. | `status`, `ensureRunning`, `launchVersion` | 3 | 5 | 3 | 45 | size 2, risk 1 (`writeRefusal` in status and before a relaunch, #300) | Scores to fix (rule 3); held by decision (#300, 2026-10-18: the deploy side is fixed, the rest held) until a box runs a link under the root, or renaming or aliasing an app becomes a feature. No box does. Nothing here writes into the sibling's releases or data: a deploy, push or rollback through such a dir is refused before it writes (`writeRefusal`), so a rollback to a listed version is refused and named; a relaunch through a linked app dir, `releases/`, `shared/` or `run/` is refused at launch (`resolveBindSources`), red and named; and the socket prune never follows a link (`linkAt`), so no sibling loses its socket and pin that way. |
| A push refused before its upload is staged (a dir that is not the directory it names, like a 413 or a staging failure) leaves `last_deploy` as it was, where a URL deploy or rollback refused the same way replaces it. | `deployPush` | 2 | 5 | 1 | 10 | size 2, risk 2 (a push refusal through the pipeline's bookkeeping) | Under 20, and not a size-1, risk-1 fix (rule 3). The push's own response is the refusal, a 500 naming the path, the reason and the remedy, and the journal says it with who asked (`deploy failed`, `via`, `remote`). Every refusal of a push before staging already leaves `last_deploy` alone, so a status read after a push tells the same story for each. |

**Owed** (over the bar, not yet fixed):

| Finding | S | O | D | Where the fix lands |
|---|---|---|---|---|
| A refused token's timing names configured apps: a forged token under an unknown kid for a configured app waits on its issuer's key fetch (at most once a minute per app), where one for a name that does not exist is refused at once when the global sources hold no OIDC source (Behavior specification, the webhook's timing). | 3 | 5 | 5 | #306 |
| A bind mount can give two apps one directory, or put one app's data inside another's: an fstab source another app also uses, or a whole app dir mounted at another's. A mount resolves to itself, so the launch check (`resolveBindSources`) passes it: blog launches on shop's data, writable, and nothing says so. | 5 | 5 | 5 | #308, to design: equal `(st_dev, st_ino)` alone misses a source nested in another's; when to check, what to refuse and what to say are open there. Until then, the README's bind-mount advice says the source must be the app's own |

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
