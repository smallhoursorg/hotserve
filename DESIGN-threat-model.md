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
5. **Backup credentials** — `/etc/hotserve/backup.env`, mode `0600`
   owned by root: the repository password and the storage key. They
   reach a copy of every app's declared state, so they rank with the
   data itself. systemd reads the file as root and passes the values
   to each backup job; no app is ever in a view that contains it, and
   hotserve itself never reads it — nor does any `hotserve backup`
   command: systemd is its only reader. `hotserve backup init` checks
   new settings, before there is a file, by running them the same way
   from a short-lived copy in `/run/hotserve-backup` — a root-only
   directory on tmpfs, so the copy is removed when each check ends, and
   an init that is killed outright leaves it in memory until the next
   boot at most, never on disk.
   Purge removes both. Who holds the credential, and who can reach
   what, is tabled under "Backups" in "Trust boundaries".
   The box's storage key should not be able to *delete* — `init` tries
   a delete and says which kind you have (docs/backups.md).
6. **Staged database copies** — `/var/lib/hotserve-backup/<app>/data`,
   mode `0750` owned by `hotserve`: a consistent copy of that app's
   declared databases, taken before each upload and removed when it
   ends (one left by a run that was cut off is cleared by the next).
   Plaintext, like the database it came from, and outside
   every app's view. What is in those dirs is written by jobs, so the
   launcher, which is root, makes and chowns none of it: systemd makes
   each unit's dir (`StateDirectory=`; `RuntimeDirectory=` for init's
   checks), does not start a unit whose dir is a link, and changes
   owners without following one. The staging root itself is root's,
   `0750`: nothing running as `hotserve` — hotserve.service included —
   needs to enter it, so none can.
   SQLite creates `-shm` beside a WAL database to read it at all, so
   whatever copies a database has that app's `shared/` **writable**.
   That is a unit of its own, given nothing else: no network
   (`PrivateNetwork=`) and no settings file, so no repository
   credentials. restic — the process that talks to the network, and
   parses what the storage sends back — runs in the unit after it,
   with the app's data read-only. The one unit with both the network
   and an app's data writable is a restore, which an operator starts
   and confirms.
   A restore (`hotserve backup restore`) takes its
   copies out of the repository into `<app>/restore` beside them and
   removes them when it ends. It runs in that app's backup unit with
   the app's data writable, and with nothing else writable but its own
   `<app>/restore` dir: the backup's copies and its cache are not in
   its view. A link the app left in its
   own data can therefore steer the restore's writes only into the
   app's own data or that scratch dir.
   The repository itself is never on the box: it is a backend URL
   (`s3:`, `b2:`, `rest:` …), and init, the hourly run and
   restore refuse a path. No backup unit has any of it in its view.
7. **Sibling app data** — `/var/lib/liveswap/<app>/{releases,shared,state.json}`.
8. **System integrity** — root, persistence, other system services.
9. **Availability** — serving traffic and the deploy pipeline.

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

The only application-level authenticated entry point. Auth is a
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
  external program hotserve runs, with a fixed argument list —
  bounded by `deploy_log_lines` (default 40) and 8 KiB; the first 512
  bytes of a failing health probe's body; and the exit status the
  runner recorded. `deploy_log_lines 0` keeps the app's bytes — the
  tail and the probe body both — on the box; the exit status and the
  probe's status code are hotserve's observations and stay. Reading
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
Secondary: a malicious host can trickle bytes under the cap (only a
30 s `ResponseHeaderTimeout`, set in `newDownloadClient`) to hold the
per-app deploy lock open — DoS-of-deploys, not of serving.

### Tar extraction — `liveswap/extract.go`

Two-pass (validate-all, then write — no partial residue,
`extractArchive`, liveswap/extract.go). Traversal via stdlib
`filepath.IsLocal`, which both passes reach through `safeRelPath`.
Symlink/hardlink targets must resolve inside the archive root
(`linkTargetStaysInside`). Modes: `Perm()|0600` strips
setuid/setgid/sticky structurally (`writeEntry`); dirs forced `0750`.
Devices/FIFOs rejected. Decompression bomb capped at
`max_artifact_size × 10` over the *decompressed* stream
(`decompressionRatioCap`, enforced by the `io.LimitedReader` in
`walkArchive`).

Residual items for the model:

- **Link TOCTOU shape (unproven, worth review):** validation is
  *symbolic* (string resolution); writing (`writeEntry`,
  liveswap/extract.go) does `os.Symlink` then later
  `os.Link`/`os.OpenFile` under `destDir` with no `openat`-style
  re-check after intermediate symlinks exist on disk. Each entry name
  passes `IsLocal`, but nothing resolves the on-disk path *through*
  an earlier-written symlink. Blast radius is bounded (targets must stay
  symbolically under root; extraction is into a hidden staging dir
  `os.Rename`d on success, in `releaseFetcher.fetch`,
  liveswap/download.go). Not asserted as exploitable.
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

