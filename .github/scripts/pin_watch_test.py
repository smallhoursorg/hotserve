#!/usr/bin/env python3
"""The manifest rules of pin-watch.py, run by `make pin-watch-check`:
stdlib unittest, `gh` replaced by a fake. PyYAML is stubbed out so the
tests run in the python image too; nothing under test reads YAML."""
import contextlib
import importlib.util
import io
import json
import os
import sys
import tempfile
import types
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.modules.setdefault("yaml", types.ModuleType("yaml"))
spec = importlib.util.spec_from_file_location("pin_watch", os.path.join(HERE, "pin-watch.py"))
pw = importlib.util.module_from_spec(spec)
spec.loader.exec_module(pw)

GHSA = "GHSA-xxxx-yyyy-zzzz"


class FakeGH:
    """Answers the paginated listing the way `gh api --paginate --jq`
    does — one JSON array per page, back to back — and records every
    other call (the PATCH that dismisses)."""

    def __init__(self, alerts, per_page=100):
        self.pages = [alerts[i:i + per_page] for i in range(0, len(alerts), per_page)] or [[]]
        self.calls = []

    def __call__(self, args, token=None):
        if "--paginate" in args:
            return "\n".join(json.dumps(p) for p in self.pages).encode()
        self.calls.append(args)
        return b""


def entry(**kw):
    e = {"ghsa": GHSA, "reason": "not_used", "comment": "c"}
    e.update(kw)
    return e


def alert(number, state, manifest="go.mod", ghsa=GHSA, package="example.com/m/v3", fixed_at=None):
    return {"number": number, "state": state, "ghsa": ghsa, "manifest": manifest,
            "package": package, "fixed_at": fixed_at}


