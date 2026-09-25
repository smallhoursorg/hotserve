#!/bin/sh
# The status suite. Runs inside e2e-backup-box as root, after the backup
# suite and before the restore suite, against a repository of its own.
#
# What it holds the commands to:
#
#	hotserve-backup status
#	hotserve-backup validate <Caddyfile>
#
# status reads the record a run leaves, asks the manager what is running,
# and says per app whether its backup is fresh and whether a restore of
# it has been proven lately; it exits 0 only if both hold for every app
# that has anything to back up. It needs no root. validate says whether
# a run could turn a Caddyfile into a plan — before the file goes live —
# and names the apps in it that declare no backup; it needs no root
# either, and changes nothing.
#
# status exits 0 healthy, 1 unhealthy, 3 when it could not tell: not set
# up, or a record it cannot read. (2 is the usage text's.)
#
# Until the commands exist each of them exits 2 with the usage text, so
# no check below is content with a non-zero exit alone.
. /lib.sh
. /lib-backup.sh

REPO=s3:http://e2e-s3:9000/statusrepo
BLOG=/var/lib/liveswap/blog/shared
V=/srv/validate

wait_for_systemd
[ -f /root/Caddyfile.base ] || cp "$CADDYFILE" /root/Caddyfile.base
cp /root/Caddyfile.base "$CADDYFILE"

st() { hotserve-backup status >"$OUT" 2>&1; }
# exits <n> <what>: the status just asked for left with <n>.
st_exits() {
	hotserve-backup status >"$OUT" 2>&1
	got=$?
	[ "$got" = "$1" ] && pass "$2: status exits $1" || fail "$2: status exited $got, want $1: $(cat "$OUT")"
}
drill() { hotserve-backup drill >/root/drill.out 2>&1; }
as_nobody() { setpriv --reuid nobody --regid nogroup --clear-groups "$@"; }
says() { grep -q -e "$1" "$OUT"; }
# age <app> <key> <when>: the record, with the time of the app's <key>
# (last_ok's, or restore_proven's own — not the snapshot's inside it)
# set to <when>. A container has no clock of its own to move; where the
# line between fresh and stale lies is the unit tests' to show.
age() {
	awk -v app="    \"$1\": {" -v key="      \"$2\": {" -v when="$3" '
		$0 == app { inapp = 1 }
		inapp && $0 == key { inkey = 1 }
		inkey && /^        "time": / { sub(/"time": "[^"]*"/, "\"time\": \"" when "\""); inkey = 0; inapp = 0 }
		{ print }' "$STATUS" >"$STATUS.aged" && mv "$STATUS.aged" "$STATUS" && chmod 0644 "$STATUS"
}
ago() { date -u -d "$1 ago" +%Y-%m-%dT%H:%M:%SZ; }

echo "=== status 0: before setup, status says so and is not content ==="
rm -f "$ENVFILE" "$STATUS"
seed
st_exits 3 "not set up is not 'unhealthy': status could not look"
says "not set up" && says "$ENVFILE" && pass "and says backups are not set up, and which file is not there" || fail "status before setup said: $(cat "$OUT")"

echo "=== status 1: set up and not yet run is pending, not failing ==="
write_env "$REPO" "$PASSWORD"
rr init -q || { echo "FATAL: could not initialise the repository at $REPO"; exit 1; }
if st; then says "pending first run" && pass "a new setup is 'pending first run', exit 0" || fail "status exited 0 and said: $(cat "$OUT")"; else fail "a new setup is reported as failing: $(cat "$OUT")"; fi

