#!/bin/sh
# Runs INSIDE the e2e-hotserve container (systemd as PID 1), after the
# main suites and before the recovery suite: the assertions that need
# systemctl, journalctl and the process tree, which the runner
# container on the compose network cannot see. Proves the systemd
# runner's contract end to end: the app is a transient unit under the
# hotserve user's manager, it survives hotserve restarts and an
# unclean hotserve death, stopping a version takes its whole process
# tree, a crash leaves nothing behind, and app output is in the journal.
set -u

PROXY="http://127.0.0.1:8080"
HOOK="http://127.0.0.1:8081/demo"
ART="http://e2e-artifacts:8080/artifacts"
. /lib.sh
wait_for_token

uid=$(id -u hotserve)
as_hotserve() { su -s /bin/sh hotserve -c "$*"; }
# systemctl --user for a nologin system user: hotserve's own trick —
# its manager's private socket under /run/user/<uid>.
user_systemctl() { as_hotserve "XDG_RUNTIME_DIR=/run/user/$uid systemctl --user $*"; }

wait_hook() { # -> 0 once the webhook answers 200, 1 after 30s
	i=0
	until [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 -H "Authorization: Bearer $TOKEN" "$HOOK")" = "200" ]; do
		i=$((i + 1))
		[ "$i" -ge 30 ] && return 1
		sleep 1
	done
}

echo "=== systemd 1: the app is a transient unit under user@$uid ==="
s=$(status)
unit=$(json_str "$s" unit)
pid=$(json_num "$s" pid)
if [ -n "$unit" ] && [ -n "$pid" ]; then
	pass "status names the unit ($unit, pid $pid)"
else
	fail "status lacks unit/pid: $s"
	echo "$FAILURES SYSTEMD ASSERTION(S) FAILED"
	exit 1
fi
systemctl is-active --quiet "user@$uid.service" && pass "user@$uid is active" || fail "user@$uid not active"
user_systemctl is-active --quiet "$unit" && pass "unit is active under the user manager" || fail "unit $unit not active"
[ "$(user_systemctl show -p MainPID --value "$unit")" = "$pid" ] \
	&& pass "unit MainPID matches the status pid" || fail "MainPID differs from status pid $pid"
[ "$(user_systemctl show -p Restart --value "$unit")" = "no" ] \
	&& pass "unit has Restart=no (the watchdog is the only restarter)" || fail "unit Restart is not 'no'"
[ "$(user_systemctl show -p KillMode --value "$unit")" = "control-group" ] \
	&& pass "unit has KillMode=control-group" || fail "unit KillMode is not control-group"

echo "=== systemd 2: systemctl restart hotserve keeps the app running ==="
systemctl restart hotserve || fail "systemctl restart hotserve failed"
wait_hook || fail "webhook not back within 30s of restart"
s=$(status)
if [ "$(json_num "$s" pid)" = "$pid" ] && [ "$(json_str "$s" unit)" = "$unit" ]; then
	pass "same pid and unit after restart (reattached, not relaunched)"
else
	fail "instance changed across restart: $s"
fi
case "$(body)" in "hello "*) pass "proxy serves after restart" ;; *) fail "proxy not serving after restart" ;; esac

echo "=== systemd 3: SIGKILL of hotserve, then start: reattach ==="
systemctl kill -s SIGKILL hotserve
sleep 1
systemctl start hotserve || fail "systemctl start after SIGKILL failed"
wait_hook || fail "webhook not back within 30s of the unclean death"
s=$(status)
if [ "$(json_num "$s" pid)" = "$pid" ] && [ "$(json_str "$s" unit)" = "$unit" ]; then
	pass "same pid and unit after SIGKILL + start"
else
	fail "instance changed across the unclean death: $s"
fi
case "$s" in *'"running":true'*) pass "status reports the reattached app running" ;; *) fail "reattached app not running: $s" ;; esac

echo "=== systemd 4: stopping a version takes its whole process tree ==="
c=$(deploy demo-workers.tar.gz w1)
[ "$c" = "200" ] || fail "deploy workers: expected 200, got $c ($(cat /tmp/deploy-body))"
case "$(body)" in "hello workers"*) pass "worker-tree release serves" ;; *) fail "worker-tree release not serving" ;; esac
s=$(status)
wunit=$(json_str "$s" unit)
pidfile=/var/lib/liveswap/demo/releases/w1/pids.txt
tree=$(cat "$pidfile" 2>/dev/null | tr '\n' ' ')
[ "$(echo "$tree" | wc -w)" -ge 2 ] && pass "leader + worker recorded ($tree)" || fail "pids.txt missing or short: '$tree'"
c=$(deploy demo-v1.tar.gz sd-final)
[ "$c" = "200" ] || fail "deploy sd-final: expected 200, got $c ($(cat /tmp/deploy-body))"
survivors=""
for p in $tree; do kill -0 "$p" 2>/dev/null && survivors="$survivors $p"; done
[ -z "$survivors" ] && pass "every process of the old unit is gone" || fail "processes survived the stop:$survivors"
[ "$(user_systemctl show -p LoadState --value "$wunit")" = "not-found" ] \
	&& pass "old unit unloaded" || fail "old unit $wunit still loaded"