class DismissalTests(unittest.TestCase):
    def setUp(self):
        self.env = dict(os.environ)
        os.environ["GH_REPO"] = "o/r"
        os.environ["DEPENDABOT_ALERTS_TOKEN"] = "t"
        os.environ.pop("PIN_WATCH_CREATE_ISSUES", None)
        self.addCleanup(lambda: (os.environ.clear(), os.environ.update(self.env)))

    def run_alerts(self, alerts, dismissals, live=False, per_page=100):
        if live:
            os.environ["PIN_WATCH_CREATE_ISSUES"] = "1"
        fake = FakeGH(alerts, per_page)
        pw.gh = fake
        findings = []
        with contextlib.redirect_stdout(io.StringIO()) as out:
            pw.reconcile_alerts(dismissals, findings)
        return findings, fake.calls, out.getvalue()

    def run_code_scanning(self, alerts, dismissals):
        fake = FakeGH(alerts)
        pw.gh = fake
        findings = []
        with contextlib.redirect_stdout(io.StringIO()):
            pw.reconcile_code_scanning(dismissals, findings)
        return findings, fake.calls

    def test_a_comment_over_the_limit_is_refused_not_posted_cut(self):
        findings, calls, out = self.run_alerts(
            [{"number": 1, "state": "open", "ghsa": GHSA}],
            [entry(comment="x" * (pw.COMMENT_LIMIT + 1))], live=True)
        self.assertEqual([f[0] for f in findings],
                         [f"pin-watch: alert dismissal {GHSA} has a comment over {pw.COMMENT_LIMIT} characters"])
        self.assertEqual(calls, [])
        self.assertNotIn("dismissed", out)

    def test_a_comment_at_the_limit_is_posted_whole(self):
        comment = "y" * pw.COMMENT_LIMIT
        findings, calls, out = self.run_alerts(
            [{"number": 7, "state": "open", "ghsa": GHSA}],
            [entry(comment=comment)], live=True)
        self.assertEqual(findings, [])
        self.assertEqual(len(calls), 1)
        self.assertIn("repos/o/r/dependabot/alerts/7", calls[0])
        self.assertIn(f"dismissed_comment={comment}", calls[0])
        self.assertIn("dismissed alert #7", out)

    def test_a_bare_or_missing_comment_is_no_evidence_and_dismisses_nothing(self):
        for e in (entry(comment=None), entry(comment="  "), {"ghsa": GHSA, "reason": "not_used"}):
            findings, calls, out = self.run_alerts([alert(1, "open")], [e], live=True)
            self.assertEqual([f[0] for f in findings], [f"pin-watch: alert dismissal {GHSA} has no comment"])
            self.assertEqual(calls, [])

    def test_every_manifest_fixed_makes_the_entry_stale(self):
        findings, calls, out = self.run_alerts(
            [alert(1, "fixed", "go.mod"), alert(2, "fixed", "box/go.mod")], [entry()])
        self.assertEqual([f[0] for f in findings], [f"pin-watch: dismissal {GHSA} outlived its alerts"])
        self.assertIn("#1 fixed, #2 fixed", findings[0][1])
        self.assertEqual(calls, [])

    def test_an_open_alert_in_a_manifest_that_was_fixed_is_a_reintroduction_not_dismissed(self):
        findings, calls, out = self.run_alerts(
            [alert(1, "fixed", "go.mod"), alert(9, "open", "go.mod")], [entry()], live=True)
        self.assertEqual([f[0] for f in findings], [f"pin-watch: dismissal {GHSA} met a reintroduction"])
        self.assertIn("#9", findings[0][1])
        self.assertEqual(calls, [])
        self.assertNotIn("dismissed alert", out)

    def test_one_ghsa_over_two_packages_in_one_go_mod_is_read_per_package(self):
        findings, calls, out = self.run_alerts(
            [alert(1, "fixed", package="example.com/m/v4"), alert(2, "open", package="example.com/m/v3")],
            [entry()], live=True)
        self.assertEqual(findings, [])
        self.assertIn("dismissed alert #2", out)

    def test_a_fixed_alert_reopened_in_place_is_a_reintroduction(self):
        findings, calls, out = self.run_alerts(
            [alert(12, "open", fixed_at="2026-10-25T20:39:37Z")], [entry()], live=True)
        self.assertEqual([f[0] for f in findings], [f"pin-watch: dismissal {GHSA} met a reintroduction"])
        self.assertIn("#12", findings[0][1])
        self.assertEqual(calls, [])

    def test_a_comment_that_is_not_text_is_no_comment(self):
        import datetime
        for c in (20260817, datetime.date(2026, 8, 17), True, ["a"]):
            findings, calls, out = self.run_alerts([alert(1, "open")], [entry(comment=c)], live=True)
            self.assertEqual([f[0] for f in findings], [f"pin-watch: alert dismissal {GHSA} has no comment"], c)
            self.assertEqual(calls, [])

    def test_a_bump_landing_module_by_module_still_dismisses_the_rest(self):
        findings, calls, out = self.run_alerts(
            [alert(1, "fixed", "box/go.mod"), alert(2, "open", "go.mod"),
             alert(3, "open", "liveswap/go.mod")], [entry()], live=True)
        self.assertEqual(findings, [])
        self.assertEqual(len(calls), 2)
        self.assertIn("dismissed alert #2", out)
        self.assertIn("dismissed alert #3", out)

    def test_a_dismissed_alert_beside_a_fixed_one_still_stands_on_the_entry(self):
        findings, calls, out = self.run_alerts(
            [alert(1, "fixed"), alert(2, "dismissed")], [entry()], live=True)
        self.assertEqual((findings, calls, out), ([], [], ""))

    def test_auto_dismissed_counts_as_moved_on(self):
        findings, calls, out = self.run_alerts([alert(1, "auto_dismissed")], [entry()])
        self.assertEqual(len(findings), 1)
        self.assertIn("outlived", findings[0][0])

    def test_alerts_this_entry_dismissed_are_the_steady_state(self):
        findings, calls, out = self.run_alerts(
            [{"number": 1, "state": "dismissed", "ghsa": GHSA},
             {"number": 2, "state": "dismissed", "ghsa": GHSA}], [entry()])
        self.assertEqual((findings, calls, out), ([], [], ""))

    def test_an_open_alert_is_named_in_a_dry_run(self):
        findings, calls, out = self.run_alerts(
            [{"number": 3, "state": "open", "ghsa": GHSA},
             {"number": 4, "state": "dismissed", "ghsa": GHSA}], [entry()])
        self.assertEqual((findings, calls), ([], []))
        self.assertIn("would dismiss alert #3", out)
        self.assertNotIn("#4", out)

    def test_a_long_comment_and_stale_alerts_are_both_named_in_one_pass(self):
        findings, calls, out = self.run_alerts(
            [{"number": 1, "state": "fixed", "ghsa": GHSA}],
            [entry(comment="x" * (pw.COMMENT_LIMIT + 1))])
        self.assertEqual(len(findings), 2)

    def test_a_listing_over_one_page_is_read_whole(self):
        alerts = [{"number": n, "state": "dismissed", "ghsa": "GHSA-other"} for n in range(150)]
        alerts.append({"number": 150, "state": "open", "ghsa": GHSA})
        findings, calls, out = self.run_alerts(alerts, [entry()], per_page=100)
        self.assertEqual(findings, [])
        self.assertIn("would dismiss alert #150", out)

    def test_code_scanning_comment_over_the_limit_is_refused(self):
        findings, calls = self.run_code_scanning(
            [{"number": 9, "state": "open", "rule": "r", "path": "p"}],
            [{"rule": "r", "path": "p", "reason": "won't fix",
              "comment": "z" * (pw.COMMENT_LIMIT + 1)}])
        self.assertEqual([f[0] for f in findings],
                         [f"pin-watch: code-scanning dismissal r in p has a comment over {pw.COMMENT_LIMIT} characters"])
        self.assertEqual(calls, [])


