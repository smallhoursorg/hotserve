#!/bin/sh
# examples/box's bin/push against the e2e box (the e2e-hotserve
# container, reached with `docker compose exec` instead of ssh), run
# as `admin`: a user who is not root, holding only the example's
# sudoers file and the adm group (e2e/Dockerfile). A valid config is
# applied, a rejected one never touches the live file, and one that
# validates but fails to load is rolled back, so the file on disk
# always matches the running config. Also validates
# examples/box/Caddyfile itself with the binary under test. Runs on
# the host, last in `make e2e`: it swaps the e2e box's config and puts
# the original back at the end.
set -u
COMPOSE=${COMPOSE:-docker compose}
. "$(dirname "$0")/lib.sh"

# Test sites use 8180-8183: 808x and 9xxx belong to e2e/Caddyfile's own sites.
box() { $COMPOSE exec -T e2e-hotserve "$@"; }
admin() { $COMPOSE exec -T -u admin e2e-hotserve "$@"; }
push() { # <file> -> bin/push's exit status; output in $tmp/out
	YES=1 CONFIG="$1" HOTSERVE_REMOTE="$COMPOSE exec -T -u admin e2e-hotserve" \
		sh examples/box/bin/push >"$tmp/out" 2>&1
}
served() { box curl -s --max-time 5 "http://localhost:$1/"; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
box cat /etc/hotserve/Caddyfile >"$tmp/original"

echo "=== box 0: the admin user has what the README says, and no more ==="
# journalctl exits 0 on an empty (or invisible) journal too, so look
# for known lines, not just a clean exit.
admin journalctl -u hotserve --no-pager >"$tmp/out" 2>&1 && grep -q '"liveswap started"' "$tmp/out" \
	&& pass "adm reads hotserve's journal without privilege" || fail "admin cannot read the journal: $(tail -3 "$tmp/out")"
admin journalctl -t hotserve-demo --no-pager >"$tmp/out" 2>&1 && grep -q 'hotserve-demo' "$tmp/out" \
	&& pass "adm reads an app's journal" || fail "admin cannot read the app journal: $(tail -3 "$tmp/out")"
if admin sudo -n systemctl restart hotserve >"$tmp/out" 2>&1; then
	fail "the sudoers file let admin restart hotserve"
else
	pass "a command outside the sudoers file is refused"
fi
if admin ls /var/lib/hotserve >/dev/null 2>&1; then
	fail "admin can read hotserve's data dir"
else
	pass "admin cannot read hotserve's data dir: it is not the hotserve user"
fi
# The env-file grant: the one install line the sudoers file admits
# creates an empty 0640 root:hotserve file under /etc/hotserve, and
# nothing that differs from it by a path, a mode, an owner, a group, a
# source or an extra argument. A refusal must be sudo's own, not
# install failing after sudo let it through: a two-destination form,
# say, would fail in install either way. With -n, a command no
# NOPASSWD line matches is "a password is required"; a user with no
# line at all gets "is not allowed to execute". Either is sudo
# stopping before anything ran; install's own errors say "install:".
box rm -f /etc/hotserve/e2e-secrets.env
if admin sudo -n /usr/bin/install -m 0640 -o root -g hotserve -T /dev/null /etc/hotserve/e2e-secrets.env >"$tmp/out" 2>&1; then
	pass "admin can create an app's env file"
	[ "$(box stat -c '%a %U %G %s' /etc/hotserve/e2e-secrets.env)" = "640 root hotserve 0" ] \
		&& pass "the env file is empty, 0640 root:hotserve" || fail "env file is $(box stat -c '%a %U %G %s' /etc/hotserve/e2e-secrets.env)"
else
	fail "admin cannot create an env file: $(tail -3 "$tmp/out")"
fi
box rm -f /etc/hotserve/e2e-secrets.env
for args in \
	"-m 0640 -o root -g hotserve -T /dev/null /tmp/e2e-secrets.env" \
	"-m 0640 -o root -g hotserve -T /dev/null /etc/hotserve/../e2e-secrets.env" \
	"-m 0644 -o root -g hotserve -T /dev/null /etc/hotserve/e2e-secrets.env" \
	"-m 0640 -o admin -g hotserve -T /dev/null /etc/hotserve/e2e-secrets.env" \
	"-m 0640 -o root -g root -T /dev/null /etc/hotserve/e2e-secrets.env" \
	"-m 0640 -o root -g admin -T /dev/null /etc/hotserve/e2e-secrets.env" \
	"-m 0640 -o root -g hotserve -T /etc/shadow /etc/hotserve/e2e-secrets.env" \
	"-m 0640 -o root -g hotserve -T /dev/null /etc/hotserve/e2e-secrets.env -v" \
	"-m 0640 -o root -g hotserve /dev/null /etc/hotserve/e2e-secrets.env" \
	"-m 0640 -o root -g hotserve -T /dev/null /etc/hotserve/e2e-secrets.env /etc/hotserve/other.env"; do
	# shellcheck disable=SC2086 # the args are meant to split
	if admin sudo -n /usr/bin/install $args >"$tmp/out" 2>&1; then
		fail "the sudoers file let admin run: install $args"
	elif grep -q -E "a password is required|is not allowed to execute" "$tmp/out"; then
		pass "sudo refused: install $args"
	else
		fail "install $args failed, but not because sudo refused it: $(tail -1 "$tmp/out")"
	fi
done
box rm -f /etc/hotserve/e2e-secrets.env /etc/hotserve/other.env /tmp/e2e-secrets.env /etc/e2e-secrets.env
# -T is why a directory of that name (only root could make one) is an
# install error, not a root-owned file called "null" inside it.
box mkdir -p /etc/hotserve/e2e-dir.env
if admin sudo -n /usr/bin/install -m 0640 -o root -g hotserve -T /dev/null /etc/hotserve/e2e-dir.env >"$tmp/out" 2>&1; then
	fail "install replaced a directory named like an env file"
else
	box test ! -e /etc/hotserve/e2e-dir.env/null && grep -q '^install:' "$tmp/out" \
		&& pass "a directory named like an env file is an install error, and gains no file" \
		|| fail "against a directory: $(tail -1 "$tmp/out"); null exists: $(box test -e /etc/hotserve/e2e-dir.env/null && echo yes || echo no)"
fi
box rm -rf /etc/hotserve/e2e-dir.env

echo "=== box 1: examples/box/Caddyfile validates with this build ==="
if admin hotserve validate --adapter caddyfile --config /dev/stdin <examples/box/Caddyfile >"$tmp/out" 2>&1; then
	pass "examples/box/Caddyfile is a valid config"
else
	fail "examples/box/Caddyfile does not validate: $(tail -3 "$tmp/out")"
fi

echo "=== box 2: a valid change is applied and reloaded ==="
cp "$tmp/original" "$tmp/v1"
printf '\n:8180 {\n\trespond "pushed"\n}\n' >>"$tmp/v1"
if push "$tmp/v1"; then pass "push applied a valid config"; else fail "push of a valid config failed: $(cat "$tmp/out")"; fi
[ "$(served 8180)" = "pushed" ] && pass "the pushed config is running" || fail "the pushed site does not answer: '$(served 8180)'"
box cmp -s /etc/hotserve/Caddyfile - <"$tmp/v1" && pass "the live file is the pushed one" || fail "the live file differs from the pushed one"
box test ! -e /etc/hotserve/Caddyfile.prev && pass "a successful push leaves no .prev" || fail "Caddyfile.prev left after a successful push"
if push "$tmp/v1" && grep -q "already runs" "$tmp/out"; then pass "pushing the same file again is a no-op"; else fail "a repeat push: $(cat "$tmp/out")"; fi

echo "=== box 3: a config the box rejects never touches the live file ==="
cp "$tmp/v1" "$tmp/bad"
printf '\n:8181 {\n\tno_such_directive\n}\n' >>"$tmp/bad"
if push "$tmp/bad"; then fail "push accepted an invalid config"; else pass "push refused an invalid config: $(grep -o 'rejected.*' "$tmp/out")"; fi
box cmp -s /etc/hotserve/Caddyfile - <"$tmp/v1" && pass "the live file is unchanged" || fail "the live file changed after a rejected push"
box test ! -e /etc/hotserve/Caddyfile.new && pass "no staged file left behind" || fail "Caddyfile.new left on the box"
# A backup declaration is checked by the same validate, so one that
# names a sibling's shared dir stops here too, naming the path.
sed 's#files  uploads#files  ../../deno-example/shared#' "$tmp/v1" >"$tmp/bad-backup"
if cmp -s "$tmp/v1" "$tmp/bad-backup"; then
	fail "the e2e Caddyfile has no \`files  uploads\` line to break"
elif push "$tmp/bad-backup"; then
	fail "push accepted a backup path outside the app's shared dir"
elif grep -q -F 'backup files "../../deno-example/shared": the path reaches outside' "$tmp/out"; then
	pass "push refused a backup path outside the app's shared dir, naming the path"
else
	fail "push refused the config, but not for the backup path: $(tail -3 "$tmp/out")"
fi
box cmp -s /etc/hotserve/Caddyfile - <"$tmp/v1" && pass "the live file is unchanged" || fail "the live file changed after a rejected backup declaration"
box test ! -e /etc/hotserve/Caddyfile.new && pass "no staged file left behind" || fail "Caddyfile.new left after a rejected backup declaration"

echo "=== box 3c: a backup a backup run would not see stops validate and reload, not a start ==="
# The same Caddyfile, but its backup block comes from a snippet defined
# outside /etc/hotserve: a backup run's view holds /etc/hotserve alone,
# so that app would not be backed up while every run said ok.
box sh -c 'mkdir -p /srv/outside && printf "(bk) {\n\tbackup {\n\t\tsqlite demo.db\n\t\tfiles  uploads\n\t}\n}\n" >/srv/outside/bk.caddy && chmod 0644 /srv/outside/bk.caddy'
{ echo "import /srv/outside/bk.caddy"; awk '/^			backup \{$/ { print "			import bk"; skip = 1; next } skip && /^			\}$/ { skip = 0; next } !skip' "$tmp/v1"; } >"$tmp/outside"
if ! grep -q "import bk" "$tmp/outside"; then
	fail "the e2e Caddyfile has no backup block to move"
elif push "$tmp/outside"; then
	fail "push accepted a backup declared outside /etc/hotserve"
elif grep -q -F "declared in /srv/outside/bk.caddy, outside /etc/hotserve" "$tmp/out"; then
	pass "push refused a backup declared outside /etc/hotserve, naming the file"
else
	fail "push refused the config, but not for where the backup is declared: $(tail -3 "$tmp/out")"
fi
box cmp -s /etc/hotserve/Caddyfile - <"$tmp/v1" && pass "the live file is unchanged" || fail "the live file changed after a refused push"
box test ! -e /etc/hotserve/Caddyfile.new && pass "no staged file left behind" || fail "Caddyfile.new left after a refused push"
# By hand, without bin/push: the reload refuses, and the running config
# goes on.
box sh -c 'cat >/etc/hotserve/Caddyfile' <"$tmp/outside"
if box systemctl reload hotserve >"$tmp/out" 2>&1; then fail "systemctl reload took a backup declared outside /etc/hotserve"; else pass "a reload by hand is refused"; fi
box journalctl -u hotserve -n 20 --no-pager -o cat 2>/dev/null | grep -q -F "declared in /srv/outside/bk.caddy, outside /etc/hotserve" && pass "and the journal says why" || fail "the journal does not say why"
box systemctl is-active --quiet hotserve && [ "$(served 8180)" = "pushed" ] && pass "and the running config goes on serving" || fail "after the refused reload: '$(served 8180)'"
# A start is never refused for it: every site would be down.
box systemctl restart hotserve
for i in $(seq 40); do [ "$(served 8180)" = "pushed" ] && break; sleep 0.5; done
[ "$(served 8180)" = "pushed" ] && pass "a start with it serves all the same" || fail "after a start: '$(served 8180)'"
box journalctl -u hotserve -n 200 --no-pager -o cat 2>/dev/null | grep -q "served all the same" && pass "and says why it is wrong, in the journal" || fail "the start said nothing of the backup"
box sh -c 'cat >/etc/hotserve/Caddyfile' <"$tmp/v1"
box systemctl reload hotserve && pass "and with the backup back under /etc/hotserve, a reload is taken" || fail "the reload after mending it"
box rm -rf /srv/outside

echo "=== box 4: a config that validates but fails to load is rolled back ==="
# Validation does not bind listeners; loading does, and 192.0.2.1
# (TEST-NET-1) is on no interface here.
cp "$tmp/v1" "$tmp/unloadable"
printf '\nhttp://192.0.2.1:8182 {\n\tbind 192.0.2.1\n\trespond "unreachable"\n}\n' >>"$tmp/unloadable"
if push "$tmp/unloadable"; then
	fail "push reported success for a config the server cannot load"
else
	grep -q "previous Caddyfile is back" "$tmp/out" && pass "a failed reload is reported and rolled back" || fail "unexpected push output: $(cat "$tmp/out")"
fi
box cmp -s /etc/hotserve/Caddyfile - <"$tmp/v1" && pass "the live file is the running config again" || fail "the live file is not the running config after a failed reload"
[ "$(served 8180)" = "pushed" ] && pass "the running config kept serving" || fail "after the failed reload: '$(served 8180)'"

echo "=== box 5: a 'no' at the prompt applies nothing, even with stdin a pipe ==="
# No YES=1: bin/push asks, and the answer arrives on a pipe — which the
# remote commands must not have drained first.
cp "$tmp/v1" "$tmp/v2"
printf '\n:8183 {\n\trespond "never"\n}\n' >>"$tmp/v2"
if printf 'n\n' | CONFIG="$tmp/v2" HOTSERVE_REMOTE="$COMPOSE exec -T -u admin e2e-hotserve" sh examples/box/bin/push >"$tmp/out" 2>&1; then
	fail "push exited 0 after a 'no'"
else
	grep -q "not applied" "$tmp/out" && pass "a piped 'n' reaches the prompt and is honoured" || fail "unexpected push output: $(cat "$tmp/out")"
fi
box cmp -s /etc/hotserve/Caddyfile - <"$tmp/v1" && pass "the live file is unchanged after 'no'" || fail "the live file changed after a 'no'"
box test ! -e /etc/hotserve/Caddyfile.new && pass "no staged file left after 'no'" || fail "Caddyfile.new left after 'no'"

echo "=== box 6: with hotserve down, a valid push lands the file for the next start ==="
box systemctl stop hotserve
if push "$tmp/v2"; then
	fail "push exited 0 with hotserve stopped"
else
	grep -q "not running" "$tmp/out" && pass "push says hotserve is not running, not that the config failed" || fail "unexpected push output: $(cat "$tmp/out")"
fi
box cmp -s /etc/hotserve/Caddyfile - <"$tmp/v2" && pass "the validated file is in place for the next start" || fail "the live file is not the pushed one"
box test ! -e /etc/hotserve/Caddyfile.prev && pass "no .prev left behind" || fail "Caddyfile.prev left on the box"
box systemctl start hotserve
for i in $(seq 40); do [ "$(served 8183)" = "never" ] && break; sleep 0.5; done
[ "$(served 8183)" = "never" ] && pass "hotserve started with the pushed config" || fail "after start: '$(served 8183)'"

echo "=== box 7: put the e2e config back ==="
push "$tmp/original" && pass "restored the original config" || fail "could not restore the original config: $(cat "$tmp/out")"

echo ""
if [ "$FAILURES" -eq 0 ]; then
	echo "ALL BOX SCENARIOS PASSED"
	exit 0
fi
echo "$FAILURES BOX ASSERTION(S) FAILED"
exit 1
