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
# else a digit-leading dev placeholder that deb version rules accept.
VERSION ?= $(shell (git describe --tags --exact-match 2>/dev/null || echo v0.0.0-dev) | sed 's/^v//')

# Distro image for the package install smoke test (install-test).
DISTRO ?= debian:12

.PHONY: test test-integration vet tidy lint fuzz fuzz-list vulncheck secretscan build package install-test e2e soak e2e-logs clean

test:
	$(COMPOSE) run --rm dev go test -race -cover $(PKGS)

# The privileged systemd lanes (test-integration, e2e, install-test)
# need a cgroup-v2 Docker host: Docker Desktop, or Linux with systemd.
# Checked up front so a plain cgroup-v1 daemon fails with one clear
# line instead of an opaque systemd boot error.
define cgroup2_preflight
	@v=$$(docker info --format '{{.CgroupVersion}}' 2>/dev/null); \
	if [ "$$v" != "2" ]; then \
		echo "this target runs systemd inside a privileged container and needs a cgroup-v2 Docker host (got CgroupVersion=$${v:-unknown}): Docker Desktop, or Linux with systemd"; \
		exit 1; \
	fi
endef

# Runs inside the dev-systemd container (systemd as PID 1, root's user
# manager started by test/systemd/ready.sh): liveswap's runner creates
# real transient units, and the caddytest scenarios deploy a real app
# through them. -p 1: the modules' caddytest suites all pin admin :2999
# / http :9080, so their test binaries must not run in parallel.
test-integration:
	$(cgroup2_preflight)
	$(COMPOSE) up --build -d dev-systemd
	status=0; \
	$(COMPOSE) exec -T dev-systemd /bin/sh /src/test/systemd/ready.sh || status=1; \
	if [ $$status -eq 0 ]; then \
		$(COMPOSE) exec -T -e XDG_RUNTIME_DIR=/run/user/0 dev-systemd \
			go test -race -tags integration -v -run Integration -p 1 ./liveswap/... ./penaltybox/... || status=1; \
	fi; \
	if [ $$status -ne 0 ]; then $(COMPOSE) exec -T dev-systemd journalctl --no-pager -n 100 || true; fi; \
	$(COMPOSE) rm -sf dev-systemd >/dev/null; \
	exit $$status

# Both tag sets: the integration-tagged files are not compiled by
# `make test` or a bare vet, so a signature change that breaks them
# stays invisible until the integration lane boots a whole systemd
# container to find out.
vet:
	$(COMPOSE) run --rm dev go vet $(PKGS)
	$(COMPOSE) run --rm dev go vet -tags integration $(PKGS)

tidy:
	for m in $(MODULES); do $(COMPOSE) run --rm -w /src/$$m dev go mod tidy || exit 1; done

lint:
	for m in $(MODULES); do $(COMPOSE) run --rm -w /src/$$m lint golangci-lint run || exit 1; done

# Real fuzzing of the untrusted-input surfaces (seed corpora already
# run inside `make test`). One target at a time — Go allows a single
# -fuzz pattern per invocation. The corpus accumulates in the
# gobuildcache volume across runs.
FUZZTIME ?= 2m
FUZZ_MODULES = liveswap penaltybox

