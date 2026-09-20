#!/bin/sh
# The restore suite. Runs inside e2e-backup-box as root, after the
# backup suite, against a repository of its own. Nearly all failure
# paths: what a restore and a restore drill say, change and leave behind
# when the snapshot, the repository or the place restored into is not
# what it should be.
#
# What it holds the commands to:
#
#	hotserve-backup restore <app> [--snapshot <id>] [--to <dir>] [--no-pre-backup] [--yes]
#	hotserve-backup drill
#
# restore puts the newest snapshot of <app> — or the one named, by its
# hex id and nothing else — into place: each database copy checked and
# then restored into the live database, files copied in, what is not in
# the snapshot left where it is and listed. It asks first (--yes
# answers), and backs the app up first (--no-pre-backup skips that; a
# pre-backup that fails stops a restore that was not told to skip it).
# Every check comes before any change: a snapshot that lacks something
# its own plan.json declares, or holds a damaged copy, changes nothing.
# --to <dir> puts the same into a new directory instead, asks nothing
# and backs nothing up. drill does everything but install, for every
# app, and the record keeps what it last proved.
. /lib.sh
. /lib-backup.sh

REPO=s3:http://e2e-s3:9000/restorerepo
RESTAGING=/var/lib/hotserve-backup/restore
BLOG=/var/lib/liveswap/blog/shared
SHOP=/var/lib/liveswap/shop/shared

wait_for_systemd
[ -f /root/Caddyfile.base ] || cp "$CADDYFILE" /root/Caddyfile.base
cp /root/Caddyfile.base "$CADDYFILE"

restore() { hotserve-backup restore "$@" >"$OUT" 2>&1; }
drill() { hotserve-backup drill >"$OUT" 2>&1; }
# What a restore fetched: anything at all inside an app's own directory
# there — a directory's name is plaintext too.
restaged() { [ -d "$RESTAGING" ] && find "$RESTAGING" -mindepth 2 | wc -l; }
nothing_left_of_a_restore() { # <what>
	nothing_left "$1"
	[ "$(restaged)" = 0 ] && pass "$1: nothing that was fetched is left on disk" || fail "$1: left in $RESTAGING: $(find "$RESTAGING" -mindepth 2 | head -5)"
	[ "$(find /var/lib/liveswap -user root -o -user hotserve-backup | wc -l)" = 0 ] && pass "$1: nothing in the apps' data belongs to anyone but the app" || fail "$1: in the apps' data: $(find /var/lib/liveswap \( -user root -o -user hotserve-backup \) -printf '%p (%u) ')"
}
# fingerprint <shared dir> <database> <table>: what is there — the
# database's rows, and every other entry's name, type, owner, mode,
# link target and contents. Compared before and after whatever must
# change nothing.
fingerprint() {
	as_app sqlite3 -cmd '.timeout 5000' "$1/$2" "pragma integrity_check; select count(*), coalesce(sum(n), 0) from $3" 2>&1
	(
		cd "$1" || exit
		db=$(basename "$2")
		find . ! -name "$db" ! -name "$db-wal" ! -name "$db-shm" -printf '%p %y %u %m %l\n' | sort
		find . -type f ! -name "$db" ! -name "$db-wal" ! -name "$db-shm" -exec sha256sum {} + | sort -k 2
	)
}
blog_print() { fingerprint "$BLOG" app.db posts; }
shop_print() { fingerprint "$SHOP" data/shop.db orders; }
unchanged() { # <before> <after> <what>
	[ "$1" = "$2" ] && pass "$3" || fail "$3 — before: $(echo "$1" | tr '\n' '|') after: $(echo "$2" | tr '\n' '|')"
}
rows() { as_app sqlite3 -cmd '.timeout 5000' "$1" "pragma integrity_check; $2" 2>&1 | tr '\n' ' '; }
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

rm -f "$STATUS"
seed
# Modes that are not the ones a restore would make anyway, and a
# directory its owner has closed to writing: each is in every snapshot
# below, fetched by every restore and drill, and has to be removed again.
as_app sh -c "cd $BLOG/uploads && mkdir -m 0755 pub && echo pub >pub/b.png && chmod 0644 pub/b.png && mkdir ro && echo ro >ro/f && chmod 0444 ro/f && chmod 0555 ro"
write_env "$REPO" "$PASSWORD"
rr init -q || { echo "FATAL: could not initialise the repository at $REPO"; exit 1; }

echo "=== restore 0: nothing to restore is never a restore that worked ==="
blog0=$(blog_print)
if restore blog --yes; then fail "a restore from a repository that holds no snapshot of blog exited 0"; else pass "with no snapshot of the app, a restore exits non-zero"; fi
grep -qi "no snapshot" "$OUT" && pass "and says there is no snapshot of it" || fail "no snapshot: $(cat "$OUT")"
unchanged "$blog0" "$(blog_print)" "and changed nothing"
nothing_left_of_a_restore "no snapshot"
for bad in Blog ../shop 'blog shop' '${RESTIC_PASSWORD}' ''; do
	if restore "$bad" --yes; then fail "restore '$bad' exited 0"; else [ "$(units_left)" = 0 ] && grep -qi "app name" "$OUT" && pass "an app name that is not one ('$bad') is refused as that, before any unit starts" || fail "restore '$bad': $(units_left) units, and said: $(cat "$OUT")"; fi
done
if restore ghost --yes; then fail "a restore of an app nothing declares exited 0"; else grep -q ghost "$OUT" && pass "an app the Caddyfile does not declare is refused, by name" || fail "restore ghost: $(cat "$OUT")"; fi

echo "=== restore 1: the first backup proves its own restore ==="
run && pass "the first run exits 0" || fail "the first run: $(cat "$OUT")"
first_blog=$(newest blog)
first_shop=$(newest shop)
[ -n "$first_blog" ] && [ -n "$first_shop" ] || fail "the suite could not read the snapshots the first run made: nothing below is known"
[ "$(proven blog)" = "$first_blog" ] && [ -n "$first_blog" ] && pass "after its first backup, blog's record says a restore of that snapshot was proven" || fail "blog's record after the first run: $(app_json blog)"
[ "$(proven shop)" = "$first_shop" ] && [ -n "$first_shop" ] && pass "and shop's" || fail "shop's record after the first run: $(app_json shop)"
[ -z "$(proven notyet)" ] && pass "an app with nothing backed up has nothing proven" || fail "notyet's record: $(app_json notyet)"
nothing_left_of_a_restore "the first run's drill"

