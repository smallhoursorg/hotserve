#!/bin/sh
# The box channel end to end (box/DESIGN-box.md), inside the e2e box:
# the e2e-hotserve container, systemd as PID 1, hotserve.service as the
# hotserve user, and the packaged hotserve-box-apply.path and .service
# with the tmpfiles exchange tree. Commits are made and SSH-signed by
# git here, bundled the way the template's workflow bundles them, and
# POSTed to box_webhook with a `deploy_trust local` token naming the
# commit; the result is polled as the workflow polls it. Every answer
# is the real handler's and every install the real applier's — nothing
# is stubbed.
#
# The box's file is the e2e config itself, made into one a box applies:
# imports inlined (a signed file has none), a `box` block, and a
# box_webhook site on a bare hostname (served with an internal-CA
# certificate on 8443: the address rules allow no scheme or port), at
# <host>/Caddyfile in the repository. Its baseline is set by `hotserve
# init`, run inside that directory as the operator runs it.
#
# Runs last in `make e2e` (`docker compose exec`); puts the e2e config
# back at the end.
set -u
. /lib.sh

HOST=box.e2e.test
URL=https://$HOST:8443/
BOX=/var/lib/hotserve-box
LIVE=/etc/hotserve/Caddyfile
FILE=$HOST/Caddyfile

work=$(mktemp -d)
repo=$work/repo
trap 'rm -rf "$work"' EXIT
cp "$LIVE" "$work/original"

# ── the client: what the workflow does, in shell ───────────────────

newid() { od -An -N16 -tx1 /dev/urandom | tr -d ' \n'; }

# tok [<sha>]: a fresh box token, naming the commit when given one.
tok() {
	if [ -n "${1:-}" ]; then
		hotserve deploy-token --key /shared/deploy.key --audience e2e-box --ttl 5m --claims "sha=$1"
	else
		hotserve deploy-token --key /shared/deploy.key --audience e2e-box --ttl 5m
	fi
}

# req <curl args...>: one request to the box's webhook; status in
# $CODE, body in $work/resp.
req() {
	CODE=$(curl -sk --http1.1 --resolve "$HOST:8443:127.0.0.1" -o "$work/resp" -w '%{http_code}' --max-time 60 "$@")
}
resp() { cat "$work/resp"; }

# bundle <sha> <baseline> [<path>]: the bundle for <sha> in
# $work/bundle.tgz — path, Caddyfile, commit, the first-parent chain
# down to (not including) <baseline> as parents/NNNN, and the trees
# along <path>. Regular files only, named one by one, so tar writes no
# directory entries. TAMPER=1 sends other Caddyfile bytes than the
# commit's.
bundle() {
	sha=$1 base=$2 path=${3:-$FILE}
	b=$work/b
	rm -rf "$b" && mkdir -p "$b/parents" "$b/trees"
	printf '%s' "$path" >"$b/path"
	git -C "$repo" cat-file commit "$sha" >"$b/commit"
	git -C "$repo" cat-file blob "$sha:$path" >"$b/Caddyfile"
	[ "${TAMPER:-}" = 1 ] && printf '\n# not what was signed\n' >>"$b/Caddyfile"
	members="path Caddyfile commit"
	n=0
	for p in $(git -C "$repo" rev-list --first-parent "$base..$sha" | tail -n +2); do
		n=$((n + 1))
		f=parents/$(printf %04d "$n")
		git -C "$repo" cat-file commit "$p" >"$b/$f"
		members="$members $f"
	done
	t=$(git -C "$repo" rev-parse "$sha^{tree}")
	git -C "$repo" cat-file tree "$t" >"$b/trees/$t"
	members="$members trees/$t"
	dir=
	rest=$path
	while [ "${rest#*/}" != "$rest" ]; do
		dir=${dir:+$dir/}${rest%%/*}
		rest=${rest#*/}
		t=$(git -C "$repo" rev-parse "$sha:$dir")
		git -C "$repo" cat-file tree "$t" >"$b/trees/$t"
		members="$members trees/$t"
	done
	# shellcheck disable=SC2086 # one member per word
	tar -czf "$work/bundle.tgz" -C "$b" $members
}

