#!/bin/sh
# hotserve package post-remove. dpkg runs it once the package's files
# are gone: `postrm remove`, then `postrm purge` when the removal is a
# purge, and on upgrades (`upgrade`, `failed-upgrade`) and their aborts
# (`abort-install`, `abort-upgrade`, `disappear`), which do nothing
# here: postinstall's `configure` reloads the manager.
#
# remove, and the purge that follows one: the manager drops the removed
# unit files (hotserve.service, hotserve-box-apply.path and .service),
# and the path unit is masked, as dh_installsystemd has it, so that
# nothing of it comes back until an install unmasks and enables it
# (postinstall.sh).
#
# purge, on top of that — what the package made and nothing else holds:
#
# - the path unit's enable state (deb-systemd-helper purge), then the
#   mask, in debhelper's order;
# - /etc/hotserve/Caddyfile, when it is a regular file (a symlink is
#   never followed, and is kept, named), and postinstall's temporaries
#   beside it; /etc/hotserve itself only once it is empty. The box's
#   key for secrets (age/, age.pub) and the env files decrypted with
#   it (secrets/) are KEPT, and said: the key is the one way to read
#   the secrets the box's repository holds, and only the person purging
#   knows whether that is over;
# - /var/lib/hotserve-package/, the starter's record: root's alone,
#   0700, nothing of an app's in it;
# - /var/lib/hotserve-box/, the box applier's exchange tree (stage/,
#   in/, work/, out/, the transaction record, applied.json): the
#   package's own, and the Caddyfile it protected is gone by then. Not
#   where anything is mounted at or under it: an rm there would remove
#   through the mount, so the tree is kept whole, and named.
#
# Never: /var/lib/hotserve (Caddy's state: TLS certificates) and
# /var/lib/liveswap (the deployed apps' releases and data) — root does
# not delete what an app or an operator put there; dpkg removes either
# only when it is empty, as any directory a package shipped. The
# hotserve account stays: system accounts are never removed.
#
# Every step is best-effort: a postrm that exits non-zero leaves dpkg
# with a package it can neither configure nor remove, so each failure
# is said on stderr, naming the path, and the script exits 0.
set -e
unit=hotserve-box-apply.path
config=/etc/hotserve/Caddyfile
record_dir=/var/lib/hotserve-package
box=/var/lib/hotserve-box

# mounted <dir>: something is mounted at or under it, or what is
# mounted cannot be read (then nothing is removed either).
mounted() {
	[ -r /proc/self/mountinfo ] || return 0
	awk -v d="$1" '$5 == d || index($5, d "/") == 1 { found = 1 } END { exit !found }' /proc/self/mountinfo
}

# The removed unit files out of the manager, where systemd is PID 1;
# in a chroot or an image build there is no manager to reload.
case "${1:-}" in
remove | purge)
	if [ -d /run/systemd/system ]; then
		systemctl --system daemon-reload >/dev/null 2>&1 || true
	fi
	;;
esac

case "${1:-}" in
remove)
	# The mask lands only once the unit file is gone, which it is
	# here; postinstall's unmask takes it off again.
	if [ -x /usr/bin/deb-systemd-helper ]; then
		deb-systemd-helper mask "$unit" >/dev/null || true
	fi
	;;
purge)
	if [ -x /usr/bin/deb-systemd-helper ]; then
		deb-systemd-helper purge "$unit" >/dev/null || true
		deb-systemd-helper unmask "$unit" >/dev/null || true
	fi
	# The box's config: a regular file only. -f follows a symlink, so
	# the link is ruled out first; whatever else stands there is kept.
	if [ -L "$config" ]; then
		echo "hotserve: kept $config: a symlink, which this script never follows; remove it yourself" >&2
	elif [ -f "$config" ]; then
		rm -f "$config" || echo "hotserve: could not remove $config; remove it yourself" >&2
	elif [ -e "$config" ]; then
		echo "hotserve: kept $config: not a regular file; remove it yourself" >&2
	fi
	# postinstall's temporaries, from a crash mid-install. Never a
	# directory by one of these names: not this script's to remove.
	rm -f /etc/hotserve/.Caddyfile.package-* 2>/dev/null || true
	if [ -d /etc/hotserve ] && ! rmdir /etc/hotserve 2>/dev/null; then
		echo "hotserve: kept /etc/hotserve: it still holds $(ls -A /etc/hotserve | tr '\n' ' ' | sed 's/ $//')" >&2
		if [ -e /etc/hotserve/age/identity ]; then
			echo "  age/identity is the box's key for secrets, the one way to read the secrets its repository holds; once nothing needs them: sudo rm -r /etc/hotserve" >&2
		fi
	fi
	# The starter's record.
	rm -rf "$record_dir" || echo "hotserve: could not remove $record_dir; remove it yourself" >&2
	# The box applier's exchange tree, whole — unless something is
	# mounted at or under it. --one-file-system is for a mount made
	# between that check and the rm: it is skipped, not removed through.
	if [ -L "$box" ]; then
		echo "hotserve: kept $box: a symlink, which this script never follows; remove it yourself" >&2
	elif [ -d "$box" ]; then
		if mounted "$box"; then
			echo "hotserve: kept $box: something is mounted at or under it, or what is mounted cannot be read; nothing under it is removed — unmount it, then remove the directory yourself" >&2
		else
			rm -rf --one-file-system "$box" \
				|| echo "hotserve: could not remove all of $box; what is left is in it, named above" >&2
		fi
	fi
	;;
*)
	# upgrade, failed-upgrade, abort-install, abort-upgrade, disappear.
	;;
esac
# Explicit, so no message above can decide the package's state.
exit 0
