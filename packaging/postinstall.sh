#!/bin/sh
set -e
# nss <getent or id arguments>: one lookup in the account databases,
# bounded — a user directory that does not answer would otherwise hold
# dpkg's configure for as long as it likes (Copilot on #155). Exit 2 is
# "not found"; 124 is "did not answer", which is never taken for "not
# found": that would make a local account beside the directory's.
nss() {
	if command -v timeout >/dev/null 2>&1; then
		timeout 30 "$@"
	else
		"$@"
	fi
}
# absent <getent arguments>: the entry is not there, as the databases
# answered; 0 only for that.
absent() {
	st=0
	nss getent "$@" >/dev/null 2>&1 || st=$?
	[ "$st" = 2 ]
}
if command -v useradd >/dev/null 2>&1; then
	if absent group hotserve; then groupadd --system hotserve; fi
	if absent passwd hotserve; then useradd --system \
		--gid hotserve --home-dir /var/lib/hotserve \
		--shell /usr/sbin/nologin --comment "hotserve server" hotserve; fi
else
	# Alpine (busybox) fallback.
	addgroup -S hotserve 2>/dev/null || true
	adduser -S -G hotserve -h /var/lib/hotserve -s /sbin/nologin hotserve 2>/dev/null || true
fi
if ! uid=$(nss id -u hotserve 2>/dev/null); then
	echo "hotserve: the hotserve system user does not exist and could not be created" >&2
	exit 1
fi
# Best-effort: put the account IN the hotserve group. Both branches above
# set the group only when they create the account, so an account that
# already existed — an admin's, or one from another package — keeps
# whatever groups it had, and the state directories chowned below are
# group-hotserve. Not fatal: nothing the package installs is reachable
# only through the group.
in_group() { nss id -nG hotserve 2>/dev/null | tr ' ' '\n' | grep -qx hotserve; }
if ! in_group; then
	if command -v usermod >/dev/null 2>&1; then
		usermod -aG hotserve hotserve || true
	else
		addgroup hotserve hotserve 2>/dev/null || true # busybox
	fi
fi
if ! in_group; then
	echo "hotserve: the hotserve user is not a member of the hotserve group; add it with \`usermod -aG hotserve hotserve\` if its state directories become unreadable" >&2
fi
chown hotserve:hotserve /var/lib/hotserve /var/lib/liveswap
# The account hotserve-backup's restic units run as: the one line
# `hotserve-backup setup` uses (backups/engine/setup.go, held to this
# script by a test), which also makes it when it is missing. No home,
# no shell, nothing of its own but the cache the manager makes for it.
# Its comment, made-by-hotserve, marks it as hotserve's. An account of
# that name that exists is left as it is — an administrator may have
# meant it — and setup and every run use it only where it is
# hotserve's: in /etc/passwd, with the mark. Whoever is the account
# reads the repository credential from a running restic's
# environment. Never removed on purge (Debian policy: system accounts
# stay).
#
# Where it cannot be made — a group of its name is there without it,
# and useradd exits 9 [M66] — that is said, and the configure goes on
# (the owner, 2026-09-27): hotserve is installed on a box that may use
# no backups, and setup, which makes the account when it is missing,
# is where that is refused.
backup_st=0
nss getent passwd hotserve-backup >/dev/null 2>&1 || backup_st=$?
if [ "$backup_st" = 2 ]; then
	if command -v useradd >/dev/null 2>&1; then
		why=$(useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin --comment made-by-hotserve hotserve-backup 2>&1) || true
	else
		why=$(adduser -S -H -h /nonexistent -s /sbin/nologin -g made-by-hotserve hotserve-backup 2>&1) || true # busybox
	fi
fi
if [ "$backup_st" != 0 ] && [ "$backup_st" != 2 ]; then
	case "$backup_st" in
	124) why="the user directory did not answer within 30s" ;;
	*) why="getent exited $backup_st" ;;
	esac
	echo "hotserve: whether the hotserve-backup account exists could not be told ($why); no account is made beside the directory's, and backups wait for it: sudo hotserve-backup account says when it is right" >&2
elif nss getent passwd hotserve-backup >/dev/null 2>&1; then
	# The rule is setup's, and in setup's words: the binary this package
	# has just installed says whether the account is one setup and every
	# run accept. A warning, never the install's failure.
	if [ -x /usr/bin/hotserve-backup ] && ! said=$(/usr/bin/hotserve-backup account 2>&1); then
		echo "hotserve: ${said#hotserve-backup: }" >&2
		echo "hotserve: hotserve-backup setup, and every run, restore and drill, refuse that account until it is put right; hotserve-backup account says when it is" >&2
	fi
else
	echo "hotserve: the hotserve-backup account could not be made (${why:-no reason was given}); backups wait for it, and nothing else of hotserve does: sudo hotserve-backup setup <repository> makes it, or says why it cannot" >&2
