#!/bin/sh
# hotserve package pre-remove: stop the service if systemd manages it —
# but only on actual removal. dpkg also runs this script on upgrades
# (prerm upgrade <new-version>); stopping there would take the server
# down for the whole unpack and leave it disabled. Upgrades instead
# restart into the new binary in postinstall — and the deployed apps,
# which live under the hotserve user's own systemd manager, keep
# serving right through that restart.
# stop_backups <what comes next, in words>: the backup timers and the
# services they start, stopped and then looked at. By systemctl itself:
# deb-systemd-invoke asks /usr/sbin/policy-rc.d first, and where that
# says no it skips the stop and exits 0 [M69], which would leave a run
# under way. The timers first, then anything they started: a run under
# way gets SIGTERM, stops its own units by name and removes its
# plaintext copies (TimeoutStopSec=3min in the unit file).
#
# What will not stop is said, by name, with its state and the
# manager's own words, and nothing fails for it (the owner,
# 2026-09-27): an upgrade of hotserve is not held back by a backup.
# And a command run from a shell — sudo hotserve-backup run, a restore
# at its prompt — is no unit of the package's: it holds the run lock,
# is said by what the lock says of it, and is left to run.
#
# $2, where it is given, is a file to write the timers that were
# running into: what an upgrade stopped, postinstall starts again, and
# nothing else.
stop_backups() {
	[ -d /run/systemd/system ] || return 0
	if [ -n "${2:-}" ]; then
		: >"$2"
		for u in hotserve-backup.timer hotserve-backup-drill.timer; do
			if systemctl is-active --quiet "$u" 2>/dev/null; then echo "$u" >>"$2"; fi
		done
	fi
	said=$(systemctl stop hotserve-backup.timer hotserve-backup-drill.timer hotserve-backup.service hotserve-backup-drill.service 2>&1) || true
	stuck=""
	for u in hotserve-backup.timer hotserve-backup-drill.timer hotserve-backup.service hotserve-backup-drill.service; do
		state=$(systemctl show -p ActiveState --value "$u" 2>/dev/null) || state=""
		case "$state" in
		inactive | failed | "") ;;
		*)
			stuck="$stuck $u"
			echo "hotserve: $u would not stop (it is $state): a backup is still under way, and $1" >&2
			;;
		esac
	done
	if [ -n "$stuck" ]; then
		[ -z "$said" ] || echo "hotserve: systemctl stop said: $(echo "$said" | head -1)" >&2
		return 0
	fi
	lock=/run/hotserve-backup/lock
	if [ -f "$lock" ] && command -v flock >/dev/null 2>&1 && ! flock -n "$lock" true 2>/dev/null; then
		echo "hotserve: a backup command is under way from a shell ($(head -1 "$lock" 2>/dev/null)), and is left to run: $1" >&2
	fi
}
case "${1:-}" in
upgrade | failed-upgrade)
	# The backup units are the exception: a run or a drill under way
	# starts its helper units from /usr/bin/hotserve-backup by path, and
	# after the unpack that path is the new version's — an old run with
	# new helpers. Stopped here, a run ends as any stopped run does (its
	# units ended, its plaintext removed by the run itself on its way
	# out [M62]); postinstall starts the timers again, those that were
	# enabled, and the next hour's run does the backup this one did not
	# finish.
	stop_backups "what it starts from here on is the new version's; the upgrade goes on" /run/hotserve-backup.stopped-for-upgrade
	;;
*)
	if command -v systemctl >/dev/null 2>&1; then
		stop_backups "its program is about to be removed; the removal goes on"
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
