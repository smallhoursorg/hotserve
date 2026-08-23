#!/bin/sh
# e2e-hotserve entrypoint. The offline compose network has no CI OIDC
# provider, so the liveswap deploy path is exercised through the `local`
# trust source: when a shared volume is mounted at /shared, mint a
# keypair + a long-lived token into it (idempotent, so the crash-
# recovery restart keeps the same token), then run hotserve. The e2e
# Caddyfile trusts /shared/deploy.pub; the runner reads /shared/deploy.token.
set -e

if [ -d /shared ] && [ ! -f /shared/deploy.token ]; then
	hotserve deploy-keygen --out /shared/deploy.key >/dev/null 2>&1
	cp /shared/deploy.key.pub /shared/deploy.pub
	hotserve deploy-token --key /shared/deploy.key --audience e2e --ttl 24h >/shared/deploy.token
fi

exec hotserve run --config /etc/hotserve/Caddyfile --adapter caddyfile