echo "=== status 2: after a run, each app's backup and the proof of its restore ==="
run || fail "the first run: $(cat "$OUT")"
blog_id=$(newest blog)
[ -n "$blog_id" ] && pass "fixture: the repository holds a snapshot of blog" || fail "fixture: no snapshot of blog: the scenario proves nothing"
if st; then pass "status exits 0"; else fail "status after a good run and its drill: $(cat "$OUT")"; fi
says "blog: ok.*snapshot $(echo "$blog_id" | cut -c1-8)" && pass "blog's line names the snapshot the repository holds" || fail "blog: $(grep '^blog' "$OUT")"
says "blog: restore last proven: .*snapshot $(echo "$blog_id" | cut -c1-8)" && pass "and says when a restore of it was last proven, and of which snapshot" || fail "blog's proof: $(grep '^blog' "$OUT")"
says "$BLOG" && pass "and where it looked for blog's data" || fail "no path in: $(grep '^blog' "$OUT")"
says "notyet: pending" && pass "an app declared and never deployed is pending" || fail "notyet: $(grep '^notyet' "$OUT")"
says "$blog_id" && fail "a whole snapshot id is printed" || pass "ids are short"
says "no unit of a run, a restore or a drill is running" && pass "and no unit of a run is running" || fail "running: $(grep -i running "$OUT")"

echo "=== status 3: status needs no root, and no credential ==="
cp "$OUT" /root/status.root
if as_nobody cat "$ENVFILE" >/dev/null 2>&1; then fail "fixture: nobody can read $ENVFILE: the scenario proves nothing"; else pass "fixture: nobody cannot read the credential"; fi
if as_nobody hotserve-backup status >"$OUT" 2>&1; then pass "as nobody, status exits 0"; else fail "as nobody: $(cat "$OUT")"; fi
cmp -s "$OUT" /root/status.root && pass "and says what it says to root" || fail "as nobody: $(diff /root/status.root "$OUT" | head -5)"

echo "=== status 4: a last good snapshot that is gone from the repository is said, and is not fresh ==="
forget "$blog_id"
if rr snapshots --no-lock --json | grep -q "$blog_id"; then fail "fixture: $blog_id is still in the repository: the scenario proves nothing"; else pass "fixture: blog's last good snapshot has been forgotten off the box"; fi
sed 's#sqlite app.db#sqlite app.db absent.db#' /root/Caddyfile.base >"$CADDYFILE"
run && fail "fixture: a run with an absent database exited 0"
expect_class blog incomplete "an absent database"
st_exits 1 "blog's last good snapshot gone"
# The proven snapshot is the same one, and its line begins the same.
says "blog: snapshot $(echo "$blog_id" | cut -c1-8) is no longer in the repository.*it was the last complete backup" && pass "and says the last complete backup is gone, and which snapshot it was" || fail "said: $(grep '^blog' "$OUT")"
says "blog: snapshot $(echo "$blog_id" | cut -c1-8) is no longer in the repository.*the restore proven was of it" && pass "and that the snapshot a restore was proven of is gone too" || fail "said: $(grep '^blog' "$OUT")"
says "shop: snapshot .* no longer" && fail "shop's snapshot is called gone too" || pass "shop's is not"
says "shop: ok" && pass "and shop is ok" || fail "shop: $(grep '^shop' "$OUT")"
cp /root/Caddyfile.base "$CADDYFILE"
run || fail "the run after: $(cat "$OUT")"
if st; then pass "after a good run, status exits 0 again"; else fail "status after a good run: $(cat "$OUT")"; fi

echo "=== status 5: a backup three hours old is stale, a proof eight days old is old ==="
when=$(ago "4 hours")
age blog last_ok "$when"
grep -q "$when" "$STATUS" && pass "fixture: blog's last good backup is four hours old in the record" || fail "fixture: the record was not changed: the scenario proves nothing"
if st; then fail "status exited 0 with a stale backup"; else pass "status exits non-zero"; fi
says "blog: stale" && pass "and calls blog stale" || fail "said: $(grep '^blog' "$OUT")"
says "shop: stale" && fail "and shop with it" || pass "and not shop"
run || fail "the run after: $(cat "$OUT")"
when=$(ago "9 days")
age shop restore_proven "$when"
grep -q "$when" "$STATUS" && pass "fixture: shop's proof is nine days old in the record" || fail "fixture: the record was not changed: the scenario proves nothing"
if st; then fail "status exited 0 with an old proof"; else pass "status exits non-zero"; fi
says "shop: restore last proven: .*: old" && pass "and calls shop's proof old" || fail "said: $(grep '^shop' "$OUT")"
says "blog: restore last proven: .*: old" && fail "and blog's with it" || pass "and not blog's"
drill || fail "the drill: $(cat /root/drill.out)"
if st; then pass "after a drill, status exits 0 again"; else fail "status after a drill: $(cat "$OUT")"; fi

