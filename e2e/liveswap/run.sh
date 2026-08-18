#!/bin/sh
# End-to-end scenarios against the xcaddy-built Caddy: real webhook
# deploys of a real binary, with continuous-traffic assertions proving
# the zero-downtime claim, plus failure containment, 409 concurrency,
# and config-reload survival.
set -u

PROXY="http://e2e-hotserve:8080"
HOOK="http://e2e-hotserve:8081/demo"
ADMIN="http://e2e-hotserve:2019"
ART="http://e2e-artifacts:8080/artifacts"
SECRET="e2e-secret"
FAILURES=0

pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; FAILURES=$((FAILURES + 1)); }

# deploy <artifact-file> <version> -> prints HTTP status code
deploy() {
	curl -s -o /tmp/deploy-body -w '%{http_code}' --max-time 60 \
		-X POST -H "X-Liveswap-Secret: $SECRET" \
		-d "{\"url\":\"$ART/$1\",\"version\":\"$2\"}" "$HOOK"
}

body() { curl -s --max-time 5 "$PROXY/"; }

# traffic_start <file>: continuous requests, one status code per line
traffic_start() {
	: > "$1"
	(while [ ! -f /tmp/stop-traffic ]; do
		curl -s -o /dev/null -w '%{http_code}\n' --max-time 2 "$PROXY/" >> "$1" 2>/dev/null
		sleep 0.05
	done) &
	TRAFFIC_PID=$!
}

traffic_stop() {
	touch /tmp/stop-traffic
	wait "$TRAFFIC_PID" 2>/dev/null
	rm -f /tmp/stop-traffic
}

assert_all_200() { # <file> <label>
	total=$(wc -l < "$1")
	bad=$(grep -cv '^200$' "$1" || true)
	if [ "$total" -lt 10 ]; then
		fail "$2: traffic loop produced only $total samples"
	elif [ "$bad" -eq 0 ]; then
		pass "$2: all $total requests returned 200"
	else
		fail "$2: $bad of $total requests were not 200: $(grep -v '^200$' "$1" | sort | uniq -c | tr '\n' ' ')"
	fi
}

echo "=== waiting for e2e-hotserve and e2e-artifacts to become ready ==="
i=0
until curl -fs -o /dev/null "$ART/demo-v1.tar.gz" \
	&& [ "$(curl -s -o /dev/null -w '%{http_code}' -H "X-Liveswap-Secret: $SECRET" "$HOOK")" = "200" ]; do
	i=$((i + 1))
	if [ "$i" -ge 60 ]; then
		echo "FATAL: services not ready within 60s"
		exit 1
	fi
	sleep 1
done
echo "ready after ${i}s"

echo "=== scenario 1: auth — wrong and missing secrets are rejected ==="
c=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "X-Liveswap-Secret: wrong" -d '{}' "$HOOK")
[ "$c" = "401" ] && pass "wrong secret gets 401" || fail "wrong secret: expected 401, got $c"
c=$(curl -s -o /dev/null -w '%{http_code}' -X POST -d '{}' "$HOOK")
[ "$c" = "401" ] && pass "missing secret gets 401" || fail "missing secret: expected 401, got $c"
c=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer wrong" "$HOOK")
[ "$c" = "401" ] && pass "wrong bearer token gets 401" || fail "wrong bearer: expected 401, got $c"
c=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $SECRET" "$HOOK")
[ "$c" = "200" ] && pass "valid bearer token accepted" || fail "valid bearer: expected 200, got $c"

echo "=== scenario 2: nothing deployed yet -> proxy 5xx, not a hang ==="
c=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$PROXY/")
case "$c" in
5*) pass "undeployed app answers $c immediately" ;;
*) fail "expected 5xx before first deploy, got $c" ;;
esac

echo "=== scenario 3: deploy v1 through the webhook ==="
c=$(deploy demo-v1.tar.gz v1)
[ "$c" = "200" ] || { fail "deploy v1: expected 200, got $c ($(cat /tmp/deploy-body))"; }
b=$(body)
case "$b" in
"hello v1"*) pass "proxy serves v1: '$b'" ;;
*) fail "expected 'hello v1 ...', got '$b'" ;;
esac

echo "=== scenario 4: zero-downtime cutover to v2 under continuous traffic ==="
traffic_start /tmp/codes-cutover
c=$(deploy demo-v2.tar.gz v2)
traffic_stop
[ "$c" = "200" ] || fail "deploy v2: expected 200, got $c ($(cat /tmp/deploy-body))"
assert_all_200 /tmp/codes-cutover "cutover"
b=$(body)
case "$b" in
"hello v2"*) pass "proxy serves v2 after cutover: '$b'" ;;
*) fail "expected 'hello v2 ...', got '$b'" ;;
esac

