# Everything runs in Docker — no host Go toolchain required.
# CI overrides COMPOSE to add docker-compose.ci.yml (bind-mounted caches).
COMPOSE ?= docker compose

# The workspace modules. NOTE: `./...` from the repo root matches only
# the root module — nested modules are excluded from a parent module's
# pattern even in workspace mode — so test/vet name each module's tree
# explicitly.
MODULES ?= . liveswap penaltybox
PKGS = ./... ./liveswap/... ./penaltybox/...

# Release version: the tag when present (release.yml passes it in),
# else a digit-leading dev placeholder that deb/apk version rules accept.
VERSION ?= $(shell (git describe --tags --exact-match 2>/dev/null || echo v0.0.0-dev) | sed 's/^v//')

.PHONY: test test-integration vet tidy lint build package e2e e2e-logs clean

test:
	$(COMPOSE) run --rm dev go test -race -cover $(PKGS)

# -p 1: the modules' caddytest suites all pin admin :2999 / http :9080,
# so their test binaries must not run in parallel with each other.
test-integration:
	$(COMPOSE) run --rm dev go test -race -tags integration -v -run Integration -p 1 ./liveswap/... ./penaltybox/...

vet:
	$(COMPOSE) run --rm dev go vet $(PKGS)

tidy:
	for m in $(MODULES); do $(COMPOSE) run --rm -w /src/$$m dev go mod tidy || exit 1; done

lint:
	for m in $(MODULES); do $(COMPOSE) run --rm -w /src/$$m lint golangci-lint run || exit 1; done

# Cross-compiles the product binary for the release targets. GOFLAGS is
# cleared so -buildvcs is back on: caddycmd stamps the version from the
# checked-out tag via build info (the dev service disables it for test
# cache friendliness). safe.directory is needed because /src is a bind
# mount owned by a different uid than the container's root.
build:
	$(COMPOSE) run --rm -e GOFLAGS= -e CGO_ENABLED=0 dev sh -c '\
		git config --global --add safe.directory /src; \
		for a in amd64 arm64; do \
			GOOS=linux GOARCH=$$a go build -trimpath -ldflags "-s -w" -o build/hotserve-linux-$$a ./cmd/hotserve || exit 1; \
		done'

# Builds .deb and .apk for both arches into dist/. The packages carry
# the systemd unit, the starter /etc/hotserve/Caddyfile and the data
# dirs; postinstall creates the hotserve system user.
package: build
	mkdir -p dist
	for a in amd64 arm64; do \
		cp build/hotserve-linux-$$a build/hotserve; \
		for f in deb apk; do \
			$(COMPOSE) run --rm -e NFPM_ARCH=$$a -e NFPM_VERSION=$(VERSION) nfpm \
				package -f packaging/nfpm.yaml -p $$f -t dist/ || exit 1; \
		done; \
	done; \
	rm -f build/hotserve

e2e:
	$(COMPOSE) up --build --exit-code-from e2e-runner e2e-runner; \
	status=$$?; \
	if [ $$status -ne 0 ]; then $(COMPOSE) logs e2e-hotserve e2e-upstream e2e-artifacts; fi; \
	$(COMPOSE) down --remove-orphans; \
	exit $$status

e2e-logs:
	$(COMPOSE) logs e2e-hotserve e2e-upstream e2e-artifacts

clean:
	$(COMPOSE) down -v --remove-orphans
