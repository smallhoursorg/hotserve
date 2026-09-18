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
# Two repositories: a path on this box for most of the suite, then a
# real S3 endpoint (e2e-s3, rclone serve s3, on an internal network of
# its own) for restic's s3 backend and the storage credentials, end to
# end through the job's sandbox. What only a real provider can show —
# TLS to it, and a key its policy makes append-only — is checked on a
# real box before release, not here; the delete check's reading of a
# refusal is pinned in the unit tests by restic's real output.
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
# As the app's own user: sqlite3 run as root on a WAL database it is the
# first to open creates -wal and -shm owned by root, and the app — the
# writer here — can then no longer open its own database.
sq_app() { su -s /bin/sh hotserve -c "sqlite3 -cmd '.timeout 5000' '$SHARED/app.db' \"$1\""; }

echo "=== preparing two apps: one with a database, one with only files ==="
install -d -o hotserve -g hotserve -m 0750 "$SHARED" "$SHARED/uploads" "$FILES_SHARED" "$FILES_SHARED/pages" /srv
as_hotserve "sqlite3 '$SHARED/app.db' \"PRAGMA journal_mode=WAL; CREATE TABLE IF NOT EXISTS rows(n INTEGER PRIMARY KEY, t TEXT);\"" >/dev/null
as_hotserve "echo photo > '$SHARED/uploads/cat.jpg'"
as_hotserve "echo page > '$FILES_SHARED/pages/index.md'"

# A writer, standing in for the app: the copy has to hold up while the
# app is writing, which is the entire reason VACUUM INTO is used — and
# so does a restore. Stopped for the idle-database checks, started
# again for the restore.
start_writer() { as_hotserve "sh -c 'i=0; while [ \$i -lt 20000 ]; do sqlite3 \"$SHARED/app.db\" \"INSERT INTO rows(t) VALUES (datetime(\\\"now\\\"));\" >/dev/null 2>&1; i=\$((i+1)); sleep 0.05; done' &" >/dev/null 2>&1; }
start_writer

echo "=== hotserve backup init ==="
rm -rf "$REPO" /etc/hotserve/backup.env
# init's checks run as the hourly job does — a unit with its sandbox,
# its user and its PATH — so nothing in root's own shell can make them
# pass. Prove it the hard way: a restic first on root's PATH that
# always fails. If init used it, init would fail; the job never would.
mkdir -p /tmp/roots-own-bin
printf '#!/bin/sh\necho "the restic on root'"'"'s PATH was used" >&2\nexit 97\n' > /tmp/roots-own-bin/restic
chmod +x /tmp/roots-own-bin/restic
init_out=$(PATH="/tmp/roots-own-bin:$PATH" RESTIC_PASSWORD=e2e hotserve backup init "$REPO" 2>&1)
if echo "$init_out" | grep -q "repository ready"; then
	pass "init created the repository"
else
	fail "init did not create the repository: $init_out"
fi
if echo "$init_out" | grep -q "PATH was used"; then
	fail "init ran the restic on root's PATH, which the hourly job never would"
else
	pass "init's checks ignored the restic on root's PATH: they run as the job runs"
fi
# restic wrote the repository as the jobs' user, from inside the unit —
# so nothing in it ever needed handing over by a root chown.
if [ "$(stat -c %U "$REPO/config")" = hotserve ] && [ "$(stat -c %U "$REPO")" = hotserve ]; then
	pass "the repository was created by the jobs' own user"
else
	fail "repository owned by $(stat -c %U "$REPO")/$(stat -c %U "$REPO/config" 2>/dev/null), want hotserve"
fi
# Everything init's checks put on disk — the password among it — lives
# in one root-only directory on tmpfs, and goes when init finishes.
if [ "$(stat -c '%U %a' /run/hotserve-backup)" = "root 700" ]; then
	pass "init's checks run from a root-only directory on tmpfs"
