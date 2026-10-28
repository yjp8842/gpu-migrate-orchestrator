package stages

// 2. recommend — 타겟 사양 · 이미지를 정하고 생성 요청을 만든다.
//
// provider=beetle   : Beetle 추천 후보 → 정책 선택 → 보정(이미지 · 디스크 · SG 규칙) → migrate-request.json
// provider=tumblebug: 사례의 spec 이 있으면 그대로, 없으면 Tumblebug recommendSpec 에 소스 조건을 걸어 고른다 → infraDynamic 본문
// 어느 쪽이든 선택 근거를 rationale.md 에 남긴다. 추천 · 스펙 · 이미지 조회는 자원을 만들지 않아 dry-run 에서도 실제로 부른다.

import (
	"fmt"
	"strings"

	"gpu-migrate-orchestrator/internal/jsonx"
	"gpu-migrate-orchestrator/internal/policy"
	"gpu-migrate-orchestrator/internal/sourcemodel"
	"gpu-migrate-orchestrator/internal/tumblebug"
)

const stRecommend = "recommend"

func init() { register(stRecommend, Stage{Preflight: preRecommend, Run: runRecommend}) }

func preRecommend(ctx *Context) error {
	if jsonx.S(ctx.Case.Target(), "provider") == "beetle" {
		if _, err := ctx.Beetle(); err != nil { // settings 에 없으면 오류 (dry-run 도 추천은 실제로 부른다)
			return err
		}
	}
	_, err := ctx.ReadJSON(stCollect, "source-model.json")
	return err
}

// GPUName 은 벤더를 한 번만 쓴다 — Tumblebug 스펙의 모델명은 벤더를 이미 담는다('NVIDIA T4').
func GPUName(g M) string {
	vendor, model := jsonx.S(g, "vendor"), jsonx.S(g, "model")
	if strings.HasPrefix(strings.ToLower(model), strings.ToLower(vendor)) {
		return model
	}
	return strings.TrimSpace(vendor + " " + model)
}

func sourceGPU(model any) M {
	nodes := jsonx.ListM(jsonx.Get(model, "nodes"))
	if len(nodes) > 0 {
		if cards := jsonx.ListM(nodes[0]["gpuCards"]); len(cards) > 0 {
			return cards[0]
		}
	}
	return M{"vendor": "", "model": "", "memoryTotalGB": 0}
}

// sourceImageWant 는 소스 OS 를 policy.image 형식('ubuntu-22.04')으로 바꾼다 (모르면 "").
func sourceImageWant(model any) string {
	nodes := jsonx.ListM(jsonx.Get(model, "nodes"))
	if len(nodes) == 0 {
		return ""
	}
	osm := jsonx.AsM(nodes[0]["os"])
	name := strings.ToLower(jsonx.SOr(osm, "id", jsonx.S(osm, "name")))
	ver := jsonx.SOr(osm, "versionId", jsonx.S(osm, "version"))
	if f := strings.Fields(ver); len(f) > 0 {
		ver = f[0] // '22.04.5 LTS (…)' 같은 꼴 → '22.04.5'
	}
	if parts := strings.Split(ver, "."); len(parts) > 2 {
		ver = parts[0] + "." + parts[1]
	}
	if name == "" || ver == "" {
		return ""
	}
	return name + "-" + ver
}

