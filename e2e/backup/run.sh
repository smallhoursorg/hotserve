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
#
# ONLY=restore-live,ctrl-c runs those sections and no others, for work on
# one of them (`make e2e BACKUP_ONLY=…`); the whole suite is what gates a
# merge. A section is an `if section <name> …` block below. What is
# outside one always runs, and is what every section stands on: the two
# apps, a repository, and one clean run made while the app was writing.
# Sections run in this file's order whatever order ONLY names them in.
# Each leaves the app's rows and files as it found them, and starts or
# stops the writer itself rather than counting on the section before it.
# Two things do carry forward, which is why the order is fixed: the
# database is WAL until the rollback-journal sections, which put it in
# that mode themselves and leave it there; and another-repository and
# init-tty each move the box to a new repository, so the snapshot id
# taken from the first run is used only above them.
set -u

pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; failures=$((failures + 1)); }
failures=0

# The names are read from this file, so there is no list of them to keep,
# and a name that is not one stops the suite before it has done anything.
sections=$(sed -n 's/^if section \([a-z-]*\) .*/\1/p' "$0" | tr '\n' ' ')
for name in $(echo "${ONLY:-}" | tr ',' ' '); do
	case " $sections " in
	*" $name "*) ;;
	*)
		echo "FATAL: ONLY names no section called '$name'; the sections are: $sections"
		exit 1
		;;
	esac
done
section() {
	if [ -n "${ONLY:-}" ]; then
		case ",$ONLY," in
		*",$1,"*) ;;
		*) return 1 ;;
		esac
	fi
	echo "=== $1: $2 ==="
}

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
first_row() { sq_app 'select count(*) from rows where n = 1' 2>/dev/null; }
count_of() { hotserve backup restic -- snapshots --json --tag "$1" 2>/dev/null | grep -o '"short_id"' | wc -l; }
# Where backup-example's job keeps its copies, and its unit.
STAGE=/var/lib/hotserve-backup/backup-example
unit=hotserve-backup-backup-example.service
wait_for() { i=0; while [ $i -lt 40 ]; do "$@" && return 0; i=$((i + 1)); sleep 0.5; done; return 1; }
# A job is a oneshot unit, which systemd calls "activating" while its
# command runs: `is-active --quiet` exits non-zero for it throughout.
unit_running() { case "$(systemctl is-active "$unit")" in active | activating | deactivating) return 0 ;; esac; return 1; }
unit_stopped() { ! unit_running; }

# Nothing has waited for the box when this suite is the only one run.
if ! wait_for curl -sf -o /dev/null --max-time 2 http://127.0.0.1:2019/config/; then
	echo "FATAL: hotserve's admin API did not answer within 20s"
	exit 1
fi
if ! wait_for curl -s -o /dev/null --max-time 2 "$S3/"; then
	echo "FATAL: the S3 endpoint at $S3 did not answer within 20s"
	exit 1
fi

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
# a restore. A section that needs it stopped, or writing, says so:
# neither does anything when that is already the case.
writer_running() { pgrep -f 'INSERT INTO rows' >/dev/null 2>&1; }
start_writer() {
	writer_running && return 0
	as_hotserve "sh -c 'i=0; while [ \$i -lt 20000 ]; do sqlite3 \"$SHARED/app.db\" \"INSERT INTO rows(t) VALUES (datetime(\\\"now\\\"));\" >/dev/null 2>&1; i=\$((i+1)); sleep 0.05; done' &" >/dev/null 2>&1
}
stop_writer() { pkill -f 'INSERT INTO rows' 2>/dev/null && sleep 1; }
# A connection holding the database exclusively keeps a backup, or a
# restore, running for as long as a check needs it to: in
# rollback-journal mode the copy and the restore both wait for it, up to
# their ten-second busy timeout. The writer is stopped so the lock is
# the only thing they wait for.
rollback_journal_idle() { stop_writer; [ "$(sq_app 'PRAGMA journal_mode=DELETE')" = delete ]; }
hold() {
	rm -f /tmp/hold
	mkfifo /tmp/hold
	chmod 666 /tmp/hold
	as_hotserve "sqlite3 '$SHARED/app.db' < /tmp/hold" >/dev/null 2>&1 &
	exec 9>/tmp/hold
	echo 'BEGIN EXCLUSIVE;' >&9
	sleep 1
}
release() { echo 'ROLLBACK;' >&9; exec 9>&-; rm -f /tmp/hold; sleep 1; }
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
if ls -A /run/hotserve-backup | grep -q . || [ -e /run/hotserve-backup-check ]; then
	fail "init left something behind: $(ls -A /run/hotserve-backup /run/hotserve-backup-check 2>&1)"
else
	pass "init left neither its checks' directory nor a copy of the settings behind"
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
# The settings that work, for a section that changes them to put back.
cp /etc/hotserve/backup.env /tmp/backup.env.good

if section no-local-path "a path on this box is never a repository"; then
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
fi

if section rebuilt-box-init "pointing a rebuilt box at the repository it already has"; then
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
fi

