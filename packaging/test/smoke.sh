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
#
# Stage 2c runs the backups README's "On a fresh box" lines — read out
# of the README itself, mounted at /README-backups.md, so the lines run
# are the lines written — as an administrator under sudo, against an
# S3 server in this container, and then holds the package to the table
# in backups/README.md: the timers enabled once, an administrator's
# disable kept across an upgrade, stopped and masked on remove, enabled
# again on reinstall, and purge keeping the credential file and saying
# why (expect_backup_state, a row per transition).
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
apt-get install -y "$deb" >/tmp/install.log 2>&1 || { cat /tmp/install.log; die "apt-get install of the package failed"; }
cat /tmp/install.log

id hotserve >/dev/null || die "postinstall did not create the hotserve user"
# The package ships no user-manager wrapper and no AppArmor profile:
# the support matrix is Debian 13, whose kernel does not restrict
# unprivileged user namespaces, so user@<uid>.service runs stock. A
# leftover from an older package would mean postinstall's cleanup did
# not run.
[ ! -e /usr/libexec/hotserve/user-manager ] \
	|| die "the retired user-manager wrapper is installed; the package should ship no such path"
[ ! -e /etc/apparmor.d/hotserve-user-manager ] \
	|| die "the retired AppArmor profile is installed; postinstall should have removed it"
# Every packaged file's mode is pinned in nfpm.yaml rather than
# inherited from the build host's umask; check the one that matters.
bmode=$(stat -c '%U:%G %a' /usr/bin/hotserve)
[ "$bmode" = "root:root 755" ] \
	|| die "/usr/bin/hotserve is '$bmode', want 'root:root 755' — an unpinned mode follows the builder's umask"
getent group hotserve >/dev/null || die "postinstall did not create the hotserve group"
# The package's directories are group-hotserve, so postinstall must
# ESTABLISH the account's membership rather than assume it — an account
# an admin or another package created keeps whatever groups it had.
id -nG hotserve | tr ' ' '\n' | grep -qx hotserve \
	|| die "the hotserve user is not in the hotserve group; postinstall must add it, not assume it"
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

# Type=notify, but Caddy sends READY=1 before it knows whether the
# config loaded, so enable --now returning 0 is not the readiness
# assertion — the curl below is.
timeout 300 systemctl enable --now hotserve || die "systemctl enable --now hotserve failed"
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
	|| die "hotserve restarted itself during install/reload (NRestarts != 0): it came up, died and systemd brought it back — see the journal"
# The unit's restart policy, read off the loaded unit: a crashed
# hotserve comes back, a start it refused (exit 1) is never retried.
[ "$(systemctl show -p Restart --value hotserve)" = "on-failure" ] \
	|| die "unit Restart is '$(systemctl show -p Restart --value hotserve)', want on-failure: a crashed hotserve would stay down"
[ "$(systemctl show -p RestartPreventExitStatus --value hotserve)" = "1" ] \
	|| die "unit RestartPreventExitStatus is '$(systemctl show -p RestartPreventExitStatus --value hotserve)', want 1: a refused start would loop"
[ "$(systemctl show -p RestartUSec --value hotserve)" = "1s" ] \
	|| die "unit RestartSec is '$(systemctl show -p RestartUSec --value hotserve)', want 1s"
[ "$(systemctl show -p TimeoutStartUSec --value hotserve)" = "4min" ] \
	|| die "unit TimeoutStartSec is '$(systemctl show -p TimeoutStartUSec --value hotserve)', want 4min: the start's bounded probe path (190s) must finish inside it, with margin"
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

# The user manager's PID, handed to the app so it can try the routes
# the sandbox is supposed to close. Captured before the config is
# written; the manager has been running since stage 1.
mgr_pid=$(systemctl show -p MainPID --value "user@$uid.service")
[ -n "$mgr_pid" ] && [ "$mgr_pid" != "0" ] || die "no MainPID for user@$uid.service"

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
			env MGR_PID __MGR_PID__
			env HOTSERVE_UID __HOTSERVE_UID__
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

# The heredoc above is quoted — it has to be, it carries Caddy's own
# {...} placeholders — so the two runtime values are substituted here.
# Without this the app receives the literal strings and every /proc
# probe below tests a path that cannot exist, passing vacuously.
sed -i "s|__MGR_PID__|$mgr_pid|; s|__HOTSERVE_UID__|$uid|" /etc/hotserve/Caddyfile
grep -q '__MGR_PID__\|__HOTSERVE_UID__' /etc/hotserve/Caddyfile \
	&& die "placeholder substitution failed; the sandbox probes would test nothing"

systemctl daemon-reload
timeout 300 systemctl restart hotserve || die "restart with liveswap config failed"

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
# `hotserve respond`, honoring liveswap's SOCKET contract and answering
# 200 on every path (which satisfies the health gate).
workdir=$(mktemp -d)
# Besides the NOFILE contract the wrapper records what its sandbox
# looks like from inside, by sourcing the shared view probe
# (liveswap/testdata/sandbox-view.sh — one asset with liveswap's integration test
# and e2e/liveswap/systemd.sh, mounted into this container by
# `make install-test`). Sourced, not run as a child, so the probe
# reports $$ as the unit's main pid; the NOFILE values below are
# inherited either way.
cp /sandbox-view.sh "$workdir/sandbox-view.sh" \
	|| die "the view probe is not mounted at /sandbox-view.sh; run this through 'make install-test'"
