# liveswap

**Your reverse proxy is your deploy pipeline.** liveswap turns a
single Caddy server into a zero-downtime deploy orchestrator for the
apps it fronts — Node.js, Go, anything that listens on a port. CI
builds a tarball, POSTs a webhook with its URL, and Caddy does the
rest: download, migrate, start the new version on a fresh localhost
port, health-gate it, atomically cut traffic over, gracefully stop the
old one. No Kubernetes, no Nomad, no SSH keys in CI, no extra daemons.
One binary. Part of [hotserve](https://github.com/smallhoursorg/hotserve), from [smallhours](https://github.com/smallhoursorg).

```
git push → CI builds app.tar.gz → uploads it → curl webhook → Caddy hot-swaps it
```

## How a deploy works

```
POST /blog {url, version}
  │ downloading   stream the artifact (size-capped, https, token-gated)
  │ extracting    hardened tar extraction into releases/<version>/
  │ preparing     pre_start command (migrations) — non-zero exit aborts
  │ starting      spawn the app on a fresh 127.0.0.1 port, PORT injected
  │ soaking       GET /health until continuously healthy for `soak`
  │ promoting     atomic cutover — new requests hit the new version
  │ draining      wait `drain` for in-flight requests on the old one
  │ stopping_old  stop the old unit: SIGTERM its whole cgroup, SIGKILL after `grace`
  └ 200 OK        (any failure before "promoting" → old version never
                   stopped serving, webhook returns 5xx, CI goes red)
```

The diagram shows a **URL pull**; the same pipeline serves two more
sources (see [Webhook API](#webhook-api)): a **push** streams the
tarball in the request body (skips `downloading`), and a **rollback**
relaunches an on-disk release (skips `downloading`/`extracting`/
`preparing`). Versions are immutable — a re-deploy of an existing
version is rejected; rollback is how you relaunch one.

The cutover is an atomic pointer swap inside a `reverse_proxy` dynamic
upstream source — no config reload, no socket juggling, and every
reverse_proxy feature (WebSockets, HTTP/2, streaming, retries) keeps
working. Rollback is one curl: `POST /<app>?rollback=<version>`.

If you know Nomad, the concept map is:

| Nomad | liveswap |
|---|---|
| `canary = 1` + `auto_promote` | start new → health gate → atomic cutover |
| `min_healthy_time` | `soak` |
| `auto_revert` | failures never promote; old version keeps serving |
| `artifact` stanza + deploy webhook | the webhook payload's `url` |
| `nomadService` template re-render + SIGUSR1 | `dynamic liveswap <app>` |

## Install

Build Caddy with the module (nothing else to install on the server):

```sh
xcaddy build --with github.com/smallhoursorg/hotserve/liveswap
```

Or in a Dockerfile:

```dockerfile
FROM caddy:2.11.4-builder AS builder
RUN xcaddy build v2.11.4 --with github.com/smallhoursorg/hotserve/liveswap

FROM caddy:2.11.4
COPY --from=builder /usr/bin/caddy /usr/bin/caddy
```

## Caddyfile

```caddyfile
{
	liveswap {
		# root /var/lib/liveswap                # where releases/state live (default)
		artifact_allowlist github.com/your-org/ # required: where artifacts may come from
		# allow_insecure_http                  # permit http:// artifact URLs (off by default)

		# Who may deploy — required (globally or per app). A deploy
		# carries an `Authorization: Bearer <JWT>`; it is authorized if
		# any deploy_trust source verifies it. No shared secret is ever
		# stored on the box.
		deploy_trust github {                  # preset: GitHub Actions OIDC
			audience hotserve
			claim repository your-org/blog     # pin who may deploy
			claim ref        refs/heads/main
		}

		app blog {                             # a Node.js app
			command node server.js             # runs with CWD = the release dir
			pre_start node migrate.js          # optional; non-zero exit aborts the deploy
			env_file /etc/liveswap/blog.env     # optional KEY=VALUE file
			env NODE_ENV production            # inline env, repeatable
			# deploy_trust local { public_key /etc/hotserve/blog.pub }  # per-app override
			# Everything below is a default, shown for reference:
			# health_path       /health        # GET must return 2xx ("off" = liveness only)
			# health_interval   5s
			# health_timeout    2s
			# soak              15s             # continuous health required before cutover
			# deadline          5m              # abort if not healthy in time
			# drain             5s              # pause between cutover and SIGTERM
			# grace             10s             # SIGTERM → grace → SIGKILL
			# watchdog          on              # restart on crash / sustained health failure
			# watchdog_failures 3               # consecutive failed probes before a restart
			# watchdog_grace    30s             # post-start window where probe failures don't count
			# watchdog_restarts 5               # restart budget within watchdog_window
			# watchdog_window   10m             # sliding window for the budget
			# keep              5               # release dirs retained on disk
			# max_artifact_size 100MB
		}

		app api {                              # a Go app
			command ./server --config config.yaml
			env DATABASE_URL sqlite:{shared_dir}/api.db
		}
	}
}

blog.example.com {
	reverse_proxy {
		dynamic liveswap blog
	}
}

api.example.com {
	reverse_proxy {
		dynamic liveswap api
	}
}

deploy.example.com {
	liveswap_webhook
}
```

Apps must listen on `127.0.0.1` at the injected `PORT`. Their
environment is, lowest precedence first: an allowlisted slice of
Caddy's environment (`PATH`, `HOME`, `LANG`, `TZ`, `LC_*` — nothing
else, so supervisor credentials like ACME DNS tokens never reach
apps) → `env_file` → inline `env` → injected `PORT` and
`HOST=127.0.0.1`, all layered on the systemd user manager's own
defaults (`XDG_RUNTIME_DIR`, `INVOCATION_ID`, …). Keys must be valid
variable names (`[A-Za-z_][A-Za-z0-9_]*`) — systemd rejects anything
else, so config load does too. Anything more an app needs must be
passed explicitly via `env` or `env_file`. Apps get the user manager's
resource limits; each unit sets its open-files limit (soft and hard)
to the manager's ceiling, which the package raises to match
`hotserve.service` (1048576) via a `user@<uid>.service.d` drop-in —
effective on systemd ≥ 256 and from the manager's next start: an
upgrade installs the drop-in but deliberately does not restart a
running manager (that would stop every app), so the ceiling changes
at the next boot or manual restart. Below 256 the manager inherits
its ceiling from PID 1 instead (#37).

### Placeholders

`command`, `pre_start` and `env` values may use deploy-time
placeholders: `{version}`, `{port}`, `{release_dir}`, `{shared_dir}`.
`{shared_dir}` (`<root>/<app>/shared/`) survives deploys — put SQLite
files and uploads there. Standard Caddy `{env.*}` placeholders are
resolved at config load.

### Options

| Option | Default | Meaning |
|---|---|---|
| `root` | `/var/lib/liveswap` | Releases, shared data, state per app |
| `deploy_trust <preset> { … }` | — (required) | Who may deploy; global default or per app (see [Deploy authentication](#deploy-authentication-deploy_trust)) |
| `allow_insecure_http` | off | Permit plain-http artifact URLs |
| `artifact_allowlist` | — (required) | Where artifacts may be fetched from. Entries are a host (`artifacts.corp`) or a host + path prefix (`github.com/your-org/`). An entry admits only the scheme's default port unless it declares one (`minio.corp:9000`) — no wildcards; the port picks which service on the host answers, so it belongs to the operator, not the payload. Pin the path on multi-tenant hosts — a bare `github.com` admits anyone's artifacts. Query strings are refused unless the entry declares the parameter names it vouches for: `gitlab.com/api/v4/projects/42/?job` allows `?job=build`, `bucket.s3.corp/releases/?X-Amz-*` allows the presigned-URL family (a trailing `*` declares a prefix; values are never re-encoded — signed queries pass through byte-identical — but every query byte must be a legal RFC 3986 query character, so percent-encode anything exotic). Closed by default because on some servers a query *name* can override path routing entirely (WordPress-style `?p=2`), which would defeat the path pin. A refused URL fails the deploy with a 422 naming the offending parameter. First hop only, by design (GitHub asset URLs redirect to S3); every hop must still be https unless `allow_insecure_http`. Apps may override |
| `command` | — (required) | argv to start the app, CWD = release dir |
| `pre_start` | — | Run-to-completion hook; failure aborts deploy |
| `env`, `env_file` | — | Extra environment |
| `health_path` | `/health` | 2xx = healthy; `off` = process-liveness only |
| `health_interval` / `health_timeout` | `5s` / `2s` | Probe cadence |
| `soak` | `15s` | Continuous health required before cutover |
| `deadline` | `5m` | Bound on pre_start and the health gate |
| `drain` | `5s` | In-flight grace after cutover, before SIGTERM |
| `grace` | `10s` | SIGTERM → SIGKILL window |
| `watchdog` | `on` | Continuous supervision: restart on crash or sustained health failure (`off` to disable) |
| `watchdog_failures` | `3` | Consecutive failed probes before a restart; one pass resets the count |
| `watchdog_grace` | `30s` | After every (re)start, probe failures don't count until this elapses; a crash always counts |
| `watchdog_restarts` | `5` | Restart budget within `watchdog_window`; crash and health restarts share it |
| `watchdog_window` | `10m` | Sliding window for the restart budget |
| `keep` | `5` | Release dirs retained (GC after success). The running version is always kept, so this can be `keep+1` after rolling back to an old release |
| `max_artifact_size` | `100MB` | Download cap; decompressed cap is 10× |
| `sandbox` | `auto` | Per-unit sandbox policy: `auto` (best tier the host delivers, warning when it is not `full`), `require` (`full` or refuse to start — see the hazard under [Sandbox](#sandbox)), `off`. Global default, per-app override |
| `extra_path <path> [rw]` | — | Host path the sandboxed app may see besides its own dirs; read-only unless `rw`. Repeatable. The recipe for a same-box database socket dir (`/run/postgresql`) |

## Watchdog

Deploys and boot recovery start instances; the watchdog keeps them
running. It watches the current instance continuously and relaunches
the **same version** when the process exits, or when the health
endpoint (`health_path`, probed every `health_interval`) fails
`watchdog_failures` consecutive probes. With `health_path off` the
watchdog is liveness-only: crashes still restart, health does not
apply.

Restart pacing is deliberately not configurable: exponential backoff
from 1s, doubling to a 60s cap, with ±20% jitter; the backoff resets
only after the app has been continuously healthy for
`max(30s, watchdog_grace)`. Every restart re-arms `watchdog_grace`, so
a slow-booting app is not probed into a kill loop.

The watchdog **never gives up**. The restart budget is a rate
limiter, not a give-up point: after `watchdog_restarts` restarts
inside `watchdog_window` it *throttles* — logs an error, reports
`"state":"throttled"` in the status JSON, unroutes a dead instance so
the proxy fails cleanly instead of dialing a dead port, waits for the
oldest restart to slide out of the window, and tries again. An app
taken down by a transient incident (a traffic flood, a dependency
outage) therefore comes back on its own once the incident ends, with
nobody at the keyboard; a persistent crash loop costs at most
`watchdog_restarts` restarts per `watchdog_window`, forever — which is
also the bound on anyone who can make your health endpoint fail. A
successful deploy is the fast path out of a throttle wait: it resets
the budget and backoff immediately. A long-running throttle loop means
the release itself is broken — watch `restarts_in_window` in the
status JSON (loop alerting is on the roadmap). Health probes never
follow redirects: a 3xx answer is simply "not 2xx".

The status JSON's `watchdog` object reports `state`
(`grace|watching|backoff|throttled|disabled|idle`),
`consecutive_failures`, `restarts_in_window`, `last_restart_at`,
`last_restart_cause` (`crash|health`) and `last_failure`.

**Upgrading from v0.1.0:** the watchdog is new and **on by default**,
which changes one promise: previously a promoted process was never
touched until the next deploy; now a process that exits, or whose
`health_path` fails `watchdog_failures` consecutive probes, is
restarted. If your health endpoint can be slow under load, raise
`health_timeout` (probe timeouts count as failures) — or set
`watchdog off` on apps that must keep the old hands-off behavior.
Health probes also no longer follow redirects (a 3xx now reads as
unhealthy, for the deploy gate too): if your health endpoint
redirects — a `/health` → `/health/` trailing slash is the classic —
point `health_path` at the final path.

## Sandbox

Every instance runs inside systemd's own per-unit sandbox, in one of
two tiers depending on the host's systemd:

| | `filesystem` (systemd 252–255: Debian 12, Ubuntu 24.04) | `full` (systemd ≥ 256: Debian 13, Ubuntu 26.04) |
|---|---|---|
| User namespace (`PrivateUsers=`) | ● | ● |
| PID namespace (`PrivatePIDs=`) | — | ● |
| Read-only system; liveswap root replaced by an empty tmpfs; only the app's release and `shared/` bound back writable | ● | ● |
| `/var/lib/hotserve` (TLS keys), `/run/hotserve` (admin socket), `/etc/hotserve`, `/run/user/<uid>` (manager socket) inaccessible | ● | ● |
| Private `/tmp`, minimal `/dev`, read-only cgroupfs, no capabilities, `@system-service` syscall filter, `AF_INET`/`AF_INET6`/`AF_UNIX`/`AF_NETLINK` only, no nested namespaces | ● | ● |
| Supervisor, user manager and siblings **invisible and unsignalable** | — (they stay visible and can be signalled: a DoS residual, not data access) | ● |

What the app sees: its release dir (working directory, writable), its
`shared/` dir (writable; `HOME` points there), a private `/tmp`, the
read-only system, and whatever `extra_path` declares. It does not see
`state.json`, `tmp/` (the upload staging dir), other releases, or any
other app. The network namespace is shared by design — the app binds
`127.0.0.1:$PORT` as before, and sibling ports remain reachable.

Both tiers close the routes the threat model ranks first: reading the
supervisor's environment or walking the host through
`/proc/<pid>/root` (the user namespace is what closes them — the
kernel refuses `ptrace`-class access across user namespaces even for
the same uid), the admin socket, the TLS keys, sibling files. The
`full` tier additionally removes process visibility and signals.

**Policy and rollout.** `sandbox auto` (the default) applies the best
tier the host delivers, and logs a warning at start and at every
launch that runs below `full`, naming what stays open. The tier is
probed once per start by running a throwaway unit and checking the
namespaces from inside (`journalctl -t hotserve-sandbox-probe` shows
what it saw). The tier an instance got is recorded in `state.json`
and reported by the status endpoint (`"sandbox": "full" | "filesystem"
| "none"`).

Sandboxing engages on each app's **next deploy** — the path with a
fallback (a version that cannot live in its sandbox fails the health
gate while the old one keeps serving) — never on a supervisor
restart, a boot recovery or a watchdog restart: those reproduce the
tier the instance was recorded with, so upgrading hotserve does not
turn into a fleet-wide, no-fallback restart into sandboxes. The
supported rollout: upgrade, confirm apps healthy; declare each app's
`extra_path` needs; deploy each app and watch `"sandbox"` in its
status; only then, optionally, `sandbox require`.

**`require` is the one setting that can keep the whole server from
starting**: it fails the start (admin socket and proxy included) when
the host cannot deliver the `full` tier — a manager below 256, or a
kernel/LSM that refuses the namespaces. Use it only once `auto` has
been proven on that host.

**When an app breaks under the sandbox** the symptom is usually an
`ENOENT` for something that exists on the host: a database's unix
socket, a data directory outside the app's own. Declare it:

```caddyfile
app blog {
	command node server.js
	extra_path /run/postgresql        # same-box Postgres over its socket (ro suffices)
	extra_path /var/cache/blog rw
}
```

Paths inside the liveswap root or hotserve's own directories are
refused. `sandbox off` per app is the escape hatch while you find the
missing path — and the answer for the few workloads the sandbox
cannot host at all: anything that creates its own namespaces
(Chromium's sandbox under Puppeteer, nested containers) or needs
devices beyond `/dev/null`-class ones.

**Hosts.** The sandbox is built on unprivileged user namespaces.
Ubuntu 24.04+ refuses those to unconfined processes
(`kernel.apparmor_restrict_unprivileged_userns=1`), which would leave
`systemd --user` unable to build the sandbox; the package therefore
ships an AppArmor profile, `hotserve-user-manager`, that grants
`userns` to hotserve's user manager only: the package starts
`user@<uid>.service` for the hotserve user through
`/usr/libexec/hotserve/user-manager` (an `ExecStart=` override in the
drop-in postinstall writes) and the profile attaches to that path —
never to PID 1 or other users' managers; the app units carry
`RestrictNamespaces=`, so a sandboxed app still cannot create
namespaces. (Attachment by path rather than `AppArmorProfile=`: newer
AppArmor turns a profile change requested by an unprivileged unit into
a stack with `unconfined`, which stays restricted.) Raw-binary
installs on Ubuntu need the same three pieces — the wrapper, the
profile in `/etc/apparmor.d/` (`apparmor_parser -r` it), and the
`ExecStart=` override — or, bluntly, that sysctl set to 0 host-wide. Debian's kernel has no such restriction. A host where the
probe still finds no usable user namespace runs apps with
`"sandbox": "none"` and a warning (the non-dumpable supervisor and
`NoNewPrivileges` remain); `journalctl -t hotserve-sandbox-probe`
says why.

## Deploy authentication (`deploy_trust`)

Deploys are authenticated with a short-lived **JWT**, verified against
public material only — there is no shared secret on the box. Every app
must resolve to at least one `deploy_trust` source (globally or per
app), or config load fails. A request is authorized if **any** source
verifies its `Authorization: Bearer <JWT>` and **all** that source's
claim constraints match.

Presets:

- `deploy_trust github { audience <a>; claim … }` — GitHub Actions
  OIDC (`issuer https://token.actions.githubusercontent.com`, fixed).
- `deploy_trust gitlab { audience <a>; … }` — GitLab CI; add
  `issuer https://gitlab.example.com` for self-hosted.
- `deploy_trust oidc { issuer <url>; audience <a>; … }` — any OIDC
  provider (CircleCI, Buildkite, k8s, …).
- `deploy_trust local { public_key <path> }` — a key you control, for
  non-CI deploys. Generate it with `hotserve deploy-keygen`, mint
  tokens with `hotserve deploy-token`.

Sub-directives: `audience` (required for OIDC — never trust an
unaudienced token), `claim <name> <value>` (exact-match, repeatable —
pin `repository`, `ref`, `environment`, etc.), `subject` (sugar for
`claim sub`), `issuer` (oidc/gitlab), `public_key` (local).

The OIDC presets also **require an identity claim** — one of
`repository`/`repository_id`/… (github), `project_path`/`project_id`/…
(gitlab), or `sub` (oidc). An audience alone is not identity: any
repo/project on the issuer can mint a token for any audience, so a
source with only an audience would authorize the whole issuer. Config
load fails without one.

`Authorization: Bearer` is the only accepted transport — Caddy redacts
it from access logs automatically.

### Multiple sources (teams)

`deploy_trust` is repeatable, and a request is authorized if **any**
block accepts it. That is how a team combines CI with per-developer
break-glass keys — think of the `local` blocks as an `authorized_keys`
list:

```caddyfile
liveswap {
	# Everyday path: CI deploys on merge, no secret anywhere.
	deploy_trust github {
		audience blog
		claim repository your-org/blog
		claim ref        refs/heads/main
	}
	# Break-glass: each dev registers their own public key (they keep
	# the private half on their laptop). Revoke one by deleting its
	# block; no shared secret, no effect on the others.
	deploy_trust local { public_key /etc/hotserve/alice.pub  subject alice }
	deploy_trust local { public_key /etc/hotserve/bob.pub    subject bob }

	app blog { command node server.js }
}
```

Each dev runs `hotserve deploy-keygen` once and hands you the `.pub`
(public — safe to share); you add a block and reload. Use OIDC in CI,
never a `local` key in CI (that would store a long-lived private key in
CI secrets — the thing OIDC exists to avoid).

Every successful deploy records **which source authorized it**: a
`deploy authorized` log line (`via` = the source label, e.g.
`local:/etc/hotserve/alice.pub` or `oidc:https://token.actions…`) and a
`deployed_by` field in the status JSON. That is your audit trail for
hand deploys — pin `subject <name>` on each dev's block so the label
names the person.

## Webhook API

`POST https://deploy.example.com/<app>`, with `Authorization: Bearer
<JWT>` (see `deploy_trust` above). There are three ways to supply the
release, all through the same endpoint and the same auth:

**1. Pull from a URL** (the default — a JSON body):

```json
{
  "url": "https://github.com/you/blog/releases/download/v1.4.2/blog.tar.gz",
  "version": "v1.4.2",
  "auth_header": "Bearer <token-for-private-assets>"
}
```

`auth_header` is optional and is sent verbatim as `Authorization` on
the artifact download (dropped automatically on cross-host redirects,
so GitHub's S3 redirect works). The URL must pass `artifact_allowlist`.

**2. Push an uploaded tarball** — no artifact host needed. Stream the
`.tar.gz` as the request body with a gzip content type; the version is
a query parameter:

```sh
curl --fail -X POST -H "Authorization: Bearer $JWT" \
  -H "Content-Type: application/gzip" --data-binary @blog.tgz \
  "https://deploy.example.com/blog?version=v1.4.2"
```

The upload is capped at `max_artifact_size`. No `artifact_allowlist` is
consulted (there is no URL to pin — the bytes come straight from an
authenticated caller), and there is no SSRF surface on this path. This
is the path for deploying a local build directly (a laptop, an
air-gapped or egress-locked box) without hosting the artifact anywhere.

**3. Roll back to an on-disk release** — relaunch a version still on
disk (retained by `keep`), with no fetch or upload:

```sh
curl --fail -X POST -H "Authorization: Bearer $JWT" \
  "https://deploy.example.com/blog?rollback=v1.4.1"
```

Rollback runs the same blue/green pipeline (start, health-gate, cut
over), so it is zero-downtime too. A `422` is returned if that version
is no longer on disk. To see what you can roll back to, `GET /<app>`
returns `available_versions` — the on-disk releases, newest-first
(`keep` of them, plus the running version when it is older than those —
see the `keep` note above, so the list can briefly hold `keep+1`).

**Versions are immutable.** A deploy (URL or push) never overwrites an
existing on-disk release, so a version you can roll back to can't be
silently replaced — re-deploying an existing version is a `422`. Use a
new version, or roll back to relaunch an existing one. (A deploy that
*fails* before cutover is cleaned up, so that version stays retriable.)

The response is synchronous:

| Code | Meaning |
|---|---|
| 200 | Deployed; body is the app's status JSON |
| 401 | Bad or missing token |
| 404 | Unknown app |
| 409 | A deploy is already running for this app (retry) |
| 413 | Pushed upload exceeded `max_artifact_size` |
| 422 | Bad request — missing/invalid version, version already running, **version already exists** (versions are immutable — deploy a new version or roll back to relaunch it), a rollback target no longer on disk, or (URL path) an artifact url refused by `artifact_allowlist` (host, path, port, or an undeclared query parameter; the body names exactly what tripped and how the entry would declare it) |
| 5xx | Deploy failed — **the old version is still serving**; body says why |

Because the response is synchronous through the whole pipeline, the
POST's wall time includes the health soak, the `drain` pause and the
old version's graceful stop — with defaults, a healthy deploy answers
in roughly soak + drain (~20s). Budget your CI step timeout for
`deadline` plus drain and grace, and expect a concurrent deploy to
409 until the first one finishes.

`GET /<app>` (same bearer token) returns status: phase, current
version, port, pid, last deploy result (including `deployed_by`), the
watchdog's state (restart counts, last restart cause), and
`available_versions` — the on-disk releases you can roll back to,
newest-first.

The tarball's contents must sit at the archive root (`tar -czf
app.tar.gz -C dist .`), with versions matching `[A-Za-z0-9._-]{1,64}`.

## Secrets and logs

What liveswap does for you:

- Deploy logs record the artifact **host only**; download errors go
  through a redactor that drops credentials and query strings (where
  presigned-URL and token secrets live).
- Deploy auth stores no secret on the box: the config holds only an
  OIDC issuer + claim allowlist, or a public key. The deploy token
  arrives per request as `Authorization: Bearer`, which Caddy redacts
  from access logs automatically.
- The packaged systemd unit deliberately does **not** use `--environ`
  (unlike Caddy's dist unit), so ACME DNS tokens and any other
  supervisor secrets never land in the journal — journals get pasted
  into bug reports. The package smoke test asserts this.

What's yours to handle:

- Keep the **local signing key** (`deploy_trust local`) off the box —
  it belongs on the machine that mints tokens. The box needs only the
  `.pub`. (CI OIDC avoids a stored key entirely.)
- Your app's **stdout/stderr go to the journal** under the identifier
  `hotserve-<app>` (`journalctl -t hotserve-blog`, or by unit name —
  the status endpoint reports it). If your app prints its own secrets
  at startup, they end up in the journal — that one's on the app.

## Deploying from CI

### GitHub Actions

No deploy secret to store — the job mints an OIDC token per run
(matching a `deploy_trust github { audience hotserve; claim repository
your-org/blog }` block on the box):

```yaml
permissions:
  id-token: write               # required to mint the OIDC token
  contents: write               # (for the release upload below)

steps:
- name: Build artifact
  run: |
    npm ci && npm run build
    tar -czf blog.tar.gz -C dist .

- name: Upload release
  uses: softprops/action-gh-release@v2
  with:
    tag_name: build-${{ github.run_number }}
    files: blog.tar.gz

- name: Deploy
  run: |
    JWT=$(curl -sH "Authorization: Bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
      "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=hotserve" | jq -r .value)
    curl --fail --max-time 600 -X POST \
      -H "Authorization: Bearer $JWT" \
      -d '{
        "url": "https://api.github.com/repos/${{ github.repository }}/releases/assets/'"$ASSET_ID"'",
        "version": "build-${{ github.run_number }}",
        "auth_header": "token ${{ secrets.GITHUB_TOKEN }}"
      }' \
      https://deploy.example.com/blog
```

(`auth_header` is a separate, artifact-download credential — the token
that reads a private release asset — not the deploy token.)

(For public repos, the plain `browser_download_url` works and needs no
`auth_header`.)

### GitLab CI

```yaml
deploy:
  stage: deploy
  id_tokens:
    HOTSERVE_JWT:               # verified by `deploy_trust gitlab { audience hotserve }`
      aud: hotserve
  script:
    - tar -czf blog.tar.gz -C dist .
    - |
      curl --fail --header "JOB-TOKEN: $CI_JOB_TOKEN" \
        --upload-file blog.tar.gz \
        "$CI_API_V4_URL/projects/$CI_PROJECT_ID/packages/generic/blog/$CI_COMMIT_SHORT_SHA/blog.tar.gz"
    - |
      curl --fail --max-time 600 -X POST \
        -H "Authorization: Bearer $HOTSERVE_JWT" \
        -d "{
          \"url\": \"$CI_API_V4_URL/projects/$CI_PROJECT_ID/packages/generic/blog/$CI_COMMIT_SHORT_SHA/blog.tar.gz\",
          \"version\": \"$CI_COMMIT_SHORT_SHA\",
          \"auth_header\": \"Bearer $DEPLOY_READ_TOKEN\"
        }" \
        https://deploy.example.com/blog
```

`--fail` makes the CI job red exactly when the deploy fails — and on
failure the previous version never stopped serving.

## Server layout

```
/var/lib/liveswap/blog/
  releases/v1.4.2/        one dir per deployed version
  releases/v1.4.1/
  current -> releases/v1.4.2   (convenience symlink; state.json is truth)
  shared/                 persistent data, survives deploys (the app's HOME)
  state.json              current version + process handle + sandbox tier
  tmp/                    download staging
```

Inside its sandbox an instance sees only `releases/<its version>/`
and `shared/` of this tree (see [Sandbox](#sandbox)).

## Semantics and trade-offs (read this)

- **Apps are systemd units, not children of hotserve.** Each instance
  is a transient service under the hotserve user's own systemd manager
  (`user@<uid>.service`, kept alive by lingering — the package sets
  this up; self-managed installs need `loginctl enable-linger
  hotserve` and `libpam-systemd`, and hotserve refuses to start apps
  without that manager). Config reloads never touch them (deploy state
  lives outside the config, reference-counted across reloads — proven
  by an e2e scenario that reloads mid-traffic and asserts the app's
  PID is unchanged), and neither do **hotserve restarts and upgrades**:
  on start, liveswap reattaches to the unit recorded in `state.json`
  and serves it immediately; only if that unit is gone (reboot, or it
  died meanwhile) is the current version relaunched. Stopping hotserve
  therefore leaves apps running until the next start; removing the
  package stops them. Removing an app (or the whole `liveswap` block)
  via a **reload** stops its units; if you instead edit the file and
  *restart* hotserve with the whole block gone, nothing is left to
  judge the old units and they keep running — decommission them
  explicitly: `sudo -u hotserve XDG_RUNTIME_DIR=/run/user/$(id -u
  hotserve) systemctl --user stop 'hotserve-*'`. Units are created with `Restart=no` — the
  watchdog is the only restarter — and stopping a version kills its
  whole cgroup, so worker trees never outlive it.
- **Changed app definitions apply on the next deploy**, never by
  restarting a running app mid-reload — the sandbox tier included: a
  relaunch reproduces the tier the instance was recorded with.
- **No post-promote *auto*-revert.** Once traffic cuts over, the deploy
  is done; if the new version misbehaves later, roll back explicitly
  with `?rollback=<version>` (its release dir is still on disk — that's
  what `keep` is for). Everything *before* promote is automatically
  contained. The
  watchdog restarts the *same* version on crash or sustained health
  failure — it never reverts to an older one.
- **One deploy at a time per app** — concurrent webhooks get 409, and
  CI retries are the queue. Different apps deploy in parallel.
- **Single node by design.** This is for the 1-server indie stack, not
  a cluster.
- **Linux with systemd only.** liveswap talks to the systemd user
  manager over D-Bus; there is no other process runner. Development on
  macOS happens in Docker (see below).
- Deploy tokens and artifact-URL query strings never appear in logs.

## Development

liveswap lives in the [hotserve](../) monorepo; the make targets at the
repo root cover it (no local Go toolchain needed — everything runs in
Docker):

```sh
make test              # unit tests, all modules (race + coverage)
make test-integration  # real Caddy + real systemd units via caddytest (privileged systemd container)
make e2e               # both module suites against the hotserve binary
make lint vet tidy
```

## License

Apache-2.0
