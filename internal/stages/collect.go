package stages

// 1. collect — Tumblebug 메타로 소스 사양을 만든다 (VM 에 들어가지 않는다).
//
// 실증 소스는 모두 Tumblebug 로 만든다. 그래서 추천에 필요한 소스 정보 — 노드 · 스펙(vCPU · 메모리 · GPU) · 이미지 · vNet · 보안 그룹 —
// 는 Tumblebug 가 이미 안다. 이 단계는 그 메타를 Beetle 입력 형식(onpremiseInfraModel)으로 바꿀 뿐이라, 소스가 중지돼 있어도 돈다.
// VM 안의 상태(서비스 · LLM 설정 · 식별 정보)는 Tumblebug 가 모르므로 package 단계가 소스에 원격 명령을 보낼 때 함께 읽는다.

import (
	"net"

	"gpu-migrate-orchestrator/internal/jsonx"
	"gpu-migrate-orchestrator/internal/sourcemodel"
	"gpu-migrate-orchestrator/internal/tumblebug"
)

const stCollect = "collect"

func init() {
	register(stCollect, Stage{Preflight: func(*Context) error { return nil }, Run: runCollect})
}

func runCollect(ctx *Context) (M, error) {
	src := ctx.Case.Source()
	ns, id := jsonx.S(src, "ns"), jsonx.S(src, "infra")
	pol := ctx.Case.Policy()
	driver, ports := jsonx.S(pol, "driver_series"), ctx.Case.OpenPorts()
	tb, err := ctx.TBSource() // 읽기 전용 조회라 dry-run 에서도 실제로 읽는다
	if err != nil {
		return nil, err
	}
	infra, err := tb.GetInfra(ns, id)
	if err != nil {
		return nil, err
	}
	if infra == nil {
		if ctx.DryRun {
			ctx.Log.Warn(stCollect, "소스 인프라 %s/%s 가 아직 없어 자리표시자 모델로 이어 간다 (dry-run)", ns, id)
			if _, err := ctx.WriteJSON(stCollect, "source-model.json", sourcemodel.Placeholder(driver, ports)); err != nil {
				return nil, err
			}
			ctx.State.SetKey("source_placeholder", true)
			return M{"placeholder": true}, nil
		}
		return nil, Errf("소스 인프라가 Tumblebug 에 없습니다: %s/%s — MapUI 로 먼저 만들고 사례의 source.infra 를 맞추세요", ns, id)
	}
	state := tumblebug.InfraState(infra)
	nodes := jsonx.ListM(infra["node"])
	if len(nodes) == 0 {
		return nil, Errf("소스 인프라 %s 에 노드가 없습니다 (상태 %s)", id, jsonx.S(infra, "status"))
	}
	if state != "Running" {
		ctx.Log.Warn(stCollect, "소스 인프라 상태 %s — 사양 · 추천은 메타로 진행하지만, package 단계 전에는 Running 이어야 한다", state)
	}

	specs, images, vnets, sgs := M{}, M{}, M{}, M{}
	var rows []sourcemodel.Row
	var cidrs []string
	for _, n := range nodes {
		sid, iid, vid := jsonx.S(n, "specId"), jsonx.S(n, "imageId"), jsonx.S(n, "vNetId")
		if _, ok := specs[sid]; !ok {
			m, err := tb.ResolveSpec(ns, sid)
			if err != nil {
				return nil, err
			}
			specs[sid] = orM(m, jsonx.AsM(n["spec"]))
		}
		if _, ok := images[iid]; !ok {
			m, err := tb.ResolveImage(ns, iid)
			if err != nil {
				return nil, err
			}
			images[iid] = orM(m, jsonx.AsM(n["image"]))
		}
		if _, ok := vnets[vid]; vid != "" && !ok {
			m, err := tb.GetVNet(ns, vid)
			if err != nil {
				return nil, err
			}
			vnets[vid] = orM(m, nil)
		}
		var rules []M
		for _, g := range jsonx.Strings(n["securityGroupIds"]) {
			if _, ok := sgs[g]; !ok {
				m, err := tb.GetSecurityGroup(ns, g)
				if err != nil {
					return nil, err
				}
				fr := jsonx.AsList(jsonx.AsM(m)["firewallRules"])
				if fr == nil {
					fr = []any{}
				}
				sgs[g] = fr
			}
			rules = append(rules, jsonx.ListM(sgs[g])...)
		}
		v := jsonx.AsM(vnets[vid])
		subnet := ""
		for _, x := range jsonx.ListM(v["subnetInfoList"]) {
			if jsonx.S(x, "id") == jsonx.S(n, "subnetId") {
				subnet = jsonx.S(x, "ipv4_CIDR")
				break
			}
		}
		spec := jsonx.AsM(specs[sid])
		if len(spec) == 0 {
			return nil, Errf("스펙 %s 를 Tumblebug 에서 찾지 못했습니다", sid)
		}
		rows = append(rows, sourcemodel.Row{Node: n, Spec: spec, Image: jsonx.AsM(images[iid]), SubnetCIDR: subnet, Rules: rules})
		cidr := sourceNetworkCIDR(jsonx.S(v, "cidrBlock"), subnet)
		if cidr != jsonx.S(v, "cidrBlock") {
			ctx.Log.Info(stCollect, "소스 vNet %s 가 /16 보다 넓어 노드 서브넷 %s 로 추천받는다 (AWS VPC 는 /16 ~ /28)", jsonx.S(v, "cidrBlock"), cidr)
		}
		cidrs = append(cidrs, cidr)
	}
	model := sourcemodel.Build(rows, cidrs, driver, ports)
	if _, err := ctx.WriteJSON(stCollect, "source-model.json", model); err != nil {
		return nil, err
	}
	nodeMeta := []any{}
	for _, n := range nodes {
		m := M{}
		for _, k := range []string{"id", "status", "cspResourceId", "specId", "imageId", "rootDiskType", "rootDiskSize",
			"vNetId", "subnetId", "securityGroupIds", "privateIP", "connectionName", "region"} {
			m[k] = n[k]
		}
		nodeMeta = append(nodeMeta, m)
	}
	imgMeta := M{}
	for k, v := range images {
		im := jsonx.AsM(v)
		m := M{}
		for _, f := range []string{"id", "osType", "osDistribution", "osArchitecture", "isGPUImage"} {
			m[f] = im[f]
		}
		imgMeta[k] = m
	}
	vnetMeta := M{}
	for k, v := range vnets {
		vm := jsonx.AsM(v)
		subs := []any{}
		for _, x := range jsonx.ListM(vm["subnetInfoList"]) {
			subs = append(subs, []any{x["id"], x["ipv4_CIDR"]})
		}
		vnetMeta[k] = M{"cidrBlock": vm["cidrBlock"], "subnets": subs}
	}
	meta := M{"infra": M{"id": infra["id"], "status": infra["status"], "label": infra["label"]},
		"nodes": nodeMeta, "specs": specs, "images": imgMeta, "vnets": vnetMeta, "securityGroups": sgs}
	if _, err := ctx.WriteJSON(stCollect, "source-meta.json", meta); err != nil {
		return nil, err
	}
	n0 := model.Nodes[0]
	gModel, gMem := "없음", ""
	if len(n0.GpuCards) > 0 {
		gModel, gMem = n0.GpuCards[0].Model, num(n0.GpuCards[0].MemoryTotalGB)
	}
	sp := jsonx.AsM(specs[jsonx.S(nodes[0], "specId")])
	ctx.Log.Info(stCollect, "소스 %s (%s) — %s · vCPU %s · %sGiB · GPU %s x%d %sGB · 디스크 %sGB · 방화벽 %d행 (Tumblebug 메타)",
		id, state, jsonx.S(nodes[0], "specId"), jsonx.S(sp, "vCPU"), jsonx.S(sp, "memoryGiB"), gModel, len(n0.GpuCards), gMem,
		jsonx.S(nodes[0], "rootDiskSize"), len(n0.FirewallTable))
	ctx.State.SetKey("source_state_at_collect", state)
	ctx.State.SetKey("source_placeholder", false)
	var gpu any
	if len(n0.GpuCards) > 0 {
		gpu = n0.GpuCards[0].Model
	}
	return M{"nodes": len(nodes), "spec": jsonx.S(nodes[0], "specId"), "gpu": gpu, "state": state}, nil
}

func orM(a, b M) M {
	if len(a) > 0 {
		return a
	}
	if b != nil {
		return b
	}
	return M{}
}

// sourceNetworkCIDR 는 Beetle 추천에 넘길 소스 네트워크 대역이다. vNet 이 /16 보다 넓으면(예: Beetle 이 만든 GCP vNet 10.0.0.0/13)
// AWS 처럼 VPC 를 /16 ~ /28 로만 받는 CSP 에서 Beetle 이 vNet 을 비워 추천하므로(10/27 GCP → AWS), 노드가 있는 서브넷 대역을 쓴다.
// 서브넷을 모르거나 vNet 대역을 읽지 못하면 vNet 대역 그대로.
func sourceNetworkCIDR(vnetCIDR, subnetCIDR string) string {
	_, vn, err := net.ParseCIDR(vnetCIDR)
	if err != nil || subnetCIDR == "" {
		return vnetCIDR
	}
	if ones, _ := vn.Mask.Size(); ones < 16 {
		return subnetCIDR
	}
	return vnetCIDR
}
