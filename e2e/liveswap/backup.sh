#!/bin/sh
# Runs INSIDE the e2e-hotserve container (systemd as PID 1): the whole
# backup path against a real box — a real hotserve serving a real
# config, real transient units, a real restic repository and a real
# SQLite database being written while it is copied.
#
# What this proves that unit tests cannot: the per-app job's sandbox is
# one systemd accepts and can start; a database copied under write
# load restores with its integrity intact; the job cannot write to the
# data it reads; and an app whose block declares no state is left
# alone.
#
# The repository is a local path standing in for S3 — the compose
# network is offline. What S3 adds (TLS, credentials, an append-only
# key) is checked on a real box before release, not here.
set -u

pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; failures=$((failures + 1)); }
failures=0

REPO=/srv/e2e-restic
SHARED=/var/lib/liveswap/backup-example/shared
FILES_SHARED=/var/lib/liveswap/files-example/shared
# Deliberately no RESTIC_* in this script's environment: the settings
# live in a root-only file, and every restic command here goes through
# `hotserve backup restic --`, exactly as docs/backups.md tells an
# operator to. An ambient repository and password would hide the very
# failure that page had — commands that cannot find the repository.
as_hotserve() { su -s /bin/sh hotserve -c "$*"; }
# The writer holds the database between commits, so every read here
# waits rather than failing with "database is locked".
sq() { sqlite3 -cmd '.timeout 5000' "$@"; }
rows_now() { sq "file:$SHARED/app.db?mode=ro" 'select count(*) from rows'; }

echo "=== preparing two apps: one with a database, one with only files ==="
install -d -o hotserve -g hotserve -m 0750 "$SHARED" "$SHARED/uploads" "$FILES_SHARED" "$FILES_SHARED/pages" /srv
as_hotserve "sqlite3 '$SHARED/app.db' \"PRAGMA journal_mode=WAL; CREATE TABLE IF NOT EXISTS rows(n INTEGER PRIMARY KEY, t TEXT);\"" >/dev/null
as_hotserve "echo photo > '$SHARED/uploads/cat.jpg'"
as_hotserve "echo page > '$FILES_SHARED/pages/index.md'"

# A writer for the whole run: the copy has to hold up while the app is
# writing, which is the entire reason VACUUM INTO is used.
as_hotserve "sh -c 'i=0; while [ \$i -lt 20000 ]; do sqlite3 \"$SHARED/app.db\" \"INSERT INTO rows(t) VALUES (datetime(\\\"now\\\"));\" >/dev/null 2>&1; i=\$((i+1)); sleep 0.05; done' &" >/dev/null 2>&1

echo "=== hotserve backup init ==="
rm -rf "$REPO" /etc/hotserve/backup.env
install -d -o hotserve -g hotserve -m 0750 "$REPO"
init_out=$(RESTIC_PASSWORD=e2e hotserve backup init "$REPO" 2>&1)
if echo "$init_out" | grep -q "repository ready"; then
	pass "init created the repository"
else
	fail "init did not create the repository: $init_out"
fi
# A local path is a key that CAN delete, so the warning must appear —
# this is the check that tells an operator their backups are erasable.
if echo "$init_out" | grep -q "WARNING: these credentials can delete backups"; then
	pass "init reported that these credentials can delete"
else
	fail "init did not run the delete check: $init_out"
fi
if [ "$(stat -c %a /etc/hotserve/backup.env)" = 600 ]; then
	pass "the environment file is root-only"
else
	fail "backup.env mode is $(stat -c %a /etc/hotserve/backup.env), want 600"
fi

echo "=== pointing a rebuilt box at the repository it already has ==="
# What recovering a box means: the same repository, opened with the
# password saved elsewhere. sudo does not carry RESTIC_PASSWORD, so it
# arrives in a file — and init must reuse that password rather than
# invent a new one that cannot read what is there.
printf 'e2e' > /tmp/restic-password
chmod 0600 /tmp/restic-password
cp /etc/hotserve/backup.env /tmp/backup.env.first
rebuilt=$(hotserve backup init "$REPO" --password-file /tmp/restic-password --force 2>&1)
if echo "$rebuilt" | grep -q "repository ready"; then
	pass "a rebuilt box reopens the existing repository with --password-file"
else
	fail "--password-file did not reopen the repository: $rebuilt"
fi
if grep -q '^RESTIC_PASSWORD=e2e$' /etc/hotserve/backup.env; then
	pass "it kept the repository's own password"
else
	fail "the password was replaced: $(grep '^RESTIC_PASSWORD=' /etc/hotserve/backup.env)"