# post <claim> [<id>]: POST the bundle with a token naming <claim>
# (empty: no sha claim); the id in $ID.
post() {
	ID=${2:-$(newid)}
	req -X POST -H "Authorization: Bearer $(tok "$1")" -H "X-Box-Request-Id: $ID" \
		-H 'Content-Type: application/gzip' --data-binary "@$work/bundle.tgz" "$URL"
}

phase() { json_str "$(resp)" phase; }

# poll: GET /?result=$ID with a fresh token each time until the phase
# is terminal; 404 is "not yet" after a POST that was not answered
# terminally (Message catalogue). Bounded at 120 s, the e2e stand-in
# for the workflow's twenty-five minutes.
poll() {
	i=0
	while [ "$i" -lt 120 ]; do
		req -H "Authorization: Bearer $(tok)" "$URL?result=$ID"
		case "$CODE:$(phase)" in
		202:* | 404:*) ;;
		*) return ;;
		esac
		i=$((i + 1))
		sleep 1
	done
}

# push <sha> [<path>]: bundle against the baseline, POST, poll to a
# terminal phase. Final status in $CODE, phase in $PHASE.
push() {
	bundle "$1" "$BASE" "${2:-$FILE}"
	post "$1"
	case $CODE in 202 | 504) poll ;; esac
	PHASE=$(phase)
}

# settled: the applier has finished its run and left nothing behind
# (I2, the empty in/ and work/; no record; no temporary beside the
# live file).
settled() {
	i=0
	while [ "$i" -lt 60 ]; do
		[ "$(systemctl is-active hotserve-box-apply.service)" != activating ] \
			&& [ -z "$(ls -A "$BOX/in")$(ls -A "$BOX/work")" ] && break
		i=$((i + 1))
		sleep 0.5
	done
	if [ -n "$(ls -A "$BOX/in")$(ls -A "$BOX/work")" ]; then
		fail "$1: left in/ or work/ non-empty: $(ls -A "$BOX/in" "$BOX/work" | tr '\n' ' ')"
	elif [ -e "$BOX/txn.json" ]; then
		fail "$1: left txn.json behind"
	elif ls /etc/hotserve/.Caddyfile.box-* >/dev/null 2>&1; then
		fail "$1: left a .Caddyfile.box-* temporary"
	fi
}

