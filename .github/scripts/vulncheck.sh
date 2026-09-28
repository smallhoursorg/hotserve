#!/bin/sh
# govulncheck over each module given as an argument (run from the repo
# root). Fails on any called vulnerability that .github/pin-watch.yml
# does not accept with a `govulncheck:` ID, and on any accepted ID no
# module reports any more — a stale acceptance is deleted, not kept.
set -u
root=$(pwd)
accepted=$(awk '$1 == "govulncheck:" {print $2}' "$root/.github/pin-watch.yml")
out=$(mktemp)
seen=$(mktemp)
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
		if echo "$accepted" | grep -qx "$id"; then
			echo "vulncheck: $m: $id accepted in .github/pin-watch.yml"
		else
			echo "vulncheck: $m: $id is not accepted in .github/pin-watch.yml" >&2
			fail=1
		fi
	done
done

for id in $accepted; do
	if ! grep -qx "$id" "$seen"; then
		echo "vulncheck: $id is no longer reported: delete its entry from .github/pin-watch.yml" >&2
		fail=1
	fi
done
exit $fail
