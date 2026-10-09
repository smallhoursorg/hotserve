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
liveswap's `systemdRunner` (liveswap/runner_systemd.go). Multi-node,
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
3. **Admin API socket** — `/run/hotserve/admin.sock` (the `admin`
   directive, packaging/Caddyfile); reconfigures the whole server.
   Gated on being the `hotserve` user, not on the network.
4. **Per-app secrets** — an app's own env vars / `env_file`
   (`/etc/hotserve/*.env`). Legitimately reachable by that app; the
   goal is to keep them from *siblings*.
5. **The box's configuration at rest** — `/etc/hotserve/Caddyfile`:
   which repository may deploy each app, each app's command and
   flags, which hosts are served, and who may change the file itself.
   Root-owned, 0644; written only by root — `hotserve init`, the `box`
   applier, the console ([box/DESIGN-box.md](box/DESIGN-box.md)). A
   writer here outlasts a restart, which is what makes it an asset
   distinct from the admin socket (whose changes do not).
6. **Sibling app data** — `/var/lib/liveswap/<app>/{releases,shared,state.json}`.
7. **System integrity** — root, persistence, other system services.
8. **Availability** — serving traffic and the deploy pipeline.

There is no deploy secret on the box. Deploys are authenticated by a
verified JWT (CI OIDC or a local public key); the verifier holds only
public material — see "Reducing the asset".

## Trust boundaries and entry points

Source is cited by symbol, not line: `Type.Method` or a package-level
name, with the file it lives in named on a passage's first citation —
later symbols are in that file unless another is named. Where a claim
rests on one statement inside a long function, the prose names it so it
can be found by search.

### Webhook endpoint — `liveswap/handler.go`

The first of two application-level authenticated entry points (the
other is `box_webhook`, "Config webhook" below). Auth is a
verified JWT in `Authorization: Bearer` (see "Reducing the asset"):
`deploy_trust` sources verify the token's signature and standard claims
against public material, then a claim allowlist. Auth happens **before**
existence is revealed: an unknown app name is verified against the
*global* trust sources so callers cannot enumerate app names
(`Handler.ServeHTTP`, liveswap/handler.go). Config load refuses an app
that resolves to zero trust sources (`App.Validate`,
liveswap/liveswap.go). Bearer is the only transport, which Caddy
redacts from access logs.

Properties that matter to the model:

- **GET and POST both sit behind the token.** GET returns full status,
  POST deploys, all else 405 — the method switch in
  `Handler.ServeHTTP` (liveswap/handler.go). The status endpoint is
  authenticated — not public.
- **What the webhook says back is filtered** (liveswap/redact.go,
  which states the filter's four rules; every body passes it —
  through `respondJSON` in liveswap/handler.go, the record route's
  `respondFiltered`, the stream's lines, the record store's write).
  The
  response's audience is wider than its caller: the paved road prints
  it into a GitHub Actions log, readable by everyone with read access
  to the repository, retained, and public for a public repository.
  The caller is trusted to run code on the box; the log's readers are
  not. A failed deploy's body carries the app's own bytes on purpose
  (liveswap/journal.go, `deployDetail` in liveswap/app.go): the last
  lines its units wrote, read back with `journalctl` — the one
  external program hotserve runs, with argument lists of its own
  making — bounded by `deploy_log_lines` (default 40) and 8 KiB; the
  first 512 bytes of a failing health probe's body; and the exit status
  the runner recorded. A line is the units' when journald named one of
  them on it, or when it is in one of their output streams —
  journald's `_STREAM_ID`, which no client can set and only the unit's
  processes hold — learnt from the lines that name a unit and from
  those written under the app's identifier by the main process of the
  `pre_start` or app that failed, found by its pid (as the manager
  recorded it, or as the runner follows it while it runs), which
  another process could hold only once pids wrap. `deploy_log_lines 0`
  keeps the app's bytes — the tail and the probe body both — on the
  box; the exit status and the probe's status code are hotserve's
  observations and stay. Reading
  the journal is a grant: journald keeps a system user's output in the
  system journal, so the packaged unit puts hotserve's process in
  `systemd-journal` (`SupplementaryGroups=`, the process and not the
  account, so the apps under the user manager keep the account's
  groups and see no journal in their sandbox anyway). What that
  widens is what a compromised hotserve reads — every unit's lines —
  which the shipped units keep free of secrets (no `--environ`, the
  smoke test asserts it). Every body passes
  four layers before it is written. A streamed deploy (`Accept:
  application/x-ndjson`) writes two kinds of line, each through the
  same filter before it is written: phase lines, generated objects
  holding a phase name and a timestamp and nothing of the app's; and
  the last line, the single response's body filtered exactly as it
  would have been, with two fixed fields — `"event":"done"` and
  `http_status` — appended after the filter so that a body the filter
  withholds still ends the stream with them. First, exact: every `env_file`
  value of 8+ characters this process has rendered for a launch (or,
  after a restart, read from the file for the filter), in each form
  the filter recognises (as written, JSON-escaped, base64 standard and
  URL alphabets padded and not, hex, URL-escaped, and a URL value's
  password on its own), becomes `[redacted:KEY]`, and the body gains
  `redacted_env` naming the keys. hotserve is the party that knows
  these values, which is why the filter lives here and not in CI,
  where `::add-mask::` can only hide what the job itself knows.
  Second, an allowlist: the versions the status names and the app's
  own paths are exempt from the heuristics below — never from the
  first layer: a safe string equal to a known value is dropped,
  whoever named it (a request, the config, a name read off disk), so
  a version, an app name or a path spelled as an `env_file` value is
  redacted like the value. Third, shape rules for
  credentials with a recognisable form. Fourth, entropy: a run of 20+
  characters from the base64 or hex alphabet, mixed and with Shannon
  entropy above the class's bar, is replaced by `[masked, N chars]`,
  never by a fingerprint. Markers are chosen so that none contains a
  known value (a key named for its own value gets a number in
  brackets), and the first layer runs once more over the finished
  bytes — any fallback included — so the promise holds for the bytes
  written, not only for the body as filtered; the safe strings are
  held out of the heuristic layers as spans, so a version shaped like
  a token survives them. The one text outside the promise is the
  report field's own name, `redacted_env`: it is fixed, in every such
  response, and so reveals nothing; its key names are filtered. The
  other is the outcome vocabulary — `succeeded`, `failed`, the phase
  names — where it stands as a `status`, `phase` or phase `name`: a
  known form that is a bare word (a value, or a URL value's password,
  spelled as one) is replaced everywhere but in such a pair, in every
  body, so a value spelled as one of the words rewrites no outcome.
  Every other form is replaced wherever it is; a body with fewer
  such pairs at the end than it had — a value that is part of a word
  was replaced inside it — is withheld; and the same word in `error`
  or `detail` is text, and filtered. A
  body the first layer would leave unparsable (a value made of JSON's
  own punctuation matching the structure rather than a string) is
  withheld, with the keys still reported; and while an app's
  `env_file` cannot be read, its values are unknown to the filter and
  every body about that app is withheld rather than sent through the
  heuristics alone. What the filter does **not** promise: an encoding not in
  the list, a value under 8 characters, a secret split across lines,
  a secret the app fetched at runtime and printed in a low-entropy
  form, an inline `env` value (not a secret by policy: the Caddyfile
  is in a repo), or a value the still-running instance was launched
  with before a hotserve restart if the file has changed since.
  Tested from both sides — a fuzz property that no known value or
  form survives and the JSON stays valid (`FuzzRedactor`), and a
  corpus that the diagnostics a reader needs do (`TestRedactorCorpus`)
  — because a filter with only the first test drifts toward blanking
  everything.
