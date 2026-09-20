# backups

Backs up what hotserve's apps declare: the SQLite databases and the
files under each app's `shared/` that its `backup` block names
([liveswap/README.md](../liveswap/README.md#declaring-backups)), into
one [restic](https://restic.net) repository off the box.

It is a program of its own, `hotserve-backup`, and not part of the
server: hotserve validates a declaration and does nothing else with it,
and nothing here talks to hotserve. The two share a declaration format
(`liveswap/backupdecl`) and nothing more — `hotserve-backup` links no
Caddy.

**On this branch it is the engine, restore, the restore drill, and
what says how they are doing.** `hotserve-backup run` does one backup
run, `hotserve-backup restore <app>` puts a snapshot back,
`hotserve-backup drill` proves that a restore would work without doing
one, `hotserve-backup status` says whether each app's backup is fresh
and its restore proven, and `hotserve-backup validate <Caddyfile>` says
whether a run could plan from a Caddyfile before it goes live. There is
no setup command, no timer and no package yet: all but `validate` need `/etc/hotserve/backup.env`
written by hand (below), Debian 13's `restic` and `sqlite3` installed,
and systemd 257 (`PrivatePIDs=`), which is Debian 13's.

## A run

`hotserve-backup run`, as root:

1. takes the run lock — one run at a time; a second says who holds it;
2. stops, by exact name, any unit an earlier run recorded and did not
   live to stop, and takes away any mount it left;
3. has an unprivileged unit adapt `/etc/hotserve/Caddyfile` and print
   the plan — the liveswap root and each app's declaration — and takes
   that plan strictly, validating every field again;
4. empties and removes the staging directory of any app that no longer
   declares a backup, and empties whatever a restore or a drill that was
   killed had fetched;
5. for each app that declares a backup — in name order, but one whose
   last run `failed` goes last:
   - empties the app's staging directory, before anything else;
   - looks for `<root>/<app>/shared` on the real filesystem, by opening
     it. Only "no such file" is absence: `pending` if nothing has ever
     been backed up of the app, `data missing` if something has. The
     record says which; where it holds no snapshot of the app — a
     rebuilt box, a new disk, or an app that really is new — the
     repository is asked, on every run that finds it so (an answer of
     "none" is not kept: it could go stale), and if it cannot be asked
     the app is `failed`, not `pending`: `pending` is the one absence a
     run exits 0 on. The directory must belong to the
     `hotserve` user;
   - copies each declared database into staging with `VACUUM INTO`, and
     keeps a copy only if `integrity_check` says exactly `ok`;
   - uploads with `restic backup`;
   - lists the snapshot that upload made — by the id in its own summary
     — and looks for every declared item in it, because restic leaves
     out a path that vanishes while it runs, with exit 0;
   - empties staging again, whatever happened;
   - and, if the app is `ok` and no drill of it is on record — proven
     or failed — drills the snapshot it has just made (below): an app's
     first good backup, once; after that it is the drill's. That is the
     whole app fetched back, in plaintext, on a timer, so above 1 GiB
     restored the run leaves it to `hotserve-backup drill`, and says so;
6. lists the repository, once, for every app (`restic snapshots
   --no-lock`), and writes beside each snapshot the record names when
   it was last there (`seen`), and when the listing was answered
   (`listed`). Not after the repository has refused the run, and not
   with no snapshot on record to look for. A listing that fails is a
   warning: it changes no app's result, and makes no snapshot look
   gone. So is one restic had anything to say beside, on stderr: it
   leaves a snapshot it cannot load out of the listing and exits 0
   [measured, with a cold cache]. The listing unit's stderr goes to a
   file, not to the journal; what restic said of a listing that was
   not believed is kept in `/var/lib/hotserve-backup/listing.err`,
   root's to read, until a listing is answered — the record, which is
   everyone's to read, holds none of restic's words but the id of a
   snapshot it could not load — and the record says since when
   listings have gone unanswered (`unlisted`). A snapshot this run itself made, or
   fetched for its first drill, is judged by the next run's listing,
   not by this one's;
7. writes `/var/lib/hotserve-backup/status.json` — also when the run
   ended early, with why, and the apps it did not reach as `not run`.

An app that has left the plan with snapshots in the repository — a
block deleted, an import that stopped matching — is a warning of the
run that finds it gone, and of `status` until the next run: once. The
record then drops it, as its operator may have meant.

It exits 0 only if every app is `ok` or `pending`.

## Who runs as what

Every step is a transient systemd unit with a deny-by-default view: an
empty read-only root, the parts of `/usr` and `/etc` a program needs to
run, and what the step is given.

| Unit | Runs as | Network | Credential | Sees |
|---|---|---|---|---|
| the run itself | root | — | never reads it | its own state and run dirs |
| plan | `hotserve-backup`, own user+PID namespaces | no | no | `/etc/hotserve`, read-only |
| dump, clean | `hotserve`, own user+PID namespaces | no | no | that app's `shared/` (dump only) and staging |
| upload | `hotserve-backup`, `CAP_DAC_READ_SEARCH` | yes | yes | that app's declared paths and staged copies, read-only |
| verify | `hotserve-backup`, no capability | yes | yes | nothing of the app |
| size | `hotserve-backup`, no capability | yes | yes | nothing of the app |
| fetch | `hotserve-backup`, no capability | yes | yes | one empty directory, writable |
| hand-over | root, `CAP_CHOWN` and `CAP_DAC_READ_SEARCH` | no | no | that directory and nothing else |
| install, check | `hotserve`, own user+PID namespaces | no | no | what was fetched — writable: it is this run's scratch, and a copy closed to its owner is opened to be read — and (install) that app's `shared/` |
| unstage | `hotserve`, own user+PID namespaces | no | no | what was fetched, to remove it |
| mkshared | `hotserve`, own user+PID namespaces | no | no | the liveswap root, to make `<app>/shared` on a rebuilt box |

The run itself needs root, `CAP_SYS_ADMIN` and the host's own mount and
PID namespaces: it makes bind mounts that the manager then has to see.

- **What parses the app's bytes holds nothing.** `sqlite3` opens files
  the app chose, so it runs as the app's own uid in the app's own kind
  of sandbox, with no network and no credential, seeing one app.
- **The credential is never under the `hotserve` uid.** `restic` runs
  as an account of its own, so the server and every app can neither
  read its environment nor signal it. The one capability lets it read
  files it does not own, in a view that holds one app and nothing else.
  The run passes the *path* of `backup.env` to systemd, which reads it
  as root; the run itself never opens it.
- **What an operator or an app wrote stays off command lines**, with
  one exception. Units get the app's declaration as a root-written file
  bound at a fixed path, and declared paths as bind sources. Every
  property is sent to systemd typed, over D-Bus, and commands as
  `ExecStartEx` with `no-env-expand`: a directory called
  `$RESTIC_PASSWORD %h` is backed up under that name. The exception is
  the listing: the directory part of a nested declared path
  (`media` of `media/uploads`) is an argument to `restic ls`, after
  `--`, as `/backup/<app>/files/media`.
- **Root never reads what an app wrote.** It opens `shared/` and each
  declared path as `O_PATH` — a descriptor that names a file and cannot
  read it — makes each app's staging directory, makes mounts, and
  starts units.
- **An app cannot aim its backup at anything else.** A declared path is
  the app's own to replace, while it runs, with a link to a sibling's
  data; a bind source is a path systemd resolves as root, following
  links; and the upload unit reads any file it is shown. So the run
  opens each path itself, refusing any symbolic link on the way, and
  binds what it opened — `mount(2)` from `/proc/self/fd/<n>`, which the
  kernel follows to that very directory — onto a mount point in its own
  root-only directory. systemd is given the mount point. (Given
  `/proc/<pid>/fd/<n>` itself, systemd reads the link's text and walks
  the path again, and an app flipping the name wins that race now and
  then.)
- **The hardening is systemd's, on a kernel with seccomp.** Debian 13's
  has it; on one without, `SystemCallFilter=`, `RestrictSUIDSGID=` and
  `LockPersonality=` are warnings in the journal, not refusals, and the
  view and the accounts are what is left.
- **What a unit says is not believed because a unit said it.** The
  plan is decoded strictly and validated again; a dump result has to
  answer what was asked, in a class the run knows; and words that came
  from `sqlite3` — which hold names the app chose — lose their control
  characters and their length before they are kept or printed.

## A restore

`hotserve-backup restore <app> [--snapshot <id>] [--to <dir>]
[--no-pre-backup] [--yes]`, as root. It works from the repository and
the Caddyfile alone: a rebuilt box has no record, and needs none. What
that costs is below, under "what a restore cannot tell".

1. The lock, the leftovers and the plan, as for a run. The app has to
   declare a backup: the liveswap root is taken from the plan. The lock
   is the run's own, and is held from here to the end — the question
   below included — so a backup run that comes due meanwhile says a run
   is in progress, and does nothing. The question waits ten minutes for
   its answer, and no answer is a no; the backup made first, the fetch
   and the install are not bounded, so a large restore is hours in
   which no app is backed up. On one box that is the accepted cost: a
   restore is the one thing the box is doing.
2. The repository is asked for the app's snapshots (host `hotserve`, tag
   `app:<name>`). The newest that a backup run made is restored, or the
   one `--snapshot` names by its hex id, or by eight or more characters
   from the start of it — never `latest` — and only if it is among the
   app's. "Newest" is by the time restic recorded. Where the record
   says the last snapshot a run ended `ok` on is another one, the
   question and the report say so, and name it.
3. It asks, naming the snapshot and when it was made, and takes the
   app's name typed back — not "y", which a hand types on its own — for
   a yes. `--yes` answers; with nobody to answer it refuses. Nothing has
   happened yet.
4. **The app is backed up first**, by the steps of a run, so the restore
   can be undone: the snapshot is tagged `pre-restore` and named in what
   the restore prints, also when the restore then fails. It holds what
   was being restored over — the damage, as often as not — so it is
   restored only by name: it is never "the newest", to a restore or to a
   drill. If that backup does not end `ok` — the live database being the
   damaged thing, often — the restore stops and says so;
   `--no-pre-backup` restores without one. Where there is no
   `<root>/<app>/shared` there is nothing to back up or overwrite: a
   unit running as `hotserve` makes it, mode 0750 — and takes it away
   again if the restore then puts nothing in it, since an hourly run
   would take an empty directory for the app's data.
5. **fetch**: `restic restore <id>:/backup/<app>` into an empty directory
   under `/var/lib/hotserve-backup/restore`, as `hotserve-backup` with
   no capability. Not in a user namespace: there every `chown` restic
   tries fails in a way it does not overlook. Always `<id>:<path>`: a
   path the snapshot does not hold is then exit 1, where `--include`
   matching nothing is exit 0. Exit 0 is still not a restore: restic's
   summary has to say it restored something, and everything. **A fetch
   is the whole app, in plaintext, under `/var/lib/hotserve-backup`, and
   needs that much room there** — a drill too. So the repository is
   asked first how large the snapshot is restored (`restic stats --mode
   restore-size`), and a fetch that is larger than what is free there is
   not begun: the restore, or that app's drill, says how much it needs
   and how much there is. An install then writes the app a second time
   while the fetch is still there, and on the usual box that is the
   same disk — the one the live apps write to — so a restore into place
   or `--to` needs **twice** the snapshot free on one filesystem, or
   the snapshot on each of two. A drill, which installs nothing, needs
   the fetch alone.
6. **hand-over**: `chown -hR hotserve:` of that directory, by root, in a
   view that holds nothing else, with no network. By name, so a restore
   does not depend on the uid the snapshot was made under. It needs
   `CAP_DAC_READ_SEARCH` beside `CAP_CHOWN`: root without it cannot read
   a directory that is another account's and closed.
7. **install**, as `hotserve` in its own namespaces with no network and
   no credential, given what was fetched (writable: it is scratch, and
   opened to be read where the app kept it closed) and the app's
   `shared/` — pinned and bound by the run itself, as for an upload.
   **Every check comes before any change**:
   - the snapshot's own `plan.json` says which paths are databases — not
     the box's declaration, which may have changed. It is read strictly
     and validated as a declaration is anywhere;
   - each database copy has to be a SQLite file of which
     `integrity_check` says exactly `ok`;
   - every declared item has to be in the snapshot;
   - nothing may be in the way: a symbolic link at `shared/`, at a
     declared directory or on the way to it, or at a database, is
     refused; and so is a file that has a `-wal` beside it in place — a
     live database that this snapshot holds only as a file, which put
     back under the app would be read with a log that is not its own.
     Nothing here can stop the app: restore before it is first deployed,
     or `--to` a directory and move the file into place with the app
     stopped.

   If anything fails, nothing is installed — the files neither. Then a
   database is restored over the live one by SQLite's own backup, as one
   transaction, with the app running: a writer waits and carries on.
   (`sqlite3` is started as for a dump, and has the same five minutes to
   *open* the live database; in rollback-journal mode, where a reader
   and a writer exclude each other, a restore that stays locked out says
   so.) Where there is no live database the copy is put there, and any
   `-wal`, `-shm` or `-journal` left beside its name removed. Nothing
   here can stop the app, and one that re-creates its database meanwhile
   keeps writing to a file that is no longer there: with no database in
   place, restore before the app is first deployed. A copy is in
   rollback-journal mode; an app that wants WAL sets it when it opens. Files are written beside their names and renamed onto
   them, in directories opened without following a link: a link where a
   file goes is replaced, never written through — a file a killed restore
   left half written under its temporary name is listed as left in place,
   never removed: a name is not a reason — and a symbolic link the
   snapshot holds comes back as the same link, its text stored and never
   followed; a file with several names comes back as one file with those
   names. Not put back: setuid, setgid and sticky bits (the unit cannot
   set them), extended attributes and ACLs, and directories' times. A
   copy the app kept closed to its own owner is read all the
   same — what was fetched is this run's scratch, under a directory no
   app can enter — and the target gets the mode the snapshot held.
   Directories the restore makes get the modes they had, and one that is
   closed to writing in place is opened while it is filled. If something fails
   once installing has begun, the restore says the data is partly
   restored — never that nothing was changed — and the same restore, run
   again, goes over it.
8. **Nothing of the app's is ever removed.** What is in place under a declared path
   and not in the snapshot is left, and listed. What is in the snapshot
   and could not be app data and could not be put back — a FIFO, a
   socket, a device — is listed and not installed, and the restore is
   none the worse for it. A declared database is never installed from
   `files/`, whatever stands there.
9. What was fetched is removed, first and last, by a unit an interrupt
   does not reach; a run, a restore and a drill each remove what a
   killed one left.

`--to <dir>` does the same into a directory it makes (it must not
exist), owned by `hotserve` and laid out as `shared/` is. Every
directory on the way to it has to be root's own and writable by nobody
else — `/root`, `/srv`, `/var/backups`, a root-owned directory of your
own; not `/tmp`, and nothing an app's user owns — since the restore is
made where the name leads, and anyone who could write a directory on
the way could have put a link there first. Nothing is
asked and nothing backed up, since nothing is overwritten; and what is
sound lands even when something else is not — a damaged copy is never
handed out as a database — with a non-zero exit. It is how to look into
an `incomplete` snapshot, which cannot be restored into place.

### What a restore cannot tell

A run that ends `incomplete` because restic exited 3 — a file
unreadable, or gone while it was read, which is ordinary for a live
uploads directory — leaves a snapshot with files left out of a
directory it did read. Every check at install passes on it, and a
restore of it puts back fewer files than the previous `ok` one would,
and says it restored. On a box with a record the restore says when the
snapshot is not the last one a run ended `ok` on, and names that one.
On a rebuilt box there is no record and nothing in the repository says
how a run ended, so a restore cannot tell: that is the cost of needing
no record. `status.json` on the box that made the backups is what
knows.

## The restore drill

`hotserve-backup drill`, as root: for every app the repository holds a
snapshot of, the newest is fetched, handed over and checked exactly as a
restore checks it — and nothing is installed; no unit of a drill sees
any app's data. The record then says, per app, `restore_proven` (which
snapshot, and when it was proven) and, until the next drill that proves
one, `restore_drill` (which snapshot proved nothing, when, and why). A
drill that fails never replaces what was proven, a drill that is
interrupted records what it finished and nothing of the app it was
interrupted on, and a backup run carries both. It exits 0
if no drill failed; an app the repository holds no snapshot of has
nothing to prove. A `run` that drills — an app's first good backup —
says what the drill found on a line of its own.

## What a snapshot holds

One snapshot per app per run, host `hotserve`, tagged `hotserve` and
`app:<name>`, of `/backup/<app>`:

```
/backup/blog/plan.json          the declaration this snapshot was made from
/backup/blog/sqlite/app.db      the consistent copy
/backup/blog/files/uploads/…    the declared files
```

The paths are the same on every box, whatever its liveswap root, and
through a symlinked root the snapshot holds the data, not the link. Each
app is its own `(host, paths)` group, so a `forget` policy applies to
each app on its own.

A declared database that sits inside a declared `files` path is not
uploaded there, nor its `-wal`, `-shm` and `-journal`: its contents are
under `sqlite/`. Two things see to that. Each is masked in the upload
unit's view — an empty, unreadable file where it was, made by systemd
when the unit starts, so the live bytes are not there to read. And each
is named, exactly, in a root-written restic exclude file, because a mask
covers only what existed when the unit started: a database the app
creates, or puts back under its name, while restic walks is excluded all
the same. (restic reads an exclude line as a pattern and expands `$VAR`
in it, so every pattern character is escaped and every dollar doubled:
`app*.db` excludes itself, and not `appX.db`.)

## Databases

A declared path is handed to `sqlite3` only if it is, right now, a
regular file with a SQLite header, reached without following a symbolic
link anywhere on the way: a FIFO, a device, a directory, a link, a path
through a link, an empty file and a missing file are each reported and
never opened.

`sqlite3` is started on an in-memory database and `ATTACH`es the real
one as `file:<path>?mode=rw`. Given the path directly, the `sqlite3`
shell opens it read-only itself before SQLite does, and that open waits
for ever on a FIFO — which an app can put in place of its database
after the checks. By `ATTACH` a FIFO, a socket or a directory is an
error within milliseconds, and a missing file is not created. It runs
with `-init /dev/null`, because it otherwise reads `~/.sqliterc` and an
app's `HOME` is a directory the app writes, and with a 30-second busy
timeout.

**The one time bound.** A FIFO the app has made mode 0400 still blocks:
the read-write open is refused and SQLite retries read-only. So
`sqlite3` has five minutes to *open* a database — for the copy's target
file to appear, which takes some ten milliseconds whatever the
database's size, after a wait of at most the busy timeout — and is
killed if it has not. Once the target exists nothing bounds the copy,
and nothing bounds an upload.

## What a run says

`status.json`, per app: `ok`, `incomplete` (a snapshot exists and
something declared is not in it, or restic exited 3), `pending`, `data
missing`, `failed`, `not attempted`, `not run`; each declared item with
whether it was found in the snapshot; the snapshot's id; the last run
that was `ok`, and the last that made a snapshot at all — each with
`seen`, when the repository was last found to hold it. An app a run
did not reach is `not run`, with those two dates and nothing else:
never the last run's result under this run's date.

restic's exit statuses are reported as measured on 0.18: 10 is "no
repository", 11 "locked" (after `--retry-lock 2h`), 12 "wrong
password". 1 is "the storage could not be reached, or refused the key,
or something else" — never "no repository": a wrong storage key ends,
after about fifteen minutes of restic's own retrying, in exit 1 and a
message about a missing repository. After any of those four the run
does not try the remaining apps, which would each wait as long. An
exit 1 that is really about one app costs the apps after it that run
and no more: an app whose last run failed goes last.

How a copy ended is read from `sqlite3`'s exit status, which is
SQLite's result code (5 and 6 busy, 13 full), never from its words:
they hold the database's path, and `busy.db` is not busy.

## Status

`hotserve-backup status`, as anyone: `status.json` is world-readable
and holds no secret, and the manager tells any user which units are
running. It prints when the last run started — and that no run has
finished for more than 3 hours, where that is so: the run itself is
aged, or a record whose apps are all `pending` would read "no data
yet" for as long as no run replaced it — since when the repository
has gone unlisted, where it has, when a drill last ran, or why the last
one could not begin, and then per app:

- how the last run ended, where it looked for the app's data, and the
  last **complete** backup — the last run that ended `ok` — with its
  snapshot. An app that is `pending` has nothing else said of it;
- `stale` when that backup is more than 3 hours old: two hourly runs
  missed, and some grace. Freshness is the last complete backup's,
  whatever is running now and whatever a later `incomplete` snapshot
  holds;
- `snapshot … is no longer in the repository` when a listing answered
  since the snapshot was last seen there did not hold it — pruned or
  forgotten off the box, lost, or rewritten under a new id (`restic
  rewrite`, `restic tag`). A last complete backup that is gone is not a
  backup; a *proven* snapshot that is gone is said, and what was proven
  of it stays proven. With no listing answered since, nothing is said
  either way: "in the repository" is never claimed, only "gone";
- `restore last proven: <when>, snapshot <id>`, and `: old` when that
  is more than 8 days ago — the weekly drill's period and a day;
- `restore not proven: <why>` when the last drill proved nothing —
  also beside an older proof: the newest snapshot is the one a restore
  reaches for — or when no drill has run;
- and what is running: each unit of a run, a restore or a drill that is
  running, starting or stopping, by what it does, to which app, since
  when. A unit whose command is still running is `activating` to
  systemd, which is not failed. The process that starts those units is
  not one of them: between two units, `no unit … is running` is what
  is said. Where the manager cannot be asked, that is said instead.

It exits 0 only if the last run finished within 3 hours and did not end
early, the repository has not gone unlisted for more than 3 hours, the
last drill could begin, and every app that is not `pending`
was backed up by the last run (or not reached by it), has a complete
backup that is fresh and that no listing since has missed, and a
restore proven in the last 8 days with no failed drill since. With no
app declaring a backup there is nothing to fail. A box set up less than
3 hours ago and not yet run is `pending first run`, exit 0; after that
it is not. Anything else of those exits 1. What kept `status` from
looking at all — no `/etc/hotserve/backup.env` (backups are not set
up), a record that cannot be read — exits 3: not the same news as
backups that are unhealthy. (2 is the usage text's. These are
`status`'s own: its 3 has nothing to do with restic's exit 3, an
incomplete backup, which a run records as `incomplete`.)

That anyone may run it is meant. `status.json` is readable by every
account on the box, and so are the app names, data paths, snapshot ids
and cleaned error text in it; nothing of the repository's location or
credentials is.

## Before a Caddyfile goes live

`hotserve-backup validate <Caddyfile>`, as anyone: what a run's plan
step does to the live file, done to this one by whoever asks — it
adapts it with `/usr/bin/hotserve adapt` and no environment (below),
and refuses what that step would refuse, in the same words. It is not
run as the plan unit's account nor inside its view, so it also refuses
what those would make of the file once it is live:

- an import from outside `/etc/hotserve` — written relatively, from
  outside the Caddyfile's own directory, so that a copy can be checked
  with what it imports beside it. As the run does, it goes by how the
  import is written, by where the directories it names lead through
  links, and by where a link among the matched files leads. A line
  that begins with `import` inside a quoted token, a heredoc or a
  comment is not an import;
- under `/etc/hotserve`, an imported file that others may not read, or
  a directory that others may not list — `/etc/hotserve` itself, those
  the import names, those its wildcards lead through, and where a link
  among the matched files leads: a run reads the
  Caddyfile as the `hotserve-backup` account, which owns nothing
  there.

It names the apps a run would back up, and each app that declares no
backup — which is said, not refused; one named through a `{$NAME}` is
said by the variable, since what the server calls it is not known here. It starts no unit, takes no lock
and changes nothing.

`examples/box/bin/push` runs it on the staged file after `hotserve
validate`, where `/usr/bin/hotserve-backup` exists, so a Caddyfile that
would break backups is refused before it goes live and not by the next
hourly run. It needs no sudoers line.

## What it refuses

- A liveswap `root`, an app's name or a backup path that depends on a
  Caddyfile `{$NAME}`. (hotserve itself refuses `root {env.X}` together
  with a `backup` block; `{$NAME}` it never sees.) See below.
- A variable in an `import` — `import sites/{$ENV:prod}/*.caddy` —
  since which files the server reads when it is set cannot be known
  here, and a glob that matches nothing adapts with any value. Write
  import paths literally.
- A variable that no made-up value adapts with.
- A Caddyfile that imports from outside `/etc/hotserve`: the plan
  unit's view holds nothing else. Refused as the import is written,
  whatever it matches: inside that view a glob reaching outside matches
  nothing, which the adapter takes for no error, and the run would plan
  without the apps declared out there. A directory under
  `/etc/hotserve` that is a link leading out of it is outside too. A
  restore and a drill plan the same way, and are refused with the run.
- A Caddyfile that imports by a snippet's argument (`import {args[0]}`):
  the adapter fills the path in from wherever the snippet is used, and
  one of those uses can name files outside `/etc/hotserve`, which
  nothing here would see. Write the import's path literally.
- A symbolic link anywhere in a declared path, or at `<app>/shared`:
  declare the real path, and put data on another disk with a bind
  mount, as liveswap itself asks. (The liveswap root may be a link.)
- An `<app>/shared` that does not belong to the `hotserve` user.
- A declared `files` path that is a FIFO, a socket or a device: a backup
  keeps files and directories, and a restore could not put anything
  else back. (restic does record such a node where it finds one
  *inside* a declared directory; nothing reads it.)
- A run's leftover unit that will not stop within two minutes: the run
  is refused, and the record says why.
- A restore into place of a snapshot that lacks something its own
  `plan.json` declares, or holds a damaged copy: restore it `--to` a
  directory, or restore another snapshot.
- A snapshot whose `plan.json` is missing, is not a valid declaration, or
  holds a field this version does not know.
- `--to` a directory that exists, one under `/var/lib/hotserve-backup`
  or `/run/hotserve-backup`, one any directory on the way to which is
  not root's own or can be written by others, and `--to` or
  `--snapshot` given with nothing in it.
- A file that would land on a live database (above).
- A restore or a drill of a snapshot larger than what is free under
  `/var/lib/hotserve-backup`, and a restore of one larger than half of
  it where the app's data is on the same filesystem.
- A run's own first drill of a snapshot above 1 GiB restored: that one
  is `hotserve-backup drill`'s.
- A unit whose state cannot be read ten looks running (five minutes) is
  stopped, and that step fails.

## The Caddyfile is read without the server's environment

The plan comes from `/etc/hotserve/Caddyfile`, which is root's, and not
from the running server: what gets backed up is not for the process the
internet talks to to say. The cost is `{$NAME}`, which Caddy fills in,
in the text, from the environment of whoever adapts the file — and a
run does not have the server's environment, where its secrets are.

So a run adapts the file with made-up values. A variable written
somewhere with no default would otherwise leave nothing where its value
goes — `email {$ACME_EMAIL}`, unset, is a parse error — so each gets a
placeholder of its own, and where the adapter refuses one (it quotes the
value: `invalid port 'hsb-placeholder-metrics-port.invalid'`) the next
kind is tried there: a number, a duration, a path. Each made-up value
is one token, so what is adapted has the structure the Caddyfile has on
the page, and none of those values is anywhere a plan looks.

Then every variable is given a second value. If the plan comes out
different — the root, an app's name or a backup path depends on it — the
run is refused, naming the variable: the server and the backup would
otherwise look in different places, and the backup would say "no data
yet" for ever. Write those three things literally; a per-box `import` of
a file with a literal `root` does what a variable would.

If no value tried adapts, the run fails with the adapter's own error,
which names the file and line; a default that adapts
(`{$NAME:value}`) settles it.

**What this cannot see.** Caddy substitutes `{$NAME}` into the text
before it reads a single token, so a value is not bound to be one token:
with a space in it `command {$CMD}` is two arguments, and with a newline
in it, it is more lines of Caddyfile — `CMD` set to `./server`, a
newline, and a `backup` block gives the server's `blog` a declaration
that is nowhere in the file [measured]. One-token values cannot show
that no value would do that, and the values the server has are not
known here. So: **a declaration that exists only through the server's
environment is not backed up, and nothing says so.** That environment
is root's to write (`hotserve.service`), like the Caddyfile's own
directory; keep what it holds to values, and what is declared in the
Caddyfile.

## By hand, until there is a setup command

```
# /etc/hotserve/backup.env — root:root 0600
RESTIC_REPOSITORY=s3:https://…/bucket
RESTIC_PASSWORD=…
AWS_ACCESS_KEY_ID=…
AWS_SECRET_ACCESS_KEY=…
```

and an account for restic: `useradd --system --no-create-home
--home-dir /nonexistent --shell /usr/sbin/nologin hotserve-backup`.
Initialise the repository once with `restic init`, using a cache
directory of your own (`RESTIC_CACHE_DIR`) so that nothing of root's is
left under `/var/cache/hotserve-backup`.

## Development

- `make test` — everything that needs neither systemd nor sqlite3: the
  plan, the record, the run's flow and failure classes against a
  scripted runner, the file inspection.
- `make test-integration` — as root against a real system manager and
  Debian's `sqlite3`: what a unit's view holds for every way of saying
  who runs it, that arguments are never expanded, a cancelled run's
  unit confirmed gone, a unit ended with its SIGKILLed orchestrator;
  and sqlite3 itself — a live WAL database, what is swapped in after
  the checks, a read-only FIFO killed at the open bound, a planted
  `.sqliterc`.
- `make test-integration` also races an app flipping a declared path
  between its directory and a link to a sibling's, as fast as it can,
  against real units starting with the read capability.
- and an app flipping the directory restored into, and the database
  restored over, between the real thing and a link to a sibling's while
  real install units run; `restic restore`'s summary and exit statuses;
  a restore over a live database under a writer, and over one with
  damaged pages; a runner made with the context that is then cancelled;
  what the manager lists as running, and since when.
- `make e2e-backup` — a box with systemd, restic and sqlite3, and an S3
  server (`rclone serve s3`): the backup suite, the status suite
  (`status` and `validate`) and the restore suite, mostly failure
  paths.
