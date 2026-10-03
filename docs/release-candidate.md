# Checking a release candidate's backups

A release candidate is a pushed tag such as `v0.2.0-rc1`: the release
workflow runs the whole CI on it and publishes it as a prerelease. Its
backups have only ever met the e2e suites' S3 server, which has one key
that may do anything, makes a bucket when asked and enforces no
policy. These eight checks are what that server cannot show. Run them
on a fresh Debian 13 box with the candidate's `.deb`
([Your first deploy](first-deploy.md)), against a bucket made for the
purpose, following [Backups](backups.md) as written.

Each check gives its commands, and what the e2e server showed beside
the word **unverified** where only a real provider can say. Write down
what the provider showed; each answer replaces an **unverified** in
[Backups](backups.md), or is a defect to fix before the release.

`<url>` is the bucket's repository URL, `s3:https://…/<bucket>` or
`b2:<bucket>:`. The off-box machine is your own, with Debian's restic
0.18 and the [retention](backups.md#retention-off-the-box) lines'
first three set.

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

## 3. A key that cannot delete

The box set up with a key the provider forbids to delete. Then:

```sh
sudo systemctl start hotserve-backup.service
sudo systemctl start hotserve-backup.service
sudo hotserve-backup drill
hotserve-backup status
```

and from the off-box machine, with the key that may delete, now and
again after a day of hourly runs:

```sh
restic list locks --no-lock | wc -l
restic unlock
restic list locks --no-lock | wc -l
```

That the runs end `ok`, the drill proves and its check is `clean`, and
whether locks pile up while the box's key cannot remove its own:
**unverified**. On the e2e server, whose key may delete, no lock was
left and `restic unlock` said nothing.

## 4. B2: what the box's key does to a `forget`

On B2 only, with the box's no-delete key, by hand, on the box (it never
runs `forget` itself). Pick a snapshot you can lose from
`restic snapshots` off the box — an old `pre-restore` one:

```sh
sudo systemd-run --quiet --pipe --wait --collect \
  -p User=hotserve-backup -p EnvironmentFile=/etc/hotserve-backup/repository.env \
  -p CacheDirectory=hotserve-backup -E RESTIC_CACHE_DIR=/var/cache/hotserve-backup -E HOME=/nonexistent \
  /usr/bin/restic forget <id>
```

then, off the box, whether `restic snapshots` still lists it, and
whether B2's console (or `b2 ls --versions`) shows its file under
`snapshots/` deleted, or hidden with the version still there. The
prediction is hidden: `restic` says it is gone and it is not, so no
probe of "can this key delete?" can be trusted on B2.
**Unverified.** On the e2e server the box's key deleted it
(`1 / 1 files deleted`, the file gone).

## 5. Retention off the box, then status on the box

[Retention, off the box](backups.md#retention-off-the-box) as written,
with the second key; then on the box
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
