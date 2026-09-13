# A box repo for hotserve

The server's config, kept in git: one `Caddyfile`, reviewed like code
and pushed to the box by a script that never leaves a bad file there.
Copy this directory into a **private** repo of its own, separate from
your apps' repos.

It is a separate repo because the Caddyfile is where the box's policy
lives: which repo may deploy each app, and each Deno app's permission
flags. An app repo can be compromised through a dependency or a
leaked token; it should not also be able to grant itself
`--allow-net`. So an app that needs a new permission asks for it here,
and the change is a diff someone reads before `make push`.

| File | What it is |
|---|---|
| `Caddyfile` | The box's whole config; becomes `/etc/hotserve/Caddyfile` |
| `bin/push` | Validates on the box, shows the diff, swaps the file in, reloads |
| `sudoers` | The eight commands `bin/push` runs as root, and nothing else |
| `Makefile` | `make check` and `make push` |
| `.github/workflows/check.yml` | `make check` on every push and pull request |

## Provision the box

Any VPS with Debian 13 will do; the cheapest tier is enough for a
few small apps. At the provider (Hetzner, say):

1. Create a server with **Debian 13** as the image, and your SSH
   public key. Architecture is your choice; note it, since the release
   `.deb` and a Node single executable are per-architecture.
2. If the provider has a firewall, allow 80 and 443 from anywhere and
   22 only from your own address (home, VPN).
3. Point a DNS name at the address — and one for the deploy webhook
   (`deploy.example.com` below), which hotserve serves on the same
   box.

Then, logged in as root once — install hotserve as
[Install](../../README.md#install) says, and create the user you will
administer it as. That user is not root: the `sudoers` file in this
directory grants exactly the commands `bin/push` needs, and the `adm`
group reads the logs.

```sh
adduser alice
install -d -m 0700 -o alice -g alice /home/alice/.ssh && cp ~/.ssh/authorized_keys /home/alice/.ssh/
curl -fsSL https://raw.githubusercontent.com/smallhoursorg/hotserve/main/examples/box/sudoers \
  -o /etc/sudoers.d/hotserve-admin && chmod 0440 /etc/sudoers.d/hotserve-admin && visudo -c
groupadd hotserve-admin && usermod -aG adm,hotserve-admin alice
printf 'PermitRootLogin no\nPasswordAuthentication no\n' > /etc/ssh/sshd_config.d/10-hardening.conf
sshd -t && systemctl reload ssh      # -t checks the config first: a bad one would lock you out
```

Check `ssh alice@box.example.com sudo -n systemctl reload hotserve`
works before you close the root session: that is the whole of what
`make push` will need.

## Set it up

1. Copy this directory into a new private repo, and replace
   `example.com`, `deploy.example.com` and `your-org/example` in the
   `Caddyfile` with yours. The `app example` block is
   [examples/deno](../deno)'s; the app's own README walks through its
   side.
2. Make sure your user on the box is set up as in
   [Provision the box](#provision-the-box): `sudoers` installed,
   membership of `adm` and `hotserve-admin`. `bin/push` needs nothing
   more, and `journalctl -u hotserve` needs no sudo at all. What that
   grants is the `hotserve` user's reach, not root's — see
   [Secrets](#secrets) — plus, through `adm`, the whole system
   journal, not only hotserve's lines. Give it to the people who may
   change what the box serves.
3. Set `HOTSERVE_VERSION` in `.github/workflows/check.yml` to the
   release the box runs: the tag without its `v`. `dpkg -s hotserve`
   shows it, except that dpkg writes a prerelease as `0.2.0~rc1`
   where the tag is `0.2.0-rc1`.

## Change the config

Edit the `Caddyfile`, open a pull request (CI validates it), merge,
then from a checkout of `main`:

```sh
make push BOX=box.example.com
```

`bin/push` copies the file to the box next to the live one, validates
it there (which needs no privilege), and shows the diff against what the
box is running — including any edit someone made on the box directly,
which this push would undo. You answer `y` or nothing is applied.
Then it renames the new file into place and reloads hotserve; if the
reload fails, it puts the previous file back, so the file on disk is
always the config that is running. (A bad file left in place would
not break anything at once, since the running config stays, but it
would stop the next restart or upgrade from starting.)

A reload never restarts a running app. A changed `app` block applies
at the app's next launch: its next deploy, or a relaunch after a crash
or a reboot.

**Widening an app's permissions:** push the new flag *before* the app
deploys code that needs it; the running version does not mind an extra
permission, and the new one will not start without it. Narrow after
the code that needed it is gone. A rollback runs the older code with
the flags pushed today.

## Secrets

They never go in this repo. Put each app's in a file on the box that
only root and the `hotserve` user can read, and name it with
`env_file` in the app's block. Creating the file is root's one-time
job; `sudoers` lets an administrator edit it afterwards:

```sh
sudo install -m 0640 -o root -g hotserve /dev/null /etc/hotserve/example.env   # as root, once
sudoedit /etc/hotserve/example.env      # DATABASE_URL=postgres://…
```

Whoever can push the Caddyfile can read these anyway: one line makes
hotserve serve any file the `hotserve` user can read, its TLS keys
included, and another adds a `deploy_trust` of their own. The sudoers
file bounds an administrator to that, not to less — and to no more,
since hotserve itself runs unprivileged.

hotserve reads the file each time the app launches, so a missing or
unreadable one fails that launch, and a changed one applies at the
next. A Deno app also needs `--allow-env=` to name each variable it
reads.
