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

**On this branch it is the engine only.** `hotserve-backup run` does one
backup run. There is no setup command, no timer, no restore and no
package yet: a run needs `/etc/hotserve/backup.env` written by hand
(below), Debian 13's `restic` and `sqlite3` installed, and systemd 257
(`PrivatePIDs=`), which is Debian 13's.

## A run

`hotserve-backup run`, as root:

1. takes the run lock — one run at a time; a second says who holds it;
2. stops, by exact name, any unit an earlier run recorded and did not
   live to stop, and takes away any mount it left;
3. has an unprivileged unit adapt `/etc/hotserve/Caddyfile` and print
   the plan — the liveswap root and each app's declaration — and takes
   that plan strictly, validating every field again;
4. empties and removes the staging directory of any app that no longer
   declares a backup;
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
6. writes `/var/lib/hotserve-backup/status.json` — also when the run
   ended early, with why, and the apps it did not reach as `not run`.

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
- **What a unit says is not believed because a unit said it.** The
  plan is decoded strictly and validated again; a dump result has to
  answer what was asked, in a class the run knows; and words that came
  from `sqlite3` — which hold names the app chose — lose their control
  characters and their length before they are kept or printed.

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
that was `ok`, and the last that made a snapshot at all. An app a run
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
  unit's view holds nothing else.
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
kind is tried there: a number, a duration, a path. The structure of what
is adapted is the server's own; only those values differ, and none of
them is anywhere a plan looks.

Then every variable is given a second value. If the plan comes out
different — the root, an app's name or a backup path depends on it — the
run is refused, naming the variable: the server and the backup would
otherwise look in different places, and the backup would say "no data
yet" for ever. Write those three things literally; a per-box `import` of
a file with a literal `root` does what a variable would.

If no value tried adapts, the run fails with the adapter's own error,
which names the file and line; a default that adapts
(`{$NAME:value}`) settles it.

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
- `make e2e-backup` — a box with systemd, restic and sqlite3, and an S3
  server (`rclone serve s3`): mostly failure paths.
