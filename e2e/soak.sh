#!/bin/sh
# Leak-hunting soak: hammer the churn paths (deploys, config reloads,
# penaltybox traffic from many distinct clients), then assert that
# goroutine and fd counts RETURN TO BASELINE once the churn stops.
# Those two are the sharp signals — in Go, leaks are almost always
# reference leaks held live by a parked goroutine. RSS is reported and
# gets a generous advisory bound only, because the Go allocator does
# not promptly return memory to the OS and a hard RSS assertion would
# flake.
#
# All measurement is over the admin API (Prometheus /metrics for
# go_goroutines / process_open_fds / process_resident_memory_bytes,
# /debug/pprof/heap?gc=1 to force a GC before sampling), so this runs
# entirely from the curl runner container.
#
# Knobs (defaults sized for a ~15-20 minute run):
#   SOAK_DEPLOYS  full deploy cycles (download+extract+spawn+cutover)
#   SOAK_RELOADS  admin /load config reloads
#   SOAK_CLIENTS  distinct penaltybox clients
#   SOAK_REQS     requests per client
set -u

PROXY="http://e2e-hotserve:8080"
HOOK="http://e2e-hotserve:8081/demo"
ADMIN="http://e2e-hotserve:2019"
PB="http://e2e-hotserve:9080"
ART="http://e2e-artifacts:8080/artifacts"
TOKEN_FILE="${DEPLOY_TOKEN_FILE:-/shared/deploy.token}"

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

DEPLOYS="${SOAK_DEPLOYS:-300}"
RELOADS="${SOAK_RELOADS:-300}"
CLIENTS="${SOAK_CLIENTS:-1500}"
REQS="${SOAK_REQS:-10}"

# Return-to-baseline slack. Goroutine/fd counts wobble a little
# (connection pools, GC workers), but a leak of one goroutine per
# deploy would show up as +hundreds.
SLACK=15

FAILURES=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; FAILURES=$((FAILURES + 1)); }

metric() { # <prometheus metric name> -> integer value (empty if absent)
	curl -s --max-time 5 "$ADMIN/metrics" | awk -v m="$1" '$1 == m {printf "%d\n", $2; exit}'
}

force_gc() {
	# ?gc=1 makes the pprof heap handler run a GC before profiling.
	curl -s -o /dev/null --max-time 15 "$ADMIN/debug/pprof/heap?gc=1" || true
}

measure() { # <label> -> sets G_<label>, FD_<label>, RSS_<label>
	force_gc
	sleep 2
	eval "G_$1=\$(metric go_goroutines)"
	eval "FD_$1=\$(metric process_open_fds)"
	eval "RSS_$1=\$(curl -s --max-time 5 \"$ADMIN/metrics\" \
		| awk '\$1 == \"process_resident_memory_bytes\" {print \$2; exit}')"
	eval "echo \"[$1] goroutines=\$G_$1 fds=\$FD_$1 rss_bytes=\$RSS_$1\""
}

deploy() { # <artifact> <version> -> status code
	curl -s -o /tmp/deploy-body -w '%{http_code}' --max-time 60 \
		-X POST -H "Authorization: Bearer $TOKEN" \
		-d "{\"url\":\"$ART/$1\",\"version\":\"$2\"}" "$HOOK"
}

echo "=== soak: waiting for services ==="
i=0
until curl -fs -o /dev/null "$ART/demo-v1.tar.gz" \
	&& [ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" "$HOOK")" = "200" ]; do
	i=$((i + 1))
	[ "$i" -ge 60 ] && { echo "FATAL: services not ready within 60s"; exit 1; }
	sleep 1
done

if [ -z "$(metric go_goroutines)" ]; then
	echo "FATAL: admin /metrics does not expose go_goroutines — cannot measure"
	exit 1
fi

echo "=== soak: initial deploy + baseline ==="
c=$(deploy demo-v1.tar.gz soak-0)
[ "$c" = "200" ] || { echo "FATAL: initial deploy failed ($c): $(cat /tmp/deploy-body)"; exit 1; }
sleep 5
measure base