// resolveImage 는 사례 image → Tumblebug(system ns)에 있으면 그대로. 없으면 policy.image(예 ubuntu-24.04 → 'ubuntu 24.04')로
// searchImage 해 기본 이미지 중 minimal 이 아닌 것을 고른다. 이 Tumblebug 의 이미지 ID 는 CSP 이미지 이름(aws 는 ami-…)이다.
func resolveImage(ctx *Context, tb Tumblebug, ns, csp, region, want string) (string, string, error) {
	if want != "" {
		im, err := tb.ResolveImage(ns, want)
		if err != nil {
			return "", "", err
		}
		if im != nil {
			ctx.State.SetKey("target_image_info", im)
			return want, fmt.Sprintf("사례에 지정 (%s)", jsonx.Str(jsonx.Or(im["osDistribution"], im["name"]))), nil
		}
		ctx.Log.Warn(stRecommend, "사례의 image %s 가 Tumblebug 에 없어 policy.image 로 찾습니다", want)
	}
	osType := strings.ReplaceAll(jsonx.S(ctx.Case.Policy(), "image"), "-", " ")
	pick, err := tb.FindImage(csp, region, osType)
	if err != nil {
		return "", "", err
	}
	if pick == nil {
		return "", "", Errf("이미지를 찾지 못했습니다: %s/%s '%s' (사례 image=%q)", csp, region, osType, want)
	}
	iid := jsonx.Str(jsonx.Or(pick["id"], pick["cspImageName"]))
	ctx.State.SetKey("target_image_info", pick)
	return iid, fmt.Sprintf("searchImage '%s' → %s (%s)", osType, iid, jsonx.Str(jsonx.Or(pick["osDistribution"], pick["name"]))), nil
}

func tumblebugPickSpec(ctx *Context, tb Tumblebug, srcGPU M, model any) (M, string, []M, error) {
	tgt := ctx.Case.Target()
	node := M{}
	if ns := jsonx.ListM(jsonx.Get(model, "nodes")); len(ns) > 0 {
		node = ns[0]
	}
	vcpu := sourcemodel.VCPU(node["cpu"])
	if vcpu == 0 {
		vcpu = 4
	}
	mem := jsonx.Int(jsonx.Get(node, "memory", "totalSize"))
	if mem == 0 {
		mem = 16
	}
	vram := jsonx.F(srcGPU["memoryTotalGB"])
	if vram == 0 {
		vram = 16
	}
	minVRAM := vram - 1
	if minVRAM < 1 {
		minVRAM = 1
	}
	arch := jsonx.Str(jsonx.Or(jsonx.Get(node, "cpu", "architecture"), "x86_64"))
	filters := []tumblebug.Filter{tumblebug.F("providerName", "==", jsonx.S(tgt, "csp")), tumblebug.F("regionName", "==", jsonx.S(tgt, "region")),
		tumblebug.F("vCPU", ">=", vcpu), tumblebug.F("memoryGiB", ">=", mem), tumblebug.F("acceleratorMemoryGB", ">=", num(minVRAM)),
		tumblebug.F("architecture", "==", arch)}
	specs, err := tb.RecommendSpec(filters, "cost", 20)
	if err != nil {
		return nil, "", nil, err
	}
	cands := make([]any, 0, len(specs))
	for _, s := range specs {
		cands = append(cands, M{"targetSpecList": []any{s}, "targetInfra": M{"nodeGroups": []any{M{"rootDiskSize": 0}}}})
	}
	idx, reason := policy.SelectCandidate(cands, srcGPU, jsonx.S(ctx.Case.Policy(), "gpu"))
	if idx < 0 {
		return nil, "", nil, Errf("Tumblebug recommendSpec 에서 맞는 GPU 사양을 찾지 못했습니다 (%s)", reason)
	}
	return specs[idx], reason, specs, nil
}

