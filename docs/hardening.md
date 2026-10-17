# Hardening the box

[Your first deploy](first-deploy.md) leaves a working box with the
doors a fresh server has. This page closes them: SSH by key only, the
SSH port shut to everyone but you, and, if you want them, an
administrator who is not root and Debian's security updates installed
on their own. It is recommended, and none of it is needed to deploy.

Nothing here changes how the box deploys or takes its config: app
deploys authenticate with the workflow's token, and config with a
signed push, and neither uses an account on the box. None of it
touches the running app.

Names are the ones from the first page, plus `alice`, the optional
administrator.

## 1. SSH by key only

As root on the box:

```sh
printf 'PermitRootLogin prohibit-password\nPasswordAuthentication no\n' > /etc/ssh/sshd_config.d/10-hardening.conf
sshd -t && systemctl reload ssh      # -t checks the config first: a bad one would lock you out
```

Keep that session open. In a second terminal on your laptop, log in
again — a new login is the only real test of the change:

```sh
ssh root@box.example.com echo ok     # prints ok: root's key still gets in
```

If it prints `ok`, you are done. If it says `Permission denied`, close
nothing: undo the change in the first terminal with `rm
/etc/ssh/sshd_config.d/10-hardening.conf && systemctl reload ssh`, and
find out why before trying again.

Nobody logs in with a password from here on: Debian allows one for
every account but root. Root
keeps logging in with its key — Debian's default; the line says so in
case the provider's image changed it — because root over SSH is the
way back [at 3am](after-first-deploy.md#at-3am), and a provider's web
console is a poor substitute. Treat root's key accordingly.

## 2. Shut the port you are not using

At the provider's firewall, allow **22 only from your own address**
(home, VPN). 80 and 443 stay open to the world; nothing else on the
box listens on a port, since every app is reached over a unix socket.
If your address changes, change the rule in the provider's firewall
settings, which do not need the box.

## 3. An administrator, if you want one

An account that is not root, for reading the logs: for a second
person, or to keep root's key for when it is needed. It reads
hotserve's journal and every app's through the `adm` group, and that
is all it holds. It needs no `sudo`: the config changes only by signed
pushes from the box's repository, and so do the apps' secrets kept
there ([Secrets](secrets.md)).

From the laptop, put the administrator's SSH public key on the box —
yours, or the second person's:

```sh
scp alice.pub root@box.example.com:      # the administrator's public key
```

Then as root on the box:

```sh
adduser --disabled-password --comment "" alice      # no password: SSH keys only
install -d -m 0700 -o alice -g alice /home/alice/.ssh
install -m 0600 -o alice -g alice alice.pub /home/alice/.ssh/authorized_keys
usermod -aG adm alice
```

Check from the laptop:

```sh
ssh alice@box.example.com journalctl -u hotserve -n 5   # the journal, through adm
```

What this grants: `adm` reads the whole system journal, not only
hotserve's lines, so it is a trust grant in its own right. An app that
writes a secret to its own output puts it there too.

A box set up from an earlier version of the box template may have
that template's `sudoers` file at `/etc/sudoers.d/hotserve-admin`, which
let the administrator create and edit env files under
`/etc/hotserve`. Once the apps' secrets are in the repository
([Move a hand-made env file in](secrets.md#move-a-hand-made-env-file-in)),
remove it and its group, as root: `rm /etc/sudoers.d/hotserve-admin &&
groupdel hotserve-admin`.

## 4. Security updates on their own, if you want them

Debian's `unattended-upgrades` installs Debian's fixes for the box's
release by itself, once a day. Like step 3 it is optional, and nothing
in it stops or restarts an app.

What it does: between 06:00 and 07:00 in the box's time zone
(`systemctl list-timers apt-daily-upgrade.timer` shows the next run)
it installs what Debian has published for the release the box runs,
from Debian's security archive and, at a point release, from the
release itself — and nothing from anywhere else. A daemon whose
package restarts it on upgrade runs the fix at once: `sshd` is one,
and open SSH sessions stay up, since the restart takes only the
listener. Everything started afterwards runs the fixed code: each app
at its next launch (a deploy, a rollback, a relaunch after a crash),
each SSH login, and the `ssh-keygen` a push to the box's repository
runs.

What it does not do: update hotserve, whose `.deb` comes from no
repository ([Upgrading](upgrading.md) is how). Nor does it reach what
a running app or daemon has already loaded, which waits for a
relaunch, or the running kernel, which waits for a reboot, both at a
time you choose; apps run on the box's own `/usr`, so a file an app
first reads after the upgrade is the new one. An app launched long ago
can thus be behind what the box has, and
[Patching the host](upgrading.md#patching-the-host) says how to give
it a launch. It never reboots the box: `Automatic-Reboot` is off
unless you turn it on, and this page does not.

As root on the box, first see whether it is already on, since a
provider's image may ship it:

```sh
apt-config dump APT::Periodic::Unattended-Upgrade    # "1": already on; nothing: not installed; "0": installed, turned off
```

If that prints nothing:

```sh
apt install unattended-upgrades      # turns itself on: writes /etc/apt/apt.conf.d/20auto-upgrades
```

If it prints `"0"`, the image installed the package and turned it
off; installing again changes nothing, so write the file the package
writes when it is on:

```sh
printf 'APT::Periodic::Update-Package-Lists "1";\nAPT::Periodic::Unattended-Upgrade "1";\n' > /etc/apt/apt.conf.d/20auto-upgrades
```

In every case, check what it would do:

```sh
apt-config dump Unattended-Upgrade::Origins-Pattern    # three origin=Debian lines, nothing else
apt-config dump Unattended-Upgrade::Automatic-Reboot   # prints nothing, or "false": no automatic reboot
unattended-upgrade --dry-run -v    # what the next run would install; "No packages found" on a box that is up to date
```

Every run, this dry run included, writes what it did to
`/var/log/unattended-upgrades/unattended-upgrades.log`.
