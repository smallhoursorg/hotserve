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
# - /run/hotserve-backup: a run killed mid-way leaves an app's data
#   bind-mounted under its run directory, and an rm -r there would
#   remove the app's files through the mount [measured in #153]. After
#   a remove or a purge there is no next run to sweep such mounts, so
#   they are unmounted here, deepest first, as the sweep does; never
#   rm -r. On purge the run directories then go by rmdir, and their
#   files one by one; what would not unmount is named, and left.
#   Nothing of it is touched while a command holds the run lock: the
#   mounts are that command's.
# - The persistent timers' stamps go, on remove and on purge: with
#   them, and the credential file kept, a reinstall reads a run and a
#   drill as missed and catches up inside the install [M68]. Without
#   them a timer's first activation writes its stamp and fires nothing
#   [M61].
# - The hotserve-backup account stays (Debian policy: system accounts
#   are never removed).
set -e
timers="hotserve-backup.timer hotserve-backup-drill.timer"
run=/run/hotserve-backup

# lock_held: a command run from a shell holds the run lock. preremove
# stopped the services, so whoever holds it now is no unit's.
lock_held() {
	[ -f "$run/lock" ] && command -v flock >/dev/null 2>&1 && ! flock -n "$run/lock" true 2>/dev/null
}

# unmount_left: what mountinfo lists under the run directory,
# unmounted deepest first. Detached, as the run's own sweep does it: a
# mount that is busy goes once it no longer is, and its mount point is
# free at once. A path mountinfo escapes (a space, a backslash) is none
# a run makes — its mount points are mount-<n> under a hex name — and
# is named rather than guessed at.
unmount_left() {
	[ -r /proc/self/mountinfo ] || return 0
	left=$(awk -v dir="$run/" 'index($5, dir) == 1 { print $5 }' /proc/self/mountinfo | sort -r)
	for m in $left; do
		case "$m" in
		*\\*) echo "hotserve: $m is still mounted: a mount point this package does not make; unmount it yourself (sudo umount), and do not rm -r $run before" >&2 ;;
		*) umount -l "$m" 2>/dev/null || true ;;
		esac
	done
	for m in $(awk -v dir="$run/" 'index($5, dir) == 1 { print $5 }' /proc/self/mountinfo | sort -r); do
		case "$m" in
		*\\*) ;;
		*) echo "hotserve: $m would not unmount: an app's data is mounted there; unmount it yourself (sudo umount $m), and do not rm -r $run before" >&2 ;;
		esac
	done
}

# remove_run: the run directory's own files and directories, one by
# one and by rmdir. A mount point still mounted refuses both, and
# whatever stays is named.
remove_run() {
	[ -d "$run" ] || return 0
	for d in "$run"/*/; do
		[ -d "$d" ] || continue
		for f in "$d"* "$d".[!.]*; do
			[ -e "$f" ] || [ -L "$f" ] || continue
			if [ -d "$f" ] && [ ! -L "$f" ]; then
				rmdir "$f" 2>/dev/null || true
			else
				rm -f "$f" 2>/dev/null || true
			fi
		done
		rmdir "$d" 2>/dev/null || true
	done
	rm -f "$run/lock" "$run/units" "$run/init-unit"
	rmdir "$run" 2>/dev/null || echo "hotserve: kept $run: not empty — $(ls -A "$run" | tr '\n' ' ')is still there; it is gone at the next boot" >&2
}

case "${1:-}" in
remove | purge)
	for u in $timers; do
		rm -f "/var/lib/systemd/timers/stamp-$u"
	done
	if lock_held; then
		echo "hotserve: a backup command is under way from a shell ($(head -1 "$run/lock" 2>/dev/null)), and is left to run: $run and what is mounted under it are left to it" >&2
	else
		unmount_left
		[ "$1" != purge ] || remove_run
	fi
	;;
esac
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
		# temp files their writers leave when killed mid-write: the
		# record's, and setup's of the id.
		rm -f "$state/status.json" "$state"/status.json.aside-* "$state"/.status-* "$state/repository-id" "$state/repository-id.new" "$state/listing.err"
		for d in "$state/staging" "$state/restore"; do
			[ -d "$d" ] || continue
			# Empty it of empty per-app directories, then itself; whatever
			# holds a file stays.
			for a in "$d"/*; do [ -d "$a" ] && rmdir "$a" 2>/dev/null || true; done
			rmdir "$d" 2>/dev/null \
				|| echo "hotserve: kept $d: not empty — copies of an app's data a backup run or a restore left there; look, then remove them yourself" >&2
		done
		# What is kept is said for what it is: the state directory stays
		# for staging/ and restore/, said above, or for a file this
		# package did not make — which is no copy of an app's data, and
		# is not called one.
		if ! rmdir "$state" 2>/dev/null; then
			others=$(ls -A "$state" | grep -v -x -e staging -e restore | tr '\n' ' ' | sed 's/ $//')
			[ -z "$others" ] || echo "hotserve: kept $state: it holds what this package did not make ($others)" >&2
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