if section init-mistakes "init on a mistake: it answers, and leaves what worked alone"; then
	# The mistakes an operator makes, over a setup that works. Output goes to
	# a file, not a $(...): a check left running holds the pipe, and this
	# suite would then wait with it.
	settings_before=$(sha256sum /etc/hotserve/backup.env)
	# A wrong storage key. `restic init` fails at once; the look for an
	# existing repository after it is what restic would keep retrying for
	# many minutes (measured: ten, and counting). init gives that a clock,
	# and stops the unit it was waiting on.
	install -m 0600 /dev/null /root/s3-key
	printf 'AWS_ACCESS_KEY_ID=%s\nAWS_SECRET_ACCESS_KEY=not-the-secret\n' "$S3_KEY_ID" >/root/s3-key
	t_typo=$(date +%s)
	RESTIC_PASSWORD=e2e timeout 120 hotserve backup init "s3:$S3/hotserve-e2e-typo" --credentials-file /root/s3-key --force >/tmp/init-wrongkey.log 2>&1
	typo_rc=$?
	typo_took=$(($(date +%s) - t_typo))
	rm -f /root/s3-key
	if [ "$typo_rc" -ne 0 ] && [ "$typo_rc" -ne 124 ] && [ "$typo_took" -lt 60 ] && grep -q "cannot create a repository" /tmp/init-wrongkey.log; then
		pass "init with a wrong storage key answers (${typo_took}s) and says what to check"
	else
		fail "init with a wrong storage key: exit $typo_rc after ${typo_took}s: $(tail -4 /tmp/init-wrongkey.log)"
	fi
	case "$(systemctl is-active hotserve-backup_check.service 2>/dev/null)" in
	active | activating | deactivating) fail "the check init gave up on is still running: restic would retry for minutes under a name the next init needs" ;;
	*) pass "the check init gave up on was stopped with it" ;;
	esac
	# A wrong password for a repository that is there: restic says so in its
	# exit status, at once.
	key_file
	t_pw=$(date +%s)
	RESTIC_PASSWORD=not-the-password timeout 120 hotserve backup init "$REPO" --credentials-file /root/s3-key --force >/tmp/init-wrongpw.log 2>&1
	pw_rc=$?
	pw_took=$(($(date +%s) - t_pw))
	rm -f /root/s3-key
	if [ "$pw_rc" -ne 0 ] && [ "$pw_took" -lt 30 ] && grep -q "this password cannot open it" /tmp/init-wrongpw.log; then
		pass "init with a wrong password for an existing repository says so (${pw_took}s)"
	else
		fail "init with a wrong password: exit $pw_rc after ${pw_took}s: $(tail -4 /tmp/init-wrongpw.log)"
	fi
	if [ "$(sha256sum /etc/hotserve/backup.env)" = "$settings_before" ] && ! ls -A /run/hotserve-backup | grep -q . && [ ! -e /run/hotserve-backup-check ]; then
		pass "neither --force changed the working settings, and no copy of a credential was left behind"
	else
		fail "after two failed inits: settings changed=$([ "$(sha256sum /etc/hotserve/backup.env)" = "$settings_before" ] && echo no || echo YES), left: $(ls -A /run/hotserve-backup /run/hotserve-backup-check 2>&1 | tr '\n' ' ')"
	fi
fi

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
# The database copies are plaintext and the size of the databases: they
# are there while a run lasts, and gone when it is over.
if [ ! -e "$STAGE/data" ] && [ -d "$STAGE/cache" ]; then
	pass "the staged database copies are removed when the run ends; restic's cache stays"
else
	fail "after a run the staging dir holds: $(ls -A "$STAGE" | tr '\n' ' ')"
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

if section sandbox "the job ran in its own sandbox"; then
	if journalctl --no-pager -u hotserve-backup-backup-example.service | grep -q 'restic backup'; then
		pass "the job logged the restic command it ran"
	else
		fail "the job did not log its restic command"
	fi
	# restic is run with --json, for the id of the snapshot it wrote; what
	# reaches the journal is still words.
	if journalctl --no-pager -u hotserve-backup-backup-example.service | grep -Eq 'snapshot [0-9a-f]{8}: [0-9]+ new and [0-9]+ changed files, .* added to the repository'; then
		pass "the job says which snapshot it wrote and what it added"
	else
		fail "no summary of the backup in the journal: $(journalctl --no-pager -u hotserve-backup-backup-example.service | tail -5)"
	fi
	if journalctl --no-pager -u hotserve-backup-backup-example.service -u hotserve-backup-files-example.service | grep -q 'message_type'; then
		fail "restic's JSON reached the journal: $(journalctl --no-pager -u hotserve-backup-backup-example.service | grep message_type | head -2)"
	else
		pass "none of restic's JSON reaches the journal"
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
	stop_writer
	# With the writer gone and the database closed cleanly, SQLite removes
	# -wal and -shm: the idle-app case, which a box hits after a reboot or
	# while an app is crash-looping. The check below means something only
	# once the sidecars are gone. Stopping the writer can catch a sqlite3
	# mid-write, and one that is killed closes nothing: the sidecars then
	# stay until a connection next closes cleanly, so one is opened and
	# closed here, as the app's user — by a query that reads the table:
	# SQLite opens the file only when asked about what is in it, and
	# `select 1` leaves the sidecars where they are (measured).
	idle=0
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		sq_app 'select count(*) from rows' >/dev/null 2>&1
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
	# The unit that reaches the network — restic's — has an app's data
	# read-only, database or not. What has to open a database (SQLite creates
	# -shm beside one to read it at all) is a step of its own before it, with
	# the data writable and nothing else: no network, no repository settings.
	# (Its real properties are checked further down, while one is running.)
	if grep -q "files-example: backing up in hotserve-backup-files-example, its data read-only:" /tmp/run1.log \
		&& grep -q "backup-example: backing up in hotserve-backup-backup-example, its data read-only:" /tmp/run1.log; then
		pass "every app's data is read-only to the unit that uploads it"
	else
		fail "the upload units: $(grep 'backing up in' /tmp/run1.log)"
	fi
	if grep -q "backup-example: copying its databases in hotserve-backup-backup-example, its data writable" /tmp/run1.log \
		&& ! grep -q "files-example: copying" /tmp/run1.log \
		&& [ "$(grep -n 'backup-example: copying' /tmp/run1.log | cut -d: -f1)" -lt "$(grep -n 'backup-example: backing up' /tmp/run1.log | cut -d: -f1)" ]; then
		pass "an app with a database has its copy taken first, in a step of its own; an app without one has no such step"
	else
		fail "the staging step: $(grep -E 'copying|backing up in' /tmp/run1.log)"
	fi
