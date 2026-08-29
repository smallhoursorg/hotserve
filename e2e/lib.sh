#!/bin/sh
# Shared helpers for the e2e suites (sourced, not run). Callers set
# PROXY / HOOK / ART first; wait_for_token then sets TOKEN from the
# deploy token the e2e-hotserve container mints into the shared
# volume (e2e/mint-token.sh) — the `deploy_trust local` path standing
# in for a CI OIDC provider the offline compose network cannot reach.
FAILURES=0
TOKEN_FILE="${DEPLOY_TOKEN_FILE:-/shared/deploy.token}"

pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; FAILURES=$((FAILURES + 1)); }

wait_for_token() {
	i=0
	until [ -s "$TOKEN_FILE" ]; do
		i=$((i + 1))
		if [ "$i" -ge 60 ]; then
			echo "FATAL: no deploy token at $TOKEN_FILE within 60s"
			exit 1
		fi
		sleep 1
	done
	TOKEN=$(cat "$TOKEN_FILE")
}

# deploy <artifact-file> <version> -> prints HTTP status code; body in /tmp/deploy-body
deploy() {
	curl -s -o /tmp/deploy-body -w '%{http_code}' --max-time 60 \
		-X POST -H "Authorization: Bearer $TOKEN" \
		-d "{\"url\":\"$ART/$1\",\"version\":\"$2\"}" "$HOOK"
}

body() { curl -s --max-time 5 "$PROXY/"; }

status() { curl -s --max-time 5 -H "Authorization: Bearer $TOKEN" "$HOOK"; }

# json_str <json> <key> / json_num <json> <key>: one top-level scalar,
# enough for the status endpoint's flat fields (no jq in the images).
json_str() { printf '%s' "$1" | sed -n "s/.*\"$2\":\"\([^\"]*\)\".*/\1/p"; }
json_num() { printf '%s' "$1" | sed -n "s/.*\"$2\":\([0-9][0-9]*\).*/\1/p"; }
