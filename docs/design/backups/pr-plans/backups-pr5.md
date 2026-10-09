# PR 5 — the repository check, and the clean-run record

Branch `backup-check` from `origin/backup` at `a594aea` (#155 and #159
merged into `backup`, `main` merged into `backup`: checked first).

**The owner's answers, before anything was built (2026-09-29):**
the review loop fixes **every genuine finding** here · the check runs
**`--no-lock`** · on a rebuilt box a restore **keeps the newest snapshot
and says** whether a clean run vouches for it, naming the newest one
that is.

**Decided by me, said in one line each to the owner before building:**
the group is n = ((ISO week − 1) mod 52) + 1 · inside `drill`: the
drill as it is, then probe → check → a second probe only after a check
that failed; the probe gates the check alone · the probe's clock is
30 s · `status.json` gains `last_check`; `status` is unhealthy on any
verdict but `clean`, and on no clean check within 8 days of setup ·
damage is reported and backups go on; the box never repairs · the
clean-run record: one per app per run that ended ok, its own retention
group, read only by `restore` where the box's record has no last ok ·
D4 is settled by the owner's rule on the numbers below, and a backstop,
if the rule says one goes in, is put to the owner before code.

---

## 1. Measured first (restic 0.18.0, debian:13, arm64, the rclone S3 fixture)

| # | Question | Result |
|---|---|---|
| M73 | What `check --json` says, and how it exits | One line on stdout, `{"message_type":"summary","num_errors":0,"broken_packs":null,…}`, exit 0. **The same summary with `num_errors: 1` is printed where it could not open the repository at all** — exit 12 (wrong password, <1 s), exit 10 (no repository, <1 s), or a host that does not resolve (still retrying at 120 s, `context canceled` when stopped). → `num_errors` is never read alone. Damage found: exit 1, `broken_packs` names them, stderr `error` lines and `exit_error` "Fatal: repository contains errors". |
| M74 | Bit rot inside a pack, and which group reads it | 16 bytes overwritten inside a 10.8 MB data pack (size unchanged): the structure-only check **exit 0**; of 52 groups **only 8/52 exit 1** — the pack id begins `3b` (59; 59 mod 52 = 7). The group is the pack id's first byte mod 52, plus one. A pack grown or shrunk is found by the structure check itself (size mismatch). |
| M75 | Locks, a check stopped, a check killed | A running `check` (exclusive) makes a `backup` exit 11 at once; `--retry-lock 5s` waits and succeeds. SIGTERM: exit 130, lock removed, temporary cache removed. **SIGKILL leaves the exclusive lock and the `restic-check-cache-*` directory**; the next backup from the same host (PID dead) exits 11 at once; **a backup with `--retry-lock 40m` against that lock, 40 min old, still exits 11 after 2401 s** — restic 0.18 never passes a stale lock by itself; `restic unlock` removes it (same host, dead PID). **`check --no-lock` writes no lock**, and runs beside an exclusive check. → the owner: `--no-lock`. |
| M76 | A record of a clean run | `backup --stdin-from-command --stdin-filename hotserve-clean-<app> --tag hotserve-clean --tag vouches:<id> -- /bin/true` → **exit 3** ("no data read") and a snapshot all the same. `-- /usr/bin/echo <id>`: exit 0 in 0.8 s, 409 bytes packed; paths `["/hotserve-clean-<app>"]`; `snapshots --tag app:<app>` leaves it out; a command that fails → exit 1 and no snapshot. |
| M77 | The tens-of-GB repository [H §6, plan §5] | 28.5 GB raw (27 GB on disk), 224,840 blobs, 1,506 snapshots (five 5 GiB streams, a 200,000-file tree, 3 × 500 hourly snapshots of small apps). Local network: `cat config` 0.5–0.8 s · `snapshots` of all 1,506 0.6–1.4 s (cold cache 1.35 s, 2 MiB fetched, cache 7 MiB after) · `ls` of a 200-child directory 2.5–3.1 s, 23 MiB fetched cold · structure-only `check --no-lock` 2.3–2.9 s · `check --no-lock --read-data-subset=n/52` 3–5 s, **571–647 MiB fetched (1/52 of the data)**, peak RSS 181 MiB · the temporary cache a check makes: 47 MiB (left behind on SIGKILL) · persistent cache 48 MiB. (A first check after the fixture restarts took 62 s: rclone's own cold listing of ~30,000 objects, not restic's; repeated, 2.3 s.) Over an emulated remote link: see M78. |
| M78 | The same over 25 ms each way and a 50 Mbit/s download cap (`tc netem` on the fixture and the client) | `cat config` 0.9 s · `snapshots` of 1,506 **19.6 s cold**, 1.3 s warm · `ls` **24.6 s cold**, 3.2 s warm · structure-only check 59 s · `check --no-lock --read-data-subset=n/52` **155–217 s**, 583–660 MiB read, peak RSS 168 MiB. |
| M79 | `--cleanup-cache` and a killed check's temporary cache | A fresh `restic-check-cache-*` is left; backdated 31 days, the next command with `--cleanup-cache` removes it. → the check passes `--cleanup-cache`: what hard kills leave is bounded to a month's. |