baseline() { json_str "$(cat "$BOX/applied.json")" sha; }
served() { curl -s --max-time 5 "http://localhost:$1/"; }
status() { curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://localhost:$1/"; }

# refused <name> <text>: the push was answered 422 refused, carrying
# <text>; nothing on the box moved.
refused() {
	if [ "$CODE" = 422 ] && [ "$PHASE" = refused ] && resp | grep -qF -- "$2"; then
		pass "$1: refused by name"
	else
		fail "$1: expected 422 refused with '$2', got $CODE $(resp | head -c 600)"
	fi
	cmp -s "$LIVE" "$work/live" && pass "$1: the live file is unchanged" || fail "$1: the live file changed"
	[ "$(baseline)" = "$BASE" ] && pass "$1: the baseline did not move" || fail "$1: the baseline is $(baseline), not $BASE"
	settled "$1"
}

# ── the repository: signer keys, the box's file, signed commits ────

for k in alice bob mallory; do ssh-keygen -q -t ed25519 -N '' -C "$k" -f "$work/$k"; done
signer() { printf '\t\tsigner %s@e2e.test %s\n' "$1" "$(cut -d' ' -f1,2 "$work/$1.pub")"; }

# The e2e config as a box's file: imports inlined, HTTPS for the one
# bare-hostname site (internal CA, port 8443, no redirect listener),
# the box block, and the webhook site last. `# signers` is where
# render puts the signer lines.
awk '
/^[ \t]*import [^ \t]+[ \t]*$/ { f = $2; while ((getline l < f) > 0) print l; close(f); next }
/^[ \t]*auto_https off[ \t]*$/ {
	print "\tlocal_certs"
	print "\tskip_install_trust"
	print "\tauto_https disable_redirects"
	print "\thttps_port 8443"
	print ""
	print "\tbox {"
	print "\t\tdeploy_trust local {"
	print "\t\t\tpublic_key /shared/deploy.pub"
	print "\t\t\taudience e2e-box"
	print "\t\t}"
	print "\t\t# signers"
	print "\t}"
	next
}
{ print }
END { print ""; print "'"$HOST"' {"; print "\tbox_webhook"; print "}" }
' "$work/original" >"$work/template"

# render <signer lines> [<extra text>]: the box's file.
render() {
	S=$1 awk '{ print } $0 == "\t\t# signers" && ENVIRON["S"] != "" { print ENVIRON["S"] }' "$work/template"
	printf '%s' "${2:-}"
}

git init -q -b main "$repo"
git -C "$repo" config user.name e2e
git -C "$repo" config user.email e2e@e2e.test
git -C "$repo" config gpg.format ssh

# commit <alice|bob|mallory|none> <file> [<path>]: commit <file> at
# <path> on top of $AT (detached), signed by the key named; prints the
# commit's id.
commit() {
	path=${3:-$FILE}
	git -C "$repo" checkout -q --detach "$AT" 2>/dev/null || true
	mkdir -p "$repo/$(dirname "$path")"
	cp "$2" "$repo/$path"
	git -C "$repo" add -A
	if [ "$1" = none ]; then
		git -C "$repo" commit -q --allow-empty --no-gpg-sign -m "unsigned"
	else
		git -C "$repo" -c user.signingkey="$work/$1" commit -q --allow-empty -S -m "by $1"
	fi
	git -C "$repo" rev-parse HEAD
}

ALICE=$(signer alice)
BOB=$(signer bob)

# The root commit: the box's file before any push.
render "$ALICE" >"$work/f0"
mkdir -p "$repo/$HOST" && cp "$work/f0" "$repo/$FILE"
git -C "$repo" add -A
git -C "$repo" -c user.signingkey="$work/alice" commit -q -S -m "$HOST: the first file"
C0=$(git -C "$repo" rev-parse HEAD)

# install_baseline <sha>: `hotserve init <sha>`, as root inside the
# box's directory copied from the commit (git archive, as init's help
# copies it), on a box where hotserve runs; its output in $work/init.out.
install_baseline() {
	rm -rf "$work/init" && mkdir -p "$work/init" \
		&& git -C "$repo" archive "$1" "$HOST" | tar -xf - -C "$work/init" \
		&& (cd "$work/init/$HOST" && hotserve init "$1") >"$work/init.out" 2>&1
}

# restore: the e2e config back, and no baseline.
restore() {
	cp "$work/original" "$LIVE.e2e-tmp" && chmod 0644 "$LIVE.e2e-tmp" && mv "$LIVE.e2e-tmp" "$LIVE" \
		&& rm -f "$BOX/applied.json" \
		&& { systemctl reload hotserve || systemctl restart hotserve; }
}

# ready: GET / answers 200 (the internal certificate is issued
# asynchronously after the reload that first names the host).
ready() {
	i=0
	while [ "$i" -lt 120 ]; do
		req -H "Authorization: Bearer $(tok)" "$URL"
		[ "$CODE" = 200 ] && return 0
		i=$((i + 1))
		sleep 0.5
	done
	return 1
}

echo "=== box apply 0: the units are in place and the box answers ==="
[ "$(systemctl is-active hotserve-box-apply.path)" = active ] \
	&& pass "hotserve-box-apply.path is active" || fail "the path unit is $(systemctl is-active hotserve-box-apply.path)"
[ "$(stat -c '%a %U %G' "$BOX" "$BOX/in" "$BOX/out" | tr '\n' ' ')" = "2750 root hotserve 770 root hotserve 2750 root hotserve " ] \
	&& pass "the exchange tree has the Paths table's modes" || fail "exchange tree: $(stat -c '%n %a %U %G' "$BOX" "$BOX/in" "$BOX/out" | tr '\n' ' ')"
if ! install_baseline "$C0"; then
	fail "hotserve init failed: $(cat "$work/init.out"); $(journalctl -u hotserve --no-pager -n 5 | grep -o '"msg":"[^"]*"' | tail -1)"
	restore
	exit 1
fi
grep -qx "init: reloaded. From here, this box takes config only as signed commits sent by your repository's workflow." "$work/init.out" \
	&& pass "hotserve init installed the baseline and reloaded" || fail "hotserve init printed: $(cat "$work/init.out")"
[ "$(json_str "$(cat "$BOX/applied.json")" path)" = "$FILE" ] && [ "$(json_str "$(cat "$BOX/applied.json")" signer)" = init ] \
	&& pass "applied.json records $FILE, derived from the host, signed by init" || fail "applied.json: $(cat "$BOX/applied.json")"
BASE=$C0
if ready; then
	pass "GET / answers once the baseline is installed"
else
	fail "GET / never answered 200: $CODE $(resp | head -c 300)"
fi
[ "$(json_str "$(resp)" commit)" = "$C0" ] && pass "GET / names the baseline commit" || fail "GET /: $(resp)"
[ "$(json_str "$(resp)" sha256)" = "$(sha256sum "$LIVE" | cut -d' ' -f1)" ] \
	&& pass "GET / carries the installed file's digest" || fail "GET / digest: $(resp)"
req "$URL"
[ "$CODE" = 401 ] && pass "GET / without a token is 401" || fail "GET / without a token: $CODE"

echo "=== box apply 1: a signed descendant is verified, applied, and polled to the end ==="
AT=$C0
render "$ALICE" "$(printf '\n:8180 {\n\trespond "v1"\n}\n')" >"$work/f1"
C1=$(commit alice "$work/f1")
before=$(status 8080)
bundle "$C1" "$BASE"
post "$C1"
C1_ID=$ID
# Step 8: the POST answers by the first phase the handler sees —
# `verified` (202) unless the applier was already done within its
# first poll interval (200 applied).
case "$CODE:$(phase)" in
202:verified | 200:applied) pass "the POST answers $CODE $(phase)" ;;
*) fail "the POST answered $CODE $(resp | head -c 300)" ;;
esac
[ "$CODE" = 202 ] && poll
PHASE=$(phase)
[ "$PHASE" = applied ] && pass "the push ends applied" || fail "push of a signed descendant: $CODE $(resp | head -c 600)"
cmp -s "$LIVE" "$work/f1" && pass "the live file is the pushed one" || fail "the live file differs from the pushed one"
[ "$(served 8180)" = v1 ] && pass "the pushed config is running" || fail "the pushed site does not answer: '$(served 8180)'"
[ "$(baseline)" = "$C1" ] && pass "the baseline advanced to the pushed commit" || fail "the baseline is $(baseline)"
for app in demo deno-example node-example; do
	resp | grep -q "\"$app\"" || fail "the result does not list app $app: $(resp | head -c 300)"
