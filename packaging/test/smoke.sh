#!/bin/bash
# Package install smoke test, run as root inside the systemd container
# started by `make install-test` (dist/ mounted read-only at /dist).
#
# Fail-fast by design: each stage depends on the previous one, so the
# first broken assertion is the diagnosis. The Makefile dumps the
# hotserve journal on any failure.
#
# Stage 2 is the one that earns its keep: a real liveswap deploy under
# the shipped systemd unit proves that download + extract work in
# /var/lib/liveswap as the sandboxed hotserve user and that the app
# comes up as a transient unit under that user's own systemd manager —
# the exact packaging interaction (ProtectSystem=full, User=hotserve,
# lingering, the user@<uid> drop-in) no other test layer exercises.
# Stage 3 then proves the app survives the upgrade restart.
set -eu

# TOKEN is minted in stage 2 with a local deploy key (deploy_trust
# local), once the binary is installed.
TOKEN=""
HOOK="http://127.0.0.1:8081/demo"
PROXY="http://127.0.0.1:8080"

stage() { echo ""; echo "════ $1 ════"; }
die() { echo "FAIL: $1" >&2; exit 1; }

stage "stage 0: wait for systemd, locate the package"
# is-system-running exits non-zero for every state but "running", so
# capture its output regardless of exit code; "degraded" is normal in a
# container (units like modules-load can't work there).
i=0
while :; do
	state=$(systemctl is-system-running 2>/dev/null || true)
	case "$state" in running|degraded) break ;; esac
	i=$((i + 1))
	if [ "$i" -ge 60 ]; then
		systemctl list-units --failed --no-pager || true
		die "systemd not up after 60s (state: ${state:-unknown})"
	fi
	sleep 1
done
echo "systemd is $state after ${i}s"

