#!/bin/sh
set -e
# nss <getent or id arguments>: one lookup in the account databases,
# bounded — a user directory that does not answer would otherwise hold
# dpkg's configure for as long as it likes (Copilot on #155). Exit 2 is
# "not found"; 124 is "did not answer", which is never taken for "not
# found": that would make a local account beside the directory's.
nss() {
	if command -v timeout >/dev/null 2>&1; then
		# And killed ten seconds after: a lookup that ignores the
		# SIGTERM must not hold the configure either (Copilot on #155).
		timeout -k 10 30 "$@"
	else
		"$@"
	fi
}
# absent <getent arguments>: the entry is not there, as the databases
# answered; 0 only for that.
absent() {
	st=0
	nss getent "$@" >/dev/null 2>&1 || st=$?
	[ "$st" = 2 ]
}
if command -v useradd >/dev/null 2>&1; then
	if absent group hotserve; then groupadd --system hotserve; fi
	if absent passwd hotserve; then useradd --system \
		--gid hotserve --home-dir /var/lib/hotserve \
		--shell /usr/sbin/nologin --comment "hotserve server" hotserve; fi
else
	# Alpine (busybox) fallback.
	addgroup -S hotserve 2>/dev/null || true
	adduser -S -G hotserve -h /var/lib/hotserve -s /sbin/nologin hotserve 2>/dev/null || true
fi
if ! uid=$(nss id -u hotserve 2>/dev/null); then
	echo "hotserve: the hotserve system user does not exist and could not be created" >&2
	exit 1
fi
# Best-effort: put the account IN the hotserve group. Both branches above
# set the group only when they create the account, so an account that
# already existed — an admin's, or one from another package — keeps
# whatever groups it had, and the state directories chowned below are
# group-hotserve. Not fatal: nothing the package installs is reachable
# only through the group.
in_group() { nss id -nG hotserve 2>/dev/null | tr ' ' '\n' | grep -qx hotserve; }
if ! in_group; then
	if command -v usermod >/dev/null 2>&1; then
		usermod -aG hotserve hotserve || true
	else
		addgroup hotserve hotserve 2>/dev/null || true # busybox
	fi
fi
if ! in_group; then
	echo "hotserve: the hotserve user is not a member of the hotserve group; add it with \`usermod -aG hotserve hotserve\` if its state directories become unreadable" >&2
fi
chown hotserve:hotserve /var/lib/hotserve /var/lib/liveswap
# The account hotserve-backup's restic units run as: the one line
# `hotserve-backup setup` uses (backups/engine/setup.go, held to this
# script by a test), which also makes it when it is missing. No home,
# no shell, nothing of its own but the cache the manager makes for it.
# Its comment, made-by-hotserve, marks it as hotserve's. An account of
# that name that exists is left as it is — an administrator may have
# meant it — and setup and every run use it only where it is
# hotserve's: in /etc/passwd, with the mark. Whoever is the account
# reads the repository credential from a running restic's
# environment. Never removed on purge (Debian policy: system accounts
# stay).
#
# Where it cannot be made — a group of its name is there without it,
# and useradd exits 9 [M66] — that is said, and the configure goes on
# (the owner, 2026-09-27): hotserve is installed on a box that may use
# no backups, and setup, which makes the account when it is missing,
# is where that is refused.
backup_st=0
nss getent passwd hotserve-backup >/dev/null 2>&1 || backup_st=$?
if [ "$backup_st" = 2 ]; then
	if command -v useradd >/dev/null 2>&1; then
		why=$(useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin --comment made-by-hotserve hotserve-backup 2>&1) || true
	else
		why=$(adduser -S -H -h /nonexistent -s /sbin/nologin -g made-by-hotserve hotserve-backup 2>&1) || true # busybox
	fi
fi
if [ "$backup_st" != 0 ] && [ "$backup_st" != 2 ]; then
	case "$backup_st" in
	124 | 137) why="the user directory did not answer within 30s" ;; # 137: killed after ignoring the SIGTERM
	*) why="getent exited $backup_st" ;;
	esac
	echo "hotserve: whether the hotserve-backup account exists could not be told ($why); no account is made beside the directory's, and backups wait for it: hotserve-backup account says when it is right" >&2
