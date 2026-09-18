# hotserve

**The Hot Sauce server.** One binary that is your reverse proxy, your
deploy pipeline, your rate limiter, and your page cache — for indie
hackers, solo devs, and small businesses running real apps on cheap
servers. No Docker, no Kubernetes, no SSH keys in CI. Powered by
[Caddy](https://caddyserver.com).

```
git push  →  CI builds app.tar.gz  →  webhook  →  hotserve swaps it live. Zero downtime.
```

Built in:

| Module | What it does |
|---|---|
| **[liveswap](liveswap/)** | Zero-downtime app deploys: webhook from CI, artifact download, migrations, health-gated start, atomic traffic cutover, graceful stop, versioned releases with rollback. Your apps run as systemd units under the hotserve user's own service manager — Node.js, Go, anything that can listen on a unix socket — surviving hotserve restarts and upgrades, with a continuous watchdog that restarts them on crash or sustained health failure (a restart budget and backoff bound the rate; it throttles rather than giving up). |
| **[penaltybox](penaltybox/)** | Rate limiting driven by your app's `X-Rate-Limit-Level` hint headers — weighted sliding-window budgets, tiers, and a penalty box for clients that cross them. |
| **cache** | HTTP page caching via [Souin](https://github.com/darkweak/souin) with in-memory [Otter](https://github.com/darkweak/storages) storage. |
| everything Caddy has | Automatic HTTPS, HTTP/2 + HTTP/3, the Caddyfile, the admin API — hotserve *is* Caddy underneath, with the modules above compiled in. |

## Install

A `.deb` for amd64 and arm64 on the
[releases](https://github.com/smallhoursorg/hotserve/releases) page.
[Your first deploy](docs/first-deploy.md) installs it and deploys an
app, in four steps; [Upgrading](docs/upgrading.md) is every release
after that. The package gives you `/usr/bin/hotserve`, a systemd
service running as the `hotserve` user, a starter config at
`/etc/hotserve/Caddyfile`, and a systemd instance for the `hotserve`
user that stays up without anyone logging in — the thing your apps
will run under.

**Supported: Debian 13, on a real virtual machine.** hotserve installs
on other systemd distributions, but nothing else is tested, and a host
that cannot deliver the per-app sandbox — many LXC-based and other
container-style servers — refuses to start rather than serving
something weaker; [liveswap/README.md](liveswap/README.md#sandbox)
has what a host has to provide.

The package depends on `libpam-systemd` and `dbus`, both present on a
stock Debian server: liveswap runs your apps as systemd units under
the `hotserve` user's own service manager, which needs `pam_systemd`
to start and `loginctl` to stay alive without a login.

<details>
<summary><b>Without the package</b> — the raw binary, for other systemd hosts</summary>

The `hotserve_<version>_linux_<arch>.tar.gz` archives on the same page
hold the **raw binary** (plus LICENSE and a README), for other systemd
hosts you wire up yourself — a NixOS-style distro, say. Prefer the
packages where you can: going this way you take on what the package
does for you, namely a dedicated `hotserve` user, a `Type=notify`
unit, `loginctl enable-linger hotserve` (with `libpam-systemd`
installed) so the user manager exists for your apps, and the config at
`/etc/hotserve/Caddyfile`. The sandbox applies here too, so the host
has to deliver both namespaces — many container and LXC hosts cannot,
and hotserve refuses to start rather than run your apps unprotected.

**systemd is not optional.** liveswap runs your apps as transient
units on your user's systemd manager and there is no fallback runner,
so this is not a path onto a non-systemd host. hotserve would still
serve there — Caddy, the cache, penaltybox — but a Caddyfile defining
any `app` refuses to start. That is also why there is no `.apk`:
Alpine runs OpenRC, and an Alpine package would install a hotserve
without the feature it exists for.

</details>

(A hosted APT repository with automatic updates is on the roadmap.)

**Installing needs root; administering does not.** Checking a config
(`hotserve validate`) needs no privilege, reading the journal —
hotserve's and every app's — needs the `adm` group, and changing the
config and reloading are eight fixed commands, listed in
[examples/box/sudoers](examples/box/sudoers). That is the `hotserve`
user's reach (its TLS keys, every app's data), not root's: hotserve
itself runs unprivileged. One thing reaches wider: `adm` reads the
whole system journal, not only hotserve's lines, so it is a trust
grant in its own right. The e2e suite administers its box that way.

## Getting started

[Your first deploy](docs/first-deploy.md) takes a fresh Debian 13
server to an app that deploys on every push to `main`, in four steps.
[After the first deploy](docs/after-first-deploy.md) then creates the
administrator, closes root login, puts the box's config in git and
turns on [backups](docs/backups.md).
Both use the three directories below, which the e2e suite builds and
deploys on every change, so what they say works:

- **[examples/node](examples/node)** — a single executable (Node's own
  build), so nothing is installed on the box. Largest tarball;
  simplest box. The tutorial's default. Start your own from the
  template it is published as, [hotserve-example-node](https://github.com/smallhoursorg/hotserve-example-node).
- **[examples/deno](examples/deno)** — `deno run` with a Deno installed
  under `/usr` on the box, and the runtime's permission flags held in
  the box's Caddyfile, where a compromised build cannot widen them.
  Its template is [hotserve-example-deno](https://github.com/smallhoursorg/hotserve-example-deno).
- **[examples/box](examples/box)** — the box's config, kept in a
  private repo of its own: the `Caddyfile` (which app, which repo may
  deploy it, where the deploy webhook answers), a `make push` that
  validates it on the box and never leaves an invalid file there, and
  the sudoers file above.

Both apps serve on the socket hotserve hands them, answer `/health`,
migrate before each version starts, stop cleanly, and deploy from
GitHub Actions with no stored secret.


## What hotserve is (and isn't)

- **A server product, distributed like Caddy.** Same CLI, same
  Caddyfile, same admin API — `hotserve run`, `hotserve reload`,
  `hotserve validate` all behave exactly as Caddy's do.
- **Made for one cheap server.** Apps run as systemd units under
  hotserve's own user manager, reached over unix sockets; no container
  runtime anywhere. That's why there's deliberately no Docker image.
- **Not a cluster.** Single-node by design. If you outgrow one server,
  you've outgrown hotserve — a good problem.

## Security

The threat model is [DESIGN-threat-model.md](DESIGN-threat-model.md);
to report something, [SECURITY.md](SECURITY.md). Three properties it
rests on:

- **The admin API is not on localhost.** It listens on a unix socket
  rather than TCP: "localhost-only" would include every app you run,
  and one SSRF bug in an app could otherwise reconfigure the server.
  Apps themselves run unprivileged, with `NoNewPrivileges`.
- **Every app is sandboxed, with no opt-out.** Apps share the
  `hotserve` user, so each runs in its own systemd sandbox: it sees
  its own release, its `shared/` data, a private `/tmp`, and the parts
  of the OS it needs. Nothing else on the host exists in its view —
  hotserve's keys and every sibling app are *absent* from its
  filesystem and invisible in its process table, not merely
  unreadable. A host that cannot deliver that refuses to start rather
  than run something weaker. What is shared by design is the network,
  for outbound calls; nothing hotserve runs listens on a port.
  Details: [liveswap/README.md](liveswap/README.md#sandbox).
- **Deploys are authenticated without a shared secret.** A deploy
  carries a short-lived token — an OIDC token from CI, verified
  against the provider's public keys, or one signed by a local key
  whose public half the box holds. Nothing an attacker can steal off
  the box lets them deploy.

## Roadmap

Per-app sandboxing is shipped and unconditional — see **Security**
above for what an app can reach,
[liveswap/README.md](liveswap/README.md#sandbox) for the unit's own
settings, and [DESIGN-threat-model.md](DESIGN-threat-model.md) for why
each one is there. What is still ahead:

- **Resource caps.** `MemoryMax=`, `TasksMax=` and `CPUQuota=` hold
  inside the unit already, and are left unset by design until an app
  needs bounding (#71).
- **Per-app UIDs.** Apps share the unprivileged `hotserve` user; giving
  each its own would need a small privileged helper, so it stays a
  later milestone.
- **A hosted APT repository**, with package signing and auto-updates.

Not committed to, but worth naming: a metrics/alerts module alongside
liveswap and penaltybox. The first thing it would earn its keep on is
alerting when the watchdog is stuck in a restart loop — the watchdog
retries forever by design, so the loop itself is the signal that a
release is broken.

## Development

No local Go toolchain needed — everything runs in Docker:

```sh
make test              # unit tests, all modules (race + coverage)
make test-integration  # real deploys through caddytest, under a real systemd user manager
make e2e               # full stack: both module suites against the shipped binary under systemd, then restart survival + crash recovery
make lint vet tidy     # golangci-lint (gofmt-gated), go vet, go mod tidy
make vulncheck         # govulncheck, all modules (tool dep in go.mod — Dependabot-bumped)
make secretscan        # gitleaks full-history secret scan (same image as the CI gate)
make fuzz              # fuzz the untrusted-input surfaces (FUZZTIME=2m per target)
make fuzz-list         # what fuzz will run (discovered per module, not listed by hand); CI runs it on every PR
make soak              # ~20min leak hunt: deploy/reload churn, goroutine/fd assertions; CI runs it per merge to main and weekly
make build             # cross-compile linux amd64/arm64
make package           # .deb via nfpm, into dist/
make install-test      # install dist/'s .deb under real systemd (Debian 13);
                       #   run `make package` first — dist/ is not rebuilt
```

`test-integration`, `e2e` and `install-test` boot systemd inside a
privileged container, which needs a cgroup-v2 Docker host — Docker
Desktop (macOS/Windows) or Linux with systemd both qualify; the targets
check and say so up front. That is also how liveswap's systemd runner
is developed and tested on a Mac: nothing here needs a Linux VM.

The repo is a Go multi-module workspace: `liveswap/` and `penaltybox/`
are lean, independently usable Caddy modules
(`xcaddy build --with github.com/smallhoursorg/hotserve/liveswap`),
and the root module builds the product binary from `cmd/hotserve`.

### Dependency policy

Dependabot opens weekly grouped PRs per ecosystem with a 7-day
cooldown (security updates bypass it); patch/minor bumps auto-merge
once the full CI graph is green. Two escape hatches, both declared in
[`.github/pin-watch.yml`](.github/pin-watch.yml) and enforced by the
weekly `pin-watch` workflow:

- **Pins.** Every `ignore` in `dependabot.yml` must have a `watches`
  entry declaring the machine-checkable condition under which the pin
  is lifted (e.g. "caddy's latest release requires cel-go ≥ 0.29").
  The workflow opens an issue when a condition is met, and flags any
  ignore↔watch drift — a pin can never rot silently.
- **Alert triage.** `alert_dismissals` entries auto-dismiss Dependabot
  alerts by GHSA id, and `code_scanning_dismissals` entries do the same
  for CodeQL alerts by rule + file (with an `expect` cap so a new flow
  in the same file is never silently swallowed) — each with a reviewed
  reason and evidence comment, so triage decisions are versioned
  instead of buried in the UI. Unlisted alerts stay open and notify as
  normal. This needs the
  `DEPENDABOT_ALERTS_TOKEN` Actions secret — a fine-grained PAT with
  **Dependabot alerts: read-write** on this repository only — because
  the Actions `GITHUB_TOKEN` cannot access the alerts API.

## Dependencies

You're being asked to install this on a production server, so
here's exactly what's in the binary and what it drags in. Module counts
are measured from `go mod graph` (deduplicated by module path); the
"pulls in" column shows what each dependency adds *beyond* what's
already in the tree above it, because the trees overlap almost
entirely. (Counts exclude the `govulncheck` tool dependency — a
`tool` directive is never compiled into the product and never
inherited by importers; it exists in go.mod so Dependabot can bump
the scanner.)

| Dependency | Pulls in (~modules) | hotserve | liveswap | penaltybox | Notes |
|---|---|:-:|:-:|:-:|---|
| [Caddy](https://github.com/caddyserver/caddy) | ~565 | ✓ | ✓ | ✓ | The foundation — hotserve *is* a Caddy distribution. Commercially sponsored, strong security track record. Nearly the entire dependency tree is Caddy's. |
| [Souin](https://github.com/darkweak/souin) (`cache`) | +24 | ✓ | — | — | HTTP cache (RFC 7234). Solo-maintained; the e2e suite exercises it directly so drift is caught in CI. The riskiest dependency here, and still better than an in-house HTTP cache. |
| [darkweak/storages/otter](https://github.com/darkweak/storages) | +6 | ✓ | — | — | In-memory storage backend for Souin, wrapping [maypok86/otter](https://github.com/maypok86/otter). |
| [zap](https://github.com/uber-go/zap) | 0 (already in Caddy's tree) | ✓ | ✓ | ✓ | Caddy's module logging API is zap; not optional for a Caddy module. |
| [go-humanize](https://github.com/dustin/go-humanize) | 0 (already in Caddy's tree) | ✓ | ✓ | — | A few formatting helpers in liveswap. |
| [go-systemd](https://github.com/coreos/go-systemd) (`dbus`) + [godbus](https://github.com/godbus/dbus) | +1 | ✓ | ✓ | — | liveswap's runner: apps are transient units created over systemd's D-Bus API on the hotserve user's own manager (no polkit, no root). go-systemd was already in the workspace graph; godbus is its one dependency, pure Go. |
| [go-oidc](https://github.com/coreos/go-oidc) + [go-jose](https://github.com/go-jose/go-jose) | 0 (already in Caddy's tree) | ✓ | ✓ | — | Deploy authentication (`deploy_trust`): OIDC discovery/JWKS verification and JWT signing/verification. Both were already transitive dependencies of Caddy; liveswap now requires them directly. Vetted and widely used — the deliberate alternative to hand-rolling JWT crypto. |

A ✓ in the liveswap and penaltybox columns means that module's own
code requires the dependency. A "—" there is not "absent from that
module's graph" — the rows marked "already in Caddy's tree" arrive via
Caddy either way. The hotserve column is ✓ throughout: the product
binary contains every row.

**Two programs the package installs beside the binary, and never links
into it:** `restic` and `sqlite3`, both from Debian, as
`Recommends:` — apt installs them by default, and removing hotserve
marks them as no longer required, so `apt autoremove` clears them (a
plain `apt remove hotserve` leaves them in place). [Backups](docs/backups.md) shell out to them
(`hotserve backup` prints each command as it runs); nothing else does,
and a box that removes them serves exactly as before. They are
deliberately not Go dependencies: a backup tool inside the serving
binary would add its cloud SDKs to every install, backups or not.

Build and CI tooling never ships to users and is pinned by image tag
in `docker-compose.yml`: `golang` (toolchain), `golangci-lint`,
`gitleaks` (secret scan), `nfpm` (deb packaging) and `curl` (the e2e
runner). GitHub Actions are pinned to commit SHAs.

What keeps this honest: `govulncheck` gates every PR and runs weekly
against the fresh vulnerability database (reachable-code analysis, all
modules), every release is blocked until the full test matrix passes —
including installing the actual `.deb` under systemd on Debian 13, on
both architectures — and any dependency bump has to survive all of the
above before it merges.

## License

Apache-2.0. hotserve is powered by [Caddy](https://caddyserver.com),
[Souin](https://github.com/darkweak/souin), and
[Otter](https://github.com/darkweak/storages).