done
resp | grep -q '"caddyfile_edited_out_of_band":false' && pass "the result lists the apps, no out-of-band edit" || fail "result: $(resp | head -c 300)"
[ "$(status 8080)" = "$before" ] && pass "the demo app answers as it did before the apply ($before)" || fail "the demo app answered $before, now $(status 8080)"
settled "apply"
BASE=$C1
cp "$LIVE" "$work/live"

echo "=== box apply 2: the same commit again is no_change, answered at once ==="
bundle "$C1" "$C1"
post "$C1"
[ "$CODE" = 200 ] && [ "$(phase)" = no_change ] && pass "a repeat is 200 no_change" || fail "repeat: $CODE $(resp)"
req -H "Authorization: Bearer $(tok)" "$URL?result=$ID"
[ "$CODE" = 404 ] && pass "the fast path writes nothing: its id polls 404" || fail "poll after the fast path: $CODE $(resp)"

echo "=== box apply 3: the handler's refusals ==="
# An id the box holds a result for is a retry after a lost response:
# told to poll, whatever its bundle.
post "$C1" "$C1_ID"
[ "$CODE" = 409 ] && resp | grep -qF "duplicate request id" && pass "a known id is 409 duplicate" || fail "known id: $CODE $(resp)"
post "$C1" "not-an-id"
[ "$CODE" = 400 ] && pass "a malformed request id is 400" || fail "bad id: $CODE $(resp)"
req -X POST -H "X-Box-Request-Id: $(newid)" -H 'Content-Type: application/gzip' --data-binary "@$work/bundle.tgz" "$URL"
[ "$CODE" = 401 ] && pass "a push without a token is 401" || fail "no token: $CODE $(resp)"

AT=$C1
render "$ALICE" "$(printf '\n:8180 {\n\trespond "v2"\n}\n')" >"$work/f2"
C2=$(commit alice "$work/f2")
bundle "$C2" "$BASE"
post "$C1"
PHASE=$(phase)
refused "a token naming another commit" "the token names commit $C1; the bundle is $C2"
post ""
PHASE=$(phase)
refused "a token with no sha claim" "box tokens must name the commit (sha claim)"
TAMPER=1 bundle "$C2" "$BASE"
post "$C2"
PHASE=$(phase)
refused "a file other than the committed one" "the file sent is not $FILE in $C2"

