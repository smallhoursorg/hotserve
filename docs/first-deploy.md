# Your first deploy

From a fresh server to an app you deploy by pushing to `main`, in four
steps. At the end you have hotserve serving one app over HTTPS, a
GitHub Actions workflow that deploys every push with no stored secret,
and the shape you copy for every app after it.

Everything here runs as root on the box, on purpose: it is the
shortest path to a working deploy. [After the first deploy](after-first-deploy.md)
is where you create the administrator, close root login, and put the
box's config in git. Do those next; none of them is needed to get
here.

Names used throughout — replace them with yours as you go:

| Name | What it is |
|---|---|
| `example.com` | The app's address |
| `deploy.example.com` | The deploy webhook's address, on the same box |
| `your-org/example` | The GitHub repository the app deploys from |
| `example` | The app's name on the box |

## 1. A box, two names, two ports

You need a real virtual machine, not a container. Providers call the
kind you want **KVM**; at Hetzner, say, the cheapest KVM tier is enough
for a few small apps. Offers described as LXC or "container VPS" will
not work: each app runs in its own sandbox, which those hosts cannot
provide, and hotserve refuses to start rather than run your apps
without it. If you are unsure what you have, step 2 tells you within a
minute. At the provider:

1. Create a server with **Debian 13** as the image and your SSH public
   key. Architecture is your choice; note it, since the release `.deb`
   and the Node example's executable are per-architecture.
2. Ports **80 and 443** must be reachable from the internet. If the
   provider has a firewall, allow both from anywhere. If it has none,
   there is nothing to do.
3. Point two DNS names at the server's address: `example.com` and
   `deploy.example.com`. hotserve gets certificates for both itself,
   once the names resolve and the ports are open.

## 2. Install hotserve

