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
| `sudoers` | The six commands `bin/push` runs as root, and nothing else |
| `Makefile` | `make check` and `make push` |
| `.github/workflows/check.yml` | `make check` on every push and pull request |

## Set it up

1. Copy this directory into a new private repo, and replace
   `example.com`, `deploy.example.com` and `your-org/example` in the
   `Caddyfile` with yours. The `app example` block is
   [examples/deno](../deno)'s; the app's own README walks through its
   side.
2. Set up your user on the box. Administering hotserve needs no
   root: `bin/push` runs as an ordinary user, whose privileged steps
   are the exact commands in `sudoers`, and reads logs through the
   `adm` group. Once, as root:

   ```sh
   sudo install -m 0440 sudoers /etc/sudoers.d/hotserve-admin
   sudo groupadd hotserve-admin
   sudo usermod -aG adm,hotserve-admin alice
   ```

   Then `ssh box.example.com sudo -n systemctl reload hotserve` works
   for alice without a prompt, and `journalctl -u hotserve` needs no
   sudo at all. What that grants is the `hotserve` user's reach, not
   root's — see [Secrets](#secrets) — so give it to the people who
   may change what the box serves.
3. Set `HOTSERVE_VERSION` in `.github/workflows/check.yml` to the
   release the box runs (`dpkg -s hotserve` on the box shows it).

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
