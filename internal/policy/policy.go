// Package policy 는 추천 검토 · 보정이다 — Beetle 후보 가운데 고르고, 넘기기 전에 우리 전제대로 고친다.
// 원본과 보정본을 둘 다 남긴다. 후보는 원본 JSON 그대로(map) 다뤄, 구조체에 없는 필드가 빠지지 않게 한다.
package policy

import (
	"fmt"
	"math"
	"net"
	"regexp"
	"strings"

	"gpu-migrate-orchestrator/internal/jsonx"
)

// M 은 JSON 객체다.
type M = jsonx.M

type gpuInfo struct {
	model            string
	vram, cost       float64
	hasCost          bool
	spec             string
	count, vcpu, mem any
}

func gpuOfCandidate(c M) *gpuInfo {
	specs := jsonx.ListM(c["targetSpecList"])
	if len(specs) == 0 {
		return nil
	}
	s := specs[0]
	cost, hasCost := jsonx.Float(s["costPerHour"])
	return &gpuInfo{model: jsonx.S(s, "acceleratorModel"), vram: jsonx.F(s["acceleratorMemoryGB"]), cost: cost, hasCost: hasCost && cost != 0,
		spec: jsonx.Str(jsonx.Or(jsonx.Or(s["cspSpecName"], s["id"]), s["name"])), count: s["acceleratorCount"], vcpu: s["vCPU"], mem: s["memoryGiB"]}
}

var vendorWords = regexp.MustCompile(`\b(nvidia|tesla|amd|radeon|instinct)\b`)
var spaces = regexp.MustCompile(`\s+`)

// NormModel 은 'NVIDIA T4' · 'Tesla T4' · 'T4' → 't4' — 칩 이름만 남긴다.
func NormModel(m string) string {
	m = vendorWords.ReplaceAllString(strings.ToLower(m), " ")
	return strings.TrimSpace(spaces.ReplaceAllString(m, " "))
}

func costKey(g *gpuInfo) float64 {
	if g.hasCost {
		return g.cost
	}
	return 9e9
}

func num(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprint(v)
}

// SelectCandidate 는 (index, 이유). 고를 것이 없으면 index=-1.
// 벤더가 다르면 후보가 아니다. same-model-first 는 같은 칩을 우선하고, 없으면 VRAM 이상 중 가까운 것.
func SelectCandidate(cands []any, sourceGPU M, mode string) (int, string) {
	wantModel := NormModel(jsonx.S(sourceGPU, "model"))
	wantVRAM := jsonx.F(sourceGPU["memoryTotalGB"])
	wantVendor := strings.ToLower(jsonx.S(sourceGPU, "vendor"))
	type scored struct {
		i          int
		g          *gpuInfo
		same, vram bool
	}
	var all []scored
	for i, x := range cands {
		g := gpuOfCandidate(jsonx.AsM(x))
		if g == nil || g.model == "" {
			continue
		}
		lm := strings.ToLower(g.model)
		if wantVendor != "" && !strings.Contains(lm, wantVendor) && wantVendor != "nvidia" {
			continue
		}
		if wantVendor == "nvidia" && (strings.Contains(lm, "amd") || strings.Contains(lm, "radeon")) {
			continue
		}
		// Beetle 이 16 을 17 로 표기하는 경우가 있어 1 GiB 여유
		all = append(all, scored{i, g, NormModel(g.model) == wantModel, g.vram >= wantVRAM-1})
	}
	if len(all) == 0 {
		return -1, "벤더가 맞고 GPU 가 있는 후보가 없다"
	}
	if mode == "same-model-first" {
		var best *scored
		for k := range all {
			x := &all[k]
			if x.same && x.vram && (best == nil || costKey(x.g) < costKey(best.g)) {
				best = x
			}
		}
		if best != nil {
			g := best.g
			return best.i, fmt.Sprintf("같은 GPU 모델(%s) 중 가장 싼 %s (VRAM %sGB, %s/h) — LLM 완전 일치의 전제", g.model, g.spec, num(g.vram), costStr(g))
		}
	}
	var best *scored
	for k := range all {
		x := &all[k]
		if !x.vram {
			continue
		}
		if best == nil || x.g.vram < best.g.vram || (x.g.vram == best.g.vram && costKey(x.g) < costKey(best.g)) {
			best = x
		}
	}
	if best == nil {
		return -1, fmt.Sprintf("VRAM %sGB 이상인 후보가 없다", num(wantVRAM))
	}
	note := ""
	if mode == "same-model-first" {
		note = "같은 모델이 없어 "
	}
	g := best.g
	return best.i, fmt.Sprintf("%sVRAM 이상 중 가장 가까운 %s (%s %sGB, %s/h) — GPU 가 달라 테스트2 는 WARN", note, g.spec, g.model, num(g.vram), costStr(g))
}

