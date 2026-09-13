# After the first deploy

[Your first deploy](first-deploy.md) leaves a working box that root set
up by hand. This page turns it into one a person administers: an
account that is not root, root login closed, the config in git, and
the leftover port shut. Each step stands alone and none touches the
running app; do them in this order, because the second locks you out
without the first.

Names are the ones from the first page, plus `alice`, the
administrator, and `box.example.com`, the box's own name (any name
that resolves to it, `example.com` included).

## 1. Create the administrator

Still as root. This user is not root: a sudoers file grants exactly
the commands that pushing a config needs, and the `adm` group reads
the logs. Everything else an administrator does — checking a config,
reading it, `journalctl` — needs no privilege at all.

```sh
adduser alice
install -d -m 0700 -o alice -g alice /home/alice/.ssh && cp ~/.ssh/authorized_keys /home/alice/.ssh/
curl -fsSL https://raw.githubusercontent.com/smallhoursorg/hotserve/main/examples/box/sudoers \
  -o /etc/sudoers.d/hotserve-admin && chmod 0440 /etc/sudoers.d/hotserve-admin && visudo -c
groupadd hotserve-admin && usermod -aG adm,hotserve-admin alice
```

Before you close the root session, from your laptop:

```sh
ssh alice@box.example.com sudo -n systemctl reload hotserve
```

That works silently or not at all, and it is the whole of what step 3
will need.

What this grants is the `hotserve` user's reach, not root's: whoever
can write the Caddyfile can make hotserve serve any file it can read,
its TLS keys and every app's data included. `adm` reaches a little
wider — the whole system journal, not only hotserve's lines. Give the
account to the people who may change what the box serves.

## 2. Secrets, if the app has any

Skip this if it does not; the examples do not. When it does, the
values go in a file on the box that only root and the `hotserve` user
can read, never in a repo, and the app's block names it with
`env_file`. Creating the file is root's one-time job, and root login
closes in the next step, so make one now for each app that will need
it:

```sh
install -m 0640 -o root -g hotserve /dev/null /etc/hotserve/example.env
```

From then on `alice` edits it with `sudoedit /etc/hotserve/example.env`
(one `KEY=VALUE` per line), which the sudoers file allows, and adds
`env_file /etc/hotserve/example.env` to the app's block. hotserve reads
the file at each launch, so a change applies at the app's next deploy.
A Deno app also needs `--allow-env=` to name each variable it reads.

## 3. Close root login

Once step 1's check passed and nothing else needs root:

```sh
printf 'PermitRootLogin no\nPasswordAuthentication no\n' > /etc/ssh/sshd_config.d/10-hardening.conf
sshd -t && systemctl reload ssh      # -t checks the config first: a bad one would lock you out
```

Log out. From here everything is `alice`, and anything that does need
root again — a package install, a new env file — is the provider's
console.

## 4. Keep the Caddyfile in git

The box's config is one file, and it is where the box's policy lives:
which repo may deploy each app, and each Deno app's permission flags.
In a repo of its own, a change to it is a diff someone reads before it
is pushed, and the push script never leaves a bad file on the box.

Copy [examples/box](../examples/box) into a new **private** repository,
separate from the app's. Then, instead of editing its example
`Caddyfile`, replace it with the one the box is already running:

```sh
scp alice@box.example.com:/etc/hotserve/Caddyfile Caddyfile
```

Set `HOTSERVE_VERSION` in `.github/workflows/check.yml` to the release
the box runs, so CI validates every change with the same binary: the
tag without its `v`. Commit, and push the config once to prove the
loop:

```sh
make push BOX=alice@box.example.com
```

It answers that the box already runs this Caddyfile, which is the
point: the repo now holds what the box runs, and every later change
goes the other way — edit, pull request, merge, `make push`. The
script validates the file on the box, shows the diff against what is
live (including any edit made on the box by hand, which the push would
undo), asks, swaps the file in, and reloads; if the reload fails it
puts the previous file back. The box README's
[Change the config](../examples/box/README.md#change-the-config) is the
full description, and its [Secrets](../examples/box/README.md#secrets)
section covers step 2 in more depth.

## 5. Shut the port you are not using

At the provider's firewall, allow **22 only from your own address**
(home, VPN). 80 and 443 stay open to the world; nothing else on the
box listens on a port, since every app is reached over a unix socket.

## 6. Keep what you installed

Keep the `.deb` you installed from, or note its version:
`apt install --allow-downgrades` of it is the way back from an upgrade
that does not suit you. The README's [Upgrading](../README.md#upgrading)
section has the rest: checking a new version accepts your config before
you install it, what visitors see during the restart, and how to pick a
quiet moment for it.

## Day to day

- **Deploy** by pushing to `main`; **roll back** from Actions → deploy →
  **Run workflow** with a version from the releases page
  ([examples/node](../examples/node/README.md#rolling-back)).
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