echo "=== restore 2: into place, under a writer, leaving what the snapshot does not hold ==="
as_app sh -c "cd $BLOG && sqlite3 -cmd '.timeout 5000' app.db 'delete from posts; insert into posts values (7);' && rm -rf uploads/a.png uploads/pub && echo later >uploads/new.png && echo changed >private.txt && chmod 0755 uploads/ro && echo changed >uploads/ro/g && chmod 0555 uploads/ro"
: >/tmp/writer.on
as_app sh -c "while [ -e /tmp/writer.on ]; do sqlite3 -cmd '.timeout 30000' $BLOG/app.db 'insert into posts values (99)' || echo failed; sleep 0.05; done" >/root/writer.err 2>&1 &
writer=$!
before=$(snapshots)
if restore blog --snapshot "$first_blog" --yes; then pass "the restore exits 0"; else fail "the restore failed: $(cat "$OUT")"; fi
sleep 0.3
rm -f /tmp/writer.on
wait "$writer" 2>/dev/null
[ ! -s /root/writer.err ] && pass "a writer that kept writing throughout saw no error" || fail "the writer: $(head -3 /root/writer.err)"
[ "$(rows "$BLOG/app.db" 'select count(*) > 0 from posts where n = 99')" = "ok 1 " ] && pass "fixture: and it did write, after the restore too" || fail "fixture: the writer wrote nothing that is there now: the scenario proves nothing about a writer"
[ "$(rows "$BLOG/app.db" 'select count(*) from posts where n in (1, 2); select count(*) from posts where n = 7')" = "ok 2 0 " ] && pass "the database holds the snapshot's rows and not the ones made since, and is intact" || fail "blog's database after the restore: $(rows "$BLOG/app.db" 'select n from posts where n <> 99')"
[ "$(cat "$BLOG/uploads/a.png" 2>/dev/null)" = img ] && pass "a file that had been removed is back" || fail "uploads/a.png: $(ls -la "$BLOG/uploads")"
[ "$(cat "$BLOG/uploads/new.png" 2>/dev/null)" = later ] && pass "a file newer than the snapshot is left where it is" || fail "uploads/new.png is gone: a restore deleted app data"
grep -q 'uploads/new.png' "$OUT" && pass "and is listed" || fail "new.png is not listed: $(cat "$OUT")"
[ "$(cat "$BLOG/private.txt")" = changed ] && pass "what was never declared is not touched" || fail "private.txt: $(cat "$BLOG/private.txt")"
grep -q "blog: .*snapshot $(echo "$first_blog" | cut -c1-8)" "$OUT" && pass "the restore names the snapshot it restored" || fail "the restore's own words: $(cat "$OUT")"
[ "$(snapshots)" = $((before + 1)) ] && pass "and backed the app up first: the restore can be undone ($before -> $(snapshots) snapshots)" || fail "snapshots went from $before to $(snapshots): no backup was made before overwriting"
pre=$(newest blog)
rm -rf /root/restored && rr restore -q --no-lock "$pre:/backup/blog/sqlite" --target /root/restored >/dev/null 2>&1
[ "$(sqlite3 /root/restored/app.db 'select count(*) from posts where n = 7' 2>&1)" = 1 ] && pass "that backup holds what the restore overwrote" || fail "the pre-restore snapshot does not hold the overwritten row: $(sqlite3 /root/restored/app.db 'select n from posts' 2>&1 | tr '\n' ' ')"
rm -rf /root/restored
[ "$(stat -c '%U %a' "$BLOG/uploads/pub" "$BLOG/uploads/pub/b.png" | tr '\n' ' ')" = "hotserve 755 hotserve 644 " ] && pass "a directory and a file that were gone are back with the modes they had, not the ones a restore writes with" || fail "uploads/pub: $(stat -c '%U %a %n' "$BLOG/uploads/pub" "$BLOG/uploads/pub/b.png" 2>&1 | tr '\n' ' ')"
[ "$(stat -c %a "$BLOG/uploads/ro")" = 555 ] && [ "$(cat "$BLOG/uploads/ro/f")" = ro ] && [ "$(cat "$BLOG/uploads/ro/g")" = changed ] && pass "a directory that is closed to writing in place is restored into, left closed, and what it held besides is still there" || fail "uploads/ro: $(stat -c %a "$BLOG/uploads/ro") $(ls -la "$BLOG/uploads/ro")"
# A second try takes the newest snapshot a backup run made: never the one
# the restore before it made of what it was restoring over.
if restore blog --yes --no-pre-backup; then grep -q "blog: restored from snapshot $(echo "$first_blog" | cut -c1-8)" "$OUT" && pass "asked for no snapshot, a restore takes the newest a run made, not what the last restore backed up first" || fail "the default snapshot: $(cat "$OUT")"; else fail "a restore of the default snapshot: $(cat "$OUT")"; fi
as_app rm -f "$BLOG/uploads/ro/g" 2>/dev/null || as_app sh -c "chmod 0755 $BLOG/uploads/ro && rm -f $BLOG/uploads/ro/g && chmod 0555 $BLOG/uploads/ro"
nothing_left_of_a_restore "a restore into place"

echo "=== restore 3: a restore asks first, and changes nothing until it is answered ==="
as_app sh -c "cd $BLOG && sqlite3 -cmd '.timeout 5000' app.db 'delete from posts where n = 99; insert into posts values (8);'"
blog0=$(blog_print)
before=$(snapshots)
if hotserve-backup restore blog </dev/null >"$OUT" 2>&1; then fail "a restore with nobody to ask and no --yes exited 0"; else grep -q -e '--yes' "$OUT" && pass "with nobody to ask, a restore refuses and names --yes" || fail "nobody to ask: $(cat "$OUT")"; fi
unchanged "$blog0" "$(blog_print)" "and changed nothing"
[ "$(snapshots)" = "$before" ] && pass "and made no backup either: nothing happens before the answer" || fail "snapshots went from $before to $(snapshots) before anyone said yes"
if printf 'n\n' | hotserve-backup restore blog >"$OUT" 2>&1; then fail "a restore answered 'n' exited 0"; else grep -qi "nothing was changed" "$OUT" && pass "answered 'n', a restore exits non-zero and says nothing was changed" || fail "answered 'n': $(cat "$OUT")"; fi
unchanged "$blog0" "$(blog_print)" "and changed nothing"
for flag in --to --snapshot; do
	if restore blog "$flag" "" --yes; then fail "$flag '' exited 0"; else [ "$(units_left)" = 0 ] && grep -q "nothing in it" "$OUT" && pass "$flag given with nothing in it is refused: it is not the same restore without the flag" || fail "$flag '': $(cat "$OUT")"; fi
