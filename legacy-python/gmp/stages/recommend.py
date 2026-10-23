"""2. recommend — 타겟 사양 · 이미지를 정하고 생성 요청을 만든다.

provider=beetle   : Beetle 추천 후보 → 정책 선택 → 보정(이미지 · 디스크 · SG 규칙) → migrate-request.json
provider=tumblebug: 사례의 spec 이 있으면 그대로, 없으면 Tumblebug recommendSpec 에 소스 조건을 걸어 고른다 → infraDynamic 본문
어느 쪽이든 후보 목록과 선택 근거를 rationale.md 에 남긴다 (식별 대조표 · 결과서의 매핑 근거).
"""
from ..policy import adjust_candidate, image_fits, select_candidate
from ._util import StageError

STAGE = "recommend"


def preflight(ctx):
    tgt = ctx.case.target
    if tgt["provider"] == "beetle":
        ctx.beetle  # settings 에 없으면 ConfigError (dry-run 도 추천은 실제로 부른다)
    ctx.read_json("collect", "source-model.json")


def _gpu_name(g):
    """Tumblebug 스펙의 모델명은 벤더를 이미 담는다('NVIDIA T4') — 벤더를 한 번만 쓴다."""
    vendor, model = g.get("vendor") or "", g.get("model") or ""
    return model if model.lower().startswith(vendor.lower()) else f"{vendor} {model}".strip()


def _source_gpu(model):
    cards = (model.get("nodes") or [{}])[0].get("gpuCards") or []
    return cards[0] if cards else {"vendor": "", "model": "", "memoryTotalGB": 0}


def _resolve_image(ctx, tb, ns, tgt):
    """사례 image → Tumblebug(system ns)에 있으면 그대로. 없으면 policy.image(예 ubuntu-24.04 → 'ubuntu 24.04')로 searchImage 해
    기본 이미지 중 minimal 이 아닌 것을 고른다. 이 Tumblebug 의 이미지 ID 는 CSP 이미지 이름(aws 는 ami-…)이다. 조회라 dry-run 에서도 한다."""
    want = tgt.get("image")
    if want:
        im = tb.resolve_image(ns, want)
        if im:
            ctx.state.set_key("target_image_info", im)
            return want, f"사례에 지정 ({im.get('osDistribution') or im.get('name') or ''})"
        ctx.log.warn(f"사례의 image {want} 가 Tumblebug 에 없어 policy.image 로 찾습니다", STAGE)
    os_type = ctx.case.policy["image"].replace("-", " ")
    pick = tb.find_image(tgt["csp"], tgt["region"], os_type)
    if not pick:
        raise StageError(f"이미지를 찾지 못했습니다: {tgt['csp']}/{tgt['region']} '{os_type}' (사례 image={want!r})")
    iid = pick.get("id") or pick.get("cspImageName")
    ctx.state.set_key("target_image_info", pick)
    return iid, f"searchImage '{os_type}' → {iid} ({pick.get('osDistribution') or pick.get('name') or ''})"


def _tumblebug_pick_spec(ctx, tb, src_gpu, model):
    tgt = ctx.case.target
    node = (model.get("nodes") or [{}])[0]
    vcpu = (node.get("cpu") or {}).get("cpus") or 4
    mem = (node.get("memory") or {}).get("totalSize") or 16
    vram = src_gpu.get("memoryTotalGB") or 16
    filters = [("providerName", "==", tgt["csp"]), ("regionName", "==", tgt["region"]),
               ("vCPU", ">=", vcpu), ("memoryGiB", ">=", mem), ("acceleratorMemoryGB", ">=", max(1, vram - 1)),
               ("architecture", "==", (node.get("cpu") or {}).get("architecture") or "x86_64")]
    specs = tb.recommend_spec(filters, priority="cost", limit=20)
    cands = [{"targetSpecList": [s], "targetInfra": {"nodeGroups": [{"rootDiskSize": 0}]}} for s in specs]
    idx, reason = select_candidate(cands, src_gpu, ctx.case.policy["gpu"])
    if idx is None:
        raise StageError(f"Tumblebug recommendSpec 에서 맞는 GPU 사양을 찾지 못했습니다 ({reason})")
    return specs[idx], reason, specs


