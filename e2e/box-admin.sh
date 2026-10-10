#!/bin/sh
# examples/box's administrator on the e2e box (the e2e-hotserve
# container, reached with `docker compose exec` instead of ssh): a user
# who is not root, holding only the example's sudoers file and the adm
# group (e2e/Dockerfile). They read the journals, create and edit the
# apps' env files, and nothing else — the Caddyfile included, which
# changes only by a signed push. Also validates the example's
# Caddyfile with the binary under test. Runs on the host, last in
# `make e2e`; it changes nothing it does not put back.
set -u
COMPOSE=${COMPOSE:-docker compose}
. "$(dirname "$0")/lib.sh"

box() { $COMPOSE exec -T e2e-hotserve "$@"; }
admin() { $COMPOSE exec -T -u admin e2e-hotserve "$@"; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "=== box 0: the admin user has what the README says, and no more ==="
# journalctl exits 0 on an empty (or invisible) journal too, so look
# for known lines, not just a clean exit.
admin journalctl -u hotserve --no-pager >"$tmp/out" 2>&1 && grep -q '"liveswap started"' "$tmp/out" \
	&& pass "adm reads hotserve's journal without privilege" || fail "admin cannot read the journal: $(tail -3 "$tmp/out")"
admin journalctl -t hotserve-demo --no-pager >"$tmp/out" 2>&1 && grep -q 'hotserve-demo' "$tmp/out" \
	&& pass "adm reads an app's journal" || fail "admin cannot read the app journal: $(tail -3 "$tmp/out")"
if admin sudo -n systemctl restart hotserve >"$tmp/out" 2>&1; then
	fail "the sudoers file let admin restart hotserve"
else
	pass "a command outside the sudoers file is refused"
fi
if admin ls /var/lib/hotserve >/dev/null 2>&1; then
	fail "admin can read hotserve's data dir"
else
	pass "admin cannot read hotserve's data dir: it is not the hotserve user"
fi
# The env-file grant: the one install line the sudoers file admits
# creates an empty 0640 root:hotserve file under /etc/hotserve, and
# nothing that differs from it by a path, a mode, an owner, a group, a
# source or an extra argument. A refusal must be sudo's own, not
# install failing after sudo let it through: a two-destination form,
# say, would fail in install either way. With -n, a command no
# NOPASSWD line matches is "a password is required"; a user with no
# line at all gets "is not allowed to execute". Either is sudo
# stopping before anything ran; install's own errors say "install:".
box rm -f /etc/hotserve/e2e-secrets.env
if admin sudo -n /usr/bin/install -m 0640 -o root -g hotserve -T /dev/null /etc/hotserve/e2e-secrets.env >"$tmp/out" 2>&1; then
	pass "admin can create an app's env file"
	[ "$(box stat -c '%a %U %G %s' /etc/hotserve/e2e-secrets.env)" = "640 root hotserve 0" ] \
		&& pass "the env file is empty, 0640 root:hotserve" || fail "env file is $(box stat -c '%a %U %G %s' /etc/hotserve/e2e-secrets.env)"
else
	fail "admin cannot create an env file: $(tail -3 "$tmp/out")"
fi
box rm -f /etc/hotserve/e2e-secrets.env
for args in \
	"-m 0640 -o root -g hotserve -T /dev/null /tmp/e2e-secrets.env" \
	"-m 0640 -o root -g hotserve -T /dev/null /etc/hotserve/../e2e-secrets.env" \
	"-m 0644 -o root -g hotserve -T /dev/null /etc/hotserve/e2e-secrets.env" \
	"-m 0640 -o admin -g hotserve -T /dev/null /etc/hotserve/e2e-secrets.env" \
	"-m 0640 -o root -g root -T /dev/null /etc/hotserve/e2e-secrets.env" \
	"-m 0640 -o root -g admin -T /dev/null /etc/hotserve/e2e-secrets.env" \
	"-m 0640 -o root -g hotserve -T /etc/shadow /etc/hotserve/e2e-secrets.env" \
	"-m 0640 -o root -g hotserve -T /dev/null /etc/hotserve/e2e-secrets.env -v" \
	"-m 0640 -o root -g hotserve /dev/null /etc/hotserve/e2e-secrets.env" \
	"-m 0640 -o root -g hotserve -T /dev/null /etc/hotserve/e2e-secrets.env /etc/hotserve/other.env"; do
	# shellcheck disable=SC2086 # the args are meant to split
	if admin sudo -n /usr/bin/install $args >"$tmp/out" 2>&1; then
		fail "the sudoers file let admin run: install $args"
	elif grep -q -E "a password is required|is not allowed to execute" "$tmp/out"; then
		pass "sudo refused: install $args"
	else
		fail "install $args failed, but not because sudo refused it: $(tail -1 "$tmp/out")"
	fi
done
box rm -f /etc/hotserve/e2e-secrets.env /etc/hotserve/other.env /tmp/e2e-secrets.env /etc/e2e-secrets.env
# -T is why a directory of that name (only root could make one) is an
# install error, not a root-owned file called "null" inside it.
box mkdir -p /etc/hotserve/e2e-dir.env
if admin sudo -n /usr/bin/install -m 0640 -o root -g hotserve -T /dev/null /etc/hotserve/e2e-dir.env >"$tmp/out" 2>&1; then
	fail "install replaced a directory named like an env file"
else
	box test ! -e /etc/hotserve/e2e-dir.env/null && grep -q '^install:' "$tmp/out" \
		&& pass "a directory named like an env file is an install error, and gains no file" \
		|| fail "against a directory: $(tail -1 "$tmp/out"); null exists: $(box test -e /etc/hotserve/e2e-dir.env/null && echo yes || echo no)"
fi
box rm -rf /etc/hotserve/e2e-dir.env
# The Caddyfile is not the administrator's: the template's sudoers
# file held bin/push's lines until the box took signed pushes. `sudo -l
# <command>` asks the policy and runs nothing, so a regression cannot
# empty the e2e box's config; it exits 0 only for an allowed command.
for cmd in "/usr/bin/tee /etc/hotserve/Caddyfile.new" "/usr/bin/mv -f /etc/hotserve/Caddyfile.new /etc/hotserve/Caddyfile" \
	"sudoedit /etc/hotserve/Caddyfile" "/usr/bin/systemctl reload hotserve"; do
	# shellcheck disable=SC2086 # the command is meant to split
	if admin sudo -n -l $cmd </dev/null >"$tmp/out" 2>&1; then
		fail "the sudoers file lets admin run: $cmd"
	else
		pass "sudo's policy refuses: $cmd"
	fi
done
# The control: the policy does admit the env-file line.
admin sudo -n -l /usr/bin/install -m 0640 -o root -g hotserve -T /dev/null /etc/hotserve/e2e-secrets.env >/dev/null 2>&1 \
	&& pass "sudo -l admits the env-file line (the refusals above are the policy's)" \
	|| fail "sudo -l refuses even the env-file line: the refusals above prove nothing"

echo "=== box 1: the example's Caddyfile, with a signer, is a valid config with this build ==="
# It ships with no signer (make signer adds the operator's), and a box
# refuses a file without one; a throwaway key stands in.
ssh-keygen -q -t ed25519 -N "" -C e2e -f "$tmp/signer" >/dev/null
key=$(cut -d' ' -f1,2 "$tmp/signer.pub")
awk -v line="signer e2e@example.com $key" '{ print } /^[[:space:]]*# signers[[:space:]]*$/ { print "\t\t" line }' \
	examples/box/deploy.example.com/Caddyfile >"$tmp/Caddyfile"
if admin hotserve validate --adapter caddyfile --config /dev/stdin <"$tmp/Caddyfile" >"$tmp/out" 2>&1; then
	pass "the example's Caddyfile is a valid config"
else
	fail "the example's Caddyfile does not validate: $(tail -3 "$tmp/out")"
fi
if admin hotserve box describe /dev/stdin <"$tmp/Caddyfile" >"$tmp/out" 2>&1 \
	&& [ "$(cat "$tmp/out")" = "$(printf 'host deploy.example.com\napp example\nsigner e2e@example.com %s' "$key")" ]; then
	pass "the box reads the example as the deploy.example.com box, one app, one signer"
else
	fail "hotserve box describe on the example: $(cat "$tmp/out")"
fi

echo ""
if [ "$FAILURES" -eq 0 ]; then
	echo "ALL BOX SCENARIOS PASSED"
	exit 0
fi
echo "$FAILURES BOX ASSERTION(S) FAILED"
exit 1
