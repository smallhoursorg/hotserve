#!/usr/bin/env python3
"""pin-watch: every pin must declare its exit condition.

Reads the manifest (.github/pin-watch.yml) and .github/dependabot.yml,
verifies they agree — every ignore has a watch, every watch that names
an ignore has a live one; a watch without `ignores` is a pin go.mod
holds itself — then evaluates each watch's unblock condition against
the upstream's latest release. Findings are printed; when
PIN_WATCH_CREATE_ISSUES=1 (set by the workflow) each finding becomes
one labeled, de-duplicated GitHub issue via `gh`.

Contains no dependency-specific logic: new pins are new manifest
entries, never script changes.

Run locally as a dry run: python3 .github/scripts/pin-watch.py
(needs PyYAML, which GitHub runners preinstall; anywhere else, the
python image in docker-compose.yml included, `pip install pyyaml`
first. pin_watch_test.py stubs the module instead: the rules under
test never read YAML, so `make pin-watch-check` runs in that image.)
"""

import json
import os
import re
import subprocess
import sys
import urllib.error
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
    """Order key for Go module versions: the release core, then a
    pre-release sorts before the release of the same core (Go's
    pseudo-versions are pre-releases: v0.30.3-0.<time>-<hash> < v0.30.3)
    and pre-releases of one core sort by their string, which for
    pseudo-versions is by commit time. Build metadata is ignored."""
    v = v.lstrip("v").split("+")[0]
    core, _, pre = v.partition("-")
    return (tuple(int(p) if p.isdigit() else 0 for p in core.split(".")),
            0 if pre else 1,
            # semver 11: dot-separated identifiers, numeric ones as
            # numbers (beta.9 < beta.10), numeric before alphanumeric.
            tuple((0, int(i), "") if i.isdigit() else (1, 0, i) for i in pre.split(".")))


def latest_release_tag(repo):
    data = json.loads(http_get(f"https://api.github.com/repos/{repo}/releases/latest"))
    return data["tag_name"]


def gomod_requirement(repo, tag, module):
    return requirement_in(http_get(f"https://raw.githubusercontent.com/{repo}/{tag}/go.mod"), module)


def directives(text):
    """(directive, rest) for every line of a go.mod or go.work, comments
    stripped, a block's lines carrying the block's directive."""
    block = None
    for line in text.splitlines():
        line = line.split("//", 1)[0].strip()
        if not line:
            continue
        if block:
            if line == ")":
                block = None
            else:
                yield block, line
            continue
        words = line.split(None, 1)
        rest = words[1].strip() if len(words) > 1 else ""
        if rest == "(":
            block = words[0]
        else:
            yield words[0], rest


def requirement_in(gomod, module):
    """The version a go.mod requires `module` at, in a require block or
    a one-line require; None when it does not. replace, exclude and
    retract lines name modules with versions too and are not
    requirements."""
    for directive, rest in directives(gomod):
        m = directive == "require" and re.match(rf"{re.escape(module)}\s+(v[0-9]\S*)$", rest)
        if m:
            return m.group(1)
    return None


WATCH_FIELDS = ("repo", "module", "min_version")
# A Go module version: vMAJOR.MINOR.PATCH, a pre-release (pseudo-versions
# are one), build metadata. ver_key reads anything, mapping what is not
# a number to zero, so the shape is checked here before a `banana` could
# compare below every real version and fire the watch.
GO_VERSION = re.compile(r"^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$")


def watch_shape(watch):
    """What is wrong with a watch's unblock condition, as text, or None.
    Checked before anything is evaluated or fetched: a missing or
    non-text field would otherwise surface as a swallowed exception
    (a transient-looking warning) or a traceback, and the pin would
    rot with no issue."""
    cond = watch.get("unblock")
    if not isinstance(cond, dict):
        return "has no `unblock` mapping"
    if cond.get("type") != "upstream-gomod-requires":
        return (f"has condition type `{cond.get('type')}`; supported types: "
                "`upstream-gomod-requires`")
    bad = [f for f in WATCH_FIELDS if not isinstance(cond.get(f), str) or not cond[f].strip()]
    if bad:
        return "lacks text for " + ", ".join(f"`{f}`" for f in bad) + " (quote a version like 0.31)"
    if not GO_VERSION.match(cond["min_version"]):
        return f"has min_version `{cond['min_version']}`, not a Go module version (vX.Y.Z, pseudo-versions included)"
    return None


