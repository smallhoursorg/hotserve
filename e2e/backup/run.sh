#!/bin/sh
# Runs INSIDE the e2e-hotserve container (systemd as PID 1): the whole
# backup path against a real box — a real hotserve serving a real
# config, real transient units, a real S3 repository and a real SQLite
# database being written while it is copied.
#
# What this proves that unit tests cannot: the per-app job's sandbox is
# one systemd accepts and can start; a database copied under write
# load restores with its integrity intact; the job cannot write to the
# data it reads; and an app whose block declares no state is left
# alone.
#
# Every repository here is on e2e-s3 (rclone serve s3, on an internal
# network of its own): restic's s3 backend and the storage credentials,
# end to end through the job's sandbox. What only a real provider can
# show — TLS to it, and a key its policy makes append-only — is checked
# on a real box before release; the delete check's reading of a refusal
# is pinned in the unit tests by restic's real output.
set -u

pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; failures=$((failures + 1)); }
failures=0

S3=http://e2e-s3:8333
S3_KEY_ID=hotserve-e2e
S3_SECRET=not-a-secret
REPO="s3:$S3/hotserve-e2e"
SHARED=/var/lib/liveswap/backup-example/shared
FILES_SHARED=/var/lib/liveswap/files-example/shared
# No RESTIC_* in this script's environment: the settings live in a
# root-only file, and every restic command here goes through `hotserve
# backup restic --`, as docs/backups.md tells an operator to.
as_hotserve() { su -s /bin/sh hotserve -c "$*"; }
# The writer holds the database between commits, so every read here
# waits rather than failing with "database is locked".
sq() { sqlite3 -cmd '.timeout 5000' "$@"; }
rows_now() { sq "file:$SHARED/app.db?mode=ro" 'select count(*) from rows'; }
# As the app's own user: sqlite3 run as root on a WAL database it is the
# first to open creates -wal and -shm owned by root, and the app — the
# writer here — can then no longer open its own database.
sq_app() { su -s /bin/sh hotserve -c "sqlite3 -cmd '.timeout 5000' '$SHARED/app.db' \"$1\""; }
# The storage key, in a root-only file made 0600 before anything is
# written into it, as docs/backups.md shows for a script.
key_file() {
	install -m 0600 /dev/null /root/s3-key
	printf 'AWS_ACCESS_KEY_ID=%s\nAWS_SECRET_ACCESS_KEY=%s\n' "$S3_KEY_ID" "$S3_SECRET" >/root/s3-key
}
# init without a terminal, against a repository on e2e-s3.
s3_init() {
	key_file
	RESTIC_PASSWORD="${PW:-e2e}" hotserve backup init "$@" --credentials-file /root/s3-key 2>&1
	rm -f /root/s3-key
}

echo "=== preparing two apps: one with a database, one with only files ==="
install -d -o hotserve -g hotserve -m 0750 "$SHARED" "$SHARED/uploads" "$FILES_SHARED" "$FILES_SHARED/pages"
as_hotserve "sqlite3 '$SHARED/app.db' \"PRAGMA journal_mode=WAL; CREATE TABLE IF NOT EXISTS rows(n INTEGER PRIMARY KEY, t TEXT);\"" >/dev/null
as_hotserve "echo photo > '$SHARED/uploads/cat.jpg'"
as_hotserve "echo page > '$FILES_SHARED/pages/index.md'"
# One page belongs to a uid this box has no user for — as every file
# does, to a box rebuilt with another uid for the hotserve user. A job's
# user namespace maps only root and hotserve, so restic 0.18 records an
# owner that a restore is then refused ("lchown …: invalid argument"),
# and exits 1 over a file it restored. The rebuilt-box restore below has
# to get past that.
echo about > "$FILES_SHARED/pages/about.md"
chown 12345:12345 "$FILES_SHARED/pages/about.md"
chmod 644 "$FILES_SHARED/pages/about.md"

# A writer, standing in for the app: the copy has to hold up while the
# app is writing, which is the reason VACUUM INTO is used — and so does
# a restore. Stopped for the idle-database checks, started again for
# the restore.
start_writer() { as_hotserve "sh -c 'i=0; while [ \$i -lt 20000 ]; do sqlite3 \"$SHARED/app.db\" \"INSERT INTO rows(t) VALUES (datetime(\\\"now\\\"));\" >/dev/null 2>&1; i=\$((i+1)); sleep 0.05; done' &" >/dev/null 2>&1; }
start_writer

echo "=== the S3 endpoint ==="
# A wrong key is refused. Checked with curl: restic treats a rejected
# request as transient and retries it for minutes.
code=$(curl -s -o /dev/null -w '%{http_code}' --aws-sigv4 "aws:amz:us-east-1:s3" --user "$S3_KEY_ID:wrong" "$S3/")
if [ "$code" = 403 ]; then
	pass "the S3 endpoint refuses a wrong key"
