import json
import pathlib
import tempfile
import types
import unittest

from gmp import inspect_vm, sourcemodel
from gmp.stages import collect
from gmp.stages._util import StageError

FIX = pathlib.Path(__file__).parent / "fixtures"
META = json.loads((FIX / "tb-meta-compose.json").read_text())


class PortTests(unittest.TestCase):
    def test_normalize(self):
        self.assertEqual(sourcemodel.normalize_port("22"), "22")
        self.assertEqual(sourcemodel.normalize_port("22-22"), "22")
        self.assertEqual(sourcemodel.normalize_port("8000-8080"), "8000-8080")
        self.assertEqual(sourcemodel.normalize_port(""), "*")
        self.assertEqual(sourcemodel.normalize_port("-1"), "*")
        self.assertEqual(sourcemodel.normalize_port(None), "*")

    def test_firewall_inbound_only_plus_open_ports(self):
        table = sourcemodel.firewall_from_sg(META["sg"]["firewallRules"], [{"port": 80, "cidr": "203.0.113.254/32"}])
        self.assertEqual([(r["dstPorts"], r["protocol"], r["srcCIDR"]) for r in table],
                         [("22", "tcp", "0.0.0.0/0"), ("80", "tcp", "203.0.113.254/32")])
        self.assertTrue(all(r["direction"] == "inbound" and r["action"] == "allow" for r in table))

    def test_firewall_dedup_and_all_protocol(self):
        rules = [{"Port": "22", "Protocol": "TCP", "Direction": "inbound", "CIDR": "0.0.0.0/0"},
                 {"Port": "22-22", "Protocol": "tcp", "Direction": "inbound", "CIDR": "0.0.0.0/0"},
                 {"Port": "", "Protocol": "ALL", "Direction": "inbound", "CIDR": "10.0.0.0/8"}]
        table = sourcemodel.firewall_from_sg(rules)
        self.assertEqual([(r["dstPorts"], r["protocol"]) for r in table], [("22", "tcp"), ("*", "*")])


class BuildTests(unittest.TestCase):
    def model(self, **kw):
        node = META["infra"]["node"][0]
        sub = next(s["ipv4_CIDR"] for s in META["vnet"]["subnetInfoList"] if s["id"] == node["subnetId"])
        return sourcemodel.build([(node, META["spec"], META["image"], sub, META["sg"]["firewallRules"])],
                                 [META["vnet"]["cidrBlock"]], **kw)

    def test_shape_matches_beetle_onprem_infra(self):
        m = self.model(driver_series="595")
        n = m["nodes"][0]
        self.assertEqual(m["network"]["ipv4Networks"]["cidrBlocks"], ["10.2.0.0/16"])
        self.assertEqual(n["machineId"], "i-0123456789abcdef0")
        self.assertEqual(n["cpu"], {"architecture": "x86_64", "cpus": 4, "cores": 2, "threads": 2})
        self.assertEqual(n["memory"]["totalSize"], 16)
        self.assertEqual(n["rootDisk"], {"label": "/", "type": "gp3", "totalSize": 100})
        self.assertEqual(n["interfaces"][0]["ipv4CidrBlocks"], ["10.2.70.79/18"])
        self.assertEqual(n["os"]["prettyName"], "Ubuntu 24.04")
        self.assertEqual(n["os"]["versionId"], "24.04")
        self.assertNotIn("minimal", json.dumps(n["os"]))           # 이미지 이름이 OS 버전 자리에 들어가지 않는다

    def test_gpu_cards_from_spec(self):
        g = self.model(driver_series="595")["nodes"][0]["gpuCards"]
        self.assertEqual(len(g), 1)
        self.assertEqual(g[0], {"driverIndex": "0", "vendor": "NVIDIA", "model": "NVIDIA T4", "type": "GPU",
                                "memoryTotalGB": 17, "driverVersion": "595"})
        self.assertIsInstance(g[0]["driverIndex"], str)             # Beetle GpuCardProperty.driverIndex 는 문자열

    def test_gpu_count_and_no_gpu(self):
        node = META["infra"]["node"][0]
        two = dict(META["spec"], acceleratorCount=2)
        m = sourcemodel.build([(node, two, META["image"], "", [])], [])
        self.assertEqual([c["driverIndex"] for c in m["nodes"][0]["gpuCards"]], ["0", "1"])
        none = dict(META["spec"], acceleratorCount=0, acceleratorModel="")
        self.assertEqual(sourcemodel.build([(node, none, META["image"], "", [])], [])["nodes"][0]["gpuCards"], [])


