#!/usr/bin/env python3
"""The merge rule of usage.py, run by `make usage-check`: stdlib unittest."""
import contextlib
import io
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import usage  # noqa: E402

KEYS, COLS = ["month", "model"], ["calls", "output"]


def rows(*quads):
    return [{"month": m, "model": k, "calls": str(c), "output": str(o)} for m, k, c, o in quads]


class MergeTests(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)

    def merge(self, old, fresh, recompute=False):
        path = os.path.join(self.dir.name, "a.csv")
        usage.write_csv(path, KEYS + COLS, old)
        with contextlib.redirect_stderr(io.StringIO()) as err:
            usage.merge([(path, KEYS, COLS, fresh)], recompute)
        return usage.read_csv(path), err.getvalue()

    def test_growth_replaces_the_month(self):
        got, err = self.merge(rows(("2026-09", "m", 1, 10)), rows(("2026-09", "m", 2, 20)))
        self.assertEqual(got, rows(("2026-09", "m", 2, 20)))
        self.assertEqual(err, "")

    def test_one_count_shrinking_keeps_the_month(self):
        got, err = self.merge(rows(("2026-09", "m", 1, 10)), rows(("2026-09", "m", 5, 9)))
        self.assertEqual(got, rows(("2026-09", "m", 1, 10)))
        self.assertIn("m shrank or vanished", err)

    def test_vanished_row_keeps_the_month(self):
        old = rows(("2026-09", "a", 1, 1), ("2026-09", "b", 1, 1))
        got, err = self.merge(old, rows(("2026-09", "a", 9, 9)))
        self.assertEqual(got, old)
        self.assertIn("b shrank or vanished", err)

    def test_recompute_accepts_a_shrink(self):
        got, err = self.merge(rows(("2026-09", "m", 5, 5)), rows(("2026-09", "m", 1, 1)), recompute=True)
        self.assertEqual(got, rows(("2026-09", "m", 1, 1)))
        self.assertEqual(err, "")

    def test_absent_month_stays_even_with_recompute(self):
        old = rows(("2026-08", "m", 1, 1), ("2026-09", "m", 1, 1))
        got, err = self.merge(old, rows(("2026-09", "m", 2, 2)), recompute=True)
        self.assertEqual(got, rows(("2026-08", "m", 1, 1), ("2026-09", "m", 2, 2)))
        self.assertIn("2026-08 is no longer in the source", err)

    def test_new_month_is_added_beside_the_old(self):
        got, err = self.merge(rows(("2026-09", "m", 1, 1)), rows(("2026-09", "m", 1, 1), ("2026-10", "m", 3, 3)))
        self.assertEqual(got, rows(("2026-09", "m", 1, 1), ("2026-10", "m", 3, 3)))
        self.assertEqual(err, "")

    def test_a_group_decides_each_month_together(self):
        """A deleted 600 s run replaced by two 60 s runs: runs grow while seconds shrink."""
        runs, jobs = os.path.join(self.dir.name, "runs.csv"), os.path.join(self.dir.name, "jobs.csv")
        rk, jk, jc = ["month", "workflow"], ["month", "workflow", "runner"], ["jobs", "seconds"]
        old_runs = [{"month": "2026-09", "workflow": "ci", "runs": "1"}]
        old_jobs = [{"month": "2026-09", "workflow": "ci", "runner": "x", "jobs": "1", "seconds": "600"}]
        new_runs = [{"month": "2026-09", "workflow": "ci", "runs": "2"}]
        new_jobs = [{"month": "2026-09", "workflow": "ci", "runner": "x", "jobs": "2", "seconds": "120"}]
        usage.write_csv(runs, rk + ["runs"], old_runs)
        usage.write_csv(jobs, jk + jc, old_jobs)
        group = [(runs, rk, ["runs"], new_runs), (jobs, jk, jc, new_jobs)]
        with contextlib.redirect_stderr(io.StringIO()) as err:
            usage.merge(group, False)
        self.assertEqual(usage.read_csv(runs), old_runs)  # grew, but kept with its jobs
        self.assertEqual(usage.read_csv(jobs), old_jobs)
        self.assertIn("runs.csv + ", err.getvalue())
        with contextlib.redirect_stderr(io.StringIO()):
            usage.merge(group, True)
        self.assertEqual(usage.read_csv(runs), new_runs)
        self.assertEqual(usage.read_csv(jobs), new_jobs)


if __name__ == "__main__":
    unittest.main()