def evaluate(watch, findings):
    cond = watch.get("unblock") or {}
    wid = watch.get("id") or watch.get("ignores") or "?"
    shape = watch_shape(watch)
    if shape:
        findings.append((
            f"pin-watch: {wid} is malformed",
            f"The watch {shape}. Fix the entry in `{MANIFEST}`; until then the "
            "pin is unwatched.",
        ))
        return
    try:
        tag = latest_release_tag(cond["repo"])
        found = gomod_requirement(cond["repo"], tag, cond["module"])
    except urllib.error.HTTPError as e:
        if e.code == 404:
            # Not transient: the repo has no releases, or is not where the
            # watch says. Left as a warning the pin would rot unwatched.
            findings.append((
                f"pin-watch: {wid} — upstream not found",
                f"`{cond['repo']}` has no latest release or no go.mod at it "
                f"(HTTP 404 from {e.url}). Fix `repo` in `{MANIFEST}`; until "
                "then the pin is unwatched.",
            ))
            return
        print(f"warning: {wid}: could not evaluate ({e})", file=sys.stderr)
        return
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
# GitHub keeps this many characters of a dismissal comment. The comment
# is the dismissal's versioned evidence, so one that would be cut is
# refused rather than posted short.
COMMENT_LIMIT = 280


def comment_fits(kind, key, comment, findings):
    """The comment is the dismissal's evidence, posted with it: text,
    present, and no longer than GitHub keeps. Anything else is a
    finding and the entry dismisses nothing."""
    if not isinstance(comment, str) or not comment.strip():
        findings.append((
            f"pin-watch: {kind} dismissal {key} has no comment",
            "The comment is the dismissal's evidence, posted with it; an "
            "entry without one — or with a bare date, number or flag where "
            f"text should be — is not a reviewed dismissal. Add it in `{MANIFEST}`.",
        ))
        return False
    if len(comment) <= COMMENT_LIMIT:
        return True
    findings.append((
        f"pin-watch: {kind} dismissal {key} has a comment over "
        f"{COMMENT_LIMIT} characters",
        f"GitHub keeps {COMMENT_LIMIT} characters of a dismissal comment and "
        f"this one is {len(comment)}, so the posted text would differ from "
        f"`{MANIFEST}`. Shorten it; the YAML comment above the entry holds "
        "the long form.",
    ))
    return False


def gh(args, token=None):
    env = dict(os.environ)
    if token:
        env["GH_TOKEN"] = token
    return subprocess.check_output(["gh", *args], env=env)


def gh_pages(args, token=None):
    """`gh api --paginate --jq '[...]'` prints one JSON array per page,
    back to back, so a listing past 100 rows is not one document.
    Decode each and return the rows of all pages as one list."""
    text = gh(args, token).decode()
    rows, pos, dec = [], 0, json.JSONDecoder()
    while True:
        while pos < len(text) and text[pos].isspace():
            pos += 1
        if pos >= len(text):
            return rows
        page, pos = dec.raw_decode(text, pos)
        rows.extend(page)


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
    alerts = gh_pages(
        ["api", "--paginate",
         f"repos/{os.environ['GH_REPO']}/dependabot/alerts?per_page=100",
         "--jq",
         "[.[] | {number, state, ghsa: .security_advisory.ghsa_id, fixed_at, "
         "manifest: .dependency.manifest_path, package: .dependency.package.name}]"],
        token=token)
    by_ghsa = {}
    for a in alerts:
        by_ghsa.setdefault(a["ghsa"], []).append(a)
    for d in dismissals:
        ghsa, reason = d.get("ghsa"), d.get("reason")
        comment = d.get("comment")
        if reason not in VALID_DISMISS_REASONS:
            findings.append((
                f"pin-watch: dismissal {ghsa} has invalid reason",
                f"`{reason}` is not one of {sorted(VALID_DISMISS_REASONS)}.",
            ))
            continue
        # Recorded, not fatal: the match checks below still run, so one
        # pass names everything wrong with the entry.
        fits = comment_fits("alert", ghsa, comment, findings)
        matched = by_ghsa.get(ghsa)
        if not matched:
            findings.append((
                f"pin-watch: dismissal {ghsa} matches no alert",
                f"No Dependabot alert in any state has GHSA `{ghsa}` — "
                f"stale entry in `{MANIFEST}`, delete it.",
            ))
            continue
        # Dependabot raises one alert per go.mod and package, so the
        # states are read per (manifest, package). Open and dismissed
        # are the states this entry works on and makes. Fixed or
        # auto_dismissed means that pair moved on: every pair moved on
        # makes the entry stale, and an alert open where its pair had
        # moved on — a new alert beside a fixed one, or a fixed alert
        # reopened (fixed_at set) — is a reintroduction by another
        # path, not the alert the entry reviewed: named, not dismissed.
        # A pair still open beside one that moved on is a bump landing
        # module by module, and is dismissed as before.
        by_pair = {}
        for a in matched:
            by_pair.setdefault((a.get("manifest"), a.get("package")), []).append(a)
        moved = ("fixed", "auto_dismissed")
        moved_on = {k for k, rows in by_pair.items() if any(a["state"] in moved for a in rows)}
        again = [a for a in matched if a["state"] == "open"
                 and ((a.get("manifest"), a.get("package")) in moved_on or a.get("fixed_at"))]
        if again:
            findings.append((
                f"pin-watch: dismissal {ghsa} met a reintroduction",
                f"GHSA `{ghsa}` was fixed and is open again in "
                + ", ".join(f"`{a.get('package')}` in `{a.get('manifest')}` (#{a['number']})"
                            for a in again)
                + ": a dependency came back by another path. NOT "
                f"dismissed — triage afresh, then amend or delete the entry "
                f"in `{MANIFEST}`.",
            ))
            continue
        # Stale only when nothing stands on the entry any more: a pair
        # holding a dismissed alert beside a fixed one is a live
        # dismissal whose evidence the entry is.
        if all(a["state"] in moved for a in matched):
            findings.append((
                f"pin-watch: dismissal {ghsa} outlived its alerts",
                f"Dependabot alerts with GHSA `{ghsa}` are "
                + ", ".join(f"#{a['number']} {a['state']}" for a in matched)
                + ". The dependency moved on, so the entry in "
                f"`{MANIFEST}` is stale: delete it (and its `govulncheck:` "
                "key).",
            ))
            continue
        if not fits:
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
                "-f", f"dismissed_comment={comment}"],
               token=token)
            print(f"dismissed alert #{a['number']} ({ghsa}) as {reason}")


