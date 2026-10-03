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
# Stage 2c runs the "Set it up" lines of the backups guide — read out
# of docs/backups.md itself, mounted at /docs-backups.md, so the lines
# run are the lines written — as an administrator under sudo, against an
# S3 server in this container, and then holds the package to the table
# in backups/README.md: the timers enabled once, an administrator's
# disable kept across an upgrade, stopped and masked on remove, enabled
# again on reinstall, and purge keeping the credential file and saying
# why (expect_backup_state, a row per transition). And to the edges of
# each transition (#154): a run under way at an upgrade left alone to
# finish, the timers' stamps and a killed run's mounts gone with the
# package, and — stage 6 — an install where the account cannot be made,
# and what purge keeps said for what it is.
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

# The root mount as a box systemd has booted has it: shared. In a
# container systemd leaves it as it found it, private, and what a
# shared mount does is then never seen: a recursive bind of it is its
# peer, and unmounting a disk beneath the bind unmounts the disk
# beneath what was bound [M71].
mount --make-rshared /
[ "$(findmnt -no PROPAGATION /)" = shared ] || die "the root mount is $(findmnt -no PROPAGATION /), not shared: the mount rows would prove nothing of a real box"

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

stage "stage 2c: backups — the guide's 'Set it up' lines, as an administrator"
# What the package set up, before any line of the guide runs: the
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
		[ "$got" = "$3" ] || die "$1: $u is-active '$got', want '$3' ($(systemctl show -p Result "$u"); $(journalctl -u "$u" -u "${u%.timer}.service" --no-pager | tail -12 | tr '\n' '|'))"
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

# The repository the guide's lines are pointed at: rclone serve s3
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
# The administrator the guide is written for: not root, sudo.
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
# Step 1 of the guide, the declaration in the app's block, put there
# the way an administrator would edit it in.
sed -i 's|^\t\tapp demo {$|&\n\t\t\tbackup {\n\t\t\t\tsqlite app.db\n\t\t\t\tfiles  uploads\n\t\t\t}|' /etc/hotserve/Caddyfile
[ "$(grep -c 'sqlite app.db' /etc/hotserve/Caddyfile)" = 1 ] || die "the backup block did not go into the demo app's block"

# The guide's lines, between its smoke markers; the example
# repository URL is this box's, and the app is demo, as written.
OUT=/tmp/docs.out
. /tty.sh
mapfile -t docs < <(awk '/<!-- smoke: begin -->/{on=1; next} /<!-- smoke: end -->/{on=0} on' /docs-backups.md | grep -v '^```' | grep -v '^#' | grep -v '^$' | sed "s|s3:https://s3.example.com/my-backups|$S3REPO|")
[ "${#docs[@]}" -ge 6 ] || die "the guide's smoke block holds ${#docs[@]} lines; the markers moved?"
for line in "${docs[@]}"; do
	echo "docs/backups.md: $line"
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
		# The guide's next claim: the first backup comes from the timer,
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
# Not in the guide's lines, and true of them: the --to rule, and the
# pre-restore snapshot the restore into place named.
as_admin "sudo hotserve-backup restore demo --to /tmp/demo-restored" >"$OUT" 2>&1 && die "a restore --to /tmp was accepted" || true
grep -q "not /tmp — /root, /srv, /var/backups, or a root-owned directory of your own" "$OUT" || die "the --to rule's words are missing: $(cat "$OUT")"
[ ! -e /tmp/demo-restored ] || die "a refused --to made its directory"
snaps=$(systemd-run --quiet --pipe --wait --collect -p User=hotserve-backup -p EnvironmentFile=$CRED -p CacheDirectory=hotserve-backup -E RESTIC_CACHE_DIR=/var/cache/hotserve-backup -E HOME=/nonexistent /usr/bin/restic snapshots --no-lock --json --tag pre-restore 2>/dev/null | grep -o '"short_id"' | wc -l)
[ "$snaps" = 1 ] || die "want one pre-restore snapshot, the repository holds $snaps"
curl -fsS --max-time 5 "$PROXY/" | grep -q "hello smoke" || die "the app is not served after the backups"
echo "the guide's lines ran as written, as the administrator: validate, reload, setup, a run, status, a restore --to, a restore into place with its pre-restore snapshot"

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
# The timers as dh_installsystemd has them (the owner, 2026-09-27): an
# upgrade restarts a timer that is enabled, one an administrator
# stopped among them — the README says so, and to disable it to keep
# it off — and leaves one the administrator masked masked.
as_admin "sudo systemctl stop hotserve-backup.timer"
as_admin "sudo systemctl mask --now hotserve-backup-drill.timer"
dpkg -i "$deb" >/dev/null
[ "$(systemctl is-active hotserve-backup.timer || true)" = active ] \
	|| die "after an upgrade, a timer stopped and left enabled is $(systemctl is-active hotserve-backup.timer || true): dh_installsystemd restarts it"
# (Debian's helper asks the manager to restart it all the same, which
# a mask refuses: masked, and failed rather than inactive, and not
# running either way.)
[ "$(systemctl is-enabled hotserve-backup-drill.timer || true)" = masked ] && [ "$(systemctl is-active hotserve-backup-drill.timer || true)" != active ] \
	|| die "after an upgrade, the administrator's mask of hotserve-backup-drill.timer is $(systemctl is-enabled hotserve-backup-drill.timer || true), $(systemctl is-active hotserve-backup-drill.timer || true)"