fi
# The opposite mistake: no password for a repository that exists would
# write a new one, and the box would back up into something it could
# not read.
wrong=$(hotserve backup init "$REPO" --force 2>&1 || true)
if echo "$wrong" | grep -q "already exists"; then
	pass "init refuses to invent a second password for an existing repository"
else
	fail "init should refuse a generated password on an existing repository: $wrong"
fi
cp /tmp/backup.env.first /etc/hotserve/backup.env

echo "=== hotserve backup run ==="
rows_before=$(rows_now)
if hotserve backup run --admin 127.0.0.1:2019 >/tmp/run1.log 2>&1; then
	pass "the run succeeded ($(grep -c '^' /tmp/run1.log) lines)"
else
	fail "the run failed: $(tail -3 /tmp/run1.log)"
fi
# Only apps that declare state are touched: demo and the examples do
# not, and must not have been launched.
if grep -q 'backup-example: ok' /tmp/run1.log && grep -q 'files-example: ok' /tmp/run1.log && ! grep -q 'demo: ok' /tmp/run1.log; then
	pass "both apps declaring state were backed up, and only those"
else
	fail "wrong apps backed up: $(grep -E ': (ok|FAILED)' /tmp/run1.log | tr '\n' ' ')"
fi

# The snapshot from THIS run is the one taken while the writer was
# inserting; it is what the restore checks below use, so it is chosen
# now, before the writer stops.
snap=$(hotserve backup restic -- snapshots --json --tag app:backup-example 2>/dev/null \
	| tr ',' '\n' | grep -o '"short_id":"[^"]*"' | tail -1 | cut -d'"' -f4)

echo "=== the job ran in its own sandbox ==="
if journalctl --no-pager -u hotserve-backup-backup-example.service | grep -q 'restic backup'; then
	pass "the job logged the restic command it ran"
else
	fail "the job did not log its restic command"
fi
# An app with a database needs its dir writable — SQLite creates the
# -shm file beside the database to read a WAL database at all — so the
# guarantee is what the job does with that access: it reads. Checked
# with the writer stopped, since otherwise the app changes its own
# database and the comparison means nothing.
pkill -f 'INSERT INTO rows' 2>/dev/null
sleep 1
# With the writer gone and the database closed cleanly, SQLite removes
# -wal and -shm: this is the idle-app case, the one a box hits after a
# reboot or while an app is crash-looping, and the copy must still be
# taken.
# The precondition has to hold, or the check below passes without
# testing anything: wait for the sidecars to go, and fail if they do
# not. A note printed into a passing suite is not a check.
idle=0
for _ in 1 2 3 4 5 6 7 8 9 10; do
	if ! ls "$SHARED" | grep -qE 'app[.]db-(wal|shm)'; then
		idle=1
		break
	fi
	sleep 1
done
if [ "$idle" = 1 ]; then
	pass "the database is idle: no -wal/-shm on disk"
else
	fail "-wal/-shm are still present, so the idle-database case below is not being exercised: $(ls "$SHARED" | tr '\n' ' ')"
fi
db_before=$(sha256sum "$SHARED/app.db" | cut -d' ' -f1)
if hotserve backup run --admin 127.0.0.1:2019 >/tmp/run-quiet.log 2>&1; then
	pass "a backup of an idle database (no -wal/-shm) succeeds"
else
	fail "backing up an idle database failed: $(tail -3 /tmp/run-quiet.log)"
fi
if [ "$db_before" = "$(sha256sum "$SHARED/app.db" | cut -d' ' -f1)" ]; then
	pass "the app's own database is byte-identical after a backup"
else
	fail "the backup modified the app's database"
fi
# An app that declares only files never opens a database, so it must
# not be given write access to its data: least privilege per app.
if grep -q "BindReadOnlyPaths=$FILES_SHARED" /tmp/run1.log; then
	pass "a files-only app's data is bound read-only"
else
	fail "a files-only app should be bound read-only: $(grep -o 'BindReadOnlyPaths=[^ ]*files-example[^ ]*' /tmp/run1.log | head -1)"
fi
if grep -q "BindPaths=$SHARED" /tmp/run1.log; then
	pass "the database app's data is bound writable (SQLite needs to create -shm)"
else
	fail "the database app needs a writable bind"
fi

echo "=== the snapshot taken under write load restores ==="
if [ -n "$snap" ]; then
	pass "the snapshot is tagged with the app ($snap)"
else
	fail "no snapshot tagged app:backup-example"
fi
rm -rf /tmp/restored
hotserve backup restic -- restore "$snap" --target /tmp/restored >/dev/null 2>&1
db=$(find /tmp/restored -name app.db | head -1)
if [ -n "$db" ] && [ "$(sq "$db" 'pragma integrity_check')" = ok ]; then
	pass "the restored database passes its integrity check"
else
	fail "the restored database is broken or missing ($db)"
