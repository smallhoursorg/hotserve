# A Node.js app on hotserve, as one executable

A small app to copy as the start of your own. It ships as a single
executable — a copy of Node with the app inside — so the box needs no
runtime installed. It serves on the unix socket hotserve hands it,
answers a health check, runs a migration before each new version
starts, finishes its requests on shutdown, and deploys from GitHub
Actions with no stored secret.
[hotserve](https://github.com/smallhoursorg/hotserve)'s e2e suite
builds and deploys these files, from its `examples/node`, on every
change to hotserve, so what is here works. Each stable release
publishes them as the template repository
[hotserve-example-node](https://github.com/smallhoursorg/hotserve-example-node): **Use this template** there makes a
repository of your own, which deploys with no file edited.

| File | What it is |
|---|---|
| `server.js` | The app: `/` and `/health`, on `$SOCKET` (or 127.0.0.1:8000 locally) |
| `migrate.js` | The `pre_start` hook: runs before each new version starts |
| `package.json` | The `dev`, `bundle` and `deploy` scripts (no dependencies) |
| `scripts/bundle.sh` | Builds `app.tar.gz`: `server` and `migrate` as executables |
| `scripts/deploy.sh` | Tells the box to deploy a release URL (or pushes a local tarball), or to roll back |
| `.github/workflows/deploy.yml` | Build, publish a release, deploy — on every push to `main`; `Run workflow` rolls back. The box's address is the repository variable `HOTSERVE_URL` |
| `hotserve.caddy` | What the app needs from the box: the lines for its `app` block |
| `AGENTS.md` | The rules the box enforces, for you and your coding agent |

## Run it locally

```sh
npm run dev            # http://127.0.0.1:8000
```

## Put it on a box

You need a Debian 13 server with hotserve installed and a DNS name
pointing at it. [Your first deploy](https://github.com/smallhoursorg/hotserve/blob/main/docs/first-deploy.md) is the
whole path from a fresh server, with this app as its example; the
three steps below are its second half.

**1. On an arm64 box, install `libatomic1`** — as root, while you
still have it: the administrator the box repo creates later cannot
install packages. The tarball carries its own Node, and Node's arm64
build links one library a stock Debian 13 does not ship; without it
the app fails to start with exit status 127 (`error while loading
shared libraries: libatomic.so.1`). amd64 needs nothing.

```sh
apt install libatomic1
```

**2. Add the app to the box's Caddyfile**, with the lines from
`hotserve.caddy` inside its `app` block. As root that is
`/etc/hotserve/Caddyfile`: edit, `hotserve validate --config` it,
`systemctl reload hotserve`. Once the box has a
[box repo](https://github.com/smallhoursorg/hotserve/tree/main/examples/box#change-the-config), it is that repo's `Caddyfile`
and `make push`. The box example ships the Deno app's block; this
app's `command`, `pre_start` and `env` lines replace those. Either
way, the file is:

```caddyfile
{
	admin unix//run/hotserve/admin.sock

	liveswap {
		artifact_allowlist api.github.com/repos/your-org/

		app example {
			# the lines from hotserve.caddy go here

			deploy_trust github {
				audience hotserve
				claim repository your-org/example
				claim ref        refs/heads/main
			}
		}
	}
}

example.com {
	reverse_proxy {
		dynamic liveswap example
	}
}

deploy.example.com {
	liveswap_webhook
}
```

The copy on the box is the one that counts: hotserve never reads
`hotserve.caddy` from a deploy. (hotserve's e2e suite instead saves
the file on its box as `/etc/hotserve/<app>.caddy` and writes `import`
of that path in the block.)

**3. Point the workflow at the box.** No file needs editing: in your
copy's GitHub repository, add the repository variable `HOTSERVE_URL`,
set to `https://deploy.example.com/example` (Settings → Secrets and
variables → Actions → Variables, or
`gh variable set HOTSERVE_URL -R your-org/example --body https://deploy.example.com/example`).
The workflow builds on an arm64 runner, since the executable is the
runner's own Node binary; for an amd64 box, also add
`HOTSERVE_RUNS_ON` set to `ubuntu-24.04`. Then push to `main`. A run
without `HOTSERVE_URL` stops at its first step, saying so, and
publishes nothing. The workflow publishes the tarball as a GitHub
release and the box fetches it — so every deployed version stays on
GitHub. The box fetches the asset by its API URL with the job's own
token, so a private repo deploys the same way as a public one, and the
box's `artifact_allowlist api.github.com/repos/your-org/` is what
admits it. The deploy step prints the app's status when the new
version is live, or why it was refused; a refused deploy leaves the
old version serving. A 401 means the box's `deploy_trust` for this
app does not accept the run — most often `claim repository` or `claim
ref` names another repository or branch — or that `HOTSERVE_URL`
names an app the box does not know, which answers the same 401. The
step prints the values this run minted its token with; the box's
journal (`journalctl -u hotserve`) names the app asked for and the
check that refused it, within the box's budget for failed
authentications: ten a minute from one address (past that, 429) and a
hundred a minute in all (past that, 401), neither written until the
minute passes — except that the box failing to consult the token's
issuer is named once a minute whatever the budget, and a re-run once
the issuer is back goes through.

What a first deploy that worked looks like, from the laptop:

- The deploy step's output ends with the app's status JSON, holding
  `"current_version":"<the commit's first 12 characters>"`.
- `curl https://example.com/` answers `hello from <that version>
  (schema v1)`; the 503 from before the deploy is gone.
- `ssh alice@box.example.com journalctl -t hotserve-example` shows the
  app's own output — the migration's line, then whatever `server.js`
  prints. The `adm` group the box README grants is what reads it.

A re-run of the same workflow run is refused with a 422: versions are
immutable on the box, and that commit's version is already there. Push
a new commit to deploy again.

## What the executable is, and isn't

`npm run bundle` runs Node's own single-executable build
(`node --build-sea`, Node 25.5+): each of `server.js` and `migrate.js`
is embedded in a copy of the Node binary. The tarball is about 45 MB
compressed and 300 MB unpacked, within hotserve's default caps, and it
runs on any Debian 13 box of the same architecture with nothing
installed beyond `libatomic1` on arm64. Inside the executable there is no `node_modules`: keep the
app dependency-free, or bundle it into one file (esbuild, say) before
the build. To run a Node that is installed on the box instead, see
the [Deno example](https://github.com/smallhoursorg/hotserve/tree/main/examples/deno) for the shape — `command node server.js`
with the runtime under `/usr` — which is smaller per deploy and lets
the box's Caddyfile hold the runtime's flags.

## Rolling back

Actions → deploy → **Run workflow**, with a version from the releases
page (the tag: a commit's first 12 characters) and "Use workflow from"
left on `main` — the box's `deploy_trust` pins that ref, so a run from
a tag or another branch is refused with a 401. The box relaunches
that release from its disk — the same start, health gate and cutover
as a deploy, and no build — so it is live in about twenty seconds, or
the run is red with the reason and nothing changed. A version the box
no longer holds is a 422: it keeps the newest five releases, plus the
running one. Roll forward the same way you deploy: push a commit.

## Deploying without CI

Given a local tarball instead of a URL, `scripts/deploy.sh` pushes it
in the request body, so a laptop build needs no release — as long as
the laptop is the box's architecture and OS (a Linux arm64 build, for
an arm64 box). The token comes from `hotserve deploy-token`, and
hotserve's release binaries are Linux only, so this is a path for a
Linux machine that is not the box (the signing key must stay off the
box).
With a `deploy_trust local` block on the box (see
[Deploy authentication](https://github.com/smallhoursorg/hotserve/blob/main/liveswap/README.md#deploy-authentication-deploy_trust)):

```sh
npm run bundle
HOTSERVE_URL=https://deploy.example.com/example \
HOTSERVE_TOKEN=$(hotserve deploy-token --key deploy.key --audience hotserve) \
npm run deploy
```

The version defaults to the commit, and versions are immutable on the
box, so that deploys once per commit; for an uncommitted build set one
(`VERSION=wip-3 npm run deploy`).
