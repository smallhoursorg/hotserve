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

echo "=== systemd 4: the app runs in its sandbox — the view from inside ==="
# Host-side facts first (never assertions): they explain a `none` tier
# on a kernel that refuses unprivileged user namespaces. Debian 13 does
# not restrict them, but the e2e container runs on whatever kernel the
# CI runner has, so record what that kernel allows.
echo "host: $(uname -r); apparmor_restrict_unprivileged_userns=$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns 2>/dev/null || echo absent)"
echo "userns: max_user_namespaces=$(cat /proc/sys/user/max_user_namespaces 2>/dev/null || echo absent)"
# The probe release's ./server records its view into its shared dir
# (e2e/liveswap/probe.sh), then becomes the app. The image is trixie
# (systemd 257) — the only release in the support matrix — so the tier
# is full: a PID namespace on top of the user namespace and the mount
# set.
code=$(deploy demo-probe.tar.gz sb1)
[ "$code" = "200" ] || fail "deploy of the probe release returned $code: $(body)"
probe=/var/lib/liveswap/demo/shared/probe.txt
i=0
until grep -q '^done=1' "$probe" 2>/dev/null; do
	i=$((i + 1)); [ "$i" -ge 20 ] && break; sleep 0.5
done
grep -q '^done=1' "$probe" || fail "probe.txt not written by the sandboxed app"
probe_val() { sed -n "s/^$1=//p" "$probe" | head -1; }
expect_probe() { # <key> <want>
	got=$(probe_val "$1")
	[ "$got" = "$2" ] && pass "inside the sandbox: $1=$2" || fail "inside the sandbox: $1=$got, want $2"
}
[ "$(json_str "$(status)" sandbox)" = "full" ] \
	&& pass "status reports sandbox=full" \
	|| fail "status sandbox is $(json_str "$(status)" sandbox), want full: $(status)"
expect_probe pid 1
[ "$(probe_val uidmap)" != "4294967295" ] \
	&& pass "inside the sandbox: own user namespace (uid_map $(probe_val uidmap))" \
	|| fail "no user namespace: uid_map covers the whole id space"
n=$(probe_val nprocs)
[ -n "$n" ] && [ "$n" -le 8 ] \
	&& pass "inside the sandbox: /proc lists $n pids (own PID namespace)" \
	|| fail "inside the sandbox: /proc lists $n pids — the host's, not a PID namespace"
[ "$(probe_val root_listing)" = "demo " ] \
	&& pass "inside the sandbox: the liveswap root shows only this app" \
	|| fail "inside the sandbox: root listing is '$(probe_val root_listing)', want only 'demo'"
expect_probe state closed
expect_probe apptmp closed
expect_probe current closed
expect_probe release writable
expect_probe root readonly
expect_probe hotserve_lib closed
expect_probe run_hotserve closed
expect_probe mgr_socket closed
expect_probe etc_hotserve closed
expect_probe cgroup readonly
expect_probe tmp writable
expect_probe home /var/lib/liveswap/demo/shared
expect_probe xdg_runtime unset
expect_probe acme_token absent
expect_probe dns ok
# The base view: an OS the app can run on. Asserted first, because
# without it every "absent" below would pass on an empty unit.
expect_probe binsh ok
expect_probe usrbinenv ok
expect_probe etcssl ok
expect_probe sslprivate absent
# Deny-by-default: nothing bound these, so they do not exist inside the
# unit — not "present but inaccessible", which is what the hidden-set
# model left behind and what could go stale between deploys.
for d in opt srv home root mnt media varlibhotserve etcliveswap; do
	expect_probe "abs_$d" absent
done
# /etc is named entry by entry, never bound whole: an app that could
# list all of /etc would see every other app's env_file.
case " $(probe_val etc_listing)" in
*" hotserve "* | *" liveswap "* | *" shadow "*)
	fail "inside the sandbox: /etc carries more than the named entries: $(probe_val etc_listing)" ;;
