#!/usr/bin/env python3
"""pin-watch: every Dependabot ignore must declare its exit condition.

Reads the manifest (.github/pin-watch.yml) and .github/dependabot.yml,
verifies they agree — every ignore has a watch, every watch has a live
ignore — then evaluates each watch's unblock condition against the
upstream's latest release. Findings are printed; when
PIN_WATCH_CREATE_ISSUES=1 (set by the workflow) each finding becomes
one labeled, de-duplicated GitHub issue via `gh`.

Contains no dependency-specific logic: new pins are new manifest
entries, never script changes.

Run locally as a dry run: python3 .github/scripts/pin-watch.py
(needs PyYAML — preinstalled on GitHub runners and in python:3 images).
"""

import json
import os
import re
import subprocess
import sys
import urllib.request

import yaml

MANIFEST = ".github/pin-watch.yml"
DEPENDABOT = ".github/dependabot.yml"
LABEL = "pin-watch"


def load(path):
    with open(path) as f:
        return yaml.safe_load(f) or {}


def http_get(url):
    headers = {"User-Agent": "pin-watch"}
    token = os.environ.get("GH_TOKEN") or os.environ.get("GITHUB_TOKEN")
    if token and url.startswith("https://api.github.com/"):
        headers["Authorization"] = "Bearer " + token
    req = urllib.request.Request(url, headers=headers)
    with urllib.request.urlopen(req, timeout=30) as resp:  # noqa: S310 — fixed https hosts
        return resp.read().decode()


def ver_key(v):
    """Order key for vX.Y.Z-ish strings; pre-release/build metadata is
    ignored (good enough for 'has the requirement reached vX.Y')."""
    core = v.lstrip("v").split("-")[0].split("+")[0]
    return tuple(int(p) if p.isdigit() else 0 for p in core.split("."))


def latest_release_tag(repo):
    data = json.loads(http_get(f"https://api.github.com/repos/{repo}/releases/latest"))
    return data["tag_name"]


def gomod_requirement(repo, tag, module):
    gomod = http_get(f"https://raw.githubusercontent.com/{repo}/{tag}/go.mod")
    m = re.search(rf"^\s*{re.escape(module)}\s+(v[0-9][^\s]*)", gomod, re.M)
    return m.group(1) if m else None


def evaluate(watch, findings):
    cond = watch.get("unblock") or {}
    wid = watch.get("id") or watch.get("ignores") or "?"
    if cond.get("type") != "upstream-gomod-requires":
        findings.append((
            f"pin-watch: {wid} has an unknown condition type",
            f"`{cond.get('type')}` is not implemented; supported types: "
            f"`upstream-gomod-requires`. Fix the entry in `{MANIFEST}`.",
        ))
        return
    try:
        tag = latest_release_tag(cond["repo"])
        found = gomod_requirement(cond["repo"], tag, cond["module"])
    except Exception as e:  # noqa: BLE001 — transient API failure: next week retries
        print(f"warning: {wid}: could not evaluate ({e})", file=sys.stderr)
        return
    action = watch.get("action", "")
    if found is None:
        findings.append((
            f"pin-watch: {wid} — upstream no longer requires the module",
            f"{cond['repo']} **{tag}** does not require `{cond['module']}` at all; "
            f"the pin's premise is gone.\n\nAction: {action}",
        ))
    elif ver_key(found) >= ver_key(cond["min_version"]):
        findings.append((
            f"pin-watch: {wid} — unblock condition met",
            f"{cond['repo']} **{tag}** requires `{cond['module']} {found}` "
            f"(condition: ≥ {cond['min_version']}).\n\n"
            f"Reason for the pin: {watch.get('reason', '')}\n\nAction: {action}",
        ))
    else:
        print(f"ok: {wid}: {cond['repo']} {tag} requires "
              f"{cond['module']} {found} (< {cond['min_version']}); still pinned")


def open_issues(findings):
    subprocess.run(  # idempotent; needs issues:write
        ["gh", "label", "create", LABEL, "--force",
         "--description", "opened by the pin-watch workflow", "--color", "d93f0b"],
        check=False, capture_output=True)
    out = subprocess.check_output(
        ["gh", "issue", "list", "--label", LABEL, "--state", "open",
         "--json", "title", "--jq", "[.[].title]"])
    existing = set(json.loads(out))
    for title, body in findings:
        if title in existing:
            print(f"issue already open: {title}")
            continue
        subprocess.check_call(
            ["gh", "issue", "create", "--title", title, "--body", body, "--label", LABEL])
        print(f"opened issue: {title}")