PSEUDO = "v0.30.3-0.20261105213751-6d5a251fbe01"


class SyncTests(unittest.TestCase):
    def gomod(self, text, name="go.mod"):
        d = tempfile.mkdtemp()
        self.addCleanup(lambda: (os.unlink(os.path.join(d, name)), os.rmdir(d)))
        path = os.path.join(d, name)
        with open(path, "w") as f:
            f.write(text)
        return path

    def test_every_ignore_needs_a_watch_and_every_named_ignore_must_live(self):
        # No go.mod-held watch: go.work is never opened (none is given).
        out = pw.sync_findings([{"ignores": "a"}, {"ignores": "gone"}], {"a", "b"})
        self.assertEqual(len(out), 2)
        self.assertIn("ignores `b`", out[0])
        self.assertIn("watches `gone`", out[1])

    def test_a_watch_without_ignores_is_a_pin_every_go_mod_holds_at_its_min_version(self):
        w = {"id": "x", "unblock": {"type": "upstream-gomod-requires", "repo": "o/r",
                                    "module": "example.com/m", "min_version": PSEUDO}}
        held = self.gomod(f"module a\n\nrequire (\n\texample.com/m {PSEUDO} // indirect\n)\n")
        without = self.gomod("module b\n\nrequire example.com/other v1.0.0\n")
        self.assertEqual(pw.sync_findings([w], set(), [held, without]), [])
        drifted = self.gomod("module c\n\nrequire example.com/m v0.30.4-0.20261201000000-abcdefabcdef\n")
        out = pw.sync_findings([w], set(), [held, drifted])
        self.assertEqual(len(out), 1)
        self.assertIn("v0.30.4-0.20261201000000-abcdefabcdef, past its min_version", out[0])
        below = self.gomod("module d\n\nrequire example.com/m v0.30.2\n")
        self.assertIn("below its min_version", pw.sync_findings([w], set(), [below])[0])
        self.assertIn(os.path.relpath(drifted), out[0])
        self.assertIn("no workspace go.mod requires", pw.sync_findings([w], set(), [without])[0])

    def test_a_malformed_watch_is_named_not_evaluated(self):
        for bad, text in (
            ({"id": "x", "unblock": {"type": "upstream-gomod-requires", "modul": "typo",
                                     "repo": "o/r", "min_version": "v1"}}, "`module`"),
            ({"id": "x", "unblock": {"type": "upstream-gomod-requires", "module": "m",
                                     "repos": "o/r", "min_version": "v1"}}, "`repo`"),
            ({"id": "x", "unblock": {"type": "upstream-gomod-requires", "module": "m",
                                     "repo": "o/r", "min_version": 0.31}}, "`min_version`"),
            ({"id": "x", "unblock": {"type": "elsewhere"}}, "condition type"),
            ({"id": "x"}, "no `unblock`"),
        ):
            self.assertIn(text, pw.watch_shape(bad), bad)
            findings = []
            pw.evaluate(bad, findings)   # fetches nothing: no http_get is reachable from here
            self.assertEqual([f[0] for f in findings], ["pin-watch: x is malformed"], bad)
            out = pw.sync_findings([bad], set(), [])
            self.assertEqual(len(out), 1)
            self.assertIn("malformed", out[0])

    def test_a_go_mod_past_the_pin_says_delete_the_watch_not_re_pin(self):
        w = {"id": "x", "unblock": {"type": "upstream-gomod-requires", "repo": "o/r",
                                    "module": "example.com/m", "min_version": PSEUDO}}
        past = self.gomod("module a\n\nrequire example.com/m v0.30.3 // indirect\n")
        out = pw.sync_findings([w], set(), [past, os.path.join(os.path.dirname(past), "missing", "go.mod")])
        self.assertEqual(len(out), 1)
        self.assertIn("past its min_version", out[0])
        self.assertIn("delete the watch", out[0])
        self.assertNotIn("re-pin", out[0])

    def test_the_workspace_go_mods_come_from_go_work(self):
        d = tempfile.mkdtemp()
        self.addCleanup(lambda: (os.unlink(os.path.join(d, "go.work")), os.rmdir(d)))
        with open(os.path.join(d, "go.work"), "w") as f:
            f.write("go 1.27.1\n\nuse (\n\t.\n\t./box // caddy-bearing\n\tmodules/foo\n)\n\nuse\t\"./my dir\"\n")
        self.assertEqual(pw.workspace_gomods(os.path.join(d, "go.work")),
                         [os.path.join(d, p) for p in ("go.mod", "box/go.mod", "modules/foo/go.mod", "my dir/go.mod")])

    def test_only_a_require_is_a_requirement(self):
        gomod = ("module a\n\nexclude (\n\texample.com/m v0.9.0\n)\n\n"
                 "replace (\n\texample.com/m v1.0.0 => ../m\n)\n\n"
                 "retract v1.1.0\n\nrequire (\n\texample.com/other v2.0.0\n\texample.com/m v1.2.0 // indirect\n)\n")
        self.assertEqual(pw.requirement_in(gomod, "example.com/m"), "v1.2.0")
        self.assertEqual(pw.requirement_in("require example.com/m v1.3.0\n", "example.com/m"), "v1.3.0")
        self.assertIsNone(pw.requirement_in("replace example.com/m v1.0.0 => ../m\n", "example.com/m"))
        self.assertIsNone(pw.requirement_in("exclude example.com/m v1.0.0\n", "example.com/m"))
        self.assertIsNone(pw.requirement_in("require example.com/m/v2 v2.0.0\n", "example.com/m"))
        self.assertEqual(pw.requirement_in("require\texample.com/m\tv1.4.0\n", "example.com/m"), "v1.4.0")