echo "=== status 5b: a record that cannot be read is 'could not tell', not 'unhealthy' ==="
cp "$STATUS" /root/status.json.good
echo '{"apps": not json' >"$STATUS"
st_exits 3 "a record that is not JSON"
says "could not be read" && pass "and says the record could not be read" || fail "said: $(cat "$OUT")"
cp /root/status.json.good "$STATUS" && chmod 0644 "$STATUS" && rm -f /root/status.json.good
st_exits 0 "the record back"

echo "=== status 5c: an app that leaves the Caddyfile with snapshots in the repository is said, once ==="
sed '/app shop {/,/^\t\t}/d' /root/Caddyfile.base >"$CADDYFILE"
grep -q "app shop" "$CADDYFILE" && fail "fixture: shop is still in the Caddyfile: the scenario proves nothing" || pass "fixture: shop's block is gone from the Caddyfile"
hotserve validate --config "$CADDYFILE" --adapter caddyfile >/dev/null 2>&1 && pass "fixture: and hotserve accepts what is left" || fail "fixture: hotserve rejects the file: the scenario proves nothing"
run || fail "the run without shop: $(cat "$OUT")"
grep -q "warning: .*shop no longer declares a backup" "$OUT" && pass "the run that finds shop gone says so" || fail "the run said: $(cat "$OUT")"
st
says "warning: .*shop no longer declares a backup" && pass "and status says it after" || fail "status said: $(cat "$OUT")"
run || fail "the second run without shop: $(cat "$OUT")"
grep -q "shop no longer" "$OUT" && fail "and the run after says it again" || pass "the run after does not say it again"
cp /root/Caddyfile.base "$CADDYFILE"
run || fail "the run with shop back: $(cat "$OUT")"
drill || fail "the drill with shop back: $(cat /root/drill.out)"
st_exits 0 "with shop back, backed up and proven"

echo "=== status 6: a run under way is running, not failed — and says since when ==="
as_app sh -c "head -c 50000000 /dev/urandom >$BLOG/uploads/big.bin"
hotserve-backup run >/root/first.out 2>&1 &
first=$!
if hold_restic 'restic backup'; then
	[ "$(units_running)" != 0 ] && pass "fixture: a run is under way, its restic held still" || fail "fixture: no unit is running: the scenario proves nothing"
	if st; then pass "status exits 0: the last good backup is minutes old, whatever is in progress"; else fail "status during a run: $(cat "$OUT")"; fi
	says "running: upload of blog, since 20" && pass "and says what is running, for which app, since when" || fail "running: $(grep -i running "$OUT")"
	says "failed" && fail "a unit still running is called failed: $(grep failed "$OUT")" || pass "and calls nothing failed"
	as_nobody hotserve-backup status >"$OUT" 2>&1
	says "running: upload of blog, since 20" && pass "and says so to anyone: the manager is asked without privilege" || fail "as nobody, running: $(grep -i running "$OUT")"
	kill -CONT "$pid"
else
	fail "no restic process appeared to hold still"
fi
wait "$first" || fail "the held run: $(cat /root/first.out)"
as_app rm -f "$BLOG/uploads/big.bin"

echo "=== status 7: a drill that proved nothing is said, with why ==="
mkdir -p /root/bare && echo x >/root/bare/f
crafted=$(craft blog /root/bare)
[ -n "$crafted" ] && pass "fixture: blog's newest snapshot is one with no plan in it" || fail "fixture: nothing was crafted: the scenario proves nothing"
drill && fail "fixture: a drill of a snapshot with no plan exited 0"
if st; then fail "status exited 0 after a drill that proved nothing"; else pass "status exits non-zero"; fi
says "blog: restore not proven: ." && pass "and says blog's restore is not proven, and why" || fail "said: $(grep '^blog' "$OUT")"
says "shop: restore not proven" && fail "and shop's with it" || pass "and not shop's"
says "shop: restore last proven" && pass "shop's is proven" || fail "shop: $(grep '^shop' "$OUT")"
forget "$crafted"
drill || fail "the drill after: $(cat /root/drill.out)"
if st; then pass "after a drill that proves it, status exits 0 again"; else fail "status after a good drill: $(cat "$OUT")"; fi

