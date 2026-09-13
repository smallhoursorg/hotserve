# Builds the demo app once, packs five release tarballs (v1, v2, a v3
# whose health check always fails, a "workers" release whose ./server
# is a shell leader that forks a worker before exec'ing the app — the
# process-tree shape the systemd suite kills — and a "probe" release
# whose ./server records its sandbox view before exec'ing the app),
# and serves them over HTTP — the stand-in for a GitHub/GitLab release
# asset URL.
#
# The build context is the repo root, not this directory (see
# docker-compose.yml), which is why every COPY path is repo-relative:
# the probe release carries liveswap/testdata/sandbox-view.sh, one view probe shared
# with liveswap's integration test and packaging/test/smoke.sh (#52).
FROM golang:1.27-trixie AS build
WORKDIR /build
COPY e2e/liveswap/testapp/main.go e2e/liveswap/workers.sh e2e/liveswap/probe-server.sh liveswap/testdata/sandbox-view.sh ./
RUN CGO_ENABLED=0 go build -o server main.go
RUN mkdir /out \
	&& for v in v1 v2; do \
		mkdir /tmp/stage-$v \
		&& cp /build/server /tmp/stage-$v/ \
		&& echo $v > /tmp/stage-$v/version.txt \
		&& tar -czf /out/demo-$v.tar.gz -C /tmp/stage-$v .; \
	done \
	&& mkdir /tmp/stage-v3 \
	&& cp /build/server /tmp/stage-v3/ \
	&& echo v3 > /tmp/stage-v3/version.txt \
	&& touch /tmp/stage-v3/broken \
	&& tar -czf /out/demo-v3-broken.tar.gz -C /tmp/stage-v3 . \
	&& mkdir /tmp/stage-workers \
	&& cp /build/server /tmp/stage-workers/server-bin \
	&& cp /build/workers.sh /tmp/stage-workers/server \
	&& chmod +x /tmp/stage-workers/server \
	&& echo workers > /tmp/stage-workers/version.txt \
	&& tar -czf /out/demo-workers.tar.gz -C /tmp/stage-workers . \
	&& mkdir /tmp/stage-probe \
	&& cp /build/server /tmp/stage-probe/server-bin \
	&& cp /build/probe-server.sh /tmp/stage-probe/server \
	&& cp /build/sandbox-view.sh /tmp/stage-probe/ \
	&& chmod +x /tmp/stage-probe/server \
	&& echo probe > /tmp/stage-probe/version.txt \
	&& tar -czf /out/demo-probe.tar.gz -C /tmp/stage-probe .

# The Deno example, built by its own scripts/bundle.sh with the same
# Deno version e2e/Dockerfile installs on the box.
FROM denoland/deno:2.9.6 AS deno-build
WORKDIR /example
COPY examples/deno/ ./
RUN sh scripts/bundle.sh

# The Node example, built by its own scripts/bundle.sh: Node's
# single-executable build, so the e2e box needs no Node installed.
FROM node:26-trixie-slim AS node-build
WORKDIR /example
COPY examples/node/ ./
RUN sh scripts/bundle.sh

FROM caddy:2.11.4
COPY --from=build /out /srv/artifacts
COPY --from=deno-build /example/app.tar.gz /srv/artifacts/deno-example.tar.gz
COPY --from=node-build /example/app.tar.gz /srv/artifacts/node-example.tar.gz
COPY e2e/liveswap/artifacts.Caddyfile /etc/caddy/Caddyfile
