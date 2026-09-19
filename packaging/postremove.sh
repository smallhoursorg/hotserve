#!/bin/sh
# hotserve package post-remove. A plain remove leaves everything on
# disk, because an operator removing the package to install a different
# build must not lose their data.
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
	# Said, because it is the one thing here that cannot be put back from
	# the package: the repository itself is untouched, and opens only with
	# the password that was in this file.
	if [ -e /etc/hotserve/backup.env ]; then
		echo "hotserve: removing /etc/hotserve/backup.env, which held this box's only copy of the backup repository's password. The repository is untouched; restoring from it needs the password printed when backups were set up." >&2
	fi
	rm -f /etc/hotserve/backup.env
	# And any copy of it: the temporary file `init` writes and renames
	# into place (a crash between the two leaves one), and the settings
	# init's checks run with, which live on tmpfs until the next boot.
	rm -f /etc/hotserve/.backup.env-*
	rm -rf /run/hotserve-backup /run/hotserve-backup-check
	# A purge is the operator starting over: forget whether the timer was
	# enabled, so an install after it enables it again.
	if [ -x /usr/bin/deb-systemd-helper ]; then
		deb-systemd-helper purge hotserve-backup.timer >/dev/null || true
		deb-systemd-helper unmask hotserve-backup.timer >/dev/null || true
	fi
	rm -rf /var/lib/hotserve-backup /var/lib/hotserve-backup-status /var/lib/hotserve-backup-verify
	;;
esac
exit 0