elif nss getent passwd hotserve-backup >/dev/null 2>&1; then
	# The rule is setup's, and in setup's words: the binary this package
	# has just installed says whether the account is one setup and every
	# run accept. A warning, never the install's failure.
	if [ -x /usr/bin/hotserve-backup ] && ! said=$(/usr/bin/hotserve-backup account 2>&1); then
		echo "hotserve: ${said#hotserve-backup: }" >&2
		echo "hotserve: hotserve-backup setup, and every run, restore and drill, refuse that account until it is put right; hotserve-backup account says when it is" >&2
	fi
else
	echo "hotserve: the hotserve-backup account could not be made (${why:-no reason was given}); backups wait for it, and nothing else of hotserve does: hotserve-backup setup <repository> makes it, or says why it cannot" >&2
fi
# The box applier's exchange tree, /var/lib/hotserve-box: made now that
# the hotserve group exists, and again at every boot, each time with
# the modes and owners box/DESIGN-box.md's Paths table gives, put back
# where they drifted. Named, as debhelper does, so no other package's
# tmpfiles.d entries run from here.
if command -v systemd-tmpfiles >/dev/null 2>&1; then
	systemd-tmpfiles --create hotserve-box.conf \
		|| echo "hotserve: systemd-tmpfiles --create hotserve-box.conf failed; the box applier has no /var/lib/hotserve-box until it succeeds" >&2
