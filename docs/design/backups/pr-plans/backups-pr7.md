# PR 7 — the integration lane's two flakes

Start checks (2026-10-02): #162 `MERGED` (`74a01ed`, head `27db308`);
`git fetch --prune origin`; `main` 0 ahead of `backup`, `backup` 16
ahead of `main`. Branching waits for the owner's answer on one PR or
two. Facts of both flakes as PR 6 left them: `backups-pr6.md` §7.

All local measurements: a fresh `dev-systemd` (systemd 257, arm64, 6
CPUs, the Mac on battery with `caffeinate -dims`), test binaries built
once with the lane's flags (`go test -c -race -tags integration`) and
run alone, candidate changes through `go test -overlay` (the tree
untouched).

## 1. backups: `TestIntegrationWhatIsBoundIsWhatWasPinnedWhateverTheAppDoesToTheName`

### 1.1 Measured

- **CI, every uncached run since `-count=1` (`245e9f7`, `28779ca`,
  `8531471`, `27db308`; 8 runs): all passed.** Refusals of the 6,000
  attempts allowed: **arm64 3,010 · 1,888 · 1,817 · 3,589** (30–60 % of
  the budget, 2.6–2.8 s); amd64 949 · 976 · 603 · 498 (4.4–5.2 s).
- **Local, idle, 100 runs: 0 failed.** Refusals p50 412, p90 1,056, p99
  3,034, max 3,718.
- **Local, six busy loops on six CPUs, 100 runs: 100 failed** — each
  `pin_integration_test.go:113: 5 units shown the directory, 16846
  flips (5995 pins refused mid-flip): not enough of a race to mean
  anything`, 0.8 s. Refusals min 5,978, max 6,000. Reproduced on
  demand.
- **Why:** an attempt costs microseconds and a flip does not. A
  flipper descheduled mid-flip holds the name refused — aside, or a
  link — for a whole time slice, and the loop, which retries at once,
  spends its whole budget of attempts inside that slice. The race is
  not weaker under load (more flips per unit, not fewer); only the
  count of attempts runs out.

### 1.2 Candidates under CPU load (50 runs each)

The runner script's `kill $(jobs -p)` killed nothing (a
non-interactive `sh` lists no jobs), so each variant's six loops
outlived it: **V2 ran under 6 loops, V3 under 12, V4 under 18 and a
concurrent `go build`.** Found by `ps` afterwards; the script now kills
by pid.

| | Change | Load | Result | Refusals | Time |
|---|---|---|---|---|---|
| V2 | 1 ms pause after a refusal; the budget as it is | 6 loops | **50/50 pass** | p50 185, max 293 (≤ 5 % of the budget) | p50 7.2 s, max 8.2 s |
| V3 | a one-minute deadline in place of the budget; no pause | 12 loops | 50/50 pass | p50 101,928, max 158,937 (spins) | p50 7.5 s, max 8.0 s |
| V4 | both | 18 loops + build | 50/50 pass | p50 374, max 570 | p50 7.0 s, max 7.9 s |

Every variant's flips stayed far above the test's floor of 100 (min
51,312).

- **V2 idle, 50 runs: 50 pass**, refusals p50 158, max 220; p50 2.5 s,
  max 3.3 s — no cost idle.
- **The test still catches what it is for.** Mutant: `mountAt` binds
  the path text of `/proc/self/fd/<n>` (the race the test's comment
  names) instead of the descriptor. Caught 20/20 with the loop as it
  is, 20/20 with V2 idle, 20/20 with V2 under six loops — as
  `mounting the pin: binding it: no such file or directory` (19, 19,
  20) and `the unit was shown "THE SIBLING'S\n"` (1, 1, 0).

### 1.3 Decided (test only; nothing a box runs changes): V2

One line and an import: a refused pin waits 1 ms before the next
attempt, so the flipper is let run and the budget counts attempts that
can succeed. Seen failing first: the six-loop reproduction above,
100/100, its message written down.

## 2. liveswap: `TestIntegrationSystemdJournalTail`, and a failed deploy's log tail

### 2.1 Measured

- **CI, the same 8 uncached runs: all passed** (0.07–0.13 s).
- **Local, the test alone, idle: 20/20 passed.** PR 6's natural rate
  stands: ~1 in 60–80 under disk load; forced by stopping journald.
