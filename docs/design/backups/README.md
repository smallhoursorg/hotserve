# Backups: the design record

The working notes behind `hotserve-backup`, kept as they were when the
feature was put on ice (2026-10-09). They are a record, not a guide: the
operator guide is [docs/backups.md](../../backups.md) and the reference is
[backups/README.md](../../../backups/README.md).

## Where things stand

- `v0.3.0-rc1` was cut from `backup` and run on a real box (arm64 Hetzner,
  Debian 13, B2). Checks 1–4, 6, 7 and 8 (timing) and the lifecycle
  bucket passed or were answered; check 5 (retention) and check 8's
  egress figure were not done.
- The run found a hang that stops backups: a run's upload sat for 27 hours
  on B2 with no output — **#177**, with the evidence and the options;
  closed 2026-10-29, after #368, with what is left accepted.
- Every RC check on B2 ran through B2's native API — restic's own
  `b2:` backend, and rclone's and `backblaze-b2`'s B2 code. Since
  D13 (2026-10-23, #354) B2 is reached through restic's S3 backend at
  B2's S3-compatible endpoint and `b2:` is refused, so every check of
  `docs/release-candidate.md` that runs restic against B2 is run again
  there, its new ones (9–12) among them; the native results stay in
  `rc-runs/` as the record they are.
- Open work is labelled [`backup`](https://github.com/smallhoursorg/hotserve/issues?q=is%3Aopen+label%3Abackup).
  #172 (the threat model's Backups section) is the gate before `backup`
  merges into `main`.

## What is here

| Path | What it is |
|---|---|
| [handover.md](handover.md) | The briefing the work started from: facts, the owner's decisions, pitfalls. Was `HANDOVER-backups.md`. |
| [plan.md](plan.md) | The running plan and log: decisions (D…), measurements (M…), each PR's brief and outcome, the RC run check by check. Was `PLAN-backups.md`. |
| [pr-plans/](pr-plans/) | One plan per PR: procedures, claims traced to code, measurements, review rankings. |
| [rc-runs/](rc-runs/) | Raw outputs of the RC checks run against real B2 through B2's native API (restic's own `b2:` backend, and rclone's and `backblaze-b2`'s B2 code), and the scripts that made them; `RC-stuck-upload.md` is #177's evidence; `rc-results/` is the RC-results PR's own. |
| [tools/lanes.sh](tools/lanes.sh) | Runs every lane (`make test` … `make e2e`) in turn, a log and a summary line each. |

Paths inside the notes that pointed at `.claude/plans/` and
`.claude/rc-runs/` now point at `pr-plans/` and `rc-runs/`; home
directories are written `~`. Nothing else was edited.
