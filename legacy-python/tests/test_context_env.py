import pathlib
import tempfile
import types
import unittest

from gmp.config import Adapter, Case
from gmp.context import Context


class SourceEnvTests(unittest.TestCase):
    def test_source_commands_get_only_llm_settings(self):
        raw = {"case": "t-case", "workload": "widgetrag-a", "source": {"ns": "default", "infra": "s"},
               "target": {"provider": "beetle", "csp": "gcp", "region": "r", "name": "t"},
               "run": {"env": {"LLM_TEMPERATURE": "0", "LLM_SEED": "42", "COMPARE": "1", "CODE_SOURCE": "package",
                               "VERIFY_ADMIN_PASSWORD": "pw"}}}
        case = Case(path=pathlib.Path("x.yaml"), raw=raw)
        with tempfile.TemporaryDirectory() as d:
            ctx = Context(case, Adapter.load("widgetrag-a"), None, types.SimpleNamespace(run_dir=d), None)
            self.assertEqual(ctx.remote("rebuild"),
                             "cd ~/WidgetRAG/scripts/local && LLM_TEMPERATURE=0 LLM_SEED=42 ./40-start-apps.sh --rebuild")
            entry = ctx.remote("entry")
            for want in ("FORM=t-case-target", "CODE_SOURCE=package", "COMPARE=1"):
                self.assertIn(want, entry)
            ctx.adapter = Adapter.load("widgetrag-b")
            restart = ctx.remote("restart")
            self.assertNotIn("COMPARE", restart)
            self.assertIn("LLM_SEED=42", restart)


if __name__ == "__main__":
    unittest.main()
