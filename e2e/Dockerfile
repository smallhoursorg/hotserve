# Builds the exact artifact an operator ships: the hotserve binary from
# cmd/hotserve (caddy standard + liveswap + penaltybox + Souin + Otter).
# This container then plays the production VPS: hotserve terminates
# HTTP, receives deploy webhooks, runs the deployed app as its own
# child process, rate-limits and caches.
FROM golang:1.26-bookworm AS builder
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOFLAGS=-buildvcs=false go build -trimpath -o /hotserve ./cmd/hotserve

FROM debian:bookworm-slim
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
COPY e2e/Caddyfile /etc/hotserve/Caddyfile
COPY e2e/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
CMD ["/bin/sh", "/usr/local/bin/entrypoint.sh"]
