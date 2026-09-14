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
#
# In GitHub Actions the same output is also dressed for the job page:
# the request's output in a collapsible group, a failure as an error
# annotation naming the phase and the cause, and one line in the job
# summary. Nothing else is sent or printed; a run from a laptop sees
# none of it.
set -eu

rollback=
if [ "${1:-}" = --rollback ]; then
	rollback=${2:?--rollback needs the version to roll back to}
	shift 2
	# The box's version alphabet, so a stray character is refused here
	# rather than mangling the query (a `#` would drop the rest of it).
	case $rollback in
	''|.*|*[!A-Za-z0-9._-]*) echo "deploy.sh: '$rollback' is not a version (letters, digits, . _ -; not starting with .)" >&2; exit 1 ;;
	esac
else
	artifact=${1:?artifact URL or file, or --rollback <version>}
	shift
fi
# `deno task deploy --rollback v` / `npm run deploy -- --rollback v`
# would arrive here with app.tar.gz already in front of the flag and
# push a stale tarball; refuse anything after the one operand.
[ $# -eq 0 ] || { echo "deploy.sh: unexpected argument '$1' (--rollback goes first, without a tarball)" >&2; exit 1; }
url=${HOTSERVE_URL:?set HOTSERVE_URL to the app webhook, e.g. https://deploy.example.com/example}
app=${url##*/}
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

# The Actions dressing. `field` reads one string field out of the
# response (no jq here: this also runs from laptops): the last
# occurrence, which for "phase" is the failing phase inside last_deploy
# and for "error" its cause, with JSON's \" and \\ unescaped so a quote
# in the cause does not cut it short. `prop` and `msg` escape what a
# workflow command's property and message may not contain.
actions=${GITHUB_ACTIONS:-}
field() {
	printf '%s' "$1" | sed -n 's/.*"'"$2"'":"\(\([^\\"]*\\.\)*[^\\"]*\)".*/\1/p' | sed 's/\\\(["\\]\)/\1/g'
}
msg() { printf '%s' "$1" | sed 's/%/%25/g' | tr '\r\n' '  '; }
prop() { msg "$1" | sed 's/:/%3A/g; s/,/%2C/g'; }
began=$(date +%s)
body=$(mktemp)
trap 'rm -f "$body"' EXIT
finish() { # <curl exit status> <what>: prints the body, dresses it, exits on failure
	rc=$1 what=$2
	cat "$body"
	echo
	[ -n "$actions" ] && echo "::endgroup::"
	took="$(( $(date +%s) - began ))s"
	if [ "$rc" -eq 0 ]; then
		[ -n "${GITHUB_STEP_SUMMARY:-}" ] && printf '**hotserve:** %s live, %s\n\n' "$what" "$took" >>"$GITHUB_STEP_SUMMARY"
		return 0
	fi
	phase=$(field "$(cat "$body")" phase)
	why=$(field "$(cat "$body")" error)
	[ -n "$actions" ] && echo "::error title=$(prop "hotserve: $what failed")::$(msg "${phase:+in $phase: }${why:-see the response above}")"
	[ -n "${GITHUB_STEP_SUMMARY:-}" ] && printf '**hotserve:** %s failed%s, %s\n\n%s\n\n' "$what" "${phase:+ in \`$phase\`}" "$took" "${why:-see the log}" >>"$GITHUB_STEP_SUMMARY"
	exit "$rc"
}

if [ -n "$rollback" ]; then
	what="$app rollback to $rollback"
	[ -n "$actions" ] && echo "::group::rolling $url back to $rollback" || echo "rolling $url back to $rollback"
	rc=0
	curl --fail-with-body --silent --show-error --max-time 600 -X POST \
		-H "Authorization: Bearer $token" \
		-o "$body" "$url?rollback=$rollback" || rc=$?
	finish "$rc" "$what"
	exit 0
fi

# Never print a URL's query string: that is where presigned-URL
# credentials live (the box redacts it from its logs for the same
# reason), and Actions output is readable by anyone who can see the job.
what="$app $version"
[ -n "$actions" ] && echo "::group::deploying ${artifact%%\?*} as $version to $url" || echo "deploying ${artifact%%\?*} as $version to $url"
rc=0
if [ -f "$artifact" ]; then
	curl --fail-with-body --silent --show-error --max-time 600 -X POST \
		-H "Authorization: Bearer $token" \
		-H "Content-Type: application/gzip" --data-binary @"$artifact" \
		-o "$body" "$url?version=$version" || rc=$?
else
	# JSON-escape what goes into the body (a quote or backslash in a
	# header value would otherwise make it malformed).
	json() { printf '%s' "$1" | sed 's/[\\"]/\\&/g'; }
	auth=${ARTIFACT_AUTH_HEADER:+,\"auth_header\":\"$(json "$ARTIFACT_AUTH_HEADER")\"}
	curl --fail-with-body --silent --show-error --max-time 600 -X POST \
		-H "Authorization: Bearer $token" \
		-H "Content-Type: application/json" \
		-d "{\"url\":\"$(json "$artifact")\",\"version\":\"$(json "$version")\"$auth}" \
		-o "$body" "$url" || rc=$?
fi
finish "$rc" "$what"
