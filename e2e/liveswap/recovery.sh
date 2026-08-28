#!/bin/sh
# Crash-recovery suite, run by `make e2e` AFTER the main suites and
# after hotserve has been SIGKILLed and started again. Proves the
# state.json relaunch path: the last deployed release (px1 — the
# rollback target from main-suite scenario 12, demo-v1 content) comes
# back without any webhook call, and the deploy machinery isn't wedged
# by the unclean death.
set -u

PROXY="http://e2e-hotserve:8080"
HOOK="http://e2e-hotserve:8081/demo"
ART="http://e2e-artifacts:8080/artifacts"
TOKEN_FILE="${DEPLOY_TOKEN_FILE:-/shared/deploy.token}"
FAILURES=0

# Wait for the hotserve container's minted local deploy token (see
# e2e/entrypoint.sh), then use it as the deploy bearer.
i=0
until [ -s "$TOKEN_FILE" ]; do
	i=$((i + 1))
	if [ "$i" -ge 60 ]; then
		echo "FATAL: no deploy token at $TOKEN_FILE within 60s"
		exit 1
	fi
	sleep 1
done
TOKEN=$(cat "$TOKEN_FILE")

pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; FAILURES=$((FAILURES + 1)); }

deploy() { # <artifact-file> <version> -> prints HTTP status code
	curl -s -o /tmp/deploy-body -w '%{http_code}' --max-time 60 \
		-X POST -H "Authorization: Bearer $TOKEN" \
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
*) fail "expected px1's content ('hello v1 ...'), got '$b'" ;;
esac

echo "=== recovery 2: status endpoint reports the recovered deploy ==="
s=$(curl -s -H "Authorization: Bearer $TOKEN" "$HOOK")
case "$s" in
*'"current_version":"px1"'*'"running":true'*|*'"running":true'*'"current_version":"px1"'*)
	pass "status reports px1 running after crash" ;;
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
