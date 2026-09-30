#!/bin/sh
# The check suite. Runs inside an e2e-backup-box of its own as root,
# against repositories of its own.
#
# What it holds the commands to:
#
#	hotserve-backup drill     checks the repository itself after its apps:
#	                          its structure, and one fifty-second of its
#	                          data — the group after the last one read, the
#	                          ISO week's where none was — taking no lock
#	hotserve-backup run       writes into the repository the record that it
#	                          ended ok on an app
#	hotserve-backup restore   on a box with no record, says what those
#	                          records say
#
# The check's verdict is one of clean, damaged, unreachable, no
# repository, wrong password and failed. restic's exit 1 is both
# "damaged" and "unreachable": a probe of the repository before the
# check, and again after a check that failed, is what tells them apart,
# and it answers in seconds where restic would retry for a quarter of an
# hour.
. /lib.sh
. /lib-backup.sh

S3=http://e2e-s3:9000
REPO=s3:$S3/checkrepo

wait_for_systemd
[ -f /root/Caddyfile.base ] || cp "$CADDYFILE" /root/Caddyfile.base
cp /root/Caddyfile.base "$CADDYFILE"
# A box in a zone of its own: every unit sees /etc/localtime, and restic
# stores a snapshot's time in the zone it runs in.
ln -sf /usr/share/zoneinfo/Europe/Berlin /etc/localtime

drill() { hotserve-backup drill >"$OUT" 2>&1; }
says() { grep -q -e "$1" "$OUT"; }
# The record's last_check, on one line, and its class.
last_check() { tr -d '\n' <"$STATUS" | sed 's/  */ /g' | sed -n 's/.*"last_check": {\([^}]*\)}.*/\1/p'; }
check_class() { last_check | sed -n 's/.*"class": "\([^"]*\)".*/\1/p'; }
# A first check's group, as the engine reads it: the ISO week's, in UTC.
week=$(date -u +%V)
week=${week#0}
g=$(((week - 1) % 52 + 1))
GROUP="$g/52"
# The check after one that read GROUP reads the group after it.
NEXT="$((g % 52 + 1))/52"
# records: the ids the repository's clean-run records vouch for.
records() { rr snapshots --no-lock --json --host hotserve --tag hotserve-clean 2>/dev/null | grep -o '"vouches:[0-9a-f]\{64\}"' | cut -d: -f2 | tr -d '"'; }
# data_pack <bucket>: a pack that holds file data, from the repository's
# own index — not a tree, which a run after must still be able to read.
data_pack() {
	for i in $(rr list index --no-lock 2>/dev/null); do
		rr cat index --no-lock "$i" 2>/dev/null | tr -d '\n' | sed 's/{"id":"\([0-9a-f]\{64\}\)","blobs":\[/\n\1 /g' | grep '"type":"data"' | cut -d' ' -f1
	done | head -1
}
# s3_delete <key>: what someone holding a key that may delete does off
# the box, through the storage's own API — with the fixture's one key,
# read from the credential file as rr reads it.
s3_delete() { (
	set -a
	. "$ENVFILE"
	set +a
	curl -s -o /dev/null -w '%{http_code}' -X DELETE --aws-sigv4 "aws:amz:us-east-1:s3" --user "$AWS_ACCESS_KEY_ID:$AWS_SECRET_ACCESS_KEY" "$S3/$1"
); }
# took <since>: seconds since.
took() { echo $(($(date +%s) - $1)); }

seed
write_env "$REPO" "$PASSWORD"
rr init -q || {
	echo "FATAL: could not initialise the repository at $REPO"
	exit 1
}

echo "=== check 1: a run records in the repository each app it ended ok on ==="
run || fail "the first run: $(cat "$OUT")"
blog_id=$(newest blog)
shop_id=$(newest shop)
[ -n "$blog_id" ] && [ -n "$shop_id" ] || fail "fixture: the first run made no snapshot: $(cat "$OUT")"
records >/root/records
grep -qx "$blog_id" /root/records && grep -qx "$shop_id" /root/records && [ "$(wc -l </root/records)" = 2 ] && pass "a record vouches for blog's snapshot and shop's, and there is none for notyet, which has no data" || fail "the records vouch for: $(cat /root/records); blog $blog_id, shop $shop_id"
paths=$(rr snapshots --no-lock --json --tag hotserve-clean 2>/dev/null | grep -o '"paths":\["[^"]*"\]' | sort -u | tr '\n' ' ')
[ "$paths" = '"paths":["/hotserve-clean-blog"] "paths":["/hotserve-clean-shop"] ' ] && pass "each app's records are a group of their own, as its snapshots are" || fail "the records' paths: $paths"
rr snapshots --no-lock --json --tag app:blog 2>/dev/null | grep -q hotserve-clean && fail "a record is among blog's own snapshots" || pass "and none is among an app's own snapshots"
# At the snapshot's own time, to the second: a forget policy that keeps
# the snapshot for a period keeps its record, in the same period.
stored=$(rr cat snapshot --no-lock "$blog_id" 2>/dev/null | grep -o '"time": *"[^"]*"' | head -1 | sed 's/.*: *"//; s/"$//')
made=$(echo "$stored" | cut -c1-19)
# In UTC, on a box in Berlin: forget sorts a snapshot into its days by
# the zone it was stored in, and its record is in UTC.
case "$stored" in *Z) pass "on a box in Berlin, blog's snapshot is stored in UTC ($stored)" ;; *) fail "on a box in Berlin, blog's snapshot is stored at $stored, not in UTC" ;; esac
recorded=$(rr snapshots --no-lock --json --tag "vouches:$blog_id" 2>/dev/null | grep -o '"time":"[^"]*"' | head -1 | cut -d'"' -f4 | cut -c1-19)
[ -n "$made" ] && [ "$made" = "$recorded" ] && pass "blog's record is at its snapshot's own time, $made" || fail "blog's snapshot was made $made, its record is at $recorded"