cat > "$workdir/server" <<'EOF'
#!/bin/sh
. ./sandbox-view.sh
echo "smoke app starting on $SOCKET"
# Caddy's unix//abs/path spelling: "unix/" + the absolute socket path.
exec /usr/bin/hotserve respond --listen "unix/$SOCKET" "hello smoke"
EOF
chmod +x "$workdir/server"
mkdir -p /srv/art
tar -czf /srv/art/demo.tar.gz -C "$workdir" server sandbox-view.sh

/usr/bin/hotserve file-server --listen 127.0.0.1:8200 --root /srv/art \
	>/tmp/artserver.log 2>&1 &
ART_PID=$!
S3_PID=""
trap 'kill $ART_PID $S3_PID 2>/dev/null || true' EXIT
i=0
until curl -fs -o /dev/null http://127.0.0.1:8200/demo.tar.gz; do
	i=$((i + 1))
	[ "$i" -ge 15 ] && die "artifact file-server not up within 15s"
	sleep 1
done

# A second app on disk, so root_listing below is a real sibling check.
# With only demo there, binding the WHOLE liveswap root into the unit
# would still list exactly "demo" and pass. This one is deliberately
# not in the config: it exists to be invisible, and its secret is what
# a leaking view would expose.
mkdir -p /var/lib/liveswap/other/shared
: > /var/lib/liveswap/other/shared/secret
chown -R hotserve:hotserve /var/lib/liveswap/other

# Deploy via Authorization: Bearer — Caddy access logs redact it
# automatically — exercised here under the real unit.
code=$(curl -s -o /tmp/deploy-body -w '%{http_code}' --max-time 90 \
	-X POST -H "Authorization: Bearer $TOKEN" \
	-d '{"url":"http://127.0.0.1:8200/demo.tar.gz","version":"s1"}' "$HOOK")
[ "$code" = "200" ] || {
	echo "deploy response: $(cat /tmp/deploy-body)"
	echo "--- app journal (hotserve-demo):"
	journalctl --no-pager -t hotserve-demo 2>/dev/null | tail -40
	die "deploy under systemd sandbox failed with HTTP $code — a deny-by-default view fails with ENOENT (or 203/EXEC for the command itself), not a permission error, so if the journal shows a missing path suspect the unit's base view rather than file modes"
}
curl -fsS --max-time 5 "$PROXY/" | grep -q "hello smoke" \
	|| die "proxy does not serve the deployed app"
status=$(curl -s --max-time 5 -H "Authorization: Bearer $TOKEN" "$HOOK")
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
	|| die "app unit must have Restart=no (apps are restarted by the liveswap watchdog, not systemd)"
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
# The view the app recorded from inside its unit. The shared probe
# writes it into the app's shared dir — the one writable, persistent
# path in the view — and smoke runs as root, so it is readable here.
# A file, not a journal line: the keys are the ones e2e and the
# integration lane assert too, and reading them back by name beats
# fifteen greps over one flat line.
view=/var/lib/liveswap/demo/shared/view.txt
i=0
until grep -q '^done=1' "$view" 2>/dev/null; do
	i=$((i + 1))
	[ "$i" -ge 20 ] && die "view.txt not written by the sandboxed app"
	sleep 0.5
done
probe_val() { sed -n "s/^$1=//p" "$view" | head -1; }
expect_probe() { # <key> <want>
	got=$(probe_val "$1")
	[ "$got" = "$2" ] || die "inside the unit: $1=$got, want $2"
}
# The evidence behind every in-unit assertion below, printed whether or
# not one fails: a cell that dies on one field is much easier to read
# next to the whole view the app actually saw.
echo "in-unit view:"
sed 's/^/  /' "$view"
app_soft=$(probe_val nofile_soft)
app_hard=$(probe_val nofile_hard)
[ -n "$mgr_nofile" ] && [ "$app_soft" = "$mgr_nofile" ] && [ "$app_hard" = "$mgr_nofile" ] \
	|| die "app NOFILE soft=$app_soft hard=$app_hard is not the user manager's DefaultLimitNOFILE ($mgr_nofile) on both"
app_nofile=$app_soft
sd_version=$(systemctl --version | awk 'NR==1 {print $2}')
# The user@<uid> drop-in only reaches the manager from systemd 256 (#37);
# the support matrix is Debian 13 (257), so this is unconditional.
[ "${sd_version%%[!0-9]*}" -ge 256 ] \
	|| die "systemd $sd_version is below the supported floor of 256 (Debian 13 ships 257)"
[ "$mgr_nofile" = "1048576" ] \
	|| die "systemd $sd_version: user@$uid DefaultLimitNOFILE is $mgr_nofile, not the drop-in's 1048576"
echo "app NOFILE $app_nofile = user@$uid DefaultLimitNOFILE = the drop-in's 1048576 (systemd $sd_version)"
echo "deployed as unit $unit under user@$uid; app output in the journal"

