package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpu-migrate-orchestrator/internal/jsonx"
)

func load(t *testing.T, name string) jsonx.M {
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	v, err := jsonx.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	return jsonx.AsM(v)
}

var t4 = M{"vendor": "NVIDIA", "model": "Tesla T4", "memoryTotalGB": 16}

func TestSameModelFirstPicksT4(t *testing.T) {
	c := jsonx.AsList(load(t, "beetle-recommend.json")["data"])
	i, why := SelectCandidate(c, t4, "same-model-first")
	if i != 1 || !strings.Contains(why, "같은 GPU 모델") {
		t.Fatal(i, why)
	}
	if i, _ := SelectCandidate(c, t4, "vram-at-least"); i != 1 {
		t.Fatal("T4 16GB 가 L4 24GB 보다 가깝다", i)
	}
	if i, _ := SelectCandidate([]any{c[2]}, t4, "same-model-first"); i != -1 {
		t.Fatal("AMD 를 골랐다")
	}
	if i, why := SelectCandidate([]any{c[0]}, t4, "same-model-first"); i != 0 || !strings.Contains(why, "WARN") {
		t.Fatal(i, why)
	}
}

func TestAdjustDiskImageAndPorts(t *testing.T) {
	c := jsonx.AsList(load(t, "beetle-recommend.json")["data"])
	cand := jsonx.AsM(c[1])
	adj, changes := AdjustCandidate(cand, M{"root_disk_gb": 100}, "gcp+asia-northeast3+ubuntu24.04", []M{{"port": 80, "cidr": "1.2.3.4/32"}}, nil)
	ng := jsonx.ListM(jsonx.Get(adj, "targetInfra", "nodeGroups"))[0]
	if jsonx.Int(ng["rootDiskSize"]) != 100 || ng["imageId"] != "gcp+asia-northeast3+ubuntu24.04" {
		t.Fatal(ng)
	}
	if jsonx.Int(jsonx.ListM(jsonx.Get(cand, "targetInfra", "nodeGroups"))[0]["rootDiskSize"]) != 50 {
		t.Fatal("원본이 바뀌었다")
	}
	rules := jsonx.ListM(jsonx.ListM(adj["targetSecurityGroupList"])[0]["firewallRules"])
	found := false
	for _, r := range rules {
		if jsonx.Equal(r, M{"Ports": "80", "Protocol": "tcp", "Direction": "inbound", "CIDR": "1.2.3.4/32"}) {
			found = true
		}
	}
	if !found || len(changes) != 3 {
		t.Fatal(rules, changes)
	}
}

func TestLiveRecommend(t *testing.T) {
	d := load(t, "beetle-recommend-live-gcp.json")
	cands := jsonx.AsList(d["data"])
	src := jsonx.ListM(jsonx.Get(d, "request", "onpremiseInfraModel", "nodes"))[0]
	gpu := jsonx.ListM(src["gpuCards"])[0]
	i, why := SelectCandidate(cands, gpu, "same-model-first")
	if i != 0 || !strings.Contains(why, "L4") || !strings.Contains(why, "WARN") {
		t.Fatal(i, why)
	}
	img := jsonx.ListM(jsonx.AsM(cands[0])["targetOsImageList"])[0]
	if ok, why := ImageFits(img, "ubuntu-24.04", "595"); !ok {
		t.Fatal(why)
	}
	if ok, _ := ImageFits(img, "ubuntu-24.04", "580"); ok {
		t.Fatal("580 을 받았다")
	}
	if ok, _ := ImageFits(img, "ubuntu-22.04", "595"); ok {
		t.Fatal("22.04 를 받았다")
	}
	// 80 규칙은 Beetle 이 firewallTable 에서 이미 옮겼고 디스크 104 ≥ 100 → 보정 없음
	if _, changes := AdjustCandidate(jsonx.AsM(cands[0]), M{"root_disk_gb": 100}, "", []M{{"port": 80, "cidr": "203.0.113.254/32"}}, nil); len(changes) != 0 {
		t.Fatal(changes)
	}
	// Beetle 이 보안 그룹 규칙을 null 로 줄 때
	c := jsonx.Clone(cands[0]).(M)
	jsonx.ListM(c["targetSecurityGroupList"])[0]["firewallRules"] = nil
	adj, changes := AdjustCandidate(c, M{"root_disk_gb": 100}, "", []M{{"port": 80, "cidr": "203.0.113.254/32"}}, nil)
	if len(changes) != 1 || jsonx.S(jsonx.ListM(jsonx.ListM(adj["targetSecurityGroupList"])[0]["firewallRules"])[0], "Ports") != "80" {
		t.Fatal(changes)
	}
}

func TestAdjustCandidateNarrowsSubnetEqualToVNet(t *testing.T) {
	cand := M{"targetInfra": M{"nodeGroups": []any{}}, "targetVNet": M{"cidrBlock": "10.2.0.0/16",
		"subnetInfoList": []any{M{"name": "subnet-01", "ipv4_CIDR": "10.2.0.0/16"}, M{"name": "subnet-02", "ipv4_CIDR": "10.2.64.0/18"}}}}
	c, changes := AdjustCandidate(cand, M{}, "", nil, nil)
	sn := jsonx.ListM(jsonx.AsM(c["targetVNet"])["subnetInfoList"])
	if jsonx.S(sn[0], "ipv4_CIDR") != "10.2.0.0/18" || jsonx.S(sn[1], "ipv4_CIDR") != "10.2.64.0/18" || len(changes) != 1 {
		t.Fatal(sn, changes)
	}
	if jsonx.S(jsonx.ListM(jsonx.AsM(cand["targetVNet"])["subnetInfoList"])[0], "ipv4_CIDR") != "10.2.0.0/16" {
		t.Fatal("원본 후보를 바꿨다")
	}
}