## 2. Starting states × what the check can meet (closed)

The check runs inside `drill`, under the run lock, after the drill's
apps. **Columns** are everything the check can meet; **rows** every
state a drill can begin from.

**Columns — what the check meets, and the verdict it writes:**

| | Probe (`cat config --no-lock`, 30 s) | Check (`check --no-lock --json --read-data-subset=n/52`) | Probe after | Verdict (`last_check.class`) |
|---|---|---|---|---|
| c1 | exit 0 | exit 0, summary `num_errors` 0 | — | `clean` |
| c2 | exit 0 | exit 1 | exit 0 | `damaged` (the count and broken packs from the summary; the journal has restic's words) |
| c3 | exit 0 | exit 1 | anything but 0 | the after-probe's class, "the check could not finish" |
| c4 | exit 10 | not run | — | `no repository` |
| c5 | exit 12 | not run | — | `wrong password` |
| c6 | exit 1, or no answer in 30 s (unit stopped) | not run | — | `unreachable` ("could not be reached, or refused the key") |
| c7 | exit 0 | exit 0, summary missing, unreadable, or `num_errors` > 0 | — | `failed` ("not believed") |
| c8 | exit 0 | exit 10 / 12 (changed since the probe) | — | `no repository` / `wrong password` |
| c9 | the unit could not be set up (2xx), ended by a signal, any other exit, or the runner lost it | or the same of the check | — | `failed`, in `resticFailure`'s words |
| c10 | an interrupt (the drill's context) during either | | | **no verdict**: `last_check` stays as it was |
| c11 | exit 0 | killed hard (SIGKILL, power) | — | no verdict (the command died with it); **nothing is left that blocks a backup** (M75, `--no-lock`); its temporary cache (~47 MiB at 28 GB) stays in `/var/cache/hotserve-backup` |
| c12 | exit 0 | exit 11 | — | cannot happen under `--no-lock`; were it to: `failed`, `resticFailure`'s words |

**Rows — where a drill begins, and what that changes:**

| | Starting state | The check | Status before → after |
|---|---|---|---|
| r1 | No `last_check` (a new setup, or a record from before this PR) | runs; c1–c12 | before: "no repository check yet" (said), or unhealthy once setup is more than 8 days old; after: the column's verdict |
| r2 | Last check `clean` | runs | old after 8 days (unhealthy); then the column's |
| r3 | Last check not clean | runs | unhealthy until a `clean` one |
| r4 | A hard-killed check left its temporary cache | runs (a new temporary cache beside it) | the column's; the leftover is said nowhere — see §4 |
| r5 | The repository holds no snapshot yet | runs; restic checks an empty repository (c1) | the column's |
| r6 | The record could not be read (replaced by `open`, said as the warning) | runs; `last_check` was lost with the record | as r1 |
| r7 | `begin` refused (no credential file, a program not installed, the account, the manager not seeing mounts) or the plan could not be made | **not run**: the drill ends at `couldNotBegin`, as today | `last_check` kept; the drill's own failure is unhealthy already |
| r8 | Interrupted, or a helper of another version (`x.upgraded`), during the apps | **not run** | `last_check` kept |
| r9 | Another command holds the run lock | **not run**: `drill` exits "another backup run is in progress", as today | unchanged |
| r10 | A run starting while the check runs | the run meets the run lock and does nothing for that hour (as it does a drill today); the check holds no restic lock | — |
| r11 | An off-box `prune`/`forget` running during the check | the check can see packs vanish: c2 `damaged` though nothing is | a false `damaged` until the next clean check — the owner's accepted cost of `--no-lock` |
| r12 | A run, a restore, or setup later | carry `last_check` unchanged (`open` copies it, as `last_drill`) | unchanged |

## 3. A failure row for every new call to another program

| Call (unit, as `hotserve-backup`, network, the credential file, the cache) | Fails how | What is done |
|---|---|---|
| `restic cat config --no-lock` (probe) | restic not installed | `begin` refuses first (programsInstalled): r7 |
| | exit 10 / 12 / 1 / clock / 2xx / signal / runner error | c4 / c5 / c6 / c6 / c9 / c9 / c9 |
| | interrupt | c10 |
| `restic check --no-lock --json --read-data-subset=n/52` | exit 0 with no summary, or a summary with errors | c7 |
| | exit 1 | the after-probe decides: c2 / c3 |
| | exit 10 / 12 | c8 |
| | 2xx / signal / other / runner error | c9 |
| | killed hard | c11 |
| | interrupt | c10 |
| `restic cat config --no-lock` (probe after) | anything but exit 0 | c3 |
| `restic backup --quiet --json --retry-lock 2h --host hotserve --tag hotserve-clean --tag vouches:<id> --stdin-from-command --stdin-filename hotserve-clean-<app> -- /usr/bin/echo <id>` (the record) | `/usr/bin/echo` missing (coreutils is Essential) → restic exit 1 "command failed", no snapshot | a warning on the run: "<app>: the record that this run ended ok could not be written into the repository …"; **the app stays `ok`** — the backup is sound; only a rebuilt box loses the word |
| | any exit but 0, no summary id, 2xx, signal, runner error | as above |
| | interrupt | nothing more of the run is done (as after any unit) |
| `restic snapshots --no-lock --json --host hotserve --tag hotserve-clean` (restore on a rebuilt box) | any failure | a warning on the restore ("whether a clean run vouches for snapshot … could not be asked: …"); the restore goes on, choosing as it would have |
| | answered, and nothing vouches for any snapshot of the app | said: "nothing in the repository says how the run that made … ended" |
| | answered, the pick not vouched, an older one vouched | said with the words a box's own record gives today (`notLastOK`), naming the vouched one and `--snapshot` |
| | answered, the pick vouched | nothing said |

## 4. Closed since

- The temporary cache a hard-killed check leaves (c11, r4): ~47 MiB at
  28 GB; the check passes `--cleanup-cache`, which removes one once it
  is 30 days old (M79).
- D4: settled — §6.

## 4b. What changes, file by file

- `record`: `Check{Time, Group, Class, Detail}`; `Status.LastCheck`
  (`last_check`); the check's classes.
- `engine/check.go` (new): `checkGroup(time)`, the probe, the check,
  the verdict (§2's columns), `startWithin` (a clock, setup's shape).
- `engine/engine.go`: `open` carries `LastCheck`; `app` writes the
  clean-run record after an `ok`; `snapshots` and `verify` under the
  30-minute backstop.
- `engine/restore.go`: `Drill` checks after its apps; `Restore` asks the
  repository's records where the box's own has no last ok.
- `status`: the check's line and its part in health.
- `cmd/hotserve-backup`: `drill` says the check's verdict and fails on
  any but `clean`; `restore`'s question and report say what vouches.
- README: the check, the record, what a rebuilt box can now tell,
  status's rules, the backstop.
- e2e: a sixth suite, `check.sh` (parallel, so it costs its own length).

## 5. Tests first — each seen failing, for the reason it names

Against stubs that compile (types, and functions that do nothing):

| Test | Seen failing with |
|---|---|
| `TestTheCheckReadsItsWeeksGroup` | `2026-12-31: group "", want "1/52"` … `52 weeks in a row read 1 groups, not every one` |
| `TestWhatTheCheckMeets` (15 rows, c1–c12) | every row: `no verdict: &{… LastCheck:<nil> …}` |
| `TestTheCheckTakesNoLockAndReadsItsWeeksGroup` | `no repocheck unit was started (started: plan history unstage size fetch handover check unstage)` |
| `TestAnInterruptedCheckLeavesTheLastOne` (probe, repocheck) | `the drill's error: <nil>` — no check to interrupt |
| `TestEveryCommandCarriesTheLastCheck` (run, restore) | `the last check on disk: <nil>, <nil>` |
| `TestAnOKBackupIsVouchedForInTheRepository` | `units: plan clean dump upload verify clean size unstage … want the record after the backup is verified and its copies removed`; `no vouch unit was started` |
| `TestOnlyAnOKRunIsVouchedFor…` (record not written, no id) | `vouched for: false, want true`; `warning "", want "blog: the record that this run ended ok could not be written into the repository"` |
| `TestARestoreOnARebuiltBoxSaysWhatVouches` (6 rows asking) | `the repository's records asked for: false, want true`; `the report names  as the last ok, want bbbbbbbb`; `the report says "", want "nothing in the repository vouches for any snapshot of blog"` |
| `TestAListingThatDoesNotAnswerIsGivenUpAtItsBackstop` | history: the drill ran to the test's own 10 s bound (`shop: <nil>`); verify: `could not be checked: context deadline exceeded`; listing: `warning ""` |
| `status` `TestWhatStatusSays` (6 new rows) | `does not say "the repository has not been checked yet: …"`; `healthy = true, want false` for nine days unchecked, damaged, unreachable, old |
| `TestIntegrationTheCheckFindsARottenPackOnlyInItsGroup` | `the summary "using temporary cache in …create exclusive lock for repository…"` — not `--json` |
| `TestIntegrationTheCheckWritesNoLock` | `restic ["check" "--read-data-subset=1/52"] did not end within 20s: List(lock) returned error, retrying … repo/locks: not a directory` |
| `TestIntegrationACleanRunRecordIsASnapshotOfItsOwn` | `the record: exit 1, "", stderr "…Fatal: nothing to backup…"` |

Rows that assert an absence passed against the stubs; each was then
seen failing against a deliberately wrong implementation, and the file
put back byte for byte:

| Row | Mutation | Seen failing with |
|---|---|---|
| `TestNoCheckWhereTheDrillCouldNotGoOn` (3 rows) | check in `couldNotBegin` and after an upgrade | `checked where the drill could not go on: probe repocheck` / `plan probe repocheck` / `… check unstage probe repocheck` |
| `TestOnlyAnOKRunIsVouchedFor…` (exit 3, item absent, copies left) | vouch any app with a snapshot | `vouched for: true, want false (plan clean dump upload verify clean vouch listing)` |
| `TestAReportHoldsNoControlCharacters` | the check's line without `record.Text` | `a control character reached the terminal: "the repository check of … dam\x1b[2Jaged: \x1b]0;x\afound"` |

**e2e, `check.sh`** (the sixth suite, parallel), against `origin/backup`'s
engine built from `git archive` into `build/old-src`: 16 assertions
failed for the reasons named — `the records vouch for: ;`, `the drill
said: blog: restore proven …` (no `repository:` line), `last_check: `,
`a drill with the wrong storage key exited 0`, `no restic check was
seen to hold`, and the question with no word of what vouches; one was a
fixture bug of the suite's own (its `grep backup` matched the
Caddyfile's comments), fixed. Check 6 ("a check killed hard"), with the
check's argv stripped of `--no-lock`: first written to assert no lock
was left, it **passed against the locking check** — `hold_restic`
stops restic before it has written a lock, and on a repository that
small a locking check holds its lock for less than a 20 ms step
(measured stepping one). So that row is not in the suite: the no-lock
property is `TestIntegrationTheCheckWritesNoLock`'s, deterministic. What
check 6 holds instead — a check killed hard is `failed`, not
`damaged`; no unit left; the next run backs up — was seen failing
against a check whose signal went down the exit-1 path (`the drill
said: … ` without "restic was ended by signal"). Two rows that could
not fail for their reason (no lock after a check that ended normally)
were taken out. Green on the new engine: `ALL CHECK SCENARIOS PASSED`,
126–130 s.

## 5b. Lanes

At `145f90c`: `make test` 0 · `make lint` 0 issues in four modules ·
`make secretscan` — **one leak found**: `check.sh`'s `s3_delete` had the
fixture key literally in `curl --user` (rule `curl-auth-user`); it now
reads the key from the credential file as `rr` does, the unpushed commit
amended, no leaks · `make test-integration` 79 passes · `make package` ·
`make install-test` (129 s) · `make e2e` all suites · `make e2e-backup`:
**two suites red, both fixtures meeting the records** — the backup
suite's retention scenario counted every snapshot (6 → 12 with the six
records), the status suite looked for a forgotten id anywhere in the
listing (a record's `vouches:` tag still names it). Fixed to ask what
they mean (`--tag hotserve`, `"id":"…"`), and the retention scenario
now holds the records to a group per app too — seen failing against
records sharing one name (`records: 6 before; after:
"paths":["/hotserve-clean"]`). At `ffbb36a`: `make test`, `secretscan`,
and `make e2e-backup` whole: six suites, all passed (setup 180 s,
backup 93, units 121, status 85, restore 199, check 125).

## 5c. `/code-review xhigh` at `ffbb36a` (2026-09-30): fifteen findings, ranked before any work

All fifteen judged against the code. Fourteen genuine, all fixed (the owner's bar). One half-finding was answered instead: `probe` duplicating setup's look — setup's uses the staged credential file and speaks to a terminal, and what the two share, the clock, is already one `startWithin`. Two went to the owner first:

- **the record's unit bounded at 30 min**, as a listing is (a new limit; the owner: yes);
- **the group read is the one after the last read**, kept as `check_read`, the ISO week's only for a first check (departs from "n from the ISO week"; the owner: yes).

| # | Fix (a row in its table) | Seen failing with |
|---|---|---|
| 2 | records listing keeps stderr, `besides` as the run's listing | `the report names bbbbbbbb as the last ok, want ` · `the question says "", want "not believed"` |
| 3 | record at the snapshot's time (`--time`, `TZ=UTC`, from verify's `ls` line) | unit: the argv without `--time`, `the record's time is read in the unit's own zone` · e2e against `ffbb36a`: `blog's snapshot was made 2026-09-30T06:34:55, its record is at 2026-09-30T06:34:57` |
| 1 | record's unit under `listClock` | `the record, in a run`: the run ran to the test's own 10 s bound |
| 4 | probe stdout to the run dir | `the probe's stdout goes to "", not a file under …/run` |
| 5 | drill says a check it made by the record | `the clock stepped back: made false, want true` |
| 6 | `nextGroup`, `check_read` | `after "40/52": group "39/52", want "41/52"` · `the check read "--read-data-subset=40/52", want group 41/52` · e2e against `ffbb36a`: the second check's line not of `$NEXT` |
| 7 | damage beside a met lock says so | `&{… Class:damaged Detail:restic check found 1 error; …}` without it; its "not said where nothing did" half against a note always added |
| 8 | "no check" unhealthy only with no drill in 8 days | against the old rule: `healthy = false, want true` |
| 9 | e2e: the count as a pattern | loosening — nothing to fail |
| 10 | listing `--tag hotserve` | `the listing: [… "--host" "hotserve"]` |
| 11 | warning names the snapshot first | `the warning: "aaaa…aaa: the record that this run ended ok could not be written …` (id past the cut) |
| 12–15 | `resticUnit`, `snapshots` with a tag, `CheckOldAfter = ProvenOldAfter`, the subtest's `t` | refactors: the whole suite, unchanged, is what holds them |

Absence rows seen failing against mutations: the group moved on by any verdict (`unreachable`, `killed by a signal`: `last group read: "41/52" … want "40/52"`). An older test asserted the listing had no `--tag` at all; it meant no app's tag, and now says so.

## 5d. Copilot's reviews of #161

**First, at `4f08d47`:** a first check's group from the clock's own zone, not UTC. Taken in `eb435c6` (`checkGroup` reads `t.UTC()`; a row, Monday 01:00 at UTC+2 is week 39, seen failing as `40/52`).

**Second, at `eb435c6`** — and the owner, 2026-09-30: "looks like you introduced some bugs, and Copilot found one that was missed". Three, all fixed, each a row seen failing first:

| Bug | Where it came from | Seen failing with |
|---|---|---|
| The drill command read `status.json` before the engine took the run lock, and took a changed last check for this drill's own; a drill that made none behind another that wrote one in that window printed that one, and failed on it | **mine, the review round** (finding 5's fix) | stubbed as the command's rule (`Drill` returning the record's last check): `an interrupted drill says it made a check: &{… Group:38/52 Class:damaged …}` · `a drill that made no check says it made &{… Class:clean}` (3 rows) |
| A first check's group and its time were two readings of the clock; either side of Sunday's midnight UTC they disagree (Copilot, inline) | the review round (finding 6's fix) | with the clock injected and still read twice: `a first check read group "39/52" and is recorded at 2026-09-28 00:00:00.001 +0000 UTC, which is week 40/52` |
| A damage verdict with the lock note ran past `record.Text`'s 300 runes at double-digit counts (306 at 12 errors in 12 packs) and lost its end | **mine, the review round** (finding 7's fix) | `cut, or not all said: … 9999 errors, in 9999 damaged packs; …` |

