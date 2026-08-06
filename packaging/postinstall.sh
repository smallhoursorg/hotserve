#!/bin/sh
# hotserve package post-install: create the service user, hand it the
# data directories, and let systemd (where present) see the new unit.
set -e

if command -v useradd >/dev/null 2>&1; then
	getent group hotserve >/dev/null 2>&1 || groupadd --system hotserve
	getent passwd hotserve >/dev/null 2>&1 || useradd --system \
		--gid hotserve \
		--home-dir /var/lib/hotserve \
		--shell /usr/sbin/nologin \
		--comment "hotserve server" \
		hotserve
else
	# Alpine (busybox) fallback.
	addgroup -S hotserve 2>/dev/null || true
	adduser -S -G hotserve -h /var/lib/hotserve -s /sbin/nologin hotserve 2>/dev/null || true
fi

chown hotserve:hotserve /var/lib/hotserve /var/lib/liveswap

if command -v systemctl >/dev/null 2>&1; then
	systemctl daemon-reload || true
	echo "hotserve installed. Start it with:"
	echo "  sudo systemctl enable --now hotserve"
fi