arch=$(dpkg --print-architecture)
set -- /dist/hotserve_*_"$arch".deb
[ $# -eq 1 ] && [ -f "$1" ] || die "expected exactly one $arch .deb in /dist, found: $*"
deb=$1
echo "package under test: $deb"

stage "stage 1: install, service basics, reload"
# The image ships without apt lists; refresh them so the package's
# Depends (libpam-systemd, dbus) resolve the way they would on a real
# box — that resolution is part of what this stage proves.
apt-get update -qq
apt-get install -y "$deb"

id hotserve >/dev/null || die "postinstall did not create the hotserve user"
getent group hotserve >/dev/null || die "postinstall did not create the hotserve group"
for d in /var/lib/hotserve /var/lib/liveswap; do
	got=$(stat -c '%U:%G %a' "$d")
	[ "$got" = "hotserve:hotserve 750" ] || die "$d is '$got', want 'hotserve:hotserve 750'"
done
echo "user/group and data dir ownership OK"

/usr/bin/hotserve version
for m in liveswap http.handlers.liveswap_webhook \
	http.reverse_proxy.upstreams.liveswap http.handlers.hint_penaltybox \
	http.handlers.cache storages.cache.otter; do
	/usr/bin/hotserve list-modules | grep -qx "$m" || die "module $m not linked in"
done
echo "all product modules linked"

# Type=notify: enable --now returning 0 IS the readiness assertion.
timeout 120 systemctl enable --now hotserve || die "systemctl enable --now hotserve failed"
curl -fsS --max-time 5 http://127.0.0.1:80/ | grep -q "hotserve is running" \
	|| die "starter Caddyfile not serving on :80"
echo "unit started and starter config answers on :80"

uid=$(id -u hotserve)
systemctl is-active --quiet "user@$uid.service" \
	|| die "hotserve's systemd user manager (user@$uid) is not active — postinstall linger/drop-in broken (is libpam-systemd installed?)"
[ -f /var/lib/systemd/linger/hotserve ] || die "lingering not enabled for hotserve"
[ -f /etc/systemd/system/hotserve.service.d/10-user-manager.conf ] || die "user-manager drop-in missing"
[ -S "/run/user/$uid/systemd/private" ] || die "user manager private socket missing"
echo "user manager user@$uid active with lingering"

# ExecReload connects through the admin unix socket — this call IS the
# socket's functional test.
systemctl reload hotserve || die "systemctl reload (ExecReload via admin unix socket) failed"
systemctl is-active --quiet hotserve || die "service not active after reload"
# The admin API must NOT answer on TCP: localhost includes every
# deployed app, and an SSRF bug in one could otherwise reconfigure the
# server. The unix socket is the whole point.
curl -s --max-time 2 -o /dev/null http://127.0.0.1:2019/config/ \
	&& die "admin API answers on TCP :2019 — it must live only on the unix socket" || true
[ -S /run/hotserve/admin.sock ] || die "admin unix socket missing from /run/hotserve"
curl -fsS --max-time 5 http://127.0.0.1:80/ | grep -q "hotserve is running" \
	|| die "not serving after reload"
[ "$(systemctl show -p NRestarts --value hotserve)" = "0" ] \
	|| die "service restarted behind our back (NRestarts != 0)"
# 'permission denied' / 'read-only file system' would mean the unit's
# XDG dirs point somewhere the sandboxed hotserve user cannot write —
# that breaks ACME cert persistence in production even though the
# service superficially runs.
journalctl -u hotserve --no-pager \
	| grep -Ei 'panic|SIGSEGV|permission denied|read-only file system' \
	&& die "crash or writability error in the journal (see lines above)" || true
echo "reload OK, journal clean"

stage "stage 2: liveswap deploy under the systemd sandbox"
# Generate a local deploy keypair; the app trusts the public half, and
# we mint the deploy bearer with the private half. The service (User=
# hotserve) reads the 0644 public key; the 0600 private key stays with
# root here, standing in for the operator's token-minting machine.
hotserve deploy-keygen --out /etc/hotserve/deploy.key

# Overwriting the packaged conffile doubles as the modification marker
# for the stage-3 config|noreplace assertion.
cat > /etc/hotserve/Caddyfile <<'EOF'
{
	admin unix//run/hotserve/admin.sock
	auto_https off

	liveswap {
		root /var/lib/liveswap
		allow_insecure_http
		artifact_allowlist 127.0.0.1:8200
		deploy_trust local {
			public_key /etc/hotserve/deploy.key.pub
			audience smoke
		}

		app demo {
			command ./server
			health_interval 250ms
			health_timeout 1s
			soak 1s
			deadline 20s
		}
	}
}

:8080 {
	reverse_proxy {
		dynamic liveswap demo
	}
}

:8081 {
	liveswap_webhook
}
EOF

systemctl daemon-reload
timeout 120 systemctl restart hotserve || die "restart with liveswap config failed"

# Mint the deploy bearer with the private key (audience must match the
# deploy_trust block).
TOKEN=$(hotserve deploy-token --key /etc/hotserve/deploy.key --audience smoke --ttl 10m) \
	|| die "deploy-token failed"

i=0
until [ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" "$HOOK")" = "200" ]; do
	i=$((i + 1))
	[ "$i" -ge 30 ] && die "webhook status endpoint not ready within 30s"
	sleep 1
done

# Build a deployable artifact on the fly: the "app" is a wrapper around
# `hotserve respond`, honoring liveswap's PORT contract and answering
# 200 on every path (which satisfies the health gate).
workdir=$(mktemp -d)
# Besides the NOFILE contract the wrapper reports what its sandbox
# looks like from inside: the uid_map range (1 in a user namespace,
# 2^32-1 in none), its own pid (1 in a PID namespace), how many pids
# /proc shows, and whether hotserve's own state dir exists in its view.
cat > "$workdir/server" <<'EOF'
#!/bin/sh
read _ _ uidmap < /proc/self/uid_map
[ -r /var/lib/hotserve ] && hslib=open || hslib=closed
echo "smoke app starting on $PORT nofile_soft=$(ulimit -Sn) nofile_hard=$(ulimit -Hn) uidmap=$uidmap pid=$$ nprocs=$(ls /proc | grep -c '^[0-9]') hotserve_lib=$hslib"
exec /usr/bin/hotserve respond --listen 127.0.0.1:"$PORT" "hello smoke"
EOF
chmod +x "$workdir/server"
mkdir -p /srv/art
tar -czf /srv/art/demo.tar.gz -C "$workdir" server

/usr/bin/hotserve file-server --listen 127.0.0.1:8200 --root /srv/art \
	>/tmp/artserver.log 2>&1 &
ART_PID=$!
trap 'kill $ART_PID 2>/dev/null || true' EXIT
i=0
until curl -fs -o /dev/null http://127.0.0.1:8200/demo.tar.gz; do
	i=$((i + 1))
	[ "$i" -ge 15 ] && die "artifact file-server not up within 15s"
	sleep 1
done

# Deploy via Authorization: Bearer — Caddy access logs redact it
# automatically — exercised here under the real unit.
code=$(curl -s -o /tmp/deploy-body -w '%{http_code}' --max-time 90 \
	-X POST -H "Authorization: Bearer $TOKEN" \
	-d '{"url":"http://127.0.0.1:8200/demo.tar.gz","version":"s1"}' "$HOOK")
[ "$code" = "200" ] || {
	echo "deploy response: $(cat /tmp/deploy-body)"
	die "deploy under systemd sandbox failed with HTTP $code — if the journal shows a permission error, suspect the unit's sandboxing (ProtectSystem/XDG dirs) vs the packaged /var/lib dirs"
}
curl -fsS --max-time 5 "$PROXY/" | grep -q "hello smoke" \
	|| die "proxy does not serve the deployed app"
status=$(curl -s -H "Authorization: Bearer $TOKEN" "$HOOK")
case "$status" in
*'"current_version":"s1"'*) : ;;
*) die "status missing current_version s1: $status" ;;
esac
case "$status" in
*'"running":true'*) : ;;
*) die "status missing running:true: $status" ;;
esac
unit=$(printf '%s' "$status" | sed -n 's/.*"unit":"\([^"]*\)".*/\1/p')
[ -n "$unit" ] || die "status missing the app's systemd unit: $status"
# systemctl --user for a nologin system user: same trick hotserve
# itself uses — its own manager's private socket under /run/user/<uid>.
user_systemctl() { su -s /bin/sh hotserve -c "XDG_RUNTIME_DIR=/run/user/$uid systemctl --user $*"; }
user_systemctl is-active --quiet "$unit" \
	|| die "app unit $unit is not active under hotserve's user manager"
