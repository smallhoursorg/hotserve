#!/bin/sh
# Deploys a release through hotserve's webhook and prints the result.
#
#   scripts/deploy.sh <artifact URL>     the box fetches it (CI: a release asset)
#   scripts/deploy.sh <app.tar.gz>       a local file: pushed in the request body
#
#   HOTSERVE_URL          the app's webhook, e.g. https://deploy.example.com/example  (required)
#   VERSION               the release's version; defaults to the commit (12 hex
#                         chars). Versions are immutable on the box, so the
#                         default deploys once per commit: set VERSION for an
#                         uncommitted build (VERSION=wip-3, say)
#   HOTSERVE_TOKEN        a deploy token. Not needed in GitHub Actions: with
#                         `permissions: id-token: write` one is minted per run.
#   HOTSERVE_AUDIENCE     the audience the box's deploy_trust expects (default: hotserve)
#   ARTIFACT_AUTH_HEADER  sent by the box when it fetches the URL, for a private
#                         release asset (e.g. "token <github token>")
#
# The request returns once the deploy has finished: 200 with the app's
# status when the new version is live, or an error body saying why it
# was refused (the old version keeps serving). Either way the body is
# printed, and a failed deploy exits non-zero.
set -eu

artifact=${1:?artifact URL or file}
url=${HOTSERVE_URL:?set HOTSERVE_URL to the app webhook, e.g. https://deploy.example.com/example}
version=${VERSION:-$(git rev-parse --short=12 HEAD 2>/dev/null || true)}
[ -n "$version" ] || { echo "deploy.sh: not in a git checkout; set VERSION" >&2; exit 1; }

if [ -n "${ACTIONS_ID_TOKEN_REQUEST_URL:-}" ]; then
	# GitHub Actions OIDC. The token stays in this process: handing it
	# to another step as an output would print it in the job log, and it
	# can deploy until it expires.
	token=$(curl -fsS -H "Authorization: Bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
		"$ACTIONS_ID_TOKEN_REQUEST_URL&audience=${HOTSERVE_AUDIENCE:-hotserve}" |
		sed -n 's/.*"value" *: *"\([^"]*\)".*/\1/p')
	[ -n "$token" ] || { echo "deploy.sh: could not mint an OIDC token" >&2; exit 1; }
	echo "::add-mask::$token"
else
	token=${HOTSERVE_TOKEN:?set HOTSERVE_TOKEN (mint one with: hotserve deploy-token) or run in GitHub Actions with id-token: write}
fi

echo "deploying $artifact as $version to $url"
if [ -f "$artifact" ]; then
	curl --fail-with-body --silent --show-error --max-time 600 -X POST \
		-H "Authorization: Bearer $token" \
		-H "Content-Type: application/gzip" --data-binary @"$artifact" \
		"$url?version=$version"
else
	auth=${ARTIFACT_AUTH_HEADER:+,\"auth_header\":\"$ARTIFACT_AUTH_HEADER\"}
	curl --fail-with-body --silent --show-error --max-time 600 -X POST \
		-H "Authorization: Bearer $token" \
		-H "Content-Type: application/json" \
		-d "{\"url\":\"$artifact\",\"version\":\"$version\"$auth}" \
		"$url"
fi
echo