Fixes: `Drill` returns the check it made (nil for none) and the command prints only that — nothing read outside the lock, `newCheck` gone; `checkClock` read once for the group and the time; the journal pointer shortened to "restic's own words: `journalctl -u …`", ~245 runes at 9999/9999.

**Third, at `ea84a05`** (overview, "previously missed"): a run stopped while writing a record returned in silence. The owner asked for a balanced view first (low likelihood, a bounded effect, near-zero cost), then "fix it carefully". Fixed in `bf8c387`: success checked first and silent; a stop under way "may not have been written: the command was stopped while it was being written"; a failure restic's own. Rows: stopped under way and not confirmed gone (`warning ""` before); failed as the stop came (silent before); written as the stop came, the guard of the order. Mutations: the stop checked first warns of a written record and calls a failure a stop; the case deleted says "(context canceled)". e2e check 8, a real run held mid-record, SIGINT, restic let go once the manager is stopping its unit: `the record's warning: ` and `status: …` failing before, six rows passing after. Every lane green at `bf8c387`.

## 5e. The owner's `/code-review xhigh` of #161 at `bf8c387` (2026-09-30): twelve findings

All twelve checked against the code; all genuine. The owner's answers: **every restic unit runs in UTC** (finding 1, measured: a Berlin box's snapshot at `+02:00` and its record at `Z` land in different `forget` days, and `--keep-daily` removed the record of the kept daily) · **a damaged group is read again until clean** (2) · **`restic stats` measured, then bounded if far below** (3) · **one record per app per run kept** (6), and — found discussing it — **a restore's own repository reads have no backstop** (someone is there to stop it; a rebuilt box's empty-cache listing is the longest a box makes, and without retention would pass 30 minutes in about a year), with a README section on retention.

| # | Fix (a row in its table, seen failing first) |
|---|---|
| 1 | `TZ=UTC` in `resticUnit`: every restic unit; e2e check suite's box in Europe/Berlin |
| 2 | `check_read` moves on only on `clean`; e2e: a second drill after damage reads the same group, and says damaged still |
| 9 | a verify or listing given up at its clock — not confirmed gone included — is the whole repository not answering: the run stops asking |
| 3 | `stats` under the backstop in runs and drills (after M80), repository-wide at the clock |
| 7 | a stop between an app's ok and its record: "was not written: the command was stopped before it was begun" |
| 8 | only restic's own non-zero exit is "could not be written"; a clock, a stop, a kill, an exit 0 with no id: "may not have been written: why" |
| 5 | the record's `--retry-lock` 20m, below its 30-minute clock; exit 11 in its own words |
| 4 | `metLock` gone; every damage verdict says a prune off the box during the check looks the same, and that the next check reads the group again |
| 10 | a restore interrupted while the records are asked stops before the question |
| 6 | the wrong test comment; README "Retention" |
| 11 | the no-lock test's group from an injected clock |
| 12 | `snapshots()` holds its own stderr to the rule; callers no longer must |
| — | the exemption: a restore's reads (history, records, size, its backup-first verify) have no backstop |

Measured first: M80 (`stats`, 6.4 s cold at 200,203 files on the remote link), M81 (zones and `forget`).

Seen failing first: `TestEveryResticUnitRunsInUTC` (every restic unit's environment without `TZ`) · the integration pin `TestIntegrationAResticUnitStoresItsTimesInUTC` against `resticUnit` without `TZ` (`a restic unit stored …+02:00, not UTC`) · `damage read`: `last group read: "41/52" … want "40/52"` · verify at its clock: `uploads after the repository did not answer: plan clean dump upload verify clean clean upload verify …`, `shop: … incomplete` · history not confirmed gone: `asked 2 times` · size in a drill: the drill ran to the test's own bound · `stopped before it was begun`: `warning ""` · the record's no id / ended from outside / signal / locked: "could not be written …" and `2h` · `the record waits 2h for a lock, and is given up at 30m0s` · damage hint absent (`restic check found 1 error; restic's own words: …`) · restore's history/verify: `did not answer within 20ms`, vouches `<nil>` · an interrupt during the records: `asked true` · the no-lock test against a clock in week 39 · e2e against `bf8c387`: `blog's snapshot is stored at …+02:00, not in UTC`, `made 20:44:20, its record is at 18:44:20`, the damage text, `the next drill said: … data group 42/52: damaged`. Refactor (12) held by its mutation: the rule taken out of `snapshots()` fails the records row and four listing tests. The restore's `size` row passed before (no clock yet) and guards the exemption.