VALID_CODE_SCANNING_REASONS = {"false positive", "won't fix", "used in tests"}


def reconcile_code_scanning(dismissals, findings):
    """Dismiss open code-scanning alerts matching a declared rule+path.
    Uses the workflow's own GITHUB_TOKEN (security-events: write is
    grantable to it, unlike the Dependabot alerts API). Entries are
    keyed on rule + file, never alert number — numbers change when a
    refactor re-detects the same flow — and `expect` caps how many
    open alerts one entry may silence: more matches means a NEW flow
    appeared, so nothing is dismissed and an issue says why."""
    if not dismissals:
        return
    live = os.environ.get("PIN_WATCH_CREATE_ISSUES") == "1"
    alerts = gh_pages(
        ["api", "--paginate",
         f"repos/{os.environ['GH_REPO']}/code-scanning/alerts?per_page=100",
         "--jq",
         "[.[] | {number, state, rule: .rule.id, "
         "path: .most_recent_instance.location.path}]"])
    for d in dismissals:
        rule, path_, reason = d.get("rule"), d.get("path"), d.get("reason")
        comment = d.get("comment")
        key = f"{rule} in {path_}"
        if reason not in VALID_CODE_SCANNING_REASONS:
            findings.append((
                f"pin-watch: code-scanning dismissal {key} has invalid reason",
                f"`{reason}` is not one of {sorted(VALID_CODE_SCANNING_REASONS)}.",
            ))
            continue
        fits = comment_fits("code-scanning", key, comment, findings)
        matched = [a for a in alerts if a["rule"] == rule and a["path"] == path_]
        if not matched:
            findings.append((
                f"pin-watch: code-scanning dismissal {key} matches no alert",
                f"No code-scanning alert in any state matches rule `{rule}` in "
                f"`{path_}` — stale entry in `{MANIFEST}`, delete it.",
            ))
            continue
        open_matches = [a for a in matched if a["state"] == "open"]
        expect = int(d.get("expect", 1))
        if len(open_matches) > expect:
            findings.append((
                f"pin-watch: code-scanning dismissal {key} matched "
                f"{len(open_matches)} open alerts (expect {expect})",
                "More open alerts than the entry declares — a NEW flow has "
                f"appeared in `{path_}`. Refusing to dismiss any of them; "
                "triage the new alert, then raise `expect` (or fix the code) "
                f"in `{MANIFEST}`.\n\nAlert numbers: "
                + ", ".join(f"#{a['number']}" for a in open_matches),
            ))
            continue
        if not fits:
            continue
        for a in open_matches:
            if not live:
                print(f"dry run: would dismiss code-scanning alert "
                      f"#{a['number']} ({key}) as {reason}")
                continue
            gh(["api", "-X", "PATCH",
                f"repos/{os.environ['GH_REPO']}/code-scanning/alerts/{a['number']}",
                "-f", "state=dismissed",
                "-f", f"dismissed_reason={reason}",
                "-f", f"dismissed_comment={comment}"])
            print(f"dismissed code-scanning alert #{a['number']} ({key}) as {reason}")


GOWORK = "go.work"


