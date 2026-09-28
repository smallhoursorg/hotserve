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
# - /run/hotserve-backup: what a killed command left there — its units,
#   its mounts of an app's data — preremove had the engine sweep at the
#   remove, while the program was there. On purge the run directory's
#   own files go (the lock, the list of units), and its directories by
#   rmdir alone: nothing is removed where anything is mounted at or
#   under it, or where what is mounted cannot be read — an rm there
#   would remove an app's files through the mount [measured in #153].
# - The persistent timers' stamps go, on remove and on purge: with
#   them, and the credential file kept, a reinstall reads a run and a
#   drill as missed and catches up inside the install [M68].
# - The hotserve-backup account stays (Debian policy: system accounts
#   are never removed).
set -e
timers="hotserve-backup.timer hotserve-backup-drill.timer"
run=/run/hotserve-backup

# mounted: something is mounted at or under the run directory, or what
# is mounted cannot be read.
mounted() {
	[ -r /proc/self/mountinfo ] || return 0
	awk -v r="$run" '$5 == r || index($5, r "/") == 1 { found = 1 } END { exit !found }' /proc/self/mountinfo
}

case "${1:-}" in
remove | purge)
	for u in $timers; do
		rm -f "/var/lib/systemd/timers/stamp-$u"
	done
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
	if [ -d "$run" ]; then
		# A command from a shell killed after the remove's sweep — which
		# found it holding the lock, and left it — leaves units no service
		# ends, an upload among them with the credential, and there is no
		# program left to sweep. Stopped here by the names it recorded,
		# and only names the engine writes (engine.go, unitNameRe; a test
		# holds the two together), where nothing holds the lock now
		# (Copilot on #155).
		# One that will not stop is said, by name, and the record of it
		# is kept (Copilot on #155).
		stuck=""
		if [ -f "$run/units" ] && [ -d /run/systemd/system ] && { ! command -v flock >/dev/null 2>&1 || flock -n "$run/lock" true 2>/dev/null; }; then
			while read -r u; do
				echo "$u" | grep -Eq '^hotserve_backup_[a-z]+[0-9]*(_[a-z0-9-]{1,63})?_[0-9a-f]{12}\.service$' || continue
				systemctl stop "$u" 2>/dev/null || true
				# A unit the manager has none of reads "inactive"
				# [measured]; nothing at all is a manager that did not
				# answer, which is no "gone" (Copilot on #155).
				state=$(systemctl show -p ActiveState --value "$u" 2>/dev/null) || state=""
				case "$state" in
				inactive | failed) systemctl reset-failed "$u" 2>/dev/null || true ;;
				"")
					stuck=yes
					echo "hotserve: $u, which a backup command left running, could not be asked about: the manager did not answer; if it runs, it holds the repository credential (sudo systemctl stop $u)" >&2
					;;
				*)
					stuck=yes
					echo "hotserve: $u, which a backup command left running, would not stop (it is $state): it holds the repository credential; stop it yourself (sudo systemctl stop $u)" >&2
					;;
				esac
			done <"$run/units"
		fi
		if [ -n "$stuck" ]; then
			echo "hotserve: kept $run: a unit named in $run/units would not stop, or could not be asked about, and that list is its only record" >&2
		elif mounted; then
			echo "hotserve: kept $run: something is mounted at or under it, or what is mounted cannot be read; nothing under it is removed — it is gone at the next boot, and is not to be rm -r'd before" >&2
		elif [ -f "$run/lock" ] && command -v flock >/dev/null 2>&1 && ! flock -n "$run/lock" true 2>/dev/null; then
			# A command that holds the run lock — a restore at its prompt
			# — keeps what is its: the lock, the list of the units it has
			# started, an init marker. Removed from under it, no sweep
			# would ever stop those units.
			echo "hotserve: kept $run: the run lock is held ($(head -1 "$run/lock" 2>/dev/null)); a backup command is under way, and what it keeps there is its own — it is gone at the next boot" >&2
		else
			rm -f "$run/lock" "$run/units" "$run/init-unit"
			for d in "$run"/*/; do [ -d "$d" ] && rmdir "$d" 2>/dev/null || true; done
			rmdir "$run" 2>/dev/null || echo "hotserve: kept $run: not empty ($(ls -A "$run" | tr '\n' ' ' | sed 's/ $//')); it is gone at the next boot" >&2
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