else
	fail "the S3 endpoint answered a wrong key with HTTP $code"
fi

echo "=== hotserve backup init ==="
rm -f /etc/hotserve/backup.env
# init's checks run as the hourly job does — a unit with its sandbox,
# its user and its PATH — so nothing in root's own shell can make them
# pass. A restic first on root's PATH that always fails proves it: if
# init used it, init would fail; the job never would.
mkdir -p /tmp/roots-own-bin
printf '#!/bin/sh\necho "the restic on root'"'"'s PATH was used" >&2\nexit 97\n' > /tmp/roots-own-bin/restic
chmod +x /tmp/roots-own-bin/restic
init_out=$(PATH="/tmp/roots-own-bin:$PATH" s3_init "$REPO")
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
# e2e-s3 is not asked to enforce an append-only policy — that is the
# storage provider's job — so its one key can delete, and init must say
# so: this is the check that tells an operator their backups are
# erasable.
if echo "$init_out" | grep -q "WARNING: these credentials can delete backups"; then
	pass "init found that this key can delete, against a real S3 server"
else
	fail "init did not report the key as able to delete: $init_out"
fi
if [ "$(stat -c %a /etc/hotserve/backup.env)" = 600 ]; then
	pass "the environment file is root-only"
else
	fail "backup.env mode is $(stat -c %a /etc/hotserve/backup.env), want 600"
fi
if grep -q "^AWS_SECRET_ACCESS_KEY=$S3_SECRET\$" /etc/hotserve/backup.env && grep -q "^RESTIC_REPOSITORY=$REPO\$" /etc/hotserve/backup.env; then
	pass "the key and repository went into the settings the jobs get"
else
	fail "backup.env does not hold the S3 settings: $(grep -v PASSWORD /etc/hotserve/backup.env)"
fi

echo "=== a path on this box is never a repository ==="
# init, run and restore each refuse one, with the same error, before
# anything is created.
refused=$(s3_init /srv/backups --force || true)
if echo "$refused" | grep -q "not a backend URL" && [ ! -e /srv/backups ]; then
	pass "init refuses a path, and creates nothing there"
else
	fail "init did not refuse a path: $refused"
fi
refused=$(s3_init local:/srv/backups --force || true)
if echo "$refused" | grep -q "not a backend URL"; then
	pass "init refuses a local: repository"
else
	fail "init did not refuse local:: $refused"
fi
if grep -q "^RESTIC_REPOSITORY=$REPO\$" /etc/hotserve/backup.env; then
	pass "the refused inits left the working settings alone"
else
	fail "backup.env was changed by a refused init: $(grep '^RESTIC_REPOSITORY=' /etc/hotserve/backup.env)"
fi
cp /etc/hotserve/backup.env /tmp/backup.env.good
sed -i 's|^RESTIC_REPOSITORY=.*|RESTIC_REPOSITORY=/srv/backups|' /etc/hotserve/backup.env
refused=$(hotserve backup run --admin 127.0.0.1:2019 2>&1 || true)
if echo "$refused" | grep -q "not a backend URL" && ! echo "$refused" | grep -q "backing up in"; then
	pass "run refuses a path in the settings, before any job starts"
else
	fail "run did not refuse a path in the settings: $refused"
fi
refused=$(hotserve backup restore backup-example --admin 127.0.0.1:2019 --yes 2>&1 || true)
if echo "$refused" | grep -q "not a backend URL"; then
	pass "restore refuses a path in the settings"
else
	fail "restore did not refuse a path in the settings: $refused"
fi
cp /tmp/backup.env.good /etc/hotserve/backup.env

echo "=== pointing a rebuilt box at the repository it already has ==="
# Recovering a box means the same repository, opened with the password
# saved elsewhere. sudo does not carry RESTIC_PASSWORD, so it arrives in
# a file — and init reuses that password rather than inventing a new one
# that cannot read what is there.
printf 'e2e' > /tmp/restic-password
chmod 0600 /tmp/restic-password
key_file
rebuilt=$(hotserve backup init "$REPO" --credentials-file /root/s3-key --password-file /tmp/restic-password --force 2>&1)
rm -f /root/s3-key
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
# No password for a repository that exists would mean a generated one,
# and the box backing up into something it cannot read.
key_file
wrong=$(hotserve backup init "$REPO" --credentials-file /root/s3-key --force 2>&1 || true)
rm -f /root/s3-key
if echo "$wrong" | grep -q "already exists"; then
	pass "init refuses to invent a second password for an existing repository"
else
	fail "init should refuse a generated password on an existing repository: $wrong"
fi
cp /tmp/backup.env.good /etc/hotserve/backup.env

