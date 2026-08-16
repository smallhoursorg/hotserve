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
| **[liveswap](liveswap/)** | Zero-downtime app deploys: webhook from CI, artifact download, migrations, health-gated start, atomic traffic cutover, graceful stop, versioned releases with rollback. Your apps run as supervised child processes — Node.js, Go, anything that listens on a port. |
| **[penaltybox](penaltybox/)** | Rate limiting driven by your app's `X-Rate-Limit-Level` hint headers — weighted sliding-window budgets, tiers, and a penalty box for clients that cross them. |
| **cache** | HTTP page caching via [Souin](https://github.com/darkweak/souin) with in-memory [Otter](https://github.com/darkweak/storages) storage. |
| everything Caddy has | Automatic HTTPS, HTTP/2 + HTTP/3, the Caddyfile, the admin API — hotserve *is* Caddy underneath, with the modules above compiled in. |

## Install

Grab the `.deb` or `.apk` for your architecture from
[releases](https://github.com/smallhoursorg/hotserve/releases):

```sh
sudo apt install ./hotserve_*_amd64.deb
sudo systemctl enable --now hotserve
```

That gives you `/usr/bin/hotserve`, a systemd service running as the
`hotserve` user, and a starter config at `/etc/hotserve/Caddyfile`.
Raw binaries are on the releases page too. (Hosted APT/APK
repositories with automatic updates are on the roadmap.)

## Quickstart: deploy an app with zero downtime

`/etc/hotserve/Caddyfile`:

```caddyfile
{
	liveswap {
		webhook_secret {env.LIVESWAP_SECRET}

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

Then from CI (GitHub Actions, GitLab CI, anything that can curl):

```sh
curl --fail -X POST -H "Authorization: Bearer $SECRET" \
  -d '{"url":"https://…/myapp.tar.gz","version":"v1.4.2"}' \
  https://deploy.example.com/myapp
```

hotserve downloads the artifact, runs your migration, starts the new
version on a private port, health-checks it until it has been solidly
up, atomically moves traffic over, and gracefully stops the old one.
If anything fails, the old version never stops serving and CI goes
red. Rollback is re-POSTing the previous version. Full details, CI
snippets, and every option: [liveswap/README.md](liveswap/README.md).

## What hotserve is (and isn't)

- **A server product, distributed like Caddy.** Same CLI, same
  Caddyfile, same admin API — `hotserve run`, `hotserve reload`,
  `hotserve validate` all behave exactly as Caddy's do.
- **Made for one cheap server.** Apps run as supervised child
  processes of hotserve on localhost ports; no container runtime
  anywhere. That's why there's deliberately no Docker image.
- **Not a cluster.** Single-node by design. If you outgrow one server,
  you've outgrown hotserve — a good problem.
- **One box, one trust domain.** The systemd sandbox protects the
  *system* from your apps, and the admin API lives on a unix socket
  rather than TCP — otherwise "localhost-only" would include every app
  you run, and one SSRF bug in an app could reconfigure the server.
  But apps currently run as one shared user, so hotserve does not
  protect your apps *from each other*: run only workloads you trust
  together, or reach for containers. Per-app sandboxing (bubblewrap,
  Flatpak-style) is designed and next up —
  [liveswap/DESIGN-sandbox.md](liveswap/DESIGN-sandbox.md).

## Roadmap

- **[High priority] Per-app sandboxing / isolation by default**
  (bubblewrap: mount + PID + user namespaces, no containers) —
  designed, see
  [liveswap/DESIGN-sandbox.md](liveswap/DESIGN-sandbox.md). The
  concrete gap driving the priority: because every app currently runs
  as the shared `hotserve` UID, a compromised app can read the
  supervisor's own environment via `/proc/<hotserve-pid>/environ`
  (which holds `LIVESWAP_SECRET` and ACME DNS tokens), connect to the
  admin unix socket, and read the TLS private keys and sibling apps'
  files. Scrubbing the child environment (done) stops direct
  inheritance but not the `/proc` and filesystem routes — only a real
  isolation boundary (bubblewrap's PID + mount namespaces hide
  `/proc/<supervisor>` and the socket/key paths entirely; a per-app
  UID via the future systemd runner is the alternative) closes it.
  This is the next security milestone, ahead of the items below.
- Hosted APT/APK repositories with package signing and auto-updates
- Continuous watchdog: restart a running app on crash or sustained
  health failure (today apps are relaunched at boot and replaced on
  deploy)
- A metrics/alerts module to sit alongside liveswap and penaltybox

## Development

No local Go toolchain needed — everything runs in Docker:

```sh
make test              # unit tests, all modules (race + coverage)
make test-integration  # real deploys through caddytest
make e2e               # full stack: both module suites against the shipped binary, then crash recovery
make lint vet tidy
make vulncheck         # govulncheck, all modules
make fuzz              # fuzz the untrusted-input surfaces (FUZZTIME=2m per target)
make soak              # ~20min leak hunt: deploy/reload churn, goroutine/fd assertions
make build             # cross-compile linux amd64/arm64
make package           # .deb/.apk via nfpm
make install-test      # install the .deb under real systemd (DISTRO=debian:12 etc.)
```

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
  alerts by GHSA id with a reviewed reason and evidence comment, so
  triage decisions are versioned instead of buried in the UI. Unlisted
  alerts stay open and notify as normal. This needs the
  `DEPENDABOT_ALERTS_TOKEN` Actions secret — a fine-grained PAT with
  **Dependabot alerts: read-write** on this repository only — because
  the Actions `GITHUB_TOKEN` cannot access the alerts API.

## Dependencies

You're being asked to run this as root on a production server, so
here's exactly what's in the binary and what it drags in. Module counts
are measured from `go mod graph` (deduplicated by module path); the
"pulls in" column shows what each dependency adds *beyond* what's
already in the tree above it, because the trees overlap almost
entirely.

| Dependency | Pulls in (~modules) | hotserve | liveswap | penaltybox | Notes |
|---|---|:-:|:-:|:-:|---|
| [Caddy](https://github.com/caddyserver/caddy) | ~565 | ✓ | ✓ | ✓ | The foundation — hotserve *is* a Caddy distribution. Commercially sponsored, strong security track record. Nearly the entire dependency tree is Caddy's. |
| [Souin](https://github.com/darkweak/souin) (`cache`) | +24 | ✓ | — | — | HTTP cache (RFC 7234). Solo-maintained; the e2e suite exercises it directly so drift is caught in CI. The riskiest dependency here, and still better than an in-house HTTP cache. |
| [darkweak/storages/otter](https://github.com/darkweak/storages) | +6 | ✓ | — | — | In-memory storage backend for Souin, wrapping [maypok86/otter](https://github.com/maypok86/otter). |
| [zap](https://github.com/uber-go/zap) | 0 (already in Caddy's tree) | ✓ | ✓ | ✓ | Caddy's module logging API is zap; not optional for a Caddy module. |
| [go-humanize](https://github.com/dustin/go-humanize) | 0 (already in Caddy's tree) | ✓ | ✓ | — | A few formatting helpers in liveswap. |

The liveswap and penaltybox columns are what a standalone
`xcaddy build --with ...` of that module pulls in; the hotserve binary
contains everything.

Build and CI tooling never ships to users and is pinned by image tag
in `docker-compose.yml`: `golang` (toolchain), `golangci-lint`,
`nfpm` (deb/apk packaging), `curl` (e2e runner). GitHub Actions are
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
