#!/bin/sh
# Deploys a release through hotserve's webhook and prints the result.
#
#   scripts/deploy.sh <artifact URL>     the box fetches it (CI: a release asset)
#   scripts/deploy.sh <app.tar.gz>       a local file: pushed in the request body
#   scripts/deploy.sh --rollback <ver>   relaunches a version still on the box's disk
#
#   HOTSERVE_URL          the app's webhook, e.g. https://deploy.example.com/example  (required)
#   VERSION               the release's version; defaults to the commit (12 hex
#                         chars). Versions are immutable on the box, so the
#                         default deploys once per commit: set VERSION for an
#                         uncommitted build (VERSION=wip-3, say). Not used by
#                         --rollback: that names its version itself
#   HOTSERVE_TOKEN        a deploy token. Not needed in GitHub Actions: with
#                         `permissions: id-token: write` one is minted per run.
#   HOTSERVE_AUDIENCE     the audience the box's deploy_trust expects (default: hotserve)
#   ARTIFACT_AUTH_HEADER  sent by the box as Authorization when it fetches the
#                         URL: "token <github token>" reads a release asset by
#                         its API URL, private repo or not (the workflow sends
#                         the job's own token)
#
# The request returns once the deploy (or rollback: the same start,
# health gate and cutover, from a release already on disk) has
# finished: 200 with the app's status when the new version is live, or
# an error body saying why it was refused (the old version keeps
# serving). Either way the body is printed, and a failure exits non-zero.
set -eu

rollback=
if [ "${1:-}" = --rollback ]; then
	rollback=${2:?--rollback needs the version to roll back to}
	shift 2
	[ $# -eq 0 ] || { echo "deploy.sh: --rollback takes only a version" >&2; exit 1; }
else
	artifact=${1:?artifact URL or file, or --rollback <version>}
fi
url=${HOTSERVE_URL:?set HOTSERVE_URL to the app webhook, e.g. https://deploy.example.com/example}
if [ -z "$rollback" ]; then
	version=${VERSION:-$(git rev-parse --short=12 HEAD 2>/dev/null || true)}
	[ -n "$version" ] || { echo "deploy.sh: not in a git checkout; set VERSION" >&2; exit 1; }
fi

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

if [ -n "$rollback" ]; then
	echo "rolling $url back to $rollback"
	curl --fail-with-body --silent --show-error --max-time 600 -X POST \
		-H "Authorization: Bearer $token" \
		"$url?rollback=$rollback"
	echo
	exit 0
fi

# Never print a URL's query string: that is where presigned-URL
# credentials live (the box redacts it from its logs for the same
# reason), and Actions output is readable by anyone who can see the job.
echo "deploying ${artifact%%\?*} as $version to $url"
if [ -f "$artifact" ]; then
	curl --fail-with-body --silent --show-error --max-time 600 -X POST \
		-H "Authorization: Bearer $token" \
		-H "Content-Type: application/gzip" --data-binary @"$artifact" \
		"$url?version=$version"
else
	# JSON-escape what goes into the body (a quote or backslash in a
	# header value would otherwise make it malformed).
	json() { printf '%s' "$1" | sed 's/[\\"]/\\&/g'; }
	auth=${ARTIFACT_AUTH_HEADER:+,\"auth_header\":\"$(json "$ARTIFACT_AUTH_HEADER")\"}
	curl --fail-with-body --silent --show-error --max-time 600 -X POST \
		-H "Authorization: Bearer $token" \
		-H "Content-Type: application/json" \
		-d "{\"url\":\"$(json "$artifact")\",\"version\":\"$(json "$version")\"$auth}" \
		"$url"
fi
echo
