# Systemd-in-docker image for the package install smoke test
# (`make install-test`). BASE_IMAGE is debian:13 — the support matrix is
# one release (systemd 257), so the smoke test asserts the full sandbox
# tier unconditionally. BASE_IMAGE stays a build arg so a candidate
# release can be tried against a newer Debian without editing this file.
#
# networkd/resolved are deliberately NOT installed: with no wait-online
# unit enabled, network-online.target (which hotserve.service Requires=)
# is reached trivially inside the container. dbus is here because
# logind (loginctl enable-linger in postinstall) needs the system bus;
# libpam-systemd is NOT — it is a package Depends, and the smoke test
# proves apt resolves it.
# (Declared before the first FROM, so that every stage sees it.)
ARG BASE_IMAGE=debian:13

# rclone: the S3 server the backups README's lines are run against —
# `rclone serve s3` in this same container, the static binary out of
# rclone's own image as e2e/backup/s3.Dockerfile takes it (Debian's
# rclone has no `serve s3`). The same pin as there; Dependabot bumps
# both.
FROM rclone/rclone:1.71@sha256:3103526c506266a9ecdf064efe99bf3677d92ef6407af124d8c56b4f49cbaa51 AS rclone

FROM ${BASE_IMAGE}

# sudo: the backups README's lines are run as an administrator, under
# sudo, the way they are written. restic and sqlite3 are NOT here: the
# package depends on them, and the smoke test proves apt brings them
# to an image that installs no recommended package. ca-certificates is
# here for curl, so its arrival by Depends is proved by the package's
# declaration alone (smoke.sh, stage 1).
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
	&& apt-get install -y --no-install-recommends \
		systemd systemd-sysv dbus ca-certificates curl sudo \
	&& rm -rf /var/lib/apt/lists/*
# No recommended package, ever, as a provider's image may have it:
# Hetzner's Debian 13 sets this. Every install the smoke makes — the
# first, each reinstall after a remove or a purge — then gets only what
# the package depends on, and a Recommends entry the package came to
# lean on would fail here rather than pass by apt's default.
RUN printf 'APT::Install-Recommends "false";\n' > /etc/apt/apt.conf.d/99no-recommends
COPY --from=rclone /usr/local/bin/rclone /usr/local/bin/rclone
# The Debian image forbids maintainer scripts to start services
# (/usr/sbin/policy-rc.d exits 101, which deb-systemd-invoke obeys). A
# real box has no such file, and postinstall starting the backup timers
# is part of what the smoke test proves.
RUN rm -f /usr/sbin/policy-rc.d

# The Debian image forbids maintainer scripts to start services
# (/usr/sbin/policy-rc.d exits 101, which deb-systemd-invoke obeys). A
# real box has no such file, and postinstall starting the box applier's
# path unit is part of what the smoke test proves.
RUN rm -f /usr/sbin/policy-rc.d

# Units that cannot work in a container and would otherwise leave the
# boot stuck or the system needlessly degraded.
RUN systemctl mask \
	systemd-udevd.service \
	systemd-udevd-kernel.socket \
	systemd-udevd-control.socket \
	systemd-modules-load.service \
	getty.target \
	console-getty.service \
	|| true

STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