echo "=== soak: $DEPLOYS deploy cycles ==="
start=$(date +%s)
i=1
while [ "$i" -le "$DEPLOYS" ]; do
	case $((i % 2)) in
	0) art=demo-v1.tar.gz ;;
	1) art=demo-v2.tar.gz ;;
	esac
	c=$(deploy "$art" "soak-$i")
	[ "$c" = "200" ] || fail "deploy soak-$i: expected 200, got $c ($(cat /tmp/deploy-body))"
	if [ $((i % 25)) -eq 0 ]; then
		echo "  deploy $i/$DEPLOYS ($(( $(date +%s) - start ))s elapsed, goroutines=$(metric go_goroutines))"
	fi
	i=$((i + 1))
done

echo "=== soak: $RELOADS config reloads ==="
i=1
while [ "$i" -le "$RELOADS" ]; do
	c=$(curl -s -o /dev/null -w '%{http_code}' --max-time 15 -X POST \
		-H "Content-Type: text/caddyfile" --data-binary @/Caddyfile "$ADMIN/load")
	[ "$c" = "200" ] || fail "reload $i: expected 200, got $c"
	if [ $((i % 50)) -eq 0 ]; then
		echo "  reload $i/$RELOADS (goroutines=$(metric go_goroutines))"
	fi
	i=$((i + 1))
done

echo "=== soak: penaltybox traffic — $CLIENTS clients x $REQS requests ==="
i=1
while [ "$i" -le "$CLIENTS" ]; do
	client="10.$((i / 65536 % 256)).$((i / 256 % 256)).$((i % 256))"
	j=1
	while [ "$j" -le "$REQS" ]; do
		# level=3 exercises counting, boxing and 429s; level=1 is pure
		# pass-through traffic.
		case $((j % 2)) in
		0) q="level=1" ;;
		1) q="level=3" ;;
		esac
		curl -s -o /dev/null --max-time 5 -H "X-Forwarded-For: $client" "$PB/?$q"
		j=$((j + 1))
	done
	if [ $((i % 250)) -eq 0 ]; then
		echo "  client $i/$CLIENTS (goroutines=$(metric go_goroutines))"
	fi
	i=$((i + 1))
done

echo "=== soak: settle and final measurement ==="
sleep 30
measure final

echo "=== soak: verdicts ==="
# The server must still work after all that.
b=$(curl -s --max-time 5 "$PROXY/")
case "$b" in
"hello v"*) pass "still serving after churn: '$b'" ;;
*) fail "proxy broken after churn: '$b'" ;;
esac

if [ "$G_final" -le $((G_base + SLACK)) ]; then
	pass "goroutines returned to baseline ($G_base -> $G_final, slack $SLACK)"
else
	fail "goroutine growth: $G_base -> $G_final after $DEPLOYS deploys + $RELOADS reloads (leak: ~$(( (G_final - G_base) ))+ goroutines held)"
fi

if [ -n "$FD_base" ] && [ -n "$FD_final" ]; then
	if [ "$FD_final" -le $((FD_base + SLACK)) ]; then
		pass "open fds returned to baseline ($FD_base -> $FD_final, slack $SLACK)"
	else
		fail "fd growth: $FD_base -> $FD_final (leaked sockets/files?)"
	fi
else
	echo "WARN: process_open_fds not exposed; fd check skipped"
fi

# RSS: advisory only. >2x growth after a forced GC is worth a human
# look but is not proof of a leak.
if awk -v a="$RSS_base" -v b="$RSS_final" 'BEGIN{exit !(b > 2 * a)}'; then
	echo "WARN: RSS more than doubled ($RSS_base -> $RSS_final bytes) — inspect heap profile"
else
	pass "RSS within advisory bound ($RSS_base -> $RSS_final bytes)"
fi

echo ""
if [ "$FAILURES" -eq 0 ]; then
	echo "SOAK PASSED ($DEPLOYS deploys, $RELOADS reloads, $((CLIENTS * REQS)) requests)"
	exit 0
fi
echo "$FAILURES SOAK ASSERTION(S) FAILED"
exit 1
