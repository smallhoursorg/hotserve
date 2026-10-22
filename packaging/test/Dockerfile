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
ARG BASE_IMAGE=debian:13
FROM ${BASE_IMAGE}

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
	&& apt-get install -y --no-install-recommends \
		systemd systemd-sysv dbus ca-certificates curl \
	&& rm -rf /var/lib/apt/lists/*

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