[ "$(user_systemctl show -p Restart --value "$unit")" = "no" ] \
	|| die "app unit must have Restart=no (the liveswap watchdog is the only restarter)"
journalctl --no-pager -t hotserve-demo | grep -q "smoke app starting" \
	|| die "app stdout did not reach the journal under identifier hotserve-demo"
# The runner's contract: every unit's NOFILE (soft and hard) is the
# user manager's DefaultLimitNOFILE — the very property it reads
# (liveswap/systemd_dbus.go). Assert against that, not a constant: a
# constant that happened to match Docker's PID 1 limit passed
# vacuously. Where the manager is systemd >= 256 the postinstall
# drop-in is what sets that ceiling, so pin it there; below 256 the
# drop-in does not reach the manager and the value is whatever PID 1
# had (#37).
mgr_nofile=$(user_systemctl show -p DefaultLimitNOFILE --value)
app_line=$(journalctl --no-pager -t hotserve-demo | grep 'smoke app starting' | tail -1)
app_soft=$(printf '%s' "$app_line" | grep -o 'nofile_soft=[0-9]*' | cut -d= -f2)
app_hard=$(printf '%s' "$app_line" | grep -o 'nofile_hard=[0-9]*' | cut -d= -f2)
[ -n "$mgr_nofile" ] && [ "$app_soft" = "$mgr_nofile" ] && [ "$app_hard" = "$mgr_nofile" ] \
	|| die "app NOFILE soft=$app_soft hard=$app_hard is not the user manager's DefaultLimitNOFILE ($mgr_nofile) on both"
app_nofile=$app_soft
sd_version=$(systemctl --version | awk 'NR==1 {print $2}')
if [ "${sd_version%%[!0-9]*}" -ge 256 ]; then
	[ "$mgr_nofile" = "1048576" ] \
		|| die "systemd $sd_version: user@$uid DefaultLimitNOFILE is $mgr_nofile, not the drop-in's 1048576"
	echo "app NOFILE $app_nofile = user@$uid DefaultLimitNOFILE = the drop-in's 1048576 (systemd $sd_version)"
else
	echo "app NOFILE $app_nofile = user@$uid DefaultLimitNOFILE (systemd $sd_version < 256: drop-in not applied, #37)"
fi
echo "deployed as unit $unit under user@$uid; app output in the journal"