echo "=== check 2: a drill checks the repository, and a sound one is clean ==="
if drill; then pass "a drill of a sound repository exits 0"; else fail "the drill: $(cat "$OUT")"; fi
says "^repository: its structure, and data group $GROUP: clean$" && pass "and says the repository's structure and, a first check, the ISO week's group, $GROUP, are clean" || fail "the drill said: $(cat "$OUT")"
[ "$(check_class)" = clean ] && last_check | grep -q "\"group\": \"$GROUP\"" && pass "the record's last_check says so" || fail "last_check: $(last_check)"
hotserve-backup status >/root/status.out 2>&1 && grep -q "repository last checked .*: its structure, and data group $GROUP: clean" /root/status.out && pass "status says when it was checked, and is content" || fail "status: $(cat /root/status.out)"

echo "=== check 3: a pack gone from the storage is damage, and backups go on ==="
pack=$(data_pack)
[ -n "$pack" ] || fail "fixture: no pack holding data in the index"
code=$(s3_delete "checkrepo/data/$(echo "$pack" | cut -c1-2)/$pack")
[ "$code" = 204 ] && pass "fixture: pack $(echo "$pack" | cut -c1-8) deleted through the storage's API" || fail "fixture: the delete answered $code"
if drill; then fail "a drill of a repository missing a pack exited 0"; else pass "a drill of a repository missing a pack exits non-zero"; fi
# A second check reads the group after the first's; and the count is
# restic's — two where the pack gone is in the group read as well.
says "^repository: its structure, and data group $NEXT: damaged: restic check found [0-9][0-9]* errors*; a prune run off the box during the check looks the same, and the next check reads this group again; restic's own words: \`journalctl -u hotserve_backup_repocheck_[0-9a-f]*.service\`$" && pass "and says damaged, how much, and where restic's words are, of the group after the last one read, $NEXT" || fail "the drill said: $(cat "$OUT")"
[ "$(check_class)" = damaged ] && pass "the record's last_check is damaged" || fail "last_check: $(last_check)"
if hotserve-backup status >/root/status.out 2>&1; then fail "status exited 0 on a damaged repository"; else grep -q "the repository check of .* (data group $NEXT): damaged: restic check found [0-9][0-9]* error" /root/status.out && pass "status is unhealthy, and says why" || fail "status: $(cat /root/status.out)"; fi
# Damage keeps its group: the next check reads it again, and says so
# until it reads clean (the owner, 2026-09-30).
drill
says "^repository: its structure, and data group $NEXT: damaged: " && pass "the next drill reads the same group again, $NEXT, and it is damaged still" || fail "the next drill said: $(grep '^repository' "$OUT")"
run
[ "$(newest blog)" != "$blog_id" ] && [ "$(newest shop)" != "$shop_id" ] && pass "and the next run still backs both apps up: the box does not stop for damage" || fail "after damage the run made no snapshot: $(cat "$OUT")"