echo "=== systemd 5: a crash leaves no failed unit behind ==="
s=$(status)
cunit=$(json_str "$s" unit)
cpid=$(json_num "$s" pid)
curl -s -o /dev/null --max-time 2 "$PROXY/boom"
i=0
until [ "$(json_num "$(status)" pid)" != "$cpid" ] && [ "$(json_num "$(status)" pid)" != "" ]; do
	i=$((i + 1))
	[ "$i" -ge 60 ] && break
	sleep 0.5
done
s=$(status)
[ "$(json_num "$s" pid)" != "$cpid" ] && pass "watchdog relaunched after the crash" || fail "no relaunch after /boom: $s"
sleep 1
[ "$(user_systemctl show -p LoadState --value "$cunit")" = "not-found" ] \
	&& pass "crashed unit was reset and unloaded" || fail "crashed unit $cunit still loaded"
n=$(user_systemctl list-units --all --no-legend --plain "'hotserve-*'" | wc -l)
[ "$n" = "1" ] && pass "exactly one hotserve unit loaded" || fail "expected 1 loaded hotserve unit, found $n"

echo "=== systemd 6: a unit gone behind hotserve's back is relaunched on start ==="
# The cold path recovery takes when the recorded unit no longer exists
# (a reboot, a manual stop — and a v0.1.0 state.json with no unit at
# all): not reattach, but launch. Stop the unit as the operator might,
# restart hotserve, and expect the same version back on a new pid.
s=$(status)
gunit=$(json_str "$s" unit)
gpid=$(json_num "$s" pid)
user_systemctl stop "$gunit"
systemctl restart hotserve || fail "systemctl restart hotserve failed"
wait_hook || fail "webhook not back within 30s of restart"
i=0
until [ -n "$(json_num "$(status)" pid)" ] && [ "$(json_num "$(status)" pid)" != "$gpid" ]; do
	i=$((i + 1))
	[ "$i" -ge 60 ] && break
	sleep 0.5
done
s=$(status)
if [ -n "$(json_num "$s" pid)" ] && [ "$(json_num "$s" pid)" != "$gpid" ] && [ "$(json_str "$s" unit)" != "$gunit" ]; then
	pass "recovery relaunched the recorded version as a new unit (pid $gpid -> $(json_num "$s" pid))"
else
	fail "no relaunch after the unit was stopped behind hotserve's back: $s"
fi
case "$s" in *'"current_version":"sd-final"'*) pass "the relaunched instance is the recorded version" ;; *) fail "unexpected version after relaunch: $s" ;; esac
case "$(body)" in "hello v1"*) pass "proxy serves the relaunched instance" ;; *) fail "proxy not serving after relaunch" ;; esac

# removal_config writes a Caddyfile without the demo app — and without
# the :8080 site that proxies to it and the :8081 webhook site, since
# liveswap_webhook refuses a config that defines no apps. The liveswap
# global block itself stays, so the module is still loaded (that is
# the shape of "an operator removed their last app").
removal_config() { # <out>
	awk '
		/^\t\tapp demo \{/ { skip = 1 }
		/^:808[01] \{/    { skip = 2 }
		skip == 0 { print }
		skip == 1 && /^\t\t}$/ { skip = 0 }
		skip == 2 && /^}$/     { skip = 0 }
	' /etc/hotserve/Caddyfile > "$1"
	grep -q 'app demo' "$1" && die "removal config still names the app"
	grep -q 'liveswap {' "$1" || die "removal config lost the liveswap block"
}
# load_config POSTs a Caddyfile to the admin API. Caddy flushes adapter
# warnings before it loads, so the HTTP status can read 200 for a
# rejected config — the body's "error" is the truth.
load_config() { # <file> -> 0 on success
	curl -s -o /tmp/load-body -w '%{http_code}' -X POST -H "Content-Type: text/caddyfile" --data-binary @"$1" "$ADMIN/load" >/tmp/load-code
	[ "$(cat /tmp/load-code)" = "200" ] && ! grep -q '"error"' /tmp/load-body
}
die() { echo "FATAL: $1"; exit 1; }
ADMIN="http://127.0.0.1:2019"
unit_gone() { # <unit> -> 0 once the manager no longer loads it (waits up to 20s)
	i=0
	until [ "$(user_systemctl show -p LoadState --value "$1")" = "not-found" ]; do
		i=$((i + 1))
		[ "$i" -ge 40 ] && return 1
		sleep 0.5
	done
}
wait_new_pid() { # <old pid> -> sets NEWPID or fails after 30s
	i=0
	NEWPID=""
	while [ "$i" -lt 60 ]; do
		p=$(json_num "$(status)" pid)
		if [ -n "$p" ] && [ "$p" != "$1" ] && [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "$PROXY/")" = "200" ]; then
			NEWPID=$p
			return 0
		fi
		i=$((i + 1))
		sleep 0.5
	done
	return 1
}

