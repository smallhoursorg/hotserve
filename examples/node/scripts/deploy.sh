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
# The deploy (or rollback: the same start, health gate and cutover,
# from a release already on disk) is streamed as it happens: one JSON
# line per phase, then the outcome — the app's status when the new
# version is live, or an error saying why it was refused (the old
# version keeps serving) — with the status code the request would
# have had in `http_status`. Every line is printed as it arrives, and
# a failure exits non-zero. A 401 — the box would not accept the
# token — is followed by what the box must trust for this run, since
# the box's own answer deliberately does not say (the reason is in its
# journal, for its operator).
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
# The app is the URL's last path segment; the box accepts a trailing
# slash there, so drop any before taking it.
app=$url
while [ "${app%/}" != "$app" ]; do app=${app%/}; done
app=${app##*/}
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
	printf '::add-mask::%s\n' "$token"
	minted=1
else
	token=${HOTSERVE_TOKEN:?set HOTSERVE_TOKEN (mint one with: hotserve deploy-token) or run in GitHub Actions with id-token: write}
	minted=
fi

# The Actions dressing. `field` reads one string field out of the
# response (no jq here: this also runs from laptops): the last
# occurrence, which for "phase" is the failing phase inside last_deploy
# and for "error" its cause, with JSON's \" and \\ unescaped so a quote
# in the cause does not cut it short. `prop` and `msg` escape what a
# workflow command's property and message may not contain. The
# commands are written with printf, never echo: dash's echo turns a
# JSON-escaped \n in a cause into a real newline and splits the line.
actions=${GITHUB_ACTIONS:-}
field() {
	printf '%s' "$1" | sed -n 's/.*"'"$2"'":"\(\([^\\"]*\\.\)*[^\\"]*\)".*/\1/p' | sed 's/\\\(["\\]\)/\1/g'
}
msg() { printf '%s' "$1" | sed 's/%/%25/g' | tr '\r\n' '  '; }
prop() { msg "$1" | sed 's/:/%3A/g; s/,/%2C/g'; }
began=$(date +%s)
# One temp directory, trapped before anything is put in it.
tmpd=$(mktemp -d)
trap 'rm -rf "$tmpd"' EXIT
body=$tmpd/body
# The stream: curl prints each line as it arrives and tee keeps a
# copy (curl's own errors go to stderr, printed but kept out of the
# copy); the response headers and curl's own exit status are kept in
# files of their own — a pipeline's status is tee's. The outcome is
# the last line's http_status — a stream is 200 from its first byte,
# so the status line says nothing. A body with no such line and no
# phase line did not stream: a box without stream support answered
# the single response, and when curl brought it whole and its status
# line is the 200 a completed deploy answers, that is the outcome: a
# 3xx from an intermediary is not a deploy, and neither is any other
# 2xx (a 202 from a queue in front of the box, say). A phase line
# with no terminal line is a stream cut short, and a single response
# curl could not finish is no outcome: failures both.
hdrs=$tmpd/headers
rcfile=$tmpd/curl-status
stream() { # <curl args...>: runs the request, prints it, keeps it
	# `|| rc=$?` keeps a failing curl from ending the group under
	# set -e before its status is written.
	{
		rc=0
		curl --fail-with-body --silent --show-error --no-buffer --max-time 600 \
			-H "Authorization: Bearer $token" -H "Accept: application/x-ndjson" \
			-D "$hdrs" "$@" || rc=$?
		echo "$rc" >"$rcfile"
	} | tee "$body"
}
httpstatus() { sed -n 's/^HTTP\/[0-9.]* \([0-9][0-9][0-9]\).*/\1/p' "$hdrs" 2>/dev/null | tail -n 1; }
outcome() { # the http_status of a complete last line; else 200 for a whole single 200; else 0
	# The whole terminal suffix, brace included: a connection cut after
	# the digits must not read as an outcome.
	code=$(tail -n 1 "$body" | sed -n 's/.*,"event":"done","http_status":\([0-9][0-9]*\)}$/\1/p')
	if [ -n "$code" ]; then echo "$code"
	elif grep -q '"event":"phase"' "$body"; then echo 0
	elif [ "$(cat "$rcfile" 2>/dev/null)" != 0 ]; then echo 0
	elif [ "$(httpstatus)" = 200 ]; then echo 200
	else echo 0
	fi
}
# refused says what the box must trust for a token it turned away.
# The box's 401 is the same flat sentence whatever the reason — an
# answer naming the check that failed would tell anyone who can mint a
# token (every GitHub repository can) which apps exist and what each
# pins — so this side is built from what the run already has, and the
# reason itself is in the box's journal. Most often, not always: the
# journal also holds the case where the box could not reach the
# issuer to check the token at all.
refused() {
	echo "the box refused this token. Most often its deploy_trust for this app does not accept:"
	if [ -n "$minted" ]; then
		printf '  %-18s%s\n' 'audience' "${HOTSERVE_AUDIENCE:-hotserve}" 'claim repository' "${GITHUB_REPOSITORY:-?}" 'claim ref' "${GITHUB_REF:-?}"
	else
		echo "  the key this token was minted with (deploy_trust local { public_key ... })"
		echo "  the audience, subject or claim it pins, against what hotserve deploy-token was given"
	fi
	echo "Check the app's deploy_trust block in the box's Caddyfile. The box's journal"
	echo "(journalctl -u hotserve, 'webhook auth failed') names the check that refused it,"
	echo "unless the box's budget for logging failed authentications is spent."
}
finish() { # <what>: dresses the outcome, exits on failure
	what=$1
	echo
	[ -n "$actions" ] && printf '::endgroup::\n'
	took="$(( $(date +%s) - began ))s"
	code=$(outcome)
	case $code in
	2??)
		[ -n "${GITHUB_STEP_SUMMARY:-}" ] && printf '**hotserve:** %s live, %s\n\n' "$what" "$took" >>"$GITHUB_STEP_SUMMARY"
		return 0 ;;
	esac
	last=$(tail -n 1 "$body")
	phase=$(field "$last" phase)
	why=$(field "$last" error)
	# Only the box's own 401, by its sentence: a 401 from something in
	# front of it (a wrong HOTSERVE_URL, say) is not about deploy_trust.
	hint=
	if [ "$(httpstatus)" = 401 ]; then
		case $why in 'invalid or missing deploy token'*) hint=$(refused) ;; esac
	fi
	[ -z "$hint" ] || printf '%s\n' "$hint"
	[ -n "$actions" ] && printf '::error title=%s::%s\n' "$(prop "hotserve: $what failed")" "$(msg "${phase:+in $phase: }${why:-see the response above}${hint:+; the log says what the box must trust}")"
	if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
		printf '**hotserve:** %s failed%s, %s\n\n%s\n\n' "$what" "${phase:+ in \`$phase\`}" "$took" "${why:-see the log}" >>"$GITHUB_STEP_SUMMARY"
		# Tildes: a git ref or repository name cannot hold one, so the
		# run's own values cannot end the fence early.
		[ -z "$hint" ] || printf '~~~\n%s\n~~~\n\n' "$hint" >>"$GITHUB_STEP_SUMMARY"
	fi
	exit 1
}