as_admin "sudo systemctl unmask hotserve-backup-drill.timer"
systemctl reset-failed hotserve-backup-drill.timer 2>/dev/null || true
as_admin "sudo systemctl start hotserve-backup-drill.timer"
expect_backup_state "after an upgrade, one timer stopped and one masked, and the mask taken off" enabled active "$CRED_SHA"
systemctl is-active --quiet hotserve || die "hotserve not active after the upgrade cycle"
# A user directory that does not answer: postinstall's lookups of the
# accounts are bounded, and one that does not answer is "could not
# tell" — never "absent", which would make a local account beside the
# directory's — and the configure completes (Copilot on #155: dpkg
# hung). Staged with a getent ahead of the real one on dpkg's PATH that
# never answers for the backup account.
# It ignores the SIGTERM a timeout sends first, as a lookup stuck in
# the kernel does: only the kill that follows ends it.
printf '#!/bin/sh\ncase "$*" in *hotserve-backup*) trap "" TERM; sleep 600; exit 0 ;; esac\nexec /usr/bin/getent "$@"\n' >/usr/local/sbin/getent
chmod 755 /usr/local/sbin/getent
t0=$(date +%s)
timeout 300 dpkg -i "$deb" >/tmp/upgrade-nss.log 2>&1 || { cat /tmp/upgrade-nss.log; rm -f /usr/local/sbin/getent; die "the upgrade with a directory that does not answer did not complete within 300s"; }
took=$(($(date +%s) - t0))
rm -f /usr/local/sbin/getent
[ "$took" -le 150 ] || die "the upgrade with a directory that does not answer took ${took}s"
grep -q "hotserve: whether the hotserve-backup account exists could not be told (the user directory did not answer within 30s)" /tmp/upgrade-nss.log \
	|| die "postinstall did not say that the directory did not answer: $(cat /tmp/upgrade-nss.log)"
[ "$(getent passwd hotserve-backup | wc -l)" = 1 ] || die "a second hotserve-backup account was made: $(getent passwd | grep hotserve-backup)"
dpkg -s hotserve | grep -q '^Status: install ok installed$' || die "hotserve is not configured after the upgrade with a directory that does not answer"
echo "a directory that does not answer: postinstall said so within ${took}s, made no account, and the configure completed"
# An upgrade from a release that had no backup timers — every box that
# first gets this feature: the helper has no state for them, and they
# are enabled and started as on an install, not left stopped until a
# reboot (the owner's review of #155). Staged as that release leaves a
# box: no enable state, no stamps, the timers stopped.
systemctl disable --now hotserve-backup.timer hotserve-backup-drill.timer >/dev/null 2>&1
rm -f /var/lib/systemd/deb-systemd-helper-enabled/hotserve-backup*.dsh-also /var/lib/systemd/deb-systemd-helper-enabled/timers.target.wants/hotserve-backup* /var/lib/systemd/timers/stamp-hotserve-backup*
systemctl reset-failed $TIMERS 2>/dev/null || true
dpkg -i "$deb" >/tmp/upgrade-first.log 2>&1 || { cat /tmp/upgrade-first.log; die "the upgrade from a release without the timers failed"; }
expect_backup_state "after an upgrade from a release without the timers" enabled active "$CRED_SHA"
# An upgrade leaves a backup under way alone (the owner, 2026-09-27):
# its upload, which is restic's, finishes, and the run goes on as the
# program it was started as — here the same program, so its helpers
# work for it. (A helper of another version refusing is the units
# suite's row and the engine's table.) Staged with a run held at its
# upload, under a service and from a shell.
# hold_upload <how a run is started, in the background>: a run held at
# its upload — $up its upload unit, $rpid that unit's restic, stopped.
hold_upload() {
	# The rows upgrade the package faster than anyone does, and each
	# upgrade restarts the timers: past five starts in ten seconds the
	# manager refuses the next (start-limit-hit). Counted from here.
	# shellcheck disable=SC2086 # two unit names
	systemctl reset-failed $TIMERS 2>/dev/null || true
	su -s /bin/sh hotserve -c 'head -c 50000000 /dev/urandom >/var/lib/liveswap/demo/shared/uploads/big.bin'
	eval "$1"
	i=0
	until up=$(systemctl list-units --plain --no-legend --state=activating 'hotserve_backup_upload_*' | awk '{print $1}' | head -1) && [ -n "$up" ]; do
		i=$((i + 1))
		[ "$i" -ge 300 ] && die "no upload unit appeared ($1): the row would prove nothing"
		sleep 0.1
	done
	rpid=$(systemctl show -p ExecMainPID --value "$up")
	[ -n "$rpid" ] && [ "$rpid" != 0 ] && kill -STOP "$rpid" || die "could not hold the upload's restic (pid '$rpid')"
}
# survived <step> <log>: the run held at its upload is still under way
# after the upgrade, said nothing of, and ends ok once let go.
survived() {
	[ "$(systemctl is-active "$up" || true)" = activating ] && kill -0 "$rpid" 2>/dev/null \
		|| die "$1: the upgrade ended the upload under way ($up is $(systemctl is-active "$up" || true))"
	# An upgrade stops and sweeps nothing: none of what a stop or a
	# sweep says is in its output (the owner's review of #155: this
	# looked for words no script prints any more).
	grep -q "not swept\|hotserve-backup sweep\|Stopping\|Stopped" "$2" && die "$1: the upgrade stopped or swept something of the backups: $(cat "$2")" || true
	kill -CONT "$rpid"
}
hold_upload "systemctl start --no-block hotserve-backup.service"
dpkg -i "$deb" >/tmp/upgrade-run.log 2>&1 || { cat /tmp/upgrade-run.log; die "the upgrade with a run under way failed"; }
survived "an upgrade with a run under way" /tmp/upgrade-run.log
i=0
until [ "$(systemctl show -p ActiveState --value hotserve-backup.service)" != activating ] || [ "$i" -ge 240 ]; do
	i=$((i + 1))
	sleep 0.5