echo "=== scenario 5: broken v3 is contained — webhook 5xx, v2 keeps serving ==="
traffic_start /tmp/codes-broken
c=$(deploy demo-v3-broken.tar.gz v3)
traffic_stop
case "$c" in
5*) pass "broken deploy reported failure ($c)" ;;
*) fail "broken deploy: expected 5xx, got $c" ;;
esac
grep -q "health gate" /tmp/deploy-body \
	&& pass "failure body names the health gate" \
	|| fail "failure body missing cause: $(cat /tmp/deploy-body)"
assert_all_200 /tmp/codes-broken "broken-deploy containment"
b=$(body)
case "$b" in
"hello v2"*) pass "v2 still serving after failed v3: '$b'" ;;
*) fail "expected v2 to keep serving, got '$b'" ;;
esac

echo "=== scenario 6: concurrent deploy gets 409 ==="
deploy demo-v1.tar.gz v4 > /tmp/first-deploy-code &
FIRST=$!
sleep 0.4
c=$(deploy demo-v1.tar.gz v5)
[ "$c" = "409" ] && pass "second deploy rejected with 409" || fail "expected 409 mid-deploy, got $c"
wait "$FIRST"
c=$(cat /tmp/first-deploy-code)
[ "$c" = "200" ] && pass "first deploy still completed ($c)" || fail "first deploy: expected 200, got $c"

echo "=== scenario 7: config reload mid-traffic — app survives, zero dropped requests ==="
pid_before=$(body | sed 's/.*pid //')
traffic_start /tmp/codes-reload
c=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
	-H "Content-Type: text/caddyfile" --data-binary @/Caddyfile "$ADMIN/load")
sleep 1
traffic_stop
[ "$c" = "200" ] && pass "admin /load accepted the config" || fail "reload: expected 200, got $c"
assert_all_200 /tmp/codes-reload "reload"
pid_after=$(body | sed 's/.*pid //')
if [ -n "$pid_before" ] && [ "$pid_before" = "$pid_after" ]; then
	pass "app process survived the reload (pid $pid_before)"
else
	fail "app pid changed across reload: '$pid_before' -> '$pid_after'"
fi

echo "=== scenario 8: deploy status endpoint ==="
s=$(curl -s -H "X-Liveswap-Secret: $SECRET" "$HOOK")
case "$s" in
*'"current_version":"v4"'*'"running":true'*|*'"running":true'*'"current_version":"v4"'*)
	pass "status reports v4 running" ;;
*) fail "unexpected status: $s" ;;
esac

echo "=== scenario 9: watchdog restarts the app after a crash ==="
pid_before=$(body | sed 's/.*pid //')
curl -s -o /dev/null --max-time 2 "$PROXY/boom"
i=0
new_pid=""
while [ "$i" -lt 60 ]; do
	b=$(body)
	case "$b" in
	"hello v1"*)
		new_pid="${b##*pid }"
		[ "$new_pid" != "$pid_before" ] && break
		;;
	esac
	new_pid=""
	i=$((i + 1))
	sleep 0.5
done
if [ -n "$new_pid" ]; then
	pass "watchdog relaunched after crash (pid $pid_before -> $new_pid)"
else
	fail "app did not come back after /boom (last body: '$b')"
fi
s=$(curl -s -H "X-Liveswap-Secret: $SECRET" "$HOOK")
case "$s" in
*'"last_restart_cause":"crash"'*) pass "status reports the crash restart" ;;
*) fail "status missing crash restart: $s" ;;
esac

echo "=== scenario 10: watchdog restarts the app on sustained health failure ==="
pid_before=$(body | sed 's/.*pid //')
curl -s -o /dev/null --max-time 2 "$PROXY/break"
i=0
new_pid=""
while [ "$i" -lt 90 ]; do
	b=$(body)
	case "$b" in
	"hello v1"*)
		new_pid="${b##*pid }"
		if [ "$new_pid" != "$pid_before" ]; then
			# Heal the release inside the replacement's watchdog grace
			# so it does not inherit the failure.
			curl -s -o /dev/null --max-time 2 "$PROXY/heal"
			break
		fi
		;;
	esac
	new_pid=""
	i=$((i + 1))
	sleep 0.5
done
if [ -n "$new_pid" ]; then
	pass "watchdog relaunched on health failure (pid $pid_before -> $new_pid)"
else
	fail "app was not restarted after /break (last body: '$b')"
fi
s=$(curl -s -H "X-Liveswap-Secret: $SECRET" "$HOOK")
case "$s" in
*'"last_restart_cause":"health"'*) pass "status reports the health restart" ;;
*) fail "status missing health restart: $s" ;;
esac
c=$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "$PROXY/health")
[ "$c" = "200" ] && pass "app is healthy again after the heal" \
	|| fail "health still failing after heal: $c"

echo ""
if [ "$FAILURES" -eq 0 ]; then
	echo "ALL E2E SCENARIOS PASSED"
	exit 0
fi
echo "$FAILURES E2E ASSERTION(S) FAILED"
exit 1