echo "=== validate 8: what a run could not plan from is refused before it goes live ==="
validate() { hotserve-backup validate "$1" >"$OUT" 2>&1; }
before=$(sha256sum "$CADDYFILE" "$STATUS")
mkdir -p "$V" && chmod 0755 "$V"
if validate "$CADDYFILE"; then says "^a run would back up blog, notyet, shop, under /var/lib/liveswap$" && pass "the box's own Caddyfile validates, and the apps it backs up are named" || fail "said: $(cat "$OUT")"; else fail "the box's own Caddyfile: $(cat "$OUT")"; fi

sed 's/# Declared, never deployed./app cart {\n\t\t\tcommand .\/server\n\t\t}\n/' /root/Caddyfile.base >"$V/cart"
hotserve validate --config "$V/cart" --adapter caddyfile >/dev/null 2>&1 && pass "fixture: hotserve accepts an app with no backup block" || fail "fixture: hotserve rejects the file: the scenario proves nothing"
if validate "$V/cart"; then says "^cart declares no backup" && pass "an app that declares no backup is named, and is no refusal" || fail "said: $(cat "$OUT")"; else fail "an app with no backup block was refused: $(cat "$OUT")"; fi
if as_nobody hotserve-backup validate "$V/cart" >"$OUT" 2>&1; then says "^cart declares no backup" && pass "validate needs no root" || fail "as nobody: $(cat "$OUT")"; else fail "as nobody: $(cat "$OUT")"; fi

# shellcheck disable=SC2016 # the Caddyfile's own {$NAME}, not the shell's
sed 's#root /var/lib/liveswap#root {$LIVESWAP_ROOT:/var/lib/liveswap}#' /root/Caddyfile.base >"$V/envroot"
hotserve validate --config "$V/envroot" --adapter caddyfile >/dev/null 2>&1 && pass "fixture: hotserve accepts a root from the environment: push would let it through" || fail "fixture: hotserve rejects the file: the scenario proves nothing"
if validate "$V/envroot"; then fail "a root that depends on the environment validated"; else says "LIVESWAP_ROOT" && pass "a root that depends on the environment is refused, by the variable's name" || fail "said: $(cat "$OUT")"; fi

# A glob that reaches outside matches nothing inside a run's view, which
# to the adapter is no error: the run would plan without what is
# declared out there. The adapter's own warning is refused — here,
# where it matches nothing on the box either, and inside
# /etc/hotserve, where a run would see nothing more than validate does.
{ echo "import $V/apps/*.caddy"; cat /root/Caddyfile.base; } >/etc/hotserve/Caddyfile.new
hotserve validate --config /etc/hotserve/Caddyfile.new --adapter caddyfile >/dev/null 2>&1 && pass "fixture: hotserve accepts a glob import that matches nothing" || fail "fixture: hotserve rejects the file: the scenario proves nothing"
if validate /etc/hotserve/Caddyfile.new; then fail "a glob import that matches nothing validated"; else says "imports $V/apps/\*.caddy, which matches no file" && pass "a glob import that matches nothing is refused, by its pattern" || fail "said: $(cat "$OUT")"; fi
{ echo "import sites/*.caddy"; cat /root/Caddyfile.base; } >/etc/hotserve/Caddyfile.new
if validate /etc/hotserve/Caddyfile.new; then fail "an empty glob under /etc/hotserve validated"; else says "imports sites/\*.caddy, which matches no file" && pass "and so is one under /etc/hotserve" || fail "said: $(cat "$OUT")"; fi
mkdir -p /etc/hotserve/sites && chmod 0755 /etc/hotserve/sites
echo "# nothing" >/etc/hotserve/sites/a.caddy && chmod 0644 /etc/hotserve/sites/a.caddy
if validate /etc/hotserve/Caddyfile.new; then pass "and validates once it matches a file"; else fail "with a match: $(cat "$OUT")"; fi
rm -rf /etc/hotserve/Caddyfile.new /etc/hotserve/sites