done
# Ended, and not still under way: a hung run keeps the Result= of the
# run before it (Copilot on #155).
[ "$(systemctl show -p ActiveState --value hotserve-backup.service)" != activating ] \
	|| die "the run under way at the upgrade had not ended 120s after it was let go: $(journalctl -u hotserve-backup.service --no-pager | tail -10)"
[ "$(systemctl show -p Result --value hotserve-backup.service)" = success ] \
	|| die "the run under way at the upgrade ended '$(systemctl show -p Result --value hotserve-backup.service)': $(journalctl -u hotserve-backup.service --no-pager | tail -10)"
grep -q '"demo": {' /var/lib/hotserve-backup/status.json && tr -d '\n' </var/lib/hotserve-backup/status.json | grep -q '"demo": { *"class": "ok"' \
	|| die "the run under way at the upgrade did not back demo up: $(tr -d '\n' </var/lib/hotserve-backup/status.json | cut -c1-400)"
su -s /bin/sh hotserve -c 'rm -f /var/lib/liveswap/demo/shared/uploads/big.bin'
expect_backup_state "after an upgrade with a run under way" enabled active "$CRED_SHA"
echo "an upgrade left the run under way alone: its upload finished, and it ended ok"
# The same from a shell.
hold_upload "setsid hotserve-backup run >/tmp/shell-run.log 2>&1 &"
shell_pid=$(sed -n 's/^pid \([0-9][0-9]*\),.*/\1/p' /run/hotserve-backup/lock)
[ -n "$shell_pid" ] && kill -0 "$shell_pid" || die "the run from a shell is not the lock's holder ('$(cat /run/hotserve-backup/lock)'): the row proves nothing"
dpkg -i "$deb" >/tmp/upgrade-shell.log 2>&1 || { cat /tmp/upgrade-shell.log; die "the upgrade with a command under way from a shell failed"; }
survived "an upgrade with a command under way from a shell" /tmp/upgrade-shell.log
i=0
while kill -0 "$shell_pid" 2>/dev/null; do
	i=$((i + 1))
	[ "$i" -ge 240 ] && die "the run from a shell did not end within 120s of its upload going on: $(cat /tmp/shell-run.log)"
	sleep 0.5
done
grep -q "^demo: ok" /tmp/shell-run.log || die "the run from a shell, under way at the upgrade, did not back demo up: $(cat /tmp/shell-run.log)"
su -s /bin/sh hotserve -c 'rm -f /var/lib/liveswap/demo/shared/uploads/big.bin'
expect_backup_state "after an upgrade with a command under way from a shell" enabled active "$CRED_SHA"
echo "a command from a shell, under way at the upgrade: left alone, and it ended ok"
# And under a policy-rc.d, which forbids a package every start and
# stop: an upgrade stops nothing of the backups, so the policy leaves
# the timers as they were, running.
printf '#!/bin/sh\nexit 101\n' >/usr/sbin/policy-rc.d
chmod 755 /usr/sbin/policy-rc.d
dpkg -i "$deb" >/tmp/upgrade-policy.log 2>&1 || { cat /tmp/upgrade-policy.log; rm -f /usr/sbin/policy-rc.d; die "the upgrade under a policy-rc.d failed"; }
rm -f /usr/sbin/policy-rc.d
expect_backup_state "after an upgrade under a policy-rc.d" enabled active "$CRED_SHA"
echo "an upgrade under a policy-rc.d left the timers running"
# The account is every run's to check, and an upgrade says so in
# setup's own words: hotserve uses only an account it made, marked as
# its own; with the mark gone — one made by someone else stands in —
# it is warned of at the upgrade, refused by a run, and accepted again
# once it is hotserve's.
usermod --comment "made by someone else" hotserve-backup
dpkg -i "$deb" >/tmp/upgrade-mark.log 2>&1 || { cat /tmp/upgrade-mark.log; die "the upgrade with an account hotserve did not make failed: a warning must not fail an install"; }
grep -q "was not made by hotserve" /tmp/upgrade-mark.log && grep -q "refuse that account" /tmp/upgrade-mark.log \
	|| die "postinstall did not warn of an account hotserve did not make: $(cat /tmp/upgrade-mark.log)"
systemctl start hotserve-backup.service && die "a run with an account hotserve did not make exited 0" || true
journalctl -u hotserve-backup.service --no-pager | grep -q "was not made by hotserve" \
	|| die "the run did not say why it refused: $(journalctl -u hotserve-backup.service --no-pager | tail -5)"
usermod --comment made-by-hotserve hotserve-backup
systemctl reset-failed hotserve-backup.service 2>/dev/null || true
hotserve-backup account >/dev/null || die "hotserve's account is not accepted: $(hotserve-backup account 2>&1)"
systemctl start hotserve-backup.service || die "a run after the account was hotserve's again failed: $(journalctl -u hotserve-backup.service --no-pager | tail -20)"
echo "an account hotserve did not make: warned of at the upgrade in setup's words, refused by a run, accepted once it is hotserve's"

stage "stage 4: removal"
# What remove has to take with it, staged: the persistent timers'
# stamps, eight days old — a reinstall beside the credential file
# would read them as a run and a drill missed, and catch up [M68] —
# and a bind mount of an app's data that a killed run left, which after
# a remove no next run sweeps.
for u in $TIMERS; do
	[ -f "/var/lib/systemd/timers/stamp-$u" ] || die "no stamp for $u before the remove: the row would prove nothing"
	touch -d '8 days ago' "/var/lib/systemd/timers/stamp-$u"
