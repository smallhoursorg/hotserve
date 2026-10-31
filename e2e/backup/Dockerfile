# The box the backup suite runs on: Debian 13 with systemd as PID 1, the
# hotserve binary (the engine's plan unit runs `hotserve adapt`),
# hotserve-backup, and Debian's own restic and sqlite3 — the two
# programs whose behaviour the suite is about — and curl, with which the
# check suite deletes a pack through the storage's own API, as someone
# holding a key that may delete would, off the box. hotserve itself is
# not started: nothing in a backup talks to it. The two binaries come
# built, from build/e2e-backup (the bin context, docker-compose.yml),
# which `make e2e-backup` fills first. Build this image through that
# target only: built by hand it copies whatever binaries the last run
# left there, not the source as it is now.
FROM debian:trixie-slim
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
	&& apt-get install -y --no-install-recommends \
		systemd systemd-sysv dbus libpam-systemd procps ca-certificates restic sqlite3 curl \
	&& rm -rf /var/lib/apt/lists/*
RUN systemctl mask \
	systemd-udevd.service \
	systemd-udevd-kernel.socket \
	systemd-udevd-control.socket \
	systemd-modules-load.service \
	getty.target \
	console-getty.service \
	|| true
COPY --from=bin hotserve hotserve-backup /usr/bin/
# The package's unit files, where the .deb puts them: the units suite
# starts the services from them, and the timers' schedule is read off
# them.
COPY packaging/hotserve-backup.service packaging/hotserve-backup.timer \
	packaging/hotserve-backup-drill.service packaging/hotserve-backup-drill.timer /lib/systemd/system/
# The production layout from the package's own postinstall, as in
# e2e/Dockerfile. Two things it does are undone here: the account
# restic runs as, which the setup suite, on a box of its own, shows
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