done
unchanged "$blog0" "$(blog_print)" "and changed nothing"
for id in latest '${RESTIC_PASSWORD}' "--target=/etc" "$(echo "$first_blog" | cut -c1-8) /etc"; do
	if restore blog --snapshot "$id" --yes; then fail "--snapshot '$id' exited 0"; else [ "$(units_left)" = 0 ] && grep -qi "hex" "$OUT" && pass "--snapshot '$id' is refused before any unit starts: a snapshot is named by its hex id" || fail "--snapshot '$id': $(units_left) units, and said: $(cat "$OUT")"; fi
done

echo "=== restore 4: a snapshot of another app, and a snapshot that does not exist ==="
shop0=$(shop_print)
if restore blog --snapshot "$first_shop" --yes --no-pre-backup; then fail "restoring blog from shop's snapshot exited 0"; else pass "blog restored from shop's snapshot exits non-zero"; fi
grep -qi "not a snapshot of blog" "$OUT" && pass "and says the snapshot is not blog's" || fail "another app's snapshot: $(cat "$OUT")"
unchanged "$blog0" "$(blog_print)" "blog is unchanged"
unchanged "$shop0" "$(shop_print)" "shop is unchanged"
[ -e "$BLOG/r.txt" ] || [ -e "$BLOG/data" ] && fail "shop's files are in blog's data" || pass "nothing of shop's is in blog's data"
if restore blog --snapshot 00000000000000000000000000000000000000000000000000000000deadbeef --yes --no-pre-backup; then fail "a snapshot that does not exist restored with exit 0"; else pass "a snapshot that does not exist exits non-zero"; fi
unchanged "$blog0" "$(blog_print)" "and changes nothing"
nothing_left_of_a_restore "a snapshot that is not the app's"

echo "=== restore 5: a damaged copy in the snapshot is never restored over a live database ==="
# The two ways sqlite3 reports damage: in words with exit 0, and by
# refusing the file with a non-zero exit.
rm -rf /root/craft && mkdir -p /root/craft/words/sqlite /root/craft/words/files/uploads
printf '{"sqlite":["app.db"],"files":["uploads"]}' >/root/craft/words/plan.json
echo crafted >/root/craft/words/files/uploads/crafted.png
sqlite3 /root/craft/words/sqlite/app.db "create table posts(n); create table other(n); insert into posts values (66),(67); create index posts_n on posts(n); create index other_n on other(n);" >/dev/null
sqlite3 /root/craft/words/sqlite/app.db ".dbconfig defensive off" "pragma writable_schema=on; update sqlite_schema set rootpage = (select rootpage from sqlite_schema where name = 'other_n') where name = 'posts_n';" >/dev/null 2>&1
words=$(sqlite3 /root/craft/words/sqlite/app.db 'pragma integrity_check' 2>&1)
words_exit=$?
[ "$words_exit" = 0 ] && [ "$words" != ok ] && pass "fixture: a copy sqlite3 calls damaged in words, with exit 0 ($(echo "$words" | head -1))" || fail "fixture: integrity_check of the first damaged copy said '$words' and exited $words_exit: the scenario proves nothing about that shape"
cp -a /root/craft/words /root/craft/refused
rm -f /root/craft/refused/sqlite/app.db
sqlite3 /root/craft/refused/sqlite/app.db "create table posts(n); insert into posts select hex(randomblob(3000)) from (select 1 union select 2 union select 3), (select 1 union select 2 union select 3);" >/dev/null
size=$(stat -c %s /root/craft/refused/sqlite/app.db)
dd if=/dev/urandom of=/root/craft/refused/sqlite/app.db bs=1 seek=100 count=$((size - 200)) conv=notrunc 2>/dev/null
sqlite3 /root/craft/refused/sqlite/app.db 'pragma integrity_check' >/dev/null 2>&1
[ $? != 0 ] && pass "fixture: a copy sqlite3 refuses outright, with a non-zero exit" || fail "fixture: sqlite3 exited 0 on the second damaged copy: the scenario proves nothing about that shape"
for shape in words refused; do
	id=$(craft blog "/root/craft/$shape")
	[ -n "$id" ] || { fail "the suite could not make the '$shape' snapshot"; continue; }
	if restore blog --snapshot "$id" --yes --no-pre-backup; then fail "[$shape] a restore of a damaged copy exited 0"; else pass "[$shape] a restore of a damaged copy exits non-zero"; fi
	grep -qi "damaged\|integrity" "$OUT" && grep -q "app.db" "$OUT" && pass "[$shape] and says which copy is damaged" || fail "[$shape] the restore's own words: $(cat "$OUT")"
	unchanged "$blog0" "$(blog_print)" "[$shape] the live database and everything beside it is unchanged: every check comes before any change"
	[ ! -e "$BLOG/uploads/crafted.png" ] && pass "[$shape] the snapshot's files were not installed either" || fail "[$shape] crafted.png was installed from a snapshot whose database is damaged"
	nothing_left_of_a_restore "[$shape] a damaged copy"
	# To a directory: what is sound may land, the damaged copy never as
	# if it were a database.
	rm -rf /srv/out
	if restore blog --snapshot "$id" --to /srv/out; then fail "[$shape] --to with a damaged copy exited 0"; else pass "[$shape] --to with a damaged copy exits non-zero"; fi
	[ "$(cat /srv/out/uploads/crafted.png 2>/dev/null)" = crafted ] && pass "[$shape] what is sound in the snapshot is there to look at" || fail "[$shape] /srv/out: $(find /srv/out 2>&1 | head -5)"
	[ ! -e /srv/out/app.db ] && pass "[$shape] and no app.db is handed out" || fail "[$shape] /srv/out/app.db exists: a damaged copy was handed out as the database"
	rm -rf /srv/out
	forget "$id"