func costStr(g *gpuInfo) string {
	if !g.hasCost {
		return "None"
	}
	return fmt.Sprint(g.cost)
}

// ImageFits 는 추천 이미지가 정책과 맞는지 — (맞음, 이유). want 예 'ubuntu-24.04'.
// GPU 이미지(드라이버 내장)는 드라이버 시리즈까지 같아야 맞다 — 다르면 bootstrap 이 드라이버를 다시 깔지 않아
// 소스와 버전이 갈린다(nvidia-smi 가 이미 동작하므로).
func ImageFits(image M, want, driverSeries string) (bool, string) {
	if image == nil {
		return false, "추천 이미지 정보 없음"
	}
	var parts []string
	for _, k := range []string{"osType", "osDistribution", "name", "cspImageName"} {
		parts = append(parts, jsonx.S(image, k))
	}
	text := strings.ToLower(strings.Join(parts, " "))
	distro, version, _ := strings.Cut(strings.ToLower(want), "-")
	if distro != "" && !strings.Contains(text, distro) {
		return false, fmt.Sprintf("배포판이 %s 가 아니다", distro)
	}
	if version != "" && !strings.Contains(text, version) {
		return false, fmt.Sprintf("버전 %s 이 아니다", version)
	}
	if jsonx.Truthy(image["isGPUImage"]) {
		if driverSeries == "" {
			return true, "GPU 이미지 (드라이버 시리즈 지정 없음)"
		}
		ds := regexp.QuoteMeta(driverSeries)
		if regexp.MustCompile(`(nvidia[^0-9]*` + ds + `\b|-` + ds + `-)`).MatchString(text) {
			return true, fmt.Sprintf("%s %s + NVIDIA %s 내장 — 드라이버 설치 단계가 건너뛰어진다", distro, version, driverSeries)
		}
		return false, fmt.Sprintf("GPU 이미지의 드라이버가 %s 계열이 아니다", driverSeries)
	}
	return true, fmt.Sprintf("기본 %s %s", distro, version)
}

