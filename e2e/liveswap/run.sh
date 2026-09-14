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
. /lib.sh

# The hotserve container mints a local deploy token into the shared
# volume at startup (see e2e/mint-token.sh); wait for it, then use it
# as the deploy bearer. This is the `deploy_trust local` path standing
# in for a real CI OIDC provider, which the offline compose network has
# no way to reach.
echo "=== waiting for the deploy token ==="
wait_for_token

# traffic_start <file>: continuous requests, one status code per line
traffic_start() { # <file> [proxy]
	: > "$1"
	proxy=${2:-$PROXY}
	(while [ ! -f /tmp/stop-traffic ]; do
		curl -s -o /dev/null -w '%{http_code}\n' --max-time 2 "$proxy/" >> "$1" 2>/dev/null
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
	&& [ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" "$HOOK")" = "200" ]; do
	i=$((i + 1))
	if [ "$i" -ge 60 ]; then
		echo "FATAL: services not ready within 60s"
		exit 1
	fi
	sleep 1
done
echo "ready after ${i}s"

# app_pid is the running instance's host pid as the status endpoint
# reports it (the unit's MainPID). Inside its sandbox the app is pid 1
# of its own PID namespace, so the pid it prints is useless for
# telling instances apart; this one changes on every (re)start.
app_pid() { json_num "$(status)" pid; }

echo "=== scenario 1: auth — invalid and missing tokens are rejected ==="
c=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer not-a-jwt" -d '{}' "$HOOK")
[ "$c" = "401" ] && pass "garbage token gets 401" || fail "garbage token: expected 401, got $c"
c=$(curl -s -o /dev/null -w '%{http_code}' -X POST -d '{}' "$HOOK")
[ "$c" = "401" ] && pass "missing token gets 401" || fail "missing token: expected 401, got $c"
c=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" "$HOOK")
[ "$c" = "200" ] && pass "valid token accepted" || fail "valid token: expected 200, got $c"

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
pid_before=$(app_pid)
[ -n "$pid_before" ] || fail "could not read the app pid from the status endpoint"
traffic_start /tmp/codes-reload
c=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
	-H "Content-Type: text/caddyfile" --data-binary @/Caddyfile "$ADMIN/load")
sleep 1
traffic_stop
[ "$c" = "200" ] && pass "admin /load accepted the config" || fail "reload: expected 200, got $c"
assert_all_200 /tmp/codes-reload "reload"
pid_after=$(app_pid)
if [ -n "$pid_before" ] && [ "$pid_before" = "$pid_after" ]; then
	pass "app process survived the reload (pid $pid_before)"
else
	fail "app pid changed across reload: '$pid_before' -> '$pid_after'"
fi

echo "=== scenario 8: deploy status endpoint ==="
s=$(curl -s -H "Authorization: Bearer $TOKEN" "$HOOK")
case "$s" in
*'"current_version":"v4"'*'"running":true'*|*'"running":true'*'"current_version":"v4"'*)
	pass "status reports v4 running" ;;
*) fail "unexpected status: $s" ;;
esac

echo "=== scenario 9: watchdog restarts the app after a crash ==="
# The pid is the host pid from the status endpoint (the unit's
# MainPID), not the one the app prints: inside its PID namespace every
# instance is pid 1, so the body cannot tell a restart from no restart.
pid_before=$(app_pid)
[ -n "$pid_before" ] || fail "could not capture the app pid before /boom"
curl -s -o /dev/null --max-time 2 "$PROXY/boom"
i=0
new_pid=""
while [ "$i" -lt 60 ]; do
	b=$(body)
	case "$b" in
	"hello v1"*)
		new_pid=$(app_pid)
		[ -n "$pid_before" ] && [ -n "$new_pid" ] && [ "$new_pid" != "$pid_before" ] && break
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
s=$(curl -s -H "Authorization: Bearer $TOKEN" "$HOOK")
case "$s" in
*'"last_restart_cause":"crash"'*) pass "status reports the crash restart" ;;
*) fail "status missing crash restart: $s" ;;
esac

echo "=== scenario 10: watchdog restarts the app on sustained health failure ==="
pid_before=$(app_pid)
[ -n "$pid_before" ] || fail "could not capture the app pid before /break"
curl -s -o /dev/null --max-time 2 "$PROXY/break"
i=0
new_pid=""
while [ "$i" -lt 90 ]; do
	b=$(body)
	case "$b" in
	"hello v1"*)
		new_pid=$(app_pid)
		if [ -n "$pid_before" ] && [ -n "$new_pid" ] && [ "$new_pid" != "$pid_before" ]; then
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
s=$(curl -s -H "Authorization: Bearer $TOKEN" "$HOOK")
case "$s" in
*'"last_restart_cause":"health"'*) pass "status reports the health restart" ;;
*) fail "status missing health restart: $s" ;;
esac
c=$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "$PROXY/health")
[ "$c" = "200" ] && pass "app is healthy again after the heal" \
	|| fail "health still failing after heal: $c"