done
# As a run binds it: recursively, with the disks an operator has
# mounted inside the app's data — one of a name with a space in it,
# which mountinfo escapes. Through a bind taken away, each disk stays
# mounted where the operator put it.
su -s /bin/sh hotserve -c 'mkdir -p /var/lib/liveswap/demo/shared/uploads/disk "/var/lib/liveswap/demo/shared/uploads/my disk"'
mkdir -p /run/hotserve-backup/dead00000001/mount-1 /tmp/disk "/tmp/my disk"
echo on-the-disk >/tmp/disk/kept.txt
echo on-my-disk >"/tmp/my disk/kept.txt"
mount --bind /tmp/disk /var/lib/liveswap/demo/shared/uploads/disk || die "could not stage a disk inside the app's data"
mount --bind "/tmp/my disk" "/var/lib/liveswap/demo/shared/uploads/my disk" || die "could not stage a disk of a name with a space inside the app's data"
disks_mounted() { # <when>
	mountpoint -q /var/lib/liveswap/demo/shared/uploads/disk && mountpoint -q "/var/lib/liveswap/demo/shared/uploads/my disk" \
		&& [ -f /var/lib/liveswap/demo/shared/uploads/disk/kept.txt ] && [ -f "/var/lib/liveswap/demo/shared/uploads/my disk/kept.txt" ] \
		|| die "$1: a disk inside the app's data was unmounted from under the app: $(grep liveswap/demo /proc/self/mountinfo | awk '{print $5}' | tr '\n' ' ')"
}
mount --rbind /var/lib/liveswap/demo/shared /run/hotserve-backup/dead00000001/mount-1 || die "could not stage a leftover bind mount"
[ -f /run/hotserve-backup/dead00000001/mount-1/app.db ] && [ -f "/run/hotserve-backup/dead00000001/mount-1/uploads/my disk/kept.txt" ] \
	|| die "the staged mount does not show the app's data: the remove row would prove nothing"
disks_mounted "before the remove"
apt-get remove -y hotserve >/tmp/remove.log 2>&1 || { cat /tmp/remove.log; die "apt-get remove failed"; }
cat /tmp/remove.log
disks_mounted "after remove"
grep -q "unmount .*yourself" /tmp/remove.log \
	&& die "remove told the administrator to unmount what it had unmounted: $(grep yourself /tmp/remove.log)" || true
for u in $TIMERS; do
	[ ! -e "/var/lib/systemd/timers/stamp-$u" ] || die "remove left the stamp of $u: a reinstall would catch up a run nobody missed"
done
grep -q " /run/hotserve-backup/" /proc/self/mountinfo \
	&& die "remove left a killed run's mount: $(grep ' /run/hotserve-backup/' /proc/self/mountinfo)" || true
[ -f /var/lib/liveswap/demo/shared/app.db ] && [ -f /var/lib/liveswap/demo/shared/uploads/a.png ] \
	|| die "remove took the app's data with the mount"
[ ! -e /run/hotserve-backup/dead00000001 ] || die "remove left a killed run's directory, which the engine's sweep takes: $(ls -la /run/hotserve-backup/dead00000001)"
echo "remove took the timers' stamps, and the engine's sweep a killed run's mount and its directory, and nothing of the app's"
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
: >/tmp/before-reinstall
apt-get install -y "$deb" >/tmp/reinstall.log 2>&1 || { cat /tmp/reinstall.log; die "reinstall after remove failed"; }
expect_backup_state "reinstalled after remove" enabled active "$CRED_SHA"
# The stamps are new: written by the timers' first activation, which
# fires nothing [M61]. With the old ones the drill's next elapse is the
# catch-up's, minutes away; with new ones it is a Sunday at 03:30 and
# its offset.
for u in $TIMERS; do
	[ "/var/lib/systemd/timers/stamp-$u" -nt /tmp/before-reinstall ] \
		|| die "reinstalled after remove: the stamp of $u is not this install's: $(ls -la --time-style=full-iso /var/lib/systemd/timers/)"
done
dnext=$(systemctl show -p NextElapseUSecRealtime --value hotserve-backup-drill.timer)
dpast=$(($(date -d "$dnext" +%s) - $(date -d "$(date -d "$dnext" '+%Y-%m-%d') 03:30:00" +%s)))
[ "$(date -d "$dnext" +%a)" = Sun ] && [ "$dpast" -ge 0 ] && [ "$dpast" -le 600 ] \
	|| die "reinstalled after remove: the drill's next elapse is '$dnext' (${dpast}s past 03:30 of its day), not a Sunday between 03:30 and 03:40: a catch-up"
for u in hotserve-backup.service hotserve-backup-drill.service; do
	[ "$(systemctl show -p ExecMainStartTimestampMonotonic --value "$u")" = 0 ] \
		|| die "reinstalled after remove: $u ran inside the install"
