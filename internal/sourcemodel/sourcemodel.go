// Package sourcemodel 은 Tumblebug 메타를 Beetle 소스 모델(onpremisemodel.OnpremInfra)로 바꾼다.
//
// 실증의 소스는 모두 Tumblebug 로 만든다는 전제에서, 추천에 필요한 소스 사양은 Tumblebug 가 이미 안다 —
// 노드(디스크 · 사설 IP · CSP 자원 ID) · 스펙(vCPU · 메모리 · GPU) · 이미지(OS) · vNet(CIDR) · 보안 그룹(규칙).
// VM 에 들어가지 않으므로 소스가 중지돼 있어도, 종료된 뒤 기록이 남아 있어도 만들 수 있다.
// 메타에 없는 값 — 설치된 GPU 드라이버 버전 — 은 정책의 driver_series(워크로드가 고정 설치하는 시리즈)로 채운다.
package sourcemodel

import (
	"math"
	"sort"
	"strconv"
	"strings"

	"gpu-migrate-orchestrator/internal/jsonx"
)

// M 은 JSON 객체다.
type M = jsonx.M

// OnpremInfra 는 Beetle 추천 입력의 onpremiseInfraModel 이다.
type OnpremInfra struct {
	Network Network `json:"network"`
	Nodes   []Node  `json:"nodes"`
}

// Network 는 네트워크 대역이다.
type Network struct {
	IPv4Networks NetworkDetail `json:"ipv4Networks"`
	IPv6Networks struct{}      `json:"ipv6Networks"`
}

// NetworkDetail 은 CIDR 과 게이트웨이다.
type NetworkDetail struct {
	CidrBlocks      []string `json:"cidrBlocks"`
	DefaultGateways []any    `json:"defaultGateways"`
}

// Node 는 서버 한 대다.
type Node struct {
	Hostname      string      `json:"hostname"`
	MachineID     string      `json:"machineId"`
	Role          string      `json:"role"`
	CPU           CPU         `json:"cpu"`
	Memory        Memory      `json:"memory"`
	RootDisk      Disk        `json:"rootDisk"`
	Interfaces    []Interface `json:"interfaces"`
	FirewallTable []Rule      `json:"firewallTable"`
	OS            OS          `json:"os"`
	GpuCards      []GPU       `json:"gpuCards"`
}

// CPU · Memory · Disk · Interface · Rule · OS · GPU 는 Beetle 스키마의 같은 이름 항목이다.
type CPU struct {
	Architecture string `json:"architecture"`
	Cpus         int    `json:"cpus"`
	Cores        int    `json:"cores"`
	Threads      int    `json:"threads"`
}

type Memory struct {
	Type      string `json:"type"`
	TotalSize int    `json:"totalSize"`
}

type Disk struct {
	Label     string `json:"label"`
	Type      string `json:"type"`
	TotalSize int    `json:"totalSize"`
}

type Interface struct {
	Name           string   `json:"name"`
	IPv4CidrBlocks []string `json:"ipv4CidrBlocks"`
	State          string   `json:"state"`
}

type Rule struct {
	SrcCIDR   string `json:"srcCIDR"`
	SrcPorts  string `json:"srcPorts"`
	DstCIDR   string `json:"dstCIDR"`
	DstPorts  string `json:"dstPorts"`
	Protocol  string `json:"protocol"`
	Direction string `json:"direction"`
	Action    string `json:"action"`
}

type OS struct {
	PrettyName string `json:"prettyName"`
	Name       string `json:"name"`
	ID         string `json:"id"`
	VersionID  string `json:"versionId"`
	Version    string `json:"version"`
	IDLike     string `json:"idLike,omitempty"`
}

type GPU struct {
	DriverIndex   string  `json:"driverIndex"` // Beetle GpuCardProperty.driverIndex 는 문자열
	Vendor        string  `json:"vendor"`
	Model         string  `json:"model"`
	Type          string  `json:"type"`
	MemoryTotalGB float64 `json:"memoryTotalGB"`
	DriverVersion string  `json:"driverVersion"`
}

