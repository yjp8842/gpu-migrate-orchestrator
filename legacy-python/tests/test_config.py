import os
import pathlib
import tempfile
import unittest
from unittest import mock

from gmp.config import PROJECT_ROOT, Adapter, Case, ConfigError, Settings, env_string, expand_env, load_dotenv, render

CASE_ENV = {"GMP_WEB_ALLOW_CIDR": "203.0.113.254/32", "GMP_VERIFY_ADMIN_PASSWORD": "test-pw"}
TB_ENV = {"GMP_TB_URL": "http://tb.example:1323/tumblebug/", "GMP_TB_USER": "u", "GMP_TB_PASSWORD": "p", "GMP_BEETLE_URL": ""}


class ConfigTests(unittest.TestCase):
    @mock.patch.dict(os.environ, CASE_ENV)
    def test_cases_and_adapters_load(self):
        for name in ("aws-to-aws-widgetrag-a", "aws-to-gcp-widgetrag-a"):
            case = Case.load(PROJECT_ROOT / "cases" / f"{name}.yaml")
            ad = Adapter.load(case.raw["workload"])
            self.assertEqual(case.name, name)
            self.assertIn(ad["form"], ("shell", "compose"))
            self.assertEqual(case.policy["on_perf_fail"], "stop")
            self.assertEqual(case.policy["open_ports"][0]["cidr"], "203.0.113.254/32")

    @mock.patch.dict(os.environ, {"GMP_WEB_ALLOW_CIDR": "", "GMP_VERIFY_ADMIN_PASSWORD": ""})
    def test_case_missing_env_named(self):
        with self.assertRaises(ConfigError) as cm:
            Case.load(PROJECT_ROOT / "cases" / "aws-to-aws-widgetrag-a.yaml")
        self.assertIn("GMP_VERIFY_ADMIN_PASSWORD, GMP_WEB_ALLOW_CIDR", str(cm.exception))

    def test_expand_env(self):
        with mock.patch.dict(os.environ, {"GMP_T_A": "x", "GMP_T_EMPTY": ""}):
            out = expand_env({"a": "${GMP_T_A}/y", "b": ["${GMP_T_EMPTY:-def}", 3], "c": {"d": "$HOME ${GMP_T_A}"}}, "x.yaml")
            self.assertEqual(out, {"a": "x/y", "b": ["def", 3], "c": {"d": "$HOME x"}})
            with self.assertRaises(ConfigError) as cm:
                expand_env({"a": "${GMP_T_NONE_2}", "b": "${GMP_T_NONE_1}"}, "x.yaml")
            self.assertIn("GMP_T_NONE_1, GMP_T_NONE_2", str(cm.exception))

    def test_load_dotenv(self):
        with tempfile.TemporaryDirectory() as d, mock.patch.dict(os.environ, {"GMP_T_SET": "shell"}):
            p = pathlib.Path(d) / ".env"
            p.write_text("# 주석\nGMP_T_NEW=\"a b\"\nexport GMP_T_EXP='c'\nGMP_T_SET=file\n", encoding="utf-8")
            os.environ.pop("GMP_T_NEW", None)
            os.environ.pop("GMP_T_EXP", None)
            load_dotenv(p)
            self.assertEqual((os.environ["GMP_T_NEW"], os.environ["GMP_T_EXP"], os.environ["GMP_T_SET"]), ("a b", "c", "shell"))
            p.write_text("not a line\n", encoding="utf-8")
            with self.assertRaises(ConfigError):
                load_dotenv(p)

    def test_tumblebug_provider_requires_spec(self):
        raw = {"case": "x", "workload": "widgetrag-a", "source": {"ns": "default", "infra": "s"},
               "target": {"provider": "tumblebug", "csp": "aws", "region": "r", "name": "t"}}
        with self.assertRaises(ConfigError):
            Case(path=pathlib.Path("x.yaml"), raw=raw)

    def test_next_candidate_not_implemented(self):
        raw = {"case": "x", "workload": "widgetrag-a", "source": {"ns": "default", "infra": "s"},
               "target": {"provider": "beetle", "csp": "gcp", "region": "r", "name": "t"}, "policy": {"on_perf_fail": "next-candidate"}}
        with self.assertRaises(ConfigError):
            Case(path=pathlib.Path("x.yaml"), raw=raw)

    def test_placeholder_adapter_message(self):
        with self.assertRaises(ConfigError) as cm:
            Adapter.load("lexai-a")
        self.assertIn("자리만", str(cm.exception))

    def test_env_string_and_render(self):
        self.assertEqual(env_string({"A": "1", "B": "x y"}), "A=1 B='x y'")
        self.assertEqual(render("{a}-{b}", a="x", b="y"), "x-y")
        with self.assertRaises(ConfigError):
            render("{missing}")

    @mock.patch.dict(os.environ, TB_ENV)
    def test_settings_example(self):
        s = Settings.load(PROJECT_ROOT / "settings.example.yaml")
        self.assertIn("default", s.tumblebugs)
        self.assertEqual(s.tumblebugs["default"].url, "http://tb.example:1323/tumblebug")
        self.assertIsNone(s.beetle)
        self.assertGreaterEqual(s.timeouts["start_min"], 60)
