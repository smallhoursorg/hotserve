#!/bin/bash
# Inside hsb-rc-box with B2_APPLICATION_KEY_ID / B2_APPLICATION_KEY in the
# environment: setup <repository> as bob with the right key. A new
# repository's password is masked in what is printed; an existing one's
# password is typed from /root/repo.pw.
. /tty.sh
OUT=/root/setup.out
K="Storage key id (B2_ACCOUNT_ID): "; X="Storage secret key (B2_ACCOUNT_KEY): "
repo=$1
args=("$K" "$B2_APPLICATION_KEY_ID" "$X" "$B2_APPLICATION_KEY")
if [ -s /root/repo.pw ]; then args+=("Repository password: " "$(cat /root/repo.pw)"); else args+=("Type stored to go on: " stored); fi
echo "bob@rcbox:~\$ sudo hotserve-backup setup $repo"
converse "runuser -u bob -- sh -c 'sudo hotserve-backup setup $repo'" "${args[@]}"
rc=$?
tr -d '\r' <"$OUT" | sed -e "s/$B2_APPLICATION_KEY_ID/<keyID>/g" -e 's/^\(Repository password (new): \).*/\1<52 characters>/'
echo "[exit $rc]"