// AdjustCandidate 는 보정본과 변경 목록. imageOverride 는 Tumblebug 이미지 ID — 정해지면 추천 이미지를 바꾼다.
// imageInfo 는 Tumblebug 의 ImageInfo — 있으면 targetOsImageList 를 그것으로 바꾸고 nodeGroups 의 cspImageName 도 맞춘다.
func AdjustCandidate(cand M, policy M, imageOverride string, openPorts []M, imageInfo M) (M, []string) {
	c := jsonx.Clone(cand).(M)
	changes := []string{}
	disk := jsonx.Int(policy["root_disk_gb"])
	ti := jsonx.AsM(c["targetInfra"])
	groups := jsonx.ListM(ti["nodeGroups"])
	if len(groups) == 0 {
		groups = jsonx.ListM(ti["subGroups"])
	}
	for _, ng := range groups {
		if disk > 0 && jsonx.Int(ng["rootDiskSize"]) < disk {
			changes = append(changes, fmt.Sprintf("rootDiskSize %s → %d", jsonx.Str(ng["rootDiskSize"]), disk))
			ng["rootDiskSize"] = disk
		}
		if imageOverride != "" && jsonx.S(ng, "imageId") != imageOverride {
			changes = append(changes, fmt.Sprintf("imageId %s → %s", jsonx.S(ng, "imageId"), imageOverride))
			ng["imageId"] = imageOverride
			if imageInfo != nil && jsonx.S(imageInfo, "cspImageName") != "" {
				ng["cspImageName"] = imageInfo["cspImageName"]
			}
		}
	}
	if imageOverride != "" {
		if imageInfo != nil {
			c["targetOsImageList"] = []any{imageInfo}
		} else {
			c["targetOsImageList"] = []any{M{"id": imageOverride}}
		}
	}
	changes = append(changes, fixSubnets(jsonx.AsM(c["targetVNet"]))...)
	for _, p := range openPorts {
		rule := M{"Ports": jsonx.Str(p["port"]), "Protocol": jsonx.SOr(p, "protocol", "tcp"), "Direction": "inbound", "CIDR": jsonx.S(p, "cidr")}
		for _, sg := range jsonx.ListM(c["targetSecurityGroupList"]) {
			rules := jsonx.AsList(sg["firewallRules"]) // Beetle 은 규칙이 없으면 null 로 준다
			if rules == nil {
				rules = []any{}
			}
			found := false
			for _, r := range rules {
				if jsonx.Equal(r, rule) {
					found = true
					break
				}
			}
			if !found {
				rules = append(rules, rule)
				changes = append(changes, fmt.Sprintf("SG %s 에 %s/%s from %s", jsonx.S(sg, "name"), rule["Ports"], rule["Protocol"], rule["CIDR"]))
			}
			sg["firewallRules"] = rules
		}
	}
	return c, changes
}

// fixSubnets 는 서브넷 CIDR 이 vNet CIDR 과 같거나 더 넓으면 vNet 안의 더 좁은 블록(접두 길이 +2, 최대 /28)으로 바꾼다.
// Beetle 0.6.2 추천은 vNet 과 서브넷을 같은 /16 으로 주는데, Tumblebug 0.13.4 는 서브넷 접두 길이가 vNet 보다 커야 받는다
// ("subnet CIDR block … is not valid for vNet CIDR block"). 서브넷이 여럿이면 차례로 겹치지 않는 블록을 준다.
func fixSubnets(vnet M) []string {
	if vnet == nil {
		return nil
	}
	_, vn, err := net.ParseCIDR(jsonx.S(vnet, "cidrBlock"))
	if err != nil || vn.IP.To4() == nil {
		return nil
	}
	vp, _ := vn.Mask.Size()
	np := vp + 2
	if np > 28 {
		return nil
	}
	var changes []string
	next := 0
	for _, sn := range jsonx.ListM(vnet["subnetInfoList"]) {
		key := "ipv4_CIDR"
		if _, ok := sn[key]; !ok {
			key = "ipv4CIDR"
		}
		_, cur, err := net.ParseCIDR(jsonx.S(sn, key))
		if err == nil {
			if cp, _ := cur.Mask.Size(); cp > vp && vn.Contains(cur.IP) {
				continue
			}
		}
		if next >= 1<<(np-vp) {
			break
		}
		ip := vn.IP.To4()
		base := uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
		base += uint32(next) << (32 - np)
		block := fmt.Sprintf("%d.%d.%d.%d/%d", byte(base>>24), byte(base>>16), byte(base>>8), byte(base), np)
		changes = append(changes, fmt.Sprintf("서브넷 %s %s → %s (vNet %s 보다 좁아야 한다)", jsonx.S(sn, "name"), jsonx.S(sn, key), block, jsonx.S(vnet, "cidrBlock")))
		sn[key] = block
		next++
	}
	return changes
}
