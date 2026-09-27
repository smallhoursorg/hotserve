#!/bin/sh
# hotserve package pre-remove: stop the service if systemd manages it —
# but only on actual removal. dpkg also runs this script on upgrades
# (prerm upgrade <new-version>); stopping there would take the server
# down for the whole unpack and leave it disabled. Upgrades instead
# restart into the new binary in postinstall — and the deployed apps,
# which live under the hotserve user's own systemd manager, keep
# serving right through that restart.
case "${1:-}" in
upgrade|failed-upgrade)
	# The backup units are the exception: a run or a drill under way
	# starts its helper units from /usr/bin/hotserve-backup by path, and
	# after the unpack that path is the new version's — an old run with
	# new helpers. Stopped here, a run ends as any stopped run does (its
	# units ended, its plaintext removed by the run itself on its way
	# out [M62]); postinstall starts
	# the timers again, those that were enabled, and the next hour's run
	# does the backup this one did not finish.
	if [ -d /run/systemd/system ]; then
		if [ -x /usr/bin/deb-systemd-invoke ]; then
			deb-systemd-invoke stop hotserve-backup.timer hotserve-backup-drill.timer hotserve-backup.service hotserve-backup-drill.service >/dev/null || true
		else
			systemctl stop hotserve-backup.timer hotserve-backup-drill.timer hotserve-backup.service hotserve-backup-drill.service 2>/dev/null || true
		fi
	fi
	;;
*)
	if command -v systemctl >/dev/null 2>&1; then
		# The backup timers first, then anything they started: a run
		# under way gets SIGTERM, stops its own units by name and removes
		# its plaintext copies (TimeoutStopSec=3min in the unit file).
		if [ -d /run/systemd/system ]; then
			if [ -x /usr/bin/deb-systemd-invoke ]; then
				deb-systemd-invoke stop hotserve-backup.timer hotserve-backup-drill.timer hotserve-backup.service hotserve-backup-drill.service >/dev/null || true
			else
				systemctl stop hotserve-backup.timer hotserve-backup-drill.timer hotserve-backup.service hotserve-backup-drill.service 2>/dev/null || true
			fi
		fi
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