# A sound repository again, for what follows; a box set up afresh.
REPO=s3:$S3/checkrepo2
write_env "$REPO" "$PASSWORD"
rr init -q || fail "fixture: could not initialise $REPO"
rm -f "$STATUS"

echo "=== check 4: the wrong password is said, and in seconds ==="
write_env "$REPO" not-the-password
t0=$(date +%s)
if drill; then fail "a drill with the wrong password exited 0"; else pass "a drill with the wrong password exits non-zero"; fi
says "^repository: its structure, and data group $GROUP: wrong password: the repository password is wrong (exit 12)$" && [ "$(took "$t0")" -lt 60 ] && pass "the check says wrong password, within a minute ($(took "$t0")s)" || fail "the drill said, in $(took "$t0")s: $(cat "$OUT")"
[ "$(check_class)" = "wrong password" ] && pass "the record's last_check is wrong password" || fail "last_check: $(last_check)"

echo "=== check 5: a wrong storage key, and a host that does not resolve, answer in seconds ==="
# No app declares a backup, so that the drill asks the repository
# nothing of its own: the check's probe is all that waits, and it waits
# thirty seconds where restic would retry for a quarter of an hour.
awk '/^\t\t\tbackup \{/ { skip = 1 } !skip { print } skip && /^\t\t\t\}/ { skip = 0 }' /root/Caddyfile.base >"$CADDYFILE"
grep -q '^[[:space:]]*backup {' "$CADDYFILE" && fail "fixture: the Caddyfile still declares a backup"
write_env "$REPO" "$PASSWORD"
sed -i 's/^AWS_SECRET_ACCESS_KEY=.*/AWS_SECRET_ACCESS_KEY=not-the-key/' "$ENVFILE"
t0=$(date +%s)
if drill; then fail "a drill with the wrong storage key exited 0"; else pass "a drill with the wrong storage key exits non-zero"; fi
says "^repository: its structure, and data group $GROUP: unreachable: the repository did not answer within 30s; the unit was stopped: the storage could not be reached, or refused the key$" && [ "$(took "$t0")" -lt 90 ] && pass "the check says unreachable or refused, within a minute and a half ($(took "$t0")s)" || fail "the drill said, in $(took "$t0")s: $(cat "$OUT")"
write_env "s3:http://no-such-host.invalid:9000/checkrepo2" "$PASSWORD"
t0=$(date +%s)
drill
says "^repository: its structure, and data group $GROUP: unreachable: " && [ "$(took "$t0")" -lt 90 ] && pass "a host that does not resolve is unreachable too ($(took "$t0")s) — never damaged" || fail "the drill said, in $(took "$t0")s: $(cat "$OUT")"
[ "$(units_left)" = 0 ] && pass "no unit is left" || fail "units left: $(systemctl list-units --all --no-legend 'hotserve_backup_*')"
cp /root/Caddyfile.base "$CADDYFILE"
write_env "$REPO" "$PASSWORD"

echo "=== check 6: a check killed hard is no verdict of damage, and the next run backs up ==="
# That the check takes no lock — so that one killed hard leaves nothing
# every backup would meet — is TestIntegrationTheCheckWritesNoLock's to
# show: on a repository this small a check that locks holds its lock
# for less than any step a suite can take.
run || fail "fixture: a run on $REPO: $(cat "$OUT")"
hotserve-backup drill >"$OUT" 2>&1 &
drilling=$!
if hold_restic "/usr/bin/restic check"; then
	kill -KILL "$pid"
	wait "$drilling"
	says "^repository: its structure, and data group $GROUP: failed: restic was ended by signal$" && pass "a check killed hard is said to have failed, not to be damaged" || fail "the drill said: $(cat "$OUT")"
	[ "$(check_class)" = failed ] && pass "the record's last_check is failed" || fail "last_check: $(last_check)"
	[ "$(units_left)" = 0 ] && pass "no unit is left" || fail "units left: $(systemctl list-units --all --no-legend 'hotserve_backup_*')"
	before=$(newest blog)
	t0=$(date +%s)
	run
	[ "$(class blog)" = ok ] && [ "$(newest blog)" != "$before" ] && [ "$(took "$t0")" -lt 120 ] && pass "and the next run backs up at once ($(took "$t0")s)" || fail "the run after: $(cat "$OUT")"
