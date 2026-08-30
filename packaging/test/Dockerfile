# Systemd-in-docker image for the package install smoke test
# (`make install-test`). One Dockerfile serves every matrix cell:
# BASE_IMAGE picks the distro: debian:12, debian:13, ubuntu:24.04 or
# ubuntu:26.04 — the support matrix; all share the same apt invocation.
# systemd is 252 / 257 / 255 / 257 respectively: debian:13 and
# ubuntu:26.04 are the >= 256 cells that the planned per-app isolation
# will need, the other two the older-systemd compatibility path.
#
# networkd/resolved are deliberately NOT installed: with no wait-online
# unit enabled, network-online.target (which hotserve.service Requires=)
# is reached trivially inside the container. dbus is here because
# logind (loginctl enable-linger in postinstall) needs the system bus;
# libpam-systemd is NOT — it is a package Depends, and the smoke test
# proves apt resolves it.
ARG BASE_IMAGE=debian:12
FROM ${BASE_IMAGE}

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
	&& apt-get install -y --no-install-recommends \
		systemd systemd-sysv dbus ca-certificates curl apparmor \
	&& rm -rf /var/lib/apt/lists/*

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