# The sandbox tier: full — a PID namespace on top of the user namespace
# and the mount set. Debian 13 is systemd 257, so there is one tier and
# the app's own view is the assertion (there is no status field: every
# unit gets the sandbox, so there is nothing to report).
# The host kernel's stance on unprivileged user namespaces is printed
# for the record: it lives in the host kernel, not the container image,
# so a CI runner that restricts them fails this cell even though Debian
# itself does not restrict them.
echo "host: $(uname -r); apparmor_restrict_unprivileged_userns=$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns 2>/dev/null || echo absent); virt=$(systemd-detect-virt 2>/dev/null || echo unknown)"
app_uidmap=$(probe_val uidmap)
app_nprocs=$(probe_val nprocs)
expect_probe pid 1
[ -n "$app_nprocs" ] && [ "$app_nprocs" -le 8 ] || die "/proc shows $app_nprocs pids inside the unit"
[ "$app_uidmap" != "4294967295" ] && [ -n "$app_uidmap" ] || die "no user namespace inside the unit (uid_map range $app_uidmap)"
expect_probe hotserve_lib closed
# These are the acceptance paths from DESIGN-sandbox.md. They are
# closed by the user namespace alone — the PID namespace on top is what
# additionally hides and protects sibling processes — so they are the
# assertions that must hold even if a host ever delivers less.
# The probes are only worth anything if the app was handed the real
# values: a literal "$mgr_pid" would make every /proc check below pass
# by testing a path that cannot exist. The probe echoes back what it
# was handed for exactly this comparison.
saw_pid=$(probe_val saw_mgr_pid)
saw_uid=$(probe_val saw_uid)
[ "$saw_pid" = "$mgr_pid" ] \
	|| die "the app was given MGR_PID='$saw_pid', not the manager's real pid $mgr_pid — the /proc probes would be vacuous"
[ "$saw_uid" = "$uid" ] \
	|| die "the app was given HOTSERVE_UID='$saw_uid', not $uid — the manager-socket probe would be vacuous"
# And prove the routes are reachable at all from outside the sandbox,
# so "closed" means the sandbox closed it rather than the path being
# absent on this host.
[ -r "/proc/$mgr_pid/environ" ] || die "/proc/$mgr_pid/environ is unreadable even to root; the in-unit probe would prove nothing"
[ -e "/run/user/$uid/systemd/private" ] || die "the manager socket does not exist; the in-unit probe would prove nothing"
[ -e /run/hotserve/admin.sock ] || die "the admin socket does not exist; the in-unit probe would prove nothing"
expect_probe sslprivate absent
for route in mgr_root mgr_environ mgr_socket admin_socket state etc_hotserve run_hotserve apptmp current; do
	got=$(probe_val "$route")
	[ "$got" = "closed" ] \
		|| die "$route is '$got' inside the unit: a route the design says is closed is open"
done
echo "in-unit routes closed: manager /proc root+environ, manager socket, admin socket, state.json, /etc/hotserve, /run/hotserve, the app's tmp and current"
# The app's own dirs are the other side of that: exactly its release
# (writable), its shared dir as $HOME, and a liveswap root that names
# no other app.
# Guarded: "demo " only means the sandbox hid the sibling if the
# sibling is actually on the host.
[ -d /var/lib/liveswap/other ] \
	|| die "the sibling app fixture is missing; root_listing would prove nothing"
expect_probe root_listing "demo "
expect_probe release writable
expect_probe root readonly
expect_probe home /var/lib/liveswap/demo/shared
expect_probe xdg_runtime unset
expect_probe cgroup readonly
expect_probe tmp writable
# Deny-by-default: nothing bound these, so they do not exist inside the
# unit — absent, not present-but-unreadable.
for d in opt srv home root mnt media etcliveswap; do
	expect_probe "abs_$d" absent
done
# /var/lib is the exception: the liveswap root lives under it, so it is
# present, and varlib_listing below is the real check.
expect_probe abs_varlib present
# The other half of the deny-by-default claim, and the one that keeps
# "closed" from being satisfied by an empty unit: the base view carries
# an OS the app can execute in, /etc is only the named entries (never
# the whole tree, which would hand every app every other app's
# env_file), and /var/lib holds nothing but the path to this app's own
# directories — hotserve's TLS keys sit next door on the host.
for present in binsh usrbinenv hsbin etcssl resolvconf; do
	got=$(probe_val "$present")
	[ "$got" = "ok" ] \
		|| die "$present is '$got' inside the unit: the base view does not carry a runnable OS, so the closed routes above prove nothing"
done
etclist=$(probe_val etc_listing)
# A non-empty guard in its own right: the case below has only failure
# branches, so an empty listing — /etc gone entirely rather than merely
# narrow — would match nothing and report success on the opposite
# regression to the one it is here to catch.
[ -n "$etclist" ] \
	|| die "/etc inside the unit lists nothing at all: the base view is empty, not narrow"
case " $etclist" in
*" hotserve "* | *" liveswap "* | *" shadow "*)
	die "/etc inside the unit carries more than the named entries ($etclist): every app would see every other app's env_file" ;;
esac
varliblist=$(probe_val varlib_listing)
[ "$varliblist" = "liveswap " ] \
	|| die "/var/lib inside the unit is '$varliblist', want only 'liveswap': anything else is a path nothing named"