done

echo "=== restore 6: what a snapshot says of itself is not believed because it is in the repository ==="
rm -rf /root/craft && mkdir -p /root/craft/base/sqlite /root/craft/base/files/uploads
sqlite3 /root/craft/base/sqlite/app.db "create table posts(n); insert into posts values (66);" >/dev/null
echo crafted >/root/craft/base/files/uploads/crafted.png
hostile() { # <name> <plan.json, or - for none> <what> <a word of the reason>
	rm -rf "/root/craft/$1" && cp -a /root/craft/base "/root/craft/$1"
	[ "$2" = - ] || printf '%s' "$2" >"/root/craft/$1/plan.json"
	id=$(craft blog "/root/craft/$1")
	[ -n "$id" ] || { fail "the suite could not make the '$1' snapshot"; return; }
	if restore blog --snapshot "$id" --yes --no-pre-backup; then fail "$3: the restore exited 0"; else grep -q "$4" "$OUT" && pass "$3: the restore exits non-zero, and says why ($4)" || fail "$3: $(cat "$OUT")"; fi
	unchanged "$blog0" "$(blog_print)" "$3: blog is unchanged"
	unchanged "$shop0" "$(shop_print)" "$3: shop is unchanged"
	[ "$(find /var/lib/liveswap /etc/hotserve -name 'crafted*' | wc -l)" = 0 ] && pass "$3: nothing of the snapshot's was installed anywhere" || fail "$3: installed: $(find /var/lib/liveswap /etc/hotserve -name 'crafted*')"
	forget "$id"
}
hostile noplan - "a snapshot with no plan.json" "holds none"
hostile garbage 'not json' "a plan.json that is not one" "invalid character"
hostile unknown '{"sqlite":["app.db"],"files":["uploads"],"install_to":"/etc"}' "a plan.json with a field nobody knows" install_to
hostile dotdot '{"sqlite":["../../shop/shared/data/shop.db"],"files":["uploads"]}' "a plan.json whose database path leaves the app" "outside the app's shared dir"
hostile absolute '{"files":["/etc/hotserve"]}' "a plan.json with an absolute path" "not absolute"
hostile lacks '{"sqlite":["app.db","missing.db"],"files":["uploads"]}' "a snapshot that lacks a database its plan.json declares" missing.db
hostile empty '{"files":["avatars"]}' "a snapshot that holds nothing of what its plan.json declares" avatars
nothing_left_of_a_restore "hostile snapshots"

echo "=== restore 7: a database inside a files path comes from its copy, never from files/ ==="
# A snapshot from before the exclude file holds an empty stand-in where
# the live database was masked.
rm -rf /root/craft && mkdir -p /root/craft/standin/sqlite/data /root/craft/standin/files/data
printf '{"sqlite":["data/shop.db"],"files":["."]}' >/root/craft/standin/plan.json
sqlite3 /root/craft/standin/sqlite/data/shop.db "create table orders(n); insert into orders values (41),(42);" >/dev/null
: >/root/craft/standin/files/data/shop.db
: >/root/craft/standin/files/data/shop.db-wal
chmod 0000 /root/craft/standin/files/data/shop.db /root/craft/standin/files/data/shop.db-wal
echo receipt-then >/root/craft/standin/files/r.txt
id=$(craft shop /root/craft/standin)
if [ -n "$id" ] && restore shop --snapshot "$id" --yes --no-pre-backup; then pass "the restore exits 0"; else fail "restoring a snapshot with a stand-in: $(cat "$OUT")"; fi
[ "$(rows "$SHOP/data/shop.db" 'select count(*), sum(n) from orders')" = "ok 2|83 " ] && pass "the live database holds the copy's rows: the empty stand-in under files/ was not installed over it" || fail "shop's database: $(rows "$SHOP/data/shop.db" 'select count(*), sum(n) from orders'); $(ls -la "$SHOP/data")"
[ "$(cat "$SHOP/r.txt")" = receipt-then ] && pass "and the files beside it were restored" || fail "r.txt: $(cat "$SHOP/r.txt")"
[ "$(find "$SHOP" -perm 0000 | wc -l)" = 0 ] && pass "no mode-0 stand-in was installed anywhere" || fail "mode-0 files in shop's data: $(find "$SHOP" -perm 0000)"
[ -n "$id" ] && forget "$id"
nothing_left_of_a_restore "a stand-in under files/"

echo "=== restore 8: a FIFO is listed and left out; a symlink comes back ==="
as_app sh -c "cd $BLOG/uploads && mkfifo trap.fifo && ln -s ../a.png here && ln -s /etc/passwd abs"
run && pass "a run with a FIFO and links inside a declared directory exits 0" || fail "the run: $(cat "$OUT")"
special=$(newest blog)
rr ls --no-lock "$special" 2>/dev/null | grep -q '/backup/blog/files/uploads/trap.fifo' && pass "fixture: the snapshot holds the FIFO" || fail "fixture: the FIFO is not in the snapshot: the scenario proves nothing"
as_app sh -c "cd $BLOG/uploads && rm -f trap.fifo here abs a.png"
if restore blog --snapshot "$special" --yes --no-pre-backup; then pass "the restore exits 0"; else fail "the restore failed: $(cat "$OUT")"; fi
[ "$(cat "$BLOG/uploads/a.png" 2>/dev/null)" = img ] && pass "the regular file beside them is restored" || fail "uploads/a.png was not restored"
[ "$(readlink "$BLOG/uploads/here")" = ../a.png ] && [ "$(readlink "$BLOG/uploads/abs")" = /etc/passwd ] && pass "a symbolic link comes back as the same link, its text not followed" || fail "the links: $(ls -la "$BLOG/uploads/here" "$BLOG/uploads/abs" 2>&1)"
[ ! -e "$BLOG/uploads/trap.fifo" ] && grep -q "uploads/trap.fifo" "$OUT" && pass "the FIFO is not installed, and is listed" || fail "trap.fifo: $(ls -la "$BLOG/uploads/trap.fifo" 2>&1); $(cat "$OUT")"
as_app rm -f "$BLOG/uploads/here" "$BLOG/uploads/abs"
nothing_left_of_a_restore "a FIFO and links"