fi

if section snapshot-restores "the snapshot taken under write load restores"; then
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
fi

if section quoted-repository "a quoted repository: systemd and hotserve read it the same"; then
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
fi

if section second-run "a second run, and a run of one app"; then
	if hotserve backup run --admin 127.0.0.1:2019 >/tmp/run2.log 2>&1; then
		pass "the second run succeeded"
	else
		fail "the second run failed: $(tail -3 /tmp/run2.log)"
	fi
	# One app, by name: what an operator runs after adding its `state`
	# lines, rather than waiting on every other app's backup first.
	if hotserve backup run files-example --admin 127.0.0.1:2019 >/tmp/run-one.log 2>&1 \
		&& grep -q 'files-example: ok' /tmp/run-one.log && ! grep -q 'backup-example' /tmp/run-one.log; then
		pass "run <app> backs up that app and no other"
	else
		fail "run files-example: $(tail -4 /tmp/run-one.log)"
	fi
	one_err=$(hotserve backup run files-exmaple --admin 127.0.0.1:2019 2>&1)
	if [ $? -ne 0 ] && echo "$one_err" | grep -q "apps that do: backup-example, files-example"; then
		pass "a name that is no app's is refused, with the names there are"
	else
		fail "run with a mistyped app name: $one_err"
	fi
fi

if section restore-live "hotserve backup restore, on a live box"; then
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
	# The restore's own dir stays (restic's cache is in it), and is the
	# hotserve user's; the copies do not.
	if [ -e "$STAGE/restore/copies" ]; then
		fail "the restore left its plaintext database copies behind"
	elif [ "$(stat -c '%U' "$STAGE/restore")" = hotserve ]; then
		pass "the restore removed its database copies, and its own dir is the hotserve user's"
	else
		fail "the restore's dir is $(stat -c '%U' "$STAGE/restore" 2>&1)'s"
	fi

	# --delete, confirmed at a terminal by typing the name.
	deleted=$(printf 'backup-example\n' | script -qec "hotserve backup restore backup-example --admin 127.0.0.1:2019 --delete" /dev/null 2>&1)
	if echo "$deleted" | grep -q "restored from snapshot" && [ ! -e "$SHARED/uploads/added-later.txt" ] && [ -f "$SHARED/uploads/cat.jpg" ]; then
		pass "typing the name confirms; --delete removes what was added since"
	else
		fail "restore --delete: $deleted / $(ls "$SHARED/uploads" | tr '\n' ' ')"
	fi
fi

if section restore-rebuilt "hotserve backup restore, on a rebuilt box"; then
	# Before the first deploy there is no shared dir at all: restore makes
	# it as liveswap would — the hotserve user's, 0750 — and fills it.
	rm -rf /var/lib/liveswap/files-example
	# This box has backed files-example up, so its data dir being gone is
	# not "never deployed": the job asks the repository, and the run fails
	# for that app rather than staying green over nothing.
	t_gone=$(date +%s)
	sleep 1
	if hotserve backup run --admin 127.0.0.1:2019 >/tmp/run-nodata.log 2>&1; then
		fail "a run over an app whose whole data dir is gone exited 0: $(tail -5 /tmp/run-nodata.log)"
	elif grep -q 'files-example: FAILED' /tmp/run-nodata.log && grep -q 'backup-example: ok' /tmp/run-nodata.log \
		&& journalctl --no-pager -u hotserve-backup-files-example.service --since "@$t_gone" | grep -q "this box has backed this app up before"; then
		pass "an app that has been backed up and whose data dir is gone fails its run, and says why"
	else
		fail "a vanished data dir: $(grep -E ': (ok|FAILED|no data)' /tmp/run-nodata.log | tr '\n' ' ') / $(journalctl --no-pager -u hotserve-backup-files-example.service --since "@$t_gone" | tail -3)"
	fi
	# A restore that goes no further than its question makes nothing: an
	# empty shared/ left behind would be an hourly failure of its own.
	hotserve backup restore files-example --admin 127.0.0.1:2019 </dev/null >/dev/null 2>&1
	if [ ! -e /var/lib/liveswap/files-example ]; then
		pass "a restore that is not confirmed leaves no empty data dir behind"
	else
		fail "an unconfirmed restore made $(find /var/lib/liveswap/files-example | tr '\n' ' ')"
	fi
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
fi