func runRecommend(ctx *Context) (M, error) {
	tgt, pol := ctx.Case.Target(), ctx.Case.Policy()
	model, err := ctx.ReadJSON(stCollect, "source-model.json")
	if err != nil {
		return nil, err
	}
	if jsonx.S(pol, "image") == "source" { // 소스와 같은 OS · 버전으로 (기본)
		want := sourceImageWant(model)
		if want == "" {
			return nil, Errf("policy.image=source 인데 소스 모델에 OS 정보가 없습니다 — 사례 policy.image 에 'ubuntu-22.04' 처럼 적어 주세요")
		}
		pol["image"] = want
		ctx.Log.Info(stRecommend, "policy.image=source → 소스 OS %s", want)
	}
	src := sourceGPU(model)
	var ports []string
	for _, p := range ctx.Case.OpenPorts() {
		ports = append(ports, fmt.Sprintf("%s/%s (%s)", jsonx.Str(p["port"]), jsonx.SOr(p, "protocol", "tcp"), jsonx.S(p, "cidr")))
	}
	if len(ports) == 0 {
		ports = []string{"없음"}
	}
	lines := []string{"# 추천 · 선택 근거 — " + ctx.Case.Name, "", fmt.Sprintf("- 소스 GPU: %s %sGB", GPUName(src), jsonx.Str(src["memoryTotalGB"])),
		fmt.Sprintf("- 정책: gpu=%s · image=%s · root_disk=%sGB · 열 포트=%s", jsonx.S(pol, "gpu"), jsonx.S(pol, "image"), jsonx.S(pol, "root_disk_gb"), strings.Join(ports, ", ")), ""}
	if ctx.State.HasKey("source_placeholder") {
		lines[len(lines)-1] = "- **소스 인프라가 아직 없어 자리표시자 모델로 추천받았다 (dry-run)** — 실제 소스를 만든 뒤 다시 돌린다"
		lines = append(lines, "")
	}
	ns := jsonx.S(tgt, "ns")
	if jsonx.S(tgt, "provider") == "tumblebug" {
		tb, err := ctx.TBTarget()
		if err != nil {
			return nil, err
		}
		var specID, why string
		if s := jsonx.S(tgt, "spec"); s != "" {
			specID, why = s, "사례에 지정"
			m, err := tb.ResolveSpec(ns, specID)
			if err != nil {
				return nil, err
			}
			if m == nil {
				return nil, Errf("Tumblebug 에 스펙이 없습니다: %s (system · %s 네임스페이스 모두)", specID, ns)
			}
		} else {
			spec, reason, specs, err := tumblebugPickSpec(ctx, tb, src, model)
			if err != nil {
				return nil, err
			}
			specID, why = jsonx.S(spec, "id"), reason
			if _, err := ctx.WriteJSON(stRecommend, "candidates.json", specs); err != nil {
				return nil, err
			}
		}
		imageID, imgWhy, err := resolveImage(ctx, tb, ns, jsonx.S(tgt, "csp"), jsonx.S(tgt, "region"), jsonx.S(tgt, "image"))
		if err != nil {
			return nil, err
		}
		ng := M{"name": "g1", "nodeGroupSize": 1, "specId": specID, "imageId": imageID,
			"rootDiskSize": jsonx.Int(pol["root_disk_gb"]), "label": M{"gmp.role": "target"}}
		zone := jsonx.S(tgt, "zone")
		if zone != "" {
			ng["zone"] = zone
		}
		body := M{"name": jsonx.S(tgt, "name"), "description": fmt.Sprintf("gpu-migrate-orchestrator %s run %s", ctx.Case.Name, ctx.State.Data.RunID),
			"label": M{"gmp.run": ctx.State.Data.RunID, "gmp.case": ctx.Case.Name}, "policyOnPartialFailure": "rollback", "nodeGroups": []any{ng}}
		if err := tumblebug.ValidateInfraDynamic(body); err != nil {
			return nil, err
		}
		if _, err := ctx.WriteJSON(stRecommend, "migrate-request.json", body); err != nil {
			return nil, err
		}
		if zone == "" {
			zone = "Auto"
		}
		lines = append(lines, "## Tumblebug infraDynamic", "- 사양: "+specID+" — "+why, "- 이미지: "+imageID+" — "+imgWhy,
			fmt.Sprintf("- 디스크: %sGB · 존: %s", jsonx.S(pol, "root_disk_gb"), zone),
			"- 80 포트 등 추가 규칙은 provision 단계에서 SG 에 더한다 (infraDynamic 의 기본 SG 는 22 만)")
		if _, err := ctx.WriteText(stRecommend, "rationale.md", strings.Join(lines, "\n")); err != nil {
			return nil, err
		}
		return M{"spec": specID, "image": imageID}, nil
	}

	// ---- Beetle ----
	req := M{"desiredCspAndRegionPair": M{"csp": jsonx.S(tgt, "csp"), "region": jsonx.S(tgt, "region")}, "onpremiseInfraModel": model}
	if _, err := ctx.WriteJSON(stRecommend, "recommend-request.json", req); err != nil {
		return nil, err
	}
	b, err := ctx.Beetle()
	if err != nil {
		return nil, err
	}
	cands, err := b.RecommendInfra(req, 5) // 조회 — dry-run 에서도 실제로 부른다
	if err != nil {
		return nil, err
	}
	if len(cands) == 0 {
		return nil, Errf("Beetle 추천 후보가 없습니다 (%s/%s) — 요청은 recommend/recommend-request.json", jsonx.S(tgt, "csp"), jsonx.S(tgt, "region"))
	}
	if _, err := ctx.WriteJSON(stRecommend, "candidates.json", cands); err != nil {
		return nil, err
	}
	if why := nothingToRecommend(cands); why != "" {
		return nil, Errf("Beetle 이 추천하지 못했습니다 (%s/%s): %s — 후보 원문은 recommend/candidates.json", jsonx.S(tgt, "csp"), jsonx.S(tgt, "region"), why)
	}
	idx, reason := policy.SelectCandidate(cands, src, jsonx.S(pol, "gpu"))
	if idx < 0 {
		return nil, Errf("후보 %d개 중 정책에 맞는 것이 없습니다 (%s)", len(cands), reason)
	}
	cand := jsonx.AsM(cands[idx])
	imageID, imgWhy := "", ""
	var recImg M
	if l := jsonx.ListM(cand["targetOsImageList"]); len(l) > 0 {
		recImg = l[0]
	}
	if jsonx.S(pol, "image") == "keep" {
		imgWhy = "Beetle 추천 이미지 유지 (policy.image=keep)"
	} else if fits, fitWhy := policy.ImageFits(recImg, jsonx.S(pol, "image"), jsonx.S(pol, "driver_series")); fits {
		imgWhy = fmt.Sprintf("Beetle 추천 이미지 유지 — %s (%s)", fitWhy, jsonx.S(recImg, "osDistribution"))
	} else {
		tb, err := ctx.TBTarget()
		if err != nil {
			return nil, err
		}
		id, why, err := resolveImage(ctx, tb, ns, jsonx.S(tgt, "csp"), jsonx.S(tgt, "region"), "")
		if err != nil {
			return nil, err
		}
		imageID, imgWhy = id, fmt.Sprintf("추천 이미지를 바꿈 (%s) — %s", fitWhy, why)
	}
	var info M
	if imageID != "" {
		info = jsonx.AsM(ctx.State.Key("target_image_info"))
	}
	adjusted, changes := policy.AdjustCandidate(cand, pol, imageID, ctx.Case.OpenPorts(), info)
	// 사례 target.zone — Beetle 생성은 노드 그룹이 아니라 서브넷의 zone 으로 VM 존을 정한다 (비우면 연결의 기본 존).
	// GPU 는 존마다 자원이 달라 기본 존에서 「does not have enough resources」가 나면 다른 존을 적는다 (10/23 GCP L4).
	if z := jsonx.S(tgt, "zone"); z != "" {
		for _, sn := range jsonx.ListM(jsonx.Get(adjusted, "targetVNet", "subnetInfoList")) {
			if jsonx.S(sn, "zone") != z {
				changes = append(changes, fmt.Sprintf("서브넷 %s 존 %s → %s (사례 target.zone)", jsonx.S(sn, "name"), jsonx.SOr(sn, "zone", "기본"), z))
				sn["zone"] = z
			}
		}
	}
	if _, err := ctx.WriteJSON(stRecommend, "migrate-request.json", adjusted); err != nil {
		return nil, err
	}
	lines = append(lines, "## Beetle 추천", fmt.Sprintf("- 후보 %d개 중 %d번째 선택 — %s", len(cands), idx+1, reason), "- 이미지: "+imgWhy, "- 보정:")
	if len(changes) == 0 {
		lines = append(lines, "  - (없음)")
	}
	for _, c := range changes {
		lines = append(lines, "  - "+c)
	}
	vLines, vErr := precheckCreate(ctx, b, ns, adjusted)
	lines = append(lines, vLines...)
	if _, err := ctx.WriteText(stRecommend, "rationale.md", strings.Join(lines, "\n")); err != nil {
		return nil, err
	}
	if vErr != nil {
		return nil, vErr
	}
	return M{"candidate": idx, "changes": changes}, nil
}