U=$(commit none "$work/f2")
push "$U"
refused "an unsigned commit" "$U is not signed"

M=$(commit mallory "$work/f2")
push "$M"
refused "a commit by an unlisted key" "$M is signed by a key that is not a signer"

AT=$U
render "$ALICE" "$(printf '\n:8180 {\n\trespond "v3"\n}\n')" >"$work/f3"
S=$(commit alice "$work/f3")
push "$S"
refused "an unsigned commit under a signed one" "$U, between the commit this box runs and $S, is not signed"

push "$C0"
refused "a replay of an older commit" "$C0 does not descend from the commit this box runs ($C1)"

AT=$C0
SIB=$(commit alice "$work/f2")
push "$SIB"
refused "a commit off another branch" "$SIB does not descend from the commit this box runs ($C1)"

AT=$C1
C2B=$(commit alice "$work/f2" box2/Caddyfile)
push "$C2B" box2/Caddyfile
refused "another box's file" "this box's file is $FILE; the bundle is box2/Caddyfile"

sed "s/^$HOST {/other.e2e.test {/" "$work/f2" >"$work/fhost"
H=$(commit alice "$work/fhost")
push "$H"
refused "a file for another host" "this file is for other.e2e.test; this box is $HOST"

render "" >"$work/fnosigner"
N=$(commit alice "$work/fnosigner")
push "$N"
refused "a file with no signer" "the new Caddyfile has no signer"

render "$BOB" >"$work/fbob"
D=$(commit alice "$work/fbob")
push "$D"
refused "a file that drops the key that signed it" "drops the key that signed this commit (alice@e2e.test)"

grep -v '^[[:space:]]*box_webhook$' "$work/f2" >"$work/fnohook"
W=$(commit alice "$work/fnohook")
push "$W"
refused "a file with no box_webhook" "the new Caddyfile has no site with box_webhook"

render "$ALICE" "$(printf '\n:8181 {\n\tno_such_directive\n}\n')" >"$work/fbad"
X=$(commit alice "$work/fbad")
push "$X"
refused "a config hotserve validate rejects" "hotserve validate:"

echo "=== box apply 4: a config that validates but does not load is rolled back ==="
# Validation binds no listener; loading does, and 192.0.2.1 (TEST-NET-1)
# is on no interface here.
render "$ALICE" "$(printf '\n:8180 {\n\trespond "v1"\n}\n\nhttp://192.0.2.1:8182 {\n\tbind 192.0.2.1\n\trespond "unreachable"\n}\n')" >"$work/funload"
R=$(commit alice "$work/funload")
push "$R"
[ "$PHASE" = rolled_back ] && resp | grep -qF "the previous Caddyfile is back and running" \
	&& pass "a failed reload is rolled_back" || fail "unloadable: $CODE $(resp | head -c 600)"
cmp -s "$LIVE" "$work/live" && pass "the live file is the running config again" || fail "the live file is not the previous one"
[ "$(served 8180)" = v1 ] && pass "the previous config kept serving" || fail "after the rollback: '$(served 8180)'"
[ "$(baseline)" = "$C1" ] && pass "the baseline did not move" || fail "the baseline is $(baseline)"
settled "rollback"

echo "=== box apply 5: a console edit is reported, and the next push reinstalls the commit ==="
printf '\n# edited at the console\n' >>"$LIVE"
systemctl reload hotserve
bundle "$C1" "$C1"
post "$C1"
case $CODE in 202 | 504) poll ;; esac
PHASE=$(phase)
[ "$PHASE" = applied ] && resp | grep -q '"caddyfile_edited_out_of_band":true' \
	&& pass "a replay after a console edit applies, reporting the edit" || fail "replay after an edit: $CODE $(resp | head -c 600)"
cmp -s "$LIVE" "$work/f1" && pass "the live file is the commit's again" || fail "the console edit survived"
settled "out-of-band"