### Backups — `backup/`

Backups add root processes, a repository credential, a network peer
whose answers are parsed, and plaintext copies of app data. The
boundaries are these, and each is a rule a change has to keep:

1. **Root touches nothing a job writes.** Everything under the staging
   root is written by unprivileged units, so root neither makes, chowns,
   reads nor removes any of it: systemd makes each unit's directory
   (`homeProperties`, backup/run.go — `StateDirectory=`,
   `RuntimeDirectory=`), and does not start a unit whose directory is a
   link. What a box has backed up before is read from the repository
   (`Job.lastCleanPaths`, backup/job.go), not from a file a job left.
2. **restic never runs as root.** It talks to the network and parses
   what the storage sends back. Every call is a unit as `hotserve` in
   the jobs' sandbox (`sandboxProperties`) — the hourly upload, a
   restore, init's checks and `status`'s listing (`asJob`) — and
   `backup restic --` is a unit as `hotserve` too (`PassthroughArgs`,
   backup/restic.go), with the filesystem an operator restores into
   and a user namespace of its own.
3. **No backup unit has both the network and an app's data writable.**
   SQLite needs the data writable to read a database at all, so the
   copy is a unit of its own with no network and no credentials
   (`StageArgs`); the upload has the network and the data read-only
   (`LaunchArgs`). The exception is a restore, which has to write what
   it fetches, and which an operator starts and confirms.
4. **Only systemd reads the settings file, and the credential reaches a
   unit one way.** systemd reads the root-only file (`EnvironmentFile=`)
   and puts the values in the unit's environment. They are never in an
   argv — systemd is told not to expand `$VAR` in any unit's command
   (`noExpansion`, backup/run.go: measured, it otherwise writes the
   password into the argv of a job whose declared path says
   `${RESTIC_PASSWORD}`) — and never in `systemd-run`'s environment
   (`asJob` hands it none), and the file is not in any unit's view. No root command opens
   it either: its format is systemd's, and a second reader is a second
   opinion about what it says. `run` and `restore` hold it to this
   package's rules by asking a unit with nothing in its view to look at
   its own environment (`settingsCheckArgs`, `checkSettings`), and
   refuse before anything is launched; `status` and `backup restic --`
   name the file to systemd and never see inside it. `init` writes it,
   from values it was given, and reads it never.
5. **One unit name per app** (`unitName`) for its copy, its upload and
   its restore, so no two of them run at once — and a unit is stopped
   only by the command that started it and is waiting on it
   (`stopWithContext`), never by pattern: the name an hourly run would
   sweep up is also an operator's restore. Units that are no app's — a
   check, a report's restic — have an underscore in their names
   (`oneOffUnit`, `checkUnit`), which no app's name has.
6. **What decides is the repository or an exit status**, not state on
   the box and not a program's wording: clean runs are records in the
   repository (`CleanTag`), a repository's presence is `cat config`'s
   exit status (`repositoryState`, backup/init.go). The one exception,
   and why, is at `deniedBy`.
7. **Root believes nothing the admin API says.** Which apps declare
   state comes over a socket from the hotserve process — the first
   thing an attacker on the internet reaches — and root turns it into a
   unit's name, its `StateDirectory=` and a `BindPaths=` value, which
   systemd splits on whitespace and colons. So the answer is held to
   this package's own rules whatever liveswap validated at config load:
   an app's name to liveswap's alphabet (`appNameRe`, backup/admin.go),
   the root to a plain absolute path (`checkRootPath`), a declaration's
   kind to the two there are, each declared path to the inside of
   `shared/` (`sharedPath`, backup/backup.go) by the job, which gets it
   as an argument systemd passes on untouched (rule 4). A
   name, a root or a kind that fails is refused, and nothing is launched on
   that answer.

**Why any of it is root.** Four commands start as root — `run`,
`restore`, `init`, `status` — for two things only root can do, and they
do nothing else:

