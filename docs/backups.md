# Backups

The package ships `hotserve-backup`: an hourly backup of what each
app's Caddyfile block declares — its SQLite databases, copied safely
while the app runs, and the files under its `shared/` — into one
[restic](https://restic.net) repository off the box. restic is a backup
program Debian ships; `hotserve-backup` runs it. A weekly drill proves
a restore would work: it fetches every app's newest snapshot, checks it
as a restore would, and installs nothing. This page is how to set
backups up, check on them, restore, rebuild a box, keep the repository
small, and recover from an attack. How `hotserve-backup` works, and
everything it refuses, is [backups/README.md](../backups/README.md).

Every transcript below was observed on a fresh Debian 13 box with the
package installed: against the e2e suites' S3 server over TLS, and,
where a bucket's versions matter, against MinIO standing in for a
provider. On your box the URL, the ids, the times and the password
differ. On Backblaze B2 the release candidate `v0.3.0-rc1` then ran
seven of the eight [release-candidate checks](release-candidate.md)
(2026-10-04 to 10-06), through B2's native API — restic's own `b2:`
backend, which the box no longer uses, and rclone's and
`backblaze-b2`'s B2 code — and what B2 showed is said where it bears,
[one hang included](#against-a-real-provider). On B2 each line says
what ran, and what has not been seen through the door the box uses,
B2's S3-compatible endpoint, says **unverified**. On AWS S3 and
Hetzner nothing has been run: what depends on them says
**unverified**, and [the last section](#against-a-real-provider) lists
it all.

## Who runs these

On the box, `hotserve-backup setup`, `restore`, `drill`, `run` and
`sweep`, and every `systemctl` line that starts, stops, reloads,
enables or disables a unit, are root's: run them as root over SSH
(`ssh root@box.example.com`, the way back [Hardening](hardening.md)
keeps). An administrator of your own whose `sudo` is root's puts the
word before them. What only reads needs no privilege:
`hotserve-backup validate` and `status`, `systemctl list-timers` and
`systemctl --failed`, and the journal through the `adm` group. The
administrator Hardening makes is in that group alone, with no `sudo`:
they — or an agent working as one — run those. Run as root, `status`
also checks the credential file, which only root can read. The
Caddyfile reaches the box by a signed push from its repository, and the
box runs `hotserve-backup validate` on each one before it is applied
([examples/box](../examples/box/README.md)). Keep `setup` for a person:
it shows the repository password on the terminal it runs in, and it
writes the one credential that reads every backup ever made.

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
was refused every destruction and [undone](#after-an-attack). On B2,
through B2's own API on `v0.3.0-rc1`
([RC-check4-attack.out](design/backups/rc-runs/RC-check4-attack.out)),
the box's key was refused every destruction too, and a copy made with
that key into the same bucket recovered the backups; the guide's own
recovery, with the account's key into a new bucket, is **unverified**
on B2, and so is all of it through B2's S3 endpoint, which the same
key reaches. On AWS S3 and Hetzner all of it is **unverified**.

- **A bucket of its own**, one per box: on Backblaze B2, AWS S3 or
  another S3 provider. `setup` asks for an `s3:` URL, B2's included
  (below); `rest:`, `gs:` and `swift:` are written
  [by hand](../backups/README.md#by-hand), and restic's own `b2:`
  backend is refused (why: "On B2", below). Versioning on, and one
  lifecycle rule: old (noncurrent) versions are removed after 180 days.
  Never a rule that expires current objects by age: a pack judged old
  can hold the only copy of data every backup uses.
- **Two keys**, neither able to destroy a version: the box's, which
  `setup` asks for, and your own machine's, for
  [retention](#retention-off-the-box) and the look. Both need to
  *delete*: restic replaces its lock file every five minutes, and with
  a key that may delete nothing, a command still running at 22 minutes
  was stopped [measured, on MinIO]. On a versioned bucket a delete only
  hides a file.
- **Somewhere off the box** for the repository URL, the box's key, and
  the password `setup` shows once. With those, `restic` reads every
  backup from any machine; without the password nothing can.

**On B2**, the easiest: every bucket keeps versions. The repository
URL is B2's S3-compatible endpoint, the one the bucket's page shows,
with the bucket after it: `s3:https://s3.<region>.backblazeb2.com/my-backups`
(HTTPS and Signature v4 only, Backblaze's
[S3-compatible API](https://www.backblaze.com/apidocs/introduction-to-the-s3-compatible-api)).
restic's own `b2:` backend is refused, by `setup` and by every run,
restore and drill: the B2 library Debian 13's restic is built with
retries a failed upload without end, and held a run for 27 hours with
nothing said ([#177](https://github.com/smallhoursorg/hotserve/issues/177),
[#354](https://github.com/smallhoursorg/hotserve/issues/354)). restic's
own documentation points B2 users to the S3 endpoint "due to issues
with error handling in the current B2 library that restic uses"
([Backblaze B2](https://restic.readthedocs.io/en/stable/030_preparing_a_new_repo.html#backblaze-b2),
`doc/030_preparing_a_new_repo.rst`).
Given the endpoint as the bucket's page writes it, `setup` writes the
region, its second part (`eu-central-003` in
`s3.eu-central-003.backblazeb2.com`), beside the key — `B2's region,
eu-central-003, from the endpoint's name, goes in the file as
AWS_DEFAULT_REGION` — so restic never asks B2 where the bucket is,
which these keys may not ask: B2 puts Get Bucket Location under
`readBuckets`
([S3-compatible app keys](https://www.backblaze.com/docs/cloud-storage-s3-compatible-app-keys)).
A `b2:` URL is refused before anything is asked, naming the S3 form
with its bucket and path and `<region>` for you to fill in from the
bucket's page:

```
hotserve-backup: b2: is not supported: B2 is reached at its S3-compatible endpoint, s3:https://s3.<region>.backblazeb2.com/my-backups, with an application key, not the master key; restic's own B2 backend retries a failed upload without end
```

Each key is an application key for the bucket alone, with the
capabilities `listBuckets`, `listFiles`, `readFiles` and `writeFiles`
— **not `deleteFiles`**, the capability that deletes a version. restic
over S3 only ever deletes by name (`Remove` in restic's
`internal/backend/s3`), which on B2 leaves a hide marker over the old
version ([Delete Object](https://www.backblaze.com/apidocs/s3-delete-object)),
so `deleteFiles` would add nothing but a stolen key's power to destroy
history. The key's keyID and applicationKey are what `setup` asks for
as the storage key id and secret. Never the account's master key, which
the S3 endpoint does not take
([S3-compatible API](https://www.backblaze.com/apidocs/introduction-to-the-s3-compatible-api)),
and no key or bucket made before 4 May 2020, which are not
S3-compatible (Backblaze's help,
[archived](https://web.archive.org/web/20230925042836/https://help.backblaze.com/hc/en-us/articles/360047425453)).
The web console offers three presets and no single capabilities —
Read and Write, which may delete, Read Only and Write Only — so make
the two keys once from the command line, with Debian 13's
`backblaze-b2` (3.19; a `debian:13` container will do: apt brings the
CA certificates with it, which it does not with `restic` or `rclone`
[measured: the `apt-get install -s` and `apt-cache depends` lines of
[`docs/design/backups/rc-runs/rc-results/L11-tar-exclude-umask.out`](design/backups/rc-runs/rc-results/L11-tar-exclude-umask.out)]):

```sh
apt-get update && apt-get install -y backblaze-b2
backblaze-b2 authorize-account     # asks for the master key's id, then the key
backblaze-b2 create-key --bucket my-backups hotserve-box listBuckets,listFiles,readFiles,writeFiles
backblaze-b2 create-key --bucket my-backups hotserve-laptop listBuckets,listFiles,readFiles,writeFiles
backblaze-b2 clear-account         # forgets the master key authorize-account stored on this machine
```

Each `create-key` prints the new key's id and the key itself, once.
(The two keys the checks of [release-candidate.md](release-candidate.md)
ran with were made with `create-key`;
these exact lines, from the tool's own usage text, are **unverified**.)
The master key has now been typed at a terminal: generate a new one in
the console, which ends the old.

Whether B2 lets such a key delete by name — restic's lock removal and
`prune` both do — is **unverified** through the S3 endpoint:
Backblaze's own page says in prose that a delete by name needs
`writeFiles` and a delete of a version `deleteFiles`, and its table
lists Delete Object under `deleteFiles` alone. Through B2's own API,
on `v0.3.0-rc1`, such a key was refused the delete (`401`) and
restic's own `b2:` backend hid the file instead: a backup that held
its lock for 30 minutes ended `snapshot … saved`, no lock left
[measured]. Were the S3 endpoint to refuse it, `setup` would refuse
these keys at its opening — restic writes its lock there, cannot
remove it, and says so (`error while unlocking:`), which `setup` reads
as a refusal — so no box could be set up on B2 with the keys this
guide makes
([release-candidate check 2](release-candidate.md#2-set-up-a-first-backup-status-and-the-drill-over-real-tls)).

In the bucket's Lifecycle Settings, "Keep prior versions for this
number of days": 180. B2 then removes a hide marker itself once the
versions under it are gone — its
[lifecycle rules](https://www.backblaze.com/docs/cloud-storage-lifecycle-rules),
and seen for a hide made through B2's own API on a bucket whose rule
keeps them a day: a file hidden on 2026-10-04 and its marker were both
gone two days on
([RC-lifecycle-day2.out](design/backups/rc-runs/RC-lifecycle-day2.out)); a hide made through the S3
endpoint is the same marker by Backblaze's documentation, **unverified**.
restic's documentation suggests "Keep only the last version" instead,
which frees the space a day after a hide; here the 180 days are the
protection, and that rule would make a stolen key's deletions final
after one.

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

As root on the box:

<!-- smoke: begin -->
```sh
# 1. In the app's block of the Caddyfile, what to back up:
#        backup {
#            sqlite app.db
#            files  uploads
#        }
#    With a box repo (examples/box), commit and push it: the box asks
#    hotserve-backup validate before it applies the push, and reloads
#    itself, so skip the next two lines. Without one, edit
#    /etc/hotserve/Caddyfile, then, before it goes live, ask whether a
#    run could plan from it:
hotserve-backup validate /etc/hotserve/Caddyfile
systemctl reload hotserve
# 2. The repository, once: the storage key is asked for at the terminal,
#    and a new repository's password is shown once, to store elsewhere.
hotserve-backup setup s3:https://s3.example.com/my-backups
# 3. The first backup runs within the hour and ten minutes
#    (systemctl list-timers hotserve-backup.timer), or now:
systemctl start hotserve-backup.service
hotserve-backup status
# 4. A restore: into a directory of root's own to look at, or into
#    place — which backs the app up first, tagged pre-restore, and asks.
hotserve-backup restore demo --to /root/demo-restored
hotserve-backup restore demo
```
<!-- smoke: end -->

The package's smoke test (`make install-test`) runs exactly those
lines, read out of this page, as root — `validate` and `status` as an
account with no privilege — on a fresh Debian 13 with the `.deb` just
installed and no `sudo`. Keep the block in the box repo's Caddyfile:
a push of a file without it stops that app's backups, and a run says
so once. The other order works too: `setup` before any block says
that a run would back nothing up.

`setup`, for a bucket nobody has made yet:

```
$ hotserve-backup setup s3:https://s3.example.com/my-backups
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
next: the first backup runs within the hour and ten minutes (systemctl list-timers hotserve-backup.timer); to run one now, systemctl start hotserve-backup.service; then hotserve-backup status
```

A wrong key is asked for again, up to three times, the password shown
standing — on B2 a wrong key was refused at once (`401`): on
`v0.3.0-rc1`, after a look that said it had timed out, the password
shown, then the init refused; the current look refuses it there and
shows none, **unverified** on B2; a setup that ends early says
whether to keep that password or discard it
([backups/README.md](../backups/README.md#setup)). Then
[look from your own machine](#look-from-your-own-machine) with the URL,
the key and the password as you stored them: a password copied wrong is
otherwise found only when the box is gone.

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
no backup unit running now
```

An app's first good backup, up to 1 GiB restored, is drilled by the
run that made it, as here. A larger one is left to the weekly drill,
and until then `status` says `restore not proven` and exits 1:
`hotserve-backup drill` proves it now. A run that fails is a
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
unknown authority` [measured: the failure in
[`docs/design/backups/rc-runs/rc-results/L6-offbox-container.out`](design/backups/rc-runs/rc-results/L6-offbox-container.out),
the dependency in the `apt-cache depends restic` line of
[`docs/design/backups/rc-runs/rc-results/L11-tar-exclude-umask.out`](design/backups/rc-runs/rc-results/L11-tar-exclude-umask.out)]):
`restic snapshots` for
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

On B2 the URL is the bucket's S3 endpoint, the two key lines carry
your own key's keyID and applicationKey, and the region goes with them,
as `setup` writes it into the box's file, so that restic does not ask
B2 for it (restic reads it, `internal/backend/s3/config.go`):

```bash
export RESTIC_REPOSITORY=s3:https://s3.eu-central-003.backblazeb2.com/my-backups
export AWS_DEFAULT_REGION=eu-central-003                         # the endpoint's second part
read -r AWS_ACCESS_KEY_ID && export AWS_ACCESS_KEY_ID          # your own key's keyID
read -rs AWS_SECRET_ACCESS_KEY && export AWS_SECRET_ACCESS_KEY # its applicationKey
```

The newest backup of each app should be within the hour; a snapshot a
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

After a second run, `systemctl start hotserve-backup.service`
(snapshot `39021de9`), into a new directory, to look before anything
changes:

```
$ hotserve-backup restore demo --to /root/demo-restored
demo: restored from snapshot 39021de9 of 2026-10-03 07:01 UTC into /root/demo-restored: sqlite app.db, files uploads
```

Every directory on the way has to be root's own and writable by nobody
else: `/root`, `/srv`, `/var/backups`, not `/tmp`. `--to` is root's
alone, and never to be granted through `sudo`
([why](../backups/README.md#a-restore)).

Into place, with the app running. It asks, takes the app's name typed
back for a yes, and backs the app up first:

```
$ hotserve-backup restore demo
demo: restore snapshot 39021de9, made 2026-10-03 07:01 UTC, into /var/lib/liveswap/demo/shared?
What is there is backed up first. Its databases are then replaced and its files overwritten; what the snapshot does not hold is left.
Type the app's name, demo, to go on: demo
demo: backed up first: snapshot 66e09572 (restore --snapshot 66e09572 puts back what was there)
demo: restored from snapshot 39021de9 of 2026-10-03 07:01 UTC into /var/lib/liveswap/demo/shared: sqlite app.db, files uploads
```

`hotserve-backup restore demo --snapshot 66e09572` undoes it: the
snapshot tagged `pre-restore` holds what was there. A restore needs the
snapshot's size free twice over where the fetch and the app share a
disk, and holds the run lock throughout: an hourly run that comes due
meanwhile fails, and says why.

## A rebuilt box

A new box, or the same one reinstalled; the `hotserve` user may get
another uid, which a restore does not mind:

1. Install the package ([Your first deploy](first-deploy.md), step 2)
   and give it the box's Caddyfile with `hotserve init` from the box
   repository, as the first deploy did (step 3; with secrets,
   [A rebuilt box](secrets.md#a-rebuilt-box)).
2. `hotserve-backup setup <the same URL>`, with the box's key. It
   finds the repository and asks for its password instead of making
   one:

   ```
   looking for a repository at s3:https://s3.example.com/my-backups (up to 10s)
   the repository exists; its password is needed
   Repository password:
   repository ready: s3:https://s3.example.com/my-backups (existing, id e9157010)
   ```

3. Before each app's first deploy, `hotserve-backup restore <app>`.
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
(`systemctl disable --now hotserve-backup.timer hotserve-backup-drill.timer`).

**A backup holds what the apps declare under `shared/`, and nothing of
`/etc/hotserve`**: not the Caddyfile, whose `backup` blocks a restore
needs before it can run, no env file, and no `deploy_trust local`
public key file. The Caddyfile and the secrets come back from the box
repo, which the first deploy makes
([examples/box](../examples/box/README.md)): the Caddyfile with
`hotserve init` in step 1; the secrets with `make rekey`, pushed and
applied before step 4, since until then the box has no env file for
the app ([A rebuilt box](secrets.md#a-rebuilt-box)). A `deploy_trust
local` public key is a file the Caddyfile names, not a commit, and
`hotserve init` refuses a Caddyfile whose key file is missing: in
step 1, after the install and before `hotserve init`, put the `.pub`
back from where the pair is kept. Or make a new pair with `hotserve
deploy-keygen`: its private key then replaces the old one wherever
tokens are minted — the CI secret, or your own machine — and only the
`.pub` goes to the box. Until the private key is replaced, a deploy
minted with the old one is refused: a flat 401, and the journal's
`webhook auth failed` line carries, in its `refused` field,
`local:<path>: go-jose/go-jose: error in cryptographic primitive`
([liveswap/README.md](../liveswap/README.md)).

Every other `env_file` the Caddyfile names comes back only if it is in
the box repo. One made by hand, such as `/etc/hotserve/demo.env`, is in
neither the backup nor the repo, and a launch that reads a missing env
file fails. Move a hand-made one in by
[Move a hand-made env file in](secrets.md#move-a-hand-made-env-file-in)
while the old box still runs: until then, nothing this guide covers
restores it.

## The weekly drill, and what it costs

Every Sunday between 03:30 and 03:40, the box's time — an offset of
this box's own — the drill fetches the newest snapshot of every app
whole, checks it as a restore would, and installs nothing; then it
checks the repository's structure and one fifty-second of its data.
`hotserve-backup drill` runs it now:

```
$ hotserve-backup drill
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
  larger repository makes them lower. The S3 endpoint is priced as B2's
  own API, egress and calls alike
  ([pricing](https://www.backblaze.com/cloud-storage/pricing),
  [transactions](https://www.backblaze.com/cloud-storage/transaction-pricing)).
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
[the look](#look-from-your-own-machine) (on B2, its four):

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
locked`, exit 11 [measured]; with it, it waits. A lock no command
holds any more is waited on all the same: restic never passes a stale
lock by itself [M75]. Two things leave one, each measured against the
e2e S3 server behind a proxy that injects the fault:

- **A storage outage that begins during a box's backup** leaves its
  lock, and the box's next backup goes on [measured]. A `prune` then
  waits out its hour and exits 11: restic refuses an exclusive lock
  over any lock there, never asking whether it is stale (restic 0.18.0,
  `internal/restic/lock.go:188-189`; M75, measured for a backup).
- **A `prune` that fails or is stopped** leaves its own, which no
  other command may share — after 35 minutes with every delete
  refused, three of them. Every run and drill of the box's then waits
  for it — two hours by its `--retry-lock`, of which 40 minutes were
  watched, nothing said meanwhile — exits 11, and says the repository
  stayed locked; `status`
  calls the backups stale from the third hour. So read `prune`'s exit
  status, not its words: the one whose every delete was refused
  printed `2 snapshots have been removed` and removed none. On B2,
  every `prune` would end this way if a key without `deleteFiles`
  may not delete by name (above, **unverified**).

Either way `restic list locks --no-lock` lists what is there. `restic
unlock` removes a lock restic judges stale; whether it judges a dead
prune's so was not tried. `restic unlock --remove-all` removes every
lock, a running command's too, so first stop the box's timers and let
any run end (`systemctl stop hotserve-backup.timer
hotserve-backup-drill.timer`; `hotserve-backup status` says whether a
unit is running), and have no command of your own running; then start
the timers again, and prune. The
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
`hotserve-backup drill` checks again at once, where the next drill
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
`…_KEY` (a key's keyID and applicationKey), and the same
`store,version_at='…'`: rclone's own B2 code, which speaks B2's own API
rather than the S3 endpoint the box uses, and which takes an
application key or the account's master key
([rclone's B2 page](https://rclone.org/b2/)). There a copy at
`status`'s minute and one from the bucket's history both checked
clean, and a box set up onto the copy (through restic's own `b2:`
backend) ran, drilled and was healthy [measured — with the box's own
key, which on B2 reads old versions too (`listFiles` and `readFiles`),
and into a folder of the attacked bucket
([RC-check4-recover.out](design/backups/rc-runs/RC-check4-recover.out));
the account's key and a new bucket, as written, are **unverified**].
On AWS S3 and Hetzner all of this is **unverified**.
Three things the copy needs, each seen going wrong without it:

- the time in UTC, with its `T` and `Z`: `2026-10-03 08:49:48` is read
  in your machine's zone, and in Berlin found nothing;
- `version_at` on the source alone: as `--s3-version-at` it makes the
  new bucket read-only too, `can't modify or delete files in
  --s3-version-at mode`;
- the single quotes, without which the time's colons cut it short:
  `couldn't parse config item "version_at" = "2026-10-03T08"`.

**The moment** has to be before the attack's first change, and the copy
decides whether it was: with `RESTIC_REPOSITORY` set to the new bucket,
and a key of the new bucket's own — made now, for it alone, as
[above](#before-you-start-the-bucket-and-what-it-protects); on B2 an
application key, since the S3 endpoint does not take the master key —
`restic snapshots` and `restic check --no-lock --read-data`. A check
that finds errors means the moment was too late — make another copy,
earlier. A check can also come back clean on a moment too late: an
attack that first hides the newest snapshot leaves a copy after that
hide whole, and short of it. So for a moment taken from the bucket's
history, the newest snapshot `restic snapshots` lists for each app must
be the one `hotserve-backup status` named before the attack — the
`snapshot <id>` on its `last complete backup` line; one missing means
the moment fell after the attack began, and an earlier one is needed.
(A copy at `status`'s own minute holds the backup before that one, as
below.) The copy never changes the attacked bucket, so trying again
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
  its lock (seen through B2's own API), and the provider's own tool,
  Debian's `backblaze-b2`, which speaks B2's own API with any key that
  may list files, lists each upload and hide with its time to the
  second — by name, so sort it by the upload stamp inside each file
  id, milliseconds since the epoch in the ninth `_` field:

  ```bash
  backblaze-b2 ls --long --versions -r b2://my-backups | sort -t_ -k9,9
  ```

  On the RC's own listing a hide and its upload in one second came
  out upload first, 346 ms apart [measured,
  `rc-runs/rc-results/L10-timeline-sort.out` in the design record]. Not
  rclone's `lsl --b2-versions`: of a bucket whose every file was
  hidden it listed nothing (the RC's record of 2026-10-05, in the
  plan; no capture).

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

Whoever had root had the apps' secrets too: every env file the
Caddyfile names, in clear — those under `/etc/hotserve/secrets` and
any made by hand — and the box's key with them. Then every value of
every one of them changes — database passwords, third-party keys,
hand-made files' included — on the rebuilt box in this order: `make
rekey` first, since `recipients` still names the old box's key and a
`make secret` before it would encrypt to that key and the workflow
refuse the push ([A rebuilt box](secrets.md#a-rebuilt-box)); then each
value set anew with `make secret` ([Add a secret](secrets.md#add-a-secret)),
a hand-made file's too, with the app's block changed to read
`/etc/hotserve/secrets/<app>.env` — not imported by
[the door](secrets.md#move-a-hand-made-env-file-in), which would carry
the attacker's values in; then the first deploy. `make rekey` alone
re-encrypts the values the attacker holds.

## Against a real provider

**On Backblaze B2**, the release candidate `v0.3.0-rc1` ran the checks
of [release-candidate.md](release-candidate.md) from 2026-10-04 to
10-06, from an arm64 Hetzner VPS and from fresh test boxes, all but
retention — through B2's native API: restic's own `b2:` backend,
which the box no longer uses, and rclone's and `backblaze-b2`'s B2
code. Through B2's S3-compatible endpoint, the one the box uses now,
none of it has run: each line below is **unverified** there, and so
are the endpoint's own questions after them.

- **TLS**: `setup`, runs, restores, the drill and the look from another
  machine, over B2's own certificate.
- **The bucket above**: with keys without `deleteFiles`, the backups,
  one `forget`, the checks, a restore and the drills all ran and left
  no lock, a backup holding its lock for 30 minutes included —
  restic's own `b2:` backend hides a file its key may not delete;
  retention (check 5) did not run. An attack with the box's key
  destroyed nothing — `401` for each of 17 files — then overwrote
  `config` and hid everything; the next run failed `exit 10`. Two
  recovery copies, one at `status`'s minute and one from the bucket's
  history, both checked clean, and `setup` was run onto the second. B2
  removed a hidden file's version and its hide marker within two days
  of a 1-day rule.
- **A bucket that does not exist**: the key could not make one, and
  `setup` ended after three askings in 7.5 s, nothing written
  (`NewBucket: b2_list_buckets: 401:`). **A wrong secret**: three
  askings in 3.8 s (`b2.NewClient: b2_authorize_account: 401:`). B2
  answers at once where the e2e server kept restic retrying.
- **A read-only key**: `setup` opens a repository that is there
  already taking its lock, which writes a lock file and removes it. A
  key that cannot write a lock file ends setup on one neutral line — at
  once where the storage says so plainly, with what restic said,
  without its retry lines, cut at 300 characters; at its two-minute
  clock where restic retries, with the last line restic said, which is
  then a retry line — and a key that wrote the lock
  file and could not remove it is refused by restic's own word that it
  could not, `error while unlocking:`
  ([M4](design/backups/rc-runs/rc-results/M4-unlock.out), cases A and
  B). Seen on the e2e server behind a proxy that refuses writes, all
  three ways; on B2, `v0.3.0-rc1`'s `setup`, which only read, took a
  console Read Only key, and every run then failed after 14½ minutes of
  retries (`b2_get_upload_url: 401:`, from restic's own `b2:` backend),
  as did the drill after it, after 14 minutes. The refusals themselves,
  and what B2's S3 endpoint answers such a key, are **unverified**.
- **A rebuilt box** with another uid for `hotserve`: restored before
  the first deploy in 10 s, every file the new uid's. The RC box had
  no box repo, and its Caddyfile was copied over; the guide's way
  back, the box repo and `hotserve init`, is **unverified** on any box
  ([check 7](release-candidate.md#7-restore-onto-a-second-box-with-another-uid-for-hotserve)).
- **One drill, timed**: with 500 MB of new uploads, the run that
  uploaded them 24 s, the drill that fetched them back 14 s, its check
  included. Its egress on a bill: **unverified**.
- **A run's upload hung**, on the owner's box, for 27 hours with no
  output, and no later run was scheduled meanwhile: `status` said
  `running: upload of dev, since …` and, after 3 hours, exited 1 with
  the backups stale. `kill -QUIT` on the unit's restic ended it, and
  the timer caught up at once. That is why the box no longer uses
  restic's own `b2:` backend (#177, #354): the B2 library Debian 13's
  restic is built with retries a failed upload without limit, below
  restic's own retries; which failure it was is not known. Over S3,
  every sustained fault measured that answers ended the command, exit
  1, some 15 minutes into the operation it failed, and a storage that
  never answers did not within 40 minutes, which is #177's question
  still [M87, M88].
- **Retention off the box**: **unverified**.

**Through B2's S3-compatible endpoint**, besides every line above:
whether a key without `deleteFiles` may delete by name, which
restic's lock removal and `prune` both do and Backblaze's own page
leaves unsettled ("On B2", above); B2's warning that a file hidden
within the same second it was uploaded may not be hidden
([calling the S3-compatible API](https://www.backblaze.com/docs/cloud-storage-call-the-s3-compatible-api)),
which a restic command shorter than a second does to its own lock
file — one native data point: an upload and its hide in the same
second, 11:47:00, and the hide held
([RC-lifecycle-day0.out](design/backups/rc-runs/RC-lifecycle-day0.out));
and its answers to a read-only key, a bucket that does not exist and
a wrong secret, another API's than the `401`s above: all
**unverified**.

**On AWS S3 and Hetzner** none of this has been run, and every line
above is **unverified** there. Over TLS to the e2e S3 server, from a
private CA added to the box's trust store, `setup`, runs, restores and
the drill all worked; on MinIO the bucket, an attack and its recovery
did. Not known on any provider: whether the recovery copy can be made
there without coming down to your machine.
