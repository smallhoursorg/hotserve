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
# - /run/hotserve-backup: a command killed mid-way leaves units
#   running — from a shell it has no service to bind them to — and an
#   app's data bind-mounted under its run directory. After a remove or
#   a purge there is no next run to sweep either, so they are swept
#   here as a run sweeps them: the units stopped by the exact names
#   the command recorded, never by a pattern; the mounts made private
#   and taken away. Private first: on a box systemd has booted the
#   root mount is shared, a recursive bind of it is its peer, and
#   unmounting the disk beneath the bind unmounts the disk beneath the
#   app's own directory [M71]. Never rm -r: through a mount that
#   removes the app's files [measured in #153]. On purge the run
#   directories then go by rmdir, and their files one by one — and not
#   at all while anything is still mounted beneath, which is named.
#   None of it is touched while anything holds the run lock: the units
#   and the mounts are that command's.
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

# mounted_beneath: the mount points under the run directory, as
# mountinfo writes them — a space as \040 — shallowest first.
mounted_beneath() {
	[ -r /proc/self/mountinfo ] || return 0
	awk -v dir="$run/" 'index($5, dir) == 1 { print $5 }' /proc/self/mountinfo | sort
}

# stop_left: the units a killed command left running, by the exact
# names it recorded as it started each, as the next run would have
# stopped them. A line that is not a name the engine writes is not
# passed to the manager. init-unit is not among them, and is left to
# end: it is a restic init, which stopped half way leaves a repository
# no password opens.
stop_left() {
	[ -f "$run/units" ] && [ -d /run/systemd/system ] || return 0
	while read -r u; do
		# The engine's own grammar (engine.go, unitNameRe): a role, an
		# app where there is one, a run's twelve hex digits.
		echo "$u" | grep -Eq '^hotserve_backup_[a-z]+[0-9]*(_[a-z0-9-]{1,63})?_[0-9a-f]{12}\.service$' || continue
		systemctl stop "$u" 2>/dev/null || true
		state=$(systemctl show -p ActiveState --value "$u" 2>/dev/null) || state=""
		case "$state" in
		inactive | failed | "") systemctl reset-failed "$u" 2>/dev/null || true ;;
		*) echo "hotserve: $u, which a backup command left running, would not stop (it is $state): it holds the repository credential; stop it yourself (sudo systemctl stop $u)" >&2 ;;
		esac
	done <"$run/units"
}

# unmount_left: what is mounted under the run directory, taken away.
# Each of the engine's own mount points — mount-<n>, and seen, under a
# run's twelve hex digits, and nothing else: a mount of another name
# there is none the engine made — is made private with all beneath it,
# and then detached with all beneath it, as the run's own sweep does:
# a disk that came along with the bind, whatever its name, goes with
# it and takes nothing of the app's. One that cannot be made private
# is not detached as it is, which would take the app's disk with it:
# it is unmounted plainly, which is refused while anything is mounted
# beneath it, or left. What is still mounted after that is named,
# once.
unmount_left() {
	for m in $(mounted_beneath | grep -E "^$run/[0-9a-f]{12}/(mount-[0-9]+|seen)\$" || true); do
		if mount --make-rprivate "$m" 2>/dev/null; then
			umount -l "$m" 2>/dev/null || true
		else
			umount "$m" 2>/dev/null || true
		fi
	done
	# printf, not echo: a shell's echo may read mountinfo's escapes, and
	# what is printed is what mountinfo says, whatever the shell.
	left=$(mounted_beneath | tr '\n' ' ')
	[ -z "$left" ] || printf '%s\n' "hotserve: still mounted under $run: $left— unmount each yourself (sudo umount), and do not rm -r $run before: that removes an app's files through the mount" >&2
}

# remove_run: the run directory's own files and directories, one by
# one and by rmdir — and nothing at all while anything is mounted
# beneath it, which unmount_left has named, or where what is mounted
# cannot be read: an empty answer is then no answer (Copilot on #155:
# with no /proc, a bind on a run directory lost the app's files).
remove_run() {
	[ -d "$run" ] || return 0
	if [ ! -r /proc/self/mountinfo ]; then
		echo "hotserve: what is mounted under $run could not be read (/proc/self/mountinfo): nothing under it is removed; it is gone at the next boot, and is not to be rm -r'd before" >&2
		return 0
	fi
	[ -z "$(mounted_beneath)" ] || return 0
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

# sweep: what a killed command left, swept with the run lock held from
# the first of it to the last, so that nothing is another command's by
# the time it is touched. Exit 3: the lock is somebody's.
sweep() {
	if command -v flock >/dev/null 2>&1; then
		flock -n 9 || return 3
	fi
	stop_left
	unmount_left
	[ "$1" != purge ] || remove_run
}

case "${1:-}" in
remove | purge)
	for u in $timers; do
		rm -f "/var/lib/systemd/timers/stamp-$u"
	done
	if [ -d "$run" ]; then
		swept=0
		(sweep "$1") 9>>"$run/lock" || swept=$?
		if [ "$swept" = 3 ]; then
			echo "hotserve: the run lock is held ($(head -1 "$run/lock" 2>/dev/null)): a backup is still under way, and is left to run; $run, its units and what is mounted under it are left to it" >&2
		fi
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