- **Keep the credential from the `hotserve` uid** — which is not the
  same as holding it, and no root command does. The internet-facing
  server and every deployed app run as `hotserve` (see "The shared-UID
  rule"), so a file that uid can read is a file a compromised server
  can read. The repository password and storage key are therefore
  root's, `0600`, and reach a unit through PID 1. Measured on Debian 13,
  as `hotserve` outside any sandbox — what hotserve.service is —
  against a running backup unit of the same uid: the settings file is
  "permission denied"; so are the unit process's `/proc/<pid>/environ`
  and `/proc/<pid>/root`, because the unit is in a user namespace that
  uid has no capability in; `systemctl show` names the file and not its
  contents.
- **Start a unit under the system manager.** Only it can read a
  root-only `EnvironmentFile=` and then drop to `User=`, which is rule
  4. As `hotserve`, `systemd-run` of a system unit is "Access denied",
  and so is stopping a backup that is running. (liveswap's app units
  are the `hotserve` user manager's, and for a unit there the credential
  would have to be readable by `hotserve`.)

What root is *not* for: it does not run restic or sqlite3, open an
app's data, write where a job writes, or read the settings file. Each
root command reads the admin API (rule 7), asks systemd for units, and
prints; `init` also writes the settings file.

For the one root command nobody is watching — the timer's launcher —
that is held to by its unit (packaging/hotserve-backup.service) rather
than promised by the code: a read-only filesystem, the staging dirs and
hotserve's own state not in its view at all (`InaccessiblePaths=`, which
is rule 1 made physical), the settings file an empty file that merely
exists (rule 4 made physical: measured, it reads as nothing from in
there, and a unit started from in there still gets the values), no
network, a system-call filter, and one
capability left of root's forty — `CAP_DAC_OVERRIDE`, because the admin
socket is writable by its owner alone, and on a read-only filesystem it
opens files to read and sockets to connect to and changes nothing. The
package smoke test starts the real unit against the real socket. It is
what a fault in reading the admin API's answer has to work with, not a
boundary against the launcher being taken over outright (see below).
`restore`, `init` and `status` are run by an operator through `sudo`,
and are whatever `sudo` makes them.

**Why not a user of its own.** A less privileged `hotserve-backup` user
would need leave to start units under the system manager, and that
leave is not a smaller thing than root: whoever may create a transient
unit chooses its `User=` and its command. (Measured: a root process
stripped of every capability, with a read-only filesystem and no
network, still starts a unit that runs as uid 0 and reads
`/etc/shadow`.) The leave itself would come from polkit, which a Debian
13 server does not have — systemd only suggests it — so it would be a
new dependency, to grant something root-equivalent under another name.
What would be a real reduction is no transient units at all: template
units shipped in the package, with the sandbox in the unit file, and a
launcher allowed to *start* them and nothing else. That moves the
per-app facts a unit is given today — the declarations, liveswap's
root, a restore's snapshot — out of its arguments and into files
somebody has to write and somebody else has to trust, which is the
kind of boundary rules 1 and 6 exist to avoid. It is an idea, not a
plan.

Who runs as what, and what each can reach:

| Process | Runs as | Sandbox | App data | Network | Credential | Runs |
|---|---|---|---|---|---|---|
| `backup run` — the timer's launcher (`RunAll`) | root | its unit's: read-only, one capability, no network | **none**: the job says when its app has no data yet (`exitNoData`) | the admin socket | **none**: looks that the file exists; in its unit, cannot read it | `systemd-run` |
| the settings check (`settingsCheckArgs`) | `hotserve` | full | none | yes, and uses none | yes: it is what it checks | `hotserve backup check-settings` |
| the copy (`StageArgs`) | `hotserve` | full | that app's, **writable** | **none** (`PrivateNetwork=`) | **none** | sqlite3 |
| the upload (`LaunchArgs`) | `hotserve` | full | that app's, read-only | yes | yes | restic |
| `backup restore` — its launcher (`cmdRestore`, backup/cmd.go) | root | none | `stat`; a missing `shared/` is made by a unit as `hotserve` (`ensureShared`) | the admin socket | none: looks that the file exists | `systemd-run`, `systemctl` |
| the restore (`RestoreArgs`, backup/restore.go) | `hotserve` | full | that app's, **writable** | yes | yes | restic, sqlite3 |
| `backup verify` — the weekly check's launcher (`cmdVerify`) | root | its unit's: read-only, **no capability**, no network | none | none | none: looks that the file exists; in its unit, cannot read it | `systemd-run` |
| the weekly check (`Verify`, backup/verify.go) | `hotserve` | full | **none** | yes | yes | restic (`check`, then a record of what it found) |
| `backup init` (`Init`) | root | none | none | none of its own | writes the settings file, last, once the checks pass | `systemd-run` |
| init's checks (`asJob`) | `hotserve` | full | **none** | yes | yes — the settings being tried, from a root-only copy on tmpfs removed after each | restic |
| `backup status` (`cmdStatus`) | root | none | none | the admin socket | none: looks that the file exists | `systemd-run`, `systemctl` |
| `status`'s listing (`inUnit`) | `hotserve` | full | **none** | yes | yes | restic |
| `backup restic -- …` (`PassthroughArgs`, backup/restic.go) | `hotserve`: a unit systemd sets up, `User=` and `EnvironmentFile=` | a user namespace (`PrivateUsers=`) and nothing else: the settings are in its environment, and without one the hotserve uid reads them from `/proc/<pid>/environ` (measured) | everything `hotserve` owns | yes | yes | restic, with the operator's arguments |
| `backup app` by hand, no `--phase` | whoever ran it | **none** | whatever that user can reach | yes | from that user's environment | sqlite3, restic |