fi
# Packages before the Debian-13-only matrix copied an AppArmor profile
# into /etc/apparmor.d (which the package itself does not own, so dpkg
# will not remove it on upgrade). Unload and delete it: apparmor.service
# parses that whole directory at every boot, and the wrapper it attached
# to is gone.
if [ -f /etc/apparmor.d/hotserve-user-manager ]; then
	if [ -d /sys/kernel/security/apparmor ] && command -v apparmor_parser >/dev/null 2>&1; then
		apparmor_parser -R /etc/apparmor.d/hotserve-user-manager 2>/dev/null || true
	fi
	rm -f /etc/apparmor.d/hotserve-user-manager
fi
if command -v systemctl >/dev/null 2>&1; then
	# liveswap runs deployed apps as transient units under the hotserve
	# user's own systemd manager (user@$uid.service), so they outlive
	# hotserve restarts and upgrades. Lingering keeps that manager
	# running with nobody logged in; the drop-in starts it alongside
	# hotserve and orders hotserve after it. No polkit and no root path:
	# hotserve talks to its own manager over /run/user/$uid.
	mkdir -p /etc/systemd/system/hotserve.service.d
	cat > /etc/systemd/system/hotserve.service.d/10-user-manager.conf <<EOF
# Generated by the hotserve package (postinstall); do not edit.
# liveswap's app units run under the hotserve user's manager.
[Unit]
Wants=user@$uid.service
After=user@$uid.service
EOF
	# The user manager's own limits are what its units inherit; match
	# hotserve.service so apps keep the file-descriptor headroom they
	# had as its children. (Effective from systemd 256, which is the
	# floor of the support matrix — see README.)
	mkdir -p "/etc/systemd/system/user@$uid.service.d"
	cat > "/etc/systemd/system/user@$uid.service.d/10-hotserve.conf" <<EOF
# Generated by the hotserve package (postinstall); do not edit.
[Service]
LimitNOFILE=1048576
EOF
	systemctl daemon-reload || true
	# Lingering starts user@$uid.service at once, so it comes after the
	# drop-ins above: the manager must be born with those limits. (An
	# already-running manager keeps them until it next starts —
	# restarting it would stop every app, which an upgrade must not do;
	# until then the sandbox probe reports what the running manager can
	# do.)
	loginctl enable-linger hotserve 2>/dev/null || {
		mkdir -p /var/lib/systemd/linger && touch /var/lib/systemd/linger/hotserve
	}
	systemctl try-restart hotserve 2>/dev/null || true
	echo "hotserve installed. Start it with:"
	echo "  sudo systemctl enable --now hotserve"
fi
# The backup timers, the way dh_installsystemd would have it: enabled on
# first installation; an administrator's `disable` kept across upgrades
# (was-enabled is false then, and update-state only tidies the
# symlinks); started, or restarted on an upgrade ($2 is the version
# upgraded from), by deb-systemd-invoke, which starts nothing that is
# disabled or masked. The services they start do nothing until
# `hotserve-backup setup` has written the credential file
# (ConditionPathExists= in the unit files): an install runs no backup.
if [ -x /usr/bin/deb-systemd-helper ]; then
	for u in hotserve-backup.timer hotserve-backup-drill.timer; do
		deb-systemd-helper unmask "$u" >/dev/null || true
		if deb-systemd-helper --quiet was-enabled "$u"; then
			deb-systemd-helper enable "$u" >/dev/null || true
		else
			deb-systemd-helper update-state "$u" >/dev/null || true
		fi
	done
fi
# Started on an install, restarted on an upgrade ($2 is the version
# upgraded from), exactly as dh_installsystemd does it (the owner,
# 2026-09-27): deb-systemd-invoke starts nothing disabled or masked,
# and obeys a policy-rc.d. So an upgrade starts again a timer an
# administrator stopped and left enabled — disable it to keep it off.
# Nothing of the backups was stopped for an upgrade: a run under way
# is left to finish.
if [ -d /run/systemd/system ]; then
	systemctl --system daemon-reload >/dev/null || true
	if [ -n "${2:-}" ]; then action=restart; else action=start; fi
	if [ -x /usr/bin/deb-systemd-invoke ]; then
		deb-systemd-invoke "$action" hotserve-backup.timer hotserve-backup-drill.timer >/dev/null || true
	else
		# No helper (a systemd host that is not Debian): enabled here,
		# and started, with no memory of an administrator's disable.
		systemctl enable hotserve-backup.timer hotserve-backup-drill.timer 2>/dev/null || true
		systemctl "$action" hotserve-backup.timer hotserve-backup-drill.timer 2>/dev/null || true
	fi
	if systemctl is-enabled --quiet hotserve-backup.timer 2>/dev/null; then
		echo "Backups: the hourly timer and the Sunday restore drill are enabled, and run nothing until:"
		echo "  sudo hotserve-backup setup <repository>"
	else
		echo "Backups: the hourly timer and the Sunday restore drill are disabled, as they were left; sudo systemctl enable --now hotserve-backup.timer hotserve-backup-drill.timer turns them on"
	fi
fi