else
	fail "/run/hotserve-backup is $(stat -c '%U %a' /run/hotserve-backup), want root 700"
fi
if ls -A /run/hotserve-backup | grep -q .; then
	fail "init left something behind: $(ls -A /run/hotserve-backup)"
else
	pass "init left neither its check directory nor a copy of the settings behind"
fi

# A directory with anything else in it is never taken over: init gives
# the whole repository to the backup user and every job mounts it
# writable. /var/backups on Debian holds shadow.bak.
mkdir -p /srv/not-a-repo
echo 'root:$y$j9T$secret' > /srv/not-a-repo/shadow.bak
chmod 0600 /srv/not-a-repo/shadow.bak
refused=$(RESTIC_PASSWORD=e2e hotserve backup init /srv/not-a-repo --force 2>&1 || true)
if echo "$refused" | grep -q "not a restic repository"; then
	pass "init refuses a populated directory that is not a repository"
else
	fail "init took over a populated directory: $refused"
fi
if [ "$(stat -c %U /srv/not-a-repo/shadow.bak)" = root ] && [ "$(stat -c %a /srv/not-a-repo/shadow.bak)" = 600 ]; then
	pass "the refused directory's contents were not touched"
else
	fail "shadow.bak is now $(stat -c '%U %a' /srv/not-a-repo/shadow.bak)"
fi
if grep -q "^RESTIC_REPOSITORY=$REPO\$" /etc/hotserve/backup.env; then
	pass "refusing the populated directory left the working settings alone"
else
	fail "backup.env was changed by a refused init"
fi
rm -rf /srv/not-a-repo
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

# A repository path is checked as it resolves on disk, not as it
# reads: init hands the whole tree to the backup user and every job
# mounts it writable, so a link into a system directory would give
# both away. Checked on a real filesystem because the bug is a real
# symlink, not a string.
ln -sfn /etc /srv/looks-harmless
linked=$(hotserve backup init /srv/looks-harmless --force 2>&1 || true)
if echo "$linked" | grep -q "resolves to"; then
	pass "a repository path that resolves into a system directory is refused"
else
	fail "a symlinked repository was not refused: $linked"
fi
if grep -q "^RESTIC_REPOSITORY=$REPO\$" /etc/hotserve/backup.env; then
	pass "the refused init left the working settings alone"
else
	fail "backup.env was changed by a refused init: $(grep '^RESTIC_REPOSITORY=' /etc/hotserve/backup.env)"
fi
rm -f /srv/looks-harmless

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
# Run as root, `backup restic --` drops to the backups' user; it must
# take that user's HOME with it, or restic has no cache and says so on
# every command the docs give.
passthrough_err=$(hotserve backup restic -- snapshots --tag hotserve 2>&1 >/dev/null)
if echo "$passthrough_err" | grep -q "unable to open cache"; then
	fail "backup restic -- runs restic without a cache: $passthrough_err"
else
	pass "backup restic -- runs restic with the backups' user's own cache"
fi

echo "=== the job ran in its own sandbox ==="
if journalctl --no-pager -u hotserve-backup-backup-example.service | grep -q 'restic backup'; then
	pass "the job logged the restic command it ran"
else
	fail "the job did not log its restic command"
fi
# A run only counts once the snapshot has been read back out of the
# repository holding what was declared. Checked here against restic's
# real JSON, which the unit tests can only imitate: backup-example
# declares one database and one files path, so two.
if journalctl --no-pager -u hotserve-backup-backup-example.service | grep -q 'read back: 2 declared path(s) present'; then
	pass "the job read its snapshot back and found every declared path in it"
else
	fail "the job did not verify its snapshot: $(journalctl --no-pager -u hotserve-backup-backup-example.service | tail -5)"
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
if grep -q "files-example: backing up in hotserve-backup-files-example, its data read-only:" /tmp/run1.log; then
	pass "a files-only app's data is bound read-only"
