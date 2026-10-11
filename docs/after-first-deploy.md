# After the first deploy

[Your first deploy](first-deploy.md) leaves a working box whose config
is in git. This page has what comes next. First, running it: changing
the config, secrets, and the way back from an upgrade. Then hardening,
which is recommended and none of it needed to deploy: SSH by key only,
the SSH port shut to everyone but you, and, if you want one, an
administrator who is not root. None of it touches the running app.

Names are the ones from the first page, plus `alice`, the optional
administrator.

## 1. Change the config

Edit `deploy.example.com/Caddyfile` in the box's repository, commit —
signed, as every commit from that checkout is — and push to `main`.
The run prints what the box did:

```
apply: deploy.example.com/Caddyfile → https://deploy.example.com/: the box runs 4f1c2a9; sending c6d0e81 (1 commit), push 018f3a…
apply: deploy.example.com at c6d0e81, signed by alice@example.com; verified; installing
--- installed
+++ pushed
…
apply: deploy.example.com at c6d0e81: loaded; running apps are not restarted; apps example
```

The box applied it because a key it lists signed the commit and every
commit since the one it ran, the file is the one in that commit, and
hotserve validates it; then it swapped the file in and reloaded. A
reload that fails puts the previous file back. A push it refuses
changes nothing, and the run says why.

A reload never restarts a running app: a changed `app` block applies
at the app's next launch — its next deploy or rollback, or a relaunch
after a crash. Widen a Deno app's permissions *before* deploying code
that needs them; narrow after the code that needed them is gone.

A change someone else should read first goes on a branch and a pull
request, where the workflow checks it, and lands from a signer's
laptop — never with GitHub's merge button, which cannot make a commit
the box applies:

```sh
git switch main && git pull --ff-only
git merge --ff-only the-branch     # a branch of signed commits, rebased onto main
git push origin main
```

