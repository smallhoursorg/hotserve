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