echo "=== systemd 7: removing the app via reload stops its units; adding it back relaunches ==="
s=$(status)
runit=$(json_str "$s" unit)
rpid=$(json_num "$s" pid)
removal_config /tmp/without-demo.Caddyfile
load_config /tmp/without-demo.Caddyfile && pass "reload without the app accepted" \
	|| fail "reload without the app rejected: $(cat /tmp/load-code) $(cat /tmp/load-body)"
unit_gone "$runit" && pass "removed app's unit stopped and unloaded" || fail "unit $runit survived the app's removal"
kill -0 "$rpid" 2>/dev/null && fail "removed app's process $rpid still alive" || pass "removed app's process is gone"
load_config /etc/hotserve/Caddyfile || fail "reload restoring the app rejected: $(cat /tmp/load-body)"
wait_new_pid "$rpid" && pass "re-added app relaunched from state.json (pid $rpid -> $NEWPID)" \
	|| fail "re-added app did not come back: $(status)"
case "$(status)" in *'"current_version":"sd-final"'*) pass "relaunched the recorded version" ;; *) fail "unexpected version after re-add: $(status)" ;; esac

echo "=== systemd 8: an app removed while hotserve was down is swept on the next start ==="
s=$(status)
dunit=$(json_str "$s" unit)
dpid=$(json_num "$s" pid)
cp /etc/hotserve/Caddyfile /tmp/with-demo.Caddyfile
systemctl stop hotserve
kill -0 "$dpid" 2>/dev/null && pass "app survives hotserve stop (pid $dpid)" || fail "app died with hotserve stop"
cp /tmp/without-demo.Caddyfile /etc/hotserve/Caddyfile
systemctl start hotserve || fail "start without the app failed"
unit_gone "$dunit" && pass "start-time sweep stopped the unit of an app no config names" \
	|| fail "unit $dunit of a removed app survived the restart"
cp /tmp/with-demo.Caddyfile /etc/hotserve/Caddyfile
systemctl restart hotserve || fail "restart with the app failed"
wait_hook || fail "webhook not back after restoring the app"
wait_new_pid "$dpid" && pass "restored app relaunched from state.json (pid $dpid -> $NEWPID)" \
	|| fail "restored app did not come back: $(status)"

echo "=== systemd 9: app output reaches the journal ==="
journalctl --no-pager -t hotserve-demo | grep -q "workers up" \
	&& pass "app stdout is in the journal under identifier hotserve-demo" \
	|| fail "no app output in the journal for -t hotserve-demo"

echo "=== systemd 10: hotserve is non-dumpable — its /proc is closed to its own UID ==="
# Apps run as the hotserve user. The kernel opens /proc/<pid>/environ
# and /proc/<pid>/root to any same-UID reader of a dumpable process —
# whatever sandbox that reader sits in — so without this floor every
# app could read the supervisor's environment (ACME DNS tokens) or walk
# the host filesystem through /proc/<hotserve>/root. liveswap/harden's
# init marks the process non-dumpable before main; this proves it
# under the packaged unit.
hpid=$(systemctl show -p MainPID --value hotserve)
[ -n "$hpid" ] && [ "$hpid" != "0" ] || fail "no MainPID for hotserve.service"
# (The /proc/<pid> directory itself keeps the task's uid so `ps` can
# stat it; the per-process files inside are what turn root-owned.)
[ "$(stat -c %U "/proc/$hpid/environ")" = "root" ] \
	&& pass "/proc/$hpid/environ is root-owned (hotserve is non-dumpable)" \
	|| fail "/proc/$hpid/environ is owned by $(stat -c %U "/proc/$hpid/environ"): hotserve is dumpable"
# Each denial is asserted by its reason, not by a non-zero exit: a
# broken `su` would otherwise pass these vacuously.
denied_to_hotserve() { # <what> <command...> -> pass if the command fails with EACCES
	what=$1; shift
	out=$(as_hotserve "$*" 2>&1)
	case "$out" in
	*"Permission denied"*) pass "$what is closed to the hotserve user" ;;
	*) fail "expected EACCES: $what as the hotserve user, got: $out" ;;
	esac
}
denied_to_hotserve "hotserve's environ" cat "/proc/$hpid/environ"
denied_to_hotserve "/proc/$hpid/root" ls "/proc/$hpid/root/"
apid=$(json_num "$(status)" pid)
as_hotserve "cat /proc/$apid/environ" >/dev/null 2>&1 \
	&& pass "control: the app's own environ is still readable to its uid (the floor is hotserve's alone)" \
	|| fail "control: the app's environ (pid $apid) is unreadable to its own uid"

echo ""
if [ "$FAILURES" -eq 0 ]; then
	echo "ALL SYSTEMD SCENARIOS PASSED"
	exit 0
fi
echo "$FAILURES SYSTEMD ASSERTION(S) FAILED"
exit 1
