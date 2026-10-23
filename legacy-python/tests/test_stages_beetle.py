"""Beetle 경로 단계 — dry-run 의 추천 조회, 결함 회귀(정의되지 않은 변수), 보안 그룹 규칙 중복 방지."""
import copy
import json
import pathlib
import tempfile
import types
import unittest
from unittest import mock

import requests

from gmp.clients.beetle import Beetle, BeetleError
from gmp.config import Endpoint
from gmp.log import Log
from gmp.stages import provision, recommend
from gmp.stages._util import StageError

FIX = pathlib.Path(__file__).parent / "fixtures"
LIVE = json.loads((FIX / "beetle-recommend-live-gcp.json").read_text(encoding="utf-8"))
OPEN_80 = [{"port": 80, "cidr": "203.0.113.254/32"}]


class FakeResponse:
    def __init__(self, status, body):
        self.status_code = status
        self._body = body
        self.text = json.dumps(body)
        self.content = self.text.encode()
        self.url = "http://beetle/x"

    def json(self):
        return self._body


class BeetleDryRunTests(unittest.TestCase):
    def client(self, responses):
        s = mock.MagicMock(spec=requests.Session)
        s.headers = {}
        s.request.side_effect = responses
        return Beetle(Endpoint("b", "http://beetle/beetle"), Log(None, verbose=False), dry_run=True, session=s, min_interval=0), s

    def test_recommend_is_read_only_and_allowed(self):
        b, s = self.client([FakeResponse(200, {"success": True, "data": LIVE["data"]})])
        self.assertEqual(len(b.recommend_infra(LIVE["request"])), len(LIVE["data"]))
        self.assertEqual(s.request.call_count, 1)

    def test_writes_and_key_issue_blocked(self):
        b, s = self.client([])
        with self.assertRaises(BeetleError):
            b.migrate_infra("default", {}, name_seed="x")
        with self.assertRaises(BeetleError):
            b.encryption_key()                 # GET 이지만 일회용 키를 발급한다
        with self.assertRaises(BeetleError):
            b.data_migration({}, "rid")
        self.assertEqual(s.request.call_count, 0)


class MissingRulesTests(unittest.TestCase):
    def test_beetle_sg_already_has_rule(self):
        existing = [{"Port": "22", "Protocol": "TCP", "Direction": "inbound", "CIDR": "0.0.0.0/0"},
                    {"Port": "80", "Protocol": "TCP", "Direction": "inbound", "CIDR": "203.0.113.254/32"},
                    {"Port": "", "Protocol": "ALL", "Direction": "outbound", "CIDR": "0.0.0.0/0"}]
        self.assertEqual(provision.missing_rules(existing, OPEN_80), [])

    def test_default_sg_gets_rule(self):
        existing = [{"Port": "22", "Protocol": "TCP", "Direction": "inbound", "CIDR": "0.0.0.0/0"}]
        self.assertEqual(provision.missing_rules(existing, OPEN_80),
                         [{"Ports": "80", "Protocol": "tcp", "Direction": "inbound", "CIDR": "203.0.113.254/32"}])

    def test_range_and_duplicates(self):
        existing = [{"Port": "80-80", "Protocol": "tcp", "Direction": "inbound", "CIDR": "203.0.113.254/32"}]
        self.assertEqual(provision.missing_rules(existing, OPEN_80), [])
        self.assertEqual(len(provision.missing_rules([], OPEN_80 + OPEN_80)), 1)
        # 다른 CIDR 은 다른 규칙
        self.assertEqual(len(provision.missing_rules(existing, [{"port": 80, "cidr": "10.0.0.0/8"}])), 1)


class AdjustNullRulesTests(unittest.TestCase):
    def test_null_firewall_rules_from_beetle(self):
        from gmp.policy import adjust_candidate
        c = copy.deepcopy(LIVE["data"][0])
        c["targetSecurityGroupList"][0]["firewallRules"] = None     # 소스 방화벽 표가 비면 Beetle 이 null 로 준다
        adj, changes = adjust_candidate(c, {"root_disk_gb": 100}, open_ports=OPEN_80)
        self.assertEqual(adj["targetSecurityGroupList"][0]["firewallRules"][0]["Ports"], "80")
        self.assertEqual(len(changes), 1)


class PlaceholderModelTests(unittest.TestCase):
    def test_placeholder_has_full_os_and_firewall(self):
        from gmp.stages.collect import placeholder_model
        m = placeholder_model({"driver_series": "595", "open_ports": OPEN_80})
        n = m["nodes"][0]
        self.assertEqual((n["os"]["id"], n["os"]["versionId"]), ("ubuntu", "24.04"))
        self.assertEqual([r["dstPorts"] for r in n["firewallTable"]], ["22", "80"])
        self.assertEqual(n["gpuCards"][0]["driverIndex"], "0")