class FakeTB:
    """collect 가 부르는 읽기 전용 조회만 — POST 가 오면 테스트 실패."""
    def __init__(self, infra, state="Running"):
        self.infra, self.state, self.calls = infra, state, []

    def get_infra(self, ns, i):
        self.calls.append(("infra", i)); return self.infra

    def infra_state(self, infra):
        return self.state

    def resolve_spec(self, ns, s):
        self.calls.append(("spec", s)); return META["spec"]

    def resolve_image(self, ns, i):
        self.calls.append(("image", i)); return META["image"]

    def get_vnet(self, ns, v):
        self.calls.append(("vnet", v)); return META["vnet"]

    def get_security_group(self, ns, g):
        self.calls.append(("sg", g)); return META["sg"]

    def __getattr__(self, name):
        raise AssertionError(f"collect 가 {name} 을 불렀다 (VM 명령 · 쓰기 금지)")


class FakeCtx:
    def __init__(self, tb, dry_run=False):
        self.tb_source, self.dry_run = tb, dry_run
        self.case = types.SimpleNamespace(source={"ns": "default", "infra": "widgetrag-src-aws"},
                                          policy={"driver_series": "595", "open_ports": []})
        self.dir = pathlib.Path(tempfile.mkdtemp())
        self.keys = {}
        self.warnings = []
        self.log = types.SimpleNamespace(info=lambda *a, **k: None, warn=lambda m, *a, **k: self.warnings.append(m))
        self.state = types.SimpleNamespace(set_key=lambda k, v: self.keys.__setitem__(k, v))

    def write_json(self, stage, name, obj):
        (self.dir / name).write_text(json.dumps(obj, ensure_ascii=False))

    def read(self, name):
        return json.loads((self.dir / name).read_text())


class CollectStageTests(unittest.TestCase):
    def test_builds_model_from_meta_without_vm_commands(self):
        tb = FakeTB(META["infra"])
        ctx = FakeCtx(tb)
        out = collect.run(ctx)
        self.assertEqual(out["gpu"], "NVIDIA T4")
        model = ctx.read("source-model.json")
        self.assertEqual(model["nodes"][0]["firewallTable"][0]["dstPorts"], "22")
        self.assertEqual({c[0] for c in tb.calls}, {"infra", "spec", "image", "vnet", "sg"})
        self.assertIn("securityGroups", ctx.read("source-meta.json"))

    def test_runs_when_source_suspended(self):
        ctx = FakeCtx(FakeTB(META["infra"], state="Suspended"))
        collect.run(ctx)
        self.assertEqual(ctx.keys["source_state_at_collect"], "Suspended")
        self.assertTrue(any("Suspended" in w for w in ctx.warnings))

    def test_missing_source(self):
        with self.assertRaises(StageError):
            collect.run(FakeCtx(FakeTB(None)))
        ctx = FakeCtx(FakeTB(None), dry_run=True)
        self.assertTrue(collect.run(ctx)["placeholder"])
        self.assertEqual(ctx.read("source-model.json")["nodes"][0]["gpuCards"][0]["driverIndex"], "0")


class InspectTests(unittest.TestCase):
    def test_llm_mismatch(self):
        env = {"LLM_TEMPERATURE": "0", "LLM_SEED": "42"}
        self.assertFalse(inspect_vm.llm_mismatch({"llm_temperature": 0.0, "llm_seed": 42}, env)[0])
        self.assertTrue(inspect_vm.llm_mismatch({"llm_temperature": 0.7, "llm_seed": None}, env)[0])
        self.assertTrue(inspect_vm.llm_mismatch({}, env)[0])
        self.assertFalse(inspect_vm.llm_mismatch({}, {})[0])

    def test_identify_parses_outputs(self):
        ad = {"form": "shell", "workload": "widgetrag", "services": ["ollama"],
              "identify": {"units": ["widgetrag-ai"], "env_file": "/etc/x.env"}}
        names = [n for n, _ in inspect_vm.commands(ad)]
        self.assertEqual(names[0], "gpu")
        out = {"gpu": "0, GPU-1, Tesla T4, 595.45.04, 00000000:00:1E.0, 15360", "cuda": "CUDA Version: 13.0",
               "os": 'PRETTY_NAME="Ubuntu 24.04.3 LTS"', "units": "Id=widgetrag-ai.service\nActiveState=active\n---",
               "env_keys": "LLM_SEED=\nLLM_TEMPERATURE=", "code_ref": "7fbeb17\n", "health": '{"llm_temperature":0,"llm_seed":42}'}
        ident = inspect_vm.identify(ad, out)
        self.assertEqual(ident["gpu"][0]["model"], "Tesla T4")
        self.assertEqual(ident["gpu"][0]["cudaVersion"], "13.0")
        self.assertEqual(ident["gpu"][0]["memoryTotalGB"], 15)
        self.assertEqual(ident["units"][0]["activeState"], "active")
        self.assertEqual(ident["env_keys"], ["LLM_SEED", "LLM_TEMPERATURE"])
        self.assertEqual(ident["health"]["llm_seed"], 42)


if __name__ == "__main__":
    unittest.main()