echo "=== scenario 11: push an uploaded artifact (no artifact host) ==="
curl -fs "$ART/demo-v1.tar.gz" -o /tmp/px1.tgz || fail "could not fetch a tarball to push"
c=$(curl -s -o /tmp/deploy-body -w '%{http_code}' --max-time 60 \
	-X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/gzip" \
	--data-binary @/tmp/px1.tgz "$HOOK?version=px1")
[ "$c" = "200" ] || fail "push px1: expected 200, got $c ($(cat /tmp/deploy-body))"
b=$(body)
case "$b" in
"hello v1"*) pass "pushed artifact deploys and serves: '$b'" ;;
*) fail "expected 'hello v1 ...' after push, got '$b'" ;;
esac

echo "=== scenario 12: push a second version, then roll back to the first ==="
curl -fs "$ART/demo-v2.tar.gz" -o /tmp/px2.tgz || fail "could not fetch the second tarball"
c=$(curl -s -o /tmp/deploy-body -w '%{http_code}' --max-time 60 \
	-X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/gzip" \
	--data-binary @/tmp/px2.tgz "$HOOK?version=px2")
[ "$c" = "200" ] || fail "push px2: expected 200, got $c ($(cat /tmp/deploy-body))"
b=$(body)
case "$b" in "hello v2"*) pass "second push serves v2: '$b'" ;; *) fail "expected 'hello v2 ...', got '$b'" ;; esac

# Discover what's available to roll back to, then roll back.
s=$(curl -s -H "Authorization: Bearer $TOKEN" "$HOOK")
case "$s" in
*'"available_versions"'*'px1'*) pass "status lists available_versions for rollback" ;;
*) fail "status missing px1 in available_versions: $s" ;;
esac

# Rollback relaunches px1's on-disk release — no fetch, no upload.
c=$(curl -s -o /tmp/deploy-body -w '%{http_code}' --max-time 60 \
	-X POST -H "Authorization: Bearer $TOKEN" "$HOOK?rollback=px1")
[ "$c" = "200" ] || fail "rollback px1: expected 200, got $c ($(cat /tmp/deploy-body))"
b=$(body)
case "$b" in
"hello v1"*) pass "rollback relaunches the on-disk release: '$b'" ;;
*) fail "expected 'hello v1 ...' after rollback, got '$b'" ;;
esac
# Rollback to a version that was never deployed is a 422.
c=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $TOKEN" "$HOOK?rollback=nope")
[ "$c" = "422" ] && pass "rollback to a missing release gets 422" || fail "missing rollback: expected 422, got $c"

