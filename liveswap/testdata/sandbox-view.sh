#!/bin/sh
# sandbox-view.sh — the VIEW PROBE. It runs inside a sandboxed app unit
# and records what the sandbox looks like from within: one `k=v` line
# per check, written into the app's shared dir (the one writable,
# persistent path in the view) where each test lane reads it back.
#
# Deliberately not executable: every lane SOURCES it (see below), so
# that `pid` below is the unit's main process rather than a child of
# it. (Resource limits are inherited unchanged, so `nofile_*` would be
# right either way — the pid is the whole reason.)
#
# Not to be confused with the CAPABILITY PROBE (probeSandboxCapability,
# liveswap/sandbox.go), which is production code that runs a throwaway
# unit at App.Start to decide whether this host can deliver a sandbox
# at all. This file is a test fixture and ships in no release.
#
# One asset, three lanes: liveswap's integration test embeds it, the
# e2e artifacts image bakes it into a release tarball, and the
# packaging smoke test mounts it into its container. It lives in this
# module's testdata because being *embedded* is what makes editing it
# invalidate the Go test cache — read from outside the module, both Go
# lanes silently returned a cached pass when only this file changed.
# Keeping one copy is the point: three copies drifted on fifteen keys,
# and one of them had stopped testing that /etc/ssl/private stays out
# of the view (#72).
#
# Each lane wraps it in its own ./server, which SOURCES this file and
# then becomes whatever that lane needs (a sleep, the demo app,
# `hotserve respond`). Sourced rather than run as a child so that `$$`
# below is the unit's main pid — which is the assertion that the PID
# namespace is in effect at all.
#
# Paths are derived from the working directory — the release dir,
# <root>/<app>/releases/<version> — so this works for any root and any
# app name. Three facts cannot be derived, because the sandbox is what
# hides them, and arrive as environment:
#
#   MGR_PID         the user manager's pid, as seen on the host
#   PROBE_DNS_NAME  a name to resolve, proving DNS works from inside
#   HOTSERVE_UID    the uid hotserve runs as
#
# HOTSERVE_UID is the one with a defensible default: apps run as the
# hotserve user and PrivateUsers= maps that uid to itself, so `id -u`
# in here IS hotserve's uid. Rather than trust that, the probe emits
# the value it actually used as `uid`, and the lane compares it against
# the uid it looked up from outside — proving the manager-socket path
# below is the real one rather than merely absent.
#
# A check whose input is missing emits `skipped` rather than nothing.
# That is deliberate: a lane that silently lost its input must fail an
# assertion, not pass vacuously. Every key below is emitted on every
# run, in every lane.

release=$(pwd)
app=$(cd .. && cd .. && pwd) # <root>/<app>
root=$(cd "$app/.." && pwd)  # <root>
shared="$app/shared"
out="$shared/view.txt"
: >"$out"

emit() { printf '%s=%s\n' "$1" "$2" >>"$out"; }
# exists <key> <value-if-present> <value-if-absent> <path>
exists() { [ -e "$4" ] && emit "$1" "$2" || emit "$1" "$3"; }
# writable <key> <path-to-create>
writable() { touch "$2" 2>/dev/null && emit "$1" writable || emit "$1" readonly; }

# ── The namespaces ────────────────────────────────────────────────
# pid 1 means our own PID namespace (we are sourced by the unit's main
# process, so $$ is its pid); a uid_map covering one id rather than the
# initial namespace's 2^32-1 means our own user namespace.
emit pid "$$"
read _ _ uidmap </proc/self/uid_map
emit uidmap "$uidmap"
emit nprocs "$(ls /proc | grep -c '^[0-9]')"

# ── The app's own dirs, and the rest of the liveswap root ─────────
# root_listing is the sibling assertion: the root must show this app
# and nothing else, so no other app's releases, shared data or env
# file is even nameable from in here.
emit root_listing "$(ls "$root" 2>/dev/null | tr '\n' ' ')"
exists state open closed "$app/state.json"
exists apptmp open closed "$app/tmp"
exists current open closed "$app/current"
writable release "$release/.probe-w"
writable root "$root/.probe-w"

# ── hotserve's own state, sockets and config ──────────────────────
exists hotserve_lib open closed /var/lib/hotserve
exists run_hotserve open closed /run/hotserve
exists etc_hotserve open closed /etc/hotserve
exists admin_socket open closed /run/hotserve/admin.sock
probe_uid=${HOTSERVE_UID:-$(id -u)}
emit uid "$probe_uid"
exists mgr_socket open closed "/run/user/$probe_uid/systemd/private"

# ── The manager's process, via /proc ──────────────────────────────
# These are the acceptance paths. The user namespace closes them on
# its own (the kernel refuses ptrace-class access across user
# namespaces), and the PID namespace closes them a second time over.
# From in here the two cannot be told apart: PrivatePIDs= is always
# on, so /proc/$MGR_PID does not exist in this /proc at all, and
# `closed` is the PID namespace's answer whatever the user namespace
# would have said. The user-namespace claim on its own rests on the
# 2026-08-30 measurement (DESIGN-threat-model.md, "The shared-UID
# rule"), not on this probe.
if [ -n "$MGR_PID" ]; then
	ls "/proc/$MGR_PID/root/" >/dev/null 2>&1 && emit mgr_root open || emit mgr_root closed
	cat "/proc/$MGR_PID/environ" >/dev/null 2>&1 && emit mgr_environ open || emit mgr_environ closed