done
echo "reinstalled after remove: the stamps are new, and nothing caught up"
timeout 300 systemctl enable --now hotserve || die "hotserve did not start after the reinstall"
systemctl start hotserve-backup.service || die "a run after the reinstall failed: $(journalctl -u hotserve-backup.service --no-pager | tail -20)"
[ "$(systemctl show -p Result --value hotserve-backup.service)" = success ] || die "the run after the reinstall ended '$(systemctl show -p Result --value hotserve-backup.service)'"
echo "reinstalled after remove: the timers enabled and running again, the same credential file, a run works"
# What a killed run can leave behind: a bind mount of an app's data
# under the run directory, and the record writer's temp file. Purge
# must remove nothing of the app's through the first, and not be
# defeated by the second.
# A directory and a file, as a run binds them, a mount inside a mount,
# and the files a run keeps beside them; and the temp file setup's
# writer of the repository id leaves when it is killed mid-write.
su -s /bin/sh hotserve -c 'echo inner >/var/lib/liveswap/demo/shared/uploads/inner.txt'
mkdir -p /run/hotserve-backup/dead00000001/mount-1
: >/run/hotserve-backup/dead00000001/mount-2
disks_mounted "before the purge"
mount --rbind /var/lib/liveswap/demo/shared /run/hotserve-backup/dead00000001/mount-1 || die "could not stage a leftover bind mount"
mount --bind /var/lib/liveswap/demo/shared/app.db /run/hotserve-backup/dead00000001/mount-2 || die "could not stage a leftover bind mount of a file"
[ -f /run/hotserve-backup/dead00000001/mount-1/app.db ] && [ -f /run/hotserve-backup/dead00000001/mount-1/uploads/disk/kept.txt ] \
	|| die "the staged mounts do not show the app's data: the purge check would prove nothing"
# The mount a run makes of nothing, to ask the manager whether it is
# seen: a run killed right then leaves it.
mkdir -p /run/hotserve-backup/dead00000001/seen
mount --bind /run/hotserve-backup/dead00000001/seen /run/hotserve-backup/dead00000001/seen || die "could not stage a run's own probe mount"
echo '{}' >/run/hotserve-backup/dead00000001/plan.json
: >/run/hotserve-backup/dead00000001/.listing.err
# And the units a command killed from a shell left running — it has no
# service for the manager to end them with — recorded by their exact
# names, as a run records each before it starts it. A line that is no
# name the engine writes is nothing to stop.
LEFT=hotserve_backup_upload_demo_0123456789ab.service
systemd-run --quiet --unit="$LEFT" -p User=hotserve-backup /usr/bin/sleep 600 || die "could not stage a unit a killed command left running"
systemd-run --quiet --unit=smoke-bystander.service /usr/bin/sleep 600 || die "could not stage a unit that is none of the engine's"
# And one of the engine's prefix that is no name the engine writes: no
# role and app, no run's twelve hex digits (Copilot on #155).
systemd-run --quiet --unit=hotserve_backup_bystander.service /usr/bin/sleep 600 || die "could not stage a unit of the engine's prefix"
printf '%s\nsmoke-bystander.service\nhotserve_backup_bystander.service\n' "$LEFT" >/run/hotserve-backup/units
[ "$(systemctl is-active "$LEFT" || true)" = active ] || die "the staged unit is not running: the purge row would prove nothing"
: >/var/lib/hotserve-backup/.status-crash
echo 0123 >/var/lib/hotserve-backup/repository-id.new
apt-get purge -y hotserve >/tmp/purge.log 2>&1 || { cat /tmp/purge.log; die "apt-get purge failed"; }
cat /tmp/purge.log
[ -f /var/lib/liveswap/demo/shared/app.db ] && [ -f /var/lib/liveswap/demo/shared/uploads/a.png ] && [ -f /var/lib/liveswap/demo/shared/uploads/inner.txt ] && [ -f /tmp/disk/kept.txt ] \
	|| die "purge removed the app's data through a mount a killed run left under /run/hotserve-backup"
disks_mounted "after purge"
[ "$(systemctl is-active "$LEFT" || true)" != active ] \
	|| die "purge left a unit a killed command had left running, and removed the one record of it: $LEFT is still active, with the credential"
for u in smoke-bystander.service hotserve_backup_bystander.service; do
	[ "$(systemctl is-active "$u" || true)" = active ] \
		|| die "purge stopped $u, which is no name the engine writes, for being written in the units file"
done
systemctl stop smoke-bystander.service hotserve_backup_bystander.service
grep -q " /run/hotserve-backup" /proc/self/mountinfo \
	&& die "purge left a killed run's mounts, with no next run to sweep them: $(grep ' /run/hotserve-backup' /proc/self/mountinfo)" || true
[ ! -e /run/hotserve-backup ] || die "purge left /run/hotserve-backup: $(find /run/hotserve-backup)"
grep -q "still mounted\|/run/hotserve-backup" /tmp/purge.log \
	&& die "purge spoke of a run directory it removed whole: $(grep 'hotserve-backup' /tmp/purge.log)" || true
umount /var/lib/liveswap/demo/shared/uploads/disk "/var/lib/liveswap/demo/shared/uploads/my disk"
[ ! -e /var/lib/hotserve-backup/repository-id.new ] || die "purge left setup's temp file, repository-id.new"
grep -q "kept.*/var/lib/hotserve-backup" /tmp/purge.log \
	&& die "purge kept the state directory, which held nothing but the package's own: $(grep kept /tmp/purge.log)" || true
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
for u in $TIMERS; do
	[ ! -e "/var/lib/systemd/timers/stamp-$u" ] || die "purge left the stamp of $u"
done
echo "purge kept the credential file and said why, removed the package's own state, the cache, the stamps and a killed run's mounts, kept the account"

