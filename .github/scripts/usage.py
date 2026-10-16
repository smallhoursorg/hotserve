#!/usr/bin/env python3
"""usage: the public ledger of what building hotserve costs.

Three sources, one month-level CSV each under usage/, rendered to
docs/footprint.md:

  collect-claude   usage/claude.csv   Claude Code sessions, from the
                   transcripts Claude Code keeps under ~/.claude/projects
                   (mounted read-only at CLAUDE_PROJECTS by the `usage`
                   compose service); only project directories whose name
                   contains --match (default: hotserve), which covers the
                   repo and its worktrees. One row per month and model:
                   API calls and tokens by kind.
  collect-actions  usage/actions.csv  GitHub Actions, from the API for
                   --repo (GH_TOKEN required). One row per month,
                   workflow and runner label: runs, jobs, seconds, billed
                   minutes (each job rounded up to the minute, as GitHub
                   bills).
  render           docs/footprint.md  from the CSVs, usage/copilot.csv
                   (AI credits typed in by hand from GitHub billing) and
                   usage/factors.json (prices, CO2 factors, runner watts,
                   each with its source).

The CSVs hold facts (tokens, seconds); cost and CO2 are recomputed from
factors.json at render time, so a price or factor change never rewrites
history by hand. Months are deliberately the finest grain: per-session
or per-day rows would publish working hours.

A month already in a CSV is replaced by the fresh count unless the fresh
count is smaller: a deleted transcript, or Actions runs past GitHub's
retention — then the committed row stays and a warning says why.
--recompute replaces regardless (for a parser fix).

`make usage` runs all three steps. Stdlib only.
"""

import argparse
import csv
import glob
import http.client
import json
import math
import os
import sys
import tempfile
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime

CLAUDE_CSV = "usage/claude.csv"
ACTIONS_CSV = "usage/actions.csv"
COPILOT_CSV = "usage/copilot.csv"
FACTORS = "usage/factors.json"
FOOTPRINT = "docs/footprint.md"

CLAUDE_COLS = ["calls", "input", "output", "cache_write_5m", "cache_write_1h", "cache_read"]
ACTIONS_COLS = ["runs", "jobs", "seconds", "billed_minutes"]
COPILOT_REVIEW_WORKFLOW = "copilot-pull-request-reviewer"
API_HOST = "api.github.com"


def warn(msg):
    print("warning: " + msg, file=sys.stderr)


# ---- CSV ledger ----------------------------------------------------------


def read_csv(path):
    if not os.path.exists(path):
        return []
    with open(path, newline="") as f:
        return list(csv.DictReader(f))


