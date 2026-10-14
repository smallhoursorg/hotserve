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
(`packaging/postinstall.sh`, `packaging/preremove.sh`) run as root on
every install, upgrade and removal. These are packaging's examples for
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
| `hotserve init` running while `apt install` configures the package, on a box still on the stock starter: postinstall hashes the stock file, init renames the box's file in, and postinstall's rename puts the starter over it | packaging/postinstall.sh (no lock) | 4 | 2 | 3 | 24, kept by rule 4: closing it means postinstall taking the box's lock (size 3, risk 3) for two root acts at the same moment, and a lock it waits on would let a long apply hold dpkg |
| A box installed from a release without the conffile, taken back to a release with it (v0.3.1-rc1 or earlier), gets dpkg's question about the Caddyfile; Y replaces the box's file with the starter | — | 4 | 2 | 1 | 8; off the documented way back (the `.deb` you upgraded from), then Y against the default; the prompt names the file and the choice |
| A box still on a starter from a release with the conffile keeps it: no record names it, so upgrades leave it alone | packaging/postinstall.sh | 2 | 1 | 5 | 10; the safe direction. `cp /usr/share/hotserve/Caddyfile /etc/hotserve/` then `dpkg-reconfigure hotserve` makes it stock: postinstall records the starter only when it runs on the release whose starter is on disk, so the `cp` alone leaves the next release's upgrade nothing to match |
| A push that installs bytes identical to the starter postinstall last wrote is replaced by the next starter | packaging/postinstall.sh | 2 | 1 | 3 | 6; a starter has no `box` block, so no push reaches such a box anyway |

**Owed** (over the bar, not yet fixed):

| Finding | S | O | D | Where the fix lands |
|---|---|---|---|---|
| `apt purge` leaves `/etc/hotserve/Caddyfile` and `/var/lib/hotserve-package/` behind on a box installed from a release without the conffile, so a purge and a reinstall come up on the old config | 3 | 5 | 3 | 45; a postrm that removes both on purge, in `packaging/postremove.sh`, which the `backup` branch adds |

## History

Dated one-liners; the full text of each is in git.

- 2026-10-14 — Packaging gets a design doc for its triage: its examples
  for the scales, #239's residuals (the starter config out of the
  conffile, #237), and the purge it owes.