stage "stage 6: an account that cannot be made, a command under way at purge, and what purge keeps"
# A group of the account's name and no account: useradd exits 9 [M66].
# The configure completes — hotserve is installed, on a box that may
# use no backups — and says what setup will have to do (the owner,
# 2026-09-27).
userdel hotserve-backup || die "could not remove the account to stage one that cannot be made"
getent group hotserve-backup >/dev/null || groupadd --system hotserve-backup
apt-get install -y "$deb" >/tmp/install-noaccount.log 2>&1 \
	|| { cat /tmp/install-noaccount.log; die "the install failed where the hotserve-backup account could not be made: hotserve is left half-configured"; }
dpkg -s hotserve | grep -q '^Status: install ok installed$' || die "hotserve is not configured: $(dpkg -s hotserve | grep ^Status)"
getent passwd hotserve-backup >/dev/null && die "the account was made: the row proves nothing" || true
grep -q "hotserve: the hotserve-backup account could not be made (useradd: group hotserve-backup exists" /tmp/install-noaccount.log \
	|| die "postinstall did not say that the account could not be made, and why: $(cat /tmp/install-noaccount.log)"
grep -q "sudo hotserve-backup setup <repository> makes it, or says why it cannot" /tmp/install-noaccount.log \
	|| die "postinstall did not say what makes the account: $(cat /tmp/install-noaccount.log)"
timeout 300 systemctl enable --now hotserve || die "hotserve did not start on the box where the account could not be made"
expect_backup_state "installed where the account cannot be made" enabled active "$CRED_SHA"
systemctl start hotserve-backup.service && die "a run with no account exited 0" || true
journalctl -u hotserve-backup.service --no-pager | grep -q "the hotserve-backup account is not there" \
	|| die "the run did not say that the account is not there: $(journalctl -u hotserve-backup.service --no-pager | tail -5)"
systemctl reset-failed hotserve-backup.service 2>/dev/null || true
groupdel hotserve-backup
echo "a group of the account's name: installed, and said; a run says the account is not there"
# Purge with a command holding the run lock: said, its mounts left to
# it. And what purge keeps, said for what it is: copies of an app's
# data in staging, and a file in the state directory that the package
# did not make, which is no app's data.
mkdir -p /run/hotserve-backup/11fe00000001/mount-1 /var/lib/hotserve-backup/staging/demo
mount --bind /var/lib/liveswap/demo/shared /run/hotserve-backup/11fe00000001/mount-1 || die "could not stage a mount of a run under way"
flock /run/hotserve-backup/lock -c 'echo "pid $$, since 2026-09-27T00:00:00Z" >/run/hotserve-backup/lock; exec sleep 300' &
holder=$!
i=0
until ! flock -n /run/hotserve-backup/lock true 2>/dev/null; do
	i=$((i + 1))
	[ "$i" -ge 50 ] && die "could not hold the run lock: the row would prove nothing"
	sleep 0.1
done
echo plaintext >/var/lib/hotserve-backup/staging/demo/app.db
echo mine >/var/lib/hotserve-backup/notes.txt
apt-get purge -y hotserve >/tmp/purge-held.log 2>&1 || { cat /tmp/purge-held.log; die "apt-get purge with a command under way failed"; }
cat /tmp/purge-held.log
grep -q "hotserve: what a backup command left was not swept, and is left: another backup run is in progress: pid [0-9]*, since 2026-09-27T00:00:00Z" /tmp/purge-held.log \
	|| die "preremove did not say that a command holds the run lock"
grep -q "hotserve: kept /run/hotserve-backup: something is mounted at or under it" /tmp/purge-held.log \
	|| die "postremove did not say why it kept the run directory"
[ -f /run/hotserve-backup/11fe00000001/mount-1/app.db ] || die "purge unmounted under a command that holds the run lock"
grep -q "hotserve: kept /var/lib/hotserve-backup/staging: not empty — copies of an app's data" /tmp/purge-held.log \
	|| die "purge did not say that staging is kept, and what is in it"
grep -q "hotserve: kept /var/lib/hotserve-backup: it holds what this package did not make (notes.txt)" /tmp/purge-held.log \
	|| die "purge did not say what the state directory is kept for"
grep "kept /var/lib/hotserve-backup:" /tmp/purge-held.log | grep -q "copies of an app's data" \
	&& die "purge blamed copies of app data for a file that is none" || true
[ -f /var/lib/hotserve-backup/staging/demo/app.db ] && [ -f /var/lib/hotserve-backup/notes.txt ] || die "purge removed what it said it kept"
pkill -P "$holder" 2>/dev/null || true
kill "$holder" 2>/dev/null || true
umount /run/hotserve-backup/11fe00000001/mount-1
rm -rf /run/hotserve-backup /var/lib/hotserve-backup
echo "purge under a command's lock: said, its mount left; what is kept is said for what it is"
# Mounts under the run directory of names the engine does not make:
# the directory is root's alone, 0700, and nothing but the engine
# mounts there, so its sweep takes every mount under it — made
# private first, so that what was beneath the app's own directory
# stays — and the app's files stay (the owner, 2026-09-27: the
# engine's sweep, not a shell copy with its own rules).
apt-get install -y "$deb" >/tmp/install-again.log 2>&1 || { cat /tmp/install-again.log; die "the install before the last purge failed"; }
ODD="/run/hotserve-backup/odd /run/hotserve-backup/dead00000001/other /run/hotserve-backup/notarun/mount-1"
mkdir -p "/run/hotserve-backup/odd dir" /run/hotserve-backup/dead00000001/mount-1 $ODD
echo '{}' >/run/hotserve-backup/dead00000001/plan.json
for m in "/run/hotserve-backup/odd dir" $ODD /run/hotserve-backup/dead00000001/mount-1; do
	mount --bind /var/lib/liveswap/demo/shared "$m" || die "could not stage a mount at $m"
