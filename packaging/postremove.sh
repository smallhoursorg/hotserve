#!/bin/sh
# hotserve package post-remove. Only `purge` does anything here: a
# plain remove leaves everything on disk, because an operator removing
# the package to install a different build must not lose their data.
#
# What purge takes is what the package itself put there and what is
# dangerous to leave lying about: the backup credentials (a repository
# password and a storage key — the asset DESIGN-threat-model.md ranks
# fifth) and the staged database copies, which are plaintext copies of
# app data.
#
# What purge deliberately does NOT take: the backups themselves, which
# live in the repository and are the whole point; /var/lib/liveswap,
# which holds the apps' own data; and /etc/hotserve/Caddyfile, which
# dpkg removes as a conffile in its own right.
case "${1:-}" in
remove)
	# As dh_installsystemd does: the unit file is gone, so its enablement
	# is masked until the package comes back — and remembered, so a
	# reinstall restores the timer exactly as the administrator had it.
	if [ -x /usr/bin/deb-systemd-helper ]; then
		deb-systemd-helper mask hotserve-backup.timer >/dev/null || true
	fi
	;;
purge)
	rm -f /etc/hotserve/backup.env
	# And any copy of it: the temporary file `init` writes and renames
	# into place (a crash between the two leaves one), and the settings
	# init's checks run with, which live on tmpfs until the next boot.
	rm -f /etc/hotserve/.backup.env-*
	rm -rf /run/hotserve-backup
	# A purge is the operator starting over: forget whether the timer was
	# enabled, so an install after it enables it again.
	if [ -x /usr/bin/deb-systemd-helper ]; then
		deb-systemd-helper purge hotserve-backup.timer >/dev/null || true
		deb-systemd-helper unmask hotserve-backup.timer >/dev/null || true
	fi
	rm -rf /var/lib/hotserve-backup
	;;
esac
exit 0