echo "=== restore 8b: a file a backup read with a capability comes back ==="
# A backup reads any file through its capability; a restore is the file's
# owner and no more, and reads the fetched copy — closed to its owner or
# not — so a mode-0 file comes back, with the mode it had.
as_app sh -c "cd $BLOG/uploads && echo secret >zero && chmod 0000 zero"
run && pass "a run with a file closed to its own owner exits 0" || fail "the run: $(cat "$OUT")"
closed=$(newest blog)
as_app rm -f "$BLOG/uploads/zero"
if restore blog --snapshot "$closed" --yes --no-pre-backup; then pass "the restore exits 0"; else fail "a mode-0 file: $(cat "$OUT")"; fi
[ "$(as_app cat "$BLOG/uploads/zero" 2>/dev/null; true)" = "" ] && [ "$(cat "$BLOG/uploads/zero")" = secret ] && [ "$(stat -c %a "$BLOG/uploads/zero")" = 0 ] && pass "it is back, with its contents and its mode 0" || fail "uploads/zero: $(stat -c %a "$BLOG/uploads/zero" 2>&1): $(cat "$BLOG/uploads/zero" 2>&1)"
as_app rm -f "$BLOG/uploads/zero"
nothing_left_of_a_restore "a file closed to its owner"
forget "$closed"

echo "=== restore 9: an app cannot aim its restore at a sibling's data ==="
# Both apps run as one uid, so the unit that installs could write shop's
# files: where it writes is never for a link the app made to say.
shop0=$(shop_print)
as_app sh -c "cd $BLOG && mv uploads uploads.real && ln -s ../../shop/shared uploads"
if restore blog --snapshot "$first_blog" --yes --no-pre-backup; then fail "a restore through a link to shop's data exited 0"; else pass "with blog's uploads a link to shop's data, the restore exits non-zero"; fi
grep -qi "symbolic link" "$OUT" && pass "and says a symbolic link is in the way" || fail "a link in the way: $(cat "$OUT")"
unchanged "$shop0" "$(shop_print)" "shop's data is unchanged"
[ ! -e "$SHOP/a.png" ] && pass "blog's file did not land in shop's data" || fail "a.png is in shop's data"
as_app sh -c "cd $BLOG && rm uploads && mv uploads.real uploads"
# The same, one level down: the file to be overwritten is a link.
as_app sh -c "cd $BLOG/uploads && rm -f a.png && ln -s ../../../shop/shared/r.txt a.png"
if restore blog --snapshot "$first_blog" --yes --no-pre-backup; then pass "with a file to be overwritten a link to shop's file, the restore exits 0"; else fail "a restore over a file that is a link: $(cat "$OUT")"; fi
[ ! -L "$BLOG/uploads/a.png" ] && [ "$(cat "$BLOG/uploads/a.png")" = img ] && pass "the link is replaced by the snapshot's file" || fail "uploads/a.png: $(ls -la "$BLOG/uploads/a.png")"
[ "$(cat "$SHOP/r.txt")" = receipt-then ] && pass "and shop's file was not written through it" || fail "shop's r.txt now holds: $(cat "$SHOP/r.txt")"
unchanged "$shop0" "$(shop_print)" "shop's data is unchanged"
as_app sh -c "cd $BLOG/uploads && rm -f a.png && echo img >a.png"
# And the database: the live database's name is a link to shop's.
as_app sh -c "cd $BLOG && sqlite3 -cmd '.timeout 5000' app.db 'pragma wal_checkpoint(truncate)' >/dev/null && mv app.db app.db.real && ln -s ../../shop/shared/data/shop.db app.db"
if restore blog --snapshot "$first_blog" --yes --no-pre-backup; then fail "a restore into a database that is a link exited 0"; else grep -qi "symbolic link" "$OUT" && grep -q "app.db" "$OUT" && pass "with blog's database a link to shop's, the restore exits non-zero and says so" || fail "a database that is a link: $(cat "$OUT")"; fi
[ "$(rows "$SHOP/data/shop.db" 'select count(*), sum(n) from orders')" = "ok 2|83 " ] && pass "shop's database was not restored over" || fail "shop's database: $(rows "$SHOP/data/shop.db" 'select count(*), sum(n) from orders')"
as_app sh -c "cd $BLOG && rm app.db && mv app.db.real app.db"
nothing_left_of_a_restore "links in the place restored into"

echo "=== restore 10: the live database is the damaged thing ==="
as_app sh -c "cd $BLOG && sqlite3 -cmd '.timeout 5000' app.db 'pragma wal_checkpoint(truncate); insert into posts select hex(randomblob(3000)) from posts, posts, posts;' >/dev/null"
as_app sh -c "cd $BLOG && sqlite3 app.db 'pragma wal_checkpoint(truncate)' >/dev/null && dd if=/dev/urandom of=app.db bs=1 seek=4200 count=6000 conv=notrunc 2>/dev/null"
[ "$(as_app sqlite3 "$BLOG/app.db" 'pragma integrity_check' 2>&1 | head -1)" != ok ] && pass "fixture: blog's live database is damaged" || fail "fixture: blog's live database is still intact: the scenario proves nothing"
if restore blog --snapshot "$first_blog" --yes; then fail "a restore whose backup-first failed went on without being told to"; else pass "when the backup it makes first fails, a restore stops"; fi
grep -q -e '--no-pre-backup' "$OUT" && pass "and names --no-pre-backup" || fail "a failed backup-first: $(cat "$OUT")"
[ "$(as_app sqlite3 "$BLOG/app.db" 'pragma integrity_check' 2>&1 | head -1)" != ok ] && pass "having changed nothing" || fail "the live database was restored over although the restore said it stopped"
if restore blog --snapshot "$first_blog" --yes --no-pre-backup; then pass "told to skip it, the restore exits 0"; else fail "restore --no-pre-backup over a damaged database: $(cat "$OUT")"; fi
[ "$(rows "$BLOG/app.db" 'select count(*) from posts')" = "ok 2 " ] && pass "and the database is the snapshot's, intact" || fail "blog's database: $(rows "$BLOG/app.db" 'select count(*) from posts')"
nothing_left_of_a_restore "a damaged live database"

