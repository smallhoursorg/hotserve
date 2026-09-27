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
# and its upload, which is restic's, finishes — stopped, it was sent
# again whole. The helpers it starts from then on are the new
# version's, and each does nothing for a command of another version,
# saying so; that hour's unit is red, and the next run is whole.
#
# stop_backups: at a remove, the backup timers and the services they
# start, stopped and then looked at — the program is about to go. By
# systemctl itself: deb-systemd-invoke asks /usr/sbin/policy-rc.d
# first, and where that says no it skips the stop and exits 0 [M69].
# The timers first, then anything they started: a run under way gets
# SIGTERM, stops its own units by name and removes its plaintext
# copies (TimeoutStopSec=3min in the unit file). What will not stop is
# said, by name and state, and the removal goes on; so is a command
# run from a shell, which holds the run lock and is no unit.
# postremove sweeps what is left, with the lock held.
stop_backups() {
	[ -d /run/systemd/system ] || return 0
	said=$(systemctl stop hotserve-backup.timer hotserve-backup-drill.timer hotserve-backup.service hotserve-backup-drill.service 2>&1) || true
	stuck=""
	for u in hotserve-backup.timer hotserve-backup-drill.timer hotserve-backup.service hotserve-backup-drill.service; do
		state=$(systemctl show -p ActiveState --value "$u" 2>/dev/null) || state=""
		case "$state" in
		inactive | failed | "") ;;
		*)
			stuck="$stuck $u"
			echo "hotserve: $u would not stop (it is $state): a backup is still under way, and its program is about to be removed; the removal goes on" >&2
			;;
		esac
	done
	if [ -n "$stuck" ]; then
		[ -z "$said" ] || echo "hotserve: systemctl stop said: $(echo "$said" | head -1)" >&2
		return 0
	fi
	lock=/run/hotserve-backup/lock
	if [ -f "$lock" ] && command -v flock >/dev/null 2>&1 && ! flock -n "$lock" true 2>/dev/null; then
		echo "hotserve: a backup command is under way from a shell ($(head -1 "$lock" 2>/dev/null)), and is left to run: its program is about to be removed; the removal goes on" >&2
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
