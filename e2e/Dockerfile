# Builds the exact artifact an operator ships: the hotserve binary from
# cmd/hotserve (caddy standard + liveswap + penaltybox + Souin + Otter).
# The e2e-hotserve container then plays the production VPS: systemd as
# PID 1 running the packaged hotserve.service as the hotserve user,
# with deployed apps as transient units under that user's own systemd
# manager — exactly the layout the .deb's postinstall produces.
# (e2e-upstream reuses the image as a plain `hotserve run` container.)
FROM golang:1.27-trixie AS builder
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOFLAGS=-buildvcs=false go build -trimpath -o /hotserve ./cmd/hotserve

FROM debian:trixie-slim
ENV DEBIAN_FRONTEND=noninteractive
# libpam-systemd: pam_systemd gives user@<uid>.service its runtime dir;
# dbus: logind; procps + curl: the in-container systemd suite.
RUN apt-get update \
	&& apt-get install -y --no-install-recommends \
		systemd systemd-sysv dbus libpam-systemd procps curl ca-certificates apparmor \
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
# Fail the build outright if any module didn't link in. (Otter is
# additionally proven at startup: the `otter` cache option makes config
# load fail if the storage module is absent.)
RUN hotserve list-modules | grep -qx liveswap \
	&& hotserve list-modules | grep -qx http.handlers.liveswap_webhook \
	&& hotserve list-modules | grep -qx http.reverse_proxy.upstreams.liveswap \
	&& hotserve list-modules | grep -qx http.handlers.hint_penaltybox \
	&& hotserve list-modules | grep -qx http.handlers.cache \
	&& hotserve list-modules | grep -qx storages.cache.otter

# The production layout, produced by the package's own postinstall
# (user, data dirs, lingering, the user@<uid> drop-ins) so the e2e
# container is what a .deb install is — never a re-implementation.
# systemd is not running at build time; postinstall tolerates that
# (linger-file fallback, daemon-reload/try-restart ignored).
COPY packaging/hotserve.service /etc/systemd/system/hotserve.service
COPY packaging/apparmor/hotserve-user-manager /etc/apparmor.d/hotserve-user-manager
COPY packaging/user-manager.sh /usr/libexec/hotserve/user-manager
COPY packaging/postinstall.sh /tmp/postinstall.sh
RUN mkdir -p /var/lib/hotserve /var/lib/liveswap \
	&& chmod 750 /var/lib/hotserve /var/lib/liveswap \
	&& sh /tmp/postinstall.sh && rm /tmp/postinstall.sh
COPY e2e/systemd/e2e.conf /etc/systemd/system/hotserve.service.d/e2e.conf
COPY e2e/systemd/e2e-token.service /etc/systemd/system/e2e-token.service
COPY e2e/systemd/e2e-apparmor.service /etc/systemd/system/e2e-apparmor.service
RUN systemctl enable hotserve.service e2e-token.service e2e-apparmor.service
COPY e2e/Caddyfile /etc/hotserve/Caddyfile
COPY e2e/mint-token.sh /usr/local/bin/mint-token.sh
RUN chmod +x /usr/local/bin/mint-token.sh

STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