fi
rows_restored=$(sq "$db" 'select count(*) from rows' 2>/dev/null || echo 0)
if [ "${rows_restored:-0}" -ge "${rows_before:-0}" ]; then
	pass "the copy holds every row written before the run ($rows_restored >= $rows_before)"
else
	fail "rows lost: restored $rows_restored, live had $rows_before before the run"
fi
if find /tmp/restored -name cat.jpg | grep -q .; then
	pass "the declared uploads dir is in the snapshot"
else
	fail "the uploads dir is missing from the snapshot"
fi

echo "=== a quoted repository: systemd and hotserve must read it the same ==="
# systemd's EnvironmentFile= strips surrounding quotes before the job
# sees the value. If hotserve's own reader kept them, the launcher
# would look for a repository named "\"/srv/…\"", find no leading
# slash, and never bind it into the job's view — the job would then
# fail every hour with "repository does not exist". Only a real unit
# proves the two agree.
cp /etc/hotserve/backup.env /tmp/backup.env.plain
sed -i "s|^RESTIC_REPOSITORY=.*|RESTIC_REPOSITORY=\"$REPO\"|" /etc/hotserve/backup.env
if hotserve backup run --admin 127.0.0.1:2019 >/tmp/run-quoted.log 2>&1; then
	pass "a quoted repository in the environment file still works"
else
	fail "a quoted repository broke the run: $(tail -3 /tmp/run-quoted.log)"
fi
cp /tmp/backup.env.plain /etc/hotserve/backup.env

echo "=== a second run (the staged copy from the first must be cleared) ==="
if hotserve backup run --admin 127.0.0.1:2019 >/tmp/run2.log 2>&1; then
	pass "the second run succeeded"
else
	fail "the second run failed: $(tail -3 /tmp/run2.log)"
fi

echo "=== the restore docs/backups.md tells an operator to run ==="
# The commands below are the ones on that page, in order. They are run
# here because a restore that only looks right is the failure nobody
# finds until they need it: the database is stored under the path of
# the COPY the backup took, so a bare `restic restore --target /`
# leaves the app's own database untouched. That was a real defect in
# this page, and this is what would have caught it.
rows_expected=$(sq "$SHARED/app.db" 'select count(*) from rows')
rm -f "$SHARED/app.db" "$SHARED/app.db-wal" "$SHARED/app.db-shm"
rm -rf "$SHARED/uploads"

hotserve backup restic -- restore latest --tag app:backup-example --target / >/dev/null 2>&1
install -o hotserve -g hotserve -m 0640 \
	/var/lib/hotserve-backup/backup-example/data/app.db \
	"$SHARED/app.db" 2>/dev/null
rm -f "$SHARED/app.db-wal" "$SHARED/app.db-shm"

if [ -f "$SHARED/app.db" ] && [ "$(sq "$SHARED/app.db" 'pragma integrity_check')" = ok ]; then
	pass "the documented restore puts a working database back in the app's own dir"
else
	fail "the documented restore did not restore the database to $SHARED"
fi
rows_after=$(sq "$SHARED/app.db" 'select count(*) from rows' 2>/dev/null || echo 0)
if [ "${rows_after:-0}" -ge "$((rows_expected - 5))" ]; then
	pass "the restored database holds the app's rows ($rows_after of $rows_expected)"
else
	fail "rows missing after the documented restore: $rows_after of $rows_expected"
fi
if [ -f "$SHARED/uploads/cat.jpg" ]; then
	pass "the documented restore puts declared files back where the app reads them"
else
	fail "uploads/ did not come back to $SHARED"
fi

echo "=== hotserve backup status ==="
status_out=$(hotserve backup status --admin 127.0.0.1:2019 2>&1)
# The app's own row must not say "never": a `grep -qv never` over the
# whole report always succeeds, because the header line matches.
if echo "$status_out" | grep -qE '^backup-example .*(just now|(min|hours|days) ago)'; then
	pass "status reports the app's row as backed up"
else
	fail "status did not report the backup: $status_out"
fi
if echo "$status_out" | grep -qE '^backup-example.*never'; then
	fail "status says the app has never been backed up"
fi
if hotserve backup status --check --admin 127.0.0.1:2019 >/dev/null 2>&1; then
	pass "--check passes with a current backup"
else
	fail "--check failed with a current backup: $status_out"
fi

echo "=== summary ==="
pkill -f 'INSERT INTO rows' 2>/dev/null
if [ "$failures" -eq 0 ]; then
	echo "backup suite: all checks passed"
	exit 0
fi
echo "backup suite: $failures check(s) failed"
journalctl --no-pager -u hotserve-backup-backup-example.service -n 30 | tail -15
exit 1
