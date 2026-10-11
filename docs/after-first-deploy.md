# After the first deploy

[Your first deploy](first-deploy.md) leaves a working box whose config
is in git, and root still logs in to it. This page makes it one a
person administers: how a config change goes, an account that is not
root, secrets, root login closed, and the leftover port shut. None of
it touches the running app. Do the steps in this order: the third uses
the account the second creates, and the fourth locks you out without
it.

Names are the ones from the first page, plus `alice`, the
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

## 2. Create the administrator

This user is not root, and cannot change the box's config: that is a
signed push. It reads the logs — hotserve's, every app's, and what the
box did with each push — through the `adm` group, and creates and
edits the apps' secret files (step 3) through the `sudoers` file in
the box's repository, which grants exactly those two commands as root
and nothing else. Read the file: it is short, and it says what it
grants.

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

Before you close the root session, check from your laptop that `alice`
gets in and reads the journal:

```sh
ssh alice@box.example.com journalctl -u hotserve -n 5
```

What this grants: `adm` reads the whole system journal, not only
hotserve's lines, so it is a trust grant in its own right. The secrets
grant is every app's environment, read at the app's next launch — and
for most runtimes an environment variable (`NODE_OPTIONS`, say) is a
way to run code as that app. Give the account to the people who may
change what the apps run.

## 3. Secrets, if the app has any

Skip this if it does not; the examples do not. Secrets do not go
through the box's repository: the values go in a file on the box that
only root and the `hotserve` user can read, and the app's block names
it with `env_file`. `alice` creates the file and edits it over SSH,
and the sudoers file admits exactly one way to do each:

```sh
sudo install -m 0640 -o root -g hotserve -T /dev/null /etc/hotserve/example.env   # once: empty, root:hotserve
sudoedit /etc/hotserve/example.env      # one KEY=VALUE per line
```

Then uncomment `env_file /etc/hotserve/example.env` in the app's block
and push it — in that order: hotserve reads the file each time the app
launches, and a missing one fails the launch. A changed file applies
at the app's next launch: its next deploy. A Deno app also needs
`--allow-env=` to name each variable it reads. The box README's
[Secrets](../examples/box/README.md#secrets) section has more.

## 4. Close root login

Once step 2's check passed:

```sh
printf 'PermitRootLogin no\nPasswordAuthentication no\n' > /etc/ssh/sshd_config.d/10-hardening.conf
sshd -t && systemctl reload ssh      # -t checks the config first: a bad one would lock you out
```

Log out. From here the config is a signed push, and `alice` reads the
logs and edits the secrets. Anything that needs root again — a package
install, an upgrade, `hotserve init` or `hotserve box baseline` — is
the provider's console.

## 5. Shut the port you are not using

At the provider's firewall, allow **22 only from your own address**
(home, VPN). 80 and 443 stay open to the world; nothing else on the
box listens on a port, since every app is reached over a unix socket.

## 6. Know the way back

The `.deb` you installed from is still in `/var/local/hotserve`, and
`apt install --allow-downgrades` of it, as root at the console, is the
way back from an upgrade that does not suit you; download each later
release into the same directory. [Upgrading](upgrading.md) has the
rest: checking a new version accepts your config before you install
it, what visitors see during the restart, and how to pick a quiet
moment for it.

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
  hotserve-box-apply` for what the box did with a push — all without
  sudo as `alice`.
- **If hotserve crashes** — a SIGKILL, out of memory, a bug — systemd
  starts it again a second later and it picks the running apps back up.
  A plain `kill` (SIGTERM) is a clean stop, like `systemctl stop`, and
  stays stopped; so does a start it *refuses*: `journalctl -u hotserve
  -n 50` says why. Fix it, then, as root at the console, `systemctl
  start hotserve` (after "start request repeated too quickly",
  `systemctl reset-failed hotserve` first). A stopped box takes no
  pushes, so a fix to the config is made there too ([At 3am](#at-3am)).
  A restart loads `/etc/hotserve/Caddyfile` as it stands: a pushed file
  that did not load was put back, but a hand edit at the console that
  was not reloaded goes live then — or, if it does not load, keeps
  hotserve down.
- **A second app:** another `app` block and site in the box's
  Caddyfile, pushed; another copy of the example, with its own
  `HOTSERVE_URL`. The Caddyfile is the only thing on the box that
  changes.
- **A second box:** another directory in the box's repository, named
  for that box's deploy host, and `hotserve init` from it once as root
  on the new box. Every push goes to every box
  ([More than one box](../examples/box/README.md#more-than-one-box)).
- **Patching the box** (`apt upgrade`, at the console) reaches an app
  only at its next launch; after patching, push a commit to each app or
  reboot ([Upgrading](upgrading.md#patching-the-host)).
- **Everything else:** [liveswap/README.md](../liveswap/README.md) is the
  reference for every option, status code and edge.

## At 3am

If a pushed change broke the deploy host itself, the next push cannot
reach the box. The way back is the provider's console, as root: edit
`/etc/hotserve/Caddyfile` and `systemctl reload hotserve`, or copy the
box's directory over and run `hotserve init` again from it. Then make
the fix a commit: the next signed push overwrites a hand edit — its run
warns that it did — because the repository is the truth and the edit
is a bridge back to it. A change that loads but misbehaves is reverted
like any commit: `git revert`, push.
