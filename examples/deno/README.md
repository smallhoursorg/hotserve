# A Deno app on hotserve

A small app to copy as the start of your own. It serves on the unix
socket hotserve hands it, answers a health check, runs a migration
before each new version starts, finishes its requests on shutdown, and
deploys from GitHub Actions with no stored secret.
[hotserve](https://github.com/smallhoursorg/hotserve)'s e2e suite
builds and deploys these files, from its `examples/deno`, on every
change to hotserve, so what is here works. Each stable release
publishes them as the template repository
[hotserve-example-deno](https://github.com/smallhoursorg/hotserve-example-deno): **Use this template** there makes a
repository of your own, which deploys with no file edited.

| File | What it is |
|---|---|
| `main.ts` | The app: `/` and `/health`, on `$SOCKET` (or 127.0.0.1:8000 locally) |
| `migrate.ts` | The `pre_start` hook: runs before each new version starts |
| `deno.json` | Dependencies and the `dev`, `bundle` and `deploy` tasks |
| `scripts/bundle.sh` | Builds `app.tar.gz`, with the module cache inside it |
| `scripts/deploy.sh` | Tells the box to deploy a release URL (or pushes a local tarball), or to roll back |
| `.github/workflows/deploy.yml` | Build, publish a release, deploy — on every push to `main`; `Run workflow` rolls back. The box's address is the repository variable `HOTSERVE_URL` |
| `hotserve.caddy` | What the app needs from the box: the lines for its `app` block |
| `AGENTS.md` | The rules the box enforces, for you and your coding agent |

## Run it locally

```sh
deno task dev          # http://127.0.0.1:8000
```

## Put it on a box

You need a Debian 13 server with hotserve installed and a DNS name
pointing at it. [Your first deploy](https://github.com/smallhoursorg/hotserve/blob/main/docs/first-deploy.md) is the
whole path from a fresh server, with this app as its one fork; the
three steps below are its second half.

**1. Install Deno on the box, under `/usr/local`** — as root, while
you still have it: the administrator the box repo creates later cannot
install packages. The app's sandbox contains `/usr` and nothing else
of the host's software, so a Deno in a home directory would not exist
for it. Use the version in `.deno-version`, which is what the workflow
builds with:

```sh
v=v$(cat .deno-version)               # from a checkout; on the box, type what the file says
f=deno-$(uname -m)-unknown-linux-gnu.zip
curl -fsSLO "https://github.com/denoland/deno/releases/download/$v/$f"
curl -fsSLO "https://github.com/denoland/deno/releases/download/$v/$f.sha256sum"
sha256sum -c "$f.sha256sum"
apt install -y unzip && unzip -o "$f" deno -d /usr/local/bin
```

**2. Add the app to the box's Caddyfile**, with the lines from
`hotserve.caddy` inside its `app` block. As root that is
`/etc/hotserve/Caddyfile`: edit, `hotserve validate --config` it,
`systemctl reload hotserve`. Once the box has a
[box repo](https://github.com/smallhoursorg/hotserve/tree/main/examples/box#change-the-config), it is that repo's `Caddyfile`
and `make push`. The box example ships this app's block already.
Either way, the file is:

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
Then push to `main`. A run without `HOTSERVE_URL` stops at its first
step, saying so, and publishes nothing. The workflow publishes the
tarball as a GitHub release and the box fetches it — so every deployed
version stays on GitHub. The box fetches the asset by its API URL with
the job's own token, so a private repo deploys the same way as a
public one, and the box's
`artifact_allowlist api.github.com/repos/your-org/` is what admits it.
The deploy step prints the app's status when the new version is live,
or why it was refused; a refused deploy leaves the old version
serving.

What a first deploy that worked looks like, from the laptop:

- The deploy step's output ends with the app's status JSON, holding
  `"current_version":"<the commit's first 12 characters>"`.
- `curl https://example.com/` answers `hello from <that version>
  (schema v1)`; the 503 from before the deploy is gone.
- `ssh alice@box.example.com journalctl -t hotserve-example` shows the
  app's own output — the migration's line, then whatever `main.ts`
  prints. The `adm` group the box README grants is what reads it.

A re-run of the same workflow run is refused with a 422: versions are
immutable on the box, and that commit's version is already there. Push
a new commit to deploy again.

## When the code needs a new permission

A new env var, file path or network host is a new Deno flag, and the
flags live on the box, so that a compromised build cannot grant itself
more. Change them on the box *before* you deploy code that needs them:
the running version does not mind an extra permission, and the new
one will not start without it. Remove a flag after the code that
needed it is gone. Keep `hotserve.caddy` here in step, so the repo
says what the code expects.

A reload never restarts the running app; the new flags apply at its
next launch (a deploy, or a restart after a crash). A rollback runs
the older code with the flags on the box today.

## Rolling back

Actions → deploy → **Run workflow**, with a version from the releases
page (the tag: a commit's first 12 characters) and "Use workflow from"
left on `main` — the box's `deploy_trust` pins that ref, so a run from
a tag or another branch is refused with a 401. The box relaunches
that release from its disk — the same start, health gate and cutover
as a deploy, and no build — so it is live in about twenty seconds, or
the run is red with the reason and nothing changed. A version the box
no longer holds is a 422: it keeps the newest five releases, plus the
running one. It runs the older code with the Deno flags on the box
today (see above). Roll forward the same way you deploy: push a commit.

## Deploying without CI

Given a local tarball instead of a URL, `scripts/deploy.sh` pushes it
in the request body, so a laptop build needs no release. The token
comes from `hotserve deploy-token`, and hotserve's release binaries
are Linux only, so this is a path for a Linux machine that is not the
box (the signing key must stay off the box). With a `deploy_trust local` block
on the box (see
[Deploy authentication](https://github.com/smallhoursorg/hotserve/blob/main/liveswap/README.md#deploy-authentication-deploy_trust)):

```sh
deno task bundle
HOTSERVE_URL=https://deploy.example.com/example \
HOTSERVE_TOKEN=$(hotserve deploy-token --key deploy.key --audience hotserve) \
deno task deploy
```

The version defaults to the commit, and versions are immutable on the
box, so that deploys once per commit; for an uncommitted build set one
(`VERSION=wip-3 deno task deploy`).