else
	fail "a files-only app should be bound read-only: $(grep 'files-example: backing up' /tmp/run1.log)"
fi
if grep -q "backup-example: backing up in hotserve-backup-backup-example, its data writable, for SQLite:" /tmp/run1.log; then
	pass "the database app's data is bound writable (SQLite needs to create -shm)"
else
	fail "the database app needs a writable bind: $(grep 'backup-example: backing up' /tmp/run1.log)"
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

echo "=== hotserve backup restore, on a live box ==="
# The restore docs/backups.md tells an operator to run, run the way it
# says: with the app up and the writer still writing. Bad data first —
# rows the snapshot holds deleted, an upload deleted, one added since.
start_writer
sleep 1
rows_pre=$(rows_now)
sleep 2
if [ "$(rows_now)" -gt "$rows_pre" ]; then
	pass "the writer is writing as the restore begins"
else
	fail "the writer had stopped before the restore ($rows_pre rows), so the checks after it prove nothing about writes: $(ls -l "$SHARED")"
fi
sq_app 'delete from rows where n <= 10'
as_hotserve "rm '$SHARED/uploads/cat.jpg'; echo later > '$SHARED/uploads/added-later.txt'"
first_row() { sq_app 'select count(*) from rows where n = 1' 2>/dev/null; }

# Not at a terminal and no --yes: it asks nobody, so it restores nothing.
unasked=$(hotserve backup restore backup-example --admin 127.0.0.1:2019 </dev/null 2>&1 || true)
if echo "$unasked" | grep -q -- "--yes" && [ "$(first_row)" = 0 ]; then
	pass "without a terminal or --yes, restore refuses and changes nothing"
else
	fail "restore went ahead unasked: $unasked"
fi
# At a terminal, a wrong name is a no.
wrong=$(printf 'y\n' | script -qec "hotserve backup restore backup-example --admin 127.0.0.1:2019" /dev/null 2>&1 || true)
if echo "$wrong" | grep -q "nothing was restored" && [ "$(first_row)" = 0 ]; then
	pass "restore asks for the app's name, and 'y' is not it"
else
	fail "a wrong answer did not stop the restore: $wrong"
fi

# The snapshot taken under write load, by id.
restored=$(hotserve backup restore backup-example --admin 127.0.0.1:2019 --snapshot "$snap" --yes 2>&1)
if echo "$restored" | grep -q "restored from snapshot $snap"; then
	pass "restore --snapshot $snap succeeded"
else
	fail "restore failed: $restored"
fi
if echo "$restored" | grep -q "restoring in hotserve-backup-backup-example"; then
	pass "the restore ran in the app's backup unit, so it cannot overlap a backup"
else
	fail "the restore did not say where it ran: $restored"
fi
if [ "$(first_row)" = 1 ] && [ "$(sq_app 'pragma integrity_check')" = ok ]; then
	pass "the deleted rows are back, and the live database passes its integrity check"
else
	fail "the database was not restored: first row $(first_row), integrity $(sq_app 'pragma integrity_check' 2>&1)"
fi
if [ "$(sq_app 'pragma journal_mode')" = wal ] && [ "$(stat -c %U "$SHARED/app.db")" = hotserve ]; then
	pass "the database is still the app's: WAL mode, owned by hotserve"
else
	fail "the restored database is $(sq_app 'pragma journal_mode'), owned by $(stat -c %U "$SHARED/app.db")"
fi
# The writer kept going through the restore, and keeps going after it.
rows_mid=$(rows_now)
sleep 2
if [ "$(rows_now)" -gt "$rows_mid" ] && [ "$(sq_app 'pragma integrity_check')" = ok ]; then
	pass "the app writes on after the restore ($rows_mid → $(rows_now) rows), and the database stays intact"
else
	fail "writes stopped or the database broke after the restore: $rows_mid → $(rows_now)"