fi
# The starter config (nfpm.yaml says why it is not a conffile). Install
# it where nothing stands at /etc/hotserve/Caddyfile, and replace it
# only while it is byte-identical to the starter this script last wrote
# there, which the record says. Leave anything else alone, silently: an
# edit, a file `hotserve init` or a push wrote, a symlink (never
# followed), anything not a regular file, a file with no record. Each
# write lands whole, by rename, before the try-restart below loads it. A
# failure never fails the install; the file stays as it was.
starter=/usr/share/hotserve/Caddyfile
config=/etc/hotserve/Caddyfile
record=/var/lib/hotserve-package/starter.sha256
sha256() { sha256sum <"$1" | cut -d' ' -f1; }
# Records $want as the starter on disk. /etc/hotserve is synced first,
# so the record never names a rename a power cut could still undo,
# which would make a stock file look edited for good; then the record's
# own rename, and its directory.
starter_record() {
	record_tmp="$record.$$"
	if sync /etc/hotserve && printf '%s\n' "$want" >"$record_tmp" \
		&& chmod 0644 "$record_tmp" && sync "$record_tmp" \
		&& mv -fT "$record_tmp" "$record" && sync "${record%/*}"; then
		return 0
	fi
	rm -f "$record_tmp" || true
	echo "hotserve: could not write $record durably; until it is, upgrades may leave $config as it is" >&2
}
starter_put() {
	config_tmp="/etc/hotserve/.Caddyfile.package-$$"
	# -T: a directory standing at that name is refused, never written into.
	if cp -T "$starter" "$config_tmp" && chown root:root "$config_tmp" \
		&& chmod 0644 "$config_tmp" && sync "$config_tmp" \
		&& mv -fT "$config_tmp" "$config"; then
		starter_record
		return 0
	fi
	rm -f "$config_tmp" || true
	echo "hotserve: could not install the starter config at $config; it is at $starter" >&2
}
# A crash's temporaries: only this script makes them. Never fatal: a
# directory by one of these names is not this script's to remove.
rm -f /etc/hotserve/.Caddyfile.package-* "$record".* 2>/dev/null || true
if want=$(sha256 "$starter") && [ ${#want} -eq 64 ]; then
	if [ ! -e "$config" ] && [ ! -L "$config" ]; then
		starter_put
	elif [ -f "$config" ] && [ ! -L "$config" ]; then
		have=$(sha256 "$config") || have=
		recorded=
		if [ -f "$record" ] && [ ! -L "$record" ]; then
			recorded=$(head -n 1 "$record") || recorded=
		fi
		if [ "$have" = "$want" ]; then
			# Already the starter: a crash between the rename and the
			# record, or a box that has never had a record. Without
			# this it would look edited for good.
			[ "$recorded" = "$want" ] || starter_record
		elif [ -n "$have" ] && [ "$have" = "$recorded" ]; then
			starter_put
		fi
	fi
else
	echo "hotserve: could not read $starter; $config is left as it is" >&2
fi
# The box's key for secrets (box/DESIGN-box.md, "The box's key"), made
# once and never replaced, so that a box has one with no step. Only on
# a running system: an image built without systemd as PID 1 would
# otherwise bake one key into every box made from it. A failure never
# fails the install; init, or the console, makes it later.
if [ -d /run/systemd/system ]; then
	/usr/bin/hotserve box key >/dev/null 2>&1 \
		|| echo "hotserve: could not make the box's key for secrets; run hotserve box key as root" >&2
fi
if command -v systemctl >/dev/null 2>&1; then
	# liveswap runs deployed apps as transient units under the hotserve
	# user's own systemd manager (user@$uid.service), so they outlive
	# hotserve restarts and upgrades. Lingering keeps that manager
	# running with nobody logged in; the drop-in starts it alongside
	# hotserve and orders hotserve after it. No polkit and no root path:
	# hotserve talks to its own manager over /run/user/$uid.
	mkdir -p /etc/systemd/system/hotserve.service.d
	cat > /etc/systemd/system/hotserve.service.d/10-user-manager.conf <<EOF
# Generated by the hotserve package (postinstall); do not edit.
# liveswap's app units run under the hotserve user's manager.
[Unit]
Wants=user@$uid.service
After=user@$uid.service
EOF
	# The user manager's own limits are what its units inherit; match
	# hotserve.service so apps keep the file-descriptor headroom they
	# had as its children. (Effective from systemd 256; the package
	# requires 257 — see nfpm.yaml.)
	mkdir -p "/etc/systemd/system/user@$uid.service.d"
	cat > "/etc/systemd/system/user@$uid.service.d/10-hotserve.conf" <<EOF
# Generated by the hotserve package (postinstall); do not edit.
[Service]
LimitNOFILE=1048576
EOF
	systemctl daemon-reload || true
	# Lingering starts user@$uid.service at once, so it comes after the
	# drop-ins above: the manager must be born with those limits. (An
	# already-running manager keeps them until it next starts —
	# restarting it would stop every app, which an upgrade must not do;
	# until then the sandbox probe reports what the running manager can
	# do.)
	loginctl enable-linger hotserve 2>/dev/null || {
		mkdir -p /var/lib/systemd/linger && touch /var/lib/systemd/linger/hotserve
	}
	# An upgrade restarts a running hotserve onto the new binary;
	# try-restart leaves a stopped one stopped. Only where systemd is
	# PID 1: in a chroot or an image build systemctl fails for reasons
	# that say nothing about hotserve, and there the start hint below is
	# the whole story.
	restart=skipped
	if [ -d /run/systemd/system ]; then
		if systemctl try-restart hotserve 2>/dev/null; then
			restart=ok
		else
			restart=failed
		fi
	fi
	if [ "$restart" = failed ]; then
		# The new binary refused to start — most often a Caddyfile it
		# rejects — and the old one is already stopped, so the site is
		# down. Say so, on stderr. The script still exits 0: a non-zero
		# postinst leaves the package half-configured and fails this
		# apt run, and every later one that retries the configure,
		# until it succeeds. The package is installed correctly; what
		# needs fixing is the config, which dpkg cannot do.
		state=$(systemctl is-active hotserve 2>/dev/null) || true
		echo "hotserve: restarting onto the new version FAILED; hotserve.service is now ${state:-in an unknown state}." >&2
		if [ "$state" != active ]; then
			echo "  Nothing is served until it starts (deployed apps keep running, unreachable)." >&2
		fi
		echo "  The likely cause is something in /etc/hotserve/Caddyfile this version refuses. See why with:" >&2
		echo "    journalctl -u hotserve -e" >&2
		echo "    systemctl status hotserve" >&2
		echo "  then fix it and run: sudo systemctl restart hotserve" >&2
	elif [ "$restart" = ok ] && systemctl is-enabled --quiet hotserve 2>/dev/null; then
		# Already set up: no start hint. If is-enabled itself fails,
		# the hint below prints, which is harmless on an enabled unit.
		if systemctl is-active --quiet hotserve 2>/dev/null; then
			echo "hotserve restarted."
		else
			# try-restart does not start a stopped or failed unit — an
			# upgrade that fixes an earlier failed start included.
			echo "hotserve is enabled but not running. Start it with:"
			echo "  sudo systemctl start hotserve"
		fi
	else
		echo "hotserve installed. Start it with:"
		echo "  sudo systemctl enable --now hotserve"
	fi
fi
# The box applier's path unit, the way dh_installsystemd would have it:
# enabled on first installation; an administrator's `disable` kept
# across upgrades (was-enabled is false then, and update-state only
# tidies the symlinks); started, or restarted on an upgrade ($2 is the
# version upgraded from), by deb-systemd-invoke, which starts nothing
# disabled or masked. The service it starts has no [Install]: the path
# unit is its only start (box/DESIGN-box.md, "The applier unit").
if [ -x /usr/bin/deb-systemd-helper ]; then
	deb-systemd-helper unmask hotserve-box-apply.path >/dev/null || true
	if deb-systemd-helper --quiet was-enabled hotserve-box-apply.path; then
		deb-systemd-helper enable hotserve-box-apply.path >/dev/null || true
	else
		deb-systemd-helper update-state hotserve-box-apply.path >/dev/null || true
	fi
fi
# The daemon-reload above has loaded the unpacked units.
if [ -d /run/systemd/system ]; then
	if [ -n "${2:-}" ]; then action=restart; else action=start; fi
	if [ -x /usr/bin/deb-systemd-invoke ]; then
		deb-systemd-invoke "$action" hotserve-box-apply.path >/dev/null || true
	else
		# No helper (a systemd host that is not Debian): enabled here,
		# and started, with no memory of an administrator's disable.
		systemctl enable hotserve-box-apply.path 2>/dev/null || true
		systemctl "$action" hotserve-box-apply.path 2>/dev/null || true
	fi
fi
# The backup timers, the way dh_installsystemd would have it: enabled on
# first installation; an administrator's `disable` kept across upgrades
# (was-enabled is false then, and update-state only tidies the
# symlinks); started, or restarted on an upgrade ($2 is the version
# upgraded from), by deb-systemd-invoke, which starts nothing that is
# disabled or masked. The services they start do nothing until
# `hotserve-backup setup` has written the credential file
# (ConditionPathExists= in the unit files): an install runs no backup.
if [ -x /usr/bin/deb-systemd-helper ]; then
	for u in hotserve-backup.timer hotserve-backup-drill.timer; do
		deb-systemd-helper unmask "$u" >/dev/null || true
		if deb-systemd-helper --quiet was-enabled "$u"; then
			deb-systemd-helper enable "$u" >/dev/null || true
		else
			deb-systemd-helper update-state "$u" >/dev/null || true
		fi
	done
fi
# Started on an install, restarted on an upgrade ($2 is the version
# upgraded from), exactly as dh_installsystemd does it (the owner,
# 2026-09-27): deb-systemd-invoke starts nothing disabled or masked,
# and obeys a policy-rc.d. So an upgrade starts again a timer an
# administrator stopped and left enabled — disable it to keep it off.
# Nothing of the backups was stopped for an upgrade: a run under way
# is left to finish. The daemon-reload above has loaded their units.
if [ -d /run/systemd/system ]; then
	if [ -n "${2:-}" ]; then action=restart; else action=start; fi
	if [ -x /usr/bin/deb-systemd-invoke ]; then
		deb-systemd-invoke "$action" hotserve-backup.timer hotserve-backup-drill.timer >/dev/null || true
	else
		# No helper (a systemd host that is not Debian): enabled here,
		# and started, with no memory of an administrator's disable.
		systemctl enable hotserve-backup.timer hotserve-backup-drill.timer 2>/dev/null || true
		systemctl "$action" hotserve-backup.timer hotserve-backup-drill.timer 2>/dev/null || true
	fi
	if systemctl is-enabled --quiet hotserve-backup.timer 2>/dev/null; then
		echo "Backups: the hourly timer and the Sunday restore drill are enabled, and run nothing until:"
		echo "  hotserve-backup setup <repository>"
	else
		echo "Backups: the hourly timer and the Sunday restore drill are disabled, as they were left; systemctl enable --now hotserve-backup.timer hotserve-backup-drill.timer turns them on"
	fi
fi
# Explicit, so no message above can decide the package's state.
exit 0
