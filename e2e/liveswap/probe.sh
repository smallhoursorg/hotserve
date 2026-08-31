#!/bin/sh
# The "probe" release's ./server: before becoming the app it records,
# one line per check, what its sandbox looks like from inside — into
# its shared dir, the one writable persistent path in the view, where
# the systemd suite reads it back as root. Paths are derived from the
# working directory (the release dir: <root>/<app>/releases/<version>)
# so the same script works for any root and app name.
release=$(pwd)
app=$(cd .. && cd .. && pwd)        # <root>/<app>
root=$(cd "$app/.." && pwd)         # <root>
shared="$app/shared"
out="$shared/probe.txt"
: > "$out"
# Paths the sandbox keeps out are tested for EXISTENCE, not merely
# readability: the view is deny-by-default (TemporaryFileSystem=/ plus
# explicit binds), so anything nothing named is absent — a stronger
# statement than the inaccessible-but-present node InaccessiblePaths=
# used to leave behind, and the one this suite must pin.
echo "pid=$$" >> "$out"
read _ _ n < /proc/self/uid_map; echo "uidmap=$n" >> "$out"
echo "nprocs=$(ls /proc | grep -c '^[0-9]')" >> "$out"
echo "root_listing=$(ls "$root" | tr '\n' ' ')" >> "$out"
[ -e "$app/state.json" ] && echo "state=open" >> "$out" || echo "state=closed" >> "$out"
[ -e "$app/tmp" ] && echo "apptmp=open" >> "$out" || echo "apptmp=closed" >> "$out"
[ -e "$app/current" ] && echo "current=open" >> "$out" || echo "current=closed" >> "$out"
touch "$release/.probe-w" 2>/dev/null && echo "release=writable" >> "$out" || echo "release=readonly" >> "$out"
touch "$root/.probe-w" 2>/dev/null && echo "root=writable" >> "$out" || echo "root=readonly" >> "$out"
[ -e /var/lib/hotserve ] && echo "hotserve_lib=open" >> "$out" || echo "hotserve_lib=closed" >> "$out"
[ -e /run/hotserve ] && echo "run_hotserve=open" >> "$out" || echo "run_hotserve=closed" >> "$out"
[ -e "/run/user/$(id -u)/systemd/private" ] && echo "mgr_socket=open" >> "$out" || echo "mgr_socket=closed" >> "$out"
[ -e /etc/hotserve ] && echo "etc_hotserve=open" >> "$out" || echo "etc_hotserve=closed" >> "$out"
# The rest of the host, which no bind names and which therefore is not
# there at all. /var/lib is the sharp one: it holds hotserve's own TLS
# keys, and the only thing under it in the view is the liveswap root's
# path to this app's own dirs.
for d in /opt /srv /home /root /mnt /media /var/lib/hotserve /etc/liveswap; do
	k=$(echo "$d" | tr -d /)
	[ -e "$d" ] && echo "abs_$k=present" >> "$out" || echo "abs_$k=absent" >> "$out"
done
echo "etc_listing=$(ls /etc 2>/dev/null | tr '\n' ' ')" >> "$out"
echo "varlib_listing=$(ls /var/lib 2>/dev/null | tr '\n' ' ')" >> "$out"
# The other half of the claim: the base view carries an OS the app can
# actually run on, so "absent" above cannot mean "the unit is empty".
[ -x /bin/sh ] && echo "binsh=ok" >> "$out" || echo "binsh=MISSING" >> "$out"
[ -x /usr/bin/env ] && echo "usrbinenv=ok" >> "$out" || echo "usrbinenv=MISSING" >> "$out"
[ -e /etc/ssl ] && echo "etcssl=ok" >> "$out" || echo "etcssl=MISSING" >> "$out"
cg="/sys/fs/cgroup$(cut -d: -f3 /proc/self/cgroup)"
(echo max > "$cg/memory.max") 2>/dev/null && echo "cgroup=writable" >> "$out" || echo "cgroup=readonly" >> "$out"
touch /tmp/.probe-w 2>/dev/null && echo "tmp=writable" >> "$out" || echo "tmp=readonly" >> "$out"
echo "home=$HOME" >> "$out"
[ -n "$XDG_RUNTIME_DIR" ] && echo "xdg_runtime=set" >> "$out" || echo "xdg_runtime=unset" >> "$out"
# The e2e supervisor is started with a decoy ACME token in its
# environment (e2e/systemd/e2e.conf); the app must never see it.
[ -n "$ACME_TEST_TOKEN" ] && echo "acme_token=leaked" >> "$out" || echo "acme_token=absent" >> "$out"
# DNS: on systemd-resolved hosts /etc/resolv.conf points into
# /run/systemd/resolve, which the sandbox must keep reachable; here
# it proves the resolver is usable from inside at all.
getent hosts e2e-artifacts >/dev/null 2>&1 && echo "dns=ok" >> "$out" || echo "dns=fail" >> "$out"
echo "done=1" >> "$out"
echo "probe written to $out"
exec ./server-bin