echo "=== box apply 6: with hotserve stopped, the applier refuses and changes nothing ==="
# Nothing can POST with hotserve down, so the bundle is dropped as the
# handler drops it: assembled in stage/ by the hotserve uid, renamed
# into in/.
AT=$C1
C3=$(commit alice "$work/f2")
bundle "$C3" "$C1"
systemctl stop hotserve
ID=$(newid)
install -o hotserve -g hotserve -m 0644 "$work/bundle.tgz" "$BOX/stage/.e2e-$ID"
mv "$BOX/stage/.e2e-$ID" "$BOX/in/$ID.tar"
i=0
while [ ! -s "$BOX/out/$ID.json" ] && [ "$i" -lt 60 ]; do
	i=$((i + 1))
	sleep 0.5
done
[ "$(json_str "$(cat "$BOX/out/$ID.json" 2>/dev/null)" phase)" = refused ] && grep -qF "hotserve is not running; nothing applied" "$BOX/out/$ID.json" \
	&& pass "the applier refuses while hotserve is stopped" || fail "with hotserve stopped: $(cat "$BOX/out/$ID.json" 2>/dev/null)"
cmp -s "$LIVE" "$work/f1" && pass "the live file is unchanged" || fail "the live file changed with hotserve stopped"
settled "stopped"
systemctl start hotserve
ready && pass "hotserve is back" || fail "hotserve did not come back: $CODE"

echo "=== box apply 7: the template's bin/apply, unchanged, against this box ==="
# examples/box/bin/apply is what a box repository's workflow runs. It
# posts to the address the committed file names, https://$HOST/ on
# 443; curl's own config file routes that to 8443 and takes the
# internal CA's certificate (the script has no test hooks). The
# laptop's token: one, naming the commit, via HOTSERVE_TOKEN.
mkdir -p "$repo/bin" "$work/curl"
cp /template-apply "$repo/bin/apply"
printf 'connect-to = %s:443:127.0.0.1:8443\ninsecure\nhttp1.1\n' "$HOST" >"$work/curl/.curlrc"
template_apply() { # <sha> → its output in $work/tout, exit status returned
	CURL_HOME=$work/curl HOTSERVE_TOKEN=$(tok "$1") "$repo/bin/apply" "$HOST" >"$work/tout" 2>&1
}
AT=$C1
C4=$(commit alice "$work/f2")
if template_apply "$C4" && grep -qF "loaded; running apps are not restarted" "$work/tout"; then
	pass "bin/apply applied a signed commit"
else
	fail "bin/apply: $(cat "$work/tout")"
fi
[ "$(baseline)" = "$C4" ] && pass "the baseline is bin/apply's commit" || fail "the baseline is $(baseline), not $C4"
cmp -s "$LIVE" "$work/f2" && pass "the live file is the commit's" || fail "the live file is not the commit's"
settled "template apply"
template_apply "$C4" && grep -qF "the box already runs this Caddyfile" "$work/tout" \
	&& pass "bin/apply again: no_change" || fail "bin/apply again: $(cat "$work/tout")"
# An unsigned commit under a signed one: refused, naming the commit to
# rebase out — the real handler's words, unmasked and unescaped.
AT=$C4
C5=$(commit none "$work/f1")
AT=$C5
C6=$(commit alice "$work/f2")
if template_apply "$C6"; then
	fail "bin/apply exited 0 for an unsigned commit under a signed one"
elif grep -qF "$C5, between the commit this box runs and $C6, is not signed" "$work/tout"; then
	pass "bin/apply reports the unsigned commit by name"
else
	fail "bin/apply on an unsigned commit: $(cat "$work/tout")"
fi
[ "$(baseline)" = "$C4" ] && pass "the baseline did not move" || fail "the baseline is $(baseline)"
settled "template refused"

echo "=== box apply 8: the path unit never spun ==="
[ "$(systemctl is-active hotserve-box-apply.path)" = active ] \
	&& pass "the path unit is still active" || fail "the path unit is $(systemctl is-active hotserve-box-apply.path)"
if journalctl -u hotserve-box-apply.path -u hotserve-box-apply.service --no-pager | grep -qiE 'trigger limit|start request repeated|start-limit'; then
	fail "the journal has a trigger or start limit line"
else
	pass "no trigger or start limit was hit"
fi

echo "=== box apply 9: put the e2e config back ==="
restore && pass "restored the original config" || fail "could not load the original config"

echo ""
if [ "$FAILURES" -eq 0 ]; then
	echo "ALL BOX APPLY SCENARIOS PASSED"
	exit 0
fi
echo "$FAILURES BOX APPLY ASSERTION(S) FAILED"
exit 1
