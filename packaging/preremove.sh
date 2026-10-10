#!/bin/sh
# hotserve package pre-remove: stop the service if systemd manages it —
# but only on actual removal (`remove`, including `remove in-favour`
# when a conflicting package replaces this one). dpkg also runs this
# script on upgrades (prerm upgrade <new-version>); stopping there would
# take the server down for the whole unpack and leave it disabled.
# Upgrades instead restart into the new binary in postinstall — and the
# deployed apps, which live under the hotserve user's own systemd
# manager, keep serving right through that restart. `deconfigure` is
# not removal either: dpkg runs it when a package being installed
# Breaks hotserve or one of its dependencies (dbus, libpam-systemd,
# swapped out during a dist-upgrade), the package stays installed, and postinstall's later `configure` only
# try-restarts — nothing would re-enable a torn-down box.
case "${1:-}" in
remove)
	if command -v systemctl >/dev/null 2>&1; then
		systemctl stop hotserve 2>/dev/null || true
		# The box applier's path unit: stopped after hotserve, not
		# disabled, the service left to finish — box/DESIGN-box.md, "The
		# applier unit", says why each, and what a removal can leave in
		# in/.
		if [ -d /run/systemd/system ]; then
			if [ -x /usr/bin/deb-systemd-invoke ]; then
				deb-systemd-invoke stop hotserve-box-apply.path >/dev/null || true
			else
				systemctl stop hotserve-box-apply.path 2>/dev/null || true
			fi
		fi
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
*)
	# upgrade, failed-upgrade, deconfigure: keep serving.
	;;
esac
exit 0