done
cp /var/lib/dpkg/info/hotserve.postrm /tmp/hotserve.postrm
apt-get purge -y hotserve >/tmp/purge-odd.log 2>&1 || { cat /tmp/purge-odd.log; die "apt-get purge with mounts of other names failed"; }
cat /tmp/purge-odd.log
[ -f /var/lib/liveswap/demo/shared/app.db ] && [ -f /var/lib/liveswap/demo/shared/uploads/a.png ] \
	|| die "purge removed the app's files through a mount under the run directory"
grep -q " /run/hotserve-backup" /proc/self/mountinfo \
	&& die "purge left mounts under the run directory: $(grep ' /run/hotserve-backup' /proc/self/mountinfo | awk '{print $5}' | tr '\n' ' ')" || true
[ ! -e /run/hotserve-backup ] || die "purge left /run/hotserve-backup: $(find /run/hotserve-backup)"
echo "mounts of other names under the run directory: taken away by the engine's sweep, and nothing of the app's with them"

stage "stage 7: purge where the mount table cannot be read"
# In a chroot or a namespace with no /proc, postremove cannot see what
# is mounted under the run directory: it removes nothing there, and
# says so, rather than taking an empty mount table for none (Copilot
# on #155). Run in a mount namespace of its own with /proc gone, the
# script as the package shipped it, against a bind of the app's data
# sitting right on a run directory.
[ -f /tmp/hotserve.postrm ] || die "the package's postremove was not kept for this stage"
unshare --mount --propagation private sh -c '
	set -e
	# Right on a run directory, whose files purge removes one by one
	# once it believes nothing is mounted there.
	mkdir -p /run/hotserve-backup/0123456789ab
	mount --bind /var/lib/liveswap/demo/shared /run/hotserve-backup/0123456789ab
	[ -f /run/hotserve-backup/0123456789ab/app.db ] || { echo "FIXTURE: the bind shows nothing"; exit 3; }
	umount -l /proc
	[ ! -r /proc/self/mountinfo ] || { echo "FIXTURE: /proc is still there"; exit 3; }
	sh /tmp/hotserve.postrm purge >/tmp/purge-noproc.log 2>&1 || true
	[ -f /run/hotserve-backup/0123456789ab/app.db ] || { echo "GONE"; exit 4; }
' ; rc=$?
[ "$rc" != 3 ] || die "the mount table could still be read: the row proves nothing"
[ "$rc" = 0 ] || die "purge with no mount table removed the app's files through a mount under the run directory (exit $rc): $(cat /tmp/purge-noproc.log)"
[ -f /var/lib/liveswap/demo/shared/app.db ] && [ -f /var/lib/liveswap/demo/shared/uploads/a.png ] || die "purge with no mount table removed the app's files"
grep -q "hotserve: kept /run/hotserve-backup: something is mounted at or under it, or what is mounted cannot be read; nothing under it is removed" /tmp/purge-noproc.log \
	|| die "postremove did not say that it could not read the mount table: $(cat /tmp/purge-noproc.log)"
rm -rf /run/hotserve-backup
echo "no mount table: purge removed nothing under the run directory, and said why"
# And a mount on the run directory itself, with /proc there: purge
# removes the run directory's own files by name, and through such a
# mount those would be the app's (Copilot on #155).
mkdir -p /run/hotserve-backup /tmp/at-run
echo "the app's" >/tmp/at-run/lock
echo "the app's" >/tmp/at-run/units
mount --bind /tmp/at-run /run/hotserve-backup
sh /tmp/hotserve.postrm purge >/tmp/purge-atrun.log 2>&1 || true
umount /run/hotserve-backup
[ -f /tmp/at-run/lock ] && [ -f /tmp/at-run/units ] || die "purge removed files through a mount on the run directory itself: $(ls -la /tmp/at-run)"
grep -q "hotserve: kept /run/hotserve-backup: something is mounted at or under it" /tmp/purge-atrun.log \
	|| die "postremove did not say why it kept the run directory: $(cat /tmp/purge-atrun.log)"
rm -rf /run/hotserve-backup /tmp/at-run
echo "a mount on the run directory itself: purge removed nothing through it, and said why"
# And a command that holds the run lock with nothing mounted — a
# restore waiting at its prompt: purge leaves the lock, the list of
# its units and the init marker to it, and says so (the owner's
# review of #155: they were removed from under it, and the units it
# had started had no record left for any sweep).
mkdir -p /run/hotserve-backup
chmod 700 /run/hotserve-backup
echo hotserve_backup_upload_demo_0123456789ab.service >/run/hotserve-backup/units
echo hotserve_backup_init_0123456789ab.service >/run/hotserve-backup/init-unit
flock /run/hotserve-backup/lock -c 'echo "pid $$, since 2026-09-27T00:00:00Z" >/run/hotserve-backup/lock; exec sleep 300' &
holder=$!
i=0
until ! flock -n /run/hotserve-backup/lock true 2>/dev/null; do
	i=$((i + 1))
	[ "$i" -ge 50 ] && die "could not hold the run lock: the row would prove nothing"
	sleep 0.1
done
sh /tmp/hotserve.postrm purge >/tmp/purge-locked.log 2>&1 || true
pkill -P "$holder" 2>/dev/null || true
kill "$holder" 2>/dev/null || true
[ -f /run/hotserve-backup/units ] && [ -f /run/hotserve-backup/init-unit ] && [ -f /run/hotserve-backup/lock ] \
	|| die "purge removed the run directory's files from under a command that holds the run lock: $(ls -la /run/hotserve-backup 2>&1)"