"Full" is `sandboxProperties`: a private user and PID namespace, an
empty read-only root with `/usr` and a handful of `/etc` files bound
in, no capabilities, and the unit's own directory. The last two rows
are an operator's tools, not anything the timer runs.

What is on disk, whose it is, and who writes it:

| Path | Owner, mode | Written by | Read by root |
|---|---|---|---|
| `/etc/hotserve/backup.env` | root, `0600` | `init`, as a `0600` temp file renamed into place (`writeEnvFile`) | by systemd alone, for `EnvironmentFile=` (rule 4) |
| `/run/hotserve-backup/` (tmpfs) | root, `0700`, checked before use (`requireRootOnlyDir`) | `init` alone (`asJob`): the settings one check is tried with, removed when it ends | — |
| `/run/hotserve-backup-check/` (tmpfs) | `hotserve`, `0700`, made by systemd | init's checks (restic's cache) | never; `init` removes it by name — `/run` is root's, and the removal follows no link |
| `/var/lib/hotserve-backup/` | root, `0750` | nobody: only systemd makes entries in it | never |
| `/var/lib/hotserve-backup/<app>/` | `hotserve`, `0750`, made by systemd | that app's copy and upload: `data/` (the staged copies — plaintext, there while a run lasts and removed when its upload ends), `cache/` | **never** |
| `/var/lib/hotserve-backup/<app>/restore/` | `hotserve`, `0750`, made by systemd | that app's restore: its cache, and `copies/`, removed when it ends | **never** |
| `/var/lib/hotserve-backup-status/` | `hotserve`, `0750`, made by systemd | `status`'s and `snapshots`' restic (its cache) | never |
| `/var/lib/hotserve-backup-verify/` | `hotserve`, `0750`, made by systemd | the weekly check's restic (its cache) | never |

Pinned by: `TestTheUnitThatCanWriteAnAppsDataCanReachNothing` (rule 3:
the two units differ by exactly the data's bind, the network and the
settings file), `TestTheJobAndInitsChecksShareOneSandbox` and
`TestRestoreRunsInTheBackupJobsUnitAndSandbox` (one sandbox),
`TestStatusRunsResticAsTheJobsDo`, `TestAsJobRunsResticAsTheJobDoes`,
`TestTheSettingsAreCheckedByAUnitNotByRoot` and
`TestPassthroughIsAUnitSystemdSetsUp` (rules 2 and 4),
`TestNoUnitsCommandIsExpandedBySystemd` (rule 4: no unit's command is
expanded from its settings), `TestRunAllStopsTheJobItIsWaitingOnAndNoOther`
and `TestInitsCheckUnitIsNoAppsUnit` (rule 5), `TestRunAllContinuesAfterOneFailureAndReportsIt` (rule
1: the launcher makes nothing under the staging root),
`TestFetchAppsHoldsTheAdminAPIsAnswerToItsOwnRules` (rule 7: a name or
a root that is two binds, climbs, or carries a unit suffix launches
nothing). On a real box
the e2e backup suite reads the copy unit's properties from systemd while
one is held open, plants a failing `restic` first on root's `PATH` for
`init` and for `status`, and plants a link where a job's directory
belongs; the package smoke test holds the staging root to root, `0750`.

What these do not give. Every unit that reaches the repository holds the
same credential, so a compromised restic in one app's upload can read
every app's snapshots — and delete them, unless the key cannot (see
"Backup credentials" under Assets). It can also read that one app's
data, which is what it is there to do. The restore is the one unit with
both the network and an app's data writable. And `backup restic --` is
restic as `hotserve` with the whole filesystem in view — it is for an
operator at a terminal, to restore where they say, and nothing starts it
on a schedule — in a user namespace of its own, which is what keeps its
environment, the credential in it, from the other processes of that uid.

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
  is withholding. Faces `extract.go` and the first-hop SSRF gap.
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

For **T2**: deploy-arbitrary-code (a push is contained by nothing but
the claim scope; a pull additionally by the allowlist); deliberate
rollback. For **T3**: archive-borne CPU (bounded by the byte and
entry caps; inode exhaustion is closed by the entry cap); the
link-TOCTOU shape (unproven); first-hop→any-https SSRF. For **T4**:
log-amplification, bounded by the auth-failure throttle (online
*token forgery* is infeasible).
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
  holds no grant; keep it that way.
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