VALID_DISMISS_REASONS = {
    "fix_started", "inaccurate", "no_bandwidth", "not_used", "tolerable_risk"}


def gh(args, token=None):
    env = dict(os.environ)
    if token:
        env["GH_TOKEN"] = token
    return subprocess.check_output(["gh", *args], env=env)


def reconcile_alerts(dismissals, findings):
    """Dismiss open Dependabot alerts whose GHSA is declared in the
    manifest. Needs DEPENDABOT_ALERTS_TOKEN (the Actions GITHUB_TOKEN
    cannot access the Dependabot alerts API); without it, one
    deduplicated issue points at the setup instructions instead."""
    if not dismissals:
        return
    token = os.environ.get("DEPENDABOT_ALERTS_TOKEN")
    if not token:
        findings.append((
            "pin-watch: alert reconciliation is unconfigured",
            f"`{MANIFEST}` declares alert dismissals, but the "
            "`DEPENDABOT_ALERTS_TOKEN` secret is not set, and the Actions "
            "GITHUB_TOKEN cannot access the Dependabot alerts API.\n\n"
            "Create a fine-grained PAT with **Dependabot alerts: read-write** "
            "on this repository only and add it as an Actions secret named "
            "`DEPENDABOT_ALERTS_TOKEN` (see README → Dependency policy).",
        ))
        return
    live = os.environ.get("PIN_WATCH_CREATE_ISSUES") == "1"
    alerts = json.loads(gh(
        ["api", "--paginate",
         f"repos/{os.environ['GH_REPO']}/dependabot/alerts?per_page=100",
         "--jq",
         "[.[] | {number, state, ghsa: .security_advisory.ghsa_id}]"],
        token=token))
    by_ghsa = {}
    for a in alerts:
        by_ghsa.setdefault(a["ghsa"], []).append(a)
    for d in dismissals:
        ghsa, reason = d.get("ghsa"), d.get("reason")
        if reason not in VALID_DISMISS_REASONS:
            findings.append((
                f"pin-watch: dismissal {ghsa} has invalid reason",
                f"`{reason}` is not one of {sorted(VALID_DISMISS_REASONS)}.",
            ))
            continue
        matched = by_ghsa.get(ghsa)
        if not matched:
            findings.append((
                f"pin-watch: dismissal {ghsa} matches no alert",
                f"No Dependabot alert in any state has GHSA `{ghsa}` — "
                f"stale entry in `{MANIFEST}`, delete it.",
            ))
            continue
        for a in matched:
            if a["state"] != "open":
                continue
            if not live:
                print(f"dry run: would dismiss alert #{a['number']} "
                      f"({ghsa}) as {reason}")
                continue
            gh(["api", "-X", "PATCH",
                f"repos/{os.environ['GH_REPO']}/dependabot/alerts/{a['number']}",
                "-f", "state=dismissed",
                "-f", f"dismissed_reason={reason}",
                "-f", f"dismissed_comment={d.get('comment', '')[:280]}"],
               token=token)
            print(f"dismissed alert #{a['number']} ({ghsa}) as {reason}")


def main():
    manifest = load(MANIFEST)
    watches = manifest.get("watches") or []
    ignores = set()
    for update in load(DEPENDABOT).get("updates") or []:
        for ig in update.get("ignore") or []:
            ignores.add(ig["dependency-name"])

    findings = []

    watched = {w.get("ignores") for w in watches}
    sync = [
        f"- dependabot ignores `{n}` but `{MANIFEST}` has no watch for it — "
        "every ignore must declare its exit condition"
        for n in sorted(ignores - watched)
    ] + [
        f"- `{MANIFEST}` watches `{n}` but dependabot no longer ignores it — "
        "delete the stale watch"
        for n in sorted(watched - ignores)
    ]
    if sync:
        findings.append(
            ("pin-watch: manifest and dependabot ignores are out of sync",
             "\n".join(sync)))

    for w in watches:
        evaluate(w, findings)

    reconcile_alerts(manifest.get("alert_dismissals") or [], findings)

    if not findings:
        print("pin-watch: every pin accounted for, no conditions met")
        return 0
    for title, body in findings:
        print(f"\nFINDING: {title}\n{body}")
    if os.environ.get("PIN_WATCH_CREATE_ISSUES") == "1":
        open_issues(findings)
    return 0


if __name__ == "__main__":
    sys.exit(main())
