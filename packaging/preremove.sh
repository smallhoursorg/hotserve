#!/bin/sh
# hotserve package pre-remove: stop the service if systemd manages it.
if command -v systemctl >/dev/null 2>&1; then
	systemctl stop hotserve 2>/dev/null || true
	systemctl disable hotserve 2>/dev/null || true
fi
exit 0