if [ -n "$rollback" ]; then
	what="$app rollback to $rollback"
	printf '%srolling %s back to %s\n' "${actions:+::group::}" "$url" "$rollback"
	stream -X POST "$url?rollback=$rollback"
	finish "$what"
	exit 0
fi

# Never print a URL's query string: that is where presigned-URL
# credentials live (the box redacts it from its logs for the same
# reason), and Actions output is readable by anyone who can see the job.
what="$app $version"
printf '%sdeploying %s as %s to %s\n' "${actions:+::group::}" "${artifact%%\?*}" "$version" "$url"
if [ -f "$artifact" ]; then
	stream -X POST -H "Content-Type: application/gzip" --data-binary @"$artifact" "$url?version=$version"
else
	# JSON-escape what goes into the body (a quote or backslash in a
	# header value would otherwise make it malformed).
	json() { printf '%s' "$1" | sed 's/[\\"]/\\&/g'; }
	auth=${ARTIFACT_AUTH_HEADER:+,\"auth_header\":\"$(json "$ARTIFACT_AUTH_HEADER")\"}
	stream -X POST -H "Content-Type: application/json" \
		-d "{\"url\":\"$(json "$artifact")\",\"version\":\"$(json "$version")\"$auth}" "$url"
fi
finish "$what"
