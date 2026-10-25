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
`v0.3.0-rc1` ran them on Backblaze B2 from 2026-10-04 to 10-06, all
but check 5, and each check says what B2 showed; on AWS S3 and Hetzner
every one is still **unverified**.

`<url>` is the bucket's repository URL, `s3:https://…/<bucket>` or
`b2:<bucket>:`. The off-box machine is your own, with Debian's restic
0.18 and the [look](backups.md#look-from-your-own-machine) lines' first
three set, with its own key; where it is a `debian:13` container,
install `ca-certificates` beside `restic` and `rclone`: apt brings it
with neither (rclone only suggests it; measured), and
restic without it fails every https storage,
`x509: certificate signed by unknown authority`.

## 1. A bucket that does not exist, and a wrong secret

On a box not set up yet, with buckets of their own — not the one
checks 2 to 8 use (a failed setup writes nothing; one that succeeds
leaves the box on a bucket nobody made as
[Backups](backups.md#before-you-start-the-bucket-and-what-it-protects)
says, so set the box up afresh for check 2). A key made as Backups
says should not be able to make a bucket at all:

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
hotserve-backup: restic could not make or open the repository (exit 1): …; the password shown above was never used: discard it
```

On B2 a wrong key is refused at once (measured, check 1 of
`v0.3.0-rc1`, nothing written): rc1's look ended at once but worded
it as a timeout, a password was shown, and the init was refused —
`b2.NewClient: b2_authorize_account: 401` for a wrong secret,
`NewBucket: b2_list_buckets: 401` for a bucket the key may not reach —
three askings in 7.5 s for the bucket nobody made and 3.8 s for the
wrong secret. What `setup` says of it has changed since (#353): the
look itself refuses, no password is shown until the storage takes a
key, and three refusals end in restic's words with nothing said of a
password — **unverified** on B2.

## 2. Set up, a first backup, status and the drill, over real TLS

[Set it up](backups.md#set-it-up) as written, then
`sudo hotserve-backup drill`, `hotserve-backup status`, and the
[look from your own machine](backups.md#look-from-your-own-machine),
both its lines. Each as [Backups](backups.md) shows it: **unverified**
over a provider's TLS; the e2e server over TLS from a private CA gave
exactly those lines, and MinIO the look's. **On B2:** all of it, as
written, from an arm64 Hetzner VPS; the drill 10 s, the look's check
`no errors were found`.

## 3. Keys that cannot destroy a version

The bucket and both keys as [Backups](backups.md#before-you-start-the-bucket-and-what-it-protects)
makes them. First, a locking command past restic's 22-minute mark:
with the box's timers stopped, so that no run of its own holds a lock
meanwhile (`sudo systemctl stop hotserve-backup.timer
hotserve-backup-drill.timer`, and `start` after), from the off-box
machine, with the *box's* key, a backup that holds its lock for half an
hour, and what it leaves:

```bash
read -rs RESTIC_PASSWORD && export RESTIC_PASSWORD    # the repository's password
(sleep 1800; echo probe) | restic backup --stdin --stdin-filename rc-lock-probe
restic list locks --no-lock | wc -l
```

(The password in the environment: with its data on stdin restic cannot
ask for it, `Fatal: cannot read both password and data from stdin`.)

Then on the box, `sudo systemctl start hotserve-backup.service` twice,
`sudo hotserve-backup drill` and `hotserve-backup status`. That the
probe ends `snapshot … saved` after 30 minutes with no lock left, the
runs end `ok`, and the drill proves and its check is `clean`:
**unverified** on S3 and Hetzner. On MinIO all of that held; a key that may delete
nothing, on the other hand, stopped the probe at 22 minutes —
`failed to refresh stale lock: client.RemoveObject: Access Denied.`,
`Fatal: unable to save snapshot: context canceled`, exit 1, seven locks
left. Remove the probe after: `restic forget <its id>`, then
`unset RESTIC_PASSWORD`, so that restic asks again. **On B2**, with
the box's key without `deleteFiles`: `processed 1 files, 6 B in 30:01`,
`snapshot … saved`, no lock left; the timers started again and
`status` said `dev: ok` (whether the two `systemctl start` runs were
made is not recorded); the drill after it, the box's first, proved the
restore and checked `data group 40/52: clean` in 10 s (dummy data, not
check 8's figure); and the `forget` hid the snapshot's file and left
its version
(`backblaze-b2 ls --long --versions` showed its upload, then its
upload and a hide).

On B2, the bucket's own rule as well: a second bucket set to keep
prior versions for **1** day, with a key without `deleteFiles`; upload
a file and hide it (`backblaze-b2 upload-file`, then `hide-file`, as
restic's removal does), and after the next daily run —
two days on, to be sure — `backblaze-b2 ls --long --versions -r
b2://<that bucket>` should list neither the file's version nor its hide
marker: B2's documentation says it removes a hide marker itself once
nothing is under it. **On B2** it does: a file hidden at 11:47 UTC on
2026-10-04 was still listed, upload and hide, a day on (17:36 UTC on
the 5th, the plan's record), and both were gone by 48 hours (11:26
UTC on the 6th). On S3 the guide's rule asks for it (expired delete
markers), and Hetzner's cannot; both **unverified**.

## 4. An attack with the box's key, and the recovery

On a bucket you can spare. From the off-box machine, with rclone set up
as in [After an attack](backups.md#after-an-attack) but holding the
*box's* key, try to destroy, then delete every file:

```bash
rclone delete --b2-hard-delete store:<bucket>    # B2: every file refused, nothing gone
rclone delete --s3-versions store:<bucket>       # S3: every old version refused (see below)
rclone delete store:<bucket>                     # every file hidden
```

(On B2 `--b2-versions` would not do: in that mode rclone refuses every
write itself, and B2 is never asked. On AWS the box's key may not list
versions, so the S3 line stops before it tries — #171.)

Then on the box, `sudo systemctl start hotserve-backup.service` and
`hotserve-backup status`, and [After an attack](backups.md#after-an-attack)
as written, with the account's key, from `status`'s minute and from the
bucket's history, to compare. That destroying is refused for every
file, that the deletion only hides, that the run fails `exit 10`, and
that the copy, checked, holds the last complete backup:
**unverified**. On MinIO, run after the deletion, the S3 line was
`AccessDenied` for each of 48 versions, all of which remained, and the
deletion emptied the bucket's listing; the run said
`there is no repository at the configured location (exit 10)`, and
the copy as written held the last complete backup and checked clean.
**On B2:** the first line was `401 unauthorized` for each of 17 files,
three tries each over some three minutes, and all 17 remained; the
deletion hid every file; the run failed `exit 10` and `status` exited
1. The copy at `status`'s minute held the backup before the last and
checked clean, as the guide says of that minute; one at a moment from
the bucket's history (`backblaze-b2 ls --long --versions -r …`, sorted
by its date and time columns, since B2 lists by name; the guide now
sorts by the millisecond stamp in each file id) held the last backup
too and checked clean;
`setup` onto it kept the record, and the run, the drill and `status`
were healthy. Both copies were made with the *box's* key — on B2
`listFiles` and `readFiles` read old versions — into a folder of the
same bucket; the account's key and a new bucket, as the guide writes
it, are still **unverified**.

## 5. Retention off the box, then status on the box

[Retention, off the box](backups.md#retention-off-the-box) as written,
with the off-box machine's key; then on the box
`sudo systemctl start hotserve-backup.service` and
`hotserve-backup status`. Each line as [Backups](backups.md) shows it:
**unverified** on a provider; on the e2e server, as shown there.

## 6. A read-only key

A key that may read and not write, on a box that is not your real one,
and a repository made beforehand with a key that may write:

```sh
time sudo hotserve-backup setup <url>
```

`setup` opens a repository that is there already taking its lock —
`restic cat config`, which writes a lock file and removes it — and
should refuse this key: at once where the storage says so plainly, at
its two-minute clock where restic retries. **Unverified** on a
provider; on the e2e server behind a proxy that refuses writes, both —
the lines as the e2e asserts them, with the clock a release's binary
has (the e2e's is 30 s); a run's lock ids and timings are its own:

```
hotserve-backup: restic could not open the repository with its lock (exit 1): unable to create lock in backend: client.PutObject: Access Denied.
```

at once, and at the clock:

```
still waiting for s3:http://…/box1 (Ctrl-C is safe: nothing of setup's has been written; a lock file restic was writing may stay, and hold off a prune, or a check from elsewhere, until restic unlock removes it)
hotserve-backup: restic had not finished opening the repository with its lock within 2m0s (restic said: Save(<lock/…>) returned error, retrying after …: …)
```

each exit 1, with nothing of setup's written: `status` after either is
what it was before — `backups are not set up` on a box that was not,
the earlier setup on one that was (the credential file is left byte
for byte as it was).

**A key that may write and not delete** — narrower than the guide's
policies — writes the lock file and cannot remove it; restic says so,
`error while unlocking:`, and exits 0 ([M4](design/backups/rc-runs/rc-results/M4-unlock.out),
case A), and setup refuses the key, quoting restic's line, which names
the lock (on the e2e server, behind a proxy that refuses DELETE
alone):

```
hotserve-backup: restic opened the repository and could not remove the lock file it wrote (restic said: Remove(<lock/…>) failed: client.RemoveObject: Access Denied.): a key that may not delete, or a storage that refuses or does not answer deletes, ends here; the lock stays until restic unlock removes it: from this box at once once setup has ended, from another machine once it is half an hour old, or restic unlock --remove-all sooner, when nothing else holds the repository
```

`restic unlock` with the key that may delete clears it: from the box
at once, once setup has ended (restic takes a same-host lock whose pid
is gone for stale: M4 cases C and D), from your own machine once it is
half an hour old (from another machine restic takes only those for
stale), or with
`restic unlock --remove-all` sooner, when nothing else holds the
repository ([look from your own machine](backups.md#look-from-your-own-machine)).

**On B2**, `v0.3.0-rc1`, whose `setup` only read, took a console Read
Only key (`repository ready … (existing, …)`). The run after it
retried the lock about once a minute (`b2_get_upload_url: 401:`) and
failed after 868 s:
`demo: failed: restic failed (exit 1): the storage could not be reached, or refused the key, or something else went wrong; …`;
the drill failed after 832 s in #170's words; `status` exited 1. Where
a `setup` does take such a key, `setup` again with the box's own
afterwards.

## 7. Restore onto a second box, with another uid for `hotserve`

[A rebuilt box](backups.md#a-rebuilt-box) on a second fresh box — its
Caddyfile from the box repo with `hotserve init`, as step 1 says —
with `id -u hotserve` there not the first box's (make a system user or
two before installing the package). Then
`sudo ls -ln /var/lib/liveswap/<app>/shared`: every file the second
box's uid. **Unverified** on a provider; on the e2e server (uids 996
and 993) the restore put back the database and the files, owned by
993, and the first run and drill there were `ok` and proven. **On B2:**
a second box with no box repo, its Caddyfile copied over (as the e2e
run, L5 in the design record, before the box channel), uids 996 and
993, the restore 10 s. The guide's way back — the box repo and
`hotserve init` — is **unverified** on any box.

## 8. One drill, timed, and its egress

On the box set up in check 2, with apps of a realistic size:

```sh
du -sh /var/lib/liveswap/*/shared
time sudo hotserve-backup drill
```

then the provider's egress for that hour, from its console or bill.
Against [the drill's cost](backups.md#the-weekly-drill-and-what-it-costs),
this decides whether the lighter drill is ever needed.
The egress on a bill: **unverified.** On the e2e server, on one
machine, a 400 MiB app took the drill 7 s, its check included. **On
B2** (EU Central, an arm64 Hetzner VPS), with 500 MB of new uploads:
the run that uploaded them 24 s, the drill 14 s — `restore proven`,
its check 41/52 `clean` included.