// NormalizePort 는 Tumblebug 규칙의 Port('22' · '22-22' · '1-65535' · ” · '-1')를 Beetle 의 dstPorts('22' · '1-65535' · '*')로.
func NormalizePort(v string) string {
	v = strings.TrimSpace(v)
	switch v {
	case "", "-1", "*", "-1--1":
		return "*"
	}
	a, b, ok := strings.Cut(v, "-")
	if ok && a == b {
		return a
	}
	return v
}

// RulePort 는 규칙의 포트 값 — 조회 응답은 Port, 추가 요청은 Ports 를 쓴다.
func RulePort(r M) string {
	if v, ok := r["Port"]; ok {
		return jsonx.Str(v)
	}
	return jsonx.S(r, "Ports")
}

// FirewallFromSG 는 인바운드 규칙만 옮긴다 (아웃바운드는 CSP 기본이 전부 허용이라 Beetle 이 따로 만들지 않아도 된다).
// openPorts(사례 정책)는 소스에 아직 없어도 타겟에 열어야 할 포트 — 같은 표에 더해 Beetle 이 보안 그룹 규칙으로 옮기게 한다.
func FirewallFromSG(rules []M, openPorts []M) []Rule {
	table := []Rule{}
	seen := map[string]bool{}
	add := func(cidr, port, proto string) {
		if cidr == "" {
			cidr = "0.0.0.0/0"
		}
		if proto == "all" || proto == "-1" || proto == "" {
			proto = "*"
		}
		key := cidr + "|" + port + "|" + proto
		if seen[key] {
			return
		}
		seen[key] = true
		table = append(table, Rule{SrcCIDR: cidr, SrcPorts: "*", DstCIDR: "*", DstPorts: port, Protocol: proto, Direction: "inbound", Action: "allow"})
	}
	for _, r := range rules {
		if strings.ToLower(jsonx.S(r, "Direction")) != "inbound" {
			continue
		}
		add(jsonx.S(r, "CIDR"), NormalizePort(RulePort(r)), strings.ToLower(jsonx.SOr(r, "Protocol", "tcp")))
	}
	for _, p := range openPorts {
		add(jsonx.S(p, "cidr"), jsonx.Str(p["port"]), strings.ToLower(jsonx.SOr(p, "protocol", "tcp")))
	}
	return table
}

func cpuOf(spec M) CPU {
	vcpu := jsonx.Int(spec["vCPU"])
	threads := 1
	if vcpu >= 2 && vcpu%2 == 0 { // 클라우드 vCPU 는 보통 하이퍼스레드 2 — 메타에 코어 수가 없어 추정
		threads = 2
	}
	cores := vcpu / threads
	if cores < 1 {
		cores = 1
	}
	return CPU{Architecture: jsonx.SOr(spec, "architecture", "x86_64"), Cpus: vcpu, Cores: cores, Threads: threads}
}

func gpuCards(spec M, driverSeries string) []GPU {
	cards := []GPU{}
	n := jsonx.Int(spec["acceleratorCount"])
	model := jsonx.S(spec, "acceleratorModel")
	if n <= 0 || model == "" {
		return cards
	}
	vendor := ""
	if f := strings.Fields(model); len(f) > 0 {
		vendor = f[0]
	}
	for i := 0; i < n; i++ {
		cards = append(cards, GPU{DriverIndex: strconv.Itoa(i), Vendor: vendor, Model: model,
			Type: strings.ToUpper(jsonx.SOr(spec, "acceleratorType", "gpu")), MemoryTotalGB: jsonx.F(spec["acceleratorMemoryGB"]),
			DriverVersion: driverSeries})
	}
	return cards
}

func osOf(image M) OS {
	osType := jsonx.S(image, "osType") // 'Ubuntu 24.04' — 이미지 이름(osDistribution)은 OS 버전이 아니라 넣지 않는다
	parts := strings.Fields(osType)
	name, ver := "", ""
	if len(parts) > 0 {
		name = parts[0]
	}
	if len(parts) > 1 {
		ver = parts[len(parts)-1]
	}
	pretty := osType
	if pretty == "" {
		pretty = "unknown"
	}
	o := OS{PrettyName: pretty, Name: name, ID: strings.ToLower(name), VersionID: ver, Version: ver}
	if l := strings.ToLower(name); l == "ubuntu" || l == "debian" {
		o.IDLike = "debian"
	}
	return o
}

