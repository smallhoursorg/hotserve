# Builds the demo app once, packs five release tarballs (v1, v2, a v3
# whose health check always fails, a "workers" release whose ./server
# is a shell leader that forks a worker before exec'ing the app — the
# process-tree shape the systemd suite kills — and a "probe" release
# whose ./server records its sandbox view before exec'ing the app),
# and serves them over HTTP — the stand-in for a GitHub/GitLab release
# asset URL.
FROM golang:1.26-trixie AS build
WORKDIR /build
COPY testapp/main.go workers.sh probe.sh ./
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
	&& cp /build/probe.sh /tmp/stage-probe/server \
	&& chmod +x /tmp/stage-probe/server \
	&& echo probe > /tmp/stage-probe/version.txt \
	&& tar -czf /out/demo-probe.tar.gz -C /tmp/stage-probe .

FROM caddy:2.11.4
COPY --from=build /out /srv/artifacts
COPY artifacts.Caddyfile /etc/caddy/Caddyfile
