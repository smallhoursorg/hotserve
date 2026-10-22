# DESIGN — packaging

This document describes the present: what is built, why, and what it
promises. History lives in git (`git log -- packaging/DESIGN-packaging.md`);
the "History" section at the end holds only dated one-liners.

The `.deb`'s design is stated beside the code it governs:
`packaging/nfpm.yaml`'s comments (what ships, at which mode, and why
the starter config is not a conffile) and `packaging/postinstall.sh`'s.
This document holds what has no place there: packaging's triage.

## Triage

Findings are triaged by the triage skill (`.claude/skills/triage/SKILL.md`):
its scales, the fix's cost, and the rule. The maintainer scripts
(`packaging/postinstall.sh`, `packaging/preremove.sh`,
`packaging/postremove.sh`) run as root on every install, upgrade,
removal and purge. These are packaging's examples for
the scales:

| Axis | Examples |
|---|---|
| **Severity 5** | the package puts bytes it did not ship where root trusts them, or opens root-only state (the starter's record, the box's key) to another user |
| **Severity 4** | the box's own `/etc/hotserve/Caddyfile` replaced by the starter, so no push reaches the box; dpkg left half-configured; hotserve left down after an install — each until the console |
| **Occurrence 1** | root's own entry in a directory only root can write, where the scripts expect none |
| **Detection 1** | the install fails, or the script says so on stderr, naming the path |

**Accepted residuals:**

| Residual | Where stated | S | O | D | Why accepted |
|---|---|---|---|---|---|
| `hotserve init` running while `apt install` configures a package whose starter changed, on a box still on the stock starter: postinstall hashes the stock file, init renames the box's file in, and postinstall's rename puts the starter over it | packaging/postinstall.sh (no lock) | 4 | 2 | 3 | 24, kept by rule 4: closing it means postinstall taking the box's lock (size 3, risk 3) for two root acts at the same moment, and a lock it waits on would let a long apply hold dpkg |
| A push that installs bytes identical to the starter postinstall last wrote is replaced by the next starter | packaging/postinstall.sh | 2 | 1 | 3 | 6; a starter has no `box` block, so no push reaches such a box anyway |
| `apt purge` finds something mounted at or under `/var/lib/hotserve-box`, by either its literal or its physical path (or cannot read `/proc/self/mountinfo`, resolve the path, or finish the scan, or the physical path holds a space, tab, newline or backslash, which the table escapes): the exchange tree is kept whole, so a purge and a reinstall come up on the old `applied.json` and record | packaging/postremove.sh (`mounted`) | 2 | 1 | 1 | 2; root's own mount in a directory the package owns; named on stderr with the remedy (unmount, then remove); the alternative, an rm through the mount, would remove what is not the package's |
| `apt purge` finds a symlink, or anything but a regular file, at `/etc/hotserve/Caddyfile`: kept, so a reinstall comes up on what it points to | packaging/postremove.sh | 2 | 5 | 1 | 10; an operator's own path (not a fault, so occurrence 5 and detection carries it): named on stderr, and postinstall leaves a symlink alone too, so the box comes up on the file the operator chose |
| `apt purge` while a run of the box applier holds root's lock for longer than the purge waits (30s; a run is seconds): the config and the exchange tree are both kept, so a reinstall comes up on them | packaging/postremove.sh (the lock) | 2 | 1 | 1 | 2; a run that long is a fault of its own (a disk that will not sync), and the purge names both paths and the journal to read; removing under a run would let it put a Caddyfile back after the purge (Copilot on #255, S3 O2 D5 = 30, fixed by taking the lock) |
| The smoke's stage 5 (`packaging/test/smoke.sh`, the reinstall) reads a start job a timer enqueued but the manager has not dispatched — the drill's, ordered `After=hotserve-backup.service` behind a run still going — as nothing running: its `ActiveState` is `inactive` and its `ExecMainStartTimestampMonotonic` 0. The stage's own run then meets the drill once dispatched, or the drill holds the run lock into the purge's staging | packaging/test/smoke.sh, stage 5 (the catch-up check and the hold's wait) | 2 | 2 | 3 | 12; the drill's elapse (a Sunday 03:30–03:40) and a run of the hourly still going twenty minutes after its own elapse (03:00–03:10) have to meet, in a smoke whose runs take seconds (PR #341, the Opus review) |
| The smoke's stage 5 reads an empty `NextElapseUSecRealtime` as a fired run still going, and waits 600 s for it; a timer that is not `waiting` for another reason (stopped, failed or masked by a regression after `expect_backup_state` read it active) gives the same empty value, and the red after the wait names a run that never was | packaging/test/smoke.sh, stage 5 (the two next-elapse waits) | 2 | 2 | 3 | 12; a regression in the units or the maintainer scripts, landing in the second between two reads of the same timer (PR #341, the Opus review) |

**Owed** (over the bar, not yet fixed): none.

**Kept on purpose, not scored** — `apt purge` leaves `/etc/hotserve/age/`
(the box's key for secrets), `age.pub` and `/etc/hotserve/secrets/`,
and names them on stderr, with the `rm -r` that finishes the job
where the key is among them.
Deleting the key is one-way: it is the one way to read the secrets
the box's repository holds, and a purge cannot know whether that is
over. `/var/lib/hotserve` (Caddy's state) and `/var/lib/liveswap` (the
apps' releases and data) are never removed by a maintainer script;
dpkg removes either only when it is empty, as any directory a package
shipped. The account stays.

## History

Dated one-liners; the full text of each is in git.

- 2026-10-14 — Packaging gets a design doc for its triage: its examples
  for the scales, #239's residuals (the starter config out of the
  conffile, #237), and the purge it owes.
- 2026-10-16 — The owed purge (scored 45) is paid by
  `packaging/postremove.sh` (PR #255): a purge removes the box's
  config, the starter's record and the box applier's exchange tree,
  and masks the path unit on remove the dh_installsystemd way, under
  the applier's own lock so a run under way cannot put the Caddyfile
  back. Three residuals of that script accepted; the key kept on
  purpose. The
  `backup` branch folds its timers' handling into the same file.
- 2026-10-17 — No box from before v0.3.1 exists but the owner's, which
  runs v0.3.1: postinstall stops removing the AppArmor profile packages
  before the Debian-13-only matrix left, and the two residuals of
  taking a box to or from a release with the starter as a conffile go.