echo "in-unit base view: /bin/sh, /usr/bin/env, /usr/bin/hotserve, /etc/ssl/certs, /etc/resolv.conf; /etc=$etclist /var/lib=$varliblist"
[ "$(user_systemctl show -p PrivateUsers --value "$unit")" = "yes" ] || die "unit lacks PrivateUsers=yes"
[ "$(user_systemctl show -p TemporaryFileSystem --value "$unit")" = "/:ro" ] || die "unit lacks TemporaryFileSystem=/:ro — the view is not deny-by-default"
# The retired half of the old model, read back off the live unit. Both
# properties still exist on every service, so test the value rather
# than its presence: what must be gone is the setting, not the row.
[ "$(user_systemctl show -p ProtectSystem --value "$unit")" != "strict" ] || die "unit still sets ProtectSystem=strict; the view names what exists rather than making the rest read-only"
case "$(user_systemctl show -p InaccessiblePaths --value "$unit")" in
*/*) die "unit still masks a list with InaccessiblePaths=" ;;
esac
echo "sandbox (systemd $sd_version): uid_map range $app_uidmap, pid $(probe_val pid), $app_nprocs pids visible, /var/lib/hotserve $(probe_val hotserve_lib)"

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

stage "stage 2b: a SIGKILLed hotserve is back within seconds, reattached"
# Stage 3's upgrade restart is a clean stop and start, which never
# exercises Restart=; this is the one lane that runs the installed
# .deb's unit, so the crash path is proven here too. The unit's own
# record is the assertion, never a systemctl exit status.
hp0=$(systemctl show -p ExecMainPID --value hotserve)
systemctl kill --kill-whom=main -s SIGKILL hotserve
i=0
until [ "$(systemctl show -p ActiveState --value hotserve)" = "active" ] \
	&& [ "$(systemctl show -p ExecMainPID --value hotserve)" != "$hp0" ]; do
	i=$((i + 1))
	[ "$i" -ge 60 ] && die "hotserve not back within 30s of SIGKILL: $(systemctl show -p ActiveState,SubState,Result,NRestarts hotserve | tr '\n' ' ')"
	sleep 0.5
done
[ "$(systemctl show -p NRestarts --value hotserve)" = "1" ] \
	|| die "NRestarts is $(systemctl show -p NRestarts --value hotserve), want 1: more than one automatic restart after a single kill"
deadline=$(($(date +%s) + 30))
until [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 -H "Authorization: Bearer $TOKEN" "$HOOK")" = "200" ]; do
	[ "$(date +%s)" -ge "$deadline" ] && die "webhook not back within 30s of the automatic restart"
	sleep 1
done
status=$(curl -s --max-time 5 -H "Authorization: Bearer $TOKEN" "$HOOK")
[ "$(printf '%s' "$status" | sed -n 's/.*"pid":\([0-9][0-9]*\).*/\1/p')" = "$pid_live" ] \
	|| die "app pid changed across hotserve's SIGKILL restart: reattach failed: $status"
curl -fsS --max-time 5 "$PROXY/" | grep -q "hello smoke" || die "app not served after hotserve's automatic restart"
echo "SIGKILL: systemd restarted hotserve (NRestarts=1); app pid $pid_live reattached"

stage "stage 2c: backups — the README's 'On a fresh box' lines, as an administrator"
# What the package set up, before any line of the README runs: the
# programs it recommends (apt brings Recommends by default), the
# account as setup makes it, the four unit files, the timers enabled
# and running and their services untouched, and no credential file —
# that is setup's, and postinstall says so.
S3REPO=s3:http://127.0.0.1:9000/hotserve
S3KEY=AKIDSMOKEFIXTURE
S3SECRET=smoke-fixture-key-not-a-secret
CRED=/etc/hotserve-backup/repository.env
TIMERS="hotserve-backup.timer hotserve-backup-drill.timer"
# expect_backup_state <step> <is-enabled> <is-active> <absent|sha256>:
# one row of the table, for both timers and the credential file.
expect_backup_state() {
	local u got
	for u in $TIMERS; do
		got=$(systemctl is-enabled "$u" 2>/dev/null || true)
		[ "$got" = "$2" ] || die "$1: $u is-enabled '$got', want '$2'"
		got=$(systemctl is-active "$u" 2>/dev/null || true)
		[ "$got" = "$3" ] || die "$1: $u is-active '$got', want '$3'"
	done
	if [ "$4" = absent ]; then
		[ ! -e "$CRED" ] || die "$1: $CRED exists, and the package must not make it"
	else
		got=$(sha256sum "$CRED" | cut -d' ' -f1)
		[ "$got" = "$4" ] || die "$1: $CRED changed (was $4, is $got)"
	fi
	echo "$1: timers $2, $3; credential file $4 — as the table says"
}
command -v restic >/dev/null && command -v sqlite3 >/dev/null \
	|| die "restic and sqlite3 did not come with the package: Recommends: did not resolve (apt installs them by default)"
getent passwd hotserve-backup | grep -q ':/nonexistent:/usr/sbin/nologin$' \
	|| die "postinstall did not make the hotserve-backup account as setup does: $(getent passwd hotserve-backup || echo none)"
for u in hotserve-backup.service hotserve-backup-drill.service $TIMERS; do
	got=$(stat -c '%U:%G %a' "/lib/systemd/system/$u")
	[ "$got" = "root:root 644" ] || die "/lib/systemd/system/$u is '$got', want 'root:root 644'"
