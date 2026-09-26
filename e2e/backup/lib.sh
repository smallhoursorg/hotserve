#!/bin/sh
# What the backup suites share (sourced, not run): the box's paths, the
# suite's own look into the repository, and what a run must never leave
# behind. Callers source /lib.sh first, for pass and fail.

ENVFILE=/etc/hotserve-backup/repository.env
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

# write_env <repository> <password>: the credential file by hand, as
# setup would write it — the suites that are not about setup start
# from one.
write_env() {
	# The account restic runs as, which setup makes: a suite that
	# writes the file by hand stands on its own, whatever the setup
	# suite found.
	id hotserve-backup >/dev/null 2>&1 || useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin hotserve-backup
	mkdir -p "$(dirname "$ENVFILE")" && chmod 0755 "$(dirname "$ENVFILE")"
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

# What the restore and the status suites both need of the repository and
# of the record.
newest() { rr snapshots --no-lock --json --tag "app:$1" --host hotserve 2>/dev/null | grep -o '"id":"[0-9a-f]\{64\}"' | tail -1 | cut -d'"' -f4; }
# app_json <app>: the app's own part of the record.
app_json() { awk -v open="    \"$1\": {" '$0 == open { on = 1 } on { print } on && /^    },?$/ { exit }' "$STATUS" | tr -d '\n' | sed 's/  */ /g'; }
proven() { app_json "$1" | sed -n 's/.*"restore_proven": { "snapshot": { "id": "\([0-9a-f]\{64\}\)".*/\1/p'; }
# craft <app> <dir>: a snapshot of <dir> as /backup/<app>, made by hand
# with the box's key — what an older engine, another tool or whoever
# holds that key could have put in the repository. Prints its id.
craft() {
	rm -rf /backup
	mkdir -p /backup
	cp -a "$2" "/backup/$1"
	rr backup --quiet --json --host hotserve --tag hotserve --tag "app:$1" "/backup/$1" 2>/dev/null | sed -n 's/.*"snapshot_id":"\([0-9a-f]\{64\}\)".*/\1/p'
	rm -rf /backup
}
# A crafted snapshot is forgotten once its scenario is over, so that the
# newest snapshot of an app is one the engine made.
forget() { rr forget --quiet "$@" >/dev/null 2>&1 || fail "the suite could not forget its own snapshot $*"; }
hold_restic() { # <pattern>: stops the first restic whose command line matches, and sets $pid
	i=0
	pid=
	until pid=$(pgrep -f "$1" | head -1) && [ -n "$pid" ]; do
		i=$((i + 1))
		[ "$i" -ge 600 ] && return 1
		sleep 0.05
	done
	kill -STOP "$pid"
}
