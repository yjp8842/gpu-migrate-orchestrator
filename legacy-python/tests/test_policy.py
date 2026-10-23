import json
import pathlib
import unittest

from gmp.policy import adjust_candidate, select_candidate

FIX = pathlib.Path(__file__).parent / "fixtures"


class PolicyTests(unittest.TestCase):
    def setUp(self):
        self.cands = json.loads((FIX / "beetle-recommend.json").read_text(encoding="utf-8"))["data"]
        self.src = {"vendor": "NVIDIA", "model": "Tesla T4", "memoryTotalGB": 16}

    def test_same_model_first_picks_t4(self):
        idx, reason = select_candidate(self.cands, self.src, "same-model-first")
        self.assertEqual(idx, 1)
        self.assertIn("같은 GPU 모델", reason)

    def test_vram_at_least_picks_closest_vram(self):
        idx, reason = select_candidate(self.cands, self.src, "vram-at-least")
        self.assertEqual(idx, 1)      # T4 16GB 가 L4 24GB 보다 가깝다

    def test_amd_excluded_for_nvidia_source(self):
        only_amd = [self.cands[2]]
        idx, reason = select_candidate(only_amd, self.src, "same-model-first")
        self.assertIsNone(idx)

    def test_fallback_when_no_same_model(self):
        idx, reason = select_candidate([self.cands[0]], self.src, "same-model-first")
        self.assertEqual(idx, 0)
        self.assertIn("WARN", reason)

    def test_adjust_disk_image_and_ports(self):
        adj, changes = adjust_candidate(self.cands[1], {"root_disk_gb": 100}, image_override="gcp+asia-northeast3+ubuntu24.04",
                                        open_ports=[{"port": 80, "cidr": "1.2.3.4/32"}])
        ng = adj["targetInfra"]["nodeGroups"][0]
        self.assertEqual(ng["rootDiskSize"], 100)
        self.assertEqual(ng["imageId"], "gcp+asia-northeast3+ubuntu24.04")
        self.assertEqual(self.cands[1]["targetInfra"]["nodeGroups"][0]["rootDiskSize"], 50)   # 원본은 그대로
        rules = adj["targetSecurityGroupList"][0]["firewallRules"]
        self.assertIn({"Ports": "80", "Protocol": "tcp", "Direction": "inbound", "CIDR": "1.2.3.4/32"}, rules)
        self.assertEqual(len(changes), 3)


class LiveRecommendTests(unittest.TestCase):
    """2026-10-22 Beetle(v0.6.2 계열) 실응답 — GCP 서울에 T4 가 없어 L4 로 간다."""

    def setUp(self):
        d = json.loads((FIX / "beetle-recommend-live-gcp.json").read_text(encoding="utf-8"))
        self.cands = d["data"]
        self.src = d["request"]["onpremiseInfraModel"]["nodes"][0]["gpuCards"][0]

    def test_falls_back_to_l4_with_warn(self):
        idx, reason = select_candidate(self.cands, self.src, "same-model-first")
        self.assertEqual(idx, 0)
        self.assertIn("L4", reason)
        self.assertIn("WARN", reason)

    def test_gpu_image_with_same_driver_is_kept(self):
        from gmp.policy import image_fits
        img = self.cands[0]["targetOsImageList"][0]
        ok, why = image_fits(img, "ubuntu-24.04", "595")
        self.assertTrue(ok, why)
        ok, why = image_fits(img, "ubuntu-24.04", "580")
        self.assertFalse(ok)
        ok, why = image_fits(img, "ubuntu-22.04", "595")
        self.assertFalse(ok)

    def test_sg_rules_already_from_firewall_table(self):
        adj, changes = adjust_candidate(self.cands[0], {"root_disk_gb": 100}, open_ports=[{"port": 80, "cidr": "203.0.113.254/32"}])
        self.assertEqual(changes, [])     # 디스크 104 ≥ 100, 80 규칙은 Beetle 이 firewallTable 에서 이미 옮겼다