done
expect_backup_state "after install" enabled active absent
[ ! -e /etc/hotserve-backup ] || die "the package made /etc/hotserve-backup; that is setup's"
for u in hotserve-backup.service hotserve-backup-drill.service; do
	[ "$(systemctl show -p ActiveState --value "$u")" = inactive ] && [ "$(systemctl show -p Result --value "$u")" = success ] \
		|| die "$u after install: $(systemctl show -p ActiveState,Result "$u" | tr '\n' ' ') — nothing may start before setup"
done
grep -q "sudo hotserve-backup setup <repository>" /tmp/install.log \
	|| die "postinstall did not say that the timers wait for setup"
grep -q "refuse that account" /tmp/install.log \
	&& die "postinstall warned of the account it had just made: $(grep hotserve-backup /tmp/install.log)" || true
hotserve-backup account >/dev/null || die "the account postinstall made is not one setup accepts: $(hotserve-backup account 2>&1)"
echo "the package's own: restic and sqlite3 by Recommends, the account, the units, the timers enabled and waiting"

# The repository the README's lines are pointed at: rclone serve s3
# here, on the loopback, with a key that is no secret.
mkdir -p /srv/s3
rclone serve s3 --addr 127.0.0.1:9000 --auth-key "$S3KEY,$S3SECRET" /srv/s3 >/tmp/s3.log 2>&1 &
S3_PID=$!
i=0
until curl -s -o /dev/null http://127.0.0.1:9000/; do
	i=$((i + 1))
	[ "$i" -ge 30 ] && die "rclone serve s3 not up within 15s: $(cat /tmp/s3.log)"
	sleep 0.5
done
# The administrator the README is written for: not root, sudo.
useradd -m admin
echo 'admin ALL=(ALL) NOPASSWD: ALL' >/etc/sudoers.d/admin
chmod 0440 /etc/sudoers.d/admin
visudo -c -q || die "the smoke's sudoers line does not parse"
# runuser, not `su -`: su -c drops the controlling terminal (sudo
# then finds no /dev/tty to ask at), runuser keeps it [measured].
as_admin() { runuser -u admin -- sh -c "$1"; }
# The app's data, as the app itself would make it: a database and an
# uploads directory under its shared/, the hotserve user's.
su -s /bin/sh hotserve -c 'cd /var/lib/liveswap/demo/shared && sqlite3 app.db "create table t(n); insert into t values (1),(2);" && mkdir -p uploads && echo pic >uploads/a.png' \
	|| die "could not seed the demo app's data"
# Step 1 of the README, the declaration in the app's block, put there
# the way an administrator would edit it in.
sed -i 's|^\t\tapp demo {$|&\n\t\t\tbackup {\n\t\t\t\tsqlite app.db\n\t\t\t\tfiles  uploads\n\t\t\t}|' /etc/hotserve/Caddyfile
[ "$(grep -c 'sqlite app.db' /etc/hotserve/Caddyfile)" = 1 ] || die "the backup block did not go into the demo app's block"

