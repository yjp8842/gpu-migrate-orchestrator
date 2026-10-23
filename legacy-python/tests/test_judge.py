import pathlib
import unittest

from gmp import judge

FIX = pathlib.Path(__file__).parent / "fixtures"


class JudgeTests(unittest.TestCase):
    def setUp(self):
        self.results = (FIX / "results.csv").read_text(encoding="utf-8")
        self.compare = (FIX / "migration-compare.csv").read_text(encoding="utf-8")

    def test_new_row_pass_with_warn(self):
        v = judge.evaluate(self.results, baseline_rows=1, compare_text=self.compare, require_compare=True)
        self.assertEqual(v["result"], "PASS")
        self.assertEqual(v["timestamp"], "2026-10-22 02:10:31")
        self.assertEqual(v["compare"]["by_result"]["PASS"], 4)
        self.assertEqual(len(v["warn"]), 1)
        self.assertIn("runtime:gpu", v["warn"][0])

    def test_no_new_row(self):
        v = judge.evaluate(self.results, baseline_rows=2, compare_text=self.compare)
        self.assertEqual(v["result"], "NO-RESULT")

    def test_compare_fail_flips_verdict(self):
        bad = self.compare + "2026-10-22 02:10:31,f,shell-native,p,T1-3,db:company,3,2,FAIL,값 다름\n"
        v = judge.evaluate(self.results, baseline_rows=1, compare_text=bad)
        self.assertEqual(v["result"], "FAIL")
        self.assertEqual(len(v["compare"]["fail_items"]), 1)

    def test_require_compare_without_rows(self):
        v = judge.evaluate(self.results, baseline_rows=1, compare_text="", require_compare=True)
        self.assertEqual(v["result"], "FAIL")
