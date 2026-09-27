#!/bin/sh
# hotserve package pre-remove: stop the service if systemd manages it —
# but only on actual removal. dpkg also runs this script on upgrades
# (prerm upgrade <new-version>); stopping there would take the server
# down for the whole unpack and leave it disabled. Upgrades instead
# restart into the new binary in postinstall — and the deployed apps,
# which live under the hotserve user's own systemd manager, keep
# serving right through that restart.
# An upgrade leaves the backup units alone (the owner, 2026-09-27): a
# run or a drill under way goes on as the program it was started as,
# and its upload, which is restic's, finishes. The helpers it starts
# from then on are the new version's, and each does nothing for a
# command of another version, saying so.
#
# stop_backups: at a remove, the timers and the services, stopped as
# dh_installsystemd stops them (the owner, 2026-09-27); then, while the
# program is still there, `hotserve-backup sweep`: the engine's own
# sweep of what a killed command left — its units, by the names it
# recorded, and what it left mounted under /run/hotserve-backup, made
# private and taken away — under the run lock, as the next command
# would have, since there will be none. A command that holds the lock
# — from a shell, or a service that did not stop — is left to run,
# and the sweep says so in the lock's own words.
stop_backups() {
	[ -d /run/systemd/system ] || return 0
	if [ -x /usr/bin/deb-systemd-invoke ]; then
		deb-systemd-invoke stop hotserve-backup.timer hotserve-backup-drill.timer hotserve-backup.service hotserve-backup-drill.service >/dev/null || true
	else
		systemctl stop hotserve-backup.timer hotserve-backup-drill.timer hotserve-backup.service hotserve-backup-drill.service 2>/dev/null || true
	fi
	[ -x /usr/bin/hotserve-backup ] || return 0
	# Bounded: SIGTERM at five minutes, which the sweep takes as a
	# cancel, and SIGKILL ten seconds after — a unit slow to stop, or a
	# manager that does not answer, must not hold the remove.
	if command -v timeout >/dev/null 2>&1; then sweep="timeout -k 10 300 /usr/bin/hotserve-backup sweep"; else sweep="/usr/bin/hotserve-backup sweep"; fi
	if ! said=$($sweep 2>&1); then
		echo "hotserve: what a backup command left was not swept, and is left: ${said#hotserve-backup: }" >&2
	fi
}
case "${1:-}" in
upgrade | failed-upgrade) ;;
*)
	if command -v systemctl >/dev/null 2>&1; then
		stop_backups
		systemctl stop hotserve 2>/dev/null || true
		systemctl disable hotserve 2>/dev/null || true
		# Removal is the one time the apps go too: stopping the user
		# manager stops every unit under it (cgroup kill), and the
		# linger + drop-in postinstall created come out with it.
		if uid=$(id -u hotserve 2>/dev/null); then
			systemctl stop "user@$uid.service" 2>/dev/null || true
			loginctl disable-linger hotserve 2>/dev/null || rm -f /var/lib/systemd/linger/hotserve
		fi
		rm -f /etc/systemd/system/hotserve.service.d/10-user-manager.conf
		rmdir /etc/systemd/system/hotserve.service.d 2>/dev/null || true
		if [ -n "${uid:-}" ]; then
			rm -f "/etc/systemd/system/user@$uid.service.d/10-hotserve.conf"
			rmdir "/etc/systemd/system/user@$uid.service.d" 2>/dev/null || true
		fi
		systemctl daemon-reload 2>/dev/null || true
	fi
	;;
esac
exit 0