Undoing a change is `git revert` and a push. The box README has the
rest: [the rules](../examples/box/README.md#the-rules) that follow
from what the box checks, [adding a signer](../examples/box/README.md#adding-a-signer),
and [what to do when a push is refused](../examples/box/README.md#when-a-push-is-refused).

## 2. Secrets, if the app has any

Skip this if it does not; the examples do not. Secrets do not go
through the box's repository: the values go in a file on the box that
only root and the `hotserve` user can read, and the app's block names
it with `env_file`. As root on the box (`ssh root@box.example.com`):

```sh
install -m 0640 -o root -g hotserve -T /dev/null /etc/hotserve/example.env   # once: empty, root:hotserve
editor /etc/hotserve/example.env      # one KEY=VALUE per line
```

Then uncomment `env_file /etc/hotserve/example.env` in the app's block
and push it — in that order: hotserve reads the file each time the app
launches, and a missing one fails the launch. A changed file applies
at the app's next launch: its next deploy. A Deno app also needs
`--allow-env=` to name each variable it reads. An administrator who is
not root can do the same (step 6), and the box README's
[Secrets](../examples/box/README.md#secrets) section has more.

## 3. Know the way back

The `.deb` you installed from is still in `/var/local/hotserve`, and
`apt install --allow-downgrades` of it, as root, is the way back from
an upgrade that does not suit you; download each later release into
the same directory. [Upgrading](upgrading.md) has the rest: checking a
new version accepts your config before you install it, what visitors
see during the restart, and how to pick a quiet moment for it.

## Hardening: recommended, not needed to deploy

Nothing below changes how the box deploys or takes its config: app
deploys authenticate with the workflow's token, and config with a
signed push, and neither uses an account on the box.

### 4. SSH by key only

As root on the box:

```sh
printf 'PermitRootLogin prohibit-password\nPasswordAuthentication no\n' > /etc/ssh/sshd_config.d/10-hardening.conf
sshd -t && systemctl reload ssh      # -t checks the config first: a bad one would lock you out
```

Then, from a second terminal and before you close the first, check
that `ssh root@box.example.com true` still gets in.

Nobody logs in with a password from here on: Debian allows one for
every account but root, the administrator's of step 6 included. Root
keeps logging in with its key — Debian's default; the line says so in
case the provider's image changed it — because root over SSH is the
way back at 3am, and a provider's web console is a poor substitute.
Treat root's key accordingly.

### 5. Shut the port you are not using

At the provider's firewall, allow **22 only from your own address**
(home, VPN). 80 and 443 stay open to the world; nothing else on the
box listens on a port, since every app is reached over a unix socket.
If your address changes, change the rule in the provider's firewall
settings, which do not need the box.

### 6. An administrator, if you want one

An account that is not root, for reading the logs and editing the
secrets without root's key: for a second person, or to keep root's
key for when it is needed. It cannot change the box's config — that is
a signed push — and holds nothing else: the logs through the `adm`
group, and the two secrets commands of step 2 through the `sudoers`
file in the box's repository. Read the file: it is short, and it says
what it grants.

From the laptop, in the box's repository, put it on the box:

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

Check from the laptop:

```sh
ssh alice@box.example.com journalctl -u hotserve -n 5   # the journal, through adm
ssh alice@box.example.com sudo -n -l                    # the two secrets commands, no password
```

`alice` then does step 2 with `sudo` in front of the `install` line,
and `sudoedit` in place of `editor`; the sudoers file admits exactly
those two.

What this grants: `adm` reads the whole system journal, not only
hotserve's lines, so it is a trust grant in its own right. The secrets
grant is every app's environment, read at the app's next launch — and
for most runtimes an environment variable (`NODE_OPTIONS`, say) is a
way to run code as that app. Give the account to the people who may
change what the apps run.

## Day to day

- **Deploy** by pushing to `main`; **roll back** from Actions → deploy →
  **Run workflow**, with "Use workflow from" left on `main` and the
  version to go back to: a commit's first 12 characters, as the
  releases page lists them, and one of the newest five, which is what
  the box still holds ([examples/node](../examples/node/README.md#rolling-back)).
- **Change the box** — an app's flags, a second app, a new site, a new
  signer — by a signed push to the box's repository (step 1). The run's
  output is what changed.
- **Logs:** `journalctl -t hotserve-example` for the app's own output,
  `journalctl -u hotserve` for hotserve's, `journalctl -u
  hotserve-box-apply` for what the box did with a push — as root, or as
  `alice` without sudo.
- **If hotserve crashes** — a SIGKILL, out of memory, a bug — systemd
  starts it again a second later and it picks the running apps back up.
  A plain `kill` (SIGTERM) is a clean stop, like `systemctl stop`, and
  stays stopped; so does a start it *refuses*: `journalctl -u hotserve
  -n 50` says why. Fix it, then, as root, `systemctl start hotserve`
  (after "start request repeated too quickly", `systemctl reset-failed
  hotserve` first). A stopped box takes no pushes, so a fix to the
  config is made on the box too ([At 3am](#at-3am)). A restart loads
  `/etc/hotserve/Caddyfile` as it stands: a pushed file that did not
  load was put back, but a hand edit that was not reloaded goes live
  then — or, if it does not load, keeps hotserve down.
- **A second app:** another `app` block and site in the box's
  Caddyfile, pushed; another copy of the example, with its own
  `HOTSERVE_URL`. The Caddyfile is the only thing on the box that
  changes.
- **A second box:** another directory in the box's repository, named
  for that box's deploy host, and `hotserve init` from it once as root
  on the new box. Every push goes to every box
  ([More than one box](../examples/box/README.md#more-than-one-box)).
- **Patching the box** (`apt upgrade`, as root) reaches an app only at
  its next launch; after patching, push a commit to each app or reboot
  ([Upgrading](upgrading.md#patching-the-host)).
- **Everything else:** [liveswap/README.md](../liveswap/README.md) is the
  reference for every option, status code and edge.

## At 3am

If a pushed change broke the deploy host itself, the next push cannot
reach the box. The way back is root over SSH (the provider's console
only if SSH itself is down), in one of two ways:

- Edit `/etc/hotserve/Caddyfile` and `systemctl reload hotserve`, then
  make the same fix a commit and push it. The next signed push
  overwrites a hand edit — its run warns that it did — because the
  repository is the truth and the edit is a bridge back to it.
- Or commit the fix first, and run `hotserve init` with that commit
  from its directory, copied over as the first page did it.

A change that loads but misbehaves is reverted like any commit: `git
revert`, push.