if validate "$V/absent"; then fail "a file that is not there validated"; else says "$V/absent" && pass "a file that is not there is refused, by name" || fail "said: $(cat "$OUT")"; fi
says "declare" && fail "and something is said about what it declares: $(cat "$OUT")" || pass "and nothing is said about what it declares"
[ "$(sha256sum "$CADDYFILE" "$STATUS")" = "$before" ] && pass "validate changed neither the Caddyfile nor the record" || fail "validate changed something"
[ "$(units_left)" = 0 ] && pass "and started no unit" || fail "units: $(systemctl list-units --all --plain --no-legend 'hotserve_backup_*')"

echo "=== validate 8b: and the run itself refuses what its view hides ==="
# validate is a courtesy to whoever pushes; the plan step is what stands
# between an import the run cannot see and a run that plans without it.
# Here the glob matches on the box, so validate lets it through: inside
# the run's view, which holds /etc/hotserve alone, it matches nothing.
mkdir -p "$V/apps" && echo "# nothing" >"$V/apps/x.caddy"
{ echo "import $V/apps/*.caddy"; cat /root/Caddyfile.base; } >"$CADDYFILE"
journalctl --sync >/dev/null 2>&1
before=$(journalctl --no-pager -o cat | grep -c "which matches no file")
if run; then fail "a run exited 0 on a Caddyfile that imports from outside /etc/hotserve"; else pass "the run exits non-zero"; fi
journalctl --sync >/dev/null 2>&1
[ "$(journalctl --no-pager -o cat | grep -c "which matches no file")" -gt "$before" ] && pass "and its plan unit says why, in the journal" || fail "the journal does not say: $(cat "$OUT")"
if st; then fail "status exited 0 after a run that could not plan"; else says "the last run ended early" && pass "status says the last run ended early" || fail "said: $(cat "$OUT")"; fi
# A directory under /etc/hotserve the run's account may not list: root
# lists it, and validate as root sees the match; the run does not.
mkdir -p /etc/hotserve/sites && echo "# nothing" >/etc/hotserve/sites/a.caddy && chmod 0644 /etc/hotserve/sites/a.caddy && chmod 0700 /etc/hotserve/sites
{ echo "import sites/*.caddy"; cat /root/Caddyfile.base; } >"$CADDYFILE"
if run; then fail "a run exited 0 through a directory its account may not list"; else pass "a run through a directory its account may not list exits non-zero"; fi
rm -rf /etc/hotserve/sites
cp /root/Caddyfile.base "$CADDYFILE"
run || fail "the run after: $(cat "$OUT")"
if st; then pass "and after a good run, status exits 0 again"; else fail "status after: $(cat "$OUT")"; fi

echo "=== validate 9: nothing an operator types reaches a unit's command ==="
usage_for() { # <what> <args...>
	what=$1
	shift
	hotserve-backup "$@" >"$OUT" 2>&1
	[ $? = 2 ] && says "usage:" && says "hotserve-backup status" && says "hotserve-backup validate <Caddyfile>" && pass "$what is the usage text, exit 2" || fail "$what: $(cat "$OUT")"
}
usage_for "status with an argument" status now
usage_for "validate with no file" validate
usage_for "validate with two files" validate "$V/cart" "$V/envroot"
usage_for "the unit's own check, given a path" check "$V/cart"

rm -rf "$V" /root/bare /root/status.root /root/drill.out /root/first.out
cp /root/Caddyfile.base "$CADDYFILE"

echo ""
if [ "$FAILURES" -eq 0 ]; then
	echo "ALL STATUS SCENARIOS PASSED"
	exit 0
fi
echo "$FAILURES STATUS ASSERTION(S) FAILED"
exit 1
