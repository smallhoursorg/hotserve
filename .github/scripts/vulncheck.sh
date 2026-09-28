#!/bin/sh
# govulncheck over each module given as an argument (run from the repo
# root). Fails on any called vulnerability that an alert_dismissals
# entry in .github/pin-watch.yml does not accept with a `govulncheck:`
# ID, and on any accepted ID none of the scanned modules reports any
# more — a stale acceptance is removed, not kept.
set -u
root=$(pwd)
manifest=$root/.github/pin-watch.yml
# `govulncheck:` keys inside the top-level alert_dismissals section
# only; tolerates a leading "- " and quoting.
accepted=$(awk '
	/^[^ #]/ { in_sec = ($0 ~ /^alert_dismissals:/) }
	in_sec {
		line = $0
		sub(/^[ \t]*(- )?/, "", line)
		if (line ~ /^govulncheck:/) {
			sub(/^govulncheck:[ \t]*/, "", line)
			sub(/[ \t]*#.*$/, "", line)
			gsub(/["\047]/, "", line)
			print line
		}
	}' "$manifest")
out=$(mktemp) || exit 1
seen=$(mktemp) || exit 1
trap 'rm -f "$out" "$seen"' EXIT
fail=0

for m in "$@"; do
	echo "== govulncheck $m"
	(cd "$root/$m" && go tool govulncheck ./...) >"$out" 2>&1
	rc=$?
	cat "$out"
	case $rc in
	0) continue ;;
	3) ;; # vulnerabilities found: check them below
	*) exit "$rc" ;;
	esac
	# Called vulnerabilities are the ones under "=== Symbol Results ===".
	ids=$(awk '/^=== /{sym = ($0 == "=== Symbol Results ===")} sym && /^Vulnerability #/{print $3}' "$out")
	if [ -z "$ids" ]; then
		# Exit 3 with nothing parsed: the output format changed.
		echo "vulncheck: $m: govulncheck found vulnerabilities but none parsed; update $0" >&2
		fail=1
	fi
	for id in $ids; do
		echo "$id" >>"$seen"
		if printf '%s\n' "$accepted" | grep -Fqx "$id"; then
			echo "vulncheck: $m: $id accepted in .github/pin-watch.yml"
		else
			echo "vulncheck: $m: $id is not accepted in .github/pin-watch.yml" >&2
			fail=1
		fi
	done
done

# Not called any more (fix landed, or the vulndb entry was reviewed and
# names symbols we never reach): the govulncheck acceptance is stale.
# The rest of the entry — the Dependabot dismissal — pin-watch keeps
# judging on its own.
for id in $accepted; do
	if ! grep -Fqx "$id" "$seen"; then
		echo "vulncheck: $id is not called in any scanned module ($*): remove its govulncheck: key from .github/pin-watch.yml" >&2
		fail=1
	fi
done
exit $fail
