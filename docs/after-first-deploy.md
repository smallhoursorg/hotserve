# After the first deploy

[Your first deploy](first-deploy.md) leaves a working box that root set
up by hand. This page turns it into one a person administers: the
config in git, an account that is not root, root login closed, and the
leftover port shut. None of it touches the running app. Do the steps
in this order: the second takes a file from the first, and the fourth
locks you out without the second.

Names are the ones from the first page, plus `alice`, the
administrator, and `box.example.com`, the box's own name (any name
that resolves to it, `example.com` included).

## 1. Keep the Caddyfile in git

The box's config is one file, and it is where the box's policy lives:
which repo may deploy each app, and each Deno app's permission flags.
In a repo of its own, a change to it is a diff someone reads before it
is pushed, and the push script never leaves a bad file on the box.

On your laptop, take [examples/box](../examples/box) from a checkout of
hotserve at the release the box runs, and make it a new **private**
repository, separate from the app's:

```sh
git clone --depth 1 --branch v0.2.0 https://github.com/smallhoursorg/hotserve
cp -r hotserve/examples/box example-box && cd example-box && git init
```

Then, instead of editing its example `Caddyfile`, replace it with the
one the box is already running:

```sh
scp root@box.example.com:/etc/hotserve/Caddyfile Caddyfile
```

Set `HOTSERVE_VERSION` in `.github/workflows/check.yml` to the same
release, the tag without its `v`, so CI validates every change with the
binary the box has. Commit, and push the config once to prove the
loop:

```sh
make push BOX=root@box.example.com
```

It answers that the box already runs this Caddyfile, which is the
point: the repo now holds what the box runs, and every later change
goes the other way — edit, pull request, merge, `make push`. The
script validates the file on the box, shows the diff against what is
live (including any edit made on the box by hand, which the push would
undo), asks, swaps the file in, and reloads; if the reload fails it
puts the previous file back. It runs every privileged step through
`sudo -n`, which root passes without a password; step 2 gives it a
user that is not root. The box README's
[Change the config](../examples/box/README.md#change-the-config) is the
full description.

## 2. Create the administrator

This user is not root. The `sudoers` file in the repo you just made
grants exactly the eight commands `make push` runs as root, plus
creating and editing the apps' env files under `/etc/hotserve`
(step 3), and nothing else; the `adm` group reads the logs. Everything else an
administrator does — checking a config, reading it, `journalctl` —
needs no privilege at all. Read the file: it is short, and it says
what it grants.

From the laptop, put it on the box:

```sh
scp sudoers root@box.example.com:/etc/sudoers.d/hotserve-admin
```

Then as root on the box:

```sh
chmod 0440 /etc/sudoers.d/hotserve-admin && visudo -c
adduser alice
install -d -m 0700 -o alice -g alice /home/alice/.ssh && cp ~/.ssh/authorized_keys /home/alice/.ssh/
groupadd hotserve-admin && usermod -aG adm,hotserve-admin alice
```

Before you close the root session, from your laptop:

```sh
ssh alice@box.example.com sudo -n systemctl reload hotserve
```

That works silently or not at all, and it is the whole of what
`make push BOX=alice@box.example.com` needs from now on.

What this grants is the `hotserve` user's reach, not root's: whoever
can write the Caddyfile can make hotserve serve any file it can read,
its TLS keys and every app's data included. `adm` reaches a little
wider — the whole system journal, not only hotserve's lines. Give the
account to the people who may change what the box serves.

## 3. Secrets, if the app has any

Skip this if it does not; the examples do not. When it does, the
values go in a file on the box that only root and the `hotserve` user
can read, never in a repo, and the app's block names it with
`env_file`. This is `alice`'s job, now or whenever an app first needs
one: the sudoers file admits exactly one way to create the file and
one way to edit it.

```sh
sudo install -m 0640 -o root -g hotserve /dev/null /etc/hotserve/example.env   # once: empty, root:hotserve
sudoedit /etc/hotserve/example.env      # one KEY=VALUE per line
```

Then add `env_file /etc/hotserve/example.env` to the app's block.
hotserve reads the file at each launch, so a change applies at the
app's next deploy. A Deno app also needs `--allow-env=` to name each
variable it reads. The box README's
[Secrets](../examples/box/README.md#secrets) section has more.

## 4. Close root login

Once step 2's check passed and nothing else needs root:

```sh
printf 'PermitRootLogin no\nPasswordAuthentication no\n' > /etc/ssh/sshd_config.d/10-hardening.conf
sshd -t && systemctl reload ssh      # -t checks the config first: a bad one would lock you out
```

Log out. From here everything is `alice`, and anything that does need
root again — a package install, say — is the provider's console.

## 5. Shut the port you are not using

At the provider's firewall, allow **22 only from your own address**
(home, VPN). 80 and 443 stay open to the world; nothing else on the
box listens on a port, since every app is reached over a unix socket.

## 6. Know the way back

The `.deb` you installed from is still in `/var/local/hotserve`, and
`apt install --allow-downgrades` of it is the way back from an upgrade
that does not suit you; download each later release into the same
directory. [Upgrading](upgrading.md) has
the rest: checking a new version accepts your config before you install
it, what visitors see during the restart, and how to pick a quiet
moment for it.

## Day to day

- **Deploy** by pushing to `main`; **roll back** from Actions → deploy →
  **Run workflow**, with "Use workflow from" left on `main` and the
  version to go back to: a commit's first 12 characters, as the
  releases page lists them, and one of the newest five, which is what
  the box still holds ([examples/node](../examples/node/README.md#rolling-back)).
- **Logs:** `journalctl -t hotserve-example` for the app's own output,
  `journalctl -u hotserve` for hotserve's, both without sudo as `alice`.
- **A second app:** another `app` block and site in the Caddyfile,
  pushed from the box repo; another copy of the example. The Caddyfile
  is the only thing on the box that changes.
- **A changed `app` block** applies at the app's next launch — its next
  deploy or rollback, or a relaunch after a crash — never to the one
  running. Widen a Deno app's permissions before deploying code that
  needs them; narrow after the code that needed them is gone.
- **Everything else:** [liveswap/README.md](../liveswap/README.md) is the
  reference for every option, status code and edge.