# The examples are the apps newcomers copy. Each tarball comes from its
# own scripts/bundle.sh (artifacts image, standing in for the release
# its workflow publishes), its app-block lines from its own
# hotserve.caddy (imported by e2e/Caddyfile), and it is deployed here
# by its own scripts/deploy.sh — so a change to the app contract, or a
# runtime release that changes what the flags mean, fails here rather
# than in someone's first deploy. First by URL (the CI path), then a
# redeploy from a local file under traffic (the laptop path; its
# SIGTERM handler must finish in-flight requests), then a rollback to
# the first (the Run-workflow path), then a refused
# deploy: deploy.sh exits non-zero and prints the reason, and the
# running version is untouched.
example_scenario() { # <app> <port> <deploy.sh path> <version prefix>
	app=$1 hook="http://e2e-hotserve:8081/$1" proxy="http://e2e-hotserve:$2" script=$3 pre=$4
	# The auth header is what the workflow sends for a release asset;
	# the artifacts server ignores it, and a file upload has no URL to
	# send it with. Set here so deploy.sh's JSON-escaping of it is run.
	ex_deploy() { # <version> <artifact URL or file>
		HOTSERVE_URL=$hook HOTSERVE_TOKEN=$TOKEN VERSION=$1 ARTIFACT_AUTH_HEADER="token e2e" \
			sh "$script" "$2" >/tmp/ex-deploy.out 2>&1
	}
	ex_body() { curl -s --max-time 5 "$proxy/"; }
	if ex_deploy "${pre}1" "$ART/$app.tar.gz"; then
		pass "$app: deploy.sh had the box fetch the tarball by URL"
	else
		fail "$app: deploy.sh ${pre}1 failed: $(cat /tmp/ex-deploy.out)"
	fi
	b=$(ex_body)
	[ "$b" = "hello from ${pre}1 (schema v1)" ] \
		&& pass "$app: serves on its socket with its own flags, after its pre_start: '$b'" \
		|| fail "$app: expected 'hello from ${pre}1 (schema v1)', got '$b'"
	curl -fsS -o "/tmp/$app.tgz" "$ART/$app.tar.gz" || fail "$app: could not fetch the tarball"
	traffic_start "/tmp/codes-$app" "$proxy"
	ex_deploy "${pre}2" "/tmp/$app.tgz" || fail "$app: deploy.sh ${pre}2 failed: $(cat /tmp/ex-deploy.out)"
	traffic_stop
	assert_all_200 "/tmp/codes-$app" "$app cutover"
	b=$(ex_body)
	[ "$b" = "hello from ${pre}2 (schema v1)" ] && pass "$app: serves ${pre}2" || fail "$app: expected ${pre}2, got '$b'"
	# The workflow's Run-workflow path: deploy.sh --rollback, the release
	# still on the box's disk, no artifact — run as the workflow runs it,
	# with GITHUB_ACTIONS set, so the job-summary line is exercised.
	: >/tmp/ex-summary.md
	if HOTSERVE_URL=$hook HOTSERVE_TOKEN=$TOKEN GITHUB_ACTIONS=1 GITHUB_STEP_SUMMARY=/tmp/ex-summary.md \
		sh "$script" --rollback "${pre}1" >/tmp/ex-deploy.out 2>&1; then
		pass "$app: deploy.sh --rollback relaunched ${pre}1"
	else
		fail "$app: deploy.sh --rollback ${pre}1 failed: $(cat /tmp/ex-deploy.out)"
	fi
	b=$(ex_body)
	[ "$b" = "hello from ${pre}1 (schema v1)" ] && pass "$app: serves ${pre}1 again" || fail "$app: after the rollback: '$b'"
	grep -q "rollback to ${pre}1 live" /tmp/ex-summary.md && pass "$app: in Actions, a success lands one job-summary line" \
		|| fail "$app: summary after the rollback: $(cat /tmp/ex-summary.md)"
	if ex_deploy "${pre}3" "$ART/$app-missing.tar.gz"; then
		fail "$app: deploy.sh reported success for an artifact that does not exist"
	else
		case "$(cat /tmp/ex-deploy.out)" in
		*'"error"'*404*) pass "$app: a failed deploy exits non-zero and deploy.sh prints why" ;;
		*) fail "$app: deploy.sh failure output lacks the reason: $(cat /tmp/ex-deploy.out)" ;;
		esac
	fi
	b=$(ex_body)
	[ "$b" = "hello from ${pre}1 (schema v1)" ] && pass "$app: ${pre}1 kept serving through the failed deploy" || fail "$app: after the failed deploy: '$b'"
	# The same failure as the workflow sees it: the output in a group, an
	# error annotation naming the failing phase and the cause, a summary
	# line — and the raw body still printed. A failed deploy leaves no
	# release, so the version is free to try again.
	: >/tmp/ex-summary.md
	if GITHUB_ACTIONS=1 GITHUB_STEP_SUMMARY=/tmp/ex-summary.md ex_deploy "${pre}3" "$ART/$app-missing.tar.gz"; then
		fail "$app: in Actions, deploy.sh exited 0 on a failed deploy"
	else
		case "$(cat /tmp/ex-deploy.out)" in
		*'::group::deploying '*'"error"'*404*'::endgroup::'*'::error title=hotserve%3A '*' failed::in downloading: '*404*)
			pass "$app: in Actions, a failure is grouped and annotated with its phase and cause" ;;
		*) fail "$app: Actions-mode failure output: $(cat /tmp/ex-deploy.out)" ;;
		esac
		grep -q 'failed in `' /tmp/ex-summary.md && pass "$app: the job summary names the failed phase" \
			|| fail "$app: summary after the failed deploy: $(cat /tmp/ex-summary.md)"
	fi
	# A failure with no JSON body — nothing listening — still gets an
	# annotation, pointing at the output instead of a cause.
	if HOTSERVE_URL="http://e2e-hotserve:1/$app" HOTSERVE_TOKEN=$TOKEN VERSION="${pre}3" \
		GITHUB_ACTIONS=1 GITHUB_STEP_SUMMARY=/tmp/ex-summary.md sh "$script" "/tmp/$app.tgz" >/tmp/ex-deploy.out 2>&1; then
		fail "$app: deploy.sh exited 0 with nothing listening"
	else
		grep -q '::error title=hotserve%3A .* failed::see the response above' /tmp/ex-deploy.out \
			&& pass "$app: a failure without a JSON body is annotated too" \
			|| fail "$app: non-JSON failure output: $(cat /tmp/ex-deploy.out)"
	fi
}

echo "=== scenario 13: the Deno example deploys as its README says ==="
example_scenario deno-example 8082 /deploy-example.sh dx

echo "=== scenario 14: the Node example deploys as its README says ==="
example_scenario node-example 8083 /deploy-node-example.sh nx

echo ""
if [ "$FAILURES" -eq 0 ]; then
	echo "ALL E2E SCENARIOS PASSED"
	exit 0
fi
echo "$FAILURES E2E ASSERTION(S) FAILED"
exit 1
