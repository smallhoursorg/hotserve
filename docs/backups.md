# Backups

The package ships `hotserve-backup`: an hourly backup of what each
app's Caddyfile block declares — its SQLite databases, copied safely
while the app runs, and the files under its `shared/` — into one
[restic](https://restic.net) repository off the box, and a weekly drill
that restores every app's newest snapshot to prove a restore would
work. This page is how to set it up, check on it, restore, rebuild a
box, and keep the repository small. How it works, and everything it
refuses, is [backups/README.md](../backups/README.md).

Every transcript below was observed on a fresh Debian 13 box with the
package installed, against the e2e suites' S3 server over TLS. On your
box the URL, the ids, the times and the password differ. Nothing here has been
run against a real provider yet: what depends on one says
**unverified**, and [the last section](#not-yet-tried-against-a-real-provider)
lists it all.

## Who runs these

The lines below that start with `sudo` are root's. Run them at the
provider's console, or as an administrator whose `sudo` is root's
(Debian's `sudo` group). The administrator
[After the first deploy](after-first-deploy.md) creates cannot: their
`sudoers` grants the `hotserve` user's reach and not root's, and
`setup` writes the one credential that reads every backup ever made.
That administrator — or an agent working as one — can run
`hotserve-backup validate` and `hotserve-backup status`, and read the
journal, which need no root. Keep `setup` for a person: it shows the
repository password on the terminal it runs in.

## Before you start

- **A bucket of its own**, on S3 or Backblaze B2 (`setup` asks for
  those two; `rest:`, `gs:` and `swift:` are written
  [by hand](../backups/README.md#by-hand)). One repository per box; no
  lifecycle rule that expires objects by age, since a deduplicated pack
  judged old can hold the only copy of current data.
- **Two keys.** The box's, to read and write; give it no right to
  delete if the provider can say so. A second that may delete, kept off
  the box, for [retention](#retention-off-the-box). Whether a provider
  enforces the first is **unverified**.
- **Somewhere off the box** for three things: the repository URL, the
  box's key, and the password `setup` shows once. With those, `restic`
  reads every backup from any machine; without the password nothing
  can.

## Set it up

As root, or an administrator whose `sudo` is root's:

<!-- smoke: begin -->
```sh
# 1. In the app's block of /etc/hotserve/Caddyfile, what to back up:
#        backup {
#            sqlite app.db
#            files  uploads
#        }
#    then, before it goes live, whether a run could plan from it:
hotserve-backup validate /etc/hotserve/Caddyfile
sudo systemctl reload hotserve
# 2. The repository, once: the storage key is asked for at the terminal,
#    and a new repository's password is shown once, to store elsewhere.
sudo hotserve-backup setup s3:https://s3.example.com/my-backups
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
fresh Debian 13 with the `.deb` just installed.

`setup`, for a bucket nobody has made yet:

```
$ sudo hotserve-backup setup s3:https://s3.example.com/my-backups
account hotserve-backup: present
a run would back up demo, under /var/lib/liveswap
Storage key id (AWS_ACCESS_KEY_ID): …
Storage secret key (AWS_SECRET_ACCESS_KEY):
looking for a repository at s3:https://s3.example.com/my-backups (up to 10s)
no repository answered within 10s: a bucket not made yet, a wrong key and a wrong host look alike here; a new repository will be made with the password shown next
Repository password (new): <52 characters>
Store it off the box now, with the repository URL, s3:https://s3.example.com/my-backups, and the storage key: with those three, `restic -r <url>` reads every backup from any machine; without the password nothing can.
Type stored to go on: stored
repository ready: s3:https://s3.example.com/my-backups (new, id e9157010)
credentials: /etc/hotserve-backup/repository.env (root, 0600)
next: the first backup runs within the hour and ten minutes (systemctl list-timers hotserve-backup.timer); to run one now, sudo systemctl start hotserve-backup.service; then hotserve-backup status
```

A wrong key is asked for again, up to three times, the password shown
standing; a setup that ends early says whether to keep that password or
discard it ([backups/README.md](../backups/README.md#setup)).

## Is it working

`hotserve-backup status`, as anyone, exits 0 when all is well — every
app backed up within 3 hours, a restore of it proven within 8 days, the
repository's last check clean ([the whole rule](../backups/README.md#status))
— 1 when not, and 3 when it could not look. Right after `setup` it is
`no run yet: pending first run`, exit 0. After the timer's first run:

```
$ hotserve-backup status
last run 2026-10-03 07:01 UTC
the repository has not been checked yet: the weekly drill checks it
demo: ok: last complete backup 2026-10-03 07:01 UTC, snapshot c4f0d7c1 (data at /var/lib/liveswap/demo/shared)
demo: restore last proven: 2026-10-03 07:01 UTC, snapshot c4f0d7c1
no unit of a run, a restore or a drill is running
```

An app's first good backup, up to 1 GiB restored, is drilled by the
run that made it, as here; a larger one waits for the weekly drill. A run that fails is a failed unit until the next one succeeds —
`systemctl --failed` lists `hotserve-backup.service` — and
`journalctl -u hotserve-backup.service` (the `adm` group reads it) has
one line per app:

```
hotserve-backup[9377]: demo: ok: snapshot c4f0d7c1 holds sqlite app.db, files uploads
hotserve-backup[9377]: demo: restore proven: snapshot c4f0d7c1 was fetched and checked whole
```

What every line of `status` means is under
[Status](../backups/README.md#status).

## Restore

Into a new directory, to look before anything changes:

```
$ sudo hotserve-backup restore demo --to /root/demo-restored
demo: restored from snapshot 39021de9 of 2026-10-03 07:01 UTC into /root/demo-restored: sqlite app.db, files uploads
```

Every directory on the way has to be root's own and writable by nobody
else: `/root`, `/srv`, `/var/backups`, not `/tmp`. `--to` is root's
alone and never to be granted through `sudo`: whoever chooses the
directory chooses where root puts bytes the app wrote — restored into
`/etc/systemd/system/<unit>.d`, an app's `files` became that unit's
settings at the next `daemon-reload`.

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
   and put the box's Caddyfile back (`make push`,
   [After the first deploy](after-first-deploy.md), step 1).
2. `sudo hotserve-backup setup <the same URL>`, with the box's key. It
   finds the repository and asks for its password instead of making
   one:

   ```
   looking for a repository at s3:https://s3.example.com/my-backups (up to 10s)
   the repository exists; its password is needed
   Repository password:
   repository ready: s3:https://s3.example.com/my-backups (existing, id e9157010)
   ```

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

## The weekly drill, and what it costs

Every Sunday between 03:30 and 03:40, the box's time — an offset of
this box's own — the drill fetches the newest snapshot of every app whole, checks it
as a restore would, and installs nothing; then it checks the
repository's structure and one fifty-second of its data. `sudo
hotserve-backup drill` runs it now:

```
$ sudo hotserve-backup drill
demo: restore proven: snapshot 39021de9, on 2026-10-03 07:02 UTC
repository: its structure, and data group 40/52: clean
```

The release ships this full drill. A lighter weekly one, which would
not fetch every app whole, is for later or never: what the full one
costs on a real provider decides it.

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

Nothing on the box deletes, and the box's key should not be able to.
Without retention every hourly run adds two snapshots an app — the
backup, and a small record that the run ended ok — and listings slow
as they grow ([Retention](../backups/README.md#retention)). Thin them
from a machine of your own, with Debian's restic (0.18), the key that
may delete and the repository's password:

```sh
export RESTIC_REPOSITORY=s3:https://s3.example.com/my-backups
read -r AWS_ACCESS_KEY_ID && export AWS_ACCESS_KEY_ID         # the key that may delete
read -rs AWS_SECRET_ACCESS_KEY && export AWS_SECRET_ACCESS_KEY
# restic asks for the repository's password.
restic forget --tag hotserve --group-by host,paths,tags \
  --keep-hourly 24 --keep-daily 30 --keep-monthly 12
restic forget --tag hotserve-clean \
  --keep-hourly 24 --keep-daily 30 --keep-monthly 12
restic prune
```

Two `forget`s, because one would keep the wrong snapshot. The first
thins the apps' backups, each app on its own, and the snapshots a
restore made of what it restored over (tagged `pre-restore`) apart from
them: in one group, a restore's snapshot can be the newest of its hour,
and `--keep-hourly` would then keep it and drop that hour's backup. The second
thins the records, each app's on its own; a record's `vouches:` tag is
different every time, so it cannot share the first's grouping. Add
`--dry-run` to either to see first. On the e2e S3 server, after four
runs, a restore and its undo in one hour, the first kept both
`pre-restore` snapshots and the first and last backups (restic keeps
the oldest too) and removed the two in between, the second removed
those two's records, and `restic check` then found no errors.

On the box, the next run's listing notices. A snapshot that was the
last proven restore is said, and is still proven:

```
demo: restore last proven: 2026-10-03 07:02 UTC, snapshot 39021de9
demo: snapshot 39021de9 is no longer in the repository (last seen there 2026-10-03 07:03 UTC); the restore proven was of it
```

The next drill proves the newest. A last complete backup that is gone
makes `status` unhealthy until a run makes another.

`prune` takes the repository for itself. A run's upload and a drill's
fetch that come due wait for it, up to two hours; the drill's check
takes no lock, and a prune under way can make it say `damaged` — the
next check reads the same data again. So keep a prune away from the
drill, Sunday from 03:30, the box's time. `restic unlock` from the same
machine removes the locks restic judges stale; with a box key that
cannot delete, whether the box leaves its locks behind is
**unverified**.

## Not yet tried against a real provider

Each is **unverified**, and is a check the release candidate runs
([release-candidate.md](release-candidate.md)):

- **TLS** to a real provider. Over TLS to the e2e S3 server, from a
  private CA added to the box's trust store, `setup`, runs, restores
  and the drill all worked.
- **A key that cannot delete**: that backups, the check and the
  removal of restic's own locks work with it, and whether locks pile up.
- **B2's hide versus delete**: whether a `forget` with B2's no-delete
  key hides a snapshot, so that it looks deleted and is not.
- **A read-only key**: what `status` says. Traced, not run: the upload
  fails, `demo: failed: restic failed (exit 1): the storage could not
  be reached, or refused the key, …`, after however long restic retries.
- **A bucket that does not exist** on a provider that will not make
  one for the box's key. The e2e S3 server makes a bucket when asked,
  so `setup` there makes it.
- **A wrong secret's wording** on a real provider. On the e2e server
  `setup` asked three times, in 31 seconds, and ended
  `restic could not make or open the repository (exit 1): … The request
  signature we calculated does not match the signature you provided.`