*) pass "inside the sandbox: /etc is only the named entries ($(probe_val etc_listing))" ;;
esac
# And /var/lib holds nothing but the path to this app's own dirs — the
# TLS keys next door on the host are not in the view at all.
[ "$(probe_val varlib_listing)" = "liveswap " ] \
	&& pass "inside the sandbox: /var/lib shows only the liveswap root" \
	|| fail "inside the sandbox: /var/lib listing is '$(probe_val varlib_listing)', want only 'liveswap'"
# The sandboxed app still serves through hotserve like any other.
curl -fsS --max-time 5 http://127.0.0.1:8080/ | grep -q "probe" \
	&& pass "the sandboxed release serves through the proxy" \
	|| fail "the sandboxed release does not serve"
# And the unit carries the properties the runner set.
sunit=$(json_str "$(status)" unit)
props=$(user_systemctl show "$sunit" -p PrivatePIDs,PrivateUsers,ProtectSystem,ProtectControlGroups,TemporaryFileSystem,InaccessiblePaths)
for want in "PrivatePIDs=yes" "PrivateUsers=yes" "ProtectControlGroups=yes" "TemporaryFileSystem=/:ro"; do
	case "$props" in
	*"$want"*) pass "unit property $want" ;;
	*) fail "unit lacks $want: $props" ;;
	esac
done
# The retired half of the old model, read back off the live unit: the
# view names what exists rather than masking a list of names.
for gone in "ProtectSystem=strict" "InaccessiblePaths=/"; do
	case "$props" in
	*"$gone"*) fail "unit still carries $gone: $props" ;;
	*) pass "unit no longer carries $gone" ;;
	esac
done

echo "=== systemd 5: a bare-recorded instance relaunches bare; the sandbox engages on the next deploy ==="
# The upgrade contract: enabling sandboxing must never force a running
# app into a sandbox on a relaunch that has no fallback (boot
# recovery, watchdog). Simulate a pre-sandbox state.json: stop
# hotserve, drop the recorded tier, kill the unit so recovery has to
# relaunch rather than reattach, start hotserve.
state=/var/lib/liveswap/demo/state.json
systemctl stop hotserve
sunit=$(sed -n 's/.*"unit": *"\([^"]*\)".*/\1/p' "$state")
[ -n "$sunit" ] || fail "could not read the recorded unit from state.json"
sed -i -e ':a' -e 'N' -e '$!ba' -e 's/,[[:space:]]*"sandbox":[[:space:]]*"[a-z]*"//' "$state"
grep -q '"sandbox"' "$state" && fail "could not strip the recorded tier from state.json"
user_systemctl stop "$sunit" >/dev/null 2>&1 || fail "could not stop $sunit"
# The unit must not be running afterwards (a transient unit whose app
# exits non-zero on SIGTERM stays loaded as "failed" until something
# resets it; recovery resets and relaunches, never adopts, either way).
user_systemctl is-active --quiet "$sunit" \
	&& fail "unit $sunit still active; the relaunch below would be a reattach"
systemctl start hotserve
wait_hook || fail "hotserve did not come back"
i=0
until [ "$(json_str "$(status)" phase 2>/dev/null)" = "running" ] && [ "$(json_num "$(status)" pid 2>/dev/null)" != "" ]; do
	i=$((i + 1)); [ "$i" -ge 40 ] && break; sleep 0.5
done
[ "$(json_str "$(status)" sandbox)" = "none" ] \
	&& pass "bare-recorded instance relaunched bare (sandbox=none) although config says on" \
	|| fail "relaunch changed the recorded tier: sandbox=$(json_str "$(status)" sandbox)"
journalctl --no-pager -u hotserve | grep -q "launching without the full sandbox" \
	&& pass "the bare relaunch was warned about in the journal" \
	|| fail "no WARN for the bare relaunch in the journal"
code=$(deploy demo-v2.tar.gz sb2)
[ "$code" = "200" ] || fail "deploy after the bare relaunch returned $code: $(body)"
[ "$(json_str "$(status)" sandbox)" = "full" ] \
	&& pass "the next deploy engaged the sandbox (sandbox=full)" \
	|| fail "the next deploy did not engage the sandbox: $(json_str "$(status)" sandbox)"