class FakeState:
    def __init__(self):
        self.keys = {}
        self.data = {"run_id": "20261022-000000"}

    def get_key(self, k, default=None):
        return self.keys.get(k, default)

    def set_key(self, k, v):
        self.keys[k] = v


class FakeCtx:
    def __init__(self, beetle=None, tb=None, dry_run=False, source_model=None):
        self.dry_run = dry_run
        self.beetle = beetle
        self.tb_target = tb
        self.case = types.SimpleNamespace(
            name="t-case",
            target={"provider": "beetle", "csp": "gcp", "region": "asia-northeast3", "ns": "default", "name": "wr-gcp", "image": None},
            policy={"gpu": "same-model-first", "image": "ubuntu-24.04", "driver_series": "595", "root_disk_gb": 100, "open_ports": OPEN_80})
        self.state = FakeState()
        self.dir = pathlib.Path(tempfile.mkdtemp())
        self.model = source_model or LIVE["request"]["onpremiseInfraModel"]
        self.log = types.SimpleNamespace(info=lambda *a, **k: None, warn=lambda *a, **k: None)
        self.dry_calls = []

    def read_json(self, stage, name):
        if (stage, name) == ("collect", "source-model.json"):
            return self.model
        return json.loads((self.dir / name).read_text(encoding="utf-8"))

    def write_json(self, stage, name, obj):
        (self.dir / name).write_text(json.dumps(obj, ensure_ascii=False), encoding="utf-8")

    def write_text(self, stage, name, text):
        (self.dir / name).write_text(text, encoding="utf-8")

    def dry(self, stage, what, payload):
        self.dry_calls.append(what)


class RecommendStageTests(unittest.TestCase):
    def test_dry_run_calls_beetle_and_writes_real_request(self):
        beetle = mock.MagicMock()
        beetle.recommend_infra.return_value = copy.deepcopy(LIVE["data"])
        ctx = FakeCtx(beetle=beetle, dry_run=True)
        recommend.run(ctx)
        beetle.recommend_infra.assert_called_once()
        req = ctx.read_json("recommend", "migrate-request.json")
        self.assertNotIn("_dry_run", req)
        self.assertEqual(req["targetInfra"]["nodeGroups"][0]["specId"], "gcp+asia-northeast3+g2-standard-4")

    def test_placeholder_source_is_flagged(self):
        beetle = mock.MagicMock()
        beetle.recommend_infra.return_value = copy.deepcopy(LIVE["data"])
        ctx = FakeCtx(beetle=beetle, dry_run=True)
        ctx.state.keys["source_placeholder"] = True
        recommend.run(ctx)
        self.assertIn("자리표시자", (ctx.dir / "rationale.md").read_text(encoding="utf-8"))

    def test_no_candidates_is_stage_error(self):
        beetle = mock.MagicMock()
        beetle.recommend_infra.return_value = []
        with self.assertRaises(StageError) as cm:          # 예전에는 정의되지 않은 변수로 NameError
            recommend.run(FakeCtx(beetle=beetle))
        self.assertIn("gcp/asia-northeast3", str(cm.exception))


class ProvisionBeetleTests(unittest.TestCase):
    def test_missing_infra_id_is_stage_error(self):
        beetle = mock.MagicMock()
        beetle.migrate_infra.return_value = {"status": "Creating"}
        tb = mock.MagicMock()
        tb.get_infra.return_value = None
        ctx = FakeCtx(beetle=beetle, tb=tb)
        ctx.write_json("recommend", "migrate-request.json", {"targetInfra": {}})
        with self.assertRaises(StageError) as cm:          # 예전에는 정의되지 않은 변수로 NameError
            provision.run(ctx)
        self.assertIn("Creating", str(cm.exception))
        self.assertTrue(ctx.state.keys["beetle_migrate_started"])

    def test_open_ports_skips_rules_beetle_already_made(self):
        tb = mock.MagicMock()
        tb.get_security_group.return_value = {"firewallRules": [
            {"Port": "80", "Protocol": "TCP", "Direction": "inbound", "CIDR": "203.0.113.254/32"}]}
        ctx = FakeCtx(tb=tb)
        provision._open_ports(ctx, tb, "default", [{"securityGroupIds": ["sg-01"]}])
        tb.add_sg_rules.assert_not_called()
        self.assertEqual(ctx.state.keys["sg_rules_added"]["rules"], [])

    def test_open_ports_adds_to_default_sg(self):
        tb = mock.MagicMock()
        tb.get_security_group.return_value = {"firewallRules": [
            {"Port": "22", "Protocol": "TCP", "Direction": "inbound", "CIDR": "0.0.0.0/0"}]}
        ctx = FakeCtx(tb=tb)
        provision._open_ports(ctx, tb, "default", [{"securityGroupIds": ["sg-01"]}])
        tb.add_sg_rules.assert_called_once()
        self.assertEqual(tb.add_sg_rules.call_args[0][2][0]["Ports"], "80")


if __name__ == "__main__":
    unittest.main()
