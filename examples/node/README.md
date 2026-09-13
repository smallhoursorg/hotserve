# A Node.js app on hotserve, as one executable

A small app to copy as the start of your own. It ships as a single
executable — a copy of Node with the app inside — so the box needs no
runtime installed. It serves on the unix socket hotserve hands it,
answers a health check, runs a migration before each new version
starts, finishes its requests on shutdown, and deploys from GitHub
Actions with no stored secret. hotserve's e2e suite builds and deploys
this directory, with these files, on every change to hotserve, so
what is here works.

| File | What it is |
|---|---|
| `server.js` | The app: `/` and `/health`, on `$SOCKET` (or 127.0.0.1:8000 locally) |
| `migrate.js` | The `pre_start` hook: runs before each new version starts |
| `package.json` | The `dev`, `bundle` and `deploy` scripts (no dependencies) |
| `scripts/bundle.sh` | Builds `app.tar.gz`: `server` and `migrate` as executables |
| `scripts/deploy.sh` | Tells the box to deploy a release URL (or pushes a local tarball) |
| `.github/workflows/deploy.yml` | Build, publish a release, deploy — on every push to `main` |
| `hotserve.caddy` | What the app needs from the box: the lines for its `app` block |
| `AGENTS.md` | The rules the box enforces, for you and your coding agent |

## Run it locally

```sh
npm run dev            # http://127.0.0.1:8000
```

## Put it on a box

You need a Debian 13 server with hotserve installed (see
[Install](../../README.md#install)) and a DNS name pointing at it.

**1. On an arm64 box, install `libatomic1`.** The tarball carries its
own Node, and Node's arm64 build links one library a stock Debian 13
does not ship; without it the app fails to start with exit status 127
(`error while loading shared libraries: libatomic.so.1`). amd64
needs nothing.

```sh
sudo apt install libatomic1
```

**2. Add the app to the box's Caddyfile** (`/etc/hotserve/Caddyfile`),
with the lines from `hotserve.caddy` inside its `app` block:

```caddyfile
{
	admin unix//run/hotserve/admin.sock

	liveswap {
		artifact_allowlist github.com/your-org/

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

Instead of pasting them, you can save `hotserve.caddy` on the box (as
`/etc/hotserve/example.caddy`, say) and write
`import /etc/hotserve/example.caddy` in the block: that is how the e2e
suite uses it. Check the config, then load it:

```sh
hotserve validate --config /etc/hotserve/Caddyfile
sudo systemctl reload hotserve
```

**3. Point the workflow at the box.** In your copy of this directory,
set `HOTSERVE_URL` in `.github/workflows/deploy.yml` to
`https://deploy.example.com/example`, and `runs-on` to the box's
architecture (the executable is the runner's own Node binary, so an
arm64 box needs an arm64 build). Then push to `main`. The workflow
publishes the tarball as a GitHub release and the box fetches it — so
every deployed version stays on GitHub, and the box's
`artifact_allowlist github.com/your-org/` is what admits it. The
deploy step prints the app's status when the new version is live, or
why it was refused; a refused deploy leaves the old version serving.
(A private repo's assets are only readable through GitHub's API: see
"Deploying from CI" in hotserve's
[liveswap/README.md](../../liveswap/README.md#deploying-from-ci).)

## What the executable is, and isn't

`npm run bundle` runs Node's own single-executable build
(`node --build-sea`, Node 25.5+): each of `server.js` and `migrate.js`
is embedded in a copy of the Node binary. The tarball is about 45 MB
compressed and 300 MB unpacked, within hotserve's default caps, and it
runs on any Debian 13 box of the same architecture with nothing
installed beyond `libatomic1` on arm64. Inside the executable there is no `node_modules`: keep the
app dependency-free, or bundle it into one file (esbuild, say) before
the build. To run a Node that is installed on the box instead, see
the [Deno example](../deno) for the shape — `command node server.js`
with the runtime under `/usr` — which is smaller per deploy and lets
the box's Caddyfile hold the runtime's flags.

## Deploying without CI

Given a local tarball instead of a URL, `scripts/deploy.sh` pushes it
in the request body, so a laptop build needs no release — as long as
the laptop is the box's architecture and OS (a Linux arm64 build, for
an arm64 box). With a `deploy_trust local` block on the box (see
[Deploy authentication](../../liveswap/README.md#deploy-authentication-deploy_trust)):

```sh
npm run bundle
HOTSERVE_URL=https://deploy.example.com/example \
HOTSERVE_TOKEN=$(hotserve deploy-token --key deploy.key --audience hotserve) \
npm run deploy
```
