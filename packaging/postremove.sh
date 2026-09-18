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
purge)
	rm -f /etc/hotserve/backup.env
	# The marker that says the backup timer has been enabled once. A
	# purge is the operator starting over, and an install after one
	# should enable the timer again.
	rm -f /etc/hotserve/.backup-timer-configured
	rm -rf /var/lib/hotserve-backup
	;;
esac
exit 0