- **Auth failures are throttled in the journal**
  (liveswap/authlimit.go). Token *forgery* is infeasible (no private
  key), so this is not a guessing oracle; what
  an unauthenticated caller can do with failures is make hotserve
  write a Warn per request. Two sliding windows bound that, in one
  lock scope so a burst of concurrent requests cannot each see room:
  per address, 10 failures a minute, after which further bad tokens
  get 429 until the oldest ages out; process-wide, 100 failures a
  minute are logged however many addresses a flood comes from. The
  address budget decides the 429; the process budget decides what is
  logged, the once-per-window "budget spent" lines included — so
  under a spent process budget an address is throttled silently. The
  logged `app` field is cut to
  what a real name could be. The address is Caddy's `client_ip`
  (`trusted_proxies` honoured), IPv6 keyed by /64; the table is bounded
  at 4096 addresses (drained ones swept at most once a window, then an
  arbitrary one dropped), and the global budget is what holds above
  that. The token is still verified for a throttled address and a
  valid one is admitted (and clears the address): a NAT, a CI egress
  pool, a proxy without `trusted_proxies` or a unix-socket listener all
  collapse clients onto one key, and the throttle must never be a
  deploy-denial primitive for whoever shares it. Verification cost is
  therefore not bounded by the throttle — it is comparable to the TLS
  handshake the request already paid, so the throttle is not what
  bounds CPU. One limiter for the process, not per handler, so a
  reload hands out no fresh budget and a second mount does not double
  it. Body is capped at 64 KiB → 413
  (`maxPayloadBytes`, enforced in `Handler.deployURL`,
  liveswap/handler.go); `deployMu.TryLock()` → 409 serializes deploys
  (`managedApp.Deploy`, liveswap/app.go; the push path takes it before
  staging, in `Handler.deployPush`, liveswap/handler.go).
- **Path routing is `path.Base(path.Clean(...))`**, the first statement
  of `Handler.ServeHTTP` (liveswap/handler.go): `/anything/deep/myapp`
  targets `myapp`. A naive `path /deploy/*` site matcher does not
  constrain the app name; the operator's matcher is the only constraint.
- **Three request shapes** (`Handler.deploy` dispatches on them,
  liveswap/handler.go), all behind the same token: a JSON body pulls
  from a URL (below); a gzip body **pushes** the artifact itself
  (`POST /<app>?version=`, `Handler.deployPush`) — the bytes come straight
  from the authenticated caller, capped at `max_artifact_size`, staged
  under the app's `tmp/` and extracted like a download, and **no
  `artifact_allowlist` is consulted** because there is no URL to pin;
  `?rollback=` relaunches an on-disk release. The push path has no
  SSRF surface, but it means a token-holder can deploy arbitrary
  bytes with no artifact host in the loop: the allowlist confines
  *pulls*, and only the claim scope confines *who*.
- **Pull payload:** four fields only — `url`, `version`, `auth_header`,
  `sha256` (`deployPayload`, liveswap/app.go); unknown JSON silently
  ignored (no `DisallowUnknownFields`). `version` is
  `^[A-Za-z0-9_-][A-Za-z0-9._-]{0,63}$` (no leading dot, so never
  `.`/`..` or a release-GC bookkeeping name), double-sanitized before
  touching the filesystem (`versionRe`, `versionPathComponent` and
  `validVersion`, liveswap/names.go). `auth_header` is only
  control-char-checked (`parseDeployPayload`, liveswap/handler.go); its
  contents are attacker-chosen and forwarded to the allowlisted host.
  `sha256` is optional; when present it is exactly 64 hex characters,
  lowercased (`parseDeployPayload`), and the download must hash to it
  (below).
