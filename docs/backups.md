# Backups

Your box keeps two things that cannot be rebuilt from git: each app's
data, and the certificates. Certificates are reissued automatically, so
this page is about the data — the SQLite databases and uploaded files
under each app's `shared/` dir.

Setting it up is two steps: one command per box, and two lines per app.

```
sudo hotserve backup init s3:s3.us-west-004.backblazeb2.com/my-bucket \
	AWS_ACCESS_KEY_ID=0045f8… AWS_SECRET_ACCESS_KEY=…
```

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

Every `restic` and `sqlite3` command is printed as it runs, so anything
here can be reproduced by hand:

```
journalctl -u hotserve-backup.service -n 50     # what the run decided
journalctl -u hotserve-backup-blog.service      # one app's job
```

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

# the same repository, and the password you saved when you first set it
# up — in a file, because sudo does not carry environment variables
printf '%s' 'correct-horse-battery-staple-42' | sudo tee /root/restic-password >/dev/null
sudo chmod 0600 /root/restic-password
sudo hotserve backup init s3:… AWS_ACCESS_KEY_ID=… AWS_SECRET_ACCESS_KEY=… \
	--password-file /root/restic-password
sudo rm /root/restic-password

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
sudo hotserve backup run          # run it now, don't wait for the timer
systemctl list-timers hotserve-backup.timer
sudo hotserve backup restic -- snapshots --tag hotserve
```

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
