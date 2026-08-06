# hotserve

**The Hot Source server.** One binary that is your reverse proxy, your
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
[releases](https://github.com/hotsauce-team/hotserve/releases):

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
curl --fail -X POST -H "X-Liveswap-Secret: $SECRET" \
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

## Roadmap

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
make e2e               # full stack: both module suites against the shipped binary
make lint vet tidy
make build             # cross-compile linux amd64/arm64
make package           # .deb/.apk via nfpm
```

The repo is a Go multi-module workspace: `liveswap/` and `penaltybox/`
are lean, independently usable Caddy modules
(`xcaddy build --with github.com/hotsauce-team/hotserve/liveswap`),
and the root module builds the product binary from `cmd/hotserve`.

## License

Apache-2.0. hotserve is powered by [Caddy](https://caddyserver.com),
[Souin](https://github.com/darkweak/souin), and
[Otter](https://github.com/darkweak/storages).