fi
if [ "$(cat "$SHARED/uploads/cat.jpg" 2>/dev/null)" = photo ] && [ -f "$SHARED/uploads/added-later.txt" ]; then
	pass "the deleted upload is back, and the one added since is kept"
else
	fail "uploads after restore: $(ls "$SHARED/uploads" | tr '\n' ' ')"
fi
# The restore's own dir stays (restic's cache is in it); the copies do not.
if [ -e /var/lib/hotserve-backup/backup-example/restore/copies ]; then
	fail "the restore left its plaintext database copies behind"
else
	pass "the restore removed its database copies"
fi

# --delete, confirmed at a terminal by typing the name.
deleted=$(printf 'backup-example\n' | script -qec "hotserve backup restore backup-example --admin 127.0.0.1:2019 --delete" /dev/null 2>&1)
if echo "$deleted" | grep -q "restored from snapshot" && [ ! -e "$SHARED/uploads/added-later.txt" ] && [ -f "$SHARED/uploads/cat.jpg" ]; then
	pass "typing the name confirms; --delete removes what was added since"
else
	fail "restore --delete: $deleted / $(ls "$SHARED/uploads" | tr '\n' ' ')"
fi

echo "=== hotserve backup restore, on a rebuilt box ==="
# Before the first deploy there is no shared dir at all: restore makes
# it as liveswap would — the hotserve user's, 0750 — and fills it.
rm -rf /var/lib/liveswap/files-example
rebuilt=$(hotserve backup restore files-example --admin 127.0.0.1:2019 --yes 2>&1)
if [ "$(cat "$FILES_SHARED/pages/index.md" 2>/dev/null)" = page ]; then
	pass "restore before the first deploy puts the app's files back"
else
	fail "restore on a rebuilt box: $rebuilt"
fi
if [ "$(stat -c '%U %a' "$FILES_SHARED")" = "hotserve 750" ] && [ "$(stat -c '%U %a' /var/lib/liveswap/files-example)" = "hotserve 750" ]; then
	pass "the app's dirs it made are the hotserve user's, mode 0750"
else
	fail "made $(stat -c '%U %a' /var/lib/liveswap/files-example) and $(stat -c '%U %a' "$FILES_SHARED")"
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

# restic writes a snapshot even when it exits non-zero, so snapshots
# alone are not evidence that backups work: a clean run says so with a
# record in the repository. Make a snapshot no clean run vouches for —
# newer than every one that is — and a restore must name it, not take
# it: it may be missing files, and with --delete those would go.
if hotserve backup restic -- backup --quiet --tag hotserve --tag app:backup-example "$SHARED/uploads" >/dev/null 2>&1; then
	pass "made a snapshot no clean-run record vouches for"
else
	fail "could not make an unvouched snapshot"
fi
if hotserve backup restic -- snapshots --tag hotserve-clean 2>/dev/null | grep -q hotserve-clean; then
	pass "clean runs are recorded in the repository itself"
else
	fail "no clean-run record in the repository: $(hotserve backup restic -- snapshots 2>&1 | tail -5)"
fi
# No terminal and no --yes: it describes its choice and then refuses,
# so nothing is restored here.
picked=$(hotserve backup restore backup-example --admin 127.0.0.1:2019 </dev/null 2>&1 || true)
if echo "$picked" | grep -q "is newer, but the run that took it did not finish cleanly"; then
	pass "restore passes over a snapshot no clean run vouches for, and says so"
else
	fail "restore did not pass over the unvouched snapshot: $picked"
fi