# The sandbox tier: full (PID + user namespace) on systemd >= 256,
# filesystem (user namespace + mount set) below — the status endpoint
# reports it and the app's own view must agree. The host kernel's
# stance on unprivileged user namespaces is printed for the record:
# Ubuntu's AppArmor restriction (kernel.apparmor_restrict_unprivileged_userns)
# lives in the host kernel, not the container image, so a CI runner
# that restricts them would fail every cell here, not just Ubuntu's.
echo "host: $(uname -r); apparmor_restrict_unprivileged_userns=$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns 2>/dev/null || echo absent); virt=$(systemd-detect-virt 2>/dev/null || echo unknown)"
sandbox=$(printf '%s' "$status" | sed -n 's/.*"sandbox":"\([a-z]*\)".*/\1/p')
# Where the kernel restricts unprivileged user namespaces, the user
# manager must be running under the shipped profile — that is the
# package's whole answer to Ubuntu 24.04+ — and the tier below proves
# the profile actually lifts the restriction.
if [ "$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns 2>/dev/null)" = "1" ]; then
	# Diagnostics first: which link of the chain (securityfs → parser
	# → loaded profile → AppArmorProfile= on the unit) is missing shows
	# up here rather than as a bare label mismatch.
	echo "apparmor: enabled=$(cat /sys/module/apparmor/parameters/enabled 2>&1); securityfs=$(ls -d /sys/kernel/security/apparmor 2>&1); parser=$(command -v apparmor_parser || echo none)"
	echo "apparmor: loaded profiles matching hotserve: $(grep -c hotserve /sys/kernel/security/apparmor/profiles 2>&1)"
	echo "apparmor: parser says: $(apparmor_parser -r /etc/apparmor.d/hotserve-user-manager 2>&1 | head -c 300)"
	echo "apparmor: unit property: $(systemctl show -p AppArmorProfile --value "user@$uid.service")"
	journalctl --no-pager -b | grep -i "apparmor" | tail -5 || true
	mgr_pid=$(systemctl show -p MainPID --value "user@$uid.service")
	mgr_label=$(cat "/proc/$mgr_pid/attr/apparmor/current" 2>/dev/null || cat "/proc/$mgr_pid/attr/current" 2>/dev/null)
	case "$mgr_label" in
	hotserve-user-manager*) echo "user@$uid runs under the hotserve-user-manager AppArmor profile ($mgr_label)" ;;
	*) die "kernel restricts unprivileged user namespaces but user@$uid is not under the hotserve-user-manager profile (label '$mgr_label'); the sandbox would be off" ;;
	esac
fi
app_uidmap=$(printf '%s' "$app_line" | grep -o 'uidmap=[0-9]*' | cut -d= -f2)
app_pid=$(printf '%s' "$app_line" | grep -o ' pid=[0-9]*' | cut -d= -f2)
app_nprocs=$(printf '%s' "$app_line" | grep -o 'nprocs=[0-9]*' | cut -d= -f2)
app_hslib=$(printf '%s' "$app_line" | grep -o 'hotserve_lib=[a-z]*' | cut -d= -f2)
if [ "${sd_version%%[!0-9]*}" -ge 256 ]; then
	[ "$sandbox" = "full" ] || die "systemd $sd_version: status sandbox is '$sandbox', want full"
	[ "$app_pid" = "1" ] || die "full tier but the app is pid $app_pid inside its unit, not 1 (no PID namespace)"
	[ -n "$app_nprocs" ] && [ "$app_nprocs" -le 8 ] || die "full tier but /proc shows $app_nprocs pids inside the unit"
else
	[ "$sandbox" = "filesystem" ] || die "systemd $sd_version: status sandbox is '$sandbox', want filesystem"
fi
[ "$app_uidmap" != "4294967295" ] && [ -n "$app_uidmap" ] || die "no user namespace inside the unit (uid_map range $app_uidmap)"
[ "$app_hslib" = "closed" ] || die "/var/lib/hotserve is visible inside the sandboxed unit"
[ "$(user_systemctl show -p PrivateUsers --value "$unit")" = "yes" ] || die "unit lacks PrivateUsers=yes"
[ "$(user_systemctl show -p ProtectSystem --value "$unit")" = "strict" ] || die "unit lacks ProtectSystem=strict"
journalctl --no-pager -u hotserve | grep -q "launching without the full sandbox" && degraded=yes || degraded=no
if [ "$sandbox" = "full" ]; then
	[ "$degraded" = "no" ] || die "full tier must not log the degraded WARN"
else
	[ "$degraded" = "yes" ] || die "$sandbox tier must be warned about at launch"
fi
echo "sandbox tier $sandbox (systemd $sd_version): uid_map range $app_uidmap, pid $app_pid, $app_nprocs pids visible, /var/lib/hotserve $app_hslib"

