# DESIGN — triage

This document describes the present: what is built, why, and what it
promises. History lives in git (`git log -- DESIGN-triage.md`); the
"History" section at the end holds only dated one-liners.

This document says which findings get fixed. A finding comes from a
review, a test or the field, in any part of the repository. It is
scored on what the failure does and on what its fix costs, and the
rule below decides. Without a rule every edge case looks mandatory,
and each guard added for a multi-fault case becomes a new branch for
the next review to probe.

The scales are general. A subsystem's design doc gives its own
examples for each level, and keeps its own table of accepted residuals
next to the behaviour they describe:

| Subsystem | Examples and accepted residuals |
|---|---|
| box | [box/DESIGN-box.md](box/DESIGN-box.md), "Triage: which failures to defend" |
| packaging | "Packaging", below (it has no design doc of its own) |

A subsystem with no examples yet is scored on the general scales
alone.

## Scoring the failure

Three axes, FMEA's:

| Axis | 5 | 4 | 3 | 2 | 1 |
|---|---|---|---|---|---|
| **Severity** — the effect if it happens | silently breaks what the subsystem exists to guarantee | stuck until someone at the console fixes it | a wrong answer someone trusts: a verdict, or a doc or tool that leaves someone in a wrong state they believe is right | a wrong message; red where green was due | logging only |
| **Occurrence** — what must go wrong | normal operation | one common fault: a crash, a restart, a failed reload | one rare fault: `EIO`, a full disk, a clock step | two independent faults | three or more, or state only root can write found corrupted |
| **Detection** — how it surfaces | silently | — | a wrong but plausible answer | — | loudly, naming the cause |

A hostile hotserve uid (T5 in
[DESIGN-threat-model.md](DESIGN-threat-model.md)) is scored by what it
gains: an effect it could already have by other means scores severity
1.

## Scoring the fix

Two axes. Cost is never multiplied into severity × occurrence ×
detection: that product measures the failure, and these measure the
fix.

| Cost | 1 | 2 | 3 |
|---|---|---|---|
| **Size** — what the fix adds to the code that runs | tightens lines already there: no new branch, state or file | one new branch, or about ten lines | a new state, lock or file, or more |
| **Risk** — what the fix may break | the normal path behaves as before, and a new test proves the fix | anything between: the normal path changes under a test, new or existing, or it behaves as before with no new test proving the fix | the normal path changes, and no test covers it |

## The rule

In order:

1. **Severity 5 is fixed**, whatever its occurrence.
2. **Single faults are fixed** (occurrence 3 or more): under any one
   fault the system must end correct, or red and named — the
   single-fault criterion of safety engineering.
3. Anything else is fixed if severity × occurrence × detection is 20
   or more, or if its fix scores 1 for both size and risk: a fix that
   cheap and that safe is made whatever the finding scores. Otherwise
   the finding is **accepted** and recorded in its subsystem's table
   of accepted residuals, with its scores.
4. **Stop** when the next guard costs more than it buys (ALARP): a fix
   that adds a branch for a multi-fault case is itself a finding to
   weigh, since every branch is one more state the next fault can
   reach. The cost axes are how that cost is weighed. Rules 1 and 2
   hold whatever a fix costs: the cost changes how they are fixed, not
   whether.

## Packaging

The `.deb`'s maintainer scripts (`packaging/postinstall.sh`,
`packaging/preremove.sh`) run as root on every install, upgrade and
removal.

| Axis | Examples |
|---|---|
| **Severity 5** | the package puts bytes it did not ship where root trusts them, or opens root-only state (the starter's record, the box's key) to another user |
| **Severity 4** | the box's own `/etc/hotserve/Caddyfile` replaced by the starter, so no push reaches the box; dpkg left half-configured; hotserve left down after an install — each until the console |
| **Occurrence 1** | root's own entry in a directory only root can write, where the scripts expect none |
| **Detection 1** | the install fails, or the script says so on stderr, naming the path |

**Accepted residuals:**

| Residual | Where stated | S | O | D | Why accepted |
|---|---|---|---|---|---|
| `hotserve init` running while `apt install` configures the package, on a box still on the stock starter: postinstall hashes the stock file, init renames the box's file in, and postinstall's rename puts the starter over it | packaging/postinstall.sh (no lock) | 4 | 2 | 3 | 24, kept by rule 4: closing it means postinstall taking the box's lock (size 3, risk 3) for two root acts at the same moment, and a lock it waits on would let a long apply hold dpkg |
| A box installed from a release without the conffile, taken back to a release with it (v0.3.1-rc1 or earlier), gets dpkg's question about the Caddyfile; Y replaces the box's file with the starter | — | 4 | 2 | 1 | 8; off the documented way back (the `.deb` you upgraded from), then Y against the default; the prompt names the file and the choice |
| A box still on a starter from a release with the conffile keeps it: no record names it, so upgrades leave it alone | packaging/postinstall.sh | 2 | 1 | 5 | 10; the safe direction. `cp /usr/share/hotserve/Caddyfile /etc/hotserve/` makes it stock, and the next upgrade records it |
| A push that installs bytes identical to the starter postinstall last wrote is replaced by the next starter | packaging/postinstall.sh | 2 | 1 | 3 | 6; a starter has no `box` block, so no push reaches such a box anyway |

**Owed** (over the bar, not yet fixed): `apt purge` leaves
`/etc/hotserve/Caddyfile` and `/var/lib/hotserve-package/` behind on a
box installed from a release without the conffile, so a purge and a
reinstall come up on the old config. Scored S3, O5, D3: 45. The fix
is a postrm that removes both on purge, in `packaging/postremove.sh`,
which the `backup` branch adds.

## History

Dated one-liners; the full text of each is in git.

- 2026-10-14 — The triage rule moves here from box/DESIGN-box.md, where
  it was written for the box applier (box PR 3a), so that every
  subsystem's findings are scored on one scale. The fix is now scored
  too, on size and risk, and a finding under the bar is still fixed
  when its fix scores 1 on both (first used on #239, where tracing
  such a fix turned up a worse case than the one reported). Packaging
  gets its examples, and #239's residuals as its table.
