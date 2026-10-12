# Hardening the box

[Your first deploy](first-deploy.md) leaves a working box with the
doors a fresh server has. This page closes them: SSH by key only, the
SSH port shut to everyone but you, and, if you want one, an
administrator who is not root. It is recommended, and none of it is
needed to deploy.

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

An account that is not root, for reading the logs and editing the
secrets without root's key: for a second person, or to keep root's
key for when it is needed. It cannot change the box's config — that is
a signed push — and holds nothing else: the logs through the `adm`
group, and the two commands of
[Secrets](after-first-deploy.md#2-secrets-if-the-app-has-any) through
the `sudoers` file in the box's repository. Read the file: it is
short, and it says what it grants.

From the laptop, in the box's repository, put it on the box, with the
administrator's SSH public key — yours, or the second person's:

```sh
scp sudoers root@box.example.com:/etc/sudoers.d/hotserve-admin
scp alice.pub root@box.example.com:      # the administrator's public key
```

Then as root on the box:

```sh
chmod 0440 /etc/sudoers.d/hotserve-admin && visudo -c
adduser --disabled-password --comment "" alice      # no password: SSH keys only, and the sudoers lines ask for none
install -d -m 0700 -o alice -g alice /home/alice/.ssh
install -m 0600 -o alice -g alice alice.pub /home/alice/.ssh/authorized_keys
groupadd hotserve-admin && usermod -aG adm,hotserve-admin alice
```

Check from the laptop:

```sh
ssh alice@box.example.com journalctl -u hotserve -n 5   # the journal, through adm
ssh alice@box.example.com sudo -n -l                    # the two secrets commands, no password
```

`alice` then follows
[Secrets](after-first-deploy.md#2-secrets-if-the-app-has-any) with
`sudo` in front of the `install` line, and `sudoedit` in place of
`editor`; the sudoers file admits exactly those two.

What this grants: `adm` reads the whole system journal, not only
hotserve's lines, so it is a trust grant in its own right. The secrets
grant is every app's environment, read at the app's next launch — and
for most runtimes an environment variable (`NODE_OPTIONS`, say) is a
way to run code as that app. Give the account to the people who may
change what the apps run.