class WatchTests(unittest.TestCase):
    def test_an_entry_that_is_not_a_mapping_is_a_finding_not_a_crash(self):
        findings = []
        self.assertEqual(pw.mappings([None, "x", {"id": "ok"}], findings), [{"id": "ok"}])
        self.assertEqual([f[0] for f in findings],
                         ["pin-watch: watch #1 is not a mapping", "pin-watch: watch #2 is not a mapping"])
        findings = []
        self.assertEqual(pw.mappings([{"ignores": ["github.com/foo"]}], findings), [])
        self.assertEqual([f[0] for f in findings], ["pin-watch: watch #1 has an `ignores` that is not text"])

    def test_a_repo_that_is_not_found_is_a_finding_not_a_transient_warning(self):
        import urllib.error
        w = {"id": "x", "unblock": {"type": "upstream-gomod-requires", "repo": "o/typo",
                                    "module": "example.com/m", "min_version": "v1.0.0"}}

        def gone(url):
            raise urllib.error.HTTPError(url, 404, "Not Found", {}, io.BytesIO())

        def flaky(url):
            raise urllib.error.HTTPError(url, 503, "Unavailable", {}, io.BytesIO())

        saved, pw.http_get = pw.http_get, gone
        try:
            findings = []
            with contextlib.redirect_stderr(io.StringIO()) as err:
                pw.evaluate(w, findings)
            self.assertEqual([f[0] for f in findings], ["pin-watch: x — upstream not found"])
            self.assertNotIn("could not evaluate", err.getvalue())
            pw.http_get = flaky
            findings = []
            with contextlib.redirect_stderr(io.StringIO()) as err:
                pw.evaluate(w, findings)
            self.assertEqual(findings, [])
            self.assertIn("could not evaluate", err.getvalue())
        finally:
            pw.http_get = saved


class VersionTests(unittest.TestCase):
    def test_a_pseudo_version_is_below_its_release_and_ordered_by_time(self):
        k = pw.ver_key
        self.assertLess(k(PSEUDO), k("v0.30.3"))
        self.assertLess(k("v0.30.2"), k(PSEUDO))
        self.assertLess(k("v0.30.3-0.20261001000000-0000000a0000"), k(PSEUDO))
        self.assertLess(k(PSEUDO), k("v0.30.3-0.20261201000000-0000000b0000"))
        self.assertGreaterEqual(k(PSEUDO), k(PSEUDO))
        self.assertLess(k("v0.30.3"), k("v0.31.0"))
        self.assertLess(k("v2.11.9"), k("v2.12.0+incompatible"))
        self.assertLess(k("v1.0.0-beta.9"), k("v1.0.0-beta.10"))
        self.assertLess(k("v1.0.0-alpha"), k("v1.0.0-beta"))
        self.assertLess(k("v1.0.0-rc.1"), k("v1.0.0"))


if __name__ == "__main__":
    unittest.main()