echo "=== hotserve backup run ==="
rows_before=$(rows_now)
if hotserve backup run --admin 127.0.0.1:2019 >/tmp/run1.log 2>&1; then
	pass "the run succeeded ($(grep -c '^' /tmp/run1.log) lines)"
else
	fail "the run failed: $(tail -3 /tmp/run1.log)"
fi
# Only apps that declare state are touched: demo and the examples do
# not, and are never launched.
if grep -q 'backup-example: ok' /tmp/run1.log && grep -q 'files-example: ok' /tmp/run1.log && ! grep -q 'demo: ok' /tmp/run1.log; then
	pass "both apps declaring state were backed up, and only those"
else
	fail "wrong apps backed up: $(grep -E ': (ok|FAILED)' /tmp/run1.log | tr '\n' ' ')"
fi

# The snapshot from this run is the one taken while the writer was
# inserting; the restore checks below use it, so it is chosen now,
# before the writer stops.
snap=$(hotserve backup restic -- snapshots --json --tag hotserve,app:backup-example 2>/dev/null \
	| tr ',' '\n' | grep -o '"short_id":"[^"]*"' | tail -1 | cut -d'"' -f4)
# Run as root, `backup restic --` drops to the backups' user and takes
# that user's HOME with it, so restic has its cache.
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
# repository holding what was declared — checked here against restic's
# real JSON. backup-example declares one database and one files path.
if journalctl --no-pager -u hotserve-backup-backup-example.service | grep -q 'read back: 2 declared path(s) present'; then
	pass "the job read its snapshot back and found every declared path in it"
else
	fail "the job did not verify its snapshot: $(journalctl --no-pager -u hotserve-backup-backup-example.service | tail -5)"
fi
# An app with a database gets its dir writable — SQLite creates the -shm
# file beside the database to read a WAL database at all — so the
# guarantee is what the job does with that access: it reads. Checked
# with the writer stopped; otherwise the app changes its own database
# and the comparison means nothing.
pkill -f 'INSERT INTO rows' 2>/dev/null
sleep 1
# With the writer gone and the database closed cleanly, SQLite removes
# -wal and -shm: the idle-app case, which a box hits after a reboot or
# while an app is crash-looping. The check below means something only
# once the sidecars are gone.
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
# An app that declares only files never opens a database, so it gets no
# write access to its data: least privilege per app.
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

echo "=== a quoted repository: systemd and hotserve read it the same ==="
# systemd's EnvironmentFile= strips surrounding quotes before the job
# sees the value, and hotserve's own reader does the same; if it did
# not, run would read "\"s3:…\"" and refuse it as not a backend URL.
# Only a real unit shows the two agree.
sed -i "s|^RESTIC_REPOSITORY=.*|RESTIC_REPOSITORY=\"$REPO\"|" /etc/hotserve/backup.env
if hotserve backup run --admin 127.0.0.1:2019 >/tmp/run-quoted.log 2>&1; then
	pass "a quoted repository in the environment file works"
else
	fail "a quoted repository broke the run: $(tail -3 /tmp/run-quoted.log)"
fi
cp /tmp/backup.env.good /etc/hotserve/backup.env

echo "=== a second run (the staged copy from the first is cleared) ==="
if hotserve backup run --admin 127.0.0.1:2019 >/tmp/run2.log 2>&1; then
	pass "the second run succeeded"
else
	fail "the second run failed: $(tail -3 /tmp/run2.log)"
fi

echo "=== hotserve backup restore, on a live box ==="
# The restore docs/backups.md tells an operator to run, run the way it
# says: with the app up and the writer writing. Bad data first — rows
# the snapshot holds deleted, an upload deleted, one added since.
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
# The writer keeps going through the restore, and after it.
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
# The page whose recorded owner this box cannot give it: restic exits 1
# over it, and the restore still counts — the file is back, and it is
# the hotserve user's.
if [ "$(cat "$FILES_SHARED/pages/about.md" 2>/dev/null)" = about ] && [ "$(stat -c '%U %a' "$FILES_SHARED/pages/about.md")" = "hotserve 644" ] \
	&& echo "$rebuilt" | grep -q "belong to this box's user" && echo "$rebuilt" | grep -q "restored from snapshot"; then
	pass "a file whose recorded owner is no user here is restored as the hotserve user's, and the restore says so"
else
	fail "a recorded owner this box cannot give failed the restore: $rebuilt ($(stat -c '%U %a' "$FILES_SHARED/pages/about.md" 2>&1))"
fi
if [ "$(stat -c '%U %a' "$FILES_SHARED")" = "hotserve 750" ] && [ "$(stat -c '%U %a' /var/lib/liveswap/files-example)" = "hotserve 750" ]; then
	pass "the app's dirs it made are the hotserve user's, mode 0750"
else
	fail "made $(stat -c '%U %a' /var/lib/liveswap/files-example) and $(stat -c '%U %a' "$FILES_SHARED")"
fi

