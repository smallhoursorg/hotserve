# liveswap

**Your reverse proxy is your deploy pipeline.** liveswap turns a
single Caddy server into a zero-downtime deploy orchestrator for the
apps it fronts — Node.js, Go, anything that can listen on a unix
socket. CI builds a tarball, POSTs a webhook with its URL, and Caddy
does the rest: download, migrate, start the new version on its own
socket, health-gate it, atomically cut traffic over, gracefully stop
the old one. No Kubernetes, no Nomad, no SSH keys in CI, no extra daemons.
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
  │ starting      spawn the app on a fresh unix socket, SOCKET injected
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

Build Caddy with the module:

```sh
xcaddy build --with github.com/smallhoursorg/hotserve/liveswap
```

Or take the prebuilt binary from
[hotserve](https://github.com/smallhoursorg/hotserve/releases), which
ships liveswap compiled in — on Debian 13 the `.deb` sets up everything
below for you.

**The server needs systemd.** liveswap runs your apps as transient
units on the service user's own systemd manager, so the box also needs
`libpam-systemd`, `dbus`, and `loginctl enable-linger <user>`; without that
manager, a config defining any app refuses to start. A container image
is not a deployment target for the same reason — there is no user
manager in one, and the per-app [sandbox](#sandbox) needs namespaces
most container hosts will not give you.

## Caddyfile

```caddyfile
{
	# Keep the admin API off TCP: every app can reach localhost. This
	# is the hotserve package's path (its unit creates /run/hotserve and
	# `systemctl reload` finds the socket here); a self-managed unit
	# needs RuntimeDirectory=hotserve, or another dir its user owns.
	admin unix//run/hotserve/admin.sock

	liveswap {
		# root /var/lib/liveswap                # where releases/state live (default)
		artifact_allowlist github.com/your-org/ # required: where artifacts may come from
		# allow_insecure_http                  # permit http:// artifact URLs (off by default)

		# A deploy_trust block may also sit here, as the default for
		# apps that declare none of their own. Claims that name one
		# repo belong with the app they authorize, as below — a
		# per-app block REPLACES the global one, it does not add to it.

		app blog {                             # a Node.js app
			command node server.js             # runs with CWD = the release dir
			pre_start node migrate.js          # optional; non-zero exit aborts the deploy
			env_file /etc/hotserve/blog.env    # optional KEY=VALUE file
			env NODE_ENV production            # inline env, repeatable

			# Who may deploy this app — required (here or globally). A
			# deploy carries an `Authorization: Bearer <JWT>`; it is
			# authorized if any deploy_trust source verifies it. No
			# shared secret is ever stored on the box.
			deploy_trust github {                 # preset: GitHub Actions OIDC
				audience hotserve
				claim repository your-org/blog     # pin who may deploy
				claim ref        refs/heads/main
			}

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
			# max_artifact_entries 100000
		}

		app api {                              # a Go app
			command ./server --config config.yaml
			env DATABASE_URL sqlite:{shared_dir}/api.db
			deploy_trust github {                 # its own repo, its own block
				audience hotserve
				claim repository your-org/api
			}
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

Apps must listen on the unix socket at the injected `SOCKET` path —
one per instance, in a `run/<nonce>/` dir of its own; hotserve proxies to
it and health-probes it, and nothing listens on a TCP port. Their
environment is, lowest precedence first: an allowlisted slice of
Caddy's environment (`PATH`, `LANG`, `TZ`, `LC_*` — nothing else, so
supervisor credentials like ACME DNS tokens never reach apps) →
`HOME` set to the app's `shared/` → `env_file` → inline `env` →
injected `SOCKET`, all layered on the systemd user manager's own
defaults (`XDG_RUNTIME_DIR`, `INVOCATION_ID`, …). Two of those
defaults are **reserved** in a sandboxed unit and cannot be set by
`env` or `env_file`: `XDG_RUNTIME_DIR` and `DBUS_SESSION_BUS_ADDRESS`
are unset after everything else, because they name the user manager's
own runtime directory and bus — the sockets a sandboxed app must not
hold. Setting either has no effect rather than an error. Keys must be valid
variable names (`[A-Za-z_][A-Za-z0-9_]*`) — systemd rejects anything
else, so config load does too. Anything more an app needs must be
passed explicitly via `env` or `env_file`. Apps get the user manager's
resource limits; each unit sets its open-files limit (soft and hard)
to the manager's ceiling, which the package raises to match
`hotserve.service` (1048576) via a `user@<uid>.service.d` drop-in —
effective from the manager's next start: an upgrade installs the
drop-in but deliberately does not restart a running manager (that
would stop every app), so the ceiling changes at the next boot or
manual restart.

### Listening on the socket

The socket is a path, so every server that can listen on a unix
socket works unchanged; locally you keep using a port. One branch
covers both:

```js
// Node / Express: server.listen() takes a path as well as a port.
server.listen(process.env.SOCKET ?? 3000);
```

```ts
// Deno: the `deno serve` CLI is port-only; call Deno.serve yourself.
const sock = Deno.env.get("SOCKET");
Deno.serve(sock ? { path: sock } : { port: 8000 }, handler);
```

```go
// Go
ln, err := net.Listen("unix", os.Getenv("SOCKET"))
if err != nil {
	log.Fatal(err)
}
log.Fatal(http.Serve(ln, mux))
```

Bun: `Bun.serve({ unix: process.env.SOCKET, fetch })`. Hono on Node:
`createAdaptorServer(app).listen(process.env.SOCKET)`. Python:
`gunicorn --bind unix:$SOCKET`, `uvicorn --uds $SOCKET`. Ruby: `puma
-b unix://$SOCKET`. Rust: axum/hyper on a `tokio::net::UnixListener`.

Framework CLIs that only bind TCP (`next start`, Astro's node adapter)
need a small custom server that calls the framework's request handler
from an `http.Server` listening on the socket.

hotserve's health probe arrives as `GET <health_path>` with
`Host: localhost`; an app that allowlists hosts (Django's
`ALLOWED_HOSTS`, Rails' `config.hosts`) must accept it. To poke a
running app yourself, run as the hotserve user — `run/` is
`0750 hotserve:hotserve` — and send the same request:
`sudo -u hotserve curl --unix-socket "$SOCKET" http://localhost/health`
(the path is `.socket` in the status JSON).

Bind the socket **once** per instance. At the first successful
connect hotserve hard-links the socket's inode into a directory of
its own (`<app>/proxy/`, outside the sandbox) and dials that from
then on — never the name under `run/<nonce>/`, which the app can write — so
an app that unlinks and re-binds its path is, as far as hotserve is
concerned, gone: its health probes fail and the watchdog restarts it.
(With `health_path off` the watchdog only watches the process, so a
re-bound app stays up and unroutable — another reason not to
re-bind.) A stale socket file is never your problem either: the unit
removes its socket when it stops, and every launch gets a fresh path.

### Placeholders

`command`, `pre_start` and `env` values may use deploy-time
placeholders: `{version}`, `{socket}`, `{release_dir}`, `{shared_dir}`.
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
| `max_artifact_entries` | `100000` | Cap on the files, directories and links one artifact creates (implied parent directories included). The byte cap does not bound what extraction *consumes* — every object costs an inode and most a disk block — so this is what keeps one hostile artifact from filling the disk for everything else on the box. A CI-built artifact is thousands; a Next.js standalone output is ~5–20k |

Both caps are cliffs an app can grow into, so each successful deploy
logs the artifact's entry count and decompressed size and, past **75%**
of either cap, warns naming the directive to raise; `GET /<app>` shows
the last deploy's figures as `artifact_entries` / `artifact_bytes`.
Entry names and link targets are also bounded at PATH_MAX (4096
bytes, under the release directory) and NAME_MAX (255 per component),
and the content entries *declare* is capped like the stream (a sparse
entry expands from no stream at all); not knobs — no real path or
artifact is close.

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
the proxy fails cleanly instead of dialing a socket nothing is
listening on, waits for the
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

If your health endpoint can be slow under load, raise `health_timeout`
(probe timeouts count as failures). If it redirects — a `/health` →
`/health/` trailing slash is the classic — point `health_path` at the
final path, since a 3xx reads as unhealthy for the watchdog and the
deploy gate alike.

## Sandbox

Every instance runs inside systemd's own per-unit sandbox. There is
one sandbox, no setting that turns it off or widens it, and a host
either delivers it or hotserve does not start:

| What every unit gets |
|---|
| User namespace (`PrivateUsers=`) |
| PID namespace (`PrivatePIDs=`) — supervisor, user manager and siblings **invisible and unsignalable** |
| Deny-by-default filesystem: the whole host filesystem replaced by an empty read-only tmpfs, only named paths bound back |
| hotserve's keys and sockets, other apps, and every path nothing named — **absent**, not merely unreadable |
| Private `/tmp`, minimal `/dev`, read-only cgroupfs, no capabilities, `@system-service` syscall filter, `AF_INET`/`AF_INET6`/`AF_UNIX`/`AF_NETLINK` only, no nested namespaces |

`PrivatePIDs=` needs systemd 256; Debian 13 — the supported host —
ships 257. Where the namespaces cannot be had at all (a container, an
LXC VPS, a kernel built without user namespaces, an LSM that refuses
them) hotserve refuses to start, naming what the host lacks. It is
deliberately not a ladder, and there is deliberately no mode that runs
anyway: a weaker sandbox wearing the same name — or none at all — is
the "looks configured, quietly weaker" failure this design refuses.
An app with no sandbox would not be alone in paying for it: every app
runs as the hotserve user, so one bare app could read every sibling's
data and hotserve's own keys. That is why there is no per-app opt-out
either.

**An app sees its release dir, its `shared/`, a private `/tmp`, the OS
runtime. Nothing else on the host
exists in its view.**

That is the whole guarantee. The view is built deny-by-default
(`TemporaryFileSystem=/:ro` plus explicit binds), so it is a policy
rather than a list of things to hide: `state.json`, `tmp/` (the upload
staging dir), other releases, other apps, `/var/lib/hotserve` (TLS
keys), `/run/hotserve` (admin socket), `/run/user/<uid>` (the manager
socket), `/etc/hotserve`, `/home`, `/opt`, `/srv` and every operator
`env_file` are absent — not present-but-unreadable — and no list has
to be kept current for that to stay true.

"The OS runtime" is a named set, not the host: `/usr` and its usrmerge
aliases (`/bin`, `/sbin`, `/lib*`), the certificate directories of the
TLS trust store, and the individual `/etc` entries needed for name and
user resolution, timezone and the dynamic linker. `sandboxBaseView` in
`sandbox.go` is the list, and it names entries rather than the trees
containing them: `/etc` is not bound whole (that would hand every app
every other app's `env_file`), and neither is `/etc/ssl`, which also
holds `/etc/ssl/private`. Every entry is optional, since no distro has
all of them, so inside a unit `ls /etc` shows however many this host
actually has — a dozen or so — and `ls /var/lib` shows exactly one,
the liveswap root.

The working directory is the release dir and `HOME` defaults to
`shared/`; both are writable. `HOME` is applied before `env_file` and
inline `env`, so you can point it elsewhere — inside the release dir,
say. Point it somewhere the sandbox does not bind and
the app gets a `HOME` that does not exist inside its unit; liveswap
warns at every launch rather than refusing, since you asked for it.
The network namespace is shared by design — apps make outbound calls
and reach a same-box database over loopback — but nothing hotserve
runs listens on a port: each instance binds its own unix socket under
`run/<nonce>/`, which hotserve reaches at its real path, and a sibling's
socket is outside the view like everything else of the sibling's.

This closes the routes the threat model ranks first: reading the
supervisor's environment or walking the host through
`/proc/<pid>/root` (the **user** namespace is what closes them — the
kernel refuses `ptrace`-class access across user namespaces even for
the same uid), the admin socket, the TLS keys, sibling files. The
**PID** namespace adds the rest: process visibility and signals. Worth
keeping straight, because it is why a host that delivers neither is
refused rather than given something in between.

**What this costs you.** An app that reads something under `/opt`,
`/srv` or `/var/lib`, or whose runtime lives outside `/usr` (a
vendored Node, an `nvm`/`asdf` shim), cannot be reached at all —
those paths do not exist inside the unit. A deploy is where this
surfaces, and it has a fallback there: the health gate fails the new
version while the old one keeps serving. A command that is not in the
view is refused before the unit is even created, with a message that
says where the runtime has to live, rather than failing as a bare
`203/EXEC`.

**The host is measured at start.** Capability is probed by running a
throwaway unit and checking the namespaces from inside
(`journalctl -t hotserve-sandbox-probe` shows what it saw). A host
that cannot deliver the sandbox fails the start — admin socket and
proxy included — with the probe's reason. That measurement is cached
per connection to the user manager, so a reload does not repeat it;
a refused host is measured again on the next start or reload, so
fixing the host does take effect. Restarting the user manager
re-measures either way.

**The sandbox is an availability dependency.** A host that stops being
able to deliver it will not start hotserve until the host is fixed;
there is no setting that runs apps without one. That is deliberate —
the alternative is a supervisor that silently runs every app with no
isolation because the kernel changed its mind — so prove a box before
you restart into it. `hotserve validate` does not measure the host (it
never starts an app); a reload does, once: the verdict is held for the
life of hotserve's connection to the user manager, so the first start
or reload after hotserve dials the manager is the measurement and a
later reload reuses it. On a fresh box that first reload is the proof
you want, and a reload that cannot activate leaves the running config
serving where a restart does not. A host changed underneath a running
hotserve — a sysctl, an LSM policy load — is not re-measured until the
next restart, which is where it can refuse; there is deliberately no
uncached preflight, because the cache is what keeps a throwaway unit
off every reload.
Every launch is sandboxed the same way — a deploy, a crash relaunch,
boot recovery, a reattach after `systemctl restart hotserve` — so
there is nothing per instance to roll out and nothing to watch in
status: an app is running, or hotserve did not start.

**What a running instance's sandbox is fixed to.** A unit's view is
built when it starts and is never rebuilt under it — reloads
deliberately leave running apps alone — so a config change reaches an
app at its next launch (a deploy, a rollback, or a relaunch after a
crash or reboot), not before. That is the only
thing that ages now, and it fails safe: a secret belonging to an app
you add tomorrow is already absent from every unit running today,
because nothing ever bound it.

**Keep env files outside every app's view.** `/etc/hotserve` is the
documented location and no app may name it. hotserve refuses a config
in which one app's `env_file` sits inside another app's own dirs,
or anywhere in the OS base view (`/usr`, the named `/etc` entries) —
both would put one app's secrets in another app's sandbox, and the
second would put them in *every* app's. An `env_file` inside its own
app's `shared/` is a warning rather than an error: the app receives
those variables anyway, but under `shared/` it can also rewrite the
file and so choose its own next launch's environment.

hotserve reads the file itself, as the `hotserve` user, each time the
app launches (a deploy, a rollback, a relaunch after a crash or a
reboot); loading the config does not open it. So create it readable by
that user and nobody else —
`sudo install -m 0640 -o root -g hotserve blog.env /etc/hotserve/` —
and keep it in place: a missing or unreadable file fails that launch.

**The sandbox is not containment for what an app did before it had
one.** It restricts what an app can *reach*; it cannot un-copy. An
app's `shared/` survives every deploy and is bound writable into every
sandbox, so anything an instance put there before sandboxing existed
(a hotserve older than this, where every app ran as the shared user
with the whole host in reach) is inside the view afterwards. A
hardlink is worse than a copy: it stays a live view of the file, so
rotating a secret by editing it in place republishes it. If an app may
have been compromised on such a hotserve, clear its `shared/` and
rotate anything it could read — the sandbox does not undo access it
already had.

**When an app breaks under the sandbox** the symptom is usually an
`ENOENT` for something that exists on the host: a database's unix
socket, a data directory outside the app's own, a runtime under
`/opt`. **There is no way to widen a view.** An app sees the OS base
view, its own release dir and its own `shared/`. That is the whole
list, and it is fixed.

That is a deliberate limit, not an oversight. The one mechanism that
would widen a view is also the one that has to be correct against
symlinks, TOCTOU, cross-app containment and the base view all at once,
and getting it wrong hands one app another's secrets. It comes back
when a running app needs it and can be designed and reviewed on its
own; until then the answers below are the answers. There is no
per-app opt-out either: an app with no sandbox runs as the same user
as everything else and reaches all of it.

Practical consequences worth planning around:

- **Put persistent data in `shared/`.** It survives deploys, it is
  writable, and it is `$HOME` inside the unit. A SQLite file belongs
  there, not in `/var/lib/myapp`.
- **A same-box database is reached over TCP loopback**, which the
  shared network namespace allows; its unix socket is outside the
  view.
- **Install runtimes under `/usr`.** The base view binds it, so
  `/usr/local/bin/deno` or an apt-installed `node` is already inside
  every unit. A vendored runtime under `/opt`, or an `nvm`/`asdf` shim
  under a home directory, is not — ship it inside the release instead.

An app's own `releases/<version>` and `shared/` must **be** the
directories they name. hotserve resolves them immediately before the
unit is created and refuses to launch if either points somewhere else,
because the app dir is writable by the app: a symlink placed there
cannot be told from one you meant. To put an app's data on another
disk, bind-mount it at the same path (`mount --bind /mnt/blog-data
/var/lib/liveswap/blog/shared`, or the equivalent fstab entry) — a
mount resolves to itself, so it is invisible to that check, and an app
cannot forge one.

**Workloads the sandbox cannot host are not supported.** Anything
that creates its own namespaces (Chromium's sandbox under Puppeteer,
nested containers) or needs devices beyond the `/dev/null` class has
no place to run: there is no opt-out to put it in. That is a decision
to revisit when such an app exists, on its own terms, not a reason to
reopen the box for everyone.

**Hosts.** The sandbox is built on unprivileged user namespaces.
Debian 13's kernel permits them, which is why it is the supported
host and why the package ships no LSM policy of its own. Some kernels
refuse them — an LSM restriction on unprivileged user namespaces, or a
container or LXC host with them off entirely. hotserve does not work around
that: it probes, and a host that cannot deliver the namespaces refuses
to start rather than running apps bare. `journalctl -t
hotserve-sandbox-probe` says why it refused. The fix is the host's:
permit unprivileged user namespaces for hotserve's user manager, or
move to Debian 13. There is no way to run there without the sandbox.

## Runtime permissions (Deno, Node)

The sandbox above is the **ceiling**: it is enforced by the kernel, it
is decided by your Caddyfile, and it survives a compromised artifact.
A runtime with its own permission model lets the app **narrow itself
further inside that ceiling** — and it can express things a mount
namespace cannot, most importantly *which network addresses the
process may reach*.

liveswap does not parse, synthesize or verify these flags. They are
just part of `command`, which is the point: `command` lives in your
Caddyfile on the box, so a deployed tarball cannot widen its own
permissions the way it could if they lived in the artifact.

A Deno app, with the placeholders from [Placeholders](#placeholders)
and a `main.ts` that calls `Deno.serve({ path: Deno.env.get("SOCKET") })`
(see [Listening on the socket](#listening-on-the-socket)):

```
app example {
    command deno run \
        --cached-only \
        --allow-read={release_dir},{shared_dir},{socket} \
        --allow-write={shared_dir},{socket} \
        --allow-env=SOCKET,DATABASE_URL \
        main.ts
    env DENO_DIR {release_dir}/.deno
    env DATABASE_URL {shared_dir}/app.db
    health_path /health
}
```

**What that buys you** (measured against Deno 2.8.3; re-check with
`deno run --help=full` when you upgrade):

- **No `--allow-net` at all.** A unix socket is a file: serving needs
  read and write on `{socket}` and nothing else, so every network
  address is refused with `NotCapable` — a dependency that wakes up
  and tries to POST your secrets somewhere fails at the runtime
  boundary. Add `--allow-net=<host>` only for the hosts the app must
  call. (`--allow-net` is an **address allowlist, not a direction**:
  it does not distinguish listening from connecting.) The same holds
  for a `deno compile`d binary: the flags are baked in, and a socket
  path needs no per-deploy value baked with them.
- Without `--allow-read` the app cannot read `/etc/hosts`, and without
  `--allow-env` it cannot read its own environment — including the
  `SOCKET` liveswap injected. Name only what the app actually needs.
- Remote imports are governed by `--allow-import`, **not**
  `--allow-net`. Leave it off and add `--cached-only` so the serving
  process can never fetch a module. Vendor dependencies at build time
  and ship the populated cache **inside the tarball**, with `DENO_DIR`
  pointing into the release dir as above — the tarball is extracted
  there, so a cache shipped in it is the one Deno reads. Pointing
  `DENO_DIR` at `{shared_dir}` instead means the shipped cache is never
  consulted and a first `--cached-only` start fails; if you want the
  cache to survive deploys, warm it into `{shared_dir}` from
  `pre_start` and point `DENO_DIR` there.

**The runtime must be under `/usr`.** The sandbox base view
binds `/usr`, so a normal `/usr/local/bin/deno` (or an apt-installed
one) is already inside every unit. A runtime somewhere else —
`/opt/node/bin/node`, an nvm or asdf shim — is absent from the
sandbox and cannot be reached: ship it inside the release.

**What it does not buy you.** These flags are enforced *in-process* by
the runtime, so they hold exactly as long as the runtime does: `-A` /
`--allow-all`, `--allow-run` or `--allow-ffi` hand it all back, and a
bug in the runtime itself is outside their reach. That is why they are
the inner layer and not the only one — the user and PID namespaces and
the deny-by-default view are what still stand if the runtime is the
thing that breaks. Use both; do not trade one for the other.

**Health.** `Deno.serve` hands every path to your handler, so
`health_path` only works if it answers 2xx on it. If it does not, set
`health_path off` and the deploy gate falls back to "the process is
still alive after `soak`".

Node's `--permission` model layers the same way for the filesystem,
child processes and workers — it does not gate the network. The
principle is identical whatever the runtime: the ceiling is
liveswap's, the narrowing is the app's, and neither substitutes for
the other.

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
  "url": "https://github.com/your-org/blog/releases/download/v1.4.2/blog.tar.gz",
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
curl --fail-with-body -X POST -H "Authorization: Bearer $JWT" \
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
curl --fail-with-body -X POST -H "Authorization: Bearer $JWT" \
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
| 400 | The body could not be read, or is not valid JSON |
| 401 | Bad or missing token |
| 404 | Unknown app |
| 405 | A method other than `GET` or `POST` |
| 409 | A deploy is already running for this app (retry) |
| 413 | Pushed upload exceeded `max_artifact_size`, or a JSON body exceeded 64 KiB |
| 422 | Bad request — missing/invalid version, version already running, **version already exists** (versions are immutable — deploy a new version or roll back to relaunch it), a rollback target no longer on disk, or (URL path) an artifact url refused by `artifact_allowlist` (host, path, port, or an undeclared query parameter; the body names exactly what tripped and how the entry would declare it) |
| 429 | This token failed, and the address has already failed 10 times this minute; `Retry-After` says when the oldest failure ages out. A *valid* token from the same address is never refused — see [Secrets and logs](#secrets-and-logs) |
| 5xx | Deploy failed — **the old version is still serving**; body says why. An artifact over `max_artifact_entries` or the decompressed byte cap fails here, before anything is written |

Because the response is synchronous through the whole pipeline, the
POST's wall time includes the health soak, the `drain` pause and the
old version's graceful stop — with defaults, a healthy deploy answers
in roughly soak + drain (~20s). Budget your CI step timeout for
`deadline` plus drain and grace, and expect a concurrent deploy to
409 until the first one finishes.

`GET /<app>` (same bearer token) returns status: phase, current
version, socket, pid, `command` — the argv the running instance was
actually launched with, read back from systemd, which is not
necessarily what the config says now (a reload does not restart a
running app, so an edited `command` applies at the next launch: a
deploy, a rollback, or a relaunch after a crash or reboot). It is the
*rendered* argv, not the configured text:
`command ./server {version}` reports as
`["/var/lib/liveswap/blog/releases/v1.4.2/server", "v1.4.2"]`, since
the unit runs an absolute resolved path with the placeholders already
substituted. Compare it against the release it should be running, not
against the directive. Anything you put in `command` is readable by
anyone who can read status, so keep secrets in `env`, which status
does not report. Then: last deploy result (including `deployed_by` and
the artifact's `artifact_entries` / `artifact_bytes` against the caps),
the watchdog's state (restart counts, last restart cause), and
`available_versions` — the on-disk releases you can roll back to,
newest-first.

The tarball's contents must sit at the archive root (`tar -czf
app.tar.gz -C dist .`), with versions matching `[A-Za-z0-9._-]{1,64}` and not starting with a dot.

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
- Failed webhook authentications are **throttled in the journal**.
  Nobody can guess a token — forging one needs a private key — so the
  throttle is not about guessing; it bounds what an unauthenticated
  flood can write to your journal. Per client address, 10 failures a
  minute; further bad tokens from that address get `429` until the
  oldest failure ages out, and one line records that the address is
  throttled. Process-wide, 100 failures a minute
  are logged however many addresses a flood comes from, again with one
  line saying the budget is spent; that process budget governs every
  line, so once it is spent an address is throttled silently. The
  address is Caddy's `client_ip`
  (so `trusted_proxies` is honoured), IPv6 keyed by /64. The token is
  still verified for a throttled address: a valid one is admitted and
  clears the address, so sharing an address with a flood (a NAT, a CI
  egress pool, a proxy without `trusted_proxies`) never costs a
  deploy. Fixed, not configurable, and it survives config reloads.

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
  id: release
  uses: softprops/action-gh-release@v2
  with:
    tag_name: build-${{ github.run_number }}
    files: blog.tar.gz

- name: Deploy
  env:
    ASSET_URL: ${{ fromJSON(steps.release.outputs.assets)[0].url }}
    GH_TOKEN: ${{ github.token }}
  run: |
    JWT=$(curl -fsS -H "Authorization: Bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
      "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=hotserve" | jq -er .value)
    echo "::add-mask::$JWT"
    curl --fail-with-body --max-time 600 -X POST \
      -H "Authorization: Bearer $JWT" \
      -d '{
        "url": "'"$ASSET_URL"'",
        "version": "build-${{ github.run_number }}",
        "auth_header": "token '"$GH_TOKEN"'"
      }' \
      https://deploy.example.com/blog
```

`ASSET_URL` is the uploaded asset's API URL
(`https://api.github.com/repos/your-org/blog/releases/assets/<id>`),
so the box needs `artifact_allowlist api.github.com/repos/your-org/`:
hosts match exactly, and `github.com/your-org/` does not admit it.
(`jq -er` fails the step if the mint returned nothing, instead of
sending an empty bearer and blaming `deploy_trust`.)

(`auth_header` is a separate, artifact-download credential — the token
that reads a private release asset — not the deploy token. It is the
job's own token, which expires when the job ends.)

(For public repos, the plain `browser_download_url`
(`https://github.com/your-org/blog/releases/download/<tag>/blog.tar.gz`)
works with `artifact_allowlist github.com/your-org/` and needs no
`auth_header`.)

Mint and use the deploy token in the one step. Interpolating it into
a later step's `run:` script (`${{ steps.….outputs… }}`) prints it in
the job log, and until it expires anyone who reads the log can deploy;
if a later step must have it, pass it through `env:` and keep the
`::add-mask::`.

A version the box already has gets a `422` — versions are immutable —
so a re-run of a job that deployed fails at this step rather than
deploying twice. A deploy that fails is cleaned up, so its version can
be deployed again — unless the 5xx body says `release <version> left
on disk` (the failed instance could not be confirmed stopped, so the
dir is kept rather than pulled from under a process that may still
run) or `cleanup of failed release` (removing it failed). Either way
that version stays a 422 until release GC — which runs after a
*successful* deploy and keeps the newest `keep` dirs — drops it; use
a new version.

### GitLab CI

```yaml
deploy:
  stage: deploy
  id_tokens:
    HOTSERVE_JWT:               # verified by `deploy_trust gitlab { audience hotserve; claim project_path your-org/blog }`
      aud: hotserve
  script:
    - tar -czf blog.tar.gz -C dist .
    - |
      curl --fail-with-body --header "JOB-TOKEN: $CI_JOB_TOKEN" \
        --upload-file blog.tar.gz \
        "$CI_API_V4_URL/projects/$CI_PROJECT_ID/packages/generic/blog/$CI_COMMIT_SHORT_SHA/blog.tar.gz"
    - |
      curl --fail-with-body --max-time 600 -X POST \
        -H "Authorization: Bearer $HOTSERVE_JWT" \
        -d "{
          \"url\": \"$CI_API_V4_URL/projects/$CI_PROJECT_ID/packages/generic/blog/$CI_COMMIT_SHORT_SHA/blog.tar.gz\",
          \"version\": \"$CI_COMMIT_SHORT_SHA\",
          \"auth_header\": \"Bearer $DEPLOY_READ_TOKEN\"
        }" \
        https://deploy.example.com/blog
```

On gitlab.com that URL needs `artifact_allowlist
gitlab.com/api/v4/projects/<project id>/` on the box.
`DEPLOY_READ_TOKEN` is the artifact-download credential: a project
access token with `read_api`, stored as a masked CI/CD variable. The
job token is not a substitute: `auth_header` is sent as the
`Authorization` header, and GitLab takes `CI_JOB_TOKEN` in a
`JOB-TOKEN` header, as the upload line above shows.

`--fail-with-body` makes the CI job red exactly when the deploy fails
and prints the JSON `error` that says why (plain `--fail` hides it) —
and on failure the previous version never stopped serving. The app's
own output (a crashing start, a failing `pre_start`) is not in that
body: it is in the journal on the box, `journalctl -t hotserve-blog`.

## Server layout

```
/var/lib/liveswap/blog/
  releases/v1.4.2/        one dir per deployed version
  releases/v1.4.1/
  current -> releases/v1.4.2   (convenience symlink; state.json is truth)
  shared/                 persistent data, survives deploys (the app's HOME)
  state.json              current version, nonce and unit name
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
  PID is unchanged) — so an edited `command` or `env` does not apply
  to the running instance; it applies at the next launch (a deploy, a
  rollback, a relaunch after a crash or reboot), which is why status
  reports the `command` the instance is actually running — and
  neither do **hotserve restarts and upgrades**:
  on start, liveswap reattaches to the unit recorded in `state.json`
  and serves it without relaunching it (what visitors see while
  hotserve itself restarts: [Upgrading](../README.md#upgrading));
  only if that unit is gone (reboot, or it died meanwhile) is the
  current version relaunched. Stopping hotserve
  therefore leaves apps running until the next start; removing the
  package stops them. Removing an app (or the whole `liveswap` block)
  via a **reload** stops its units; if you instead edit the file and
  *restart* hotserve with the whole block gone, nothing is left to
  judge the old units and they keep running — decommission them
  explicitly: `sudo -u hotserve XDG_RUNTIME_DIR=/run/user/$(id -u
  hotserve) systemctl --user stop 'hotserve-*'`. Units are created with `Restart=no` — the
  watchdog is the only restarter — and stopping a version kills its
  whole cgroup, so worker trees never outlive it.
- **Changed app definitions apply at the app's next launch** — a
  deploy, a rollback, or a relaunch after a crash or reboot — never by
  restarting a running app mid-reload.
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
