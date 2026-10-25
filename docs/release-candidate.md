# Checking a release candidate's backups

A release candidate is a pushed tag such as `v0.2.0-rc1`: the release
workflow runs the whole CI on it and publishes it as a prerelease. Its
backups have met the e2e suites' S3 server, which has one key that may
do anything, makes a bucket when asked and enforces no policy, and
MinIO standing in for a provider's versions and key policies. These
twelve checks are what neither can show. Run them on a fresh Debian 13
box with the candidate's `.deb` ([Your first deploy](first-deploy.md)),
against a bucket made for the purpose as
[Backups](backups.md#before-you-start-the-bucket-and-what-it-protects)
makes it — on B2 first, the provider it is easiest on, through its
S3-compatible endpoint — following [Backups](backups.md) as written.
On B2, check 3's first step comes before every other: its answer
decides whether the rest apply.

Each check gives its commands, and what the e2e server showed beside
the word **unverified** where only a real provider can say. Write down
what the provider showed; each answer replaces an **unverified** in
[Backups](backups.md), or is a defect to fix before the release.
`v0.3.0-rc1` ran them on Backblaze B2 from 2026-10-04 to 10-06, all
but check 5, and each check says what B2 showed; on AWS S3 and Hetzner
every one is still **unverified**.

`<url>` is the bucket's repository URL: on B2
`s3:https://s3.<region>.backblazeb2.com/<bucket>`, the endpoint the
bucket's page shows; elsewhere `s3:https://…/<bucket>`. The off-box
machine is your own, with Debian 13's restic 0.18, awscli, rclone and
backblaze-b2, the [look](backups.md#look-from-your-own-machine) lines'
first three set, and its own key; where it is a `debian:13`
container, install `ca-certificates` beside `restic` and `rclone`: apt
brings it with neither (rclone only suggests it; measured: the
`apt-get install -s` and `apt-cache depends` lines of
[`docs/design/backups/rc-runs/rc-results/L11-tar-exclude-umask.out`](design/backups/rc-runs/rc-results/L11-tar-exclude-umask.out)),
and restic without it fails every https storage, `x509: certificate
signed by unknown authority`. On B2, three more: every `aws` line
below then goes to B2's endpoint and not to AWS, and adds no checksum
an operation does not require (restic sends an upload's Content-MD5
and no other):

```bash
export AWS_DEFAULT_REGION=<region> AWS_ENDPOINT_URL=https://s3.<region>.backblazeb2.com
export AWS_REQUEST_CHECKSUM_CALCULATION=when_required
```

Nothing here prints a storage key or secret: keys are read with
`read`, never typed on a command line, and the credential file is shown
by its keys alone (`sudo sed 's/=.*/=…/' /etc/hotserve-backup/repository.env`).
One secret is shown: `setup` on a new repository shows its password
once, on the terminal (`Repository password (new): …`). Before you
share a transcript of a check, put `<N characters>` in its place.

## 1. A `b2:` URL, a bucket that does not exist, and a wrong secret

On a box not set up yet, with buckets of their own — not the one
checks 2 to 8 use (a failed setup writes nothing; one that succeeds
leaves the box on a bucket nobody made as
[Backups](backups.md#before-you-start-the-bucket-and-what-it-protects)
says, so set the box up afresh for check 2). A key made as Backups
says should not be able to make a bucket at all:

```sh
time sudo hotserve-backup setup b2:<bucket>:rc-check1    # refused before any prompt
time sudo hotserve-backup setup <url of a bucket nobody has made>
time sudo hotserve-backup setup <url>    # the right key id, a wrong secret, three times
```

The first is refused before it asks anything, exit 1, nothing written,
naming the same bucket and path at B2's S3-compatible endpoint, the
region left for you to read from the bucket's page:

```
hotserve-backup: b2: is not supported: B2 is reached at its S3-compatible endpoint, s3:https://s3.<region>.backblazeb2.com/<bucket>/rc-check1, with an application key, not the master key; restic's own B2 backend retries a failed upload without end
```

What the other two say, and how long each takes: **unverified**. On the
e2e server the second took 13 s and made the bucket:

```
looking for a repository at s3:https://…/box1 (up to 10s)
no repository answered within 10s: a bucket not made yet, a wrong key and a wrong host look alike here; a new repository will be made with the password shown next
…
repository ready: s3:https://…/box1 (new, id e9157010)
```

and the third ended after 31 s, nothing written:

```
restic could not make or open the repository (exit 1): Fatal: create repository at s3:https://…/box2 failed: Fatal: unable to open repository at s3:https://…/box2: client.BucketExists: The request signature we calculated does not match the signature you provided. Check your key and signing method.
the storage refused the key, or could not be reached: the key id and secret again (the password shown above still applies)
…
hotserve-backup: restic could not make or open the repository (exit 1): …; the password shown above was never used: discard it
```

Through B2's native API a wrong key was refused at once (measured,
check 1 of `v0.3.0-rc1`, nothing written): rc1's look ended at once but worded
it as a timeout, a password was shown, and the init was refused —
`b2.NewClient: b2_authorize_account: 401` for a wrong secret,
`NewBucket: b2_list_buckets: 401` for a bucket the key may not reach —
three askings in 7.5 s for the bucket nobody made and 3.8 s for the
wrong secret. What `setup` says of it has changed since (#353), and the box now
reaches B2 at its S3 endpoint: whether the look is refused at once
there — a key restricted to another bucket, or a wrong secret,
answered `403` — or only at its 10 s clock, and whether a password is
shown before the storage takes a key: **unverified**.

## 2. Set up, a first backup, status and the drill, over real TLS

[Set it up](backups.md#set-it-up) as written, then
`sudo hotserve-backup drill`, `hotserve-backup status`, and the
[look from your own machine](backups.md#look-from-your-own-machine),
both its lines. Each as [Backups](backups.md) shows it: **unverified**
over S3's and Hetzner's TLS; the e2e server over TLS from a private CA gave
exactly those lines, and MinIO the look's. **On B2:** all of it, as
written — the box's lines on an arm64 Hetzner VPS, the look from the
owner's laptop in a `debian:13` container with `ca-certificates`
added; the drill 10 s, the look's check `no errors were found`.

On B2, `setup` says
`B2's region, <region>, from the endpoint's name, goes in the file as AWS_DEFAULT_REGION`
before it asks for the key — for a host that is
`s3.<region>.backblazeb2.com` in any case, read past a port or a
trailing dot — and the credential file
then holds five keys: `RESTIC_REPOSITORY`, `RESTIC_PASSWORD`,
`AWS_DEFAULT_REGION`, `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`.
That both keys work without `listAllBucketNames` or `readBuckets`,
which the guide does not give them: **unverified**.

Where `setup` finds a repository there already — check 4's copy, check
7's second box, check 9 — its opening takes the repository's lock,
writing a lock file and removing it. After such a `setup`, and before
the next run, `restic list locks --no-lock | wc -l` from the off-box
machine should be 0. Where B2 refuses a delete by name to a key
without `deleteFiles` (check 3's first step), restic exits 0 and says
`error while unlocking:`, and `setup` refuses the key, naming the lock
it left: `restic opened the repository and could not remove the lock
file it wrote (restic said: Remove(<lock/…>) failed: …)`, then where
`restic unlock` clears it (check 6 has the whole line). That lock is
removed with a key that may delete, and, as check 3 says, the bucket's
design is decided before any other check.

## 3. Keys that cannot destroy a version

**On B2, first of every check:** what B2's S3 API does with a delete by
name — what restic sends for every lock file it removes and every file
`prune` removes, never naming a version — from a key without
`deleteFiles`. [Backblaze's page on its S3 keys](https://www.backblaze.com/docs/cloud-storage-s3-compatible-app-keys)
says both: its text, that
`writeFiles` is needed to delete a file by name and `deleteFiles` to
delete a version; its table, that Delete Object needs `deleteFiles`. In
a bucket set to keep prior versions for **1** day, from the off-box
machine, with a key for that bucket alone that has the box's
capabilities:

```bash
aws s3api put-object --bucket <that bucket> --key rc-s3-hide/probe.txt --body probe.txt
sleep 2    # a hide in the same second as the upload may not hide (check 11)
aws s3api delete-object --bucket <that bucket> --key rc-s3-hide/probe.txt
aws s3api delete-object --bucket <that bucket> --key rc-s3-hide/probe.txt --version-id <the upload's VersionId>
```

The first delete answered with `"DeleteMarker": true` means the guide's
keys can remove restic's locks and prune, as it says; `AccessDenied`
means they cannot — then stop here, and decide the bucket's design
before any other check. The second, a version deleted by its id as an
attack would, should be `AccessDenied`. Both **unverified**.

Then a locking command past restic's 22-minute mark: with the box's
timers stopped, so that no run of its own holds a lock meanwhile
(`sudo systemctl stop hotserve-backup.timer
hotserve-backup-drill.timer`, and `start` after), from the off-box
machine, with the *box's* key and the repository's password in
`RESTIC_PASSWORD` (restic cannot read both it and the data from stdin),
a backup that holds its lock for half an hour, and what it leaves:

```bash
read -r AWS_ACCESS_KEY_ID && export AWS_ACCESS_KEY_ID          # the BOX's key, not your own machine's
read -rs AWS_SECRET_ACCESS_KEY && export AWS_SECRET_ACCESS_KEY
read -rs RESTIC_PASSWORD && export RESTIC_PASSWORD    # the repository's password
(sleep 1800; echo probe) | restic backup --stdin --stdin-filename rc-lock-probe
restic list locks --no-lock | wc -l
aws s3api list-object-versions --bucket <bucket> --prefix locks/ \
  --query '[length(Versions || `[]`), length(DeleteMarkers || `[]`)]'
```

(On B2 the two key lines carry the key's keyID and applicationKey,
with `AWS_DEFAULT_REGION` beside them, as in the look. The password in the environment: with its data on stdin
restic cannot ask for it, `Fatal: cannot read both password and data
from stdin`.)

Then on the box, `sudo systemctl start hotserve-backup.service` twice,
`sudo hotserve-backup drill` and `hotserve-backup status`. That the
probe ends `snapshot … saved` after 30 minutes with no lock left, each
lock it removed a delete marker over a version that stays, the runs end
`ok`, and the drill proves and its check is `clean`: **unverified**
through B2's S3 endpoint, on AWS S3 and on Hetzner (through B2's
native API, on `v0.3.0-rc1`, the probe held its lock 30 minutes and
ended saved, no lock left). On
MinIO all of that held; a key that may delete nothing, on the other
hand, stopped the probe at 22 minutes —
`failed to refresh stale lock: client.RemoveObject: Access Denied.`,
`Fatal: unable to save snapshot: context canceled`, exit 1, seven locks
left. Remove the probe after, still with the box's key: `restic
forget <its id>`; then `unset RESTIC_PASSWORD`, so that restic asks
again, and set the two key lines back to your own machine's key.
**On B2**, from the laptop with the box's key, without `deleteFiles`: `processed 1 files, 6 B in 30:01`,
`snapshot … saved`, no lock left; the timers started again and
`status` said `dev: ok` (whether the two `systemctl start` runs were
made is not recorded); the drill after it, the box's first, proved the
restore and checked `data group 40/52: clean` in 10 s (dummy data, not
check 8's figure); and the `forget` hid the snapshot's file and left
its version
(`backblaze-b2 ls --long --versions` showed its upload, then its
upload and a hide).

Over S3 restic removes a file by name alone (`RemoveObject` with no
version), so on B2 even a key with `deleteFiles` should only hide what
restic removes. With such a key, in a repository of its own on a bucket
you can spare, a backup and a `restic snapshots`, then the listing of
its `locks/` as above: versions and delete markers both there means
restic hid; neither, that it destroyed. **Unverified.**

On B2, the bucket's own rule as well: two days after the first step,
`aws s3api list-object-versions --bucket <that bucket> --prefix
rc-s3-hide/` and `backblaze-b2 ls --long --versions -r b2://<that
bucket> rc-s3-hide` should list neither the probe's version nor its
marker: B2's documentation says a delete by name inserts a delete
marker ([Delete Object](https://www.backblaze.com/apidocs/s3-delete-object)),
and that its lifecycle rules always remove an orphan delete marker
([Put Bucket Lifecycle Configuration](https://www.backblaze.com/apidocs/s3-put-lifecycle-configuration)).
**Unverified** through the S3 endpoint. Through B2's own API it does:
a file hidden at 11:47 UTC on 2026-10-04 was still listed, upload and
hide, about 30 hours on (17:36 UTC on the 5th, the plan's record), and
both were gone by 48 hours (11:26 UTC on the 6th). On S3 the guide's
rule asks for it (expired delete markers), and Hetzner's cannot; both
**unverified**.

## 4. An attack with the box's key, and the recovery

On a bucket you can spare, with a box set up on it and run at least
once. A B2 key reaches both of B2's APIs, so from the off-box machine
the *box's* key is tried through each, in two rclone remotes set in the
environment as in [After an attack](backups.md#after-an-attack):
`b2store:` over the native API (`RCLONE_CONFIG_B2STORE_TYPE=b2`, with
`…_ACCOUNT` and `…_KEY`), and `s3store:` over the S3 endpoint
(`RCLONE_CONFIG_S3STORE_TYPE=s3`, `…_PROVIDER=Other`,
`…_ENDPOINT=https://s3.<region>.backblazeb2.com`, `…_REGION`, and the
key pair). Count the versions, try to destroy, count again, then
overwrite and delete every file:

```bash
aws s3api list-object-versions --bucket <bucket> --query 'length(Versions)'
rclone delete --b2-hard-delete b2store:<bucket>    # native API: every file refused, nothing gone
rclone delete --s3-versions s3store:<bucket>       # S3: every old version refused (see below)
aws s3api delete-object --bucket <bucket> --key config --version-id <its version>   # S3 by hand: refused
aws s3api list-object-versions --bucket <bucket> --query 'length(Versions)'         # as before
aws s3api put-object --bucket <bucket> --key config --body junk.txt                 # overwritten
rclone delete s3store:<bucket>                     # every file hidden
```

On another S3 provider, the `s3store:` lines alone, with its endpoint.
(On B2 `--b2-versions` would not do: in that mode rclone refuses every
write itself, and B2 is never asked. On AWS the box's key may not list
versions, so the S3 line stops before it tries — #171.)

Then on the box, `sudo systemctl start hotserve-backup.service` and
`hotserve-backup status`, and [After an attack](backups.md#after-an-attack)
as written, with the account's key, from `status`'s minute and from the
bucket's history, to compare; the copy is read with a new key of its
own. That destroying is refused through both APIs for every file, that
the version count does not fall, that the deletion only hides, that the
run fails `exit 10`, and that the copy, checked, holds the last
complete backup: **unverified** through B2's S3 endpoint and on AWS S3
and Hetzner (through B2's own API, on `v0.3.0-rc1`, every destruction
was refused, `401` for each of 17 files, the run failed `exit 10`,
and two copies checked clean:
[RC-check4-attack.out](design/backups/rc-runs/RC-check4-attack.out),
[RC-check4-recover.out](design/backups/rc-runs/RC-check4-recover.out)). On MinIO, run after the deletion, the
S3 line was `AccessDenied` for each of 48 versions, all of which
remained, and the deletion emptied the bucket's listing; the run said
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

On B2, with the same key, what `prune` removes should become delete
markers over versions that stay:

```bash
aws s3api list-object-versions --bucket <bucket> --prefix data/ \
  --query '[length(Versions || `[]`), length(DeleteMarkers || `[]`)]'
```

before and after a prune that removes something shows more markers and
no fewer versions, and
`restic check --no-lock` then finds no errors. On a box a few hours old
nothing is due: a snapshot of its own (`head -c 8388608 /dev/urandom |
restic backup --stdin --tag rc-retention`), forgotten by its id, gives
`prune` a pack to remove. **Unverified.**

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
hotserve-backup: restic could not open the repository with its lock (exit 1): Save(<lock/…>) failed: client.PutObject: Access Denied. unable to create lock in backend: client.PutObject: Access Denied.
```

at once, and at the clock:

```
still waiting for s3:http://…/box1 (Ctrl-C is safe: nothing of setup's has been written; a lock file restic was writing may stay, and hold off a prune, or a check from elsewhere, until restic unlock removes it)
hotserve-backup: restic had not finished opening the repository with its lock within 2m0s (restic said: Save(<lock/…>) returned error, retrying after …: …); nothing of setup's has been written; a lock file restic was writing may stay, and hold off a prune, or a check from elsewhere, until restic unlock removes it, with a key that may delete: from this box at once, once setup has ended, given that key's credentials, if no other machine with this hostname runs restic against the repository; otherwise from another machine that holds it once the lock is half an hour old; or with restic unlock --remove-all sooner, when nothing else holds the repository
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
hotserve-backup: restic opened the repository and could not remove the lock file it wrote (restic said: Remove(<lock/…>) failed: client.RemoveObject: Access Denied.): a key that may not delete, or a storage that refuses or does not answer deletes, ends here; the lock stays until restic unlock removes it, with a key that may delete — not this one: from this box at once, once setup has ended, given that key's credentials, if no other machine with this hostname runs restic against the repository; otherwise from another machine that holds it once the lock is half an hour old; or with restic unlock --remove-all sooner, when nothing else holds the repository
```

`restic unlock` with the key that may delete clears it: from the box
at once, once setup has ended, if no other machine with this hostname
runs restic against the repository (restic takes a same-host lock whose
pid is gone for stale: M4 cases C and D, and a live lock of a second
machine with the same hostname too: case E); otherwise from your own
machine once it is half an hour old (from another machine restic takes
only those for stale), or with
`restic unlock --remove-all` sooner, when nothing else holds the
repository ([look from your own machine](backups.md#look-from-your-own-machine)).

**On B2's native API**, `v0.3.0-rc1`, whose `setup` only read, took a
console Read Only key (`repository ready … (existing, …)`). The run
after it retried the lock about once a minute
(`b2_get_upload_url: 401:`) and failed after 868 s:
`demo: failed: restic failed (exit 1): the storage could not be reached, or refused the key, or something else went wrong; …`;
the drill failed after 832 s in #170's words; `status` exited 1. **On
B2's S3 endpoint**, a key without `writeFiles` — the bucket alone,
`listBuckets`, `listFiles` and `readFiles` — should be answered
`AccessDenied` at the lock's write, which restic's S3 backend does not
retry, so `setup` refuses at once: **unverified**. Where a `setup` does
take such a key, `setup` again with the box's own afterwards.

## 7. Restore onto a second box, with another uid for `hotserve`

[A rebuilt box](backups.md#a-rebuilt-box) on a second fresh box — its
Caddyfile from the box repo with `hotserve init`, as step 1 says —
with `id -u hotserve` there not the first box's (make a system user or
two before installing the package). Then
`sudo ls -ln /var/lib/liveswap/<app>/shared`: every file the second
box's uid. **Unverified** on S3 and Hetzner; on the e2e server (uids 996
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
B2**, through its native API on `v0.3.0-rc1` (EU Central, an arm64
Hetzner VPS), with 500 MB of new uploads:
the run that uploaded them 24 s, the drill 14 s — `restore proven`,
its check 41/52 `clean` included.

## 9. A box set up on `b2:`, moved to B2's S3 endpoint

On a box that an earlier release set up with a `b2:` repository, with
the candidate installed: `sudo systemctl start hotserve-backup.service`,
then `sudo hotserve-backup status`. The run refuses at once, before
any unit starts, in these words (in its journal,
`journalctl -u hotserve-backup.service`, after the date and host):

```
hotserve-backup[<pid>]: hotserve-backup: the repository is b2:, which is not supported: sudo hotserve-backup setup 's3:https://s3.<region>.backblazeb2.com/<bucket>/<path>' moves it to B2's S3-compatible endpoint, with an application key; restic's own B2 backend retries a failed upload without end
```

and `status` says `the last run ended early: the repository is b2:,
which is not supported: …`, and as root warns of the file in setup's
words: `warning: /etc/hotserve-backup/repository.env:
RESTIC_REPOSITORY: b2: is not supported: …`.

A restore and a drill refuse the same way. Then, with the same bucket
and path, the same key id and secret, and the repository's password:

```sh
sudo cat /var/lib/hotserve-backup/repository-id
sudo hotserve-backup setup 's3:https://s3.<region>.backblazeb2.com/<bucket>/<path>'
sudo cat /var/lib/hotserve-backup/repository-id
```

then, from the off-box machine, `restic list locks --no-lock | wc -l`
(check 2: 0). `setup` should find the repository (`existing, id …`,
the id beside the record unchanged), put no record aside, leave no
lock, and write the file with `AWS_DEFAULT_REGION` and no `B2_` line;
then a run, the drill and
`status` as before the candidate, the last backup and the restore last
proven going on from there: **unverified**.

## 10. A key restricted to a name prefix

A key for one bucket you can spare, made with `--name-prefix
rc-prefix/` and the box's capabilities; from the off-box machine,
`restic init`, a backup, `restic snapshots` and `restic check
--no-lock` with `RESTIC_REPOSITORY` at `…/<bucket>/rc-prefix`, then
`restic init` at `…/<bucket>/rc-outside`. Backblaze documents such
keys for its S3 API. That restic works inside the prefix and is refused
outside it, how fast, and in what words: **unverified**.

## 11. A lock removed in the second it was written

[B2's documentation](https://www.backblaze.com/docs/cloud-storage-call-the-s3-compatible-api)
says a hide made in the same second as the file's
upload may not hide it, and a short restic command writes its lock and
removes it within a second. From the off-box machine, in a repository
of its own on a bucket you can spare, with a key of the box's
capabilities:

```bash
n=0; while [ $n -lt 100 ]; do restic snapshots --quiet >/dev/null; n=$((n + 1)); done
restic list locks --no-lock | wc -l
```

0 expected. A lock left behind holds off a `prune`, which ends
`repository is already locked`, exit 11, after its `--retry-lock`,
until `restic unlock` removes it. **Unverified.**

## 12. The region

Without a region, restic's S3 library first asks the bucket's location,
which B2 gives only to a key with `readBuckets` — the guide's keys have
none — and on that refusal signs every request for `us-east-1`. Off the
box only, `restic cat config --no-lock` with `DEBUG_LOG` set to a file,
once without `AWS_DEFAULT_REGION` and once with it; from each file only
its request and status lines,
`grep -E '^(GET|HEAD|PUT|DELETE) |^HTTP/'`, then delete the file. With
the region there should be no `?location` request. Without it, whether
B2 takes the `us-east-1` signature decides whether the region is needed
or only spares a request: **unverified**. `setup` writes the region for
a B2 endpoint either way (check 2).
