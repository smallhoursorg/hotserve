# Builds the demo app once, packs three release tarballs (v1, v2, and a
# v3 whose health check always fails), and serves them over HTTP — the
# stand-in for a GitHub/GitLab release asset URL.
FROM golang:1.25-bookworm AS build
WORKDIR /build
COPY testapp/main.go .
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
	&& tar -czf /out/demo-v3-broken.tar.gz -C /tmp/stage-v3 .

FROM caddy:2.11.4
COPY --from=build /out /srv/artifacts
COPY artifacts.Caddyfile /etc/caddy/Caddyfile