if section status "hotserve backup status"; then
	# status is root; the restic it runs is not. It talks to the network and
	# parses what the storage sends back, so it runs as the jobs' restic
	# does: in a unit, as the hotserve user, in their sandbox. A failing
	# restic first on root's PATH shows whose PATH found it.
	status_out=$(PATH="/tmp/roots-own-bin:$PATH" hotserve backup status --admin 127.0.0.1:2019 2>&1)
	if echo "$status_out" | grep -q "the restic on root's PATH was used"; then
		fail "status ran restic as root, from root's PATH: $status_out"
	elif [ "$(stat -c '%U %a' /var/lib/hotserve-backup-status 2>/dev/null)" = "hotserve 750" ] && ! ls -A /run/hotserve-backup | grep -q .; then
		pass "status runs its restic in a unit as the hotserve user, and leaves no copy of the settings"
	else
		fail "status's restic: $(stat -c '%U %a' /var/lib/hotserve-backup-status 2>&1) / left in /run/hotserve-backup: $(ls -A /run/hotserve-backup 2>&1)"
	fi
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
	hotserve backup status --check --admin 127.0.0.1:1 >/dev/null 2>&1
	check_rc=$?
	if [ "$check_rc" -eq 2 ]; then
		pass "--check that could not find out exits 2, which is not the 1 of a stale backup"
	else
		fail "--check with hotserve unreachable exited $check_rc, want 2"
	fi
fi

if section passthrough "backup restic -- keeps its settings from the hotserve uid"; then
	# The settings are in that restic's environment, and it runs with the
	# uid of the internet-facing process, which can read a same-uid
	# process's environ — unless a user namespace is in the way. Held open
	# for a few seconds by a backup of a command's output.
	hotserve backup restic -- backup --quiet --stdin-from-command --stdin-filename e2e-held -- sleep 6 >/dev/null 2>&1 &
	held_pid=$!
	if wait_for pgrep -u hotserve -x restic >/dev/null; then
		rpid=$(pgrep -u hotserve -x restic | head -1)
		if ! tr '\0' '\n' <"/proc/$rpid/environ" | grep -q '^RESTIC_PASSWORD='; then
			fail "setup: pid $rpid is not a restic holding the settings, so the check below would be vacuous"
		elif as_hotserve "cat /proc/$rpid/environ" 2>/dev/null | tr '\0' '\n' | grep -q RESTIC_PASSWORD; then
			fail "the hotserve uid can read the repository password from the environment of backup restic --"
		else
			pass "the hotserve uid cannot read the environment of backup restic --"
		fi
	else
		fail "backup restic -- never started a restic"
	fi
	wait "$held_pid"
fi

if section unvouched "a snapshot no clean run vouches for"; then
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
	# Asked for by id, it is what is restored — and said to be unvouched
	# before anyone confirms, since --delete would remove what it lacks.
	unvouched=$(hotserve backup restic -- snapshots --json --tag hotserve,app:backup-example 2>/dev/null \
		| tr ',' '\n' | grep -o '"short_id":"[^"]*"' | tail -1 | cut -d'"' -f4)
	asked=$(hotserve backup restore backup-example --admin 127.0.0.1:2019 --snapshot "$unvouched" --delete </dev/null 2>&1 || true)
	# The listing an operator chooses from says which is which, as
	# restic's own cannot.
	listed=$(hotserve backup snapshots backup-example 2>&1)
	if echo "$listed" | grep -E "^$unvouched " | grep -q "no — it may be missing files" && echo "$listed" | grep -E "^$snap " | grep -q "yes\$"; then
		pass "backup snapshots marks the snapshot no clean run vouches for, and the clean ones"
	else
		fail "backup snapshots: $listed"
	fi
	if echo "$asked" | grep -q "No clean run vouches for snapshot $unvouched"; then
		pass "a snapshot asked for by id is said to be unvouched before the operator confirms"
	else
		fail "no warning for --snapshot $unvouched --delete: $asked"
	fi
fi

if section another-repository "another repository"; then
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
fi

if section failing-backup "a backup that fails says so, and costs nobody else theirs"; then
	# A file the job cannot read. restic backs up everything else, WRITES A
	# SNAPSHOT, and exits 3 — the case the clean-run records exist for.
	echo secret > "$SHARED/uploads/unreadable"
	chown root:root "$SHARED/uploads/unreadable"
	chmod 000 "$SHARED/uploads/unreadable"
	clean_before=$(count_of hotserve-clean,clean-app:backup-example)
	snaps_before=$(count_of hotserve,app:backup-example)
	t_fail=$(date +%s)
	sleep 1
	if hotserve backup run --admin 127.0.0.1:2019 >/tmp/run-fail.log 2>&1; then
		fail "a run in which one app's backup failed exited 0: $(tail -5 /tmp/run-fail.log)"
	else
		pass "a run in which one app's backup failed exits non-zero"
	fi
	if grep -q 'backup-example: FAILED' /tmp/run-fail.log && grep -q 'files-example: ok' /tmp/run-fail.log; then
		pass "the failing app is named, and the other app still got its backup"
	else
		fail "after a failing job: $(grep -E ': (ok|FAILED)' /tmp/run-fail.log | tr '\n' ' ')"
	fi
	if journalctl --no-pager -u hotserve-backup-backup-example.service --since "@$t_fail" | grep -q 'restic: .*uploads/unreadable: permission denied'; then
		pass "the journal says, in restic's words, which file it could not read"
	else
		fail "restic's error is not in the journal: $(journalctl --no-pager -u hotserve-backup-backup-example.service --since "@$t_fail" | tail -6)"
	fi
	if [ "$(count_of hotserve,app:backup-example)" -eq $((snaps_before + 1)) ] && [ "$(count_of hotserve-clean,clean-app:backup-example)" -eq "$clean_before" ]; then
		pass "restic left a snapshot of the failed run, and no clean-run record vouches for it"
	else
		fail "after a failed run: snapshots $snaps_before → $(count_of hotserve,app:backup-example), clean records $clean_before → $(count_of hotserve-clean,clean-app:backup-example)"
	fi
	picked=$(hotserve backup restore backup-example --admin 127.0.0.1:2019 </dev/null 2>&1 || true)
	if echo "$picked" | grep -q "is newer, but the run that took it did not finish cleanly"; then
		pass "restore passes over the failed run's snapshot, and says so"
	else
		fail "restore did not pass over the failed run's snapshot: $picked"
	fi
	rm -f "$SHARED/uploads/unreadable"
	if hotserve backup run --admin 127.0.0.1:2019 >/tmp/run-mend.log 2>&1 && [ "$(count_of hotserve-clean,clean-app:backup-example)" -eq $((clean_before + 1)) ]; then
		pass "with the file gone the next run is clean, and is recorded as one"
	else
		fail "the run after the fix: $(tail -5 /tmp/run-mend.log)"
	fi
