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
(below), and Debian's `restic` and `sqlite3` installed.

## A run

`hotserve-backup run`, as root:

1. takes the run lock — one run at a time; a second says who holds it;
2. stops, by exact name, any unit an earlier run recorded and did not
   live to stop;
3. has an unprivileged unit adapt `/etc/hotserve/Caddyfile` and print
   the plan — the liveswap root and each app's declaration — and takes
   that plan strictly, validating every field again;
4. for each app that declares a backup:
   - looks for `<root>/<app>/shared` on the real filesystem. Only "no
     such file" is absence: `pending` if the app has never been backed
     up, `data missing` if it has;
   - empties the app's staging directory;
   - copies each declared database into staging with `VACUUM INTO`, and
     keeps a copy only if `integrity_check` says exactly `ok`;
   - uploads with `restic backup`;
   - lists the snapshot that upload made — by the id in its own summary
     — and looks for every declared item in it, because restic leaves
     out a path that vanishes while it runs, with exit 0;
   - empties staging again, whatever happened;
5. writes `/var/lib/hotserve-backup/status.json`.

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

- **What parses the app's bytes holds nothing.** `sqlite3` opens files
  the app chose, so it runs as the app's own uid in the app's own kind
  of sandbox, with no network and no credential, seeing one app.
- **The credential is never under the `hotserve` uid.** `restic` runs
  as an account of its own, so the server and every app can neither
  read its environment nor signal it. The one capability lets it read
  files it does not own, in a view that holds one app and nothing else.
  The run passes the *path* of `backup.env` to systemd, which reads it
  as root; the run itself never opens it.
- **Nothing an operator or an app wrote reaches a command line.** Units
  get the app's declaration as a root-written file bound at a fixed
  path, and declared paths as bind sources. Every property is sent to
  systemd typed, over D-Bus, and commands as `ExecStartEx` with
  `no-env-expand`: a directory called `$RESTIC_PASSWORD` is backed up
  under that name.
- **Root never reads what an app wrote.** It `lstat`s declared paths,
  makes each app's staging directory, and starts units.

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

A declared database that sits inside a declared `files` path is masked
there, with its `-wal`, `-shm` and `-journal`: under `files/` it is an
empty, mode-0 file, and its contents are under `sqlite/`. The live file
is never what is uploaded.

## Databases

A declared path is handed to `sqlite3` only if it is, right now, a
regular file with a SQLite header, reached without following a link out
of `shared/`: a FIFO, a device, a directory, a symbolic link, an empty
file and a missing file are each reported and never opened.

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
missing`, `failed`, `not attempted`; each declared item with whether it
was found in the snapshot; the snapshot's id; and the last run that was
`ok`.

restic's exit statuses are reported as measured on 0.18: 10 is "no
repository", 11 "locked" (after `--retry-lock 2h`), 12 "wrong
password". 1 is "the storage could not be reached, or refused the key,
or something else" — never "no repository": a wrong storage key ends,
after about fifteen minutes of restic's own retrying, in exit 1 and a
message about a missing repository. After any of those four the run
does not try the remaining apps, which would each wait as long.

## What it refuses

- A liveswap `root` or a backup path that depends on a Caddyfile
  `{$NAME}`. A run adapts the Caddyfile without hotserve's environment,
  so it tries each variable the file and its imports mention, and
  refuses, naming the variable, if setting it changes the plan. (hotserve
  itself refuses `root {env.X}` together with a `backup` block.)
- A Caddyfile that imports from outside `/etc/hotserve`: the plan
  unit's view holds nothing else.
- A declared database path that is itself a symbolic link: declare the
  file it points to.

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
- `make e2e-backup` — a box with systemd, restic and sqlite3, and an S3
  server (`rclone serve s3`): mostly failure paths, each of which has
  been seen to fail against an engine broken in the way it is there to
  catch.