grep -q "hotserve: kept /run/hotserve-backup: the run lock is held (pid [0-9]*, since 2026-09-27T00:00:00Z)" /tmp/purge-locked.log \
	|| die "postremove did not say that the run lock is held: $(cat /tmp/purge-locked.log)"
rm -rf /run/hotserve-backup
echo "a command holding the run lock: purge left its files, and said why"
# A command from a shell, killed between a remove and a purge: its
# upload unit, which no service ends, still runs with the credential,
# and its mounts are there. The program is gone, so no sweep: purge
# stops the units the command recorded, by the engine's grammar, and
# leaves the mounts, as it says (Copilot on #155).
LEFT=hotserve_backup_upload_demo_0123456789ab.service
systemd-run --quiet --unit="$LEFT" /usr/bin/sleep 600 || die "could not stage a unit a killed command left running"
systemd-run --quiet --unit=smoke-bystander.service /usr/bin/sleep 600 || die "could not stage a unit that is none of the engine's"
mkdir -p /run/hotserve-backup/0123456789ab/mount-1
chmod 700 /run/hotserve-backup
mount --bind /var/lib/liveswap/demo/shared /run/hotserve-backup/0123456789ab/mount-1 || die "could not stage a killed command's mount"
printf '%s\nsmoke-bystander.service\n' "$LEFT" >/run/hotserve-backup/units
sh /tmp/hotserve.postrm purge >/tmp/purge-killed.log 2>&1 || true
[ "$(systemctl is-active "$LEFT" || true)" != active ] \
	|| die "purge left a killed command's upload unit running, with the credential: $(cat /tmp/purge-killed.log)"
[ "$(systemctl is-active smoke-bystander.service || true)" = active ] || die "purge stopped a unit that is none of the engine's"
systemctl stop smoke-bystander.service
mountpoint -q /run/hotserve-backup/0123456789ab/mount-1 || die "purge took a mount away without the engine's sweep"
[ -f /var/lib/liveswap/demo/shared/app.db ] || die "purge removed the app's files"
grep -q "hotserve: kept /run/hotserve-backup: something is mounted at or under it" /tmp/purge-killed.log || die "purge did not say why it kept the run directory"
umount /run/hotserve-backup/0123456789ab/mount-1
rm -rf /run/hotserve-backup
echo "a killed command's units after a remove: stopped by purge, by their recorded names; its mount left, and said"
# And one that will not stop: said, by name, and its record kept, so
# that the name is not lost with the credential still in its
# environment (Copilot on #155).
STUCK=hotserve_backup_upload_demo_0123456789ab.service
mkdir -p "/run/systemd/system/$STUCK.d"
printf '[Unit]\nRefuseManualStop=yes\n' >"/run/systemd/system/$STUCK.d/10-smoke.conf"
systemd-run --quiet --unit="$STUCK" /usr/bin/sleep 600 || die "could not stage a unit that will not stop"
systemctl daemon-reload
mkdir -p /run/hotserve-backup
chmod 700 /run/hotserve-backup
echo "$STUCK" >/run/hotserve-backup/units
sh /tmp/hotserve.postrm purge >/tmp/purge-stuck.log 2>&1 || true
[ "$(systemctl is-active "$STUCK" || true)" = active ] || die "the unit staged as one that will not stop stopped: the row proves nothing"
grep -q "hotserve: $STUCK, which a backup command left running, would not stop" /tmp/purge-stuck.log \
	|| die "purge did not say that a unit would not stop: $(cat /tmp/purge-stuck.log)"
grep -qx "$STUCK" /run/hotserve-backup/units 2>/dev/null || die "purge removed the record of a unit that would not stop"
rm -rf "/run/systemd/system/$STUCK.d"
systemctl daemon-reload
systemctl stop "$STUCK" 2>/dev/null || true
rm -rf /run/hotserve-backup
echo "a unit that would not stop: said by name, and its record kept"
# And the manager unreachable: its state cannot be asked, which is no
# "gone" — a missing unit reads "inactive" [measured] — so it is said,
# and the record kept (Copilot on #155).
LEFT=hotserve_backup_upload_demo_0123456789ab.service
systemd-run --quiet --unit="$LEFT" /usr/bin/sleep 600 || die "could not stage a unit a killed command left running"
mkdir -p /run/hotserve-backup
chmod 700 /run/hotserve-backup
echo "$LEFT" >/run/hotserve-backup/units
mv /run/systemd/private /run/systemd/private.smoke-aside
systemctl show -p ActiveState --value "$LEFT" >/dev/null 2>&1 && { mv /run/systemd/private.smoke-aside /run/systemd/private; die "the manager still answered: the row proves nothing"; }
sh /tmp/hotserve.postrm purge >/tmp/purge-nobus.log 2>&1 || true
mv /run/systemd/private.smoke-aside /run/systemd/private
grep -qx "$LEFT" /run/hotserve-backup/units 2>/dev/null || die "purge removed the record of a unit whose state could not be asked: $(cat /tmp/purge-nobus.log)"
grep -q "hotserve: $LEFT, which a backup command left running, could not be asked about" /tmp/purge-nobus.log \
	|| die "purge did not say that it could not ask about a unit: $(cat /tmp/purge-nobus.log)"
systemctl stop "$LEFT" 2>/dev/null || true
rm -rf /run/hotserve-backup
echo "the manager unreachable: said, and the record kept"

echo ""
echo "ALL PACKAGE SMOKE STAGES PASSED ($deb on $(. /etc/os-release && echo "$PRETTY_NAME"))"