fi

if section missing-path "a declared path that is not there: not yet, or not any more"; then
	# What this box has backed up before is read from the repository — its
	# newest clean run's snapshot — and only when a declared path is missing.
	# files-example declares `drafts`, which nothing has ever created.
	status_paths=$(hotserve backup status --check --admin 127.0.0.1:2019 2>&1)
	paths_rc=$?
	if [ "$paths_rc" -eq 0 ] && echo "$status_paths" | grep -q "drafts has not existed at any backup yet"; then
		pass "a declared path the app has not made yet is skipped and named by status, and --check still passes"
	else
		fail "a never-created path (exit $paths_rc): $status_paths"
	fi
	# `pages` has been backed up. Gone, it must fail that app's run rather
	# than drop out of every backup from now on.
	mv "$FILES_SHARED/pages" "$FILES_SHARED/pages.aside"
	t_gone=$(date +%s)
	sleep 1
	if hotserve backup run --admin 127.0.0.1:2019 >/tmp/run-gone.log 2>&1; then
		fail "a run in which a backed-up path had vanished exited 0: $(tail -5 /tmp/run-gone.log)"
	elif grep -q 'files-example: FAILED' /tmp/run-gone.log && grep -q 'backup-example: ok' /tmp/run-gone.log \
		&& journalctl --no-pager -u hotserve-backup-files-example.service --since "@$t_gone" | grep -q "pages was backed up before and is not there now"; then
		pass "a declared path that was backed up and is gone fails that app's run, and says which path"
	else
		fail "a vanished path: $(grep -E ': (ok|FAILED)' /tmp/run-gone.log | tr '\n' ' ') / $(journalctl --no-pager -u hotserve-backup-files-example.service --since "@$t_gone" | tail -3)"
	fi
	mv "$FILES_SHARED/pages.aside" "$FILES_SHARED/pages"
	if hotserve backup run --admin 127.0.0.1:2019 >/tmp/run-back.log 2>&1 && grep -q 'files-example: ok' /tmp/run-back.log; then
		pass "with the path back the next run is clean"
	else
		fail "the run after the path came back: $(tail -5 /tmp/run-back.log)"
	fi
fi

if section staging "the launcher is root, and makes nothing under the staging root"; then
	# systemd makes each job's dir (StateDirectory=): the job's user's, in a
	# staging root that stays root's. restic's cache is in it, made by the
	# job itself.
	if [ "$(stat -c '%U %a' /var/lib/hotserve-backup)" = "root 750" ] && [ "$(stat -c '%U %a' "$STAGE")" = "hotserve 750" ]; then
		pass "each job's dir is the hotserve user's, made by systemd, in a staging root that is root's"
	else
		fail "staging: root $(stat -c '%U %a' /var/lib/hotserve-backup), app $(stat -c '%U %a' "$STAGE" 2>&1)"
	fi
	if journalctl --no-pager -u hotserve-backup-backup-example.service -u hotserve-backup-files-example.service | grep -q "unable to open cache"; then
		fail "a job ran without restic's cache: $(journalctl --no-pager -u hotserve-backup-backup-example.service | grep 'unable to open cache' | tail -1)"
	else
		pass "every job had restic's cache, in its own dir"
	fi
	# A link where a job's dir belongs is the one thing a job could leave
	# for a later run, as root, to follow. systemd does not start that unit,
	# and what the link points at is untouched; the other app is backed up.
	mkdir -p /srv/planted
	mv /var/lib/hotserve-backup/files-example /var/lib/hotserve-backup/files-example.real
	ln -s /srv/planted /var/lib/hotserve-backup/files-example
	if hotserve backup run --admin 127.0.0.1:2019 >/tmp/run-link.log 2>&1; then
		fail "a run whose job dir is a link exited 0: $(tail -5 /tmp/run-link.log)"
	elif grep -q 'files-example: FAILED' /tmp/run-link.log && grep -q 'backup-example: ok' /tmp/run-link.log \
		&& [ "$(stat -c '%U' /srv/planted)" = root ] && [ -z "$(ls -A /srv/planted)" ]; then
		pass "a link where a job's dir belongs fails that app only, and nothing is made or chowned through it"
	else
		fail "a planted link: $(grep -E ': (ok|FAILED)' /tmp/run-link.log | tr '\n' ' ') / /srv/planted is $(stat -c '%U' /srv/planted) holding '$(ls -A /srv/planted)'"
	fi
	rm -f /var/lib/hotserve-backup/files-example
	mv /var/lib/hotserve-backup/files-example.real /var/lib/hotserve-backup/files-example
	rmdir /srv/planted
fi

