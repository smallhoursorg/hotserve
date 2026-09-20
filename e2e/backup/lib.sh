#!/bin/sh
# What the backup suites share (sourced, not run): the box's paths, the
# suite's own look into the repository, and what a run must never leave
# behind. Callers source /lib.sh first, for pass and fail.

ENVFILE=/etc/hotserve/backup.env
STATUS=/var/lib/hotserve-backup/status.json
CADDYFILE=/etc/hotserve/Caddyfile
PASSWORD=e2e-repository-password
OUT=/root/run.out

wait_for_systemd() {
	i=0
	while :; do
		state=$(systemctl is-system-running 2>/dev/null || true)
		case "$state" in running | degraded) break ;; esac
		i=$((i + 1))
		[ "$i" -ge 60 ] && { echo "FATAL: systemd did not come up (state: ${state:-unknown})"; exit 1; }
		sleep 1
	done
}

write_env() { # <repository> <password>
	printf 'RESTIC_REPOSITORY=%s\nRESTIC_PASSWORD=%s\nAWS_ACCESS_KEY_ID=AKIDE2EFIXTURE\nAWS_SECRET_ACCESS_KEY=e2e-fixture-key-not-a-secret\n' "$1" "$2" >"$ENVFILE"
	chmod 0600 "$ENVFILE"
}
# The suite's own look into the repository: its own cache, so that it
# leaves nothing of root's where a unit must write.
rr() { (
	set -a
	. "$ENVFILE"
	RESTIC_CACHE_DIR=/root/suite-cache
	set +a
	restic "$@"
); }
as_app() { setpriv --reuid hotserve --regid hotserve --clear-groups "$@"; }
run() { hotserve-backup run >"$OUT" 2>&1; }
# class <app>: the app's class in the status record.
class() { tr -d '\n' <"$STATUS" | sed 's/  */ /g' | sed -n "s/.*\"$1\": { \"class\": \"\([^\"]*\)\".*/\1/p"; }
snapshots() { rr snapshots --no-lock --json 2>/dev/null | grep -o '"short_id"' | wc -l; }
units_left() { systemctl list-units --all --plain --no-legend 'hotserve_backup_*' | wc -l; }
units_running() { systemctl list-units --plain --no-legend --state=active,activating,deactivating 'hotserve_backup_*' | wc -l; }
staged() { find /var/lib/hotserve-backup/staging -mindepth 2 2>/dev/null | wc -l; }
expect_class() { # <app> <class> <what>
	got=$(class "$1")
	[ "$got" = "$2" ] && pass "$3: $1 is '$2'" || fail "$3: $1 is '$got', want '$2' ($(grep "^$1:" "$OUT"))"
}
nothing_left() { # <what>
	[ "$(staged)" = 0 ] && pass "$1: no plaintext copy is left in staging" || fail "$1: left in staging: $(find /var/lib/hotserve-backup/staging -mindepth 2)"
	[ "$(units_left)" = 0 ] && pass "$1: no unit is left" || fail "$1: units left: $(systemctl list-units --all --no-legend 'hotserve_backup_*')"
}

seed() {
	rm -rf /var/lib/liveswap/blog /var/lib/liveswap/shop
	install -d -o hotserve -g hotserve -m 0750 /var/lib/liveswap/blog /var/lib/liveswap/blog/shared /var/lib/liveswap/shop /var/lib/liveswap/shop/shared
	as_app sh -c 'umask 077; cd /var/lib/liveswap/blog/shared && sqlite3 app.db "pragma journal_mode=wal; create table posts(n); insert into posts values (1),(2);" >/dev/null && mkdir uploads && echo img >uploads/a.png && echo undeclared >private.txt'
	as_app sh -c 'umask 077; cd /var/lib/liveswap/shop/shared && mkdir data && sqlite3 data/shop.db "pragma journal_mode=wal; create table orders(n); insert into orders values (1),(2),(3);" >/dev/null && echo receipt >r.txt'
}
