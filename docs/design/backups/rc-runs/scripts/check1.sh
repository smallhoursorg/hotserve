#!/bin/bash
# Inside hsb-rc-box, with B2_APPLICATION_KEY_ID / B2_APPLICATION_KEY in the
# environment (docker exec --env-file): RC check 1, as bob at a real tty.
# Usage: check1.sh <repository> <secret to type: right|wrong> <times>
. /tty.sh
OUT=/root/c1.out
K="Storage key id (B2_ACCOUNT_ID): "; X="Storage secret key (B2_ACCOUNT_KEY): "
repo=$1; which=$2; n=$3
secret=$B2_APPLICATION_KEY; [ "$which" = wrong ] && secret=not-the-secret-key-0000000000
args=("$K" "$B2_APPLICATION_KEY_ID" "$X" "$secret" "Type stored to go on: " stored)
for i in $(seq 2 "$n"); do args+=("$K" "$B2_APPLICATION_KEY_ID" "$X" "$secret"); done
echo "bob@rcbox:~\$ time sudo hotserve-backup setup $repo"
t0=$(date +%s.%N)
converse "runuser -u bob -- sh -c 'sudo hotserve-backup setup $repo'" "${args[@]}"
rc=$?
t1=$(date +%s.%N)
tr -d '\r' <"$OUT" | sed -e "s/$B2_APPLICATION_KEY_ID/<keyID>/g" -e 's/^\(Repository password (new): \).*/\1<52 characters>/'
echo "[exit $rc, $(awk "BEGIN{printf \"%.1f\", $t1-$t0}")s]"
ls -la /etc/hotserve-backup/ 2>&1 | tail -n +2