Logged in as root, fetch the `.deb` for the box's architecture from
[releases](https://github.com/smallhoursorg/hotserve/releases), check
it, and install it:

```sh
v=0.2.0                             # the release you are installing
arch=$(dpkg --print-architecture)   # amd64 or arm64
base=https://github.com/smallhoursorg/hotserve/releases/download/v$v
mkdir -p /var/local/hotserve && cd /var/local/hotserve
curl -fsSLO "$base/hotserve_${v}_${arch}.deb" && curl -fsSLO "$base/checksums.txt"
sha256sum -c --ignore-missing checksums.txt
apt install ./hotserve_${v}_${arch}.deb
systemctl enable --now hotserve
```

(The directory is where the `.deb` stays, so the way back from a later
upgrade is on the box already; `/tmp` would not survive a reboot. A
prerelease's `.deb` is named differently from its tag —
`hotserve_0.2.0.rc1_arm64.deb` under `v0.2.0-rc1` — so take that file
name from the release page. `enable --now` prints only `Created
symlink …`; that is success.)

Check it:

```sh
systemctl status hotserve          # active (running)
curl -s localhost                  # "hotserve is running. Edit /etc/hotserve/Caddyfile …"
journalctl -u hotserve | grep 'liveswap started'   # one JSON line: "msg":"liveswap started","apps":0
```

If it is not running, `journalctl -u hotserve -n 50` says why. A host
that cannot deliver the sandbox is refused here, with the missing piece
named; that is the step 1 check, and the fix is a different host, not
a setting.

## 3. Tell hotserve about the app

The package installed a starter config at `/etc/hotserve/Caddyfile`:
the placeholder site you curled in step 2, and the rest as comments.
Replace its contents with this, then change
`example.com`, `deploy.example.com` and `your-org` — the last one
twice: `artifact_allowlist` pins the organization, `deploy_trust` the
repository:

```caddyfile
{
	# Keep: the admin API off TCP (every app can reach localhost), and
	# `systemctl reload` finds the socket here.
	admin unix//run/hotserve/admin.sock

	liveswap {
		# Where a deploy may fetch from. The example's workflow sends its
		# release asset's API URL, and hosts match exactly, so it is
		# api.github.com here, not github.com.
		artifact_allowlist api.github.com/repos/your-org/

		app example {
			# From examples/node/hotserve.caddy: both commands are
			# executables inside the tarball.
			command ./server
			pre_start ./migrate
			env APP_VERSION {version}
			env DATA_DIR {shared_dir}

			# Who may deploy it: the repo's main branch, by the OIDC token
			# its workflow mints. Nothing secret is stored here.
			deploy_trust github {
				audience hotserve
				claim repository your-org/example
				claim ref refs/heads/main
			}
		}
	}
}

example.com {
	reverse_proxy {
		dynamic liveswap example
	}
	# Before the first deploy, and while the app is down, the proxy has
	# no upstream and answers 503. Keep the status; add words.
	handle_errors 502 503 {
		respond "example is not running: nothing has been deployed yet, or it is down and being restarted." {http.error.status_code}
	}
}

deploy.example.com {
	liveswap_webhook
}
```

Do not add an `env_file` line yet: hotserve reads that file each time
the app launches, and a missing one fails the launch. Secrets come
later, in [After the first deploy](after-first-deploy.md#3-secrets-if-the-app-has-any).

Then check the file and load it:

```sh
hotserve validate --config /etc/hotserve/Caddyfile
systemctl reload hotserve
```

A file that fails to validate never loads, and the running config
stays; but it would stop the next restart from starting, so fix it
before moving on. With nothing deployed yet, a correct box looks like
this:

- `https://example.com` answers 503 with the sentence from the
  `handle_errors` block — once the certificate is issued, which takes
  a few seconds after the reload with 80 and 443 open.
- `https://deploy.example.com/example` answers 401 with
  `{"error":"invalid or missing deploy token …"}`, in a browser or with
  `curl -i`. That is the webhook working: a deploy carries a token, and
  nothing without one gets further than this.
- `journalctl -u hotserve | grep 'liveswap started'` gains a line with
  `"apps":1,"app_names":["example"]`.

**On an arm64 box, one more command.** The Node example's executable is
a copy of Node, and Node's arm64 build links one library a stock Debian
13 does not ship; without it the app fails to start with exit status
127. amd64 needs nothing.

```sh
apt install libatomic1
```

<details>
<summary><b>If you would rather deploy the Deno example</b></summary>

The Deno example runs `deno run` with a Deno installed on the box, and
the runtime's permission flags live in this Caddyfile — where a
compromised build cannot widen them. That is what it buys over the
single executable; the cost is one runtime to install. Two changes to
the above:

1. Install Deno under `/usr/local`, as root. The app's sandbox contains
   `/usr` and nothing else of the host's software, so a Deno anywhere
   else would not exist for it. Use the version in the example's
   `.deno-version`, which is what its workflow builds with:

   ```sh
   v=v2.9.6                              # what examples/deno/.deno-version says
   f=deno-$(uname -m)-unknown-linux-gnu.zip
   curl -fsSLO "https://github.com/denoland/deno/releases/download/$v/$f"
   curl -fsSLO "https://github.com/denoland/deno/releases/download/$v/$f.sha256sum"
   sha256sum -c "$f.sha256sum"
   apt install -y unzip && unzip -o "$f" deno -d /usr/local/bin
   ```

2. In the `app example` block, replace the four lines from the Node
   example with the ones in
   [examples/deno/hotserve.caddy](../examples/deno/hotserve.caddy):

   ```caddyfile
   command deno run --cached-only \
   	--allow-net=unix:{socket} \
   	--allow-read={shared_dir},{socket} \
   	--allow-write={socket} \
   	--allow-env=SOCKET,APP_VERSION,DATA_DIR \
   	main.ts
   pre_start deno run --cached-only \
   	--allow-read={shared_dir} \
   	--allow-write={shared_dir} \
   	--allow-env=DATA_DIR \
   	migrate.ts
   env DENO_DIR {release_dir}/.deno
   env DENO_NO_UPDATE_CHECK 1
   env APP_VERSION {version}
   env DATA_DIR {shared_dir}
   ```

Then in step 4, copy [examples/deno](../examples/deno) instead, and
leave its `runs-on` alone: the tarball has no native code, so any
runner builds it.

</details>

## 4. The app

Copy [examples/node](../examples/node) into a new GitHub repository,
`your-org/example`. Private is fine: the box fetches the release asset
by its API URL with the workflow's own token, and the
`artifact_allowlist` above admits it.

In `.github/workflows/deploy.yml`, set two things:

- `HOTSERVE_URL` (it appears twice) to
  `https://deploy.example.com/example`.
- `runs-on` to the box's architecture: `ubuntu-24.04-arm` for an arm64
  box, `ubuntu-24.04` for amd64. The executable is the runner's own
  Node binary, so this has to match.

Push to `main`. The workflow builds the executables, publishes them as
a GitHub release tagged with the commit's first 12 characters, mints an
OIDC token, and asks the box to deploy. The box fetches the release,
runs the migration, starts the new version on its own socket, health
checks it for fifteen seconds, and moves traffic over. The request
returns when that is done, so the deploy step takes about half a
minute.

What a first deploy that worked looks like:

- The deploy step's output ends with the app's status JSON, holding
  `"current_version":"<the commit's first 12 characters>"`.
- `curl https://example.com/` answers `hello from <that version>
  (schema v1)`; the 503 from step 3 is gone.
- On the box, `journalctl -t hotserve-example` shows the app's own
  output — the migration's line, then whatever `server.js` prints.

If the deploy step is red, its output says which stage refused it and
why — a token the box would not accept, a URL outside the allowlist, a
health check that never passed. The app's own output is not in that
message: for a migration that failed or an app that exited on start,
`journalctl -t hotserve-example` on the box is where the reason is.
Nothing was serving before, so nothing changed.

A re-run of the same workflow run is refused with a 422: versions are
immutable on the box, and that commit's version is already there. Push
a new commit to deploy again.

## What you have

An app at `example.com` that deploys on every push to `main`, with no
SSH key or password anywhere in CI. Rollback is a button:
Actions → deploy → **Run workflow**, with a version from the releases
page; the example's README says [how](../examples/node/README.md#rolling-back).
The next app is the same four steps minus the first two: another
`app` block and site in the Caddyfile, another copy of the example.

Now do [After the first deploy](after-first-deploy.md): it turns this
box from something root set up into something a person administers,
and it takes less time than this did.
