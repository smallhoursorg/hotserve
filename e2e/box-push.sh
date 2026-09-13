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
admin journalctl -u hotserve -n 1 --no-pager >"$tmp/out" 2>&1 \
	&& pass "adm reads hotserve's journal without privilege" || fail "admin cannot read the journal: $(cat "$tmp/out")"
admin journalctl -t hotserve-demo -n 1 --no-pager >"$tmp/out" 2>&1 \
	&& pass "adm reads an app's journal" || fail "admin cannot read the app journal: $(cat "$tmp/out")"
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
