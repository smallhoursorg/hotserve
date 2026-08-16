#!/bin/sh
# Crash-recovery suite, run by `make e2e` AFTER the main suites and
# after hotserve has been SIGKILLed and started again. Proves the
# state.json relaunch path: the last deployed release (v4, shipped with
# demo-v1 content by main-suite scenario 6) comes back without any
# webhook call, and the deploy machinery isn't wedged by the unclean
# death.
set -u

PROXY="http://e2e-hotserve:8080"
HOOK="http://e2e-hotserve:8081/demo"
ART="http://e2e-artifacts:8080/artifacts"
SECRET="e2e-secret"
FAILURES=0

pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; FAILURES=$((FAILURES + 1)); }

deploy() { # <artifact-file> <version> -> prints HTTP status code
	curl -s -o /tmp/deploy-body -w '%{http_code}' --max-time 60 \
		-X POST -H "X-Liveswap-Secret: $SECRET" \
		-d "{\"url\":\"$ART/$1\",\"version\":\"$2\"}" "$HOOK"
}

body() { curl -s --max-time 5 "$PROXY/"; }

echo "=== recovery 1: app relaunches from state.json after SIGKILL ==="
i=0
until [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "$PROXY/")" = "200" ]; do
	i=$((i + 1))
	if [ "$i" -ge 60 ]; then
		fail "proxy not serving within 60s of restart"
		echo "$FAILURES RECOVERY ASSERTION(S) FAILED"
		exit 1
	fi
	sleep 1
done
pass "proxy serving again ${i}s after restart"

b=$(body)
case "$b" in
"hello v1"*) pass "relaunched app serves the last deployed release: '$b'" ;;
*) fail "expected v4's content ('hello v1 ...'), got '$b'" ;;
esac

echo "=== recovery 2: status endpoint reports the recovered deploy ==="
s=$(curl -s -H "X-Liveswap-Secret: $SECRET" "$HOOK")
case "$s" in
*'"current_version":"v4"'*'"running":true'*|*'"running":true'*'"current_version":"v4"'*)
	pass "status reports v4 running after crash" ;;
*) fail "unexpected status after crash: $s" ;;
esac

echo "=== recovery 3: deploys still work after the unclean death ==="
c=$(deploy demo-v2.tar.gz v6)
[ "$c" = "200" ] && pass "post-recovery deploy accepted" \
	|| fail "post-recovery deploy: expected 200, got $c ($(cat /tmp/deploy-body))"
b=$(body)
case "$b" in
"hello v2"*) pass "post-recovery deploy serves: '$b'" ;;
*) fail "expected 'hello v2 ...', got '$b'" ;;
esac

echo ""
if [ "$FAILURES" -eq 0 ]; then
	echo "ALL RECOVERY SCENARIOS PASSED"
	exit 0
fi
echo "$FAILURES RECOVERY ASSERTION(S) FAILED"
exit 1