- **What journald does (systemd v257 source, then measured):** each
  read of a stdout stream carries the writer's `SCM_CREDENTIALS`; a
  new pid gets a new context, acquired at that pid's first line, from
  `/proc/<pid>` (`journald-stream.c:620–638`, `:268–275`). The user
  manager passes no unit name to fall back on (`EXEC_PASS_LOG_UNIT` is
  the system manager's alone, `unit.c:5391`; journald takes it only
  from uid 0, `journald-stream.c:391`). Under `PrivatePIDs=` the
  stream is connected by the intermediate, before the namespace fork
  (`exec-invoke.c:4608` before `:5082`), which then exits — so the
  stream's peer pid is dead at once, and only the per-read pid counts.
- **Spike (scratch test through liveswap's own runner and unit
  properties; never committed):**
  - **`_PID` is the writer's own pid, line by line.** The main
    process's lines carry the manager's `ExecMainPID` — read after a
    failed oneshot ends, before its reset, and after a simple unit
    dies (4 units: 72373, 72504, 72564; 72385, 72516, 72576). A
    child's line carries the child's pid.
  - **One `_STREAM_ID` for the whole unit:** stdout, stderr and a
    child's line alike. journald assigns it; only processes holding
    the stream's descriptor write into it.
  - **journald stopped for the unit's life: every line, of either
    unit type, without `_SYSTEMD_USER_UNIT`, `_SYSTEMD_INVOCATION_ID`
    or `_COMM`; `_PID`, `_STREAM_ID`, `SYSLOG_IDENTIFIER` present.**
  - A 1 ms D-Bus poller on the unit (the spike's own) was load enough
    to lose every immediate line with journald running; without it the
    main process's lines kept their unit. Load decides.
- **By the writer's shape, journald idle, oneshots, 20 each:**

  | What wrote the line | Kept its unit |
  |---|---|
  | the main process, one line, then exit | 20/20 |
  | **a child, one line, then exit** (`sh -c 'echo …'`) | **0/20** |
  | a child that lives 0.2 s after writing | 20/20 |

  A line keeps its unit only if journald reads it before its writer is
  reaped. The manager reaps the main process slowly enough that
  journald nearly always wins, and loses under load (the flake). A
  shell reaps its child at once: **a child's last words never carry
  the unit** — node's error under `npm run migrate`, say. Not a flake;
  every time.
- `kernel.pid_max` 4,194,304 (Debian's `50-pid-max.conf`).
- **Unmeasured items of PR 6's §7.1, answered:** `ExecMainPID` of a
  failed transient oneshot after exit is the main process's pid, and
  stays readable until the reset; `MainPID` is 0 then. `_STREAM_ID`
  cannot be known from the manager: a live unit's `systemctl --user
  show` carries it nowhere (measured); journald's own state file under
  `/run/systemd/journal/streams/` has it, keyed by the socket's
  dev:ino and removed when the stream closes. Only a line in the
  journal gives it. CI's rate: 0 in 8.
- **Reproduced, the unmodified test, journald stopped for its first
  2 s: 3/3 failed** — `runner_systemd_integration_test.go:386: journal
  tail of hotserve-itest.journaltail.0a1b2c3d0a1b2c3d.prestart.service:
  [], <nil>` (5.11–5.14 s), the natural failure's message. Control:
  pass, 0.07 s.
- Not measured, from the source only: that the stream's peer is the
  intermediate. Nothing below leans on it — the per-line `_PID` is
  measured.

### 2.2 The fix — the owner's (it decides what a public CI log may carry)

`log_tail` today is every line journald attributed to the deploy's
units (`_SYSTEMD_USER_UNIT`, a field journald derives from the
cgroup; nothing an app writes can set it). The options:

- **(A) Read the units' streams.** One `journalctl -o json
  --output-fields=_STREAM_ID` learns the streams, from lines
  attributed to the units **or** written by a unit's main process
  (`_PID=` the manager's `ExecMainPID`, with `SYSLOG_IDENTIFIER=
  hotserve-<app>`); a second reads `_SYSTEMD_USER_UNIT=… +
  _STREAM_ID=…` as the tail. Recovers the flake's lines (the main
  process's) **and children's last lines (0/20 today)**. Trust: a
  stream id is journald's, and only the unit's own processes hold the
  stream; the pid step trusts a kernel-attested pid inside the
  deploy's window, which another process could hold only after
  4,194,304 pids wrap. Size: the pid onto `exitError` and the app's
  exit (one fake runner), the reader's two runs; ~100–150 lines with
  tests.
- **(B) The main process's pid only.** One run: `_SYSTEMD_USER_UNIT=…
  + _PID=<main pid> SYSLOG_IDENTIFIER=hotserve-<app>`. Fixes the
  flake; a child's last lines stay lost every time. ~50–70 lines.
- **(C) Say so only.** When nothing is attributed, `log_tail_error`
  says why and names `journalctl -t hotserve-<app>`. No line
  recovered; a tail missing a child's last line stays silent. Changes
  `TestFailureDetail…` "an empty journal is no tail and no error".

Tests first for (A), each seen failing: the integration test with
journald stopped through the unit's life (the message above); a
child's one line in the tail, journald idle (lost 20/20 today);
unit rows for the query's arguments.

### 2.3 Measured for (A)'s reader (a throwaway `hotserve-dev-systemd`, systemd 257)

- `journalctl -F _STREAM_ID` takes no matches: "Extraneous arguments
  starting with '_SYSTEMD_USER_UNIT=…'". So the streams are learnt
  with `-o json --output-fields=_STREAM_ID`, which prints, besides,
  `__CURSOR`, `__REALTIME_TIMESTAMP`, `__MONOTONIC_TIMESTAMP`,
  `__SEQNUM`, `__SEQNUM_ID`, `_BOOT_ID` (~420 bytes an entry).
- Matching: `_SYSTEMD_USER_UNIT=A _SYSTEMD_USER_UNIT=B + _PID=p
  _PID=q SYSLOG_IDENTIFIER=x` is (A or B) or ((p or q) and x).
  A unit whose main process wrote `a-main` and whose child wrote
  `a-child` (unattributed, same stream): unit or stream gives both;
  pid and identifier alone gives `a-main` and its stream. The
  identifier alone gives as well the live instance's lines and a
  `systemd-cat -t` line of the same identifier — what the stream keeps
  out.
- **A client cannot set `_STREAM_ID` or `_SYSTEMD_USER_UNIT`:**
  `logger --journald` as root with both in its fields — journald
  stored neither (`_TRANSPORT=journal`, `_UID=0`).

## 3. The owner's answers (2026-10-02)

1. **Two PRs:** liveswap's fix into `main` (the owner then merges
   `main` into `backup`); the pin test's into `backup`.
2. **Flake 1: (A)**, the units' streams.
3. Flake 2: V2, decided (test only), said in one line.
4. **The review loop fixes every genuine finding.**

## 3. Questions for the owner

## 4. Tests first — each seen failing, the message written down

### PR 7b (`backup-pin-race`, from `backup` at `74a01ed`)

| Row | Seen failing (before) | After |
|---|---|---|
| the pin race under six busy loops | 100/100: `pin_integration_test.go:113: 5 units shown the directory, 16846 flips (5995 pins refused mid-flip): not enough of a race to mean anything` | the committed file: 20/20 under the same load, refusals max 235 |

`fb8a45b`. Lanes at `fb8a45b`: see §5.

### PR 7a (from `main`): the invariant — `log_tail` is the last lines the launch's units wrote, whatever journald could attribute

Steps: signatures first with nothing filled (the reader as today), the
rows written and seen failing, then the fill.

| Row | Kind | Holds |
|---|---|---|
| the pre_start's pid and the app's identifier reach the journal | unit (`app_detail_test.go`) | `exitError` carries `ExecMainPID`; `failureDetail` passes it |
| an app that died: its pid reaches the journal | unit | `runner.Exit` gives the pid |
| the arguments: streams learnt by unit or (pid and identifier); tail by unit or stream | unit | the two argument lists, exactly |
| stream ids read from journalctl's JSON; a cut line, a wrong shape, a repeat skipped | unit | `streamIDs` |
| a child's one line is in the tail, journald idle | integration | the stream (0/20 attributed today) |
| journald stopped through the unit's life: both lines in the tail | integration | the pid step (the flake) |
| e2e 5b: the crash release's `fatal: cannot start` comes from a child | e2e | the whole path as the `hotserve` user, the system journal |

**Seen failing (2026-10-02), the signatures in, nothing filled** (pids
0, the reader asking by unit only, stubs for the new pieces):

- `app_detail_test.go:69: journal asked by pids [] under "hotserve-demo", want [731] under hotserve-demo`
- `app_detail_test.go:104: journal asked by pids [], want the app's [4242]`
- `TestJournalArgs`: all five cases `got []`, each `want` the list in the table
- `app_detail_test.go:252: stream ids = "", want "0123…,fedc…"`
- `runner_systemd_integration_test.go:376: journal tail of [hotserve-itest.journaltail.0a1b2c3d0a1b2c3d.prestart.service] (pids [0]): ["first line" "second line"], <nil>` (5.18 s) — the child's line missing
- `runner_systemd_integration_test.go:402: journal tail of [hotserve-itest.journaltailwhenjournaldisbehind.0a1b2c3d0a1b2c3d.prestart.service] (pids [0]): [], <nil>` (5.22 s) — the flake
- `make e2e`: `FAIL: stderr line missing from the tail: … "detail":{"exit":"exit status 3","log_tail":["starting with SECRET=[redacted:SECRET]"]}` — 1 assertion of the suite

## 4b. `/code-review xhigh` on 7b at `fb8a45b` (2026-10-02): seven findings, ranked before any work

All in `TestIntegrationWhatIsBoundIsWhatWasPinned…`; the sleep itself
judged sound (never on the success path). Ranked:

| # | Finding | Severity | To decide it |
|---|---|---|---|
| R1 | "enough of a race" (`n ≥ 100` flips in all) can pass with no flip between a pin and its bind — a path-text bind could pass under load | high if real: a false negative on what the test guards | the mutant under 12 and 18 loops (20/20 caught at 6) |
| R2 | the budget counts attempts; 1 ms tunes it to one measured load | medium | V2 under 12 and 18 loops |
| R3 | the flipper is stopped only on success: after a `Fatalf` it spins on through the package's later tests and races the `RemoveAll` | medium (a failing run) | a failing run: the directory left, the goroutine |
| R4 | any `beneath` error counts as a refusal: an unrelated one now costs ≥ 6 s and ends in a message that hides it | low–medium | the errors the race gives |
| R5 | a bind left mounted is never asserted (`mountsUnder`) | low | an unmount that does nothing |
| R6 | `PIN_RACE_UNITS` unchecked: 0 or less tests nothing | low | `0` |
| R7 | the comment's "its whole time slice": a descheduled thread waits out *others'* slices | low (words) | — |

**Measured, then decided (2026-10-02; fresh `dev-systemd`, the Mac
awake):**

- **R1 — not reproduced, answered.** The path-text mutant over the
  committed loop: caught 20/20 at twelve busy loops and 20/20 at
  eighteen (and 20/20 idle and at six before): `binding it: no such
  file or directory` 18 and 17, `the unit was shown "THE SIBLING'S\n"`
  2 and 2 (one at eighteen by another line).
- **R2 — not reproduced, answered in the comment.** The committed loop:
  30/30 at twelve loops (refusals max 352), 30/30 at eighteen (max
  529). Each refusal now gives the CPU up for ≥ 1 ms, so the budget is
  ≥ 100 ms of refusals a unit; the comment says so.
- **R3 — genuine, both race tests.** Before: 9 of 40 failing runs (the
  mutant) left `/root/pin-race-<n>/blog/shared/uploads.aside` or
  `uploads` behind; the restore race, a failure injected at unit 3,
  16 of 20 left `/root/restore-race-<n>`. Fixed: `sync.OnceValue` stop
  in a `t.Cleanup` registered after the `RemoveAll`, so it runs first.
  After: 0 of 40, 0 of 20.
- **R4 — genuine.** The race's refusals, 20 runs idle and at six
  loops: only `a symbolic link is in the way` and `open uploads: no
  such file or directory`. Before, `beneath` made to answer EACCES:
  7.05 s, `0 units shown the directory, 285160 flips (6000 pins
  refused mid-flip): not enough of a race to mean anything`. After:
  0.00 s, `pinning uploads: open uploads: permission denied`.
- **R5 — genuine, both race tests.** Before, `mountAt`'s unmount made
  to do nothing: both **pass**, 60 and 40 binds left mounted. After:
  both fail, `still mounted under the run directory: […]`
  (`unmountedAll`).
- **R6 — genuine, both (one variable).** Before, `PIN_RACE_UNITS=0`:
  `0 units shown the directory, 0 flips (0 pins refused mid-flip): not
  enough of a race…` and `0 units (0 installed, 0 refused mid-flip), 0
  flips: …`. After: `PIN_RACE_UNITS="0": a count of units, one or
  more` from both (`raceUnits`). An overflow of `100*units` needs a
  count past 9×10¹⁶: not guarded.
- **R7 — genuine, words.** Rewritten with R2's figures.

After all of it: both tests 5/5 idle, 10/10 at six loops (refusals max
226).

## 4c. `/code-review xhigh` on 7a at `3601090` (2026-10-02): fourteen findings, ranked before any work

| # | Finding | Severity | To decide it |
|---|---|---|---|
| F1 | streams are learnt only from attributed lines or the main pid's: a silent wrapper (`sh` → `node`) whose child prints and exits still gives an empty tail; the README's "their child processes' lines are in it too" is false for it | high | the shape through the reader; can a sandboxed app reach journald's sockets (a full fix would trust unattributed streams — the security model) |
| F2 | `failureDetail` stops after two empty reads (~200 ms): journald behind by more → no tail, no error; the tests poll 5 s on their own | medium | a pre_start failure through `failureDetail`, journald paused |
| F4 | a pre_start that succeeded records no pid: its unattributed lines are lost if the app then fails | medium-low | is `ExecMainPID` readable when a successful oneshot's job returns |
| F3 | an app hotserve stops (health timeout, probe) passes no pid | medium-low | unit row |
| F9 | the instance's pid path (`Start`, `Exit`, the watcher's snapshot) is tested only through a fake | medium (test gap) | integration row |
| F8 | `pauseJournald` resumes from `t.Cleanup` only: a panic leaves journald stopped for the lane | low-medium | a resume that outlives the test binary |
| F6 | lines sent to the journal directly fill the learning window's n+1 slots | low | can apps reach the socket; `_TRANSPORT=stdout` |
| F7 | `Exit`'s fallback (`sh.pid`) can be the intermediate's | low | — |
| F5 | a failed stream run drops the whole tail | low | — |
| F10 | `-o cat --output-fields=_STREAM_ID` simpler than JSON | low | its output on 257 |
| F11 | two runs per attempt, streams relearnt | low | their time |
| F12 | `pids` a slice holding at most one | low | with F4 |
| F13 | the pid by three routes | low (design) | — |
| F14 | `isID128` a hand loop; the package uses anchored regexps | nit | — |

**Measured (2026-10-02/03):**

- **F1 reproduced.** A pre_start `sh -c 'echo child-only-line >&2';
  exit 2` through the real runner and the new reader: tail `[]`. The
  line is in the journal with `_PID` the child's (222, the main 221),
  `_STREAM_ID`, no unit field.
- **A sandboxed unit cannot reach journald but by its own stream.**
  Inside the unit: `/run/systemd/journal`, its `socket` and `stdout`,
  `/dev/log` all absent; `logger -t` left nothing, `systemd-cat -t`
  failed "Failed to create stream fd: No such file or directory";
  nothing under the spoofed identifier. → F6 cannot arise from an app.
- **F4:** a successful oneshot's start job returns `done` with the unit
  already `not-found`, `ExecMainPID` 0 — 10 of 10. No pid to record
  without changing the pre_start unit's lifecycle.
- **F10:** `-o cat --output-fields=_STREAM_ID` prints the id alone, 33
  bytes an entry. **F11:** the two runs, ~4 ms a pair (10 rounds,
  small journal). `journalctl --sync` as a non-root `systemd-journal`
  member: "Failed to connect to Varlink socket: Permission denied" — no
  way to wait for journald but asking again.

**The owner on F1 (2026-10-03): keep (A) and say the limit** — no new
trust; the silent wrapper's and a succeeded late pre_start's lines stay
out, as before this PR, and the docs say which lines the tail recovers.

**Decided, each fixed as a row seen failing first:**

- **F1/F4 — said.** README `log_tail`, `journalctlReader`'s comment:
  the two shapes named, `journalctl -t hotserve-<app>` for every line.
- **F2 — fixed.** Before: `app_detail_test.go:145: a journal two reads
  behind: detail &{Exit:exit status 3 … LogTail:[] …} after 2 reads`.
  The loop gives up on "no growth" only once it has lines; with none it
  asks its five times (~0.8 s more on a failure that printed nothing).
- **F3 — fixed.** Before: `app_detail_test.go:183: journal asked by pid
  0, want the instance's 4242`. `runner.Exit` gives the pid while the
  instance runs too (the handle's); the gate reads it on any failure.
- **F8 — fixed.** Before: a test binary that died while journald was
  paused left it `Ts` 5 s later. A setsid'd `sleep 3; pkill -CONT`
  started first: after, `Ts` at once, `Ss` 4 s later.
- **F9 — fixed.** `TestIntegrationSystemdJournalTailOfAnInstance`:
  `Start`, journald paused through the instance's life, `Exit`'s pid
  finds both lines. Passes; with `Exit` reporting pid 0 (overlay):
  `runner_systemd_integration_test.go:435: journal tail of
  [hotserve-itest.journaltailofaninstance.0a1b2c3d0a1b2c3d.service]
  (pid 0): [], <nil>`.
- **F10 — fixed.** Before: `TestJournalArgs` wanting `-o cat`, and
  `stream ids = ""` on cat output. After: `-o cat
  --output-fields=_STREAM_ID`, no JSON, a 64 KiB bound.
- **F12 — fixed:** `tailOf.pid int`. **F14 — fixed:** `streamIDRe`.
- **Answered:** F5 (both runs are the same program on the same journal
  with the same grant; a failure is said in `log_tail_error`, not
  dropped silently) · F6 (an app cannot reach the journal but by its
  stream, measured) · F7 (with the unit gone the handle's pid is all
  the manager left; the intermediate wrote no line, so it finds none —
  no wrong line) · F11 (~4 ms a pair) · F13 (`RunOnce` has no handle,
  so its pid rides `exitError`; the instance's comes from `Exit`).

After: unit tests pass; the three journal rows 10/10 each.

## 5. Lanes

### PR 7b at `fb8a45b` (fresh containers, the Mac awake, `caffeinate`)

- `test` 0 (6 s) · `lint` 0 (9 s) · `secretscan` 0 (4 s, 273 commits,
  no leaks) · `test-integration` 0 (218 s: 89 passed, none failed or
  skipped, none cached; the pin test 159 refused) · `package` 0 (21 s)
  · `install-test` 0 (127 s) · `e2e-backup` 0 (202 s) · `e2e` 0 (174 s).

### PR 7b at `912d12e` (after the review)

- All eight green: secretscan 274 commits, no leaks; integration 89
  passed, none failed, skipped or cached (pin race 231 refused,
  restore race 27); package smoke, every e2e-backup and box scenario.
  No sleep in `pmset -g log`.
- **Pushed; PR #166 into `backup`; Copilot requested** (timeline:
  `review_requested Copilot`). Its reply goes to the owner before any
  action.
- **Copilot at `912d12e`: "Approval recommended", no findings.** CI:
  all 18 checks green. CI's own figures for the pin race now: arm64
  198 refused (1,817–3,589 before), amd64 133 (498–976); the restore
  race 24 and 28 refused mid-flip.

### PR 7a (`liveswap-journal-streams`, from `main` at `e5709c7`)

- `3601090`: the fix, its seven rows, the README's `log_tail` and the
  threat model's paragraph. After the fill: the unit rows pass; both
  integration rows 20/20, 2 s apart. A trial merge into `backup`
  (`git merge-tree`): clean, and no file `backup` adds calls what
  changed signature.
- Lanes at `3601090` (`main`'s: no `e2e-backup`; Go's test cache
  cleared before the integration lane, which on `main` has no
  `-count=1`): all green — integration 29 passed, none cached; e2e
  `PASS: the tail has the app's stderr`.
- `/code-review xhigh`: §4c. `748a74c`: the review's fixes.
- **Lanes at `748a74c`:** all green — secretscan 276 commits, no
  leaks; integration 30 passed, none failed, skipped or cached (the
  three journal rows); package smoke; e2e every suite. Trial merge into
  `backup` clean. No sleep in `pmset -g log`.
- **Pushed; PR #167 into `main`; Copilot requested** (timeline:
  `review_requested Copilot`).
- **Copilot at `748a74c`: "Approval recommended", no findings, no
  inline comments.** CI: 17 checks pass (CodeQL included), automerge
  skipped; the three journal rows ran on both arches, not cached
  (0.08–0.16 s). Put to the owner; nothing to act on.
