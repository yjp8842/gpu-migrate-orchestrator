package sourcemodel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpu-migrate-orchestrator/internal/jsonx"
)

func meta(t *testing.T) M {
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "tb-meta-compose.json"))
	if err != nil {
		t.Fatal(err)
	}
	v, _ := jsonx.Decode(b)
	return jsonx.AsM(v)
}

var open80 = []M{{"port": 80, "cidr": "203.0.113.254/32"}}

func TestNormalizePort(t *testing.T) {
	for in, want := range map[string]string{"22": "22", "22-22": "22", "8000-8080": "8000-8080", "": "*", "-1": "*"} {
		if got := NormalizePort(in); got != want {
			t.Fatalf("%q → %q", in, got)
		}
	}
}

func TestFirewallInboundOnlyPlusOpenPorts(t *testing.T) {
	m := meta(t)
	table := FirewallFromSG(jsonx.ListM(jsonx.Get(m, "sg", "firewallRules")), open80)
	if len(table) != 2 || table[0].DstPorts != "22" || table[0].Protocol != "tcp" || table[1].DstPorts != "80" || table[1].SrcCIDR != "203.0.113.254/32" {
		t.Fatal(table)
	}
	dedup := FirewallFromSG([]M{{"Port": "22", "Protocol": "TCP", "Direction": "inbound", "CIDR": "0.0.0.0/0"},
		{"Port": "22-22", "Protocol": "tcp", "Direction": "inbound", "CIDR": "0.0.0.0/0"},
		{"Port": "", "Protocol": "ALL", "Direction": "inbound", "CIDR": "10.0.0.0/8"}}, nil)
	if len(dedup) != 2 || dedup[1].DstPorts != "*" || dedup[1].Protocol != "*" {
		t.Fatal(dedup)
	}
}

func build(t *testing.T) OnpremInfra {
	m := meta(t)
	node := jsonx.ListM(jsonx.Get(m, "infra", "node"))[0]
	sub := ""
	for _, s := range jsonx.ListM(jsonx.Get(m, "vnet", "subnetInfoList")) {
		if s["id"] == node["subnetId"] {
			sub = jsonx.S(s, "ipv4_CIDR")
		}
	}
	return Build([]Row{{Node: node, Spec: jsonx.AsM(m["spec"]), Image: jsonx.AsM(m["image"]), SubnetCIDR: sub,
		Rules: jsonx.ListM(jsonx.Get(m, "sg", "firewallRules"))}}, []string{jsonx.S(jsonx.AsM(m["vnet"]), "cidrBlock")}, "595", nil)
}

func TestShapeMatchesBeetle(t *testing.T) {
	m := build(t)
	n := m.Nodes[0]
	if m.Network.IPv4Networks.CidrBlocks[0] != "10.2.0.0/16" || n.MachineID != "i-0123456789abcdef0" {
		t.Fatal(m)
	}
	if n.CPU != (CPU{"x86_64", 4, 2, 2}) || n.Memory.TotalSize != 16 || n.RootDisk != (Disk{"/", "gp3", 100}) {
		t.Fatal(n)
	}
	if n.Interfaces[0].IPv4CidrBlocks[0] != "10.2.70.79/18" || n.OS.PrettyName != "Ubuntu 24.04" || n.OS.VersionID != "24.04" {
		t.Fatal(n)
	}
	b, _ := json.Marshal(m)
	if strings.Contains(string(b), "minimal") || strings.Contains(string(b), "null") {
		t.Fatalf("이미지 이름이 OS 자리에 들어갔거나 빈 배열이 null 로 나간다: %s", b)
	}
	g := n.GpuCards[0]
	if len(n.GpuCards) != 1 || g != (GPU{"0", "NVIDIA", "NVIDIA T4", "GPU", 17, "595"}) {
		t.Fatal(g)
	}
	if !strings.Contains(string(b), `"driverIndex":"0"`) {
		t.Fatal("driverIndex 는 문자열이어야 한다")
	}
}

func TestGPUCountAndNone(t *testing.T) {
	m := meta(t)
	node := jsonx.ListM(jsonx.Get(m, "infra", "node"))[0]
	two := jsonx.Clone(m["spec"]).(M)
	two["acceleratorCount"] = 2
	if c := Build([]Row{{Node: node, Spec: two, Image: jsonx.AsM(m["image"])}}, nil, "", nil).Nodes[0].GpuCards; len(c) != 2 || c[1].DriverIndex != "1" {
		t.Fatal(c)
	}
	none := jsonx.Clone(m["spec"]).(M)
	none["acceleratorCount"], none["acceleratorModel"] = 0, ""
	if c := Build([]Row{{Node: node, Spec: none, Image: jsonx.AsM(m["image"])}}, nil, "", nil).Nodes[0].GpuCards; len(c) != 0 || c == nil {
		t.Fatal(c)
	}
}

func TestPlaceholder(t *testing.T) {
	n := Placeholder("595", open80).Nodes[0]
	if n.OS.ID != "ubuntu" || n.OS.VersionID != "24.04" || len(n.FirewallTable) != 2 || n.FirewallTable[1].DstPorts != "80" || n.GpuCards[0].DriverIndex != "0" {
		t.Fatal(n)
	}
}
