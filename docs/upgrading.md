# Upgrading hotserve

A release is a new `.deb`; installing it over the old one is the
upgrade:

```sh
# from the release page: the .deb for your architecture, and checksums.txt
cd /var/local/hotserve              # next to the release you are upgrading from
v=0.2.0                             # the version in the .deb's file name
arch=$(dpkg --print-architecture)   # amd64 or arm64
sha256sum -c --ignore-missing checksums.txt
sudo apt install ./hotserve_${v}_${arch}.deb
```

The package keeps your `/etc/hotserve/Caddyfile` and restarts hotserve
into the new binary. Your apps are not restarted: they run under
hotserve's user manager rather than inside hotserve, so the new process
finds each one in `state.json` and picks it up as it is — same process,
no relaunch, no redeploy.

**The restart itself is not zero-downtime.** New visitors get errors
for about a tenth of a second, or longer while a slow request finishes:
the old process gets up to 5 seconds to stop before the new one starts.
So upgrade at a quiet moment. A
config change does not need a restart: `sudo systemctl reload hotserve`
swaps the config inside the running process, drops no requests, and
never restarts a running app (an edited app definition applies the
next time the app starts: its next deploy or rollback, or a relaunch
after a crash, a sustained health failure, or a reboot).

<details>
<summary><b>What visitors see during the restart</b> — and how to pick a quiet moment</summary>

Deploys are zero-downtime; upgrading hotserve is not, because the
process listening on 80 and 443 is the one being replaced. On stop it
closes its listeners at once and lets the requests already in flight
finish, and the new process cannot listen until the old one has
exited. New visitors get connection refused for that whole window,
sometimes followed by a few dozen milliseconds of 503s while the new
process picks the apps back up. A crash has a window of its own:
whatever was in flight is cut off, systemd waits a second
(`RestartSec=1s`) and starts hotserve again, and the apps are picked
back up the same way — a little over a second, plus the start itself.
How long a planned restart's window lasts:

- **With only short requests in flight:** about a tenth of a second
  (measured on Debian 13 in the package test container, plain HTTP).
- **With a slow one in flight** — a large download, a long poll — as
  long as that request takes, because the old process waits for it.
- **The old process gets at most 5 seconds to stop** (`TimeoutStopSec=5s`
  in the unit). systemd then kills it, which cuts off whatever was still
  running, and logs `Failed with result 'timeout'`; the restart still
  goes ahead, and the apps are untouched. The new process starts after
  that, so the window is those 5 seconds plus its startup (5.3 seconds
  in all, in the test container) — and a new process that cannot start
  is not retried by systemd (it retries a crash, never a refused
  start): the site stays down until you fix the config or go back,
  which is what the config check below is for.

This is how Caddy itself upgrades: its official Debian package restarts
the service the same way, with the same 5-second stop timeout, and the
shutdown is Caddy's own. The 503s are the one part that is hotserve's —
the moment before liveswap has picked your apps back up.

To check for a quiet moment, add `metrics` to the Caddyfile's global
options, reload, and read how many requests are in flight:

```sh
sudo curl -fsS --unix-socket /run/hotserve/admin.sock http://localhost/metrics \
  | grep '^caddy_http_requests_in_flight'
```

Every line at 0 means nothing is mid-request. No lines means the check
is not working yet, not that the server is idle: `metrics` is not on,
or nothing has been served since the reload (load a page, then look
again). It is only a snapshot, so a request can still start the moment
after, but it catches the slow downloads that would stretch the window.

</details>

Before you upgrade:

- **Check the new version accepts your config.** The `.tar.gz` on the
  release page holds the bare binary, and
  `sudo ./hotserve validate --config /etc/hotserve/Caddyfile` runs its
  config check without touching anything that is running. A config it
  rejects would stop the upgraded hotserve from starting, and the site
  would stay down until you fixed the config or went back (systemd
  retries a crash, not a refusal).
- **The `.deb` you are upgrading from is still in `/var/local/hotserve`.**
  `sudo apt install --allow-downgrades ./hotserve_<old>_$(dpkg --print-architecture).deb`
  puts it back — the same restart, in reverse, and nothing to download
  first. On a VPS, a snapshot taken first is the fuller way back.
- **Upgrade hotserve and the host separately.** hotserve measures what
  the host can sandbox at every start and refuses to start on a host
  that cannot deliver it ([liveswap/README.md](../liveswap/README.md#sandbox)),
  so a kernel update or reboot bundled with an upgrade leaves you
  guessing which change did it.

There is no APT repository yet, so nothing upgrades hotserve behind
your back; the hosted repository on the [roadmap](../README.md#roadmap) is what
will change that.

## Patching the host

Apps run on the box's own `/usr`, so `apt upgrade` reaches an app at
its next launch — its next deploy or rollback, or a relaunch after a
crash, a sustained health failure, or a reboot — never the one
running, which keeps the runtime and libraries it has already loaded.
Upgrading hotserve does not relaunch apps either: it picks them up as
they are.

What a patch reaches depends on where the app's runtime came from:

- **`command node server.js` with Node from apt:** the new Node and the
  new system libraries.
- **The Deno under `/usr/local/bin`**, or **the Node example's
  executable**, which carries its own Node: the new system libraries
  only. A newer Deno is a new file there, and a newer Node a new build;
  either still waits for the next launch.

So after patching, relaunch each app by pushing a commit to its
repository. An empty one is enough:

```sh
git commit --allow-empty -m "Relaunch after patching" && git push
```

The version running cannot be deployed or rolled back to again, and a
new commit is a new version, so it goes through the same health gate
and cutover as any deploy. Or reboot, which relaunches every app at
once but takes the sites down while it does.
