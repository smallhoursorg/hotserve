# Builds the exact artifact an operator would ship: a Caddy binary with
# the liveswap module compiled in via xcaddy. This container then plays
# the role of the production VPS: Caddy terminates HTTP, receives the
# deploy webhook, and runs the deployed app as its own child process.
FROM caddy:2.11.4-builder AS builder
COPY . /src
RUN xcaddy build v2.11.4 \
	--with github.com/hotsauce-team/hotserve/liveswap=/src

FROM caddy:2.11.4
COPY --from=builder /usr/bin/caddy /usr/bin/caddy
# Fail the build outright if any of the three modules didn't link in.
RUN caddy list-modules | grep -q '^liveswap$' \
	&& caddy list-modules | grep -q '^http.handlers.liveswap_webhook$' \
	&& caddy list-modules | grep -q '^http.reverse_proxy.upstreams.liveswap$'
COPY e2e/Caddyfile /etc/caddy/Caddyfile
