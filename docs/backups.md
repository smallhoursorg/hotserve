# Backups

Your box keeps two things that cannot be rebuilt from git: each app's
data, and the certificates. Certificates are reissued automatically, so
this page is about the data — the SQLite databases and uploaded files
under each app's `shared/` dir.

Setting it up is two steps: one command per box, and two lines per app.

```
sudo hotserve backup init s3:s3.us-west-004.backblazeb2.com/my-bucket
```

`init` asks for the storage key: the access key ID, then the secret,
which is not shown as you type it. (On Backblaze B2 those are the
application key's `keyID` and `applicationKey`.) Typed at a prompt, the
key is in no command line — readable by every user on the box through
`/proc/*/cmdline` while it runs — and in no shell history. `init` then
creates the repository, checks the key, and keeps everything in
`/etc/hotserve/backup.env` (root-only). It prints the repository's
password once: save it somewhere other than this box, because a restore
on a new one needs it and nothing can recover it.

For a script, give `init` the same things in root-only files instead:
`--credentials-file` with `KEY=VALUE` lines, and `--password-file` for
a repository that already exists. Without a terminal it asks nothing.

```
app blog {
	command ./server
	env DATA_DIR {shared_dir}
	state sqlite app.db        # the database
	state files uploads        # what people upload
}
```

Reload, and that app is backed up hourly from then on. A second app is
the same two lines — nothing to enable, no second config file.

## What is backed up, and what is not

| | |
|---|---|
| Each app's declared `state` | **yes** — the whole point |
| `releases/` | no: redeploy instead, the artifacts are in your CI |
| The Caddyfile | no: [keep it in git](after-first-deploy.md) |
| `/etc/hotserve/*.env` (your apps' secrets) | no: keep them in a password manager |
| TLS certificates | no: they are reissued on the new box |

Backups are per app: what one app declares is copied by a job that can
see that app's `shared/` dir and nothing else.

**Declare the data, not a link to it.** restic stores a symlink as a
symlink, so `state files uploads` where `uploads` is a link somewhere else
would back up the link and none of the files. A job refuses that rather
than reporting success over nothing. (To keep an app's data on another
disk, bind-mount that disk at the app's `shared/` directory, as
[liveswap's Sandbox section](../liveswap/README.md#sandbox) describes.) A
symlink *inside* a declared directory is stored the same way — its target
is not followed.

**A declared path that disappears fails the backup.** One the app has not
created yet is skipped with a note — a new app declares where its uploads
will go before anyone has uploaded anything. But a path that has been
backed up before and is gone now fails the run, so the app stops counting
as backed up instead of staying green while that path is never copied
again. If the app no longer keeps a path, remove its `state files` line.

What this cannot catch: a data disk bind-mounted at `shared/` that fails
to mount. The app starts on the empty directory underneath and writes new
data there within seconds, and that new data then backs up like any other.
Your earlier snapshots are untouched, but nothing here will tell you the
disk is missing — watch for that at boot the way you would for any mount.

A restore needs the same repository and the same password, so keep the
password somewhere other than the box — a password manager, not a note
on the server you are restoring.

## How it runs

An hourly timer (`hotserve-backup.timer`) asks hotserve which apps
declare state, then runs one short-lived job per app, in turn. Each job:

1. copies every declared database with SQLite's `VACUUM INTO` into a
   staging dir — a live database file cannot be copied safely any other
   way, and this is the step that makes the backup restorable;
2. hands restic the copies and the declared file paths, as one snapshot
   tagged with the app's name — and with a `run:` tag of its own, which
   is how the job finds that snapshot again to read it back;
3. exits. Nothing stays running between backups.

Each job runs as the `hotserve` user inside a systemd sandbox whose
whole filesystem is a read-only empty root, with that one app's
`shared/` bound in and nothing else: not hotserve's own state, not
another app's data, not the credentials file (systemd reads that as
root and passes the values in).

What that does and does not buy you: on this box, a job can reach only
the app it is backing up. In the repository it can reach everything —
every job gets the same credentials, because there is one repository
for the box, so a compromised restic could read or delete another app's
snapshots there. Per-app isolation of the backups themselves would mean
a repository and a key for each app; that is a reasonable thing to want
and it is not what this does.

How that app's dir is bound depends on what the app declares:

- **Only `state files`** — bound **read-only**. That app's backup
  cannot write to its data at all.
- **Any `state sqlite`** — bound **writable**, because SQLite creates
  the `-shm` file next to a database to read a WAL database at all; on
  a read-only mount the copy fails with "unable to open database file".
  The job still only reads, as the same user that already owns the
  data, and the e2e suite checks the database is byte-identical after a
  backup.

A run counts only once its snapshot has been **read back** out of the
repository holding every declared path as data: each database as a
copy with something in it, each `files` path as the real thing, not a
link. A run that exits cleanly but whose snapshot is missing any of
that fails. A run that passes writes a small record into the
repository saying so — tagged `hotserve-clean`, a few hundred bytes,
and a write, which a key that cannot delete can still make. `status`
measures freshness from these records and `restore` chooses by them,
so both work from the repository alone: on a rebuilt box, and after
`init --force` onto another repository.

Every `restic` and `sqlite3` command is printed as it runs, so anything
here can be reproduced by hand:

```
journalctl -u hotserve-backup.service -n 50     # what the run decided
journalctl -u hotserve-backup-blog.service      # one app's job
```

### What `init` checks, and how

`init` runs its checks the way the hourly job will run: as a unit with
the job's own sandbox, as the `hotserve` user, with only the settings it
is about to write. Nothing from your shell is used — an `AWS_PROFILE`, a
proxy, a credentials file under your home, a `restic` earlier on your
`PATH`. If the checks pass, the job has everything it needs; if the
repository needs something more, pass it to `init` as `KEY=VALUE` or in
`--credentials-file`, and it goes into the settings the job gets.

### Where the repository can be

A repository is a restic backend URL — `s3:`, `b2:`, `rest:`, `azure:`,
`gs:` or `swift:` — and never a path on this box. `init`, the hourly
run and `restore` all refuse a path, with the same error. `sftp:` and
`rclone:` are refused too: ssh takes its key and `known_hosts` from
files, rclone its remotes from a config file, and a job's sandbox holds
only the settings `init` writes — every backend here takes its
credentials as settings. A backup on the box it protects does not survive losing the box,
and a repository the jobs could reach on disk would be mounted,
writable, into every one of them. Every job reaches the repository over
the network, and the sandbox binds nothing of it.

## Keep the box unable to delete

A backup a burglar can delete is not a backup. Whoever takes your box
gets the credentials in `/etc/hotserve/backup.env`, so those credentials
should be able to **add** backups and not to remove them.

On Backblaze B2, create an application key restricted to the bucket
(and, if several apps share it, the prefix) with `listBuckets`,
`listFiles`, `readFiles` and `writeFiles` — and **not** `deleteFiles`.
That is the set restic needs, and it never requires `deleteFiles`
(restic's append-only support for B2 is built on exactly that:
[restic#2398](https://github.com/restic/restic/pull/2398)).

**What that key can and cannot do, precisely.** On B2, removing a file
by name only *hides* it — the previous version stays and needs
`deleteFiles` to destroy. So this key can clear restic's own lock
files (which it must), and someone who takes your box can make the
repository look empty, but they cannot erase what is in it: the
versions are still there to restore. Keep lifecycle rules that retain
old versions, or that protection expires on a timer of your own
making.

Other providers express this differently — a bucket policy, a key
scoped to write-only — and some cannot at all. Whatever you set up,
you do not have to take it on trust:

`hotserve backup init` checks it, and says which you have:

```
delete refused by the storage: the box can add backups but not remove them
```

or

```
WARNING: these credentials can delete backups.
```

It finds out by actually trying: it writes a probe snapshot and asks
restic to remove it. When the key cannot delete, the probe snapshot
stays in the repository — a few hundred bytes, tagged
`hotserve-delete-probe`.

That probe is also the proof that the key can back up at all, so
`init` installs `/etc/hotserve/backup.env` only after it succeeds. A
key that can read but not write therefore leaves nothing behind and
nothing scheduled: fix the key and run the same command again.

**The trade:** old snapshots then accumulate until you remove them
yourself, with a key that is allowed to. Do that from your laptop, not
from the box — with a retention policy of your choosing; for example,
keeping a day of hourly snapshots, a month of dailies and a year of
monthlies:

```
restic forget --keep-hourly 24 --keep-daily 30 --keep-monthly 12 --prune
```

A few times a year is enough.

**Not with a lifecycle rule.** Expiring objects on the bucket's own
schedule does not work here: restic deduplicates, so a pack a rule
judges old can still hold the only copy of a chunk that this morning's
snapshot needs, and removing it corrupts backups that are perfectly
current. Lifecycle rules have one job in this setup — keeping old
*versions* around, so that hiding cannot become destroying. Retention
of snapshots is `restic forget --prune`, run by a key that is allowed
to delete.

**Going further:** storage-level immutability (S3 Object Lock, in
governance or compliance mode) protects the backups even against
someone with your storage account. restic cannot use it — it must be
able to delete its own lock files — so that path means a different tool;
Kopia supports Object Lock directly.

## Restoring

```
sudo hotserve backup restore blog
```

puts back everything `blog` declares, from its newest clean snapshot. Before
it changes anything it says what it is about to do, and asks you to
type the app's name:

```
Restoring blog from snapshot a1b2c3d4, taken 2026-09-18 03:00 UTC (3 hours ago) on box-1:
  app.db               replaced by the snapshot's copy; the app's writes wait while it goes in
  uploads              files in the snapshot put back; files added since are kept
(A path the snapshot does not hold — the app had not created it yet — is left as it is.)
Type the app's name to restore it: blog
```

- **The app can keep running.** A database goes back through SQLite's
  own backup API, as one transaction, so the app sees the restored data
  as one ordinary commit and its `-wal` and `-shm` files stay consistent
  with it. Its writes wait while the copy goes in; an app that gives up
  on a busy database sooner than that will report errors for those
  moments. (An app that keeps data in memory serves what it had until it
  next reads the database.)
- **Nothing is replaced until the snapshot checks out.** Every declared
  path is looked up in the snapshot and every database copy has to pass
  SQLite's integrity check first; if any of that fails, nothing is
  touched. A declared path the snapshot does not hold at all — the app
  had not created it yet when it was taken — is left as it is.
- **The newest clean snapshot, by default.** restic keeps a snapshot
  even from a run that failed part-way, and such a snapshot can be
  missing files. restore takes the newest one a clean run recorded
  (from any box — a rebuilt box has a new name), and when a newer one
  exists without a record, it names it for `--snapshot` and says why it
  passed it over.
- **If it fails part-way** — a network error in the middle of a
  directory, say — that directory may be partly restored. Run the same
  command again to finish it. A database is either restored or left as
  it was.
- **Files added since the snapshot are kept.** `--delete` makes each
  declared files path exactly as it was, removing what was added.
- **An earlier moment:** `--snapshot <id>`, from
  `sudo hotserve backup restic -- snapshots --tag app:blog`.
- **In a script:** `--yes` skips the question. Without a terminal and
  without `--yes`, it refuses.

A restore runs where that app's backup runs — in its unit, as the
`hotserve` user, in the same sandbox — so a restore and that app's
hourly backup can never run at the same time.

### The box is gone

Install hotserve, put your Caddyfile back, point the box at the same
repository, restore each app, then deploy:

```
sudo apt install ./hotserve_*.deb
sudo cp Caddyfile /etc/hotserve/Caddyfile && sudo systemctl reload hotserve

sudo hotserve backup init s3:s3.us-west-004.backblazeb2.com/my-bucket
# asks for the storage key, then — the repository already exists — for
# the password you saved when you first set it up

sudo hotserve backup restore blog

./deploy.sh v4
```

Restore before the first deploy: the app then starts on its data
rather than on an empty directory. (The app's block has to be in the
Caddyfile first — a restore puts back what the block declares.)

A rebuilt box may give the `hotserve` user a different uid than the old
one had, and a snapshot records each file's owner by number. Restored
files belong to this box's `hotserve` user whatever number was recorded,
and the restore says how many that applied to:

```
blog: uploads: 1204 files and directories belong to this box's user, not the owner the snapshot records — …
```

### A rollback does not undo a migration

Rolling back code does not roll back data: `pre_start` migrations have
already run, and `shared/` is untouched by a rollback. If a deploy
migrated the database in a way you need to undo, roll the code back
**and** restore the data from the snapshot before that deploy.

## Check it is working

```
sudo hotserve backup status       # one line per app that declares state
sudo hotserve backup run          # run it now, don't wait for the timer
systemctl list-timers hotserve-backup.timer
sudo hotserve backup restic -- snapshots --tag hotserve
```

`status` answers the two questions worth asking — is everything covered,
and is it current:

```
APP   DECLARES               SNAPSHOTS  LAST BACKUP
blog  1 database, 1 path     37         12 min ago  a1b2c3d4
shop  1 path                 0          never  ⚠
```

For a monitor, `sudo hotserve backup status --check` prints the same
report and exits non-zero when any app has no current backup. It needs
root — the repository settings are root-only — so run it from root's
crontab or a root systemd timer.

Two things it will say that are worth knowing before you see them at
three in the morning:

- **`(no clean run on this box)`** — the repository holds snapshots for
  this app, but it has no record of a backup run from *this* machine
  (by hostname) finishing cleanly. restic writes a snapshot even when it
  exits part-way through, so a fresh snapshot on its own is not
  evidence that anything works. This is also what a rebuilt box with a
  new hostname shows until its first hourly run, and what a box shows
  right after `init --force` onto a repository it has not backed up to
  yet.
- **`(last clean run 3 days ago)`** beside a recent snapshot — runs since
  then have been failing. `journalctl -u hotserve-backup-<app>` says why.
- **`uploads has not existed at any backup yet`** — a declared path no
  run has found. Backups skip a path the app has not created yet (a new
  app's empty uploads dir is normal), but a typo in a `state files` line
  looks exactly the same, so the report names it until it appears. It
  does not make `--check` fail.

Freshness is measured from the last clean run's record in this
repository, not from the newest snapshot, so an app whose every run
fails part-way cannot look healthy.

Then do the thing almost nobody does: **restore once, on purpose,
before you need to.** Restore an app's newest snapshot into a scratch
directory and look at it.

```
sudo hotserve backup restic -- restore latest --tag app:blog --target /tmp/drill
sqlite3 /tmp/drill/var/lib/hotserve-backup/blog/data/app.db 'pragma integrity_check'
ls /tmp/drill/var/lib/liveswap/blog/shared/uploads
```

A backup nobody has restored is a hypothesis.

## Not covered: losing less than an hour

Backups are hourly, so a restore can lose up to an hour of writes.
Continuous replication of a database (Litestream, for example) is not
part of this, and nothing here sets it up.