echo "=== an S3 repository: restic's s3 backend, through the job's sandbox ==="
# Everything above used a path on this box, which never touches what an
# operator's real setup does: restic's S3 client, storage credentials
# arriving through the settings file, and all of it from inside the
# job's sandbox. e2e-s3 (rclone serve s3) stands in for B2 or S3. It is
# not asked to enforce an append-only policy — that is the storage
# provider's job — so its one key can delete, and init must say so.
S3=http://e2e-s3:8333
S3_KEY_ID=hotserve-e2e
S3_WRONG_SECRET=wrong
# A wrong key is refused. Checked with curl: restic treats a rejected
# request as transient and retries it for minutes.
code=$(curl -s -o /dev/null -w '%{http_code}' --aws-sigv4 "aws:amz:us-east-1:s3" --user "$S3_KEY_ID:$S3_WRONG_SECRET" "$S3/")
if [ "$code" = 403 ]; then
	pass "the S3 endpoint refuses a wrong key"
else
	fail "the S3 endpoint answered a wrong key with HTTP $code"
fi
# Set up exactly as docs/backups.md tells an operator to: the key in a
# root-only file, created 0600 before anything is written into it.
install -m 0600 /dev/null /root/s3-key
printf 'AWS_ACCESS_KEY_ID=%s\nAWS_SECRET_ACCESS_KEY=%s\n' hotserve-e2e not-a-secret | tee /root/s3-key >/dev/null
s3init=$(RESTIC_PASSWORD=e2e-s3 hotserve backup init "s3:$S3/hotserve-e2e" --credentials-file /root/s3-key --force 2>&1)
rm -f /root/s3-key
if echo "$s3init" | grep -q "repository ready"; then
	pass "init set up an S3 repository through the job's sandbox"
else
	fail "init against S3 failed: $s3init"
fi
if echo "$s3init" | grep -q "WARNING: these credentials can delete backups"; then
	pass "init found that this S3 key can delete, against a real S3 server"
else
	fail "init did not report the S3 key as able to delete: $s3init"
fi
if grep -q "^AWS_SECRET_ACCESS_KEY=not-a-secret\$" /etc/hotserve/backup.env && grep -q "^RESTIC_REPOSITORY=s3:$S3/hotserve-e2e\$" /etc/hotserve/backup.env; then
	pass "the S3 key and repository went into the settings the jobs get"
else
	fail "backup.env does not hold the S3 settings: $(grep -v PASSWORD /etc/hotserve/backup.env)"
fi
# A repository that already holds this app's snapshots, but no clean
# run from this box: whatever this box did into the repository it used
# before, the report must not vouch for this one. (A marker file on the
# box could not tell the two repositories apart.)
hotserve backup restic -- backup --quiet --tag hotserve --tag app:backup-example "$SHARED/uploads" >/dev/null 2>&1
if hotserve backup status --check --admin 127.0.0.1:2019 >/dev/null 2>&1; then
	fail "--check passed on a repository with no clean run recorded from this box"
else
	pass "--check fails on a switched-to repository until a clean run lands in it, however fresh its snapshots"
fi
if hotserve backup status --admin 127.0.0.1:2019 2>&1 | grep -q "no clean run on this box"; then
	pass "the report says why a fresh snapshot is not a backup"
else
	fail "the report gave no reason: $(hotserve backup status --admin 127.0.0.1:2019 2>&1)"
fi
t0=$(date +%s)
sleep 1
if hotserve backup run --admin 127.0.0.1:2019 >/tmp/run-s3.log 2>&1 && grep -q 'backup-example: ok' /tmp/run-s3.log && grep -q 'files-example: ok' /tmp/run-s3.log; then
	pass "the hourly run backed both apps up to S3"
else
	fail "the run against S3 failed: $(tail -5 /tmp/run-s3.log)"
fi
if journalctl --no-pager -u hotserve-backup-backup-example.service --since "@$t0" | grep -q 'read back: 2 declared path(s) present'; then
	pass "the job read its S3 snapshot back and found every declared path in it"
else
	fail "no read-back of the S3 snapshot: $(journalctl --no-pager -u hotserve-backup-backup-example.service --since "@$t0" | tail -5)"
