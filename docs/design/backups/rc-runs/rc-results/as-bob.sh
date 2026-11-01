#!/bin/bash
# Run inside hsb-docs-box as root: each argument is one line bob types,
# run as bob (full sudo) at a real terminal (script(1)'s pty); a line
# that asks is given its answers as "prompt=answer" pairs after "::".
#   bash /root/as-bob.sh 'sudo hotserve-backup drill'
#   bash /root/as-bob.sh 'sudo hotserve-backup restore demo' :: 'go on: =demo'
. /tty.sh
OUT=/root/bob.out
line=$1; shift
answers=()
if [ "${1:-}" = "::" ]; then
	shift
	for pa in "$@"; do answers+=("${pa%%=*}" "${pa#*=}"); done
fi
echo "bob@$(hostname):~\$ $line"
t0=$(date +%s.%N)
converse "runuser -u bob -- sh -c 'cd ~ && $line'" "${answers[@]}"
rc=$?
t1=$(date +%s.%N)
tr -d '\r' <"$OUT"
echo "[exit $rc, $(echo "$t1 - $t0" | bc 2>/dev/null || awk "BEGIN{print $t1-$t0}")s, $(date -u +%FT%TZ)]"