- **Response leaks (all post-auth):** the 500 path returns raw
  `err.Error()` plus the full status snapshot
  (`Handler.mapDeployResult`, liveswap/handler.go) — filesystem paths,
  tar entry names, the operator's allowlist echoed verbatim
  (`artifactAllowEntry.String` and `describeAllowlist`,
  liveswap/allowlist.go). The status snapshot exposes the app's
  **socket, PID and the argv its unit is running** — the last read back
  from the manager's `ExecStart` by `propExecStart`
  (liveswap/systemd_dbus.go), so a credential the operator passed as a
  command-line flag is echoed there — and watchdog cause/failure state
  (`managedApp.status`, liveswap/app.go; `watchdogState.statusSnapshot`,
  liveswap/watchdog.go). The environment is deliberately not in the
  snapshot: it is the place secrets are meant to go, and unlike the argv
  it answers no question about what is running. Artifact URLs *are*
  redacted before logs/errors (`redactURL`, liveswap/download.go), so
  query signatures do not leak.
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
(`App.Validate`, liveswap/liveswap.go); no any-origin mode. Pinning
(`artifactAllowEntry.pinnedURLString`, liveswap/allowlist.go) rebuilds
the outgoing URL so scheme is constant, host/port/path-prefix come from
*config bytes*, and only the path suffix + query come from the payload
— the request can never contribute host bytes, with two fail-closed
re-checks (port, then query) and a prefix-boundary guard, all three in
`pinnedURLString`. Canonicalization (`canonicalEscapedPath`) and query
gating (`artifactAllowEntry.vetQuery`) are thorough and
closed-by-default.

`https` only unless `allow_insecure_http` (`downloadArtifact`,
liveswap/download.go), re-enforced on **every redirect hop** by the
`CheckRedirect` closure in `newDownloadClient` — closing Go's default
https→http downgrade. `auth_header` is stripped by stdlib on cross-host
redirects but **not** same-host (also `downloadArtifact`). Size:
Content-Length pre-check plus streaming `LimitReader`, default 100 MB
(`downloadArtifact`; the default is set in `AppConfig.applyDefaults`,
liveswap/liveswap.go).

**Content binding is the deployer's choice.** A pull that carries
`sha256` is hashed as it streams (`downloadArtifact`), and a body that
hashes to anything else is refused as a `validationError` — a 422
naming the pinned and the served digests — with the staged file
removed; the cap is judged first, so a cut-short body reports the
cap, never a mismatch. The pin is the deployer's own hash of the
tarball it built, so with it a host can only withhold the artifact,
not substitute one. Without it the fetched bytes are trusted on the
allowlist and the token alone. The example workflows and the README's
CI recipes send it; a push (bytes from the authenticated caller) and a
rollback (no fetch) have nothing to pin.