# The README's lines, between its smoke markers; the example
# repository URL is this box's, and the app is demo, as written.
OUT=/tmp/docs.out
. /tty.sh
mapfile -t docs < <(awk '/<!-- smoke: begin -->/{on=1; next} /<!-- smoke: end -->/{on=0} on' /README-backups.md | grep -v '^```' | grep -v '^#' | grep -v '^$' | sed "s|s3:https://s3.example.com/my-backups|$S3REPO|")
[ "${#docs[@]}" -ge 6 ] || die "the README's smoke block holds ${#docs[@]} lines; the markers moved?"
for line in "${docs[@]}"; do
	echo "README: $line"
	case "$line" in
	"sudo hotserve-backup setup "*)
		converse "runuser -u admin -- sh -c '$line'" "$P_KEY" "$S3KEY" "$P_SECRET" "$S3SECRET" "$P_STORED" stored \
			|| die "setup as the administrator failed: $(cat "$OUT")"
		grep -q "account hotserve-backup: present" "$OUT" || die "setup did not find the package's account: $(cat "$OUT")"
		grep -q "repository ready: $S3REPO (new, id " "$OUT" || die "setup did not make the repository: $(cat "$OUT")"
		grep -q "^next: the first backup runs within the hour and ten minutes (systemctl list-timers hotserve-backup.timer)" "$OUT" \
			|| die "setup's closing line does not name the timer: $(grep next: "$OUT")"
		grep -q "$S3SECRET" "$OUT" && die "the storage secret was echoed at the terminal" || true
		[ "$(stat -c '%U %a' "$CRED")" = "root 600" ] || die "$CRED is '$(stat -c '%U %a' "$CRED")', want 'root 600'"
		CRED_SHA=$(sha256sum "$CRED" | cut -d' ' -f1)
		expect_backup_state "after setup" enabled active "$CRED_SHA"
		# The README's next claim: the first backup comes from the timer,
		# within the hour and ten minutes. Not waited an hour for: a
		# drop-in makes the timer a minute's, one firing is watched, the
		# drop-in goes. The service it starts is the shipped one.
		# A persistent timer writes its stamp at its first activation —
		# postinstall's start — and fires nothing; the service has not
		# run yet, and what will say it ran is its main process's start
		# time (LastTriggerUSec shows the stamp's time after a restart,
		# which is no firing) [M61].
		[ -f /var/lib/systemd/timers/stamp-hotserve-backup.timer ] || die "the timer's first activation at install wrote no stamp"
		started0=$(systemctl show -p ExecMainStartTimestampMonotonic --value hotserve-backup.service)
		[ "$started0" = 0 ] || die "the backup service ran before anything asked it to (main process started at $started0)"
		mkdir -p /run/systemd/system/hotserve-backup.timer.d
		printf '[Timer]\nOnCalendar=\nOnCalendar=*-*-* *:*:00\nRandomizedDelaySec=0\n' >/run/systemd/system/hotserve-backup.timer.d/10-smoke.conf
		systemctl daemon-reload
		systemctl restart hotserve-backup.timer
		i=0
		until [ "$(systemctl show -p ExecMainStartTimestampMonotonic --value hotserve-backup.service)" != "$started0" ] || [ "$i" -ge 150 ]; do
			i=$((i + 1))
			sleep 0.5
		done
		[ "$i" -lt 150 ] || die "the hourly timer, made a minute's, did not start the service within 75s: $(systemctl show -p LastTriggerUSec,NextElapseUSecRealtime,ActiveState hotserve-backup.timer | tr '\n' ' '); $(systemctl show -p ActiveState,Result,ConditionResult hotserve-backup.service | tr '\n' ' ')"
		i=0
		until [ "$(systemctl show -p ActiveState --value hotserve-backup.service)" = inactive ] || [ "$i" -ge 240 ]; do
			i=$((i + 1))
			sleep 0.5
		done
		rm -rf /run/systemd/system/hotserve-backup.timer.d
		systemctl daemon-reload
		systemctl restart hotserve-backup.timer
		[ "$(systemctl show -p Result --value hotserve-backup.service)" = success ] \
			|| die "the run the timer fired ended '$(systemctl show -p Result --value hotserve-backup.service)': $(journalctl -u hotserve-backup.service --no-pager | tail -20)"
		[ -f /var/lib/hotserve-backup/status.json ] || die "the timer's run wrote no record"
		echo "the timer fired ($((i / 2))s after the minute), and its run ended success under the unit's hardening"
		;;
	"sudo hotserve-backup restore demo")
		printf 'demo\n' | as_admin "$line" >"$OUT" 2>&1 || die "the restore into place failed: $(cat "$OUT")"
		grep -q "demo: backed up first: snapshot [0-9a-f]\{8\} (restore --snapshot" "$OUT" || die "the restore into place did not back the app up first: $(cat "$OUT")"
		grep -q "demo: restored from snapshot" "$OUT" || die "the restore into place restored nothing: $(cat "$OUT")"
		;;
	"hotserve-backup status")
		as_admin "$line" >"$OUT" 2>&1 || die "status as the administrator, no sudo, exits $?: $(cat "$OUT")"
		grep -q "^demo: ok: " "$OUT" || die "status does not say demo is ok: $(cat "$OUT")"
		grep -q "restore last proven" "$OUT" || die "status does not say demo's restore is proven: $(cat "$OUT")"
		;;
	*)
		as_admin "$line" >"$OUT" 2>&1 || die "'$line' as the administrator exits $?: $(cat "$OUT")"
		;;
	esac
done
[ -f /root/demo-restored/uploads/a.png ] && [ "$(sqlite3 /root/demo-restored/app.db 'select count(*) from t')" = 2 ] \
	|| die "the restore --to put nothing there: $(find /root/demo-restored 2>&1 | head)"
# Not in the README's lines, and true of them: the --to rule, and the
# pre-restore snapshot the restore into place named.
as_admin "sudo hotserve-backup restore demo --to /tmp/demo-restored" >"$OUT" 2>&1 && die "a restore --to /tmp was accepted" || true
grep -q "not /tmp — /root, /srv, /var/backups, or a root-owned directory of your own" "$OUT" || die "the --to rule's words are missing: $(cat "$OUT")"
[ ! -e /tmp/demo-restored ] || die "a refused --to made its directory"
snaps=$(systemd-run --quiet --pipe --wait --collect -p User=hotserve-backup -p EnvironmentFile=$CRED -p CacheDirectory=hotserve-backup -E RESTIC_CACHE_DIR=/var/cache/hotserve-backup -E HOME=/nonexistent /usr/bin/restic snapshots --no-lock --json --tag pre-restore 2>/dev/null | grep -o '"short_id"' | wc -l)
[ "$snaps" = 1 ] || die "want one pre-restore snapshot, the repository holds $snaps"
curl -fsS --max-time 5 "$PROXY/" | grep -q "hello smoke" || die "the app is not served after the backups"
echo "the README's lines ran as written, as the administrator: validate, reload, setup, a run, status, a restore --to, a restore into place with its pre-restore snapshot"

stage "stage 3: reinstall — upgrade path, conffile preservation, app survival"
pid_before=$(printf '%s' "$status" | sed -n 's/.*"pid":\([0-9][0-9]*\).*/\1/p')
[ -n "$pid_before" ] || die "status missing pid: $status"
# The account already exists by definition on a reinstall, which is the
# path that used to set the group only when it CREATED the user. Strip
# the membership first, the way a hotserve account made by an admin (or
# another package) would arrive: postinstall must establish it rather
# than assume it.
# postinstall's useradd makes hotserve the account's PRIMARY group, so
# it cannot simply be removed; move the account to another primary
# group instead, which is exactly the shape a pre-existing account has.
groupadd --system smoketest-other 2>/dev/null || true
usermod -g smoketest-other hotserve \
	|| die "could not move the hotserve account off its primary group; the reinstall assertion below would be vacuous"