echo "=== restore 11: to a directory ==="
blog0=$(blog_print)
before=$(snapshots)
mkdir -p /srv/exists
if restore blog --snapshot "$first_blog" --to /srv/exists; then fail "--to a directory that exists exited 0"; else grep -qi "exists" "$OUT" && pass "--to a directory that exists is refused" || fail "--to an existing directory: $(cat "$OUT")"; fi
rm -rf /srv/out
if restore blog --snapshot "$first_blog" --to /srv/out; then pass "--to a new directory exits 0, asking nothing"; else fail "--to: $(cat "$OUT")"; fi
[ "$(rows /srv/out/app.db 'select count(*) from posts')" = "ok 2 " ] && [ "$(cat /srv/out/uploads/a.png 2>/dev/null)" = img ] && pass "it holds the app's data laid out as its shared dir is" || fail "/srv/out: $(find /srv/out | head -10)"
[ "$(find /srv/out ! -user hotserve | wc -l)" = 0 ] && pass "all of it the app's user's" || fail "in /srv/out: $(find /srv/out ! -user hotserve -printf '%p (%u) ')"
unchanged "$blog0" "$(blog_print)" "the live data is unchanged"
[ "$(snapshots)" = "$before" ] && pass "and no backup was made first: nothing was to be overwritten" || fail "snapshots went from $before to $(snapshots)"
rm -rf /srv/out /srv/exists
nothing_left_of_a_restore "--to a directory"

echo "=== restore 12: the repository refuses ==="
write_env "$REPO" not-the-password
if restore blog --yes; then fail "a restore with the wrong password exited 0"; else grep -q "the repository password is wrong (exit 12)" "$OUT" && pass "the wrong password is called the wrong password" || fail "wrong password: $(cat "$OUT")"; fi
p_blog=$(proven blog)
if drill; then fail "a drill with the wrong password exited 0"; else pass "a drill with the wrong password exits non-zero"; fi
[ "$(proven blog)" = "$p_blog" ] && [ -n "$p_blog" ] && pass "and what was last proven is still in the record" || fail "blog's record after a drill that could not reach the repository: $(app_json blog)"
write_env "$REPO/nothing-here" "$PASSWORD"
if restore blog --yes; then fail "a restore with no repository exited 0"; else grep -q "there is no repository at the configured location (exit 10)" "$OUT" && pass "no repository is called no repository" || fail "no repository: $(cat "$OUT")"; fi
rr cat config --no-lock >/dev/null 2>&1
[ $? = 10 ] && pass "and a restore did not create one" || fail "restic no longer says 'no repository' there"
write_env "$REPO" "$PASSWORD"
unchanged "$blog0" "$(blog_print)" "none of which changed anything"
nothing_left_of_a_restore "a repository that refuses"

echo "=== restore 13: a restore that is killed, and one that is interrupted ==="
as_app sh -c "head -c 80000000 /dev/urandom >$BLOG/uploads/big.bin"
run && pass "a run with a large file exits 0" || fail "the run: $(cat "$OUT")"
big=$(newest blog)
as_app sh -c "echo after >$BLOG/uploads/big.bin"
blog0=$(blog_print)
hotserve-backup restore blog --snapshot "$big" --yes --no-pre-backup >/root/first.out 2>&1 &
first=$!
if hold_restic 'restic restore'; then
	pass "a restore is under way, its restic held still"
	run && fail "a backup run exited 0 while a restore was under way" || { grep -q "in progress: pid $first" "$OUT" && pass "a backup run refuses meanwhile, and says who holds the lock" || fail "the run during a restore: $(cat "$OUT")"; }
	kill -KILL "$first"
	wait "$first" 2>/dev/null
	left=$(systemctl list-units --plain --no-legend --state=activating 'hotserve_backup_*' | awk '{print $1}')
	[ -n "$left" ] && pass "the killed restore left its unit running ($left)" || fail "no unit was left running to sweep: the scenario proved nothing"
	unchanged "$blog0" "$(blog_print)" "the killed restore had changed nothing"
	run && pass "the next run exits 0" || fail "the run after a killed restore: $(cat "$OUT")"
	systemctl list-units --all --plain --no-legend $left | grep -q . && fail "$left is still there" || pass "and stopped the unit the killed restore left"
	nothing_left_of_a_restore "after a killed restore"
else
	fail "no 'restic restore' appeared to hold still"
fi
hotserve-backup restore blog --snapshot "$big" --yes --no-pre-backup >/root/first.out 2>&1 &
first=$!
if hold_restic 'restic restore'; then
	kill -INT "$first"
	i=0
	while kill -0 "$first" 2>/dev/null && [ "$i" -lt 150 ]; do
		i=$((i + 1))
		sleep 1
	done
	if kill -0 "$first" 2>/dev/null; then
		fail "150s after Ctrl-C the restore is still there"
		kill -KILL "$first"
	else
		wait "$first"
		[ $? != 0 ] && pass "interrupted, a restore exits non-zero (after ${i}s)" || fail "an interrupted restore exited 0"
	fi
	[ "$(units_running)" = 0 ] && pass "having stopped its own unit" || fail "units still running after Ctrl-C: $(systemctl list-units --plain --no-legend 'hotserve_backup_*')"
	pgrep -x restic >/dev/null && fail "restic is still running" || pass "restic is gone"
	unchanged "$blog0" "$(blog_print)" "and changed nothing"
	nothing_left_of_a_restore "after Ctrl-C, with no later run to tidy up"
else
	fail "no 'restic restore' appeared to hold still"
