#!/bin/sh
# The setup suite. Runs inside e2e-backup-box as root, first of the four,
# against real systemd, Debian's restic and the S3 server at e2e-s3. It
# holds `hotserve-backup setup <repository>` to its word: everything
# knowable is refused before a secret is asked for; the secrets are
# typed at a terminal, the secret ones with echo off; a new repository's
# password is shown, and confirmed stored, before it takes effect
# anywhere; a mistake over a working setup leaves the working file byte
# for byte; and the account restic runs as is made here, by setup — the
# image does not make it.
#
# Until the command exists it exits 2 with the usage text, so no check
# below is content with a non-zero exit alone.
. /lib.sh
. /lib-backup.sh

S3=s3:http://e2e-s3:9000
REPO=$S3/setuprepo
KEYID=AKIDE2EFIXTURE
SECRET=e2e-fixture-key-not-a-secret
ETC=$(dirname "$ENVFILE")
OLD=/etc/hotserve/backup.env

wait_for_systemd
cp "$CADDYFILE" /root/Caddyfile.base
seed

# at_tty <cmd...>: the command at a real terminal, script(1)'s, typing
# what is on stdin — for a command that asks nothing.
at_tty() { script -qec "$*" /dev/null; }
# converse <cmd> [<prompt> <answer>]...: the command at a real terminal,
# each answer typed once its prompt is the last thing on the screen —
# a pty echoes what arrives before echo is off, and a loaded runner
# takes its time to a prompt — and then once the screen has moved on,
# so that the same prompt asked again is waited for again. Output in
# $OUT; exit status the command's.
converse() {
	cmd=$1
	shift
	rm -f /root/in
	mkfifo /root/in
	script -qec "$cmd" /dev/null </root/in >"$OUT" 2>&1 &
	cv=$!
	exec 3>/root/in
	while [ $# -ge 2 ]; do
		p=$1
		a=$2
		shift 2
		i=0
		until [ "$(tail -c "${#p}" "$OUT" 2>/dev/null)" = "$p" ] || ! kill -0 "$cv" 2>/dev/null || [ "$i" -ge 600 ]; do
			i=$((i + 1))
			sleep 0.1
		done
		kill -0 "$cv" 2>/dev/null || break
		printf '%s\n' "$a" >&3
		i=0
		while [ "$(tail -c "${#p}" "$OUT" 2>/dev/null)" = "$p" ] && kill -0 "$cv" 2>/dev/null && [ "$i" -lt 100 ]; do
			i=$((i + 1))
			sleep 0.1
		done
	done
	exec 3>&-
	wait "$cv"
}
P_KEY="Storage key id (AWS_ACCESS_KEY_ID): "
P_SECRET="Storage secret key (AWS_SECRET_ACCESS_KEY): "
P_STORED="Type stored to go on: "
P_PW="Repository password: "
setup() { hotserve-backup setup "$@" >"$OUT" 2>&1; }
says() { grep -q -e "$1" "$OUT"; }
sum() { sha256sum "$ENVFILE" 2>/dev/null | cut -d' ' -f1; }
temps() { ls -A "$ETC" 2>/dev/null | grep -c '^\.\{0,1\}repository\.env\.[0-9a-f]\{12\}'; }
shown_password() { grep -o 'Repository password (new): [a-z2-7]\{52\}' "$1" | cut -d' ' -f4; }
as_nobody() { setpriv --reuid nobody --regid nogroup --clear-groups "$@"; }
until_units() { # <n> <pattern>: waits up to a minute for that many units of the pattern to be running
	i=0
	while [ "$(systemctl list-units --plain --no-legend --state=active,activating "$2" | wc -l)" != "$1" ] && [ "$i" -lt 600 ]; do
		i=$((i + 1))
		sleep 0.1
	done
}

echo "=== setup 0: the usage text ==="
setup && fail "setup with no repository exited 0" || { says "^usage: hotserve-backup setup <repository>" && pass "setup with no repository is the usage text" || fail "said: $(cat "$OUT")"; }
setup a b && fail "setup with two arguments exited 0" || { says "^usage:" && pass "and so is setup with two" || fail "said: $(cat "$OUT")"; }

echo "=== setup 1: not root, no terminal: refused before anything is touched ==="
as_nobody hotserve-backup setup "$REPO" >"$OUT" 2>&1 && fail "setup as nobody exited 0" || { says "needs root: sudo hotserve-backup setup <repository>" && pass "setup as nobody names sudo, and the argument" || fail "as nobody: $(cat "$OUT")"; }
setup "$REPO" </dev/null && fail "setup with no terminal exited 0" || { says "asks for secrets at a terminal, and there is none here; from a provisioning tool, write $ENVFILE by hand" && ! says "^usage" && ! says "no such device" && pass "setup with no terminal says so, and where to go" || fail "no terminal: $(cat "$OUT")"; }
[ ! -e "$ETC" ] && pass "and nothing was made under /etc" || fail "$ETC exists: $(ls -la "$ETC")"
[ "$(units_left)" = 0 ] && pass "and no unit was started" || fail "units: $(systemctl list-units --all --no-legend 'hotserve_backup_*')"

echo "=== setup 2: what is knowable is refused before any prompt, and before the account is made ==="
id hotserve-backup >/dev/null 2>&1 && fail "fixture: the hotserve-backup account exists before setup ran, so nothing below can show setup making it" || pass "fixture: no hotserve-backup account before setup"
mv /usr/bin/restic /usr/bin/restic.aside
printf 'x\n' | at_tty hotserve-backup setup "$REPO" >"$OUT" 2>&1 && fail "setup without restic exited 0" || { says "restic is not installed at /usr/bin/restic: apt install restic" && pass "without restic, setup names the package" || fail "without restic: $(cat "$OUT")"; }
says "Storage key id" && fail "a prompt was asked before the preflight passed" || pass "and asked nothing"
id hotserve-backup >/dev/null 2>&1 && fail "the account was made before the preflight passed" || pass "and made no account"
mv /usr/bin/restic.aside /usr/bin/restic
for r in sftp:user@host:/srv/backups /srv/backups local:/srv/backups rclone:remote:bucket azure:container:path gs:bucket:path swift:container:/path "s3:http://user:pass@e2e-s3:9000/box" "s3:user:pass@e2e-s3:9000/box" "ftp://host/x" "s3:"; do
	if printf 'x\n' | at_tty hotserve-backup setup "$r" >"$OUT" 2>&1; then
		fail "setup $r exited 0"
	elif says "Storage key id"; then
		fail "setup $r asked before refusing"
	elif says "^usage"; then
		fail "setup $r: the usage text"
	else
		pass "refused before any prompt: $r — $(grep hotserve-backup: "$OUT" | head -1 | cut -c1-90)"
	fi
done
[ ! -e /srv/backups ] && pass "nothing was made at the refused path" || fail "/srv/backups exists"
[ ! -e "$ETC" ] && pass "and nothing under /etc" || fail "$ETC exists"

echo "=== setup 3: a fresh setup at a terminal ==="
t0=$(date +%s)
converse "hotserve-backup setup $REPO" "$P_KEY" "$KEYID" "$P_SECRET" "$SECRET" "$P_STORED" stored
rc=$?
took=$(($(date +%s) - t0))
[ "$rc" = 0 ] && says "repository ready: $REPO (new, id [0-9a-f]\{8\})" && pass "setup made the repository (${took}s)" || fail "setup: exit $rc after ${took}s: $(cat "$OUT")"
if ! says "$KEYID"; then
	fail "the key id did not show as it was typed, so the echo check proves nothing: $(cat "$OUT")"
elif ! says "Storage secret key"; then
	fail "the secret was never asked for, so the echo check proves nothing: $(cat "$OUT")"
elif says "$SECRET"; then
	fail "the secret was shown as it was typed"
else
	pass "the key id showed as it was typed; the secret did not"
fi
pw=$(shown_password "$OUT")
[ -n "$pw" ] && pass "a password was shown" || fail "no password was shown: $(cat "$OUT")"
says "Store it off the box now, with the repository URL, $REPO, and the storage key" && says "Type stored to go on" && pass "and had to be confirmed stored, with what to store beside it" || fail "the stored step: $(cat "$OUT")"
says "account hotserve-backup: made" && getent passwd hotserve-backup | grep -q ':/nonexistent:/usr/sbin/nologin$' && pass "the account was made: no home, no shell" || fail "the account: $(getent passwd hotserve-backup) — $(grep account "$OUT")"
[ "$(stat -c '%U %a' "$ETC")" = "root 755" ] && [ "$(stat -c '%U %a' "$ENVFILE")" = "root 600" ] && pass "the directory is root's and open to look into; the file is root's alone" || fail "$ETC: $(stat -c '%U %a' "$ETC"), $ENVFILE: $(stat -c '%U %a' "$ENVFILE")"
[ "$(grep -c '^' "$ENVFILE")" = 4 ] && grep -q "^RESTIC_REPOSITORY=$REPO\$" "$ENVFILE" && grep -q "^RESTIC_PASSWORD=$pw\$" "$ENVFILE" && grep -q "^AWS_ACCESS_KEY_ID=$KEYID\$" "$ENVFILE" && grep -q "^AWS_SECRET_ACCESS_KEY=$SECRET\$" "$ENVFILE" && pass "the file holds the four settings, the password among them, and nothing else" || fail "the file: $(sed 's/PASSWORD=.*/PASSWORD=…/' "$ENVFILE")"
[ "$(temps)" = 0 ] && pass "nothing was left beside it" || fail "left beside it: $(ls "$ETC")"
[ "$(units_left)" = 0 ] && pass "no unit is left" || fail "units: $(systemctl list-units --all --no-legend 'hotserve_backup_*')"
rr cat config --no-lock >/dev/null 2>&1 && pass "the repository answers the credential the file holds" || fail "the repository does not answer: $(rr cat config --no-lock 2>&1)"
says "a run would back up blog, notyet, shop, under /var/lib/liveswap" && pass "the plan was read first, and said" || fail "the plan: $(cat "$OUT")"
says "^looking for a repository at $REPO" && pass "the repository was looked for before a password was made" || fail "the look: $(cat "$OUT")"
says "^next: sudo hotserve-backup run, then hotserve-backup status. Nothing runs it on a schedule on this branch" && pass "and what to do next, and what does not happen by itself" || fail "next: $(cat "$OUT")"
journalctl --sync >/dev/null 2>&1
if ! journalctl --no-pager | grep -q "hotserve backup: init the repository"; then
	fail "the journal does not show the init unit, so it cannot show what is not in it"
elif journalctl --no-pager | grep -q -e "$pw" -e "$SECRET"; then
	fail "a credential is in the journal"
else
	pass "no credential is in the journal"
fi
if hotserve-backup status >"$OUT" 2>&1; then says "pending first run" && pass "status: pending first run, exit 0" || fail "status said: $(cat "$OUT")"; else fail "status after setup exits $?: $(cat "$OUT")"; fi
run && pass "a run works with what setup wrote" || fail "the run: $(cat "$OUT")"
expect_class blog ok "after setup"
expect_class shop ok "after setup"

echo "=== setup 4: a Caddyfile a run could not plan from is refused before any prompt ==="
# shellcheck disable=SC2016 # the Caddyfile's own {$NAME}, not the shell's
sed 's#root /var/lib/liveswap#root {$LIVESWAP_ROOT:/var/lib/liveswap}#' /root/Caddyfile.base >"$CADDYFILE"
hotserve validate --config "$CADDYFILE" --adapter caddyfile >/dev/null 2>&1 && pass "fixture: hotserve accepts the file" || fail "fixture: hotserve rejects the file: the scenario proves nothing"
before=$(sum)
printf 'x\n' | at_tty hotserve-backup setup "$S3/other" >"$OUT" 2>&1 && fail "setup exited 0 with a Caddyfile a run cannot plan from" || { says "could not be turned into a plan (exit 1): .*LIVESWAP_ROOT" && ! says "Storage key id" && pass "refused with the adapter's reason, naming the variable, before any prompt" || fail "said: $(cat "$OUT")"; }
[ "$(sum)" = "$before" ] && pass "the working file is as it was" || fail "the working file changed"
cp /root/Caddyfile.base "$CADDYFILE"

echo "=== setup 5: a file at the old path is named, and not read ==="
mv "$ENVFILE" /root/env.keep
printf 'RESTIC_PASSWORD=old\n' >"$OLD"
chmod 0600 "$OLD"
run && fail "a run with only the old file exited 0" || { says "not set up: $ENVFILE is not there" && says "$OLD is from before this version and is not read: run \`sudo hotserve-backup setup <its RESTIC_REPOSITORY>\`, which asks for its RESTIC_PASSWORD; then remove it" && ! says "lstat" && pass "a run names the new path, and says what to do with the old one" || fail "the run said: $(cat "$OUT")"; }
[ "$(units_left)" = 0 ] && pass "and started nothing" || fail "units: $(systemctl list-units --all --no-legend 'hotserve_backup_*')"
hotserve-backup status >"$OUT" 2>&1
[ $? = 3 ] && says "$OLD is from before this version" && pass "status exits 3 and says the same" || fail "status: $(cat "$OUT")"
# A setup that goes through with the old file still there says so at
# the end: that file is where an administrator's sudoers reaches.
mv /root/env.keep "$ENVFILE"
converse "hotserve-backup setup $REPO" "$P_KEY" "$KEYID" "$P_SECRET" "$SECRET" "$P_PW" "$pw"
[ $? = 0 ] && says "$OLD is still there, from before this version, and is not read; an administrator's sudoers may reach it: remove it (sudo rm $OLD)" && [ -f "$OLD" ] && pass "a setup with the old file still there says to remove it, and does not itself" || fail "the old file after a setup: $(grep "$OLD" "$OUT"; ls -la "$OLD" 2>&1)"
rm -f "$OLD"

echo "=== setup 6: mistakes over a working setup leave it as it was ==="
before=$(sum)
t0=$(date +%s)
converse "hotserve-backup setup $S3/setupwrong" "$P_KEY" "$KEYID" "$P_SECRET" wrong-secret "$P_STORED" stored "$P_KEY" "$KEYID" "$P_SECRET" wrong-secret "$P_KEY" "$KEYID" "$P_SECRET" wrong-secret
rc=$?
took=$(($(date +%s) - t0))
[ "$rc" != 0 ] && says "restic could not make or open the repository (exit 1): .*signature" && [ "$took" -lt 90 ] && pass "a wrong storage key is refused by the storage (${took}s), and said" || fail "wrong key: exit $rc after ${took}s: $(cat "$OUT")"
says "there is no repository at the configured location" && fail "an exit 1 was called 'no repository'" || pass "and not called 'no repository'"
[ "$(grep -c 'the key id and secret again (the password shown above still applies)' "$OUT")" = 2 ] && [ "$(grep -c 'Storage key id' "$OUT")" = 3 ] && pass "the key was asked for again, twice, the one password standing" || fail "the retries: $(grep -c 'Storage key id' "$OUT") askings, $(grep -c 'again' "$OUT") agains"
[ "$(grep -c 'Repository password (new)' "$OUT")" = 1 ] && says "the password shown above was never used: discard it" && pass "one password was shown, and said to be dead at the end" || fail "the dead password: $(grep -c 'Repository password (new)' "$OUT") shown; $(tail -1 "$OUT")"
says "no repository answered within 10s: a bucket not made yet, a wrong key and a wrong host look alike here" && pass "the silent look was explained" || fail "the look: $(grep looking -A1 "$OUT" | head -3)"
# A host that does not resolve: the look retries and is given up on,
# init says so at once [measured], and the key is asked for again.
t0=$(date +%s)
converse "hotserve-backup setup s3:http://nowhere.invalid:9/nothere" "$P_KEY" "$KEYID" "$P_SECRET" "$SECRET" "$P_STORED" stored
rc=$?
took=$(($(date +%s) - t0))
[ "$rc" != 0 ] && says "no such host" && says "the key id and secret again" && [ "$took" -lt 40 ] && pass "a host that does not resolve is said at once after the look (${took}s)" || fail "no such host: exit $rc after ${took}s: $(tail -4 "$OUT")"
[ "$(sum)" = "$before" ] && [ "$(temps)" = 0 ] && [ "$(units_left)" = 0 ] && pass "and left the working setup as it was" || fail "after a bad host: temps=$(temps), units=$(units_left)"
[ "$(sum)" = "$before" ] && [ "$(temps)" = 0 ] && [ "$(units_left)" = 0 ] && pass "the working file is byte-identical, no copy of a credential is left, no unit is running" || fail "after a wrong key: same=$([ "$(sum)" = "$before" ] && echo yes || echo NO), temps=$(temps), units=$(units_left)"
converse "hotserve-backup setup $REPO" "$P_KEY" "$KEYID" "$P_SECRET" "$SECRET" "$P_PW" not-the-password "$P_PW" not-that-one "$P_PW" nor-this
rc=$?
[ "$rc" != 0 ] && says "the repository exists; its password is needed" && says "this password cannot open the repository (exit 12)" && pass "a wrong password for the repository that exists is refused, and said" || fail "wrong password: exit $rc: $(cat "$OUT")"
[ "$(grep -c 'Repository password: ' "$OUT")" = 3 ] && [ "$(grep -c 'cannot open the repository (exit 12): again' "$OUT")" = 2 ] && pass "and asked for again, twice, before giving up" || fail "the password retries: $(grep -c 'Repository password: ' "$OUT") askings"
says "Repository password (new)" && fail "a password was made for a repository that exists" || pass "no password was made for a repository that exists"
[ "$(sum)" = "$before" ] && [ "$(temps)" = 0 ] && [ "$(units_left)" = 0 ] && pass "the working file is byte-identical, no copy of a credential is left, no unit is running" || fail "after a wrong password: temps=$(temps), units=$(units_left)"

echo "=== setup 7: a rebuilt box opens the repository it already has, with its own password ==="
rm -f "$ENVFILE"
converse "hotserve-backup setup $REPO" "$P_KEY" "$KEYID" "$P_SECRET" "$SECRET" "$P_PW" "$pw"
rc=$?
[ "$rc" = 0 ] && says "repository ready: $REPO (existing, id [0-9a-f]\{8\})" && pass "setup opened the existing repository" || fail "rebuilt box: exit $rc: $(cat "$OUT")"
grep -q "^RESTIC_PASSWORD=$pw\$" "$ENVFILE" && pass "the file holds the repository's own password" || fail "the file holds: $(grep PASSWORD "$ENVFILE" | sed 's/=.*/=…/')"
says "$pw" && fail "the repository's password was shown as it was typed" || pass "the repository's password was not shown as it was typed"
says "the repository exists; its password is needed" && ! says "Repository password (new)" && ! says "stored" && pass "a rebuilt box is asked for the password it has, and shown none" || fail "the rebuilt box was shown: $(grep -i password "$OUT")"
rr cat config --no-lock >/dev/null 2>&1 && pass "the repository answers" || fail "the repository does not answer"
# With no file before, the record cannot be tied to this repository: it
# is put aside, and the next run drills what it backs up here.
says "the record was put aside (no credential file was there to tie it to this repository): $STATUS.aside-.*; the next run drills what it backs up" && [ "$(ls "$STATUS".aside-* | wc -l)" = 1 ] && pass "with no file before, the record was put aside, and why was said" || fail "the record: $(ls /var/lib/hotserve-backup) — $(grep aside "$OUT")"
if hotserve-backup status >"$OUT" 2>&1; then says "pending first run" && pass "status: pending first run" || fail "status: $(cat "$OUT")"; else fail "status exits $?: $(cat "$OUT")"; fi
rm -f "$STATUS".aside-*
run && pass "a run makes a new record on it" || fail "the run: $(cat "$OUT")"

echo "=== setup 8: Ctrl-C at the secret's prompt writes nothing, and puts echo back ==="
before=$(sum)
rm -f /root/in
mkfifo /root/in
script -qec "hotserve-backup setup $S3/setupint; stty -a" /dev/null </root/in >/root/int.out 2>&1 &
sp=$!
exec 3>/root/in
printf '%s\n' "$KEYID" >&3
i=0
until grep -q "Storage secret key" /root/int.out || [ "$i" -ge 100 ]; do
	i=$((i + 1))
	sleep 0.1
done
grep -q "Storage secret key" /root/int.out && pass "setup is at the secret's prompt" || fail "setup never asked for the secret: $(cat /root/int.out)"
kill -INT "$(pgrep -x hotserve-backup)" 2>/dev/null
exec 3>&-
wait "$sp"
grep -q ' echo ' /root/int.out && pass "echo is on again after the interrupt" || fail "the terminal after the interrupt: $(grep -o '[-]*echo ' /root/int.out | head -1)"
grep -q "interrupted: nothing has been written" /root/int.out && ! grep -q "context canceled" /root/int.out && pass "the interrupt is said in the operator's words" || fail "after Ctrl-C it said: $(grep hotserve-backup: /root/int.out)"
[ "$(sum)" = "$before" ] && [ "$(temps)" = 0 ] && pass "nothing was written" || fail "after Ctrl-C: temps=$(temps)"

echo "=== setup 9: killed while the repository is being made: the next setup sweeps, and the shown password opens it ==="
OUT=/root/kill.out converse "hotserve-backup setup $S3/setupkill" "$P_KEY" "$KEYID" "$P_SECRET" "$SECRET" "$P_STORED" stored &
sp=$!
if hold_restic "restic init"; then
	kill -KILL "$(pgrep -x hotserve-backup)"
	wait "$sp" 2>/dev/null
	pw1=$(shown_password /root/kill.out)
	[ -n "$pw1" ] && pass "the killed setup had shown its password" || fail "no password in $(cat /root/kill.out)"
	[ "$(temps)" = 1 ] && [ "$(stat -c '%U %a' "$ETC"/repository.env.*)" = "root 600" ] && pass "it left its file beside the working one, root's alone" || fail "left: $(ls -la "$ETC")"
	cp "$ENVFILE" "$ENVFILE.bak"
	[ "$(sum)" = "$before" ] && pass "and the working file as it was" || fail "the working file changed"
	[ "$(units_running)" = 1 ] && pass "and its init unit running" || fail "units running: $(units_running)"
	kill -CONT "$pid"
	until_units 0 'hotserve_backup_*'
	converse "hotserve-backup setup $S3/setupkill" "$P_KEY" "$KEYID" "$P_SECRET" "$SECRET" "$P_PW" "$pw1"
	rc=$?
	says "removed a file an interrupted setup left: $ENVFILE\." && [ "$(temps)" = 0 ] && pass "the next setup removed it, and said so" || fail "the sweep: $(cat "$OUT") — $(ls "$ETC")"
	[ -f "$ENVFILE.bak" ] && ! says "$ENVFILE.bak" && pass "and left the operator's own copy beside it alone" || fail "the operator's copy: $(ls "$ETC") — $(grep bak "$OUT")"
	rm -f "$ENVFILE.bak"
	[ "$rc" = 0 ] && says "the repository exists; its password is needed" && says "repository ready: $S3/setupkill (existing" && pass "the password the killed setup showed opens the repository it made" || fail "exit $rc: $(cat "$OUT")"
	says "the record was put aside (the previous credential file named another repository): /var/lib/hotserve-backup/status.json.aside-" && [ "$(ls /var/lib/hotserve-backup/status.json.aside-* | wc -l)" = 1 ] && pass "the record of the other repository was put aside, and why was said" || fail "the record: $(ls /var/lib/hotserve-backup) — $(grep aside "$OUT")"
	[ "$(stat -c '%U %a' /var/lib/hotserve-backup/status.json.aside-*)" = "root 644" ] && pass "the aside is as the record was" || fail "the aside: $(stat -c '%U %a' /var/lib/hotserve-backup/status.json.aside-*)"
	nothing_left "after the sweep"
else
	fail "no restic init appeared to hold still"
fi

echo "=== setup 10: with the record aside, the box is a fresh setup on the new repository ==="
if hotserve-backup status >"$OUT" 2>&1; then says "pending first run" && pass "status: pending first run" || fail "status: $(cat "$OUT")"; else fail "status exits $?: $(cat "$OUT")"; fi
run && pass "the run into the new repository" || fail "the run: $(cat "$OUT")"
proof=$(proven blog)
[ -n "$proof" ] && rr snapshots --no-lock --json 2>/dev/null | grep -q "\"id\":\"$proof\"" && pass "and it drilled the snapshot it made there: restore proven on the repository in use" || fail "proof $proof is not in $S3/setupkill: $(cat "$OUT")"

echo "=== setup 11: a repository that does not answer: told what is waited for, then given up on, the unit stopped ==="
systemd-socket-activate -l 127.0.0.1:9999 /bin/sleep 3600 >/dev/null 2>&1 &
bh=$!
sleep 1
before=$(sum)
t0=$(date +%s)
converse "hotserve-backup setup s3:http://127.0.0.1:9999/blackhole" "$P_KEY" "$KEYID" "$P_SECRET" "$SECRET" "$P_STORED" stored
rc=$?
took=$(($(date +%s) - t0))
[ "$rc" != 0 ] && says "still waiting for s3:http://127.0.0.1:9999/blackhole (Ctrl-C is safe: nothing has been written)" && pass "a person waiting is told what for" || fail "the wait: exit $rc after ${took}s: $(cat "$OUT")"
says "the repository did not answer within 2m0s; the unit was stopped" && [ "$took" -ge 130 ] && [ "$took" -lt 190 ] && pass "given up on after the look's 10 s and the clock's 2 min (${took}s)" || fail "the clock: exit $rc after ${took}s: $(tail -3 "$OUT")"
[ "$(units_left)" = 0 ] && [ "$(temps)" = 0 ] && [ "$(sum)" = "$before" ] && pass "the unit is gone, nothing is left, the working file is as it was" || fail "after the clock: units=$(units_left) temps=$(temps)"
converse "hotserve-backup setup s3:http://127.0.0.1:9999/blackhole" "$P_KEY" "$KEYID" "$P_SECRET" "$SECRET" "$P_STORED" stored &
sp=$!
until_units 1 'hotserve_backup_init_*'
[ "$(systemctl list-units --plain --no-legend --state=activating 'hotserve_backup_init_*' | wc -l)" = 1 ] && pass "the init unit is waiting on the repository" || fail "no init unit is running"
kill -INT "$(pgrep -x hotserve-backup)"
wait "$sp"
[ $? != 0 ] && grep -q "interrupted: nothing has been written; the password shown above was never used: discard it" "$OUT" && pass "interrupted, setup exits non-zero and says the shown password is dead" || fail "interrupted setup: $(tail -2 "$OUT")"
until_units 0 'hotserve_backup_*'
[ "$(units_running)" = 0 ] && [ "$(temps)" = 0 ] && [ "$(sum)" = "$before" ] && pass "Ctrl-C stopped the unit and left nothing" || fail "after Ctrl-C: units=$(units_running) temps=$(temps)"
kill "$bh" 2>/dev/null

echo "=== setup 12: status as root says where the file is not read as written; as nobody it cannot look ==="
cp "$ENVFILE" /root/env.keep
hotserve-backup status >"$OUT" 2>&1
rc_before=$?
printf ' RESTIC_REPOSITORY = %s\n' "$S3/setupkill" >>"$ENVFILE"
hotserve-backup status >"$OUT" 2>&1
rc_root=$?
says "warning: $ENVFILE: line 5: the key is written with whitespace around it; the manager reads it as RESTIC_REPOSITORY" && says "warning: $ENVFILE: line 5: RESTIC_REPOSITORY is set again; the manager takes this one" && pass "status as root says what the manager makes of a hand-edited line" || fail "status as root: $(cat "$OUT")"
[ "$rc_root" = "$rc_before" ] && pass "and its verdict is unchanged by it" || fail "status exited $rc_root, $rc_before before the edit"
as_nobody cat "$ENVFILE" >/dev/null 2>&1 && fail "fixture: nobody can read the credential file" || pass "fixture: nobody cannot read the credential file"
as_nobody hotserve-backup status >"$OUT" 2>&1
[ $? = "$rc_root" ] && ! says "warning: $ENVFILE" && pass "as nobody, the same verdict and no word of the file" || fail "as nobody: $(cat "$OUT")"
cp /root/env.keep "$ENVFILE"

echo "=== setup 13: setup while a run holds the lock ==="
hotserve-backup run >/root/first.out 2>&1 &
first=$!
if hold_restic "restic backup"; then
	printf 'x\n' | at_tty hotserve-backup setup "$REPO" >"$OUT" 2>&1 && fail "setup exited 0 while a run held the lock" || { says "another backup run is in progress: pid $first" && ! says "Storage key id" && pass "setup says who holds the lock, and asks nothing" || fail "said: $(cat "$OUT")"; }
	kill -CONT "$pid"
	wait "$first"
else
	fail "no restic backup appeared to hold still"
fi
nothing_left "at the end"

# The other suites start from a record of their own.
rm -f "$STATUS" /var/lib/hotserve-backup/status.json.aside-*

if [ "$FAILURES" = 0 ]; then
	echo "ALL SETUP SCENARIOS PASSED"
	exit 0
fi
echo "$FAILURES SETUP ASSERTION(S) FAILED"
exit 1