**The documented, real gap:** the host allowlist governs the **first
hop only** — `CheckRedirect` deliberately does not re-check the host
per hop, because the GitHub→S3 redirect is load-bearing (the rationale
is on `newDownloadClient`, liveswap/download.go). So an allowlisted first
host can redirect the fetch to *any* https host — LAN, internal, an
https metadata endpoint. "https-only" is a partial SSRF barrier (it
stops plain-http metadata endpoints, not an attacker's https target).
Reaching it requires a valid deploy token **and** an allowlisted first hop.
Secondary: a malicious host can trickle bytes under the cap to hold the
per-app deploy lock open — DoS-of-deploys, not of serving. The stages
before the body are each bounded on their own (30 s connect, 10 s TLS
handshake, 30 s to response headers, set in `newDownloadClient`), so
the stall has to be in the body, which is bounded only by the caller
keeping its request open: the webhook request context carries no
deadline, so a CI job's own timeout is the bound on that trickle.

### Tar extraction — `liveswap/extract.go`

Two-pass (validate-all, then write — no partial residue,
`extractArchive`, liveswap/extract.go). Traversal via stdlib
`filepath.IsLocal`, which both passes reach through `safeRelPath`.
Symlink/hardlink targets must resolve inside the archive root
(`linkTargetStaysInside`). Modes: `Perm()|0600` strips
setuid/setgid/sticky structurally (`rootWriter.write`); dirs forced `0750`.
Devices/FIFOs rejected. Decompression bomb capped at
`max_artifact_size × 10` over the *decompressed* stream
(`decompressionRatioCap`, enforced by the `io.LimitedReader` in
`walkArchive`).

Containment is enforced twice, because the string checks above are
not the filesystem's. Once an earlier entry's symlink exists on disk,
a later name or link target that passes *through* it lands wherever
the link points, and a chain of links that are each inside as strings
(`l1 -> .`, `l1/x/y -> ../..`, `l1/x/y/z/w -> ../..`, then a regular
file or hardlink under the last) climbs out one directory per hop —
into the app dir, a sibling app, the supervisor's own state, or the
user manager's unit directory, as the `hotserve` uid. So the kernel,
not a model of it, judges what resolves where. The write pass goes
through an `os.Root` (`writeArchive`): every mkdir, open, symlink and
link is resolved by the Root's own walk, which refuses any hop that
leaves the staging dir — the chain above fails at its third hop, named
by entry, with nothing written outside. Then
`checkLinksResolveInside` walks the extracted tree and resolves every
symlink through the same Root (`Root.Stat`), refusing any whose
resolution leaves it: a `..` that climbs out through a link to `.`
(`a/..` is `.` as a string), a hard link to a symlink (that symlink's
relative target re-based to a new directory, which the walk sees as a
symlink), a target that leaves and comes back in by the staging dir's
own name (it would dangle once the tree is renamed into place), in
whichever order the archive listed them. Anything else the Root cannot
follow is refused too, a loop included: the Root gives up after 8
symlink hops where the kernel allows 40, so a link it calls
unresolvable may still resolve — outside — for a reader that is not
the Root (the price is that a chain of more than 8 inside links is
refused too, worded as a loop). A dangling target (a component that
does not exist, or a file where a directory was needed) is the one
thing accepted without resolving: the walk reports an escape ahead of
a gap, so everything up to the gap resolved inside, and what the gap
becomes later is the running app's business — its release is
writable, and an app that wants an outward link plants one, which is
why the supervisor trusts no link under an app dir (the one it
resolves, the app command, is refused unless it lands inside the
sandbox view). A
refusal leaves the partial tree in the staging dir, which the caller
removes as it does for any mid-write failure (`releaseFetcher.fetch`,
liveswap/download.go).
Links that resolve inside — a venv's `python -> python3 ->
python3.12`, a `.bin` link into a sibling directory — extract as
before. `TestWriteArchiveRootRefusesEscape` and
`TestWriteArchiveRootRefusesHardLinkFromOutside` exercise the write
pass with validation bypassed; `TestExtractRefusesLinksResolvingOutside`
the resolution check.

Residual items for the model:

- **Entry and name caps, independent of the byte budget.** The byte
  cap bounds the tar *stream*, not what extraction consumes: every
  entry costs an inode and most a 4 KB block, so 1 GB of 1-byte files
  is ~1M inodes and ~4 GB of blocks — a 20 GB ext4 root has ~1.3M
  inodes — enough to take the box to ENOSPC for ACME storage,
  `state.json`, the journal and every other app's next deploy, and it
  persists under `keep`. `max_artifact_entries` (default 100000; a
  CI-built artifact is thousands) counts the filesystem objects
  extraction creates — the parent directories an entry implies
  included, so one deep name cannot smuggle a thousand — and is
  rejected in the validate pass, so nothing is written. Names and link
  targets over PATH_MAX once joined under the release dir, or with a
  component over NAME_MAX, are refused the same way. The content the
  entries *declare* is capped like the stream: a GNU sparse entry's
  holes are synthesized by the reader from no stream bytes, so the
  stream cap alone would let one small entry write a disk of zeros.
  The byte budget is the *archive's*, not each entry's: a file may be
  nearly all of it, which is the shape of a single-executable artifact
  — `examples/node` ships two `node --build-sea` binaries, ~300 MB
  unpacked, under the 1 GB default. A per-entry ceiling would have to
  sit above the largest binary an app may ship, and there it bounds
  nothing the archive budget does not; the shape that costs more than
  its bytes is many small entries, and that is what
  `max_artifact_entries` refuses.
  A deploy reports the figures and warns past 75% of
  either cap. The archive is still read **twice**, so the byte cap
  permits 2× the CPU — bounded, and cheap next to the write pass.
- `+x` survives extraction — intended (the artifact ships the app
  binary), but it is the point where artifact bytes become code.

### Reverse proxy, admin socket, penaltybox

The starter config exposes only `:80` returning a static string; the
entire liveswap/webhook block ships commented out
(packaging/Caddyfile) — a fresh install has no deploy endpoint. Admin
is off TCP, on `unix//run/hotserve/admin.sock` (the `admin` directive,
same file), `RuntimeDirectoryMode=0750` owned by the service user
(packaging/hotserve.service) — so admin access is gated on *being the
`hotserve` user*. Every deployed app is that user, but a sandboxed app
cannot reach `/run/hotserve` at all (the path is not in its view), so
the gate holds against apps and only hotserve itself can connect. The
commented-out `deploy.example.com` example is a public TLS vhost with
`liveswap_webhook` behind `deploy_trust`; the handler's own per-address
auth-failure throttle is the only rate limiting in front.

**penaltybox** is a response-phase rate-limit-hint enforcer; it touches
untrusted input only narrowly (the client key defaults to
`{http.vars.client_ip}`, deferring XFF trust to `trusted_proxies` — the
`Key` field of `Handler`, defaulted in `Handler.Provision`,
penaltybox/penaltybox.go). A misconfigured `trusted_proxies` turns
client bytes into store keys, bounded by `max_keys` (default 100 000,
idle-eviction). The origin's hint header is strict-parsed and fails
open. It is a minor surface: denial-of-protection under bad config, not
injection or exhaustion.

### Supervisor⇄app and app⇄app boundaries

Every app runs as `hotserve`, in its own user namespace and its own PID
namespace, with a deny-by-default filesystem view — see "The shared-UID
rule" and "The shipped mechanism". The network namespace is shared, by
design. The unit environment is the user manager's defaults
(`INVOCATION_ID`, …; `XDG_RUNTIME_DIR` and `DBUS_SESSION_BUS_ADDRESS`
are unset because `/run/user` is not in the view) plus an allowlisted
slice of hotserve's (`PATH`, `LANG`, `TZ`, `LC_*` — `envAllowlist`,
liveswap/app.go; `HOME` is never inherited, `buildEnv` sets it to the
app's shared dir) — closing *direct*
inheritance of ACME tokens and any other supervisor secret
(`TestBuildEnvDoesNotLeakSupervisorSecrets`). The `/proc` route is
closed twice over (non-dumpable supervisor; cross-namespace refusal);
the filesystem routes are closed by absence.

**Where `env_file` values sit once the app runs.** hotserve reads the
file itself and hands the pairs to the manager as the transient unit's
inline `Environment=` property (`unitProperties`,
liveswap/systemd_dbus.go), never `EnvironmentFile=`. From then on the
`hotserve` uid — the trust domain that read the file — holds them by
four routes, with a host-dependent fifth, and nothing else holds them
by any:

- **The unit's property**, for as long as the unit exists:
  `systemctl --user -M hotserve@ show -p Environment <unit>` (as that
  uid or as root; a bare `--user` targets the caller's own manager). A
  key that inline `env` or the injected `SOCKET` also sets is not
  here: `buildEnvFull` (liveswap/app.go) emits both and the manager
  keeps the last.
- **The transient unit file** the manager writes under
  `/run/user/<uid>/systemd/transient/`, which carries the un-merged
  `Environment=` line — overridden values included — until the unit
  is garbage-collected.
- **The running app's `/proc/<pid>/environ`.** The user-namespace
  closure measured on the spike ("The shared-UID rule", below) runs
  the other way, app to supervisor; this direction follows from the
  same `ptrace_may_access` rule, since the app's namespace is owned by
  the hotserve uid, which holds `CAP_SYS_PTRACE` in it. Not measured.
- **hotserve's own memory**, which keeps every value it has read — for
  a launch, or for the response filter, which reads the file without
  one (`managedApp.secrets` and `redactorFor`, liveswap/app.go) — for
  as long as the app stays configured, behind the non-dumpable floor.
- **A core dump, where the host writes one.** Nothing makes an app
  non-dumpable (the floor under "A non-dumpable supervisor is the
  floor" is the supervisor's own) and no unit sets `LimitCORE=`, so an
  app crash can write its memory, values included, past the unit's
  life: under the kernel's default pattern into the crashing process's
  working directory — the release dir unless the app moved, anywhere
  writable in its view if it did; under systemd-coredump into its
  store, with an access entry for the uid. Same uid and root. Closing
  it (`LimitCORE=0` on app units) is a code change, not made here.

No account gains a read it did not already have: the file is
root:hotserve 0640 by the tutorial's install line (nothing in hotserve
enforces that), `/run/user` — the manager's socket and the transient
file alike — is outside every view (`sandboxNeverReachable`,
liveswap/sandbox.go), and the PID namespace hides the process from
siblings. Stated because the manager's two copies are not obvious from
the file's mode. `LoadCredential=` delivery would take the values out
of the unit's properties and environment, not out of the uid's reach,
and what it closes under this sandbox is unmeasured: an open question,
not a plan.

### Config webhook — `box/`

The second authenticated entry point, and the only one whose outcome
is a root-owned file. [box/DESIGN-box.md](box/DESIGN-box.md) is
authoritative for its behaviour; this section places it. Status:
designed 2026-10-09 and being built in PRs. Until the applier ships,
config reaches the box the way `examples/box/README.md` describes:
root hand-edits `/etc/hotserve/Caddyfile` on day 0, and afterwards a
laptop script (`examples/box/bin/push`) stages, validates, swaps and
reloads it over SSH as an administrator whose sudoers file
(`examples/box/sudoers`) allows exactly those eight commands plus two
for the per-app env files — a root `install -m 0640 -o root -g
hotserve -T /dev/null /etc/hotserve/<app>.env` that creates one, and
`sudoedit` of it. This section describes what replaces the first
eight.

The operator's config repository is the only writer of
`/etc/hotserve/Caddyfile`. Its workflow mints an OIDC token and POSTs
a bundle — the Caddyfile, the raw commit object of `HEAD`, the tree
objects on the path to the file, and the first-parent chain back to
the commit the box runs — to `box_webhook`, a site directive beside
`liveswap_webhook`. Two processes stand between that request and the
file, and the boundary between them is the one that matters.

**The handler** (uid hotserve, in the serving process) verifies the
token against the `box` global option's `deploy_trust` through the
same entry point liveswap's handler uses — same 401, same budgets,
same `refused` line — so an unauthenticated caller learns nothing it
could not learn from a deploy webhook. It parses the bundle from
memory under fixed names and caps, binds the bundle's commit to the
token's `sha` claim, pre-checks the signature, runs `hotserve
validate` (and `hotserve-backup validate` where installed) as bounded
children with their output redacted, and drops the bundle as one
regular file for root. It answers 202 as soon as root has published
`verified`, which precedes the install; the outcome is polled, because
a request held across the reload deadlocks on the HTTP server's
shutdown.

**The applier** (`hotserve-box-apply.service`, root, one shot, started
by a path unit watching the drop directory) trusts nothing the
handler did. It reads the bundle once into memory and never from disk
again; takes the signer list from the installed file's raw tokens
(never from the running configuration, which the hotserve uid
authors); requires the incoming file's `box_webhook` host to equal the
installed one and the bundle's path to equal the path `init` recorded
(the box's identity and which file is its: another box's equally
signed file, or another file for this host elsewhere in the same
tree, is refused); verifies the SSH signature with `ssh-keygen -Y` as uid
65534 against that list, on `HEAD` and on every first-parent commit
above the recorded baseline (an unsigned commit would otherwise ride
in under the next signed one); proves the file is the committed file
(commit, tree and blob hashes); requires first-parent descent from the
recorded baseline; refuses a file that drops the `box` block, every
signer, the `box_webhook` site or the key that signed it; and only
then writes, reloads through `systemctl reload hotserve`, and rolls
back — reloading again — on failure. It never runs Caddy's validate or
adapter: both execute module code on the input and expand `{$VAR}`
from root's environment.

What this closes, once shipped. The `HOTSERVE_CONFIG` half of the
administrator's sudoers grant — the eight commands that moved and
reloaded the Caddyfile — goes, and with it the only account that could
change config; the `HOTSERVE_SECRETS` half (a root `install` that
creates `/etc/hotserve/<app>.env` 0640 root:hotserve, and `sudoedit` of
it) stays until secrets ride the same channel, so
the administrator account itself remains until then. A leaked PAT, a
stolen GitHub session or an OAuth app with `contents:write` can push
to `main` and cannot produce the signature, so it cannot change the
box with anything a signer did not sign: its own commit is refused on
its own push and, because every commit on the chain is checked, cannot
be carried in by the next signed one (the merge button cannot either:
a squash is GitHub-GPG-signed, a rebase unsigned). What such a push
*can* do is stop every later push until it is rebased away — loud and
recoverable — and, the residual that remains, fast-forward onto `main`
a branch every commit of which a signer already signed: a signer's
own unmerged, signed configuration, which the box then applies because
it is exactly what the rule asks for. The box repository's README
therefore says that configuration not yet meant to run is committed
unsigned or lives in a fork; an unsigned branch is one the chain rule
refuses whoever lands it. A compromised action holding the
repository's OIDC token can present only commits a signer already
signed, cannot replay an older one — though replaying `HEAD` after a
console edit reinstalls HEAD's file and so reverts that edit, which is
the stated 3am contract — and cannot land a sibling
box's file.

What it changes for T5, honestly. The Caddyfile was root-owned before
`box` and the hotserve uid never wrote it, so "a supervisor RCE cannot
persist config" is not a gain — it held under `bin/push` too. The
delta is the other way: `box` adds a root process that consumes input
the supervisor controls — a tar reader, a commit and tree parser, a
path walk, an `ssh-keygen` invocation, lock and rename handling — and
what keeps T5 from reaching root through it is that the applier
re-verifies everything from the bytes, reads them once into memory,
parses under fixed names and small caps, runs `ssh-keygen` as uid
65534, trusts the handler for nothing but a well-formed bundle, and
refuses a Caddyfile with any `import` — an imported file is unsigned
bytes, and one under a hotserve-writable path would be T5's way to
make a change outlast a restart — including one spelled as a
placeholder: Caddy expands `{$NAME:default}` before it tokenizes, so
the applier refuses any `{$` at directive position, in a site address
or in the `box` block, and checks the file again after expanding
placeholders with an empty environment (box/DESIGN-box.md, "Reading
the signed file"). What the workflow is *told* remains the serving
process's word: the response and the result poll are served by
hotserve, so a T5 can lie to CI; it cannot change root's record or
root's file *itself*. What it can do through root is bounded by the
signers, not closed: the token-to-commit binding is the handler's
check, and root cannot repeat it (the token is not in the bundle, and
verifying one offline would mean a root unit fetching issuer keys or
trusting the handler's cache), so a supervisor RCE that drops a bundle
straight into `in/` can have root install any configuration a signer
signed that descends from the baseline and whose objects it holds —
`HEAD` again, which undoes a console edit, or a signer's signed but
unmerged branch. It cannot have root install anything a signer did not
sign; dropping an unsigned or self-authored file is refused outright.
Skipping the handler's validation, which root never repeats, is a
remaining T5 capability bounded by the signature, identity, proof and
descent checks: a signer-signed file that fails to load is rolled back,
one that loads but `hotserve-backup validate` would have refused is
installed. The box repository's
README rule that unmerged configuration stays unsigned is what keeps
that set to `main`'s own history.

What it does not close. `systemctl reload`'s exit status is the
hotserve uid's word; the `applied` phase trusts it, the file on disk
does not depend on it, and `systemctl restart` is the remedy. The
never-cut-the-branch guard checks presence, not reachability: a
`claim` typo passes it and strands the box until the console. The
handler's validate is a courtesy, not a boundary. Plain `crypto/sha1`
on the proof, not sha1dc. A signer's laptop is the operator; a
software-held signing key makes its compromise total for the box,
which a hardware-held (`sk-`) key reduces to "cannot sign unattended".

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
  deploy arbitrary code to every app that accepts that trust source for
  the token's lifetime — by pushing a tarball directly, which passes no
  allowlist, or by pulling from a URL within `artifact_allowlist` — and
  roll back versions. Containment is the claim scope (and, for pulls,
  the allowlist), not the runtime; the sandbox bounds what the deployed
  code then reaches.
- **T3 — Malicious/compromised artifact host** within the allowlist
  pin. Controls status, redirect targets (any https host), timing —
  and the tarball bytes, unless the pull carries `sha256`, which the
  example workflows do: then other bytes are a 422, and what remains
  is withholding. The pin is kept in the version's deploy record, for
  as long as the record is, unless the filter would redact it (it
  equals or holds an `env_file` value, or those values could not be
  read whole); liveswap/README.md, Deploy records, has the exceptions. Faces `extract.go` and the first-hop SSRF gap.
- **T4 — Unauthenticated network attacker** on the public webhook/proxy.
  Faces the token gate — forgery needs a private key, so there is no
  guessing oracle; the realer wins are log-amplification (bounded by
  the auth-failure budgets: ten failures a minute per address, then
  429, a hundred process-wide, then silence), the CPU cost of JWT/JWKS
  verification (spent on every request before the budget answers —
  the budget bounds the journal and the reply, not the work), and any
  pre-auth proxy/Caddy surface.
- **T5 — RCE in the supervisor itself** (a Caddy or liveswap bug). Low
  probability, catastrophic: it *is* the `hotserve` user, so it already
  holds every asset short of root. No app-isolation design prevents
  this; the design question is only how much *worse* it can get (does it
  reach root?) — which is why the supervisor holds no grant (see "The
  shipped mechanism").
- **T6 — Compromised identity of the box's config repository.** Three
  distinct holders, in descending likelihood: a credential that can
  push to the repository (a leaked PAT, a stolen session, an OAuth or
  GitHub App with `contents:write`); a compromised Action in that
  repository, holding its OIDC token — or, where the `box` block trusts
  a `deploy_trust local` key, whoever holds that key, who mints the
  `sha` claim themselves and so needs no workflow and no `main`; a
  compromised signer's laptop.
  The asset is the configuration at rest — asset 5, the one that
  survives a restart. The first can push and cannot sign: what it
  writes is refused on every later push too, until removed, and what it
  can land is only a branch a signer fully signed already (the README's
  rule against signed unmerged config is the answer); the second can
  present only what a signer signed, in order, for this box, including
  `HEAD` again after a console edit; the third is the operator, bounded
  only by a hardware key's touch. Faces `box/` ("Config webhook").

For **T2**: deploy-arbitrary-code (a push is contained by nothing but
the claim scope; a pull additionally by the allowlist); deliberate
rollback. For **T3**: archive-borne CPU (bounded by the byte and
entry caps; inode exhaustion is closed by the entry cap; a link chain
out of the staging dir is refused by the `os.Root` write pass and the
post-write symlink resolution check); first-hop→any-https SSRF. For **T4**:
log-amplification, bounded by the auth-failure throttle (online
*token forgery* is infeasible).
For **T5**: total, by definition — the containment question is
root-vs-not-root, and `box` adds a root process fed by the supervisor
to that question (the "Config webhook" section says what keeps it
closed). For **T6**: a push credential gets a red run and a stalled
`main` until its commit is removed, or a signer's own signed branch
landed early; an OIDC holder gets a replay of `HEAD` (a `no_change`,
or the undoing of a console edit), one bundle in `in/` at a time with
no age-out and one in root's hands, and the handler's validate on a
signer's file; a signer's software key gets the box.

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
   `CAP_SYS_PTRACE`, which apps never hold — the unit's
   `CapabilityBoundingSet=` is empty and `NoNewPrivileges=` stops an
   `execve` from acquiring any — with or without user namespaces.
   `liveswap/harden` is a leaf package
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
   a lesser sandbox. A probe made to fail, or to time out twice, keeps
   hotserve down until an operator starts it — a refused start exits 1,
   which the unit never retries; a start stretched past the unit's
   `TimeoutStartSec` is restarted by systemd, which repeats the
   exposure rather than ending it. Either way the bound is the same
   trust domain. The verdict is cached per manager connection
   (`userManagerClient.cachedSandboxCapability`,
   liveswap/systemd_dbus.go), which narrows the window, and a failed
   verdict is deliberately NOT cached, so interference costs the next
   start rather than pinning a verdict until the manager
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
   today — the trigger is an app that needs bounding (#71).

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

**The property set**, on every unit — `unitProperties` renders the
lifecycle properties and appends `sandboxProperties` for the rest
(liveswap/systemd_dbus.go):

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
  restarter of apps; hotserve.service itself is restarted by systemd
  after a crash, never after a refused start), `KillMode=control-group`,
  `UnsetEnvironment=XDG_RUNTIME_DIR DBUS_SESSION_BUS_ADDRESS`.

**The probe stays.** At `App.Start` — every config activation —
hotserve starts a real transient unit with this property set and reads
back from inside it that both namespaces engaged. The verdict is
cached against the manager connection's generation
(`userManagerClient.cachedSandboxCapability`): later activations on
the same connection read the cache, and a redial (a manager restart,
or hotserve's own) starts a new generation that the next `Start`
measures afresh. A unit launched between a redial and that next
`Start` — a watchdog relaunch after the manager came back — is not
preceded by a probe; if the host can no longer deliver the namespaces
it fails `226/NAMESPACE` and the watchdog reports it, never a bare
app. The probe is not a
proxy for the systemd version — it is what catches a container, an LXC
VPS or a kernel built without user namespaces, all of which can present
a supported manager version and still refuse the unit. CI sets
`kernel.apparmor_restrict_unprivileged_userns=0` on GitHub's Ubuntu
kernel to make the runner behave like a Debian host, so no lane
exercises a restricted kernel; the probe is the only thing standing
between such a host and a silent loss of isolation. Deleting it would
turn a measurement into a claim.

**What it does not do**, by design: the network namespace is shared
(egress open); there are no per-app UIDs; resource caps are unset.
Each is in "Residual risks".

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
  operator's private key; there is nothing to brute-force. The
  per-address failure throttle bounds the log-amplification vector.
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
  holds no grant; keep it that way. The `box` applier is a root unit,
  and the rule it holds is the same one from the other side: root
  consumes nothing from the serving process except a bundle it
  re-verifies from scratch, and the exit status of `systemctl reload`.
- **The reload's exit status is the hotserve uid's word.** The `box`
  applier marks a push `applied` on it. A compromised supervisor can
  say "loaded" and keep serving the old configuration until a restart;
  the file on disk is root's regardless, and `systemctl restart
  hotserve` is the remedy. Reading the admin API from root to check
  would be the same trust in another form, and was rejected.
- **The never-cut-the-branch guard is presence, not reachability.** A
  pushed Caddyfile that keeps a `box` block, a signer, the
  `box_webhook` site and the signing key passes it, and can still
  strand the box behind a mistyped `claim`, a host without a
  certificate, or a key no laptop holds. The remedy is the console:
  `hotserve init` again, or a hand edit the next signed push
  overwrites.
- **A signer's software-held key is the box.** Nothing on the box
  distinguishes a signature made by the operator from one made by
  whoever holds the operator's key file. A hardware-held key (`sk-`
  types, which the `signer` grammar accepts) reduces a laptop
  compromise to "cannot sign without a touch". Stated so operators
  choose knowingly.
- **Sibling localhost ports** — closed for the contract: nothing
  hotserve runs listens on a port. Each instance binds a unix socket
  in a directory of its own (`<root>/<app>/run/<nonce>/app.sock`, the
  only `run/` entry in that instance's view), and a sibling's socket is
  outside the view like the rest of the sibling.
  What remains is an app that opens a loopback listener of its own
  accord: that port is reachable by siblings, by the app's own choice.
- **The socket name is app-writable, and `connect(2)` follows
  symlinks.** The instance's `run/<nonce>/` is bound writable, so an
  app can replace its socket with a symlink to a socket outside its
  view — hotserve's admin socket, a sibling's — and a proxy that
  dialled the *name*
  would connect through it in hotserve's namespace: a confused deputy
  handing the admin API to the world. hotserve therefore never dials
  the name. The first successful connect opens the file
  `O_PATH|O_NOFOLLOW` (a symlink comes back as the symlink and is
  refused; only `S_IFSOCK` passes) and hard-links that inode, via the
  descriptor's `/proc/self/fd` magic link so no second lookup of the
  name happens, to `<app>/proxy/<nonce>.sock` — a directory no app's
  view contains — and every dial from then on, proxy and prober, goes
  to the pinned name (`socketRef`, liveswap/socket.go). What the app
  does to its own name afterwards is irrelevant. The pinned name is
  unique per instance and never reused — the nonce is 64 random bits,
  so a repeat takes on the order of 2^32 launches of one app — and
  retiring an instance simply unlinks it, so a request that obtained
  the address before the cutover and dials after it reaches nobody —
  never another app (a descriptor number would be recycled; a nonce
  is not). A reattach
  applies the same check before adopting a record and unlinks the
  candidate it pinned if the manager says definitively that the unit
  is not running. Each instance binds in a `run/<nonce>/` of its own,
  so the instance a deploy replaces — running alongside, possibly the
  compromised one the deploy exists to replace — cannot reach the new
  socket's name before it is pinned. Pinned by
  `TestGetUpstreamsRefusesASymlinkUnderTheSocketName`,
  `TestSocketRefPinsTheSocketAndRefusesASymlink` and the reattach
  cases in `TestEnsureRunningRefusesARecordWhoseSocketItCannotDial`.
- **Network egress** is open for every app. A runtime whose permission
  model gates the network (Deno's `--allow-net`, which the socket
  contract lets an app narrow to `unix:{socket}` alone) can close it
  from inside — see
  liveswap/README.md "Runtime permissions"; Node's `--permission`
  model does not cover network I/O. The runtime-agnostic answer is
  `PrivateNetwork=` on the unit — a path socket crosses network
  namespaces, so the contract already allows it — as a per-app opt-in
  when an app that wants no network exists (it would also cut the app
  off from any database): #57.
- **Resource exhaustion** — a runaway app can starve its siblings and
  Caddy (fork bomb, memory leak). `ProtectControlGroups=` makes
  `MemoryMax=`/`TasksMax=`/`CPUQuota=` real the moment they are set;
  nothing sets them until an app needs bounding (#71).
- **`state.json` must stay outside any writable sandbox view**
  (liveswap/state.go is read on relaunch for the version and the
  unit, and checked before either is used: a version that is not one,
  a unit that is not this app's, a nonce that is not one — refused,
  never a path). Normative and shipped: only the release
  being started, `shared/` and the OS base view are bound into the
  unit — the app dir root, `state.json`, `tmp/` (the upload staging
  dir: a running instance must not be able to rewrite the next
  version's tarball), `deploys/` (each version's recorded deploy
  outcome; `TestDeployRecordsAreOutsideTheSandboxView`) and the
  other releases do not exist inside.
- **The deploy record store is a trust boundary in both directions**
  (liveswap/deploys.go states its four rules; every function there
  and both filters hold them). A record is written once, through the
  filter, into a directory verified as the app's own — so no known
  secret is on disk in it (a version equal to one is the exception,
  and is already the record's file name and the release directory's),
  and a secret rotated later is not in an old record for the new
  filter to miss. Anything read back is text
  from disk: served only through the filter, trusted for nothing
  else unless it proves itself a record (a regular file, a valid
  version name, an object naming that version). Nothing from disk
  reaches a response except through the filter as body text; a name
  read off disk — a release directory's, a record's, `state.json`'s —
  is a safe string like any other and, like any other, never one
  equal to a known value (the app dir was writable by the app before
  sandboxing existed); and nothing is appended to a body after the
  filter's final pass but what the filter's own second rule names.
  Nothing there
  follows a link, ancestors included, and the store never blocks a
  deploy or a status.
  `sandboxSpecFor` in liveswap/sandbox.go is the single place that
  list is built; `TestSandboxSpecFor` and
  `TestSandboxViewIsExactlyWhatIsNamed` pin it — the latter asserts the
  rendered set of bind destinations IS the view, so an accidental
  widening fails there.
- **Log amplification is bounded, not closed.** The auth-failure
  throttle caps what the webhook writes to the journal (about 110
  lines a minute from any number of sources); an operator-enabled
  access log records every request regardless, and the verification
  CPU per request is not throttled (see the webhook section for why).

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
  the bind-source TOCTOU race (no attacker-controlled app runs outside
  a sandbox; hotserve and the user manager still share the uid, and
  are the trust domain), the app-writable recorded tier (no record), and
  "what a bare app leaves behind in `shared/`" (nothing runs bare). The
  pre-#40 "reachable today" attack-path table described bare apps and
  is history with them.
- 2026-09-06 — App contract moved from `127.0.0.1:$PORT` to a
  per-instance unix socket in `<app>/run/`; the sibling-port residual
  closed for every runtime, compiled apps included.
- 2026-09-07 — The two owed non-isolation items closed: webhook auth
  failures throttled in the journal, per address and process-wide (T4
  log amplification bounded; deploys never refused), and
  `max_artifact_entries` plus PATH_MAX/NAME_MAX name caps in
  extraction (T3 inode exhaustion closed), with a 75% warning so a
  growing app sees either cap coming.
- 2026-09-16 (#106) — Optional `sha256` on a pull deploy, checked on
  the download stream; T3 no longer chooses the bytes of a pinned
  pull. Actor and attribution claims in `deployed_by` (#111) landed
  the day before.
- 2026-10-09 — `box` designed (box/DESIGN-box.md): the config
  repository becomes the only writer of `/etc/hotserve/Caddyfile`,
  through an OIDC-authenticated webhook and a root applier that
  verifies an SSH commit signature, the file's membership in the
  commit, and descent from the commit the box runs. The configuration
  at rest joins the asset list (5); T6, the compromised config-repo
  identity, joins the profiles; the `HOTSERVE_CONFIG` sudoers grant
  leaves the model when the applier ships, the secrets grant when
  secrets ride the same channel.