// Row 는 노드 한 대의 메타 묶음이다.
type Row struct {
	Node, Spec, Image M
	SubnetCIDR        string
	Rules             []M
}

// NodeModel 은 노드 한 대를 Beetle 형식으로.
func NodeModel(r Row, driverSeries string, openPorts []M) Node {
	prefix := "24"
	if _, p, ok := strings.Cut(r.SubnetCIDR, "/"); ok {
		prefix = p
	}
	ips := []string{}
	if ip := jsonx.S(r.Node, "privateIP"); ip != "" {
		ips = append(ips, ip+"/"+prefix)
	}
	return Node{
		Hostname:      jsonx.Str(jsonx.Or(jsonx.Or(r.Node["cspResourceName"], r.Node["name"]), r.Node["id"])),
		MachineID:     jsonx.Str(jsonx.Or(r.Node["cspResourceId"], r.Node["id"])), // 소스 ↔ 타겟 이력 추적(cm-source-machine-ids)에 쓰인다
		Role:          "worker",
		CPU:           cpuOf(r.Spec),
		Memory:        Memory{Type: "unknown", TotalSize: int(math.RoundToEven(jsonx.F(r.Spec["memoryGiB"])))},
		RootDisk:      Disk{Label: "/", Type: jsonx.SOr(r.Node, "rootDiskType", "unknown"), TotalSize: jsonx.Int(r.Node["rootDiskSize"])},
		Interfaces:    []Interface{{Name: "eth0", IPv4CidrBlocks: ips, State: "up"}},
		FirewallTable: FirewallFromSG(r.Rules, openPorts),
		OS:            osOf(r.Image),
		GpuCards:      gpuCards(r.Spec, driverSeries),
	}
}

// Build 는 노드 묶음과 vNet CIDR 로 OnpremInfra 를 만든다.
func Build(rows []Row, vnetCIDRs []string, driverSeries string, openPorts []M) OnpremInfra {
	set := map[string]bool{}
	for _, c := range vnetCIDRs {
		if c != "" {
			set[c] = true
		}
	}
	cidrs := []string{}
	for c := range set {
		cidrs = append(cidrs, c)
	}
	sort.Strings(cidrs)
	nodes := []Node{}
	for _, r := range rows {
		nodes = append(nodes, NodeModel(r, driverSeries, openPorts))
	}
	return OnpremInfra{Network: Network{IPv4Networks: NetworkDetail{CidrBlocks: cidrs, DefaultGateways: []any{}}}, Nodes: nodes}
}

// Placeholder 는 소스가 아직 없을 때(dry-run)의 자리표시자 — 실증 소스(AWS g4dn.xlarge · T4 · Ubuntu 24.04 · SSH 22)와 같은 모양을
// 실제 변환기로 만든다. OS 필드가 비면 Beetle 이 엉뚱한 배포판 이미지를 추천한다.
func Placeholder(driverSeries string, openPorts []M) OnpremInfra {
	row := Row{
		Node:       M{"name": "<source>", "id": "<source>", "rootDiskType": "gp3", "rootDiskSize": 100, "privateIP": "10.2.0.10"},
		Spec:       M{"vCPU": 4, "memoryGiB": 16, "architecture": "x86_64", "acceleratorModel": "NVIDIA T4", "acceleratorCount": 1, "acceleratorMemoryGB": 16, "acceleratorType": "gpu"},
		Image:      M{"osType": "Ubuntu 24.04"},
		SubnetCIDR: "10.2.0.0/18",
		Rules:      []M{{"Port": "22", "Protocol": "TCP", "Direction": "inbound", "CIDR": "0.0.0.0/0"}},
	}
	return Build([]Row{row}, []string{"10.2.0.0/16"}, driverSeries, openPorts)
}
