#!/bin/sh
# hotserve: the hotserve user's systemd manager is started through this
# wrapper (user@<uid>.service drop-in, written by postinstall) so the
# shipped AppArmor profile can attach to it by path — the one kind of
# profile entry that lifts Ubuntu's unprivileged-user-namespace
# restriction for an unprivileged process (a change-profile request
# from one gets converted into a stack with "unconfined", which stays
# restricted). Without AppArmor this is a plain exec.
if [ -x /usr/lib/systemd/systemd ]; then
	exec /usr/lib/systemd/systemd "$@"
fi
exec /lib/systemd/systemd "$@"