id -nG hotserve | tr ' ' '\n' | grep -qx hotserve \
	&& die "the hotserve account is still in the hotserve group; the reinstall assertion below would be vacuous"
dpkg -i "$deb"
id -nG hotserve | tr ' ' '\n' | grep -qx hotserve \
	|| die "reinstall did not restore the hotserve user's group membership: the package's group-owned directories would be unreachable"
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
status=$(curl -s --max-time 5 -H "Authorization: Bearer $TOKEN" "$HOOK")
pid_after=$(printf '%s' "$status" | sed -n 's/.*"pid":\([0-9][0-9]*\).*/\1/p')
[ "$pid_after" = "$pid_before" ] \
	|| die "app pid changed across the upgrade restart ($pid_before -> $pid_after): reattach failed, the app was relaunched (or is gone): $status"
case "$status" in *'"running":true'*) : ;; *) die "reattached app not running: $status" ;; esac
curl -fsS --max-time 5 "$PROXY/" | grep -q "hello smoke" || die "reattached app not served"
echo "reinstall preserved config and user; hotserve restarted and reattached to the running app (pid $pid_after)"
# The table's upgrade rows: as installed, the timers stay enabled and
# running and the credential file is not touched; disabled by the
# administrator, an upgrade leaves them disabled (was-enabled is false,
# and deb-systemd-invoke starts nothing disabled); enabled again, an
# upgrade keeps them so.
expect_backup_state "after an upgrade" enabled active "$CRED_SHA"
as_admin "sudo systemctl disable --now hotserve-backup.timer hotserve-backup-drill.timer"
expect_backup_state "disabled by the administrator" disabled inactive "$CRED_SHA"
dpkg -i "$deb" >/dev/null
expect_backup_state "after an upgrade, disabled before it" disabled inactive "$CRED_SHA"
as_admin "sudo systemctl enable --now hotserve-backup.timer hotserve-backup-drill.timer"
dpkg -i "$deb" >/dev/null
expect_backup_state "after an upgrade, enabled before it" enabled active "$CRED_SHA"
systemctl is-active --quiet hotserve || die "hotserve not active after the upgrade cycle"
# An upgrade stops a backup under way before the new binary is
# unpacked: the old run would otherwise start its remaining helper
# units from the new version's binary. Staged with a run held at its
# upload; the upgrade has to end it, its units and its plaintext, and
# leave the timers as they were.
su -s /bin/sh hotserve -c 'head -c 50000000 /dev/urandom >/var/lib/liveswap/demo/shared/uploads/big.bin'
systemctl start --no-block hotserve-backup.service
i=0
until up=$(systemctl list-units --plain --no-legend --state=activating 'hotserve_backup_upload_*' | awk '{print $1}' | head -1) && [ -n "$up" ]; do
	i=$((i + 1))
	[ "$i" -ge 300 ] && die "no upload unit appeared under the service: the upgrade row would prove nothing"
	sleep 0.1
done
rpid=$(systemctl show -p ExecMainPID --value "$up")
[ -n "$rpid" ] && [ "$rpid" != 0 ] && kill -STOP "$rpid" || die "could not hold the upload's restic (pid '$rpid')"
dpkg -i "$deb" >/tmp/upgrade-run.log 2>&1 || { cat /tmp/upgrade-run.log; die "the upgrade with a run under way failed"; }
left=$(systemctl list-units --plain --no-legend --state=active,activating,deactivating 'hotserve_backup_*' | awk '{print $1}' | tr '\n' ' ')
[ -z "$left" ] || die "units of the old run are still running after the upgrade: $left"
[ "$(systemctl show -p ActiveState --value hotserve-backup.service)" != activating ] \
	|| die "the old run is still under way after the upgrade"
kill -0 "$rpid" 2>/dev/null && die "the old run's restic (pid $rpid) survived the upgrade" || true
[ -z "$(find /var/lib/hotserve-backup/staging -mindepth 2 2>/dev/null)" ] \
	|| die "the stopped run left plaintext in staging: $(find /var/lib/hotserve-backup/staging -mindepth 2)"
systemctl reset-failed hotserve-backup.service 2>/dev/null || true
su -s /bin/sh hotserve -c 'rm -f /var/lib/liveswap/demo/shared/uploads/big.bin'
expect_backup_state "after an upgrade with a run under way" enabled active "$CRED_SHA"
systemctl start hotserve-backup.service || die "a run after that upgrade failed: $(journalctl -u hotserve-backup.service --no-pager | tail -20)"
echo "an upgrade stopped the run under way, its units and its plaintext; the timers as they were, and the next run works"
# The account is every run's to check, and an upgrade says so in
# setup's own words: put in the hotserve group — where restic and the
# plan unit would read the apps' env files — it is warned of at the
# upgrade, refused by a run, and accepted again once taken out.
usermod -aG hotserve hotserve-backup
dpkg -i "$deb" >/tmp/upgrade-group.log 2>&1 || { cat /tmp/upgrade-group.log; die "the upgrade with the account in the hotserve group failed: a warning must not fail an install"; }
grep -q "the hotserve group among its groups" /tmp/upgrade-group.log && grep -q "refuse that account" /tmp/upgrade-group.log \
	|| die "postinstall did not warn of the account in the hotserve group: $(cat /tmp/upgrade-group.log)"