else
	emit mgr_root skipped
	emit mgr_environ skipped
fi

# ── The rest of the host, which nothing bound ─────────────────────
# Tested for EXISTENCE, not readability: the view is deny-by-default
# (TemporaryFileSystem=/ plus explicit binds), so a path nothing named
# is absent rather than present-but-unreadable — a stronger statement
# than the InaccessiblePaths= node this replaced, and the one the
# design actually makes.
for d in /opt /srv /home /root /mnt /media /var/lib /etc/liveswap; do
	k=$(echo "$d" | tr -d /)
	exists "abs_$k" present absent "$d"
done
# /etc named entry by entry, never bound whole: an app that could list
# all of /etc would see every other app's env_file. /var/lib holds
# nothing but the path to this app's own dirs — hotserve's TLS keys
# sit next door on the host.
emit etc_listing "$(ls /etc 2>/dev/null | tr '\n' ' ')"
emit varlib_listing "$(ls /var/lib 2>/dev/null | tr '\n' ' ')"

# ── The base view: an OS the app can actually run in ──────────────
# Without these, every "absent" and "closed" above would be satisfied
# by a unit that has nothing in it at all.
[ -x /bin/sh ] && emit binsh ok || emit binsh MISSING
[ -x /usr/bin/env ] && emit usrbinenv ok || emit usrbinenv MISSING
[ -x /usr/bin/hotserve ] && emit hsbin ok || emit hsbin MISSING
[ -e /etc/ssl/certs ] && emit etcssl ok || emit etcssl MISSING
# The trust store is named, not the tree that contains it: /etc/ssl
# also holds /etc/ssl/private, which nothing in an app needs and which
# is where hotserve's TLS keys would be.
exists sslprivate present absent /etc/ssl/private
[ -r /etc/resolv.conf ] && emit resolvconf ok || emit resolvconf MISSING
# On systemd-resolved hosts /etc/resolv.conf points into
# /run/systemd/resolve, which the view must keep reachable; resolving
# a real name is what proves the resolver works from in here at all.
if [ -n "$PROBE_DNS_NAME" ]; then
	getent hosts "$PROBE_DNS_NAME" >/dev/null 2>&1 && emit dns ok || emit dns fail
else
	emit dns skipped
fi

# ── The runtime environment ───────────────────────────────────────
# cgroup: is the unit's own cgroup read-only in here? Classified by
# what the write actually hit, because "the write failed" is not the
# claim: a missing cgroupfs, or an EACCES from the wrong owner, fails
# the write too, and used to read as `readonly`.
#   readonly  EROFS: cgroupfs is here and mounted read-only — the
#             ProtectControlGroups= promise
#   writable  the write went through
#   absent    the unit's cgroup is not in the view at all
#   denied:…  anything else, with the shell's own error text
# The target is cgroup.procs, not a controller file: it exists in every
# cgroup v2 directory whatever is delegated, and it is the PID-migration
# escape route as well as the limit-rewriting one. Writing 0 means "the
# writing process, into this cgroup" — it is already there, so where
# the write is allowed (the key-set test runs this outside any sandbox)
# it changes nothing. The write runs in a child shell with LC_ALL=C:
# the unit's environment passes LANG/LC_* through, and the error text
# is what tells EROFS from the rest.
cg="/sys/fs/cgroup$(cut -d: -f3 /proc/self/cgroup)"
if [ ! -e "$cg/cgroup.procs" ]; then
	emit cgroup absent
elif cgerr=$(LC_ALL=C /bin/sh -c 'echo 0 >"$1"' sh "$cg/cgroup.procs" 2>&1); then
	emit cgroup writable
else
	case $cgerr in
	*"Read-only file system"*) emit cgroup readonly ;;
	*) emit cgroup "denied: $(printf '%s' "$cgerr" | tr '\n' ' ')" ;;
	esac
fi
# Reported, not asserted: whether the memory controller reaches the
# unit's cgroup at all. That is the question resource caps (#71) stand
# on, not this sandbox's.
exists cgroup_memory_max present absent "$cg/memory.max"
writable tmp /tmp/.probe-w
emit home "$HOME"
[ -n "$XDG_RUNTIME_DIR" ] && emit xdg_runtime set || emit xdg_runtime unset
emit nofile_soft "$(ulimit -Sn)"
emit nofile_hard "$(ulimit -Hn)"
# A secret seeded into the supervisor's own environment: hotserve
# builds the unit's environment rather than passing its own on, and
# the app must never see it.
[ -n "$ACME_TEST_TOKEN" ] && emit acme_token leaked || emit acme_token absent

# ── Vacuity guards ────────────────────────────────────────────────
# What the lane actually handed us, echoed back. A literal
# "__MGR_PID__", or an empty string, would make every /proc check
# above pass by testing a path that cannot exist — so the lane
# compares these against the real values it meant to pass.
emit saw_mgr_pid "$MGR_PID"
emit saw_uid "$HOTSERVE_UID"

emit done 1