## 5f. The owner's second `/code-review xhigh` of #161, at `0b45013` (2026-09-30): fifteen findings

Checked against the code; finding 6 measured (M82: `check --cleanup-cache` cleans nothing; the probe's does). Thirteen fixed, each a row seen failing first; 12 (records per app) already the owner's decision; 13 (a restore's two listings) answered — the second runs on the cache the first filled.

| # | Fix | Seen failing with |
|---|---|---|
| 1, 5, 14 | `run.unanswered`, set in `startWithin` at a clock; the loops stop on it; `stopAfter` gives later apps the reason's words; `didNotAnswer` plumbing gone | `the record, in a run`: `uploads after the repository did not answer: … clean upload verify …`, shop incomplete · `verify, in a run`: `shop: … Detail:snapshot aaaaaaaa was made, but …` |
| 2 | counted errors, stopped before the second probe → `failed`, group kept | `verdict &{… Class:clean …}, checked <nil>` (interrupted) · `… the check could not finish: the probe's unit: … ended from outside …` |
| 3 | `unit.ErrEndedFromOutside`; a check or probe ended from outside is no verdict | `a check ended from outside came to &{… Class:failed Detail:the probe's unit: … ended from outside …}` (both); the unit integration pin without the wrap: `got {ExitStatus:15 Result:signal}, …` |
| 4 | `check_since`; status ages "none come to a verdict" from it | `check <nil>, since <nil>` · status `healthy = true, want false` |
| 6 | `--cleanup-cache` on the probe | `the probe's command: [… "--no-lock"]` |
| 7 | a plan that cannot be made still checks | `checked <nil>` |
| 8 | a vouched snapshot asked for by name: nothing said | `the report names aaaaaaaa as the last ok, want ` |
| 9 | the storage failing named beside a prune | the damage row's wording |
| 10 | a record's exit 3: "may not have been written" | `… could not be written into the repository (restic failed (exit 3)) …` |
| 11 | e2e: a first check's group is the ISO week's before or after its drill; NEXT from the group read | a race only across Monday 00:00 UTC — not reproduced; the helper holds the group to the two |
| 15 | `besides` returns only its error | the stderr mutation again: five tests fail |

Found by the existing suite while implementing: two mistakes of mine — the probe read a clock that `startWithin` had already turned into "did not answer" as `failed` (`c6 no answer in time`), and an upgrade's missing drill verdict dereferenced (`TestAnUpgradeIsNoDrillVerdict` panicked). Both fixed before the lanes.

## 5g. Copilot at `87e4636`: two findings

The owner asked for a balanced view first, then "go with your advice".
- **The first drill after the repository had not answered** (the app's record given up at its clock): genuine, very unlikely (an app's first good backup, and the storage going silent between its verify and its record), bounded (another wait at the clock, a failed verdict for a week). Fixed in `ff405e0`: skipped, no verdict, the next run drills it. Row seen failing first (`vouch size unstage fetch …`); its second half — the next run drills — fails against the skipped drill recorded as failed. A test mistake of mine on the way (the second run placed before the scenario's shop assertion) caught and fixed.
- **`check_since` set as a drill begins, even if interrupted before its check**: accurate, not a bug — answered on its thread. Setting it at the check would reopen the second review's finding 4.

Every lane green at `ff405e0`; the older Copilot threads were already resolved; the fixed thread resolved, the answered one left for the owner.

## 5h. Copilot at `ff405e0`: a first check that found damage

Its overview alone, no inline finding: "A moderate restore scheduling issue remains unresolved after a first damaged repository check." Read against the code: genuine. "Damage keeps its group" worked by not moving `check_read` on; a first check left it empty, and the next week read its own week's group — a clean result there cleared the alarm for up to a year. The same held for "counted errors, then stopped". The owner chose (B) over the minimal (A): `check_read` became **`check_next`, the group the next check reads** — the group after on `clean`, the same group on any other verdict, untouched where there is none.

Rows, each seen failing first against a rename that kept the old meaning: `groupToRead`/`groupAfter` (`told "41/52": group "42/52"`, `after 40/52: "40/52"`); what each verdict does (12 rows — `first, damaged: check_next ""`, `told 41: read 42/52`); and end to end over four weeks with an injected clock, the case named (`week 40, after a first check found damage in 39: read 40/52`). Mutations: a non-clean verdict leaving `check_next` (the old behaviour) fails the first-check rows and the four-week test; one moving it on fails the damaged and unreachable rows; no verdict setting it fails `first, ended from outside`. e2e checks 5 and 5b now expect check 4's group (carried by `check_next`), not the ISO week's.

## 5i. Copilot at `e846e70`: two headline remarks, no findings

- "A probe timeout may be classified as failed instead of unreachable" — not reproduced. Traced every runner return at the probe's clock (stopped, not confirmed gone, the start request's own timeout; units are oneshots with no start timeout, so systemd never times one out); a throwaway test of four paths gave `unreachable` each time. What it showed: an unconfirmed stop's verdict quoted Go's "context deadline exceeded". Polished at the owner's word: "the repository did not answer within 30s, and its unit could not be confirmed stopped (`systemctl status …` says whether it runs on)" — kept inside the probe's verdict, not in `startWithin`, whose error setup's look reads. Row seen failing first.
- "The restore comment needs a minor correction" — genuine: `checkInto`'s comment narrated history (against the present-tense rule), and `lastVouched`'s no longer matched the code since the second review, leaving a dead branch. Both comments rewritten, the test comment that told the same story too; `lastVouched` asks once whether the chosen snapshot is vouched — behaviour unchanged, pinned: without that check two rebuilt-box rows fail.

## 6. D4 — time bounds

The owner's rule (plan §6 D4, trigger 3), the value put to the owner and
taken (2026-09-29): **a 30-minute backstop on `snapshots` and `ls`**.
Slowest legitimate: `snapshots` 19.6 s, `ls` 24.6 s (cold, remote link,
M78); the candidate, twice restic's 864 s give-up (M15), is 70× that.
Upload, fetch and the check stay unbounded (their length scales with
the data: 155–217 s for 1/52 of 28.5 GB over that link). The probe is
clocked at 30 s.
