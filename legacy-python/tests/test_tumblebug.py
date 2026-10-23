import json
import pathlib
import unittest
from unittest import mock

import requests

from gmp.clients.tumblebug import TBError, Tumblebug
from gmp.config import Endpoint
from gmp.log import Log, mask

FIX = pathlib.Path(__file__).parent / "fixtures"


class FakeResponse:
    def __init__(self, status, body=None, text=None):
        self.status_code = status
        self._body = body
        self.text = text if text is not None else (json.dumps(body) if body is not None else "")
        self.content = self.text.encode()
        self.url = "http://tb/x"

    def json(self):
        if self._body is None:
            raise ValueError
        return self._body


def client(responses):
    s = mock.MagicMock(spec=requests.Session)
    s.headers = {}
    s.request.side_effect = responses
    tb = Tumblebug(Endpoint("default", "http://tb/tumblebug"), Log(None, verbose=False),
                   {"infra_get_interval_sec": 0, "default_interval_sec": 0}, session=s)
    return tb, s


class TumblebugClientTests(unittest.TestCase):
    def test_access_info_parses_mixed_case(self):
        tb, _ = client([FakeResponse(200, json.loads((FIX / "tb-accessinfo.json").read_text()))])
        nodes = tb.access_info("default", "widgetrag-shell-aws")
        self.assertEqual(nodes[0]["publicIP"], "198.51.100.67")
        self.assertEqual(nodes[0]["nodeUserName"], "cb-user")
        self.assertIn("PRIVATE KEY", nodes[0]["privateKey"])

    def test_nodes_and_state(self):
        infra = json.loads((FIX / "tb-infra.json").read_text())
        self.assertEqual(Tumblebug.infra_state(infra), "Running")
        n = Tumblebug.nodes(infra)[0]
        self.assertEqual(n["securityGroupIds"], ["widgetrag-shell-aws-g1"])
        self.assertEqual(n["cspResourceId"], "i-0123456789abcdef0")

    def test_429_backoff_then_success(self):
        tb, s = client([FakeResponse(429, text="rate limit exceeded"), FakeResponse(429, text="rate limit exceeded"),
                        FakeResponse(200, {"output": ["a", "b"]})])
        with mock.patch("gmp.clients.tumblebug.time.sleep"):
            self.assertEqual(tb.list_infra("default"), ["a", "b"])
        self.assertEqual(s.request.call_count, 3)

    def test_post_not_retried_on_500(self):
        tb, s = client([FakeResponse(500, text="boom")])
        with self.assertRaises(TBError):
            tb.cmd_async("default", "x", ["true"], x_request_id="gmp-1")
        self.assertEqual(s.request.call_count, 1)

    def test_cmd_async_sets_request_id_and_caps_timeout(self):
        tb, s = client([FakeResponse(202, {"xRequestId": "gmp-1", "message": "started"})])
        rid = tb.cmd_async("default", "x", ["sleep 1"], user_name="cb-user", timeout_min=999, x_request_id="gmp-1")
        self.assertEqual(rid, "gmp-1")
        kw = s.request.call_args.kwargs
        self.assertEqual(kw["headers"]["X-Request-Id"], "gmp-1")
        self.assertEqual(kw["json"]["timeoutMinutes"], 120)
        self.assertEqual(kw["params"], {"async": "true"})

    def test_wait_command_terminal(self):
        entries = json.loads((FIX / "tb-commandstatus.json").read_text())
        tb, _ = client([FakeResponse(200, entries)])
        done = tb.wait_command("default", "x", ["g1-1"], "gmp-20261022-100000-backup-abc123", timeout_sec=10, poll=0)
        self.assertEqual(done["g1-1"]["status"], "Completed")

    def test_infra_dynamic_key_allowlist(self):
        with self.assertRaises(TBError):
            Tumblebug.validate_infra_dynamic({"name": "x", "nodeGroup": []})
        with self.assertRaises(TBError):
            Tumblebug.validate_infra_dynamic({"name": "x", "nodeGroups": [{"name": "g1", "nodeGroupSize": 1, "rootDiskSize": "100"}]})
        Tumblebug.validate_infra_dynamic({"name": "x", "policyOnPartialFailure": "rollback",
                                          "nodeGroups": [{"name": "g1", "nodeGroupSize": 1, "specId": "s", "imageId": "i", "rootDiskSize": 100}]})

    def test_dry_run_reads_but_never_writes(self):
        s = mock.MagicMock(spec=requests.Session)
        s.headers = {}
        s.request.side_effect = [FakeResponse(200, {"id": "x", "node": []})]
        tb = Tumblebug(Endpoint("d", "http://tb/tumblebug"), Log(None, verbose=False),
                       {"infra_get_interval_sec": 0, "default_interval_sec": 0}, dry_run=True, session=s)
        self.assertEqual(tb.get_infra("default", "x")["id"], "x")          # 읽기 전용 조회는 한다
        with self.assertRaises(TBError):
            tb.cmd_sync("default", "x", ["true"])                           # 원격 명령 · 생성 · 제어는 하지 않는다
        with self.assertRaises(TBError):
            tb.control("default", "x", "suspend")
        self.assertEqual(s.request.call_count, 1)

    def test_missing_resource_returns_none_without_retry(self):
        tb, s = client([FakeResponse(500, text='{"message":"infra widgetrag-src-aws does not exist"}')])
        self.assertIsNone(tb.get_infra("default", "widgetrag-src-aws"))
        self.assertEqual(s.request.call_count, 1)
        tb, s = client([FakeResponse(500, text='{"message":"vNet x does not exist"}')])
        self.assertIsNone(tb.get_vnet("default", "x"))
        self.assertEqual(s.request.call_count, 1)

    def test_server_error_still_retried(self):
        tb, s = client([FakeResponse(500, text="db busy"), FakeResponse(200, {"id": "x"})])
        with mock.patch("time.sleep"):
            self.assertEqual(tb.get_infra("default", "x")["id"], "x")
        self.assertEqual(s.request.call_count, 2)