fi
as_hotserve "rm '$FILES_SHARED/pages/index.md'"
s3_restore=$(hotserve backup restore files-example --admin 127.0.0.1:2019 --yes 2>&1)
if [ "$(cat "$FILES_SHARED/pages/index.md" 2>/dev/null)" = page ]; then
	pass "a restore from S3 gives back the app's files"
else
	fail "restore from S3 failed: $s3_restore"
fi
# The drill docs/backups.md suggests: into a scratch dir, by hand.
rm -rf /tmp/s3-restore
if hotserve backup restic -- restore latest --tag app:files-example --target /tmp/s3-restore >/dev/null 2>&1 \
	&& [ "$(cat "/tmp/s3-restore$FILES_SHARED/pages/index.md" 2>/dev/null)" = page ]; then
	pass "the documented restore drill works against S3"
else
	fail "restore drill from S3 failed: $(ls -R /tmp/s3-restore 2>&1 | head -5)"
fi
if hotserve backup status --check --admin 127.0.0.1:2019 >/dev/null 2>&1; then
	pass "--check passes with current backups in S3"
else
	fail "--check failed against S3: $(hotserve backup status --admin 127.0.0.1:2019 2>&1)"
fi

echo "=== init at a terminal: one command, and it asks ==="
# The documented setup is one command: at a terminal, init asks for the
# storage key itself (the secret half without echo), so it is typed into
# no command line and no shell history. script(1) gives it a real TTY;
# the answers arrive on it the way a person would type them.
tty_init() { script -qec "hotserve backup init s3:$S3/hotserve-e2e-tty --force" /dev/null; }
# The secret is sent a moment after the key, as a person would type it
# after its prompt appears: a pty echoes input when it ARRIVES, and init
# turns echo off only when it asks for the secret. (The prompts come
# before anything touches the network, so a few seconds is ample.)
new_out=$({ printf '%s\n' "$S3_KEY_ID"; sleep 3; printf '%s\n' not-a-secret; } | tty_init 2>&1)
if echo "$new_out" | grep -q "repository ready" && grep -q "^AWS_SECRET_ACCESS_KEY=not-a-secret\$" /etc/hotserve/backup.env; then
	pass "init at a terminal asked for the key and set up a new repository"
else
	fail "init at a terminal did not set up the repository: $new_out"
fi
# The key ID, typed with echo on, must show: otherwise this capture
# would not show typed input at all and the next check would be vacuous.
if ! echo "$new_out" | grep -q "$S3_KEY_ID"; then
	fail "the key ID did not show as it was typed, so the echo check below proves nothing: $new_out"
elif echo "$new_out" | grep -q "not-a-secret"; then
	fail "the secret access key was shown on screen as it was typed"
else
	pass "the secret access key was not shown as it was typed"
fi
# A rebuilt box: the repository exists, so init asks for its password
# instead of inventing one that could not open it.
saved_pw=$(sed -n 's/^RESTIC_PASSWORD=//p' /etc/hotserve/backup.env)
old_out=$(printf '%s\n%s\n%s\n' "$S3_KEY_ID" not-a-secret "$saved_pw" | tty_init 2>&1)
if echo "$old_out" | grep -q "already exists. Its password" && echo "$old_out" | grep -q "repository ready"; then
	pass "on an existing repository, init asked for its password and opened it"
else
	fail "init did not ask for the existing repository's password: $old_out"
fi
if grep -q "^RESTIC_PASSWORD=$saved_pw\$" /etc/hotserve/backup.env; then
	pass "the settings hold the password that was asked for"
else
	fail "the settings do not hold the repository's own password"
fi
wrong_out=$(printf '%s\n%s\n%s\n' "$S3_KEY_ID" not-a-secret "not-the-password" | tty_init 2>&1 || true)
if echo "$wrong_out" | grep -q "cannot open it"; then
	pass "a wrong password is refused, and said so"
else
	fail "a wrong password was not refused: $wrong_out"
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