echo "=== systemd 6: stopping a version takes its whole process tree ==="
c=$(deploy demo-workers.tar.gz w1)
[ "$c" = "200" ] || fail "deploy workers: expected 200, got $c ($(cat /tmp/deploy-body))"
case "$(body)" in "hello workers"*) pass "worker-tree release serves" ;; *) fail "worker-tree release not serving" ;; esac
s=$(status)
wunit=$(json_str "$s" unit)
# Host pids from the unit's cgroup: inside its PID namespace the app
# sees itself as pid 1, so the pids.txt the leader writes cannot name
# host processes; the cgroup is the manager's own ledger of them.
wcg=$(user_systemctl show -p ControlGroup --value "$wunit")
tree=$(cat "/sys/fs/cgroup$wcg/cgroup.procs" 2>/dev/null | tr '\n' ' ')
[ "$(echo "$tree" | wc -w)" -ge 2 ] && pass "leader + worker in the unit's cgroup ($tree)" || fail "cgroup $wcg lists fewer than 2 pids: '$tree'"
c=$(deploy demo-v1.tar.gz sd-final)
[ "$c" = "200" ] || fail "deploy sd-final: expected 200, got $c ($(cat /tmp/deploy-body))"
survivors=""
for p in $tree; do kill -0 "$p" 2>/dev/null && survivors="$survivors $p"; done
[ -z "$survivors" ] && pass "every process of the old unit is gone" || fail "processes survived the stop:$survivors"
[ "$(user_systemctl show -p LoadState --value "$wunit")" = "not-found" ] \
	&& pass "old unit unloaded" || fail "old unit $wunit still loaded"

echo "=== systemd 7: a crash leaves no failed unit behind ==="
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

echo "=== systemd 8: a unit gone behind hotserve's back is relaunched on start ==="
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
# Recovery publishes the port as soon as the unit starts (no health
# gate on a relaunch); give the sandboxed app a moment to bind.
i=0
until case "$(body)" in "hello v1"*) true ;; *) false ;; esac; do
	i=$((i + 1)); [ "$i" -ge 20 ] && break; sleep 0.5
done
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

echo "=== systemd 9: removing the app via reload stops its units; adding it back relaunches ==="
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

echo "=== systemd 10: an app removed while hotserve was down is swept on the next start ==="
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

echo "=== systemd 11: app output reaches the journal ==="
journalctl --no-pager -t hotserve-demo | grep -q "workers up" \
	&& pass "app stdout is in the journal under identifier hotserve-demo" \
	|| fail "no app output in the journal for -t hotserve-demo"

echo "=== systemd 12: hotserve is non-dumpable — its /proc is closed to its own UID ==="
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
denied_to_hotserve() { # <what> <command...> -> pass only if the command fails AND reports EACCES
	what=$1; shift
	if out=$(as_hotserve "$*" 2>&1); then
		fail "$what is readable to the hotserve user (command succeeded): $out"
		return
	fi
	case "$out" in
	*"Permission denied"*) pass "$what is closed to the hotserve user" ;;
	*) fail "expected EACCES: $what as the hotserve user, got: $out" ;;
	esac
}
denied_to_hotserve "hotserve's environ" cat "/proc/$hpid/environ"
denied_to_hotserve "/proc/$hpid/root" ls "/proc/$hpid/root/"
# Control: root can read the app's environ, so the denials above are
# the non-dumpable floor, not a broken /proc. (The hotserve uid can no
# longer read a sandboxed app's environ either: the app sits in its
# own user namespace, which closes /proc to every process outside it.)
apid=$(json_num "$(status)" pid)
cat "/proc/$apid/environ" >/dev/null 2>&1 \
	&& pass "control: root can read the app's environ (pid $apid)" \
	|| fail "control: the app's environ (pid $apid) is unreadable even to root"

echo ""
if [ "$FAILURES" -eq 0 ]; then
	echo "ALL SYSTEMD SCENARIOS PASSED"
	exit 0
fi
echo "$FAILURES SYSTEMD ASSERTION(S) FAILED"
exit 1
