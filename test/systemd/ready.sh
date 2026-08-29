#!/bin/sh
# Waits for systemd inside the dev-systemd container to finish booting,
# then starts root's user manager — the one the integration tests (run
# as root with XDG_RUNTIME_DIR=/run/user/0) create app units under.
# "degraded" is normal in a container: masked units count as failed.
set -eu
i=0
# is-system-running exits non-zero for every state but "running", so
# decide on its output, not its status.
while :; do
	state=$(systemctl is-system-running 2>/dev/null || true)
	case "$state" in running|degraded) break ;; esac
	i=$((i + 1))
	if [ "$i" -ge 60 ]; then
		echo "systemd did not reach running/degraded within 60s (state: ${state:-unknown})" >&2
		systemctl --failed --no-pager >&2 || true
		exit 1
	fi
	sleep 1
done
systemctl start user@0.service
i=0
until [ -S /run/user/0/systemd/private ]; do
	i=$((i + 1))
	if [ "$i" -ge 30 ]; then
		echo "root's user manager has no private socket after 30s" >&2
		systemctl status user@0.service --no-pager >&2 || true
		exit 1
	fi
	sleep 1
done
echo "systemd $(systemctl --version | head -1 | cut -d' ' -f2) ready; user@0 active"