// nothingToRecommend 는 Beetle 후보가 모두 status=nothing-to-recommend 이면 그 설명을 돌려준다 (아니면 "").
// Beetle 은 맞는 사양 · 이미지가 없을 때도 빈 목록 대신 사양 없는 후보 하나에 이 상태와 이유를 담아 준다.
func nothingToRecommend(cands []any) string {
	why := ""
	for _, c := range cands {
		m := jsonx.AsM(c)
		if jsonx.S(m, "status") != "nothing-to-recommend" {
			return ""
		}
		if why == "" {
			why = jsonx.SOr(m, "description", "nothing-to-recommend")
		}
	}
	return why
}

// precheckCreate 는 생성 가능 사전 검사다. 보정까지 끝난 생성 요청을 Beetle 검증 API 에 보내(자원을 만들지 않는다)
// 생성 직전에 Beetle 이 다시 할 검사 — 사양 · 이미지 호환, 필수 값, 연결 이름 — 를 미리 받는다.
// 10/27 AWS → NCP: 추천은 나왔지만 T4(XEN) 사양과 KVM 이미지가 맞지 않아 생성에서 거부됐다 → 이제 추천 단계에서 멈춘다.
// 이름 충돌(RESOURCE_ALREADY_EXISTS)은 생성 때 nameSeed 가 이름 앞에 붙어 달라지므로 막지 않고 참고로만 적는다.
// 검증 API 를 부르지 못하면 막지 않는다 — 생성 단계에서 Beetle 이 같은 검사를 다시 한다.
func precheckCreate(ctx *Context, b Beetle, ns string, body M) ([]string, error) {
	lines := []string{"", "## 생성 가능 사전 검사 (Beetle validation)"}
	res, err := b.ValidateInfra(ns, body, false)
	if err != nil {
		ctx.Log.Warn(stRecommend, "생성 가능 사전 검사를 하지 못했습니다 (Beetle 검증 API): %v — 생성 단계에서 Beetle 이 다시 검사합니다", err)
		ctx.State.SetKey("target_validation", M{"checked": false, "error": err.Error()})
		return append(lines, "- 검사 못 함 — "+err.Error()), nil
	}
	if _, err := ctx.WriteJSON(stRecommend, "validation.json", res); err != nil {
		return lines, err
	}
	issues := jsonx.ListM(res["issues"])
	var block []string
	for _, is := range issues {
		code, msg := jsonx.S(is, "code"), jsonx.S(is, "message")
		if jsonx.S(is, "severity") == "error" && code != "RESOURCE_ALREADY_EXISTS" {
			block = append(block, fmt.Sprintf("%s: %s", code, msg))
			lines = append(lines, fmt.Sprintf("- 막힘 %s — %s", code, msg))
		} else {
			lines = append(lines, fmt.Sprintf("- 참고 %s (%s) — %s", code, jsonx.S(is, "severity"), msg))
		}
	}
	ctx.State.SetKey("target_validation", M{"checked": true, "valid": len(block) == 0, "issues": issues})
	if len(block) > 0 {
		return lines, Errf("생성 가능 사전 검사 실패 — %s (자원은 만들지 않았습니다 · recommend/validation.json)", strings.Join(block, " / "))
	}
	if len(issues) == 0 {
		lines = append(lines, "- 통과 — 사양 · 이미지 호환 · 필수 값 · 연결 이름")
	}
	ctx.Log.Info(stRecommend, "생성 가능 사전 검사 통과 (Beetle validation)")
	return lines, nil
}