class MaskTests(unittest.TestCase):
    def test_mask_secrets(self):
        s = 'x "privateKey":"abc" VERIFY_ADMIN_PASSWORD=pw -----BEGIN RSA PRIVATE KEY-----\nK\n-----END RSA PRIVATE KEY----- Authorization: Basic Zm9v'
        m = mask(s)
        self.assertNotIn("abc", m)
        self.assertNotIn("pw", m.split("VERIFY_ADMIN_PASSWORD=")[1][:3])
        self.assertIn("<PRIVATE KEY>", m)
        self.assertNotIn("Zm9v", m)


class ResolveTests(unittest.TestCase):
    def test_find_image_prefers_non_minimal(self):
        lst = {"imageCount": 2, "imageList": [
            {"id": "ami-min", "cspImageName": "ami-min", "isBasicImage": True, "osDistribution": "ubuntu-minimal/images/ubuntu-noble-24.04-minimal"},
            {"id": "ami-srv", "cspImageName": "ami-srv", "isBasicImage": True, "osDistribution": "ubuntu/images/ubuntu-noble-24.04-server"}]}
        tb, s = client([FakeResponse(200, lst)])
        pick = tb.find_image("aws", "ap-northeast-2", "ubuntu 24.04")
        self.assertEqual(pick["id"], "ami-srv")
        self.assertIn("/ns/system/resources/searchImage", s.request.call_args.args[1])

    def test_missing_resource_500_is_none(self):
        tb, s = client([FakeResponse(500, text='{"message":"the image aws+x does not exist"}')])
        self.assertIsNone(tb.get_image("default", "aws+x"))
        self.assertEqual(s.request.call_count, 1)


class RecommendSpecTests(unittest.TestCase):
    def test_body_types(self):
        tb, s = client([FakeResponse(200, [])])
        tb.recommend_spec([("providerName", "==", "gcp"), ("vCPU", ">=", 4)], priority="cost", limit=20)
        body = s.request.call_args.kwargs["json"]
        self.assertIsInstance(body["priority"]["policy"][0]["weight"], float)
        self.assertIsInstance(body["limit"], int)
        self.assertEqual(body["filter"]["policy"][1]["condition"][0], {"operator": ">=", "operand": "4"})
