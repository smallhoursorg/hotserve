# Builds the demo app once, packs four release tarballs (v1, v2, a v3
# whose health check always fails, and a "workers" release whose
# ./server is a shell leader that forks a worker before exec'ing the
# app — the process-tree shape the systemd suite kills), and serves
# them over HTTP — the stand-in for a GitHub/GitLab release asset URL.
FROM golang:1.26-trixie AS build
WORKDIR /build
COPY testapp/main.go workers.sh ./
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
	&& tar -czf /out/demo-workers.tar.gz -C /tmp/stage-workers .

FROM caddy:2.11.4
COPY --from=build /out /srv/artifacts
COPY artifacts.Caddyfile /etc/caddy/Caddyfile
