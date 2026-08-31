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
| **[liveswap](liveswap/)** | Zero-downtime app deploys: webhook from CI, artifact download, migrations, health-gated start, atomic traffic cutover, graceful stop, versioned releases with rollback. Your apps run as systemd units under the hotserve user's own service manager — Node.js, Go, anything that listens on a port — surviving hotserve restarts and upgrades, with a continuous watchdog that restarts them on crash or sustained health failure (bounded by a restart budget, with backoff). |
| **[penaltybox](penaltybox/)** | Rate limiting driven by your app's `X-Rate-Limit-Level` hint headers — weighted sliding-window budgets, tiers, and a penalty box for clients that cross them. |
| **cache** | HTTP page caching via [Souin](https://github.com/darkweak/souin) with in-memory [Otter](https://github.com/darkweak/storages) storage. |
| everything Caddy has | Automatic HTTPS, HTTP/2 + HTTP/3, the Caddyfile, the admin API — hotserve *is* Caddy underneath, with the modules above compiled in. |

## Install

Grab the `.deb` for your architecture from
[releases](https://github.com/smallhoursorg/hotserve/releases):

```sh
sudo apt install ./hotserve_*_amd64.deb
sudo systemctl enable --now hotserve
```

That gives you `/usr/bin/hotserve`, a systemd service running as the
`hotserve` user, and a starter config at `/etc/hotserve/Caddyfile`.
**Supported: Debian 12 and 13, Ubuntu 24.04 and 26.04.** Per-app
sandboxing runs at its *full* tier (PID + user namespace) on
systemd ≥ 256 (Debian 13, Ubuntu 26.04) and at the *filesystem* tier
(user namespace, no PID namespace) on Debian 12 and Ubuntu 24.04 —
see [liveswap/README.md](liveswap/README.md#sandbox). All four stay
supported; the older two get every part of the sandbox except the PID
namespace, which is what the *full* tier adds.
The package depends on `libpam-systemd` and `dbus` (present on any
stock Debian/Ubuntu server): liveswap runs your apps as systemd units
under the `hotserve` user's own service manager, which needs
`pam_systemd` to start and `loginctl` to be kept alive without a
login — the package enables that lingering for you.
Prefer the packages — they set up everything above for you. The
`hotserve_<version>_linux_<arch>.tar.gz` archives on the same page
contain the **raw binary** (plus LICENSE and a README) for systems
where you manage the service yourself — your own systemd unit,
NixOS-style distros, containers. Going that route, you own what the
package would have done: a dedicated `hotserve` user, a `Type=notify`
unit, `loginctl enable-linger hotserve` (with `libpam-systemd`
installed) so the user's manager exists for the apps, and the config
at `/etc/hotserve/Caddyfile`. (Hosted APT/APK repositories with
automatic updates are on the roadmap.)

## Quickstart: deploy an app with zero downtime

`/etc/hotserve/Caddyfile`:

```caddyfile
{
	liveswap {
		artifact_allowlist github.com/your-org/   # required: pin artifact origins

		# Who may deploy. A deploy carries an OIDC token from CI; the box
		# verifies it against the provider's public keys — no shared
		# secret ever lives on the server. (required, globally or per app)
		deploy_trust github {
			audience hotserve
			claim repository your-org/myapp
			claim ref        refs/heads/main
		}

		app myapp {
			command node server.js          # runs in the release dir, PORT injected
			pre_start node migrate.js       # failure aborts the deploy
			env_file /etc/hotserve/myapp.env
		}
	}

	cache {
		ttl 60s
		otter
	}
}

myapp.example.com {
	hint_penaltybox                     # rate limiting from app hint headers
	cache                               # page cache
	reverse_proxy {
		dynamic liveswap myapp          # traffic follows the live version
	}
}

deploy.example.com {
	liveswap_webhook
}
```

Then from CI. On GitHub Actions, request an OIDC token per run — no
stored secret, on the box or in CI:

```yaml
permissions:
  id-token: write               # let the job mint an OIDC token
steps:
  - id: tok
    run: echo "jwt=$(curl -sH "Authorization: Bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
      "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=hotserve" | jq -r .value)" >> "$GITHUB_OUTPUT"
  - run: |
      curl --fail -X POST -H "Authorization: Bearer ${{ steps.tok.outputs.jwt }}" \
        -d '{"url":"https://…/myapp.tar.gz","version":"v1.4.2"}' \
        https://deploy.example.com/myapp
```

For non-CI deploys (a laptop, a cron box), use a local key instead:
`hotserve deploy-keygen`, point a `deploy_trust local { public_key … }`
block at the `.pub`, and mint tokens with `hotserve deploy-token`.

hotserve downloads the artifact, runs your migration, starts the new
version on a private port, health-checks it until it has been solidly
up, atomically moves traffic over, and gracefully stops the old one.
If anything fails, the old version never stops serving and CI goes
red. Rollback is one call — `POST /<app>?rollback=<version>` relaunches
an on-disk release. Full details, CI snippets, and every option:
[liveswap/README.md](liveswap/README.md).

## What hotserve is (and isn't)

- **A server product, distributed like Caddy.** Same CLI, same
  Caddyfile, same admin API — `hotserve run`, `hotserve reload`,
  `hotserve validate` all behave exactly as Caddy's do.
- **Made for one cheap server.** Apps run as systemd units under
  hotserve's own user manager, on localhost ports; no container
  runtime anywhere. That's why there's deliberately no Docker image.
- **Not a cluster.** Single-node by design. If you outgrow one server,
  you've outgrown hotserve — a good problem.
- **One box, one trust domain.** Apps run without privileges
  (`NoNewPrivileges`, an unprivileged user) and the admin API lives on
  a unix socket rather than TCP — otherwise "localhost-only" would
  include every app you run, and one SSRF bug in an app could
  reconfigure the server.
  Apps run as one shared user, but each in its own systemd sandbox
  with a deny-by-default filesystem view: a user namespace, and a
  filesystem that holds nothing but that app's own release and data,
  the parts of the OS it needs to run, and whatever it was explicitly
  given. hotserve's keys, sockets and env files, the other apps, and
  the rest of the host are not made unreadable — they are *absent*.
  On systemd ≥ 256 the app also gets its own PID namespace, so
  siblings are invisible rather than merely unreadable. What is *not*
  claimed: below systemd 256 the host process list is still visible in
  `/proc`, even though other processes' contents are not. What stays
  shared by design is the network namespace: sibling `127.0.0.1` ports
  are reachable, and on older systemd same-UID processes can still be
  signalled. Details and the rollout rules:
  [liveswap/README.md](liveswap/README.md#sandbox); the reasoning:
  [DESIGN-threat-model.md](DESIGN-threat-model.md).
- **Deploys are authenticated without a shared secret.** A deploy
  carries a short-lived token — an OIDC token from CI, verified against
  the provider's public keys, or one signed by a local key whose public
  half the box holds. Nothing an attacker can steal off the box lets
  them deploy; see [DESIGN-threat-model.md](DESIGN-threat-model.md).

## Roadmap

- **Per-app sandboxing — shipped in two tiers; resource caps next.**
  Every app runs as a transient unit under the hotserve user's own
  systemd manager (chosen over the system manager: a polkit grant to
  manage units is root-equivalent), and each unit carries systemd's
  own sandboxing: a user namespace (`PrivateUsers=`), a
  deny-by-default filesystem view — the whole host filesystem replaced
  by an empty read-only tmpfs (`TemporaryFileSystem=/:ro`), with only
  the OS the app needs to run — named entry by entry, never whole
  trees — its own release and `shared/`, and its
  declared `extra_path`s bound back, so hotserve's directories and
  sockets and every other app are *absent* rather than merely
  unreadable — `PrivateTmp=`, `PrivateDevices=`, a read-only cgroupfs,
  no capabilities, and systemd's curated
  `SystemCallFilter=@system-service` — no containers, no bubblewrap.
  On systemd ≥ 256 (Debian 13, Ubuntu 26.04) the unit also
  gets a PID namespace (`PrivatePIDs=`): the *full* tier, where the
  supervisor, the user manager and sibling apps are invisible and
  unsignalable. Debian 12 and Ubuntu 24.04 get the *filesystem* tier
  — everything above except the PID namespace, so a compromised app
  can still `kill` same-UID processes (which restart) but cannot read
  their files, sockets or `/proc` — with a warning at every launch.
  Why the user namespace matters: under a shared UID the kernel would
  otherwise let any app walk the host through
  `/proc/<user-manager>/root`; see
  [DESIGN-threat-model.md](DESIGN-threat-model.md) "The shared-UID
  rule". hotserve itself additionally runs non-dumpable from its
  first milliseconds. Policy is `sandbox auto` (default) / `require` /
  `off`, engaging on each app's **next deploy** (never on an upgrade
  relaunch) — [liveswap/README.md](liveswap/README.md#sandbox).
  **Next:** resource caps (`MemoryMax=`, `TasksMax=`, `CPUQuota=`) with
  a config surface — real now that cgroupfs is read-only inside the
  unit. Per-app UIDs would need a small privileged helper and stay a
  later milestone.
- Hosted APT/APK repositories with package signing and auto-updates
- A metrics/alerts module to sit alongside liveswap and penaltybox —
  first customer: alerting when the watchdog is stuck in a restart
  loop (it retries forever by design, so the loop itself is the signal
  that a release is broken)

## Development

No local Go toolchain needed — everything runs in Docker:

```sh
make test              # unit tests, all modules (race + coverage)
make test-integration  # real deploys through caddytest, under a real systemd user manager
make e2e               # full stack: both module suites against the shipped binary under systemd, then restart survival + crash recovery
make lint vet tidy
make vulncheck         # govulncheck, all modules (tool dep in go.mod — Dependabot-bumped)
make secretscan        # gitleaks full-history secret scan (same image as the CI gate)
make fuzz              # fuzz the untrusted-input surfaces (FUZZTIME=2m per target)
make soak              # ~20min leak hunt: deploy/reload churn, goroutine/fd assertions
make build             # cross-compile linux amd64/arm64
make package           # .deb via nfpm
make install-test      # install the .deb under real systemd (DISTRO=debian:12 etc.)
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

You're being asked to run this as root on a production server, so
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

The liveswap and penaltybox columns are what a standalone
`xcaddy build --with ...` of that module pulls in; the hotserve binary
contains everything.

Build and CI tooling never ships to users and is pinned by image tag
in `docker-compose.yml`: `golang` (toolchain), `golangci-lint`,
`nfpm` (deb packaging), `curl` (e2e runner). GitHub Actions are
pinned to commit SHAs.

What keeps this honest: `govulncheck` gates every PR and runs weekly
against the fresh vulnerability database (reachable-code analysis, all
modules), every release is blocked until the full test matrix passes —
including installing the actual `.deb` under systemd on Debian and
Ubuntu — and any dependency bump has to survive all of the above
before it merges.

## License

Apache-2.0. hotserve is powered by [Caddy](https://caddyserver.com),
[Souin](https://github.com/darkweak/souin), and
[Otter](https://github.com/darkweak/storages).