# Fuzz targets are discovered (`go test -list '^Fuzz'`), never listed by
# hand, so a new target cannot be left out of the weekly run. fuzz-list
# is the deterministic half, run in every PR: it prints what fuzz will
# run, fails if a module has no targets, and fails if a committed
# testdata/fuzz/<Target> corpus (a crasher kept as a regression input)
# no longer has a target to replay it — dead corpora test nothing.
# The seed corpora themselves run in `make test`.
fuzz-list:
	@for m in $(FUZZ_MODULES); do \
		targets=$$($(COMPOSE) run --rm -T -w /src/$$m dev go test -list '^Fuzz' . | grep '^Fuzz'); \
		[ -n "$$targets" ] || { echo "no fuzz targets found in $$m"; exit 1; }; \
		for t in $$targets; do echo "$$m $$t"; done; \
		for d in $$m/testdata/fuzz/*/; do \
			[ -d "$$d" ] || continue; \
			n=$$(basename "$$d"); \
			echo "$$targets" | grep -qx "$$n" || { echo "$$d has no matching Fuzz target"; exit 1; }; \
		done; \
	done

fuzz:
	for m in $(FUZZ_MODULES); do \
		targets=$$($(COMPOSE) run --rm -T -w /src/$$m dev go test -list '^Fuzz' . | grep '^Fuzz'); \
		[ -n "$$targets" ] || { echo "no fuzz targets found in $$m"; exit 1; }; \
		for t in $$targets; do \
			$(COMPOSE) run --rm -w /src/$$m dev \
				go test -run '^$$' -fuzz "^$$t$$" -fuzztime $(FUZZTIME) . || exit 1; \
		done; \
	done

# Known-vulnerability scan per module. govulncheck is a `tool`
# dependency in each module's go.mod — never compiled into the product,
# invisible to importers — so Dependabot bumps it like any require. A
# version literal here (`go run ...@vX`) would be a pin nothing
# watches: no ecosystem parses shell strings. The vuln database itself
# always updates regardless of tool version, which is why vulncheck.yml
# also runs this on a weekly cron.
vulncheck:
	for m in $(MODULES); do \
		$(COMPOSE) run --rm -w /src/$$m dev \
			go tool govulncheck ./... || exit 1; \
	done

# Full-history secret scan — the same engine and image as CI's gitleaks
# gate (the image is pinned in docker-compose.yml, where Dependabot
# watches it), so local and CI can never drift.
secretscan:
	$(COMPOSE) run --rm gitleaks detect --source /src --redact -v

# Cross-compiles the product binary for the release targets. GOFLAGS is
# cleared so -buildvcs is back on: caddycmd stamps the version from the
# checked-out tag via build info (the dev service disables it for test
# cache friendliness). safe.directory is needed because /src is a bind
# mount owned by a different uid than the container's root.
# The trailing chmod is for Linux CI runners: the container runs as
# root, so without it the bind-mounted build/ is unwritable to the
# host user and package's cp/rm staging fails (macOS masks this).
# The two arch builds run concurrently: cache-warm, wall time is
# mostly the two large links, and those overlap cleanly.
build:
	$(COMPOSE) run --rm -e GOFLAGS= -e CGO_ENABLED=0 dev sh -c '\
		git config --global --add safe.directory /src; \
		GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o build/hotserve-linux-amd64 ./cmd/hotserve & p1=$$!; \
		GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o build/hotserve-linux-arm64 ./cmd/hotserve & p2=$$!; \
		wait $$p1 || exit 1; wait $$p2 || exit 1; \
		chmod -R a+rwX build'

# Builds .deb for both arches into dist/. (.apk dropped until it can
# ship a working OpenRC service and an Alpine install-test lane — a
# package that installs but starts nothing is worse than the raw
# tarball; returns with the hosted-repo phase.) The packages carry
# the systemd unit, the starter /etc/hotserve/Caddyfile and the data
# dirs; postinstall creates the hotserve system user.
package: build
	mkdir -p dist
	for a in amd64 arm64; do \
		cp build/hotserve-linux-$$a build/hotserve; \
		for f in deb; do \
			$(COMPOSE) run --rm -e NFPM_ARCH=$$a -e NFPM_VERSION=$(VERSION) nfpm \
				package -f packaging/nfpm.yaml -p $$f -t dist/ || exit 1; \
		done; \
	done; \
	rm -f build/hotserve

# Installs the freshly built .deb inside a systemd container (DISTRO
# picks the base image) and runs the staged smoke test: install, unit
# boot, a real liveswap deploy under the sandbox, reinstall, remove.
# Needs dist/ populated first (make package). --privileged +
# --cgroupns=host with the cgroup mount is the reliable
# systemd-in-docker recipe on cgroup-v2 hosts (GitHub runners and
# Docker Desktop alike).
# /sys/kernel/security (securityfs) is bound from the host so the
# package's AppArmor profile can be loaded from inside the container
# into the host kernel's policy — that is how the cells prove the
# sandbox under an Ubuntu kernel's user-namespace restriction (CI's
# runners); on a host without AppArmor the mount is inert.
install-test:
	$(cgroup2_preflight)
	docker build -t hotserve-install-test-$(subst :,-,$(DISTRO)) \
		--build-arg BASE_IMAGE=$(DISTRO) packaging/test
	docker rm -f hotserve-smoke >/dev/null 2>&1 || true
	docker run -d --name hotserve-smoke --privileged --cgroupns=host \
		--tmpfs /run --tmpfs /run/lock \
		-v /sys/fs/cgroup:/sys/fs/cgroup:rw \
		-v /sys/kernel/security:/sys/kernel/security \
		-v $(CURDIR)/dist:/dist:ro \
		-v $(CURDIR)/packaging/test/smoke.sh:/smoke.sh:ro \
		hotserve-install-test-$(subst :,-,$(DISTRO))
	docker exec hotserve-smoke /bin/bash /smoke.sh; status=$$?; \
	if [ $$status -ne 0 ]; then \
		docker exec hotserve-smoke journalctl -u hotserve --no-pager || true; \
	fi; \
	docker rm -f hotserve-smoke >/dev/null; \
	exit $$status

# The main suites run via the e2e-runner entrypoint. Then the systemd
# suite runs INSIDE the hotserve container (it needs systemctl,
# journalctl and the process tree): restart survival, SIGKILL of
# hotserve + reattach, cgroup teardown of a worker tree, crash
# cleanup, journal output. The recovery suite is the runner's view
# after all that: still serving, deploys still work.
e2e:
	$(cgroup2_preflight)
	$(COMPOSE) up --build -d e2e-hotserve e2e-upstream e2e-artifacts
	status=0; \
	$(COMPOSE) run --rm e2e-runner || status=1; \
	echo "════ systemd suite: restart survival, reattach, cgroup teardown ════"; \
	$(COMPOSE) exec -T e2e-hotserve /bin/sh /suite-systemd.sh || status=1; \
	echo "════ recovery suite: the runner's view after hotserve's unclean death ════"; \
	$(COMPOSE) run --rm --entrypoint "/bin/sh /suite-recovery.sh" e2e-runner || status=1; \
	if [ $$status -ne 0 ]; then \
		$(COMPOSE) logs e2e-upstream e2e-artifacts; \
		$(COMPOSE) exec -T e2e-hotserve journalctl --no-pager -n 300 || true; \
	fi; \
	$(COMPOSE) down --remove-orphans; \
	exit $$status

# Leak-hunting soak against the product binary: deploy/reload/traffic
# churn, then goroutine/fd return-to-baseline assertions over the admin
# metrics endpoint. ~15-20 min at defaults; tune with SOAK_DEPLOYS,
# SOAK_RELOADS, SOAK_CLIENTS, SOAK_REQS. Runs weekly in CI (soak.yml),
# never in the PR path.
soak:
	$(COMPOSE) up --build -d e2e-hotserve e2e-upstream e2e-artifacts
	status=0; \
	$(COMPOSE) run --rm -e SOAK_DEPLOYS -e SOAK_RELOADS -e SOAK_CLIENTS -e SOAK_REQS \
		--entrypoint "/bin/sh /soak.sh" e2e-runner || status=1; \
	if [ $$status -ne 0 ]; then $(COMPOSE) logs --tail 200 e2e-hotserve; fi; \
	$(COMPOSE) down --remove-orphans; \
	exit $$status

e2e-logs:
	$(COMPOSE) logs e2e-upstream e2e-artifacts
	$(COMPOSE) exec -T e2e-hotserve journalctl --no-pager -n 300

clean:
	$(COMPOSE) down -v --remove-orphans
