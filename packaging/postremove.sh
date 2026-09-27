#!/bin/sh
# hotserve package post-remove. On removal the backup timers are masked,
# the way dh_installsystemd has it, so that nothing of theirs comes back
# until a reinstall unmasks and re-enables them. On purge their enable
# state goes too, and of what hotserve-backup keeps only what is the
# package's own is removed:
#
# - /etc/hotserve-backup/repository.env is KEPT, and said: it holds the
#   repository password, the one way to read the backups already made
#   (PLAN D6). Whoever purges the package decides about the backups.
# - /var/lib/hotserve-backup: the record, the repository id and the
#   listing's stderr go; staging/ and restore/ are removed with rmdir,
#   never rm -r — a directory that is not empty holds copies of an
#   app's data that a killed run left, which root does not delete
#   (backups/README.md, "Who runs as what": root never reads or removes
#   app data) — and is named and kept.
# - /var/cache/hotserve-backup, restic's cache, owned by the account and
#   holding nothing of an app's, goes.
# - /run/hotserve-backup is NOT touched: a run killed mid-way leaves
#   an app's data bind-mounted under its run directory, and an rm -r
#   there would remove the app's files through the mount. It is tmpfs,
#   gone at boot, and the next run's sweep takes such mounts away.
# - The hotserve-backup account stays (Debian policy: system accounts
#   are never removed).
set -e
timers="hotserve-backup.timer hotserve-backup-drill.timer"
case "${1:-}" in
remove)
	if [ -x /usr/bin/deb-systemd-helper ]; then
		# shellcheck disable=SC2086 # two unit names
		deb-systemd-helper mask $timers >/dev/null || true
	fi
	;;
purge)
	if [ -x /usr/bin/deb-systemd-helper ]; then
		# shellcheck disable=SC2086 # two unit names
		deb-systemd-helper purge $timers >/dev/null || true
		# shellcheck disable=SC2086
		deb-systemd-helper unmask $timers >/dev/null || true
	fi
	rm -rf /var/cache/hotserve-backup
	state=/var/lib/hotserve-backup
	if [ -d "$state" ]; then
		# The record, its asides, the id, the listing's stderr, and the
		# temp file the record's writer leaves when killed mid-write.
		rm -f "$state/status.json" "$state"/status.json.aside-* "$state"/.status-* "$state/repository-id" "$state/listing.err"
		kept=""
		for d in "$state/staging" "$state/restore"; do
			[ -d "$d" ] || continue
			# Empty it of empty per-app directories, then itself; whatever
			# holds a file stays.
			for a in "$d"/*; do [ -d "$a" ] && rmdir "$a" 2>/dev/null || true; done
			rmdir "$d" 2>/dev/null || kept="$kept $d"
		done
		rmdir "$state" 2>/dev/null || kept="$kept $state"
		if [ -n "$kept" ]; then
			echo "hotserve: kept$kept: not empty — copies of an app's data a backup run or a restore left there; look, then remove them yourself" >&2
		fi
	fi
	if [ -e /etc/hotserve-backup/repository.env ]; then
		echo "hotserve: /etc/hotserve-backup/repository.env is kept: it holds the repository password, the only way to read the backups already made. Remove it yourself once those backups are not needed: sudo rm -r /etc/hotserve-backup" >&2
	fi
	;;
esac
if [ -d /run/systemd/system ]; then
	systemctl --system daemon-reload >/dev/null || true
fi
exit 0