echo "=== hotserve backup status ==="
status_out=$(hotserve backup status --admin 127.0.0.1:2019 2>&1)
# The app's own row, not the whole report: the header line would match
# a `grep -v never` of all of it.
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

echo "=== a snapshot no clean run vouches for ==="
# restic writes a snapshot even when it exits non-zero, so snapshots
# alone are not evidence that backups work: a clean run says so with a
# record in the repository. A snapshot newer than every vouched one is
# named by restore, not taken: it may be missing files, and with
# --delete those would go.
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

echo "=== another repository ==="
# A repository that already holds this app's snapshots, but no clean
# run from this box: what this box did into the repository it used
# before does not vouch for this one.
PW=e2e-2 s3_init "s3:$S3/hotserve-e2e-2" --force >/tmp/init2.log
if grep -q "repository ready" /tmp/init2.log; then
	pass "init --force moved the box to a second repository"
else
	fail "init onto a second repository failed: $(cat /tmp/init2.log)"
fi
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
	pass "the hourly run backed both apps up to the new repository"
else
	fail "the run against the new repository failed: $(tail -5 /tmp/run-s3.log)"
fi
if journalctl --no-pager -u hotserve-backup-backup-example.service --since "@$t0" | grep -q 'read back: 2 declared path(s) present'; then
	pass "the job read its snapshot back from the new repository"
else
	fail "no read-back from the new repository: $(journalctl --no-pager -u hotserve-backup-backup-example.service --since "@$t0" | tail -5)"
fi
as_hotserve "rm '$FILES_SHARED/pages/index.md'"
s3_restore=$(hotserve backup restore files-example --admin 127.0.0.1:2019 --yes 2>&1)
if [ "$(cat "$FILES_SHARED/pages/index.md" 2>/dev/null)" = page ]; then
	pass "a restore from the new repository gives back the app's files"
else
	fail "restore from the new repository failed: $s3_restore"
fi
# The drill docs/backups.md suggests: into a scratch dir, by hand.
rm -rf /tmp/s3-restore
if hotserve backup restic -- restore latest --tag hotserve,app:files-example --target /tmp/s3-restore >/dev/null 2>&1 \
	&& [ "$(cat "/tmp/s3-restore$FILES_SHARED/pages/index.md" 2>/dev/null)" = page ]; then
	pass "the documented restore drill works"
else
	fail "restore drill failed: $(ls -R /tmp/s3-restore 2>&1 | head -5)"
fi
if hotserve backup status --check --admin 127.0.0.1:2019 >/dev/null 2>&1; then
	pass "--check passes once a clean run lands in the new repository"
else
	fail "--check failed after a clean run: $(hotserve backup status --admin 127.0.0.1:2019 2>&1)"
fi

echo "=== init at a terminal: one command, and it asks ==="
# The documented setup is one command: at a terminal, init asks for the
# storage key itself (the secret half without echo), so it is typed into
# no command line and no shell history. script(1) gives it a real TTY;
# the answers arrive on it the way a person types them.
tty_init() { script -qec "hotserve backup init s3:$S3/hotserve-e2e-tty --force" /dev/null; }
# The secret is sent a moment after the key, as a person types it after
# its prompt appears: a pty echoes input when it arrives, and init turns
# echo off only when it asks for the secret. The prompts come before
# anything touches the network, so a few seconds is ample.
new_out=$({ printf '%s\n' "$S3_KEY_ID"; sleep 3; printf '%s\n' "$S3_SECRET"; } | tty_init 2>&1)
if echo "$new_out" | grep -q "repository ready" && grep -q "^AWS_SECRET_ACCESS_KEY=$S3_SECRET\$" /etc/hotserve/backup.env; then
	pass "init at a terminal asked for the key and set up a new repository"
else
	fail "init at a terminal did not set up the repository: $new_out"
fi
# The key ID, typed with echo on, shows: without it this capture would
# not show typed input at all, and the next check would be vacuous.
if ! echo "$new_out" | grep -q "$S3_KEY_ID"; then
	fail "the key ID did not show as it was typed, so the echo check below proves nothing: $new_out"
elif echo "$new_out" | grep -q "$S3_SECRET"; then
	fail "the secret access key was shown on screen as it was typed"
else
	pass "the secret access key was not shown as it was typed"
fi
# A rebuilt box: the repository exists, so init asks for its password
# instead of inventing one that could not open it.
saved_pw=$(sed -n 's/^RESTIC_PASSWORD=//p' /etc/hotserve/backup.env)
old_out=$(printf '%s\n%s\n%s\n' "$S3_KEY_ID" "$S3_SECRET" "$saved_pw" | tty_init 2>&1)
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
wrong_out=$(printf '%s\n%s\n%s\n' "$S3_KEY_ID" "$S3_SECRET" "not-the-password" | tty_init 2>&1 || true)
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
