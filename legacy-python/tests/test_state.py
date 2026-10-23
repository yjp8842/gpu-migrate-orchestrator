import pathlib
import tempfile
import unittest

from gmp.state import RunState, StateError


class StateTests(unittest.TestCase):
    def test_create_mark_resume(self):
        with tempfile.TemporaryDirectory() as d:
            st = RunState.create(d, "case-x", "cases/x.yaml", "widgetrag-shell")
            st.set_key("target_infra_id", "wr-aws-tgt-01")
            st.mark("collect", "running")
            st.mark("collect", "done", result={"gpu": "T4"})
            st.mark("recommend", "failed", error="boom")
            st.note("VPC 한도 상향 요청", stage="provision")
            rid = st.data["run_id"]
            again = RunState.open(d, "case-x", rid)
            self.assertTrue(again.is_done("collect"))
            self.assertEqual(again.stage("recommend")["status"], "failed")
            self.assertEqual(again.get_key("target_infra_id"), "wr-aws-tgt-01")
            self.assertEqual(len(again.data["interventions"]), 1)
            self.assertEqual(RunState.latest_run_id(d, "case-x"), rid)
            self.assertIsNone(RunState.latest_run_id(d, "nope"))

    def test_lock_rejects_second(self):
        with tempfile.TemporaryDirectory() as d:
            st = RunState.create(d, "c", "x", "w")
            st.lock()
            other = RunState.open(d, "c", st.data["run_id"])
            with self.assertRaises(StateError):
                other.lock()
            st.unlock()