# `hotserve validate` provisions and cleans up without starting: run
# as root (no user manager for uid 0) and as the hotserve user (the
# live manager) against the serving config, and the app must be
# untouched either way.
pid_live=$(printf '%s' "$status" | sed -n 's/.*"pid":\([0-9][0-9]*\).*/\1/p')
hotserve validate --config /etc/hotserve/Caddyfile >/dev/null 2>&1 \
	|| die "hotserve validate as root failed against a valid config"
su -s /bin/sh hotserve -c "hotserve validate --config /etc/hotserve/Caddyfile" >/dev/null 2>&1 \
	|| die "hotserve validate as the hotserve user failed against a valid config"
sleep 1
kill -0 "$pid_live" 2>/dev/null || die "hotserve validate stopped the running app (pid $pid_live)"
user_systemctl is-active --quiet "$unit" || die "hotserve validate left the app unit inactive"
curl -fsS --max-time 5 "$PROXY/" | grep -q "hello smoke" || die "app not served after validate"
echo "validate (root and hotserve) left the running app alone"

# The deploy token must never reach the journal: not via access logs
# (Authorization is redacted), not via any error path above.
journalctl -u hotserve --no-pager | grep -q "$TOKEN" \
	&& die "deploy token leaked into the journal" || true
echo "journal is free of the deploy token"

stage "stage 3: reinstall — upgrade path, conffile preservation, app survival"
pid_before=$(printf '%s' "$status" | sed -n 's/.*"pid":\([0-9][0-9]*\).*/\1/p')
[ -n "$pid_before" ] || die "status missing pid: $status"
dpkg -i "$deb"
grep -q liveswap_webhook /etc/hotserve/Caddyfile \
	|| die "reinstall clobbered the modified /etc/hotserve/Caddyfile (config|noreplace broken)"
systemctl is-active --quiet hotserve \
	|| die "service not active after reinstall — an upgrade must not leave the server down (preremove stop / missing postinstall restart)"
id hotserve >/dev/null || die "hotserve user gone after reinstall (postinstall not idempotent)"
# postinstall's try-restart swapped hotserve onto the new binary. The
# deployed app is a unit under the user manager, not a child of
# hotserve: it must still be the SAME process, reattached from
# state.json, and serving throughout.
i=0
until [ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" "$HOOK")" = "200" ]; do
	i=$((i + 1))
	[ "$i" -ge 30 ] && die "webhook not back within 30s of the upgrade restart"
	sleep 1
done
status=$(curl -s -H "Authorization: Bearer $TOKEN" "$HOOK")
pid_after=$(printf '%s' "$status" | sed -n 's/.*"pid":\([0-9][0-9]*\).*/\1/p')
[ "$pid_after" = "$pid_before" ] \
	|| die "app pid changed across the upgrade restart ($pid_before -> $pid_after): reattach failed, the app was relaunched (or is gone): $status"
case "$status" in *'"running":true'*) : ;; *) die "reattached app not running: $status" ;; esac
curl -fsS --max-time 5 "$PROXY/" | grep -q "hello smoke" || die "reattached app not served"
echo "reinstall preserved config and user; hotserve restarted and reattached to the running app (pid $pid_after)"

stage "stage 4: removal"
apt-get remove -y hotserve
systemctl is-active --quiet hotserve && die "service still active after remove (preremove did not stop it)" || true
[ ! -e /usr/bin/hotserve ] || die "/usr/bin/hotserve still present after remove"
[ -f /etc/hotserve/Caddyfile ] || die "conffile deleted on remove (should survive until purge)"
systemctl is-active --quiet "user@$uid.service" && die "user manager still running after remove (apps would linger)" || true
[ ! -e /etc/systemd/system/hotserve.service.d/10-user-manager.conf ] || die "user-manager drop-in left behind"
[ ! -e "/etc/systemd/system/user@$uid.service.d/10-hotserve.conf" ] || die "user@ limits drop-in left behind"
[ ! -e /var/lib/systemd/linger/hotserve ] || die "lingering left enabled"
kill -0 "$pid_after" 2>/dev/null && die "deployed app (pid $pid_after) survived package removal" || true
echo "removal stopped the service and the apps, kept the conffile"

echo ""
echo "ALL PACKAGE SMOKE STAGES PASSED ($deb on $(. /etc/os-release && echo "$PRETTY_NAME"))"