grep -q "usermod --shell" /tmp/upgrade-group.log && die "the warning names a usermod of the shell for a fault of who the account is" || true
systemctl start hotserve-backup.service && die "a run with the account in the hotserve group exited 0" || true
journalctl -u hotserve-backup.service --no-pager | grep -q "the hotserve group among its groups" \
	|| die "the run did not say why it refused: $(journalctl -u hotserve-backup.service --no-pager | tail -5)"
gpasswd -d hotserve-backup hotserve >/dev/null
systemctl reset-failed hotserve-backup.service 2>/dev/null || true
hotserve-backup account >/dev/null || die "the account is not accepted once out of the group: $(hotserve-backup account 2>&1)"
systemctl start hotserve-backup.service || die "a run after the account was mended failed: $(journalctl -u hotserve-backup.service --no-pager | tail -20)"
echo "in the hotserve group: warned of at the upgrade in setup's words, refused by a run, accepted once out of it"

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
# The table's remove row: the timers stopped and masked, the unit files
# gone, the credential file and the account kept.
expect_backup_state "after remove" masked inactive "$CRED_SHA"
for u in hotserve-backup.service hotserve-backup-drill.service $TIMERS; do
	[ ! -e "/lib/systemd/system/$u" ] || die "/lib/systemd/system/$u still present after remove"
done
systemctl is-active --quiet hotserve-backup.service && die "a backup run is still active after remove" || true
getent passwd hotserve-backup >/dev/null || die "the hotserve-backup account was removed with the package"
[ ! -e /usr/bin/hotserve-backup ] || die "/usr/bin/hotserve-backup still present after remove"

stage "stage 5: reinstall after remove, and purge"
apt-get install -y "$deb" >/tmp/reinstall.log 2>&1 || { cat /tmp/reinstall.log; die "reinstall after remove failed"; }
expect_backup_state "reinstalled after remove" enabled active "$CRED_SHA"
timeout 300 systemctl enable --now hotserve || die "hotserve did not start after the reinstall"
systemctl start hotserve-backup.service || die "a run after the reinstall failed: $(journalctl -u hotserve-backup.service --no-pager | tail -20)"
[ "$(systemctl show -p Result --value hotserve-backup.service)" = success ] || die "the run after the reinstall ended '$(systemctl show -p Result --value hotserve-backup.service)'"
echo "reinstalled after remove: the timers enabled and running again, the same credential file, a run works"
# What a killed run can leave behind: a bind mount of an app's data
# under the run directory, and the record writer's temp file. Purge
# must remove nothing of the app's through the first, and not be
# defeated by the second.
mkdir -p /run/hotserve-backup/deadrun/m0
mount --bind /var/lib/liveswap/demo/shared /run/hotserve-backup/deadrun/m0 || die "could not stage a leftover bind mount"
[ -f /run/hotserve-backup/deadrun/m0/app.db ] || die "the staged mount does not show the app's data: the purge check would prove nothing"
: >/var/lib/hotserve-backup/.status-crash
apt-get purge -y hotserve >/tmp/purge.log 2>&1 || { cat /tmp/purge.log; die "apt-get purge failed"; }
[ -f /var/lib/liveswap/demo/shared/app.db ] && [ -f /var/lib/liveswap/demo/shared/uploads/a.png ] \
	|| die "purge removed the app's data through a mount a killed run left under /run/hotserve-backup"
umount /run/hotserve-backup/deadrun/m0 && rm -rf /run/hotserve-backup
cat /tmp/purge.log
grep -q "$CRED is kept: it holds the repository password, the only way to read the backups already made" /tmp/purge.log \
	|| die "purge did not say that the credential file is kept, and why"
[ "$(sha256sum "$CRED" | cut -d' ' -f1)" = "$CRED_SHA" ] || die "purge changed or removed $CRED"
[ ! -e /var/lib/hotserve-backup/status.json ] && [ ! -e /var/lib/hotserve-backup/repository-id ] || die "purge left the record or the repository id"
[ ! -e /var/lib/hotserve-backup ] || die "purge left /var/lib/hotserve-backup, though nothing of an app's was in it: $(find /var/lib/hotserve-backup)"
[ ! -e /var/cache/hotserve-backup ] || die "purge left restic's cache"
getent passwd hotserve-backup >/dev/null || die "purge removed the hotserve-backup account"
for u in $TIMERS; do
	[ ! -e "/etc/systemd/system/$u" ] || die "purge left $u masked"
	[ ! -e "/etc/systemd/system/timers.target.wants/$u" ] || die "purge left $u enabled"
	[ ! -e "/var/lib/systemd/deb-systemd-helper-enabled/$u.dsh-also" ] || die "purge left the helper's state for $u"
done
[ ! -f /etc/hotserve/Caddyfile ] || die "purge left the conffile"
echo "purge kept the credential file and said why, removed the package's own state and the cache, kept the account"

echo ""
echo "ALL PACKAGE SMOKE STAGES PASSED ($deb on $(. /etc/os-release && echo "$PRETTY_NAME"))"