def workspace_gomods(gowork=GOWORK):
    """Every go.mod the workspace builds, from go.work's use
    directives, one-line or in a block."""
    with open(gowork) as f:
        dirs = [rest.strip('"') for directive, rest in directives(f.read()) if directive == "use"]
    base = os.path.dirname(os.path.abspath(gowork))
    return [os.path.normpath(os.path.join(base, d, "go.mod")) for d in dirs]


def requirements_here(module, gomods):
    """{go.mod path: version} for every workspace go.mod requiring module."""
    out = {}
    for path in gomods:
        if not os.path.exists(path):
            continue
        with open(path) as f:
            v = requirement_in(f.read(), module)
        if v:
            out[os.path.relpath(path)] = v
    return out


def mappings(watches, findings):
    """The watches that are mappings; every other entry (a bare null, a
    string) is a finding and is dropped, where touching its fields
    would have aborted the run before any issue was opened."""
    ok = []
    for i, w in enumerate(watches):
        if not isinstance(w, dict):
            findings.append((
                f"pin-watch: watch #{i + 1} is not a mapping",
                f"`watches[{i}]` in `{MANIFEST}` is `{w!r}`, not a mapping with "
                "`id`, `unblock` and the rest; fix or delete it.",
            ))
        elif "ignores" in w and not (isinstance(w["ignores"], str) and w["ignores"].strip()):
            findings.append((
                f"pin-watch: watch #{i + 1} has an `ignores` that is not text",
                f"`watches[{i}].ignores` in `{MANIFEST}` is `{w['ignores']!r}`; it "
                "names one dependabot.yml ignore, as text, or is left out for a "
                "pin go.mod holds. Fix it.",
            ))
        else:
            ok.append(w)
    return ok


def sync_findings(watches, ignores, gomods=None):
    """Every Dependabot ignore has a watch and every watch naming an
    ignore has a live one. A watch without `ignores` is a pin that
    go.mod holds itself (a require at a pseudo-version, say): it has no
    Dependabot side to agree with, so it agrees with go.mod instead —
    every workspace go.mod that requires the module does so at exactly
    the watch's min_version, the version upstream has to reach for the
    lines to go, and at least one does. go.work is read only when such
    a watch exists."""
    watched = {w["ignores"] for w in watches if w.get("ignores")}
    out = [
        f"- dependabot ignores `{n}` but `{MANIFEST}` has no watch for it — "
        "every ignore must declare its exit condition"
        for n in sorted(ignores - watched)
    ] + [
        f"- `{MANIFEST}` watches `{n}` but dependabot no longer ignores it — "
        "delete the stale watch"
        for n in sorted(watched - ignores)
    ]
    for w in watches:
        if w.get("ignores"):
            continue
        wid = w.get("id")
        if watch_shape(w):
            # evaluate names what is wrong; here only that go.mod
            # cannot be checked against it.
            out.append(
                f"- `{MANIFEST}` watch `{wid}` names no ignore, so it is a pin "
                "go.mod holds, but it is malformed (see its own finding) — the "
                "pin cannot be checked against go.mod")
            continue
        cond = w["unblock"]
        module, want = cond["module"], cond["min_version"]
        if gomods is None:
            gomods = workspace_gomods()
        have = requirements_here(module, gomods)
        if not have:
            out.append(
                f"- `{MANIFEST}` watch `{wid}` is a pin go.mod holds, but no "
                f"workspace go.mod requires `{module}` — delete the watch")
        for path, v in sorted(have.items()):
            if v == want:
                continue
            if ver_key(v) > ver_key(want):
                # Module graph pruning keeps an indirect line for every
                # module in the build, so the pin is never "dropped":
                # once upstream requires more, tidy rewrites the line to
                # upstream's version and the pin is no longer ours.
                out.append(
                    f"- `{MANIFEST}` watch `{wid}` is a pin go.mod held, but "
                    f"`{path}` now requires `{module}` at {v}, past its "
                    f"min_version {want}: upstream moved on and the line is "
                    "upstream's — delete the watch")
            else:
                out.append(
                    f"- `{MANIFEST}` watch `{wid}` is a pin go.mod holds, but "
                    f"`{path}` requires `{module}` at {v}, below its min_version "
                    f"{want} — re-pin both together, or delete the watch")
    return out


def main():
    manifest = load(MANIFEST)
    ignores = set()
    for update in load(DEPENDABOT).get("updates") or []:
        for ig in update.get("ignore") or []:
            ignores.add(ig["dependency-name"])

    findings = []
    watches = mappings(manifest.get("watches") or [], findings)

    sync = sync_findings(watches, ignores)
    if sync:
        findings.append(
            ("pin-watch: manifest and dependabot ignores are out of sync",
             "\n".join(sync)))

    for w in watches:
        evaluate(w, findings)

    reconcile_alerts(manifest.get("alert_dismissals") or [], findings)
    reconcile_code_scanning(manifest.get("code_scanning_dismissals") or [], findings)

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
