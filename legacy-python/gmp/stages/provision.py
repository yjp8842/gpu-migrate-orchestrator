"""3. provision — 타겟 VM 을 만들고(없으면) SSH 가 열릴 때까지 기다린 뒤 키와 노드 정보를 보관한다.

멱등: 인프라 이름을 먼저 state 에 적고, 같은 이름이 이미 있으면 만들지 않고 조회로 이어 간다 (infraDynamic 은 자동 재시도 금지).
"""
from ..clients.ssh import Ssh
from ..sourcemodel import normalize_port
from ._util import StageError, tick_logger

STAGE = "provision"


def preflight(ctx):
    ctx.read_json("recommend", "migrate-request.json")


def _rule_key(port, protocol, direction, cidr):
    return (normalize_port(port), str(protocol or "").lower(), str(direction or "").lower(), str(cidr or "").strip())


def missing_rules(existing, ports):
    """사례의 열 포트 가운데 보안 그룹에 아직 없는 인바운드 규칙만 (POST 형식 {Ports, Protocol, Direction, CIDR}).
    Beetle 경로는 소스 방화벽 표(열 포트 포함)로 Beetle 이 이미 규칙을 만들었으므로 보통 빈 목록이다 —
    같은 규칙을 다시 넣으면 CSP 가 중복으로 거부할 수 있다."""
    have = {_rule_key(r.get("Port") if "Port" in r else r.get("Ports"), r.get("Protocol"), r.get("Direction"), r.get("CIDR"))
            for r in existing or []}
    out = []
    for p in ports or []:
        rule = {"Ports": str(p["port"]), "Protocol": p.get("protocol", "tcp"), "Direction": "inbound", "CIDR": p["cidr"]}
        k = _rule_key(rule["Ports"], rule["Protocol"], rule["Direction"], rule["CIDR"])
        if k not in have:
            have.add(k)
            out.append(rule)
    return out


def _open_ports(ctx, tb, ns, nodes):
    ports = ctx.case.policy.get("open_ports") or []
    if not ports or ctx.state.get_key("sg_rules_added"):
        return
    sg_ids = nodes[0].get("securityGroupIds") or []
    if not sg_ids:
        ctx.log.warn("노드의 보안 그룹 ID 를 찾지 못해 포트 규칙을 더하지 못했습니다", STAGE)
        return
    sg = tb.get_security_group(ns, sg_ids[0]) or {}
    rules = missing_rules(sg.get("firewallRules"), ports)
    if not rules:
        ctx.log.info(f"보안 그룹 {sg_ids[0]} 에 열 포트 규칙이 이미 있습니다 — 추가하지 않음", STAGE)
    else:
        tb.add_sg_rules(ns, sg_ids[0], rules)
        ctx.log.info(f"보안 그룹 {sg_ids[0]} 에 규칙 추가: " + ", ".join(f"{r['Ports']}/{r['Protocol']} from {r['CIDR']}" for r in rules), STAGE)
    ctx.state.set_key("sg_rules_added", {"sg": sg_ids[0], "rules": rules})


def run(ctx):
    tgt = ctx.case.target
    ns = tgt["ns"]
    req = ctx.read_json("recommend", "migrate-request.json")
    tb = ctx.tb_target
    if ctx.dry_run:
        what = "Tumblebug POST /infraDynamic" if tgt["provider"] == "tumblebug" else f"Beetle POST /migration/ns/{ns}/infra?nameSeed={tgt['name']}"
        ctx.dry(STAGE, what, req)
        ctx.set_nodes("target", [{"nodeId": "g1-1", "publicIP": "<target.publicIP>", "sshPort": 22, "nodeUserName": "cb-user"}])
        ctx.write_json(STAGE, "target.json", {"dry_run": True, "infra": tgt["name"]})
        return {"dry_run": True}

    infra_id = ctx.state.get_key("target_infra_id")
    infra = tb.get_infra(ns, infra_id) if infra_id else None
    if infra is None and tgt["provider"] == "tumblebug":
        infra_id = tgt["name"]
        ctx.state.set_key("target_infra_id", infra_id)                 # 부작용 전에 기록
        infra = tb.get_infra(ns, infra_id)
        if infra is not None:
            other = (infra.get("label") or {}).get("gmp.case")
            if other and other != ctx.case.name:
                raise StageError(f"인프라 {infra_id} 가 이미 있고 다른 사례({other})의 것입니다 — target.name 을 바꾸세요")
            ctx.log.warn(f"인프라 {infra_id} 가 이미 있어 만들지 않고 이어 갑니다 (상태 {tb.infra_state(infra)})", STAGE)
        else:
            ctx.log.info(f"인프라 생성 요청: {infra_id} — {req['nodeGroups'][0]['specId']} · {req['nodeGroups'][0]['imageId']} (동기, 수 분)", STAGE)
            infra = tb.infra_dynamic(ns, req, timeout_sec=int(ctx.settings.timeouts["provision_min"]) * 60)
            ctx.log.info(f"생성 응답: {infra.get('status')}", STAGE)
            if infra.get("creationErrors"):
                raise StageError(f"생성 오류: {str(infra.get('creationErrors'))[:300]}")
    elif infra is None:
        if ctx.state.get_key("beetle_migrate_started"):
            raise StageError("Beetle 생성을 시작했지만 인프라 ID 를 모릅니다 — Beetle UI 에서 ID 를 찾아 state.json 의 keys.target_infra_id 에 적고 다시 돌리세요")
        ctx.state.set_key("beetle_migrate_started", True)
        data = ctx.beetle.migrate_infra(ns, req, name_seed=tgt["name"])
        infra_id = data.get("id") or data.get("name")
        if not infra_id:
            raise StageError(f"Beetle 생성 응답에 인프라 ID 가 없습니다: {str(data)[:300]}")
        ctx.state.set_key("target_infra_id", infra_id)
        ctx.log.info(f"Beetle 가 만든 인프라: {infra_id} ({data.get('status')})", STAGE)
        infra = tb.get_infra(ns, infra_id)

    infra = tb.wait_infra(ns, infra_id, ("Running",), timeout_sec=900, on_tick=tick_logger(ctx.log, STAGE))
    nodes = tb.nodes(infra)
    access = tb.access_info(ns, infra_id)
    for a in access:                                  # SG · spec 정보는 infra 쪽에만 있다
        for n in nodes:
            if n["nodeId"] == a["nodeId"]:
                a.update({k: n.get(k) for k in ("securityGroupIds", "specId", "imageId", "cspResourceId", "status")})
    ctx.save_access("target", access)
    node = access[0]
    ctx.log.info(f"타겟 노드 {node['nodeId']} {node['publicIP']} ({node['nodeUserName']}) · {node.get('cspResourceId')}", STAGE)
    ssh = Ssh(node["publicIP"], node["nodeUserName"], ctx.key_path("target"), port=node["sshPort"], known_hosts=ctx.known_hosts)
    ctx.log.info("SSH 준비 대기 (cloud-init 포함, 최대 10분)", STAGE)
    ssh.wait_ready(timeout_sec=600, poll=15)
    _open_ports(ctx, tb, ns, access)
    ctx.write_json(STAGE, "target.json", {"infra": infra_id, "status": infra.get("status"),
                                          "nodes": [{k: v for k, v in n.items() if k != "privateKey"} for n in access]})
    return {"infra": infra_id, "ip": node["publicIP"]}