if section damaged-copy "a snapshot whose database copy is damaged restores nothing"; then
	# A real copy of the database with a page of noise in it, backed up by
	# hand where the job puts its copies, so sqlite3's own integrity check —
	# not a stand-in for it — is what refuses it. The staging root is
	# root's; it is opened to the backup user for as long as this takes.
	chmod 751 /var/lib/hotserve-backup
	as_hotserve "mkdir -p '$STAGE/data'; rm -f '$STAGE/data/app.db'; sqlite3 -cmd '.timeout 5000' 'file:$SHARED/app.db?mode=ro' \"VACUUM INTO '$STAGE/data/app.db'\"; dd if=/dev/urandom of='$STAGE/data/app.db' bs=1 seek=4096 count=2048 conv=notrunc" >/dev/null 2>&1
	hotserve backup restic -- backup --quiet --tag hotserve --tag app:backup-example "$STAGE/data" "$SHARED/uploads" >/dev/null 2>&1
	chmod 750 /var/lib/hotserve-backup
	bad=$(hotserve backup restic -- snapshots --json --tag hotserve,app:backup-example 2>/dev/null \
		| tr ',' '\n' | grep -o '"short_id":"[^"]*"' | tail -1 | cut -d'"' -f4)
	sq_app 'delete from rows where n <= 10'
	as_hotserve "rm -f '$SHARED/uploads/cat.jpg'"
	damaged=$(hotserve backup restore backup-example --admin 127.0.0.1:2019 --snapshot "$bad" --yes 2>&1)
	damaged_rc=$?
	if [ "$damaged_rc" -ne 0 ] && echo "$damaged" | grep -q "fails SQLite's integrity check" && echo "$damaged" | grep -q "nothing was restored" \
		&& [ "$(first_row)" = 0 ] && [ ! -e "$SHARED/uploads/cat.jpg" ] && [ "$(sq_app 'pragma integrity_check')" = ok ]; then
		pass "a damaged database copy fails the restore on SQLite's integrity check, before anything is touched: not the database, not the files"
	else
		fail "restore from a damaged copy (exit $damaged_rc, first row $(first_row), $(ls "$SHARED/uploads" | tr '\n' ' ')): $damaged"
	fi
	# Put the data back, from the newest clean snapshot.
	mended=$(hotserve backup restore backup-example --admin 127.0.0.1:2019 --yes 2>&1)
	if [ "$(first_row)" = 1 ] && [ -f "$SHARED/uploads/cat.jpg" ]; then
		pass "the newest clean snapshot then restores both"
	else
		fail "the restore after the damaged one: $mended"
	fi
fi

if section rollback-journal "a rollback-journal database (SQLite's default mode)"; then
	# Everything above ran against a WAL database. Without WAL a reader and
	# a writer exclude each other, so the copy and the restore both have to
	# wait their turn with the app — and still come out whole.
	if rollback_journal_idle; then
		start_writer
		sleep 1
		if hotserve backup run --admin 127.0.0.1:2019 >/tmp/run-del.log 2>&1 && grep -q 'backup-example: ok' /tmp/run-del.log; then
			pass "a rollback-journal database is copied while the app writes to it"
		else
			fail "backup of a rollback-journal database: $(tail -5 /tmp/run-del.log)"
		fi
		sq_app 'delete from rows where n <= 10'
		del_restore=$(hotserve backup restore backup-example --admin 127.0.0.1:2019 --yes 2>&1)
		if echo "$del_restore" | grep -q "restored from snapshot" && [ "$(first_row)" = 1 ] && [ "$(sq_app 'pragma integrity_check')" = ok ] && [ "$(sq_app 'pragma journal_mode')" = delete ]; then
			pass "and restored while the app writes to it: rows back, intact, still a rollback-journal database"
		else
			fail "restore of a rollback-journal database (first row $(first_row), $(sq_app 'pragma journal_mode')): $del_restore"
		fi
		rows_mid=$(rows_now)
		sleep 2
		if [ "$(rows_now)" -gt "$rows_mid" ]; then
			pass "the app writes on after it ($rows_mid → $(rows_now) rows)"
		else
			fail "writes stopped after the restore: $rows_mid → $(rows_now)"
		fi
	else
		fail "could not put the database into rollback-journal mode: $(sq_app 'pragma journal_mode')"
	fi
fi

if section restore-waits "a restore waits for a backup that is running"; then
	rollback_journal_idle || fail "could not put the database into rollback-journal mode: $(sq_app 'pragma journal_mode')"
	hold
	hotserve backup run --admin 127.0.0.1:2019 >/tmp/run-held.log 2>&1 &
	run_pid=$!
	if wait_for unit_running; then
		# The unit held here is the copy: the one step with the app's data
		# writable. What systemd gave it, not what a log line says.
		# (Asked for one at a time: several at once, `show` leaves out the
		# ones that are empty, and an empty EnvironmentFiles is the point.)
		held=$(systemctl show "$unit" -p PrivateNetwork -p BindPaths -p ExecStart | tr '\n' ' ')
		if [ "$(systemctl show "$unit" -p PrivateNetwork --value)" = yes ] && systemctl show "$unit" -p BindPaths --value | grep -q "$SHARED" \
			&& [ -z "$(systemctl show "$unit" -p EnvironmentFiles --value)" ] && systemctl show "$unit" -p ExecStart --value | grep -q -- '--phase=stage'; then
			pass "the unit with an app's data writable has no network and no repository settings"
		else
			fail "the staging unit's sandbox: $held EnvironmentFiles=[$(systemctl show "$unit" -p EnvironmentFiles --value)]"
		fi
		if hotserve backup status --admin 127.0.0.1:2019 2>&1 | grep -q "backing up now"; then
			pass "status says a backup is running while one is"
		else
			fail "status does not show the running backup: $(hotserve backup status --admin 127.0.0.1:2019 2>&1)"
		fi
		busy=$(hotserve backup restore backup-example --admin 127.0.0.1:2019 --yes 2>&1)
		busy_rc=$?
		if [ "$busy_rc" -ne 0 ] && echo "$busy" | grep -q "is backing up right now"; then
			pass "a restore is refused while that app's backup is running, and says to wait"
		else
			fail "a restore during a backup (exit $busy_rc): $busy"
		fi
	else
		fail "the held backup never started: $(tail -5 /tmp/run-held.log)"
	fi
	release
	if wait "$run_pid" && grep -q 'backup-example: ok' /tmp/run-held.log; then
		pass "the backup it waited for then finished cleanly"
	else
		fail "the held backup: $(tail -5 /tmp/run-held.log)"
	fi
