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
| `sudoers` | The eight commands `bin/push` runs as root, creating and `sudoedit`ing the apps' env files, and nothing else |
| `Makefile` | `make check` and `make push` |
| `.github/workflows/check.yml` | `make check` on every push and pull request |

## Set it up

The box comes from [Your first deploy](../../docs/first-deploy.md):
installed, serving one app from a Caddyfile written by hand, still
administered as root. This repo is what
[After the first deploy](../../docs/after-first-deploy.md) makes of
that, and this is its short form. Anything that needs root and that the
administrator below cannot do — `apt install libatomic1` for a Node
executable on arm64, a Deno under `/usr/local` — happens while you
still have root; the first-deploy page has each.

1. Copy this directory into a new private repo, from a checkout of
   hotserve at the release the box runs
   (`git clone --depth 1 --branch v0.2.0 https://github.com/smallhoursorg/hotserve`).
2. Replace the example `Caddyfile` with the one the box is running:

   ```sh
   scp root@box.example.com:/etc/hotserve/Caddyfile Caddyfile
   ```

   Starting from the live file means the first push changes nothing
   and nothing edited on the box is lost. (Setting up a box from this
   repo instead, with no first deploy behind it: edit the example.
   Replace `example.com`, `deploy.example.com` and `your-org/example` —
   `your-org` twice: the `artifact_allowlist` entry pins the
   organization alone, and `deploy_trust` the repository. The
   `app example` block is [examples/deno](../deno)'s; for
   [examples/node](../node), replace its `command`, `pre_start` and
   `env` lines with the ones in `examples/node/hotserve.caddy`.)
3. Set `HOTSERVE_VERSION` in `.github/workflows/check.yml` to the
   release the box runs: the tag without its `v`. `dpkg -s hotserve`
   shows it, except that dpkg writes a prerelease as `0.2.0~rc1`
   where the tag is `0.2.0-rc1`.
4. Commit, and push once, as root, to prove the loop:

   ```sh
   make push BOX=root@box.example.com
   ```

   It answers that the box already runs this Caddyfile. `bin/push`
   runs its privileged steps through `sudo -n`, which root passes
   without a password; the next step gives it a user that is not root.
5. Create the user you will administer it as. The `sudoers` file in
   this directory grants exactly the eight commands `bin/push` runs as
   root, plus creating and `sudoedit`ing the apps' env files under
   `/etc/hotserve` (see [Secrets](#secrets)), and nothing else; the
   `adm` group reads the logs. From the laptop, from this repo:

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

   Check `ssh alice@box.example.com sudo -n systemctl reload hotserve`
   works before you close the root session: that is the whole of what
   `make push BOX=alice@box.example.com` needs from now on, and
   `journalctl -u hotserve` needs no sudo at all. What that grants is
   the `hotserve` user's reach, not root's — see [Secrets](#secrets) —
   plus, through `adm`, the whole system journal, not only hotserve's
   lines. Give it to the people who may change what the box serves.

Closing root login and the rest of the hardening is on
[After the first deploy](../../docs/after-first-deploy.md).

## Change the config

Edit the `Caddyfile`, open a pull request (CI validates it), merge,
then from a checkout of `main`:

```sh
make push BOX=alice@box.example.com     # the administrator from Set it up
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

After the first push, with nothing deployed yet, a correct box looks
like this:

- `https://example.com` answers 503 with the sentence from the
  Caddyfile's `handle_errors` block — once the certificate is issued,
  which takes a few seconds after the reload with 80 and 443 open.
- `https://deploy.example.com/example` answers 401 with
  `{"error":"invalid or missing deploy token …"}`, in a browser or
  with `curl -i`. That is the webhook working: a deploy carries a
  token, and nothing without one gets further than this. Check once:
  the eleventh tokenless request in a minute from one address gets 429
  instead, and a warning in the journal — the limiter, not a fault.
- `journalctl -u hotserve | grep 'liveswap started'` gains a line with
  `"apps":1,"app_names":["example"]`.

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
`env_file` in the app's block. `sudoers` lets an administrator create
the file and edit it; both are its exact lines, so nothing about the
mode, the owner or the directory is theirs to choose:

```sh
sudo install -m 0640 -o root -g hotserve /dev/null /etc/hotserve/example.env   # once: empty, root:hotserve
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
