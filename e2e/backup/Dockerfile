# The box the backup suite runs on: Debian 13 with systemd as PID 1, the
# hotserve binary (the engine's plan unit runs `hotserve adapt`),
# hotserve-backup, and Debian's own restic and sqlite3 — the two
# programs whose behaviour the suite is about. hotserve itself is not
# started: nothing in a backup talks to it.
FROM golang:1.27-trixie AS builder
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOFLAGS=-buildvcs=false go build -trimpath -o /hotserve ./cmd/hotserve \
	&& cd backups && CGO_ENABLED=0 GOFLAGS=-buildvcs=false go build -trimpath -o /hotserve-backup ./cmd/hotserve-backup

FROM debian:trixie-slim
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
	&& apt-get install -y --no-install-recommends \
		systemd systemd-sysv dbus libpam-systemd procps ca-certificates restic sqlite3 \
	&& rm -rf /var/lib/apt/lists/*
RUN systemctl mask \
	systemd-udevd.service \
	systemd-udevd-kernel.socket \
	systemd-udevd-control.socket \
	systemd-modules-load.service \
	getty.target \
	console-getty.service \
	|| true
COPY --from=builder /hotserve /usr/bin/hotserve
COPY --from=builder /hotserve-backup /usr/bin/hotserve-backup
# The production layout from the package's own postinstall, as in
# e2e/Dockerfile. The account restic runs as is not made here: the
# setup suite, which runs first, has `hotserve-backup setup` make it.
COPY packaging/postinstall.sh /tmp/postinstall.sh
RUN mkdir -p /var/lib/hotserve /var/lib/liveswap /etc/hotserve \
	&& chmod 750 /var/lib/hotserve /var/lib/liveswap \
	&& sh /tmp/postinstall.sh && rm /tmp/postinstall.sh
COPY e2e/backup/Caddyfile /etc/hotserve/Caddyfile
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