fi

if section run-stopped "a run that is stopped stops its job, and nothing else"; then
	# `systemctl stop hotserve-backup.service` reaches the run as SIGTERM.
	# The job it is waiting on is a unit of its own, which the run stops;
	# a unit with another app's name — an operator's restore of it, say —
	# is none of its business, stopped or not.
	rollback_journal_idle || fail "could not put the database into rollback-journal mode: $(sq_app 'pragma journal_mode')"
	hold
	systemd-run --quiet --collect --unit=hotserve-backup-files-example /bin/sleep 300
	hotserve backup run --admin 127.0.0.1:2019 >/tmp/run-stopped.log 2>&1 &
	run_pid=$!
	if wait_for unit_running; then
		kill -TERM "$run_pid"
		wait "$run_pid"
		stopped_rc=$?
		if [ "$stopped_rc" -ne 0 ] && grep -q "stopped: the job this run was waiting on was stopped with it" /tmp/run-stopped.log && wait_for unit_stopped; then
			pass "a run that is stopped stops the job it was waiting on, and says so"
		else
			fail "a stopped run (exit $stopped_rc, its job $(systemctl is-active "$unit")): $(tail -3 /tmp/run-stopped.log)"
		fi
		if [ "$(systemctl is-active hotserve-backup-files-example.service)" = active ]; then
			pass "and leaves alone a unit it did not start, whatever its name"
		else
			fail "the stopped run took another app's unit with it: an operator's restore would have been cut off"
		fi
	else
		kill "$run_pid" 2>/dev/null
		fail "the held backup never started: $(tail -5 /tmp/run-stopped.log)"
	fi
	systemctl stop hotserve-backup-files-example.service 2>/dev/null
	release
fi

if section ctrl-c "Ctrl-C stops a restore, and the unit with it"; then
	rollback_journal_idle || fail "could not put the database into rollback-journal mode: $(sq_app 'pragma journal_mode')"
	sq_app 'delete from rows where n <= 10'
	hold
	hotserve backup restore backup-example --admin 127.0.0.1:2019 --yes >/tmp/restore-int.log 2>&1 &
	restore_pid=$!
	# Interrupted once it is at the step that writes: waiting, here, for
	# the lock on the live database.
	if wait_for grep -q '\.restore ' /tmp/restore-int.log; then
		kill -INT "$restore_pid"
		wait "$restore_pid"
		int_rc=$?
		if [ "$int_rc" -ne 0 ] && grep -q "interrupted: the restore of backup-example was stopped" /tmp/restore-int.log; then
			pass "an interrupted restore exits non-zero and says what it leaves"
		else
			fail "interrupted restore (exit $int_rc): $(tail -5 /tmp/restore-int.log)"
		fi
		if wait_for unit_stopped; then
			pass "the restore's unit is stopped with it, not left writing with nobody watching"
		else
			fail "the restore unit outlived the interrupt: $(systemctl is-active "$unit")"
		fi
	else
		kill "$restore_pid" 2>/dev/null
		fail "the restore never reached the database step: $(tail -5 /tmp/restore-int.log)"
	fi
	release
	if [ "$(first_row)" = 0 ] && [ "$(sq_app 'pragma integrity_check')" = ok ]; then
		pass "the database is as it was before the interrupted restore, and intact"
	else
		fail "after an interrupted restore: first row $(first_row), $(sq_app 'pragma integrity_check' 2>&1)"
	fi
	# The rows back in place, for whatever runs next.
	hotserve backup restore backup-example --admin 127.0.0.1:2019 --yes >/dev/null 2>&1
fi

