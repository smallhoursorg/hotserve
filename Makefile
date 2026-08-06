# Everything runs in Docker — no host Go toolchain required.
# CI overrides COMPOSE to add docker-compose.ci.yml (bind-mounted caches).
COMPOSE ?= docker compose

# The workspace modules (go.work makes `./...` span them all; the list
# drives the per-module tidy/lint loops).
MODULES ?= . liveswap penaltybox

.PHONY: test test-integration vet tidy lint e2e e2e-logs clean

test:
	$(COMPOSE) run --rm dev go test -race -cover ./...

# -p 1: the modules' caddytest suites all pin admin :2999 / http :9080,
# so their test binaries must not run in parallel with each other.
test-integration:
	$(COMPOSE) run --rm dev go test -race -tags integration -v -run Integration -p 1 ./liveswap/... ./penaltybox/...

vet:
	$(COMPOSE) run --rm dev go vet ./...

tidy:
	for m in $(MODULES); do $(COMPOSE) run --rm -w /src/$$m dev go mod tidy || exit 1; done

lint:
	for m in $(MODULES); do $(COMPOSE) run --rm -w /src/$$m lint golangci-lint run || exit 1; done

e2e:
	$(COMPOSE) up --build --exit-code-from e2e-runner e2e-runner; \
	status=$$?; \
	if [ $$status -ne 0 ]; then $(COMPOSE) logs e2e-caddy e2e-artifacts; fi; \
	$(COMPOSE) down --remove-orphans; \
	exit $$status

e2e-logs:
	$(COMPOSE) logs e2e-caddy e2e-artifacts

clean:
	$(COMPOSE) down -v --remove-orphans