else
	wait "$drilling"
	fail "fixture: no restic check was seen to hold: $(cat "$OUT")"
fi

echo "=== check 7: on a box with no record, the repository's records say which snapshot a run ended ok on ==="
vouched=$(newest blog)
crafted=$(craft blog /var/lib/liveswap/blog/shared)
[ -n "$crafted" ] && [ "$(newest blog)" = "$crafted" ] && pass "fixture: blog's newest snapshot is one no run made" || fail "fixture: nothing was crafted"
rm -f "$STATUS"
echo n | hotserve-backup restore blog >"$OUT" 2>&1
says "blog: this is not the last snapshot a backup run ended ok on — that is $(echo "$vouched" | cut -c1-8)" && pass "a restore on a rebuilt box says the newest is not what a run ended ok on, and names the one that is" || fail "the question said: $(cat "$OUT")"
says "restore snapshot $(echo "$crafted" | cut -c1-8)" && pass "and still offers the newest (the owner, 2026-09-29)" || fail "the question: $(cat "$OUT")"
forget "$crafted"
echo n | hotserve-backup restore blog >"$OUT" 2>&1
says "not the last snapshot" || says "is not known" && fail "the newest, vouched for, is said not to be: $(cat "$OUT")" || pass "with the newest vouched for, nothing is said of it"
echo "=== check 8: a run stopped while it writes a record says the record may be missing, and the next run writes one ==="
hotserve-backup run >"$OUT" 2>&1 &
running=$!
if hold_restic "hotserve-clean-blog"; then
	# Held as it starts, before restic has written anything. The run is
	# stopped as Ctrl-C stops it, and restic let go only once the manager
	# is stopping its unit: it cannot finish its write first.
	kill -INT "$running"
	i=0
	until systemctl list-units --plain --no-legend --state=deactivating 'hotserve_backup_vouch_*' | grep -q .; do
		i=$((i + 1))
		[ "$i" -ge 200 ] && break
		sleep 0.05
	done
	kill -CONT "$pid"
	if wait "$running"; then fail "a run stopped while it wrote a record exited 0"; else pass "a run stopped while it wrote a record exits non-zero"; fi
	grep -q "blog: snapshot [0-9a-f]\{8\} is a complete backup, and the record that says so may not have been written: the command was stopped while it was being written" "$STATUS" && pass "the record says blog's record of a clean run may not have been written, and why" || fail "the record's warning: $(grep '"warning"' "$STATUS")"
	[ "$(class blog)" = ok ] && pass "and blog's backup, which is sound, is still ok" || fail "blog: $(app_json blog)"
	hotserve-backup status >/root/status.out 2>&1
	grep -q "^warning: .*blog: snapshot [0-9a-f]\{8\} is a complete backup, and the record that says so may not have been written" /root/status.out && pass "status says so" || fail "status: $(cat /root/status.out)"
	[ "$(units_left)" = 0 ] && pass "no unit is left" || fail "units left: $(systemctl list-units --all --no-legend 'hotserve_backup_*')"
else
	wait "$running"
	fail "fixture: no restic writing blog's record was seen to hold: $(cat "$OUT")"
fi
run || fail "the run after: $(cat "$OUT")"
records | grep -qx "$(newest blog)" && pass "the next run writes the record of its own snapshot" || fail "the records vouch for: $(records | tr '\n' ' '); blog's newest is $(newest blog)"
nothing_left "the check suite"

cp /root/Caddyfile.base "$CADDYFILE"
ln -sf /usr/share/zoneinfo/Etc/UTC /etc/localtime
rm -f /root/records /root/status.out

echo ""
if [ "$FAILURES" -eq 0 ]; then
	echo "ALL CHECK SCENARIOS PASSED"
	exit 0
fi
echo "$FAILURES CHECK ASSERTION(S) FAILED"
exit 1