fi
echo "=== restore 13b: a fetch that cannot fit is not begun ==="
# A fetch is the whole app, in plaintext, under the state dir. Where
# there is no room for it, that is said before anything is fetched — not
# found out by filling the disk the apps write to.
# A drill takes the newest snapshot, so the newest has to be the large one.
as_app sh -c "head -c 80000000 /dev/urandom >$BLOG/uploads/big.bin"
run || fail "fixture: the run: $(cat "$OUT")"
big=$(newest blog)
size=$(rr stats --no-lock --json --mode restore-size "$big" 2>/dev/null | sed -n 's/.*"total_size":\([0-9]*\).*/\1/p')
[ "${size:-0}" -gt 8388608 ] && pass "fixture: blog's newest snapshot is ${size} bytes restored, more than the 8 MiB there will be" || fail "fixture: blog's newest snapshot is ${size:-unknown} bytes: the scenario proves nothing"
mount -t tmpfs -o size=8m,mode=0700 tmpfs "$RESTAGING" || fail "fixture: no small filesystem could be put under $RESTAGING"
fetches() { journalctl --sync >/dev/null 2>&1; journalctl --no-pager -o cat | grep -c 'Starting hotserve_backup_fetch_'; }
before=$(fetches)
rm -rf /srv/out
if restore blog --snapshot "$big" --to /srv/out; then fail "a restore with no room for the fetch exited 0"; else grep -q "no room" "$OUT" && grep -q "MiB" "$OUT" && pass "with no room for it, a restore stops and says how much it needs and how much there is" || fail "no room: $(cat "$OUT")"; fi
[ "$(fetches)" = "$before" ] && pass "and no fetch was begun" || fail "a fetch was begun all the same ($before -> $(fetches) fetch units)"
[ ! -e /srv/out ] && pass "nor is the --to directory left behind" || fail "/srv/out was left behind"
if drill; then fail "a drill with no room for blog exited 0"; else grep -q "^blog: restore not proven: .*no room" "$OUT" && grep -q "^shop: restore proven" "$OUT" && pass "a drill says the same of blog, and still proves shop, which fits" || fail "the drill: $(cat "$OUT")"; fi
umount "$RESTAGING" || fail "fixture: $RESTAGING could not be unmounted"
drill && pass "with room again, the drill proves blog" || fail "the drill after: $(cat "$OUT")"
nothing_left_of_a_restore "no room"

echo "=== restore 13c: an install writes the app a second time, on the same disk ==="
# The fetch and the install both land on one filesystem, as on the
# usual box: the peak is twice the snapshot, and a restore that fits the
# fetch alone would fill the disk the apps write to half way through.
mkdir -p /srv/small && mount -t tmpfs -o size=130m,mode=0755 tmpfs /srv/small || fail "fixture: no small filesystem at /srv/small"
mkdir -p /srv/small/fetch && chmod 0700 /srv/small/fetch && mount --bind /srv/small/fetch "$RESTAGING" || fail "fixture: $RESTAGING could not be put on the small filesystem"
[ "$(stat -f -c %i /srv/small)" = "$(stat -f -c %i "$RESTAGING")" ] && pass "fixture: what is fetched and where it is restored to share one 130 MiB filesystem, for an 80 MiB snapshot" || fail "fixture: not one filesystem"
before=$(fetches)
if restore blog --snapshot "$big" --to /srv/small/out; then fail "a restore whose fetch fits but whose install would not exited 0"; else grep -q "no room" "$OUT" && grep -qi "twice" "$OUT" && pass "a restore that fits once and not twice on one filesystem is refused, and says why" || fail "fits once, not twice: $(cat "$OUT")"; fi
[ "$(fetches)" = "$before" ] && pass "and no fetch was begun" || fail "a fetch was begun all the same"
if drill; then pass "a drill, which installs nothing, needs the fetch alone, and proves blog there" || fail "the drill on the small filesystem: $(cat "$OUT")"; fi
mount -o remount,size=200m /srv/small || fail "fixture: /srv/small could not be grown"
rm -rf /srv/small/out
if restore blog --snapshot "$big" --to /srv/small/out; then pass "with room for both, the same restore exits 0" || fail "with room for both: $(cat "$OUT")"; fi
umount "$RESTAGING"; umount /srv/small || fail "fixture: /srv/small could not be unmounted"
nothing_left_of_a_restore "fits once, not twice"

echo "=== restore 13d: Ctrl-C while the install waits on the live database ==="
# The interrupt lands inside the unit that changes things — held there
# by a writer that has the database, in rollback-journal mode, where a
# restore waits for it. The unit is stopped with the restore, the
# database is as it was and intact, and the restore says what it leaves.
as_app sh -c "cd $BLOG && sqlite3 -cmd '.timeout 5000' app.db 'pragma journal_mode=delete; delete from posts; insert into posts values (55);'" >/dev/null
blog0=$(blog_print)
as_app sh -c "cd $BLOG && sqlite3 app.db 'begin exclusive; select 1;' '.shell sleep 60' 'commit;'" >/dev/null 2>&1 &
holder=$!
sleep 1
hotserve-backup restore blog --snapshot "$first_blog" --yes --no-pre-backup >/root/first.out 2>&1 &
first=$!
i=0
until [ "$(systemctl list-units --plain --no-legend --state=activating 'hotserve_backup_install_*' | wc -l)" != 0 ]; do
	i=$((i + 1))
	[ "$i" -ge 600 ] && break
	sleep 0.1
done
if [ "$(systemctl list-units --plain --no-legend --state=activating 'hotserve_backup_install_*' | wc -l)" != 0 ]; then
	pass "the install unit is running, waiting on the database"
	sleep 2
	kill -INT "$first"
	wait "$first"
	[ $? != 0 ] && grep -q "partly restored" /root/first.out && pass "interrupted there, the restore exits non-zero and says the data may be partly restored" || fail "interrupted in the install: $(cat /root/first.out)"
	[ "$(units_running)" = 0 ] && pass "its unit was stopped with it, not left writing with nobody watching" || fail "units still running: $(systemctl list-units --plain --no-legend 'hotserve_backup_*')"
else
	fail "the install unit never started: $(cat /root/first.out)"
	kill -KILL "$first" 2>/dev/null
fi
pkill -f "sleep 60" 2>/dev/null; wait "$holder" 2>/dev/null
unchanged "$blog0" "$(blog_print)" "the database is as it was — one transaction, not begun — and intact"
nothing_left_of_a_restore "an interrupt in the install"
as_app sh -c "cd $BLOG && sqlite3 -cmd '.timeout 5000' app.db 'pragma journal_mode=wal;'" >/dev/null

as_app rm -f "$BLOG/uploads/big.bin"
run || fail "the run after the interrupted restore: $(cat "$OUT")"