def write_csv(path, header, rows):
    """Atomic, so a failure mid-write leaves the committed file as it was."""
    fd, tmp = tempfile.mkstemp(dir=os.path.dirname(path) or ".", prefix=".usage-")
    with os.fdopen(fd, "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=header, lineterminator="\n")
        w.writeheader()
        w.writerows(rows)
    os.chmod(tmp, 0o644)
    os.replace(tmp, path)


def merge(path, keys, cols, fresh, recompute):
    """Month-level merge of a fresh count into the committed CSV."""
    old = read_csv(path)
    old_m = {m: [r for r in old if r["month"] == m] for m in {r["month"] for r in old}}
    new_m = {m: [r for r in fresh if r["month"] == m] for m in {r["month"] for r in fresh}}

    def total(rows):
        return sum(int(r[c]) for r in rows for c in cols)

    out = {}
    for m in sorted(set(old_m) | set(new_m)):
        if m not in new_m:
            out[m] = old_m[m]
            warn(f"{path}: {m} is no longer in the source; the committed rows stay")
        elif m in old_m and not recompute and total(new_m[m]) < total(old_m[m]):
            out[m] = old_m[m]
            warn(f"{path}: {m} counts lower now ({total(new_m[m])} < {total(old_m[m])}): "
                 "the source lost data, so the committed rows stay (--recompute overrides)")
        else:
            out[m] = new_m[m]
    rows = [r for m in sorted(out) for r in sorted(out[m], key=lambda r: [r[k] for k in keys])]
    write_csv(path, keys + cols, rows)
    return rows


# ---- Claude Code transcripts --------------------------------------------


def collect_claude(root, match):
    if not os.path.isdir(root):
        sys.exit(f"{root} is not a directory: is ~/.claude/projects mounted? (docker-compose.yml, service usage)")
    dirs = [d for d in sorted(glob.glob(os.path.join(root, "*"))) if match in os.path.basename(d)]
    if not dirs:
        sys.exit(f"no project directory under {root} has '{match}' in its name")
    counts, seen, files = {}, set(), 0
    for d in dirs:
        for path in glob.glob(os.path.join(d, "**", "*.jsonl"), recursive=True):
            files += 1
            with open(path, errors="replace") as f:
                for n, line in enumerate(f):
                    try:
                        rec = json.loads(line)
                    except ValueError:
                        continue
                    msg = rec.get("message")
                    if not isinstance(msg, dict):
                        continue
                    usage, ts, model = msg.get("usage"), rec.get("timestamp"), msg.get("model") or ""
                    # Synthetic records (model "<synthetic>") carry no usage worth counting.
                    if not usage or not ts or not model.startswith("claude"):
                        continue
                    # Claude Code writes one line per content block of a
                    # streamed reply, each repeating the message's usage:
                    # count a message once. (ccusage agrees to the token.)
                    key = (msg["id"], rec.get("requestId")) if msg.get("id") else (path, n)
                    if key in seen:
                        continue
                    seen.add(key)
                    cc = usage.get("cache_creation") or {}
                    w5, w1 = cc.get("ephemeral_5m_input_tokens"), cc.get("ephemeral_1h_input_tokens")
                    if w5 is None and w1 is None:  # older transcripts: no TTL split, billed as 5m
                        w5, w1 = usage.get("cache_creation_input_tokens") or 0, 0
                    c = counts.setdefault((ts[:7], model), dict.fromkeys(CLAUDE_COLS, 0))
                    c["calls"] += 1
                    c["input"] += usage.get("input_tokens") or 0
                    c["output"] += usage.get("output_tokens") or 0
                    c["cache_write_5m"] += w5 or 0
                    c["cache_write_1h"] += w1 or 0
                    c["cache_read"] += usage.get("cache_read_input_tokens") or 0
    print(f"claude: {len(dirs)} project dirs, {files} transcript files, {len(seen)} API calls")
    return [{"month": m, "model": model, **{k: str(v) for k, v in c.items()}} for (m, model), c in counts.items()]


# ---- GitHub Actions -------------------------------------------------------


_conn = threading.local()


def gh(path, token):
    """GET https://api.github.com{path} as JSON.

    One keep-alive connection per thread: a month's run is a few thousand
    calls, and Docker Desktop's network refuses connections when each
    call opens a new one. A dropped connection or a 5xx is retried;
    anything else ends the run with nothing written.
    """
    headers = {
        "Authorization": "Bearer " + token,
        "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28",
        "User-Agent": "hotserve-usage",
    }
    for attempt in range(4):
        conn = getattr(_conn, "c", None)
        if conn is None:
            conn = _conn.c = http.client.HTTPSConnection(API_HOST, timeout=60)
        try:
            conn.request("GET", path, headers=headers)
            resp = conn.getresponse()
            body = resp.read()
        except (OSError, http.client.HTTPException) as e:
            conn.close()
            _conn.c = None
            if attempt == 3:
                sys.exit(f"GitHub API unreachable after 4 attempts ({e}) for {path}; nothing was written")
            warn(f"{e} for {path}; retrying")
            time.sleep(2 ** attempt)
            continue
        if resp.status == 200:
            return json.loads(body.decode())
        if resp.status in (403, 429) and resp.getheader("X-RateLimit-Remaining") == "0":
            sys.exit("GitHub API rate limit exhausted; nothing was written. "
                     f"It resets at epoch {resp.getheader('X-RateLimit-Reset')}")
        if resp.status < 500 or attempt == 3:
            sys.exit(f"GitHub API {resp.status} for {path}: {body.decode(errors='replace')[:200]}")
        time.sleep(2 ** attempt)


def collect_actions(repo, token):
    runs, page = [], 1
    while True:
        batch = gh(f"/repos/{repo}/actions/runs?per_page=100&page={page}", token).get("workflow_runs", [])
        runs += batch
        if len(batch) < 100:
            break
        page += 1

    def jobs(run):
        return run, gh(f"/repos/{repo}/actions/runs/{run['id']}/jobs?per_page=100&filter=all", token).get("jobs", [])

    counts, njobs = {}, 0
    with ThreadPoolExecutor(8) as ex:
        for run, js in ex.map(jobs, runs):
            month = run["created_at"][:7]
            # ".github/workflows/ci.yml" -> "ci"; GitHub's own are
            # "dynamic/agents/copilot-pull-request-reviewer" and the like.
            workflow = os.path.splitext(os.path.basename(run.get("path") or "?"))[0]
            counted = set()
            for j in js:
                if not j.get("started_at") or not j.get("completed_at"):
                    continue
                sec = (datetime.fromisoformat(j["completed_at"]) - datetime.fromisoformat(j["started_at"])).total_seconds()
                if sec <= 0:
                    continue
                runner = ",".join(sorted(j.get("labels") or ["?"]))
                c = counts.setdefault((month, workflow, runner), dict.fromkeys(ACTIONS_COLS, 0))
                c["jobs"] += 1
                c["seconds"] += int(sec)
                c["billed_minutes"] += math.ceil(sec / 60)
                njobs += 1
                if runner not in counted:
                    c["runs"] += 1
                    counted.add(runner)
    print(f"actions: {len(runs)} runs, {njobs} timed jobs from the API")
    return [{"month": m, "workflow": w, "runner": r, **{k: str(v) for k, v in c.items()}}
            for (m, w, r), c in counts.items()]


# ---- render ---------------------------------------------------------------


def family(model):
    for f in ("fable", "mythos", "opus", "sonnet", "haiku"):
        if f in model:
            return "fable" if f == "mythos" else f
    sys.exit(f"{model}: no model family (fable/opus/sonnet/haiku) in the name")


def price(model, prices):
    p = prices["models"].get(model)
    if p is None and model[-8:].isdigit():  # dated snapshot: claude-haiku-4-5-20251001
        p = prices["models"].get(model[:-9])
    return p or prices["families"][family(model)]


def claude_derived(r, factors):
    """(usd, gCO2e) for one claude.csv row."""
    p, mult = price(r["model"], factors["prices_usd_per_mtok"]), factors["prices_usd_per_mtok"]["cache_write_multiplier"]
    g = factors["claude_gco2e_per_mtok"]
    f = g["families"][family(r["model"])]
    inp, out = int(r["input"]), int(r["output"])
    w5, w1, cr = int(r["cache_write_5m"]), int(r["cache_write_1h"]), int(r["cache_read"])
    usd = (inp * p["input"] + out * p["output"] + cr * p["cache_read"]
           + w5 * p["input"] * mult["5m"] + w1 * p["input"] * mult["1h"]) / 1e6
    gco2 = ((inp + (w5 + w1) * g["cache_write_factor"] + cr * g["cache_read_factor"]) * f["input"]
            + out * f["output"]) / 1e6
    return usd, gco2


def runner_watts(label, a):
    spec = a["runners"].get(label, a["runners"]["default"])
    return (spec["vcpu"] * (a["vcpu_min_w"] + a["cpu_utilisation"] * (a["vcpu_max_w"] - a["vcpu_min_w"]))
            + spec["gb"] * a["gb_hour_kwh"] * 1000) * a["pue"]


def read_copilot():
    out = {}
    for r in read_csv(COPILOT_CSV):
        m, v = r.get("month", ""), (r.get("ai_credits_usd") or "").strip()
        if len(m) != 7 or m[4] != "-" or not (m[:4] + m[5:]).isdigit():
            sys.exit(f"{COPILOT_CSV}: month {m!r} is not YYYY-MM")
        try:
            out[m] = float(v) if v else None
        except ValueError:
            sys.exit(f"{COPILOT_CSV}: {m}: ai_credits_usd {v!r} is not a number")
    return out


def fmt(x, d=0):
    return "—" if x is None else f"{x:,.{d}f}"


def table(header, rows):
    lines = ["| " + " | ".join(header) + " |", "|" + "|".join("---:" if i else "---" for i in range(len(header))) + "|"]
    lines += ["| " + " | ".join(r) + " |" for r in rows]
    return "\n".join(lines)


def render():
    with open(FACTORS) as f:
        factors = json.load(f)
    claude, actions, copilot = read_csv(CLAUDE_CSV), read_csv(ACTIONS_CSV), read_copilot()
    a, grid_c = factors["actions"], factors["claude_gco2e_per_mtok"]["grid_gco2e_per_kwh"]
    months = sorted({r["month"] for r in claude} | {r["month"] for r in actions} | set(copilot))

    keys = ["calls", "out", "cr", "usd", "c_kwh", "c_kg", "min", "r_usd", "r_kwh", "r_kg", "reviews", "credits"]

    def month_stats(m):
        """A source with no rows for the month renders as "—", not 0."""
        s = dict.fromkeys(keys)
        s["credits"] = copilot.get(m)
        cl, ac = [r for r in claude if r["month"] == m], [r for r in actions if r["month"] == m]
        if cl:
            s.update(calls=0, out=0.0, cr=0.0, usd=0.0, c_kwh=0.0, c_kg=0.0)
        for r in cl:
            usd, g = claude_derived(r, factors)
            s["calls"] += int(r["calls"]); s["out"] += int(r["output"]) / 1e6; s["cr"] += int(r["cache_read"]) / 1e9
            s["usd"] += usd; s["c_kg"] += g / 1000; s["c_kwh"] += g / grid_c
        if ac:
            s.update(min=0, r_usd=0.0, r_kwh=0.0, reviews=0)
        for r in ac:
            s["min"] += int(r["billed_minutes"])
            s["r_kwh"] += int(r["seconds"]) / 3600 * runner_watts(r["runner"], a) / 1000
            s["r_usd"] += int(r["billed_minutes"]) * a["usd_per_minute_private_repo"]
            if r["workflow"] == COPILOT_REVIEW_WORKFLOW:
                s["reviews"] += int(r["runs"])
        if ac:
            s["r_kg"] = s["r_kwh"] * a["grid_gco2e_per_kwh"] / 1000
        return s

    stats = {m: month_stats(m) for m in months}

    def total(k):
        vals = [s[k] for s in stats.values() if s[k] is not None]
        return sum(vals) if vals else None

    tot = {k: total(k) for k in keys}

    def row(label, s):
        return [label, fmt(s["calls"]), fmt(s["out"], 1), fmt(s["cr"], 1), fmt(s["usd"]), fmt(s["c_kwh"]), fmt(s["c_kg"], 1),
                fmt(s["min"]), fmt(s["r_usd"]), fmt(s["r_kwh"], 1), fmt(s["r_kg"], 1), fmt(s["reviews"]), fmt(s["credits"], 2)]

    monthly = table(
        ["Month", "Claude calls", "Output Mtok", "Cache-read Gtok", "Claude USD", "Claude kWh", "Claude kgCO2e",
         "Runner min", "Runner USD if private", "Runner kWh", "Runner kgCO2e", "Copilot reviews", "Copilot USD"],
        [row(m, stats[m]) for m in months] + [row("**Total**", tot)] if stats else [])

    by_model = {}
    for r in claude:
        c = by_model.setdefault(r["model"], dict.fromkeys(["calls", "in", "out", "w", "cr", "usd", "kg"], 0.0))
        usd, g = claude_derived(r, factors)
        c["calls"] += int(r["calls"]); c["in"] += int(r["input"]) / 1e6; c["out"] += int(r["output"]) / 1e6
        c["w"] += (int(r["cache_write_5m"]) + int(r["cache_write_1h"])) / 1e6; c["cr"] += int(r["cache_read"]) / 1e9
        c["usd"] += usd; c["kg"] += g / 1000
    models = table(
        ["Model", "Calls", "Input Mtok", "Output Mtok", "Cache-write Mtok", "Cache-read Gtok", "Cost USD", "kgCO2e"],
        [[m, fmt(c["calls"]), fmt(c["in"], 2), fmt(c["out"], 1), fmt(c["w"], 1), fmt(c["cr"], 2), fmt(c["usd"]), fmt(c["kg"], 1)]
         for m, c in sorted(by_model.items(), key=lambda kv: -kv[1]["usd"])])

    by_wf = {}
    for r in actions:
        c = by_wf.setdefault(r["workflow"], dict.fromkeys(["runs", "min", "kwh"], 0.0))
        c["runs"] += int(r["runs"]); c["min"] += int(r["billed_minutes"])
        c["kwh"] += int(r["seconds"]) / 3600 * runner_watts(r["runner"], a) / 1000
    workflows = table(["Workflow", "Runs", "Billed minutes", "kWh"],
                      [[w, fmt(c["runs"]), fmt(c["min"]), fmt(c["kwh"], 1)]
                       for w, c in sorted(by_wf.items(), key=lambda kv: -kv[1]["min"])])

    src = factors["_sources"]
    sources = "\n".join(f"- **{label}.** {src[key]}" for key, label in [
        ("prices", "Prices"), ("claude_co2", "CO2 per token"), ("claude_co2_range", "The other end of the range"),
        ("actions_retention", "Run retention"), ("actions", "Runner energy"), ("actions_grid", "Runner grid"),
        ("actions_price", "Runner price"), ("copilot", "Copilot credits")])
    spec = a["runners"]["default"]
    text = f"""# What building hotserve costs

Generated by `make usage` from [`usage/`](../usage): do not edit by hand.
The CSVs there are the facts (tokens, runner seconds, credits); every
derived number below is recomputed from
[`usage/factors.json`](../usage/factors.json) each time, so a price or
factor change updates the whole history at once.

Three things are counted: the Claude Code sessions that write the code
(tokens, priced at Anthropic's pay-as-you-go list prices and converted
to energy and CO2 with published per-token factors), the GitHub Actions
runner minutes that test and release it, and the Copilot AI credits that
review it. Not counted: the maintainer's laptop, web search and fetch
tool charges inside sessions, Copilot's own compute (GitHub publishes no
energy figures), network transfer, and the models' training.

## By month

{monthly}

A dash is a month the source has no rows for. Claude USD is what the
tokens would cost at list price, not what was paid (Claude Code runs on
a subscription); it is the comparable number. Runner minutes are free
for a public repository; "if private" is GitHub's list rate for them.
Every kWh and kgCO2e is an estimate, see below.

## By model

{models}

## By workflow

{workflows}

## Where the numbers come from

- **Claude Code sessions.** Claude Code keeps a transcript of every
  session under `~/.claude/projects`; each API call's token counts are
  read from there, one count per message (checked against ccusage
  20.0.34 on 2026-10-16: identical to the token). Only sessions in this
  repository and its worktrees are counted. Claude Code purges
  transcripts after 30 days by default, and did here until 2026-10-14:
  sessions before 2026-09-13 are gone, so this part of the ledger starts
  there although the repository started on 2026-08-16.
- **Claude USD.** Anthropic's published prices per model (Sources:
  prices). Cache writes cost 1.25× (5-minute) or 2× (1-hour) the input
  price; cache reads the per-model rate.
- **Claude kWh and kgCO2e.** Per-token factors fitted to measured
  Claude 3.7 Sonnet queries and scaled per model family (Sources: CO2
  per token). Cache writes count as input tokens, cache reads at
  {factors['claude_gco2e_per_mtok']['cache_read_factor']}× an input token; kWh is derived back from CO2 at the
  {grid_c} gCO2e/kWh those factors assume. Anthropic publishes no energy
  figures, and published methods disagree by more than an order of
  magnitude (Sources: the other end of the range), so read these as an
  order of magnitude, not a measurement.
- **Runner minutes.** From the GitHub Actions API: every job of every
  run, rounded up to the minute per job as GitHub bills. GitHub deletes
  a public repository's runs after at most 90 days (Sources: run
  retention), which is why the ledger is committed monthly.
- **Runner kWh, kgCO2e and USD.** Every runner is taken as
  {spec['vcpu']} vCPU / {spec['gb']} GB at {int(a['cpu_utilisation'] * 100)}% CPU, {runner_watts('default', a):.1f} W with the data-centre
  overhead (Sources: runner energy), on a {a['grid_gco2e_per_kwh']} gCO2e/kWh grid (Sources:
  runner grid; GitHub does not publish a runner's region), and priced at
  the smallest Linux runner's private-repository rate (Sources: runner
  price).
- **Copilot.** Reviews are the Copilot code-review workflow's runs.
  Credits are typed into `usage/copilot.csv` from GitHub billing each
  month (Sources: Copilot credits).

## Sources

{sources}
"""
    with open(FOOTPRINT, "w") as f:
        f.write(text)
    print(f"render: {FOOTPRINT}, {len(months)} months")


# ---- main -----------------------------------------------------------------


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("steps", nargs="+", choices=["collect-claude", "collect-actions", "render"])
    ap.add_argument("--repo", default="smallhoursorg/hotserve", help="owner/name for collect-actions")
    ap.add_argument("--match", default="hotserve", help="substring of the project directories to count")
    ap.add_argument("--projects", default=os.environ.get("CLAUDE_PROJECTS", "/claude-projects"),
                    help="the mounted ~/.claude/projects")
    ap.add_argument("--recompute", action="store_true", help="replace committed months even when the count shrank")
    args = ap.parse_args()
    for step in args.steps:
        if step == "collect-claude":
            merge(CLAUDE_CSV, ["month", "model"], CLAUDE_COLS, collect_claude(args.projects, args.match), args.recompute)
        elif step == "collect-actions":
            token = os.environ.get("GH_TOKEN")
            if not token:
                sys.exit("GH_TOKEN is not set: `make usage` passes `gh auth token`; log in with `gh auth login`")
            merge(ACTIONS_CSV, ["month", "workflow", "runner"], ACTIONS_COLS, collect_actions(args.repo, token), args.recompute)
        else:
            render()


if __name__ == "__main__":
    main()