if section verify "the weekly check of the repository, and a backup, wait for each other"; then
	if hotserve backup verify >/tmp/verify.log 2>&1 && grep -q "repository verified" /tmp/verify.log \
		&& hotserve backup status --admin 127.0.0.1:2019 2>&1 | grep -q "^repository: checked .*nothing wrong"; then
		pass "verify checks the repository, records it there, and status reports it"
	else
		fail "verify: $(tail -4 /tmp/verify.log) / $(hotserve backup status --admin 127.0.0.1:2019 2>&1 | tail -2)"
	fi
	# restic's check wants the repository to itself, and restic waits for a
	# lock only when told to: without that, whichever of a check and a
	# backup starts second fails at once. A check first, behind a backup
	# held open for a few seconds.
	hotserve backup restic -- backup --quiet --stdin-from-command --stdin-filename e2e-held -- sleep 8 >/dev/null 2>&1 &
	held_pid=$!
	wait_for pgrep -u hotserve -x restic >/dev/null
	t_wait=$(date +%s)
	if hotserve backup verify >/tmp/verify-wait.log 2>&1 && [ $(($(date +%s) - t_wait)) -ge 4 ]; then
		pass "a check that starts during a backup waits for it, and then passes"
	else
		fail "a check during a backup (after $(($(date +%s) - t_wait))s): $(tail -4 /tmp/verify-wait.log)"
	fi
	wait "$held_pid"
	# Then a backup, behind a check made slow enough to be in its way: a
	# few tens of megabytes to read back, at a few megabytes a second.
	# files-example has no copy step, so its unit running IS restic
	# running: with the check still alive at that moment the two overlap,
	# and a backup that did not wait would have failed at once (exit 11).
	head -c 45000000 /dev/urandom >/tmp/e2e-big
	chown hotserve /tmp/e2e-big
	hotserve backup restic -- backup --quiet /tmp/e2e-big >/dev/null 2>&1
	hotserve backup restic -- check --read-data --limit-download 3000 >/dev/null 2>&1 &
	check_pid=$!
	sleep 3
	hotserve backup run files-example --admin 127.0.0.1:2019 >/tmp/run-locked.log 2>&1 &
	locked_pid=$!
	files_unit_running() { case "$(systemctl is-active hotserve-backup-files-example.service)" in active | activating) return 0 ;; esac; return 1; }
	if wait_for files_unit_running && kill -0 "$check_pid" 2>/dev/null; then
		if wait "$locked_pid" && grep -q 'files-example: ok' /tmp/run-locked.log; then
			pass "a backup that starts during a check waits for the lock, and then succeeds"
		else
			fail "a backup during a check: $(tail -3 /tmp/run-locked.log) / $(journalctl --no-pager -u hotserve-backup-files-example.service -n 6 | tail -4)"
		fi
	else
		wait "$locked_pid"
		fail "setup: the backup and the check did not overlap, so nothing was shown about the lock"
	fi
	wait "$check_pid"
	rm -f /tmp/e2e-big
fi

if section retention "the retention docs/backups.md suggests keeps every app's clean runs"; then
	# `restic forget` applies its policy to each group of snapshots with
	# one host and one set of paths. Were every app's clean-run record
	# named alike they would be one group, and the hour's last record —
	# one app's — the only one kept: the other app's newest snapshot would
	# be left with nothing to vouch for it. (This box's key can delete; on
	# a real one this runs off the box. --prune is left out: it frees
	# space, and decides nothing.) It takes two runs' records to show:
	# the whole suite has many; on its own, ONLY=second-run,retention.
	if hotserve backup restic -- forget --keep-hourly 24 --keep-daily 30 --keep-monthly 12 >/tmp/forget.log 2>&1; then
		picked=$(hotserve backup restore backup-example --admin 127.0.0.1:2019 </dev/null 2>&1 || true)
		if echo "$picked" | grep -q "Restoring backup-example from snapshot" && ! echo "$picked" | grep -q "did not finish cleanly" \
			&& hotserve backup status --check --admin 127.0.0.1:2019 >/tmp/status-forget.log 2>&1; then
			pass "after the documented forget, each app's newest snapshot is still vouched for, and --check passes"
		else
			fail "after the documented forget: $picked / $(cat /tmp/status-forget.log 2>/dev/null)"
		fi
	else
		fail "the documented forget failed: $(tail -5 /tmp/forget.log)"
	fi
fi

if section init-tty "init at a terminal: one command, and it asks"; then
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
	# The box is on a repository nothing has been backed up to. An app
	# whose data dir is not there has, as far as this repository knows,
	# never been deployed: its job says so, the run passes over it, and
	# --check does not count it.
	mv /var/lib/liveswap/files-example /var/lib/liveswap/files-example.aside
	if hotserve backup run --admin 127.0.0.1:2019 >/tmp/run-never.log 2>&1 \
		&& grep -q 'files-example: nothing to back up yet, skipping' /tmp/run-never.log \
		&& journalctl --no-pager -u hotserve-backup-files-example.service -n 12 | grep -q 'it is not a failure' \
		&& ! grep -q 'files-example: ok' /tmp/run-never.log && grep -q 'backup-example: ok' /tmp/run-never.log; then
		pass "an app never backed up and with no data yet is skipped, on its job's word, and its journal says the status systemd calls a failure is not one"
	else
		fail "a run over an app with no data: $(tail -5 /tmp/run-never.log)"
	fi
	never_out=$(hotserve backup status --check --admin 127.0.0.1:2019 2>&1)
	never_rc=$?
	if [ "$never_rc" -eq 0 ] && echo "$never_out" | grep -qE '^files-example .*nothing to back up yet'; then
		pass "status names an app with nothing to back up yet, and --check does not count it"
	else
		fail "status over an app with nothing to back up (exit $never_rc): $never_out"
	fi
	mv /var/lib/liveswap/files-example.aside /var/lib/liveswap/files-example
fi

echo "=== summary ==="
pkill -f 'INSERT INTO rows' 2>/dev/null
suite="backup suite"
if [ -n "${ONLY:-}" ]; then
	suite="backup suite (ONLY=$ONLY, not the whole of it)"
fi
if [ "$failures" -eq 0 ]; then
	echo "$suite: all checks passed"
	exit 0
fi
echo "$suite: $failures check(s) failed"
journalctl --no-pager -u hotserve-backup-backup-example.service -n 30 | tail -15
exit 1
