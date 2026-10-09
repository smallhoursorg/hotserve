# RC finding, 2026-10-06: a run's upload stuck 27 h on B2 (the owner's box, pasted by the owner)

systemctl list-timers hotserve-backup.timer (before):
  NEXT - | LEFT - | LAST Mon 2026-10-05 09:01:26 UTC | PASSED 1 day 3h ago

hotserve-backup status (before), exit 1:
  last run 2026-10-05 08:01 UTC
  no run has finished since 2026-10-05 08:01 UTC, more than 3 hours ago: what follows is as of then
  last drill 2026-10-04 13:43 UTC
  repository last checked 2026-10-04 13:44 UTC: its structure, and data group 41/52: clean
  dev: ok: last complete backup 2026-10-05 08:01 UTC, snapshot 0a9fbe4e (data at /var/lib/liveswap/dev/shared)
  dev: stale: the last complete backup is from 2026-10-05 08:01 UTC, more than 3 hours ago, snapshot 0a9fbe4e
  dev: restore last proven: 2026-10-04 13:44 UTC, snapshot 167d2290
  running: upload of dev, since 2026-10-05 09:01 UTC
  hotserve-backup: not every app has a fresh backup and a restore proven lately

systemctl list-units 'hotserve*' --all:
  hotserve-backup.service                          activating start  hotserve backup run
  hotserve_backup_upload_dev_b87ecc0bb618.service  activating start  hotserve backup: upload dev
  hotserve-backup.timer                            active     running
  hotserve-backup-drill.timer                      active     waiting

journal (hotserve_backup_* and hotserve-backup.service since 2026-10-05 08:55): plan, clean, dump finished
09:01:26-27; "Starting hotserve_backup_upload_dev_b87ecc0bb618.service" 09:01:27; then NOTHING for 27 h
(restic's stderr goes to the journal: StandardError=journal, backups/unit/unit.go:607).
Also: "hotserve_backup_plan_…/clean_…: Supervising process N which is not our child. We'll most likely
not notice when it exits." (both then "Deactivated successfully") — separate, to look at.

ps: 51386 Ssl 1-03:11:40 futex_wait_queue /usr/bin/restic backup --quiet --json --retry-lock 2h
    --exclude-file /backup-exclude --host hotserve --tag hotserve --tag app:dev /backup/dev
ss -tnpo | grep restic: nothing at that moment.
/proc/51386/fd: 0 /dev/null, 1 …/dev.summary.json, 2 socket (journal), 4 eventpoll, 5 eventfd.

kill -QUIT 51386 → /root/restic-quit.txt on the box, 3525 lines. Filtered (^goroutine |^github.com/restic/restic/):
  goroutine 1 [select] (under a minute in this wait):
    backend/b2.(*b2Backend).Save ← sema ← logger ← retry.(*Backend).Save … retryNotifyErrorWithSuccess
    ← cache.Save ← repository.saveUnpacked(type 3 = lock, 0x90 = 144 bytes) ← restic.SaveJSONUnpacked
    ← restic.(*Lock).createLock ← restic.NewLock ← repository.(*locker).Lock ← repository.Lock
  => restic's first step, creating its lock file, never returned from the B2 backend's Save.
  Hundreds of goroutines in backend.(*watchdogRoundtripper).RoundTrip.func1, [select], ages "", 3 and
  5 minutes, none older (the stuck-request watchdog's 5 min), goroutine ids 573758…575524: requests
  being made continuously right up to the dump.
  Long-lived: finalizer wait 1634 min, chan receive / select 1636 min (start-up goroutines).

After the kill: the timer (Persistent=true) caught up at 12:17:45; status exit 0:
  last run 2026-10-06 12:17 UTC; dev: ok: last complete backup 2026-10-06 12:17 UTC, snapshot 044b2463;
  restore last proven 2026-10-04 13:44 UTC, snapshot 167d2290; no unit … running.
  list-timers: NEXT Tue 2026-10-06 13:01:26 UTC, LAST 12:17:45.

Why it never ended (source read, github.com/Backblaze/blazer v0.7.2 = restic v0.18.0's go.mod):
  b2/writer.go:297-333 simpleWriteFile: `redo:` uploadFile; on error, if reupload(err) →
  blog.V(2) log only, getUploadURL, `goto redo` — no limit, no backoff, nothing at the default log level.
  base/base.go:66-98 Action: AttemptNewUpload (= reupload) for b2_upload_file on any 5xx, 401, 408,
  and 400 "more than one upload using auth token".
  So the loop sits below restic's retry layer (15 min, which prints) and each request completes, so
  restic's 5-minute stuck-request watchdog never fires. Which B2 answer it got: not known (logged only
  at blazer's V(2)).
Elsewhere: restic/restic#21785 (restic 0.18.1, B2, 2026-04-20): "All restic commands that attempt
communication to b2 hang forever with no output"; closed 2026-04-22, "resolved by itself", cause not found.