echo "=== restore 14: a drill proves, or says what it could not ==="
blog0=$(blog_print)
shop0=$(shop_print)
if drill; then pass "a drill of sound snapshots exits 0"; else fail "the drill: $(cat "$OUT")"; fi
[ -n "$(proven blog)" ] && [ "$(proven blog)" = "$(newest blog)" ] && [ "$(proven shop)" = "$(newest shop)" ] && pass "the record names the newest snapshot of each app as proven" || fail "after a drill: blog $(app_json blog) shop $(app_json shop)"
unchanged "$blog0" "$(blog_print)" "a drill changes nothing of blog's"
unchanged "$shop0" "$(shop_print)" "nor of shop's"
nothing_left_of_a_restore "a drill"
good=$(proven blog)
rm -rf /root/craft && mkdir -p /root/craft/rot/sqlite /root/craft/rot/files/uploads
printf '{"sqlite":["app.db"],"files":["uploads"]}' >/root/craft/rot/plan.json
echo crafted >/root/craft/rot/files/uploads/crafted.png
sqlite3 /root/craft/rot/sqlite/app.db "create table posts(n); insert into posts select hex(randomblob(3000)) from (select 1 union select 2 union select 3), (select 1 union select 2 union select 3);" >/dev/null
dd if=/dev/urandom of=/root/craft/rot/sqlite/app.db bs=1 seek=100 count=9000 conv=notrunc 2>/dev/null
id=$(craft blog /root/craft/rot)
[ -n "$id" ] && [ "$(newest blog)" = "$id" ] || fail "fixture: the damaged snapshot is not blog's newest: the scenario proves nothing"
if drill; then fail "a drill whose newest snapshot of blog is damaged exited 0"; else pass "with blog's newest snapshot damaged, a drill exits non-zero"; fi
grep -q "^blog: .*not proven" "$OUT" && pass "and says blog's restore is not proven" || fail "the drill's own words: $(cat "$OUT")"
[ -n "$good" ] && [ "$(proven blog)" = "$good" ] && pass "the record still names the snapshot that was last proven, not the damaged one" || fail "blog's record: $(app_json blog)"
app_json blog | grep -q "\"restore_drill\": {[^}]*$(echo "$id" | cut -c1-8)" && pass "and says which snapshot failed its drill" || fail "blog's record says nothing of the failed drill: $(app_json blog)"
[ -n "$(proven shop)" ] && [ "$(proven shop)" = "$(newest shop)" ] && pass "shop is proven all the same" || fail "shop's record: $(app_json shop)"
unchanged "$blog0" "$(blog_print)" "and nothing of blog's changed"
[ -n "$id" ] && forget "$id"
nothing_left_of_a_restore "a drill of a damaged snapshot"

echo "=== restore 15: a rebuilt box: no record, no data, another uid for the app's user ==="
# Last, because it renumbers the account everything above ran as.
old_uid=$(id -u hotserve)
old_gid=$(id -g hotserve)
rebuilt=$(newest blog)
loginctl disable-linger hotserve 2>/dev/null
systemctl stop "user@$old_uid.service" 2>/dev/null
err=$(groupmod -g 4242 hotserve 2>&1 && usermod -u 4242 -g 4242 hotserve 2>&1) || fail "fixture: the hotserve user could not be renumbered: $err"
find / -xdev \( -uid "$old_uid" -o -gid "$old_gid" \) -exec chown -h hotserve:hotserve {} + 2>/dev/null
[ "$(id -u hotserve)" = 4242 ] && pass "fixture: hotserve is now uid 4242, not $old_uid as when the snapshot was made" || fail "fixture: hotserve is uid $(id -u hotserve)"
rm -rf /var/lib/liveswap/blog "$STATUS"
# A restore that makes the shared dir and then fails takes it away
# again: an hourly run would otherwise take an empty directory for the
# app's data, where before it knew the data was missing.
mount -t tmpfs -o size=1m,mode=0700 tmpfs "$RESTAGING" || fail "fixture: no small filesystem under $RESTAGING"
made() { journalctl --sync >/dev/null 2>&1; journalctl --no-pager -o cat | grep -c 'Starting hotserve_backup_mkshared_'; }
before=$(made)
restore blog --yes && fail "a restore with no room for its fetch exited 0" || true
[ "$(made)" != "$before" ] && pass "fixture: the restore made the shared dir before it failed" || fail "fixture: the shared dir was never made, so the scenario proves nothing: $(cat "$OUT")"
[ ! -e /var/lib/liveswap/blog ] && pass "a restore that was confirmed and then failed leaves no empty data dir behind" || fail "left behind: $(find /var/lib/liveswap/blog | tr '\n' ' ')"
umount "$RESTAGING"
if restore blog --yes; then pass "with no record and no data dir, a restore exits 0"; else fail "a restore on a rebuilt box: $(cat "$OUT")"; fi
grep -q "snapshot $(echo "$rebuilt" | cut -c1-8)" "$OUT" && pass "it found the newest snapshot in the repository alone" || fail "the restore's own words: $(cat "$OUT")"
[ "$(rows "$BLOG/app.db" 'select count(*) from posts')" = "ok 2 " ] && [ "$(cat "$BLOG/uploads/a.png" 2>/dev/null)" = img ] && pass "the data is back" || fail "blog's data: $(find /var/lib/liveswap/blog | head -10)"
[ -d "$BLOG" ] && [ "$(find /var/lib/liveswap/blog ! -uid 4242 | wc -l)" = 0 ] && pass "all of it owned by hotserve as this box numbers it: by name, not by the uid in the snapshot" || fail "not hotserve's: $(find /var/lib/liveswap/blog ! -uid 4242 -printf '%p (%U) ')"
case "$(stat -c %a "$BLOG" 2>/dev/null)" in *0) pass "the shared dir it made is closed to other users ($(stat -c %a "$BLOG"))" ;; *) fail "the shared dir it made is mode $(stat -c %a "$BLOG")" ;; esac
run && pass "and the next backup run exits 0" || fail "the run after a restore on a rebuilt box: $(cat "$OUT")"
nothing_left_of_a_restore "a rebuilt box"

journalctl --sync >/dev/null 2>&1
journalctl --no-pager | grep -q -e "$PASSWORD" -e e2e-fixture-key-not-a-secret && fail "a credential is in the journal" || pass "after all of it, no credential is in the journal"

echo ""
if [ "$FAILURES" -eq 0 ]; then
	echo "ALL RESTORE SCENARIOS PASSED"
	exit 0
fi
echo "$FAILURES RESTORE ASSERTION(S) FAILED"
exit 1
