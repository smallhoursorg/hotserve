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
	;;
*)
	if command -v systemctl >/dev/null 2>&1; then
		systemctl stop hotserve 2>/dev/null || true
		systemctl disable hotserve 2>/dev/null || true
		# The backup timer goes with it, and so does anything it has
		# already started: stopping a timer does not stop the run it
		# triggered, nor the per-app job that run created. A purge
		# deletes the staging dir and the credentials, so a job still
		# reading them would fail in confusing ways — or keep a
		# database copy alive in a directory dpkg has removed.
		# Stopped, not disabled: whether it is enabled is
		# deb-systemd-helper's to remember across a remove and
		# reinstall (postremove masks it; see postinstall).
		systemctl stop hotserve-backup.timer 2>/dev/null || true
		systemctl stop hotserve-backup.service 2>/dev/null || true
		systemctl stop hotserve-backup_verify.timer hotserve-backup_verify.service 2>/dev/null || true
		# The per-app jobs are transient units named hotserve-backup-<app>;
		# a check, a status report's restic and the like, hotserve-backup_<what>.
		for u in $(systemctl list-units --no-legend --plain 'hotserve-backup[-_]*.service' 2>/dev/null | awk '{print $1}'); do
			systemctl stop "$u" 2>/dev/null || true
		done
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
