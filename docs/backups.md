# Backups

Your box keeps two things that cannot be rebuilt from git: each app's
data, and the certificates. Certificates are reissued automatically, so
this page is about the data — the SQLite databases and uploaded files
under each app's `shared/` dir.

Setting it up is two steps: one command per box, and two lines per app.

```
read -rp 'Key ID: ' key_id
read -rsp 'Application key: ' app_key; echo
sudo install -m 0600 /dev/null /root/b2-key
printf 'AWS_ACCESS_KEY_ID=%s\nAWS_SECRET_ACCESS_KEY=%s\n' "$key_id" "$app_key" \
	| sudo tee /root/b2-key >/dev/null
unset app_key
sudo hotserve backup init s3:s3.us-west-004.backblazeb2.com/my-bucket \
	--credentials-file /root/b2-key
sudo rm /root/b2-key
```

The key is typed at a prompt and handed over in a root-only file, so it
is never part of a command: a command line is readable by every user on
the box through `/proc/*/cmdline` while it runs, and your shell keeps it
in its history afterwards — heredocs included. (`printf` is built into
the shell, so the key never reaches another process's arguments.)
`init` copies what it reads into `/etc/hotserve/backup.env`
(root-only), which is where it stays.

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
symlink, so `state files uploads` where `uploads` is a link to a data
disk would back up the link and none of the files. A job refuses that
rather than reporting success over nothing; mount the disk at
`shared/uploads` instead. A symlink *inside* a declared directory is
stored the same way — its target is not followed.

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
   tagged with the app's name;
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
that fails, and never counts as a backup in `status`.

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

### A repository on a disk of its own

A repository can be a path on this box instead of a bucket — a second
disk, say. `init` accepts exactly two things there:

- **a path that does not exist yet**, under a directory that does:
  `init` creates it, owned by the backup user, and restic fills it;
- **an existing restic repository** — a rebuilt box pointed at the disk
  its backups are on.

It refuses anything else, **including an empty directory**. The
repository is given to the backup user and mounted writable into every
job, so a directory with anything else in it would be handed over with
it — and on Debian, `/var/backups` holds `shadow.bak`. Point `init` at a
new path inside a mount instead: `/mnt/disk/restic`, not `/mnt/disk`.
The hourly run checks the same thing before mounting the repository
into a job, so a disk that failed to mount fails the run with a message
saying the repository is not there, instead of mounting whatever
directory is left at that path into every job.

A disk in the same box protects against a failed disk or a mistake, not
against losing the box: keep a copy somewhere else too.

## Keep the box unable to delete

A backup a burglar can delete is not a backup. Whoever takes your box
gets the credentials in `/etc/hotserve/backup.env`, so those credentials
should be able to **add** backups and not to remove them.

On Backblaze B2, create an application key restricted to the bucket
(and, if several apps share it, the prefix) with `listFiles`,
`readFiles` and `writeFiles` — and **not** `deleteFiles`. That is the
set restic needs; it never requires `deleteFiles`.

**What that key can and cannot do, precisely.** On B2, removing a file
by name only *hides* it — the previous version stays and needs
`deleteFiles` to destroy. So this key can clear restic's own lock
files (which it must), and someone who takes your box can make the
repository look empty, but they cannot erase what is in it: the
versions are still there to restore. Keep lifecycle rules that retain
old versions, or that protection expires on a timer of your own
making.

On S3, deny `s3:DeleteObject` in the bucket policy, except under
`locks/`, which restic needs to write and clear its own lock files.

`hotserve backup init` checks this for you, and says which you have:

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
from the box:

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

**Where things are in a snapshot.** Files are stored under the path
they live at, so they restore straight back. A database is stored under
the path of the *copy* the backup took, because copying is the only way
to read a live database safely:

| In the app | In the snapshot |
|---|---|
| `/var/lib/liveswap/blog/shared/uploads/` | the same path |
| `/var/lib/liveswap/blog/shared/app.db` | `/var/lib/hotserve-backup/blog/data/app.db` |

So restoring files is one command, and a database is a restore followed
by a copy into place. Both are below.

### The box is gone

Install hotserve, point it at the same repository, restore, and deploy:

```
sudo apt install ./hotserve_*.deb

# the same repository, with the password you saved when you first set
# it up and a storage key — typed at prompts so neither is ever part of
# a command, and handed over in root-only files because sudo does not
# carry environment variables
read -rsp 'Repository password: ' restic_pw; echo
read -rp 'Key ID: ' key_id
read -rsp 'Application key: ' app_key; echo
sudo install -m 0600 /dev/null /root/restic-password
sudo install -m 0600 /dev/null /root/b2-key
printf '%s' "$restic_pw" | sudo tee /root/restic-password >/dev/null
printf 'AWS_ACCESS_KEY_ID=%s\nAWS_SECRET_ACCESS_KEY=%s\n' "$key_id" "$app_key" \
	| sudo tee /root/b2-key >/dev/null
unset restic_pw app_key
sudo hotserve backup init s3:… --credentials-file /root/b2-key \
	--password-file /root/restic-password
sudo rm /root/restic-password /root/b2-key

# files go back where they were; the database copy lands under /var/lib/hotserve-backup
sudo hotserve backup restic -- restore latest --tag app:blog --target /

# put the database where the app expects it
sudo install -o hotserve -g hotserve -m 0640 \
	/var/lib/hotserve-backup/blog/data/app.db \
	/var/lib/liveswap/blog/shared/app.db

./deploy.sh v4
```

The data is in place before the app starts, so the first deploy comes up
on it.

### Bad data on a live box

The app has to stop while its data is replaced, and stopping its process
is not enough — the watchdog restarts it. Remove the app's block from
the Caddyfile and reload; that stops its units and leaves its data
alone:

```
sudo -e /etc/hotserve/Caddyfile        # comment out the app's block
sudo systemctl reload hotserve         # its units stop

sudo hotserve backup restic -- restore <snapshot> --target /
sudo install -o hotserve -g hotserve -m 0640 \
	/var/lib/hotserve-backup/blog/data/app.db \
	/var/lib/liveswap/blog/shared/app.db
sudo rm -f /var/lib/liveswap/blog/shared/app.db-wal /var/lib/liveswap/blog/shared/app.db-shm

sudo -e /etc/hotserve/Caddyfile        # put the block back
sudo systemctl reload hotserve         # the app relaunches on the restored data
```

Pick the snapshot with `sudo hotserve backup restic -- snapshots --tag app:blog`, and restore a
particular moment with `--time`. The `-wal` and `-shm` files go because
they belong to the database you are replacing, not to the copy.

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
report and exits non-zero when any app has no current backup, so a cron
line or a Nagios-style check needs nothing else.

Two things it will say that are worth knowing before you see them at
three in the morning:

- **`(no clean run on this box)`** — the repository holds snapshots for
  this app, but no backup run on *this* machine has finished cleanly.
  restic writes a snapshot even when it exits part-way through, so a
  fresh snapshot on its own is not evidence that anything works. This is
  also what a rebuilt box shows until its first hourly run.
- **`(last clean run 3 days ago)`** beside a recent snapshot — runs since
  then have been failing. `journalctl -u hotserve-backup-<app>` says why.

Freshness is measured from the last clean run, not from the newest
snapshot, so an app whose every run fails part-way cannot look healthy.

Then do the thing almost nobody does: **restore once, on purpose,
before you need to.** Restore an app's newest snapshot into a scratch
directory and look at it.

```
sudo hotserve backup restic -- restore latest --tag app:blog --target /tmp/drill
sqlite3 /tmp/drill/var/lib/hotserve-backup/blog/data/app.db 'pragma integrity_check'
ls /tmp/drill/var/lib/liveswap/blog/shared/uploads
```

A backup nobody has restored is a hypothesis.

## If an app cannot lose an hour

Hourly is a deliberate default: a full copy of a database costs about a
megabyte an hour once restic deduplicates it, and a job that exits
cannot leak. If an app genuinely cannot lose an hour of writes, run
continuous replication for that app (Litestream) alongside these
backups — it must run as its own service, never wrapped around the app,
because a deploy briefly runs two versions at once and two replicators
writing one destination corrupt it.
