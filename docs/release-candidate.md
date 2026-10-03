# Checking a release candidate's backups

A release candidate is a pushed tag such as `v0.2.0-rc1`: the release
workflow runs the whole CI on it and publishes it as a prerelease. Its
backups have met the e2e suites' S3 server, which has one key that may
do anything, makes a bucket when asked and enforces no policy, and
MinIO standing in for a provider's versions and key policies. These
eight checks are what neither can show. Run them on a fresh Debian 13
box with the candidate's `.deb` ([Your first deploy](first-deploy.md)),
against a bucket made for the purpose as
[Backups](backups.md#before-you-start-the-bucket-and-what-it-protects)
makes it — on B2 first, the provider it is easiest on — following
[Backups](backups.md) as written.

Each check gives its commands, and what the e2e server showed beside
the word **unverified** where only a real provider can say. Write down
what the provider showed; each answer replaces an **unverified** in
[Backups](backups.md), or is a defect to fix before the release.

`<url>` is the bucket's repository URL, `s3:https://…/<bucket>` or
`b2:<bucket>:`. The off-box machine is your own, with Debian's restic
0.18 and the [look](backups.md#look-from-your-own-machine) lines' first
three set, with its own key.

## 1. A bucket that does not exist, and a wrong secret

On a box not set up yet (a failed setup writes nothing; one that
succeeds is set up, and check 2 can carry on from it):

```sh
time sudo hotserve-backup setup <url of a bucket nobody has made>
time sudo hotserve-backup setup <url>    # the right key id, a wrong secret, three times
```

What each says, and how long it takes: **unverified**. On the e2e
server the first took 13 s and made the bucket:

```
looking for a repository at s3:https://…/box1 (up to 10s)
no repository answered within 10s: a bucket not made yet, a wrong key and a wrong host look alike here; a new repository will be made with the password shown next
…
repository ready: s3:https://…/box1 (new, id e9157010)
```

and the second ended after 31 s, nothing written:

```
restic could not make or open the repository (exit 1): Fatal: create repository at s3:https://…/box2 failed: Fatal: unable to open repository at s3:https://…/box2: client.BucketExists: The request signature we calculated does not match the signature you provided. Check your key and signing method.
the storage refused the key, or could not be reached: the key id and secret again (the password shown above still applies)
…
hotserve-backup: restic could not make or open the repository (exit 1): …; keep the password shown above: restic init ran with it, and may have made the repository; run setup again, which looks first and asks for it if the repository is there
```

## 2. Set up, a first backup, status and the drill, over real TLS

[Set it up](backups.md#set-it-up) as written, then
`sudo hotserve-backup drill` and `hotserve-backup status`. Each line as
[Backups](backups.md) shows it: **unverified** over a provider's TLS;
the e2e server over TLS from a private CA gave exactly those lines.

## 3. Keys that cannot destroy a version

The bucket and both keys as [Backups](backups.md#before-you-start-the-bucket-and-what-it-protects)
makes them. First, a locking command past restic's 22-minute mark:
from the off-box machine, with the *box's* key, a backup that holds its
lock for half an hour, and what it leaves:

```bash
(sleep 1800; echo probe) | restic backup --stdin --stdin-filename rc-lock-probe
restic list locks --no-lock | wc -l
```

Then on the box, `sudo systemctl start hotserve-backup.service` twice,
`sudo hotserve-backup drill` and `hotserve-backup status`. That the
probe ends `snapshot … saved` after 30 minutes with no lock left, the
runs end `ok`, and the drill proves and its check is `clean`:
**unverified**. On MinIO all of that held; a key that may delete
nothing, on the other hand, stopped the probe at 22 minutes —
`failed to refresh stale lock: client.RemoveObject: Access Denied.`,
`Fatal: unable to save snapshot: context canceled`, exit 1, seven locks
left. Remove the probe after: `restic forget <its id>`.

## 4. An attack with the box's key, and the recovery

On a bucket you can spare. From the off-box machine, with rclone set up
as in [After an attack](backups.md#after-an-attack) but holding the
*box's* key, delete every file, then try to destroy the old versions
too:

```bash
rclone delete store:<bucket>
rclone delete --s3-versions store:<bucket>    # on B2: --b2-versions
```

Then on the box, `sudo systemctl start hotserve-backup.service` and
`hotserve-backup status`, and [After an attack](backups.md#after-an-attack)
as written, with the account's key. That the deletion only hides (on
B2, restic's and rclone's deletes hide where the key may not delete),
that destroying a version is refused for every one, that the run fails
`exit 10`, and that the copy, checked, holds the last complete backup:
**unverified**. On MinIO the first emptied the bucket's listing, the
second was `AccessDenied` for each of 48 old versions, all of which
remained; the run said
`there is no repository at the configured location (exit 10)`, and
the copy as written held the last complete backup and checked clean.

## 5. Retention off the box, then status on the box

[Retention, off the box](backups.md#retention-off-the-box) as written,
with the off-box machine's key; then on the box
`sudo systemctl start hotserve-backup.service` and
`hotserve-backup status`. Each line as [Backups](backups.md) shows it:
**unverified** on a provider; on the e2e server, as shown there.

## 6. A read-only key

A key that may read and not write, on a box that is not your real one.
`setup` only reads an existing repository, so it should take the key;
whether it does is part of the check:

```sh
sudo hotserve-backup setup <url>
time sudo systemctl start hotserve-backup.service
hotserve-backup status
```

What `status` says, and after how long: **unverified**, and not run on
the e2e server, which has no read-only key. Read from the code, the
upload fails and the app is
`demo: failed: restic failed (exit 1): the storage could not be reached, or refused the key, or something else went wrong; …`,
after however long restic retries. `setup` again with the box's own
key afterwards.

## 7. Restore onto a second box, with another uid for `hotserve`

[A rebuilt box](backups.md#a-rebuilt-box) on a second fresh box, with
`id -u hotserve` there not the first box's (make a system user or two
before installing the package). Then
`sudo ls -ln /var/lib/liveswap/<app>/shared`: every file the second
box's uid. **Unverified** on a provider; on the e2e server (uids 996
and 993) the restore put back the database and the files, owned by
993, and the first run and drill there were `ok` and proven.

## 8. One drill, timed, and its egress

On the box set up in check 2, with apps of a realistic size:

```sh
du -sh /var/lib/liveswap/*/shared
time sudo hotserve-backup drill
```

then the provider's egress for that hour, from its console or bill.
Against [the drill's cost](backups.md#the-weekly-drill-and-what-it-costs),
this decides whether the lighter drill is ever needed.
**Unverified.** On the e2e server, on one machine, a 400 MiB app took
the drill 7 s, its check included.