def run(ctx):
    tgt = ctx.case.target
    pol = ctx.case.policy
    model = ctx.read_json("collect", "source-model.json")
    src_gpu = _source_gpu(model)
    lines = [f"# 추천 · 선택 근거 — {ctx.case.name}", "", f"- 소스 GPU: {_gpu_name(src_gpu)} {src_gpu.get('memoryTotalGB')}GB",
             f"- 정책: gpu={pol['gpu']} · image={pol['image']} · root_disk={pol['root_disk_gb']}GB · 열 포트={pol['open_ports']}", ""]
    if ctx.state.get_key("source_placeholder"):
        lines[-1:] = ["- **소스 인프라가 아직 없어 자리표시자 모델로 추천받았다 (dry-run)** — 실제 소스를 만든 뒤 다시 돌린다", ""]
    if tgt["provider"] == "tumblebug":
        tb = ctx.tb_target
        if tgt.get("spec"):
            spec_id, why = tgt["spec"], "사례에 지정"
            if not tb.resolve_spec(tgt["ns"], spec_id):
                raise StageError(f"Tumblebug 에 스펙이 없습니다: {spec_id} (system · {tgt['ns']} 네임스페이스 모두)")
        else:
            spec, why, specs = _tumblebug_pick_spec(ctx, tb, src_gpu, model)
            spec_id = spec.get("id")
            ctx.write_json(STAGE, "candidates.json", specs)
        image_id, img_why = _resolve_image(ctx, tb, tgt["ns"], tgt)
        ng = {"name": "g1", "nodeGroupSize": 1, "specId": spec_id, "imageId": image_id,
              "rootDiskSize": int(pol["root_disk_gb"]), "label": {"gmp.role": "target"}}
        if tgt.get("zone"):
            ng["zone"] = tgt["zone"]
        body = {"name": tgt["name"], "description": f"gpu-migrate-orchestrator {ctx.case.name} run {ctx.state.data['run_id']}",
                "label": {"gmp.run": ctx.state.data["run_id"], "gmp.case": ctx.case.name},
                "policyOnPartialFailure": "rollback", "nodeGroups": [ng]}
        tb.validate_infra_dynamic(body)
        ctx.write_json(STAGE, "migrate-request.json", body)
        lines += ["## Tumblebug infraDynamic", f"- 사양: {spec_id} — {why}", f"- 이미지: {image_id} — {img_why}", f"- 디스크: {pol['root_disk_gb']}GB · 존: {tgt.get('zone') or 'Auto'}",
                  "- 80 포트 등 추가 규칙은 provision 단계에서 SG 에 더한다 (infraDynamic 의 기본 SG 는 22 만)"]
        ctx.write_text(STAGE, "rationale.md", "\n".join(lines))
        return {"spec": spec_id, "image": image_id}

    # ---- Beetle ----
    req = {"desiredCspAndRegionPair": {"csp": tgt["csp"], "region": tgt["region"]}, "onpremiseInfraModel": model}
    ctx.write_json(STAGE, "recommend-request.json", req)
    cands = ctx.beetle.recommend_infra(req, limit=5)          # 조회 — dry-run 에서도 실제로 부른다
    if not cands:
        raise StageError(f"Beetle 추천 후보가 없습니다 ({tgt['csp']}/{tgt['region']}) — 요청은 recommend/recommend-request.json")
    ctx.write_json(STAGE, "candidates.json", cands)
    idx, reason = select_candidate(cands, src_gpu, pol["gpu"])
    if idx is None:
        raise StageError(f"후보 {len(cands)}개 중 정책에 맞는 것이 없습니다 ({reason})")
    image_id = None
    rec_img = (cands[idx].get("targetOsImageList") or [None])[0]
    if pol["image"] == "keep":
        img_why = "Beetle 추천 이미지 유지 (policy.image=keep)"
    else:
        fits, fit_why = image_fits(rec_img, pol["image"], pol.get("driver_series"))
        if fits:
            img_why = f"Beetle 추천 이미지 유지 — {fit_why} ({(rec_img or {}).get('osDistribution') or ''})"
        else:
            image_id, img_why = _resolve_image(ctx, ctx.tb_target, tgt["ns"], {"csp": tgt["csp"], "region": tgt["region"], "image": None})
            img_why = f"추천 이미지를 바꿈 ({fit_why}) — {img_why}"
    adjusted, changes = adjust_candidate(cands[idx], pol, image_override=image_id, open_ports=pol["open_ports"],
                                         image_info=ctx.state.get_key("target_image_info") if image_id else None)
    ctx.write_json(STAGE, "migrate-request.json", adjusted)
    lines += ["## Beetle 추천", f"- 후보 {len(cands)}개 중 {idx + 1}번째 선택 — {reason}", f"- 이미지: {img_why}", "- 보정:"] + ([f"  - {c}" for c in changes] or ["  - (없음)"])
    ctx.write_text(STAGE, "rationale.md", "\n".join(lines))
    return {"candidate": idx, "changes": changes}
