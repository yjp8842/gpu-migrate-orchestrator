"""1. collect — Tumblebug 메타로 소스 사양을 만든다 (VM 에 들어가지 않는다).

실증 소스는 모두 Tumblebug 로 만든다. 그래서 추천에 필요한 소스 정보 — 노드 · 스펙(vCPU · 메모리 · GPU) · 이미지 · vNet · 보안 그룹 —
는 Tumblebug 가 이미 안다. 이 단계는 그 메타를 Beetle 입력 형식(onpremiseInfraModel)으로 바꿀 뿐이라, 소스가 중지돼 있어도 돈다.
VM 안의 상태(서비스 · LLM 설정 · 식별 정보)는 Tumblebug 가 모르므로 package 단계가 소스에 명령을 보낼 때 함께 읽는다.
"""
from .. import sourcemodel
from ._util import StageError

STAGE = "collect"

def placeholder_model(policy):
    """소스가 아직 없을 때(dry-run)의 자리표시자 — 실증 소스(AWS g4dn.xlarge · T4 · Ubuntu 24.04 · SSH 22)와 같은 모양을
    실제 변환기로 만든다. OS 필드가 비면 Beetle 이 엉뚱한 배포판 이미지를 추천한다."""
    node = {"name": "<source>", "id": "<source>", "rootDiskType": "gp3", "rootDiskSize": 100, "privateIP": "10.2.0.10"}
    spec = {"vCPU": 4, "memoryGiB": 16, "architecture": "x86_64", "acceleratorModel": "NVIDIA T4", "acceleratorCount": 1,
            "acceleratorMemoryGB": 16, "acceleratorType": "gpu"}
    image = {"osType": "Ubuntu 24.04"}
    rules = [{"Port": "22", "Protocol": "TCP", "Direction": "inbound", "CIDR": "0.0.0.0/0"}]
    return sourcemodel.build([(node, spec, image, "10.2.0.0/18", rules)], ["10.2.0.0/16"],
                             driver_series=policy.get("driver_series"), open_ports=policy.get("open_ports"))


def preflight(ctx):
    return


def run(ctx):
    src = ctx.case.source
    ns, infra_id = src["ns"], src["infra"]
    tb = ctx.tb_source                         # 읽기 전용 조회라 dry-run 에서도 실제로 읽는다
    infra = tb.get_infra(ns, infra_id)
    if infra is None:
        if ctx.dry_run:
            ctx.log.warn(f"소스 인프라 {ns}/{infra_id} 가 아직 없어 자리표시자 모델로 이어 간다 (dry-run)", STAGE)
            ctx.write_json(STAGE, "source-model.json", placeholder_model(ctx.case.policy))
            ctx.state.set_key("source_placeholder", True)
            return {"placeholder": True}
        raise StageError(f"소스 인프라가 Tumblebug 에 없습니다: {ns}/{infra_id} — MapUI 로 먼저 만들고 사례의 source.infra 를 맞추세요")
    state = tb.infra_state(infra)
    nodes = infra.get("node") or []
    if not nodes:
        raise StageError(f"소스 인프라 {infra_id} 에 노드가 없습니다 (상태 {infra.get('status')})")
    if state != "Running":
        ctx.log.warn(f"소스 인프라 상태 {state} — 사양 · 추천은 메타로 진행하지만, package 단계 전에는 Running 이어야 한다", STAGE)

    specs, images, vnets, sgs = {}, {}, {}, {}
    rows, cidrs = [], []
    for n in nodes:
        sid, iid, vid = n.get("specId"), n.get("imageId"), n.get("vNetId")
        if sid not in specs:
            specs[sid] = tb.resolve_spec(ns, sid) or n.get("spec") or {}
        if iid not in images:
            images[iid] = tb.resolve_image(ns, iid) or n.get("image") or {}
        if vid and vid not in vnets:
            vnets[vid] = tb.get_vnet(ns, vid) or {}
        rules = []
        for g in n.get("securityGroupIds") or []:
            if g not in sgs:
                sgs[g] = (tb.get_security_group(ns, g) or {}).get("firewallRules") or []
            rules += sgs[g]
        v = vnets.get(vid) or {}
        sub = next((x for x in v.get("subnetInfoList") or [] if x.get("id") == n.get("subnetId")), {})
        if not specs[sid]:
            raise StageError(f"스펙 {sid} 를 Tumblebug 에서 찾지 못했습니다")
        rows.append((n, specs[sid], images[iid], sub.get("ipv4_CIDR") or "", rules))
        cidrs.append(v.get("cidrBlock"))
    model = sourcemodel.build(rows, cidrs, driver_series=ctx.case.policy.get("driver_series"),
                              open_ports=ctx.case.policy.get("open_ports"))
    ctx.write_json(STAGE, "source-model.json", model)
    ctx.write_json(STAGE, "source-meta.json", {
        "infra": {"id": infra.get("id"), "status": infra.get("status"), "label": infra.get("label")},
        "nodes": [{k: n.get(k) for k in ("id", "status", "cspResourceId", "specId", "imageId", "rootDiskType", "rootDiskSize",
                                         "vNetId", "subnetId", "securityGroupIds", "privateIP", "connectionName", "region")} for n in nodes],
        "specs": specs, "images": {k: {f: v.get(f) for f in ("id", "osType", "osDistribution", "osArchitecture", "isGPUImage")} for k, v in images.items()},
        "vnets": {k: {"cidrBlock": v.get("cidrBlock"), "subnets": [(x.get("id"), x.get("ipv4_CIDR")) for x in v.get("subnetInfoList") or []]} for k, v in vnets.items()},
        "securityGroups": sgs})
    g = (model["nodes"][0].get("gpuCards") or [{}])[0]
    sp = specs[nodes[0].get("specId")]
    ctx.log.info(f"소스 {infra_id} ({state}) — {nodes[0].get('specId')} · vCPU {sp.get('vCPU')} · {sp.get('memoryGiB')}GiB · "
                 f"GPU {g.get('model') or '없음'} x{len(model['nodes'][0].get('gpuCards') or [])} {g.get('memoryTotalGB') or ''}GB · "
                 f"디스크 {nodes[0].get('rootDiskSize')}GB · 방화벽 {len(model['nodes'][0]['firewallTable'])}행 (Tumblebug 메타)", STAGE)
    ctx.state.set_key("source_state_at_collect", state)
    ctx.state.set_key("source_placeholder", False)
    return {"nodes": len(nodes), "spec": nodes[0].get("specId"), "gpu": g.get("model"), "state": state}
