# Backups

The package ships `hotserve-backup`: an hourly backup of what each
app's Caddyfile block declares — its SQLite databases, copied safely
while the app runs, and the files under its `shared/` — into one
[restic](https://restic.net) repository off the box, and a weekly drill
that restores every app's newest snapshot to prove a restore would
work. This page is how to set it up, check on it, restore, rebuild a
box, keep the repository small, and recover from an attack. How it
works, and everything it refuses, is
[backups/README.md](../backups/README.md).

Every transcript below was observed on a fresh Debian 13 box with the
package installed: against the e2e suites' S3 server over TLS, and,
where a bucket's versions matter, against MinIO standing in for a
provider. On your box the URL, the ids, the times and the password
differ. On Backblaze B2 the release candidate `v0.3.0-rc1` then ran the
whole of it (2026-10-04), and what B2 showed is said where it bears. On
AWS S3 and Hetzner nothing has been run: what depends on them says
**unverified**, and [the last section](#against-a-real-provider) lists
it all.

## Who runs these

The lines below under `sudo` are root's, but for
`sudo systemctl reload hotserve`. Run them at the provider's console,
or as an administrator whose `sudo` is root's (Debian's `sudo` group).
The administrator [After the first deploy](after-first-deploy.md)
creates may reload hotserve and no more: their `sudoers` grants the
`hotserve` user's reach and not root's, and `setup` writes the one
credential that reads every backup ever made. That administrator — or
an agent working as one — runs `hotserve-backup validate` and
`hotserve-backup status`, reads the journal, and pushes the Caddyfile.
Keep `setup` for a person: it shows the repository password on the
terminal it runs in.

## Before you start: the bucket, and what it protects

A backup that an attacker can delete is no backup. The bucket below
keeps every version of every file, and neither key you make for it can
destroy one: what a key deletes or overwrites stays an old version for
180 days, and only the bucket's own rule removes it after that. So
**nothing the box or your own machine holds can destroy a backup, and a
deletion or overwrite made with either key can be undone for 180 days —
if someone [looks from off the box](#look-from-your-own-machine) at
least once every 90 days**, which leaves another 90 to recover; whoever
has the box can make its own `status` say anything. A stolen login to the provider account can destroy
everything: give it a second factor, and never put its keys on the
box. On MinIO standing in for S3, an attack made with the box's own key
was refused every destruction and [undone](#after-an-attack), and on B2
the same; on AWS S3 and Hetzner it is **unverified**.

- **A bucket of its own**, one per box: on Backblaze B2 or S3 (`setup`
  asks for those two; `rest:`, `gs:` and `swift:` are written
  [by hand](../backups/README.md#by-hand)). Versioning on, and one
  lifecycle rule: old (noncurrent) versions are removed after 180 days.
  Never a rule that expires current objects by age: a pack judged old
  can hold the only copy of data every backup uses.
- **Two keys**, neither able to destroy a version: the box's, which
  `setup` asks for, and your own machine's, for
  [retention](#retention-off-the-box) and the look. Both need to
  *delete*: restic replaces its lock file every five minutes, and with
  a key that may delete nothing, a command still running at 22 minutes
  was stopped [measured]. On a versioned bucket a delete only hides a
  file.
- **Somewhere off the box** for the repository URL, the box's key, and
  the password `setup` shows once. With those, `restic` reads every
  backup from any machine; without the password nothing can.

**On B2**, the easiest: every bucket keeps versions. Give each key the
bucket alone and the capabilities `listBuckets`, `listFiles`,
`readFiles` and `writeFiles` — **not `deleteFiles`**, with which restic
destroys every version of what it removes. Without it B2 refuses the
delete and restic hides the file instead: a command that held its lock
for 30 minutes ended clean, no lock left [measured]. The web console
offers three presets and no single capabilities — Read and Write, which
may delete, Read Only and Write Only (its documentation) — so make the
two keys once from the command line, with Debian 13's `backblaze-b2`
(3.19; a `debian:13` container will do):

```sh
apt-get update && apt-get install -y backblaze-b2
backblaze-b2 authorize-account     # asks for the master key's id, then the key
backblaze-b2 create-key --bucket my-backups hotserve-box listBuckets,listFiles,readFiles,writeFiles
backblaze-b2 create-key --bucket my-backups hotserve-laptop listBuckets,listFiles,readFiles,writeFiles
```

Each `create-key` prints the new key's id and the key itself, once.
The master key has now been typed at a terminal: generate a new one in
the console, which ends the old. In the bucket's Lifecycle Settings,
"Keep prior versions for this number of days": 180. B2 then removes a
hide marker itself once the versions under it are gone (its
documentation; **unverified**).

**On AWS S3**, turn Versioning on, add a lifecycle rule that
permanently deletes noncurrent versions after 180 days and deletes
expired object delete markers — restic leaves about two markers a
command, and S3 keeps each one after its versions are gone, unless told
— and make each key a user of its own with this policy (`my-backups`
is the bucket):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    { "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"],
      "Resource": "arn:aws:s3:::my-backups/*" },
    { "Effect": "Allow",
      "Action": ["s3:ListBucket", "s3:GetBucketLocation"],
      "Resource": "arn:aws:s3:::my-backups" },
    { "Effect": "Deny",
      "Action": ["s3:DeleteObjectVersion", "s3:PutBucketVersioning",
                 "s3:PutLifecycleConfiguration", "s3:PutBucketPolicy",
                 "s3:DeleteBucketPolicy", "s3:DeleteBucket"],
      "Resource": ["arn:aws:s3:::my-backups", "arn:aws:s3:::my-backups/*"] }
  ]
}
```

On MinIO that policy let restic do all it does and refused to destroy
a version, suspend versioning, change the lifecycle rule or remove the
bucket. **On Hetzner** versioning and the rule for old versions are
there, but not the one for delete markers, which pile up (restic lists
only current files, so it does not mind); and every key reaches every
bucket of its project, so the same denials have to be a bucket policy
naming each key — not worked out here.

Object Lock is not part of this. A bucket's default retention locks
each file for a period counted from its upload, and restic keeps the
files of a first backup in use for years: the data every backup shares
is the first to fall out of the lock.

## Set it up

As root, or an administrator whose `sudo` is root's:

<!-- smoke: begin -->
```sh
# 1. The repository, once: the storage key is asked for at the terminal,
#    and a new repository's password is shown once, to store elsewhere.
sudo hotserve-backup setup s3:https://s3.example.com/my-backups
# 2. In the app's block of the Caddyfile, what to back up — in the box
#    repo, pushed with make push, which runs the next line on the box
#    (examples/box); on a box without one, in /etc/hotserve/Caddyfile:
#        backup {
#            sqlite app.db
#            files  uploads
#        }
#    then, before it goes live, whether a run could plan from it:
hotserve-backup validate /etc/hotserve/Caddyfile
sudo systemctl reload hotserve
# 3. The first backup runs within the hour and ten minutes
#    (systemctl list-timers hotserve-backup.timer), or now:
sudo systemctl start hotserve-backup.service
hotserve-backup status
# 4. A restore: into a directory of root's own to look at, or into
#    place — which backs the app up first, tagged pre-restore, and asks.
sudo hotserve-backup restore demo --to /root/demo-restored
sudo hotserve-backup restore demo
```
<!-- smoke: end -->

The package's smoke test (`make install-test`) runs exactly those
lines, read out of this page, as an administrator under `sudo`, on a
fresh Debian 13 with the `.deb` just installed. Keep the block in the
box repo's Caddyfile: a `make push` of a file without it stops that
app's backups, and a run says so once. On a box without a box repo,
keep a copy of `/etc/hotserve` off the box
([A rebuilt box](#a-rebuilt-box)). The other order works too: with the
block in place first, `setup` names the apps a run would back up.

`setup`, for a bucket nobody has made yet:

```
$ sudo hotserve-backup setup s3:https://s3.example.com/my-backups
account hotserve-backup: present
no app declares a backup yet: a run would back nothing up
Storage key id (AWS_ACCESS_KEY_ID): …
Storage secret key (AWS_SECRET_ACCESS_KEY):
looking for a repository at s3:https://s3.example.com/my-backups (up to 10s)
no repository answered within 10s: a bucket not made yet, a wrong key and a wrong host look alike here; a new repository will be made with the password shown next
Repository password (new): <52 characters>
Store it off the box now, with the repository URL, s3:https://s3.example.com/my-backups, and the storage key: with those three, `restic -r <url>` reads every backup from any machine; without the password nothing can.
Type stored to go on: stored
repository ready: s3:https://s3.example.com/my-backups (new, id cff1d334)
credentials: /etc/hotserve-backup/repository.env (root, 0600)
next: declare a backup in an app's Caddyfile block (liveswap/README.md); the hourly timer backs it up from then on, or sudo systemctl start hotserve-backup.service runs one now
```

On B2, with the bucket made and a right key, the look is answered at
once and the `no repository answered` line does not come. A wrong key
is asked for again, up to three times, the password shown standing — on
B2 each asking was refused at once (`401`), three in 3.8 s; a setup
that ends early says whether to keep that password or discard it
([backups/README.md](../backups/README.md#setup)). Then
[look from your own machine](#look-from-your-own-machine) with the URL,
the key and the password as you stored them: a password copied wrong is
otherwise found only when the box is gone.

## Is it working

`hotserve-backup status`, as anyone, exits 0 when all is well — every
app backed up within 3 hours, a restore of it proven within 8 days, the
repository's last check clean ([the whole rule](../backups/README.md#status))
— 1 when not, and 3 when it could not look. Right after `setup` it is
`no run yet: pending first run`, exit 0; a run before any block is
declared makes that `no app declares a backup; nothing is backed up`,
exit 0 still. After the timer's first run with the block in place:

```
$ hotserve-backup status
last run 2026-10-03 07:01 UTC
the repository has not been checked yet: the weekly drill checks it
demo: ok: last complete backup 2026-10-03 07:01 UTC, snapshot c4f0d7c1 (data at /var/lib/liveswap/demo/shared)
demo: restore last proven: 2026-10-03 07:01 UTC, snapshot c4f0d7c1
no unit of a run, a restore or a drill is running
```

An app's first good backup, up to 1 GiB restored, is drilled by the
run that made it, as here. A larger one is left to the weekly drill,
and until then `status` says `restore not proven` and exits 1:
`sudo hotserve-backup drill` proves it now. A run that fails is a
failed unit until the next one succeeds — `systemctl --failed` lists
`hotserve-backup.service` — and `journalctl -u hotserve-backup.service`
(the `adm` group reads it) has one line per app:

```
hotserve-backup[9377]: demo: ok: snapshot c4f0d7c1 holds sqlite app.db, files uploads
hotserve-backup[9377]: demo: restore proven: snapshot c4f0d7c1 was fetched and checked whole
```

What every line of `status` means is under
[Status](../backups/README.md#status).

### Look from your own machine

The box's `status` is only as honest as the box. Right after `setup`,
and at least once in every 90 days — half the time old versions are
kept, so that the other half is left to recover — look from a machine
of your own with Debian's restic (0.18), in `bash`, with your own
machine's key and the repository's password (in a `debian:13`
container, `apt-get install -y restic ca-certificates`: restic alone
brings no CA certificates, and fails `x509: certificate signed by
unknown authority` [measured]): `restic snapshots` for
whether the backups are fresh, and `restic check --read-data` for
whether anything under them was hidden, overwritten or damaged, which a
fresh-looking listing does not show.

```bash
export RESTIC_REPOSITORY=s3:https://s3.example.com/my-backups
read -r AWS_ACCESS_KEY_ID && export AWS_ACCESS_KEY_ID          # your own machine's key
read -rs AWS_SECRET_ACCESS_KEY && export AWS_SECRET_ACCESS_KEY
# restic asks for the repository's password, for each.
restic snapshots --latest 1 --tag hotserve --group-by host,paths,tags
restic check --no-lock --read-data
```

On B2 the two key lines are `B2_ACCOUNT_ID` and `B2_ACCOUNT_KEY`. The
newest backup of each app should be within the hour; a snapshot a
restore made of what it restored over (tagged `pre-restore`) is listed
apart, and is not a backup:

```
snapshots for (host [hotserve], tags [app:demo, hotserve], paths [/backup/demo]):
ID        Time                 Host        Tags               Paths         Size
---------------------------------------------------------------------------------------
66724c5e  2026-10-03 11:48:35  hotserve    app:demo,hotserve  /backup/demo  400.008 MiB
---------------------------------------------------------------------------------------
1 snapshots
snapshots for (host [hotserve], tags [app:demo, hotserve, pre-restore], paths [/backup/demo]):
ID        Time                 Host        Tags                           Paths         Size
---------------------------------------------------------------------------------------------------
6efa7a46  2026-10-03 08:48:42  hotserve    app:demo,hotserve,pre-restore  /backup/demo  400.008 MiB
---------------------------------------------------------------------------------------------------
1 snapshots
```

The check should end `no errors were found`. It reads the whole
repository back: the repository's size in egress each time (S3: $0.09/GB
past the free 100 GB a month; B2: free up to three times what is
stored; Hetzner: within its 1 TB). `--no-lock` keeps the box's runs
from waiting on it; run it when you are not pruning, which could make
it report damage that is not there. `Fatal: wrong password or no key
found`, exit 12, after the third asking, is a password typed wrong — or,
where the stored password is right, key files hidden by an attack
[measured]. That, nothing there, a check that finds errors, or no
repository at all: [after an attack](#after-an-attack).

## Restore

After a second run, `sudo systemctl start hotserve-backup.service`
(snapshot `39021de9`), into a new directory, to look before anything
changes:

```
$ sudo hotserve-backup restore demo --to /root/demo-restored
demo: restored from snapshot 39021de9 of 2026-10-03 07:01 UTC into /root/demo-restored: sqlite app.db, files uploads
```

Every directory on the way has to be root's own and writable by nobody
else: `/root`, `/srv`, `/var/backups`, not `/tmp`. `--to` is root's
alone, and never to be granted through `sudo`
([why](../backups/README.md#a-restore)).

Into place, with the app running. It asks, takes the app's name typed
back for a yes, and backs the app up first:

```
$ sudo hotserve-backup restore demo
demo: restore snapshot 39021de9, made 2026-10-03 07:01 UTC, into /var/lib/liveswap/demo/shared?
What is there is backed up first. Its databases are then replaced and its files overwritten; what the snapshot does not hold is left.
Type the app's name, demo, to go on: demo
demo: backed up first: snapshot 66e09572 (restore --snapshot 66e09572 puts back what was there)
demo: restored from snapshot 39021de9 of 2026-10-03 07:01 UTC into /var/lib/liveswap/demo/shared: sqlite app.db, files uploads
```

`sudo hotserve-backup restore demo --snapshot 66e09572` undoes it: the
snapshot tagged `pre-restore` holds what was there. A restore needs the
snapshot's size free twice over where the fetch and the app share a
disk, and holds the run lock throughout: an hourly run that comes due
meanwhile fails, and says why.

## A rebuilt box

A new box, or the same one reinstalled; the `hotserve` user may get
another uid, which a restore does not mind:

1. Install the package ([Your first deploy](first-deploy.md), step 2)
   and put the box's Caddyfile back: `make push`
   ([After the first deploy](after-first-deploy.md), step 1), or, on a
   box without a box repo, the copy of `/etc/hotserve` you kept
   (below).
2. `sudo hotserve-backup setup <the same URL>`, with the box's key. It
   finds the repository and asks for its password instead of making
   one:

   ```
   looking for a repository at s3:https://s3.example.com/my-backups (up to 10s)
   the repository exists; its password is needed
   Repository password:
   checking that this key can write to it (up to 2m0s)
   repository ready: s3:https://s3.example.com/my-backups (existing, id cff1d334)
   ```

   A key that may read the repository and not write to it is refused
   there, before anything is written
   ([backups/README.md](../backups/README.md#setup)).

3. Before each app's first deploy, `sudo hotserve-backup restore <app>`.
   With nothing in place there is nothing to back up first:

   ```
   demo: restore snapshot 391c887b, made 2026-10-03 07:03 UTC, into /var/lib/liveswap/demo/shared?
   Its databases are replaced and its files overwritten, with no backup first; what the snapshot does not hold is left.
   Type the app's name, demo, to go on: demo
   demo: restored from snapshot 391c887b of 2026-10-03 07:03 UTC into /var/lib/liveswap/demo/shared: sqlite app.db, files uploads
   ```

4. Deploy each app. The next hourly run backs it up as before.

An hourly run that comes between 2 and 3 finds the data missing, and
`status` says so and names the restore. A repository is one box's: if
the old box still runs, turn its timers off first
(`sudo systemctl disable --now hotserve-backup.timer hotserve-backup-drill.timer`).

**A backup holds what the apps declare under `shared/`, and nothing of
`/etc/hotserve`**: not the Caddyfile, whose `backup` blocks a restore
needs before it can run, and no `env_file`. A box repo holds the
Caddyfile; the `env_file`s are on the box alone. Keep them off the box
too, beside the repository URL, the key and the password. On a box
without a box repo, the whole directory, again after every change:

```sh
sudo tar -czf etc-hotserve.tar.gz -C /etc hotserve    # on the box; then copy it off
```

and on the rebuilt box, before step 2:

```sh
sudo tar -xzf etc-hotserve.tar.gz -C /etc
hotserve-backup validate /etc/hotserve/Caddyfile
sudo systemctl reload hotserve
```

It holds your apps' secrets: store it as you store the password.

## The weekly drill, and what it costs

Every Sunday between 03:30 and 03:40, the box's time — an offset of
this box's own — the drill fetches the newest snapshot of every app
whole, checks it as a restore would, and installs nothing; then it
checks the repository's structure and one fifty-second of its data.
`sudo hotserve-backup drill` runs it now:

```
$ sudo hotserve-backup drill
demo: restore proven: snapshot 39021de9, on 2026-10-03 07:02 UTC
repository: its structure, and data group 40/52: clean
```

The release ships this full drill. A lighter weekly one, which would
not fetch every app whole, is for later or never: what the full one
costs on a real provider decides it. On B2 (EU Central, from an arm64
Hetzner VPS) with 500 MB of new uploads, the run that uploaded them
took 24 s and the drill that fetched them back 14 s, its check included
[measured].

- **Egress:** each week, the apps' size, plus 1/52 of the repository.
  About 4.3 times the apps' size a month — less for data that
  compresses, which restic does.
- **Disk:** the largest app's restored size, free under
  `/var/lib/hotserve-backup`. An app that does not fit is not drilled,
  `status` says `restore not proven` with how much it needs, and stays
  unhealthy until there is room.
- **Time:** the drill holds the run lock. An hourly run that comes due
  meanwhile fails at once, and is that hour's failed unit:

  ```
  hotserve-backup[3611]: hotserve-backup: another backup run is in progress: pid 3450, since 2026-10-03T07:06:41Z
  systemd[1]: hotserve-backup.service: Failed with result 'exit-code'.
  ```

  On a slow link a long drill fails every run it overlaps, and past 3
  hours `status` calls the backups stale.

The drill's egress at the providers' prices, fetched 2026-10-03, per
month, by the apps' total size (the check's share left out):

| Apps | Drill egress | S3, us-east-1 | B2 | Hetzner Object Storage |
|---|---|---|---|---|
| 1 GB | 4.3 GB | $0 | ~$0.01 | €0 |
| 10 GB | 43 GB | $0 | ~$0.13 | €0 |
| 30 GB | 130 GB | ~$2.70 | ~$0.40 | €0 |
| 100 GB | 433 GB | ~$30 | ~$1.33 | €0 |

- **S3:** $0.09/GB out past 100 GB a month free, which is shared by
  the whole AWS account; with that free tier spent elsewhere, $0.39 a
  month per GB of apps.
- **B2:** egress free up to three times the average stored, then
  $0.01/GB; the figures take the repository as the apps' size, and a
  larger repository makes them lower.
- **Hetzner:** €6.49 a month includes 1 TB stored and 1 TB out, then
  €1/TB (before VAT): nothing more up to about 230 GB of apps.

All **unverified** against a bill: one real drill's egress is
[release-candidate check 8](release-candidate.md#8-one-drill-timed-and-its-egress).

## Retention, off the box

Nothing on the box forgets, prunes or repairs. Without retention every hourly
run adds two snapshots an app — the backup, and a small record that the
run ended ok — and listings slow as they grow
([Retention](../backups/README.md#retention)). Thin them from your own
machine, after the first three lines of
[the look](#look-from-your-own-machine):

```bash
# restic asks for the repository's password, for each.
restic forget --retry-lock 1h --tag hotserve --group-by host,paths,tags \
  --keep-hourly 24 --keep-daily 30 --keep-monthly 12
restic forget --retry-lock 1h --tag hotserve-clean \
  --keep-hourly 24 --keep-daily 30 --keep-monthly 12
restic prune --retry-lock 1h
```

Each needs the repository to itself, and without `--retry-lock` gives up
at once while a run of the box's holds a lock: `repository is already
locked`, exit 11 [measured]; with it, it waits. The
first `forget` thins each app's backups, and the snapshots a restore
made of what it restored over (tagged `pre-restore`) apart from them;
the second, the records that runs ended ok. One `forget` would keep a
restore's snapshot in place of that hour's backup
([why](../backups/README.md#retention)). Add `--dry-run` to either to
see first. What they remove stays an old version for 180 days, so the
space comes back then. On the e2e S3 server, after four runs, a restore
and its undo in one hour, the first kept both `pre-restore` snapshots
and the first and last backups (restic keeps the oldest too) and
removed the two in between, the second removed those two's records,
and `restic check` then found no errors. On MinIO, with a key that
cannot destroy a version, the two removed what the policy said and
destroyed nothing: every removal stayed an old version. On B2, a
`forget` with a key without `deleteFiles` hid the snapshot's file and
left its version [measured].

Each keeps the newest of every hour, day and month on its own. Where a
period's newest backup has no record — a run that ended `incomplete`,
or whose record was not written — the record kept is of a backup that
goes, and a restore on a rebuilt box then says that nothing vouches for
what remains of that period.

On the box, the next run's listing notices. A snapshot that was the
last proven restore is said, and is still proven:

```
demo: restore last proven: 2026-10-03 07:02 UTC, snapshot 39021de9
demo: snapshot 39021de9 is no longer in the repository (last seen there 2026-10-03 07:03 UTC); the restore proven was of it
```

The next drill proves the newest. A last complete backup that is gone
makes `status` unhealthy until a run makes another.

`prune` takes the repository for itself. A run's upload and a drill's
fetch that come due wait for it, up to two hours, holding the run lock
meanwhile, so the hours after fail too. The drill's check takes no
lock, and a prune under way can make it say `damaged`;
`sudo hotserve-backup drill` checks again at once, where the next drill
is a week away. So prune at a quiet hour, away from the drill (Sunday
from 03:30, the box's time).

## After an attack

With the box's key, or your own machine's, someone can hide, overwrite
or delete every file, and still destroy nothing for 180 days. On MinIO
an attack that did all three, and then tried to destroy the versions
(`Access Denied`; on B2 `401` for every file), made the next run fail
and `status` exit 1:

```
demo: failed: there is no repository at the configured location (exit 10); …
```

A quieter one — a single pack overwritten — left the hourly runs ending
`ok`, `status` moving its "last complete backup" on past the attack,
and only the next drill saying so (`restore not proven`, and its check
`damaged`) [measured]; the [look's](#look-from-your-own-machine)
`restic check --read-data` finds the same.

Recovery is a copy of the bucket as it was before the attack, into a
new bucket made as [above](#before-you-start-the-bucket-and-what-it-protects),
with a key the attacker never had that may read old versions — the
provider account's own — and Debian's rclone (1.60; in a container,
with `ca-certificates`), in `bash`:

```bash
export RCLONE_CONFIG_STORE_TYPE=s3 RCLONE_CONFIG_STORE_PROVIDER=AWS RCLONE_CONFIG_STORE_REGION=<the bucket's region>
read -r RCLONE_CONFIG_STORE_ACCESS_KEY_ID && export RCLONE_CONFIG_STORE_ACCESS_KEY_ID          # the account's key
read -rs RCLONE_CONFIG_STORE_SECRET_ACCESS_KEY && export RCLONE_CONFIG_STORE_SECRET_ACCESS_KEY
rclone copy "store,version_at='2026-10-03T08:49:00Z':my-backups" store:my-backups-recovered
```

On another S3 provider, `RCLONE_CONFIG_STORE_PROVIDER=Other` and
`RCLONE_CONFIG_STORE_ENDPOINT=https://<its endpoint>` in place of the
region; on B2, `RCLONE_CONFIG_STORE_TYPE=b2` with `…_ACCOUNT` and
`…_KEY`, and the same `store,version_at='…'` — there a copy at
`status`'s minute and one from the bucket's history both checked clean,
and a box set up onto the copy ran, drilled and was healthy [measured].
On AWS S3 and Hetzner all of this is **unverified**. Three things the
copy needs, each seen going wrong without it:

- the time in UTC, with its `T` and `Z`: `2026-10-03 08:49:48` is read
  in your machine's zone, and in Berlin found nothing;
- `version_at` on the source alone: as `--s3-version-at` it makes the
  new bucket read-only too, `can't modify or delete files in
  --s3-version-at mode`;
- the single quotes, without which the time's colons cut it short:
  `couldn't parse config item "version_at" = "2026-10-03T08"`.

**The moment** has to be before the attack's first change, and the copy
decides whether it was: with `RESTIC_REPOSITORY` set to the new bucket,
`restic snapshots` and `restic check --no-lock --read-data`. A check
that finds errors means the moment was too late — make another copy,
earlier; the copy never changes the attacked bucket, so trying again
costs only the copy. Earlier is safe: a moment in the middle of a run
leaves that run's unfinished files, which restic calls "non-critical"
(`1 additional files were found in the repo, which likely contain
duplicate data`), and the check finds no errors [measured]. Where to
start:

- **The attack stopped the runs** (the next run failed, as above):
  the minute `status` gave for the last complete backup, as printed —
  `last complete backup 2026-10-03 08:49 UTC` is `2026-10-03T08:49:00Z`.
  On MinIO a copy there held the backup before it and checked clean.
  Never that minute rounded up, which can fall after the attack began.
- **The look's check or the drill found it, or the box is not to be
  trusted** — the runs went on, so `status` is no guide: the moment from
  the bucket's history, every write and delete with its time in UTC,
  from the provider's own tool — Debian's `awscli`, with the account's
  key in `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` (and
  `--endpoint-url https://<its endpoint>` on another S3 provider):

  ```bash
  aws s3api list-object-versions --bucket my-backups --output text \
    --query "[Versions[].[LastModified,'write ',Key], DeleteMarkers[].[LastModified,'delete',Key]][]" | sort
  ```

  ```
  2026-10-03T08:49:40.290000+00:00  write   locks/ae3ab8a0…
  2026-10-03T08:49:40.526000+00:00  write   data/2b/2b3113e7…
  2026-10-03T08:49:40.532000+00:00  write   index/4050462a…
  2026-10-03T08:49:40.536000+00:00  write   snapshots/9db598a5…
  2026-10-03T08:49:40.539000+00:00  delete  locks/ae3ab8a0…
  2026-10-03T08:49:51.695000+00:00  write   config
  2026-10-03T08:49:52.027000+00:00  write   data/16/16b7af23…
  ```

  A run ends by deleting its lock. The attack begins at the first thing
  restic never does: a write to a name already there — restic writes
  every file once, `config` at `init` — or a delete of `config` or
  `keys/…`. Any moment between the two will do: `2026-10-03T08:49:45Z`
  here. A deletion of snapshots and data alone looks like a `prune`:
  compare it with when you last pruned. On B2, a run ends by hiding
  its lock, and Debian's `backblaze-b2` lists each upload and hide with
  its time — by name, so sort it by its date and time columns:

  ```bash
  backblaze-b2 ls --long --versions -r b2://my-backups | sort -k3,4
  ```

  Not rclone's `lsl --b2-versions`: it lists no version of a hidden
  file, and of a bucket whose every file was hidden it listed nothing
  [measured].

Once the copy checks clean, give the new bucket new keys (the old ones
are the attacker's) and follow [A rebuilt box](#a-rebuilt-box) with its
URL. The copy went through the machine running rclone, every object:
the whole repository comes down and goes up again, at the provider's
egress price.

New keys stop the attacker writing; they do not unknow the repository's
password. Whoever had root on the box had the credential file, and so
the password and, through the key files, the master key that encrypts
every backup — and restic changes a master key only by moving the data
into a new repository (`restic copy`) or starting a new one. Anyone who
later reads the bucket with them can read the backups.

## Against a real provider

**On Backblaze B2**, the release candidate `v0.3.0-rc1` ran the eight
checks of [release-candidate.md](release-candidate.md) on 2026-10-04,
from an arm64 Hetzner VPS and from fresh test boxes:

- **TLS**: `setup`, runs, restores, the drill and the look from another
  machine, over B2's own certificate.
- **The bucket above**: with keys without `deleteFiles`, every command
  ran, one holding its lock for 30 minutes included, and left no lock.
  An attack with the box's key destroyed nothing — `401` for each of 17
  files — then overwrote and hid everything; the next run failed
  `exit 10`, and the recovery copy, at `status`'s minute and from the
  bucket's history, checked clean and was set up onto. Still
  **unverified**: that B2 removes its own hide markers, and a drill's
  egress on a bill.
- **A bucket that does not exist**: the key could not make one, and
  `setup` ended after three askings in 7.5 s, nothing written
  (`NewBucket: b2_list_buckets: 401:`). **A wrong secret**: three
  askings in 3.8 s (`b2_authorize_account: 401:`). B2 answers at once
  where the e2e server kept restic retrying.
- **A read-only key**: `v0.3.0-rc1`'s `setup` took it, and every run
  then failed after 14½ minutes — `demo: failed: restic failed (exit
  1): the storage could not be reached, or refused the key, …` — as
  did every drill. `setup` has since been made to write to a repository
  that is there already before it takes the key, and refuses one that
  cannot ([backups/README.md](../backups/README.md#setup)): at once
  where the storage says so plainly, at its two-minute clock where
  restic retries, as it did on B2. That check has met the e2e server,
  not B2: **unverified** there.
- **A rebuilt box** with another uid for `hotserve`: restored before
  the first deploy, every file the new uid's.
- **Retention off the box**: **unverified**.

**On AWS S3 and Hetzner** none of this has been run, and every line
above is **unverified** there. Over TLS to the e2e S3 server, from a
private CA added to the box's trust store, `setup`, runs, restores and
the drill all worked; on MinIO the bucket, an attack and its recovery
did. Not known on any provider: whether the recovery copy is made
there without coming down to your machine.
