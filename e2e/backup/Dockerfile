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
# A directory that does not enumerate its accounts, for the setup
# suite's one scenario of it: sssd, fronting a second passwd file
# through its proxy provider [M65]. Installed and no more: the package
# writes sss into nsswitch.conf and enables its service, and both are
# undone here, so that every other scenario runs on the box as Debian
# ships it. The scenario writes the passwd line and starts sssd itself.
RUN cp /etc/nsswitch.conf /etc/nsswitch.conf.debian \
	&& apt-get update \
	&& apt-get install -y --no-install-recommends sssd-common sssd-proxy libnss-sss libnss-extrausers \
	&& rm -rf /var/lib/apt/lists/* \
	&& mv /etc/nsswitch.conf.debian /etc/nsswitch.conf \
	&& systemctl mask sssd.service sssd-nss.socket sssd-pam.socket sssd-pam-priv.socket sssd-autofs.socket sssd-ssh.socket sssd-sudo.socket sssd-pac.socket sssd-kcm.socket sssd-kcm.service
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
# The package's unit files, where the .deb puts them: the units suite
# starts the services from them, and the timers' schedule is read off
# them.
COPY packaging/hotserve-backup.service packaging/hotserve-backup.timer \
	packaging/hotserve-backup-drill.service packaging/hotserve-backup-drill.timer /lib/systemd/system/
# The production layout from the package's own postinstall, as in
# e2e/Dockerfile. Two things it does are undone here: the account
# restic runs as, which the setup suite — first of the five — shows
# `hotserve-backup setup` making; and the two timers' enable state,
# since a timer firing into a scenario would take the run lock from
# under it — the units suite starts the services itself, and the smoke
# test (make install-test) is where the package's enabling is asserted.
COPY packaging/postinstall.sh /tmp/postinstall.sh
RUN mkdir -p /var/lib/hotserve /var/lib/liveswap /etc/hotserve \
	&& chmod 750 /var/lib/hotserve /var/lib/liveswap \
	&& sh /tmp/postinstall.sh && rm /tmp/postinstall.sh \
	&& userdel hotserve-backup \
	&& rm -f /etc/systemd/system/timers.target.wants/hotserve-backup.timer /etc/systemd/system/timers.target.wants/hotserve-backup-drill.timer \
	&& rm -rf /var/lib/systemd/deb-systemd-helper-enabled
COPY e2e/backup/Caddyfile /etc/hotserve/Caddyfile
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
