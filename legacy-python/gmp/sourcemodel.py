"""Tumblebug 메타 → Beetle 소스 모델(onpremisemodel.OnpremInfra).

실증의 소스는 모두 Tumblebug 로 만든다는 전제에서, 추천에 필요한 소스 사양은 Tumblebug 가 이미 안다 —
노드(디스크 · 사설 IP · CSP 자원 ID) · 스펙(vCPU · 메모리 · GPU) · 이미지(OS) · vNet(CIDR) · 보안 그룹(규칙).
VM 에 들어가지 않으므로 소스가 중지돼 있어도, 종료된 뒤 기록이 남아 있어도 만들 수 있다.
메타에 없는 값 — 설치된 GPU 드라이버 버전 — 은 정책의 driver_series(워크로드가 고정 설치하는 시리즈)로 채운다.
"""


def normalize_port(value):
    """Tumblebug 규칙의 Port('22' · '22-22' · '1-65535' · '' · '-1') → Beetle firewallTable 의 dstPorts('22' · '1-65535' · '*')"""
    v = str(value or "").strip()
    if v in ("", "-1", "*", "-1--1"):
        return "*"
    a, sep, b = v.partition("-")
    return a if sep and a == b else v


def firewall_from_sg(rules, open_ports=None):
    """인바운드 규칙만 옮긴다 (아웃바운드는 CSP 기본이 전부 허용이라 Beetle 이 따로 만들지 않아도 된다).
    open_ports(사례 정책)는 소스에 아직 없어도 타겟에 열어야 할 포트 — 같은 표에 더해 Beetle 이 보안 그룹 규칙으로 옮기게 한다."""
    table, seen = [], set()

    def add(cidr, port, proto):
        row = {"srcCIDR": cidr or "0.0.0.0/0", "srcPorts": "*", "dstCIDR": "*", "dstPorts": port,
               "protocol": "*" if proto in ("all", "-1", "") else proto, "direction": "inbound", "action": "allow"}
        key = (row["srcCIDR"], row["dstPorts"], row["protocol"])
        if key not in seen:
            seen.add(key)
            table.append(row)

    for r in rules or []:
        if str(r.get("Direction") or "").lower() != "inbound":
            continue
        add(r.get("CIDR"), normalize_port(r.get("Port") if "Port" in r else r.get("Ports")), str(r.get("Protocol") or "tcp").lower())
    for p in open_ports or []:
        add(p.get("cidr"), str(p["port"]), str(p.get("protocol", "tcp")).lower())
    return table


def _cpu(spec):
    vcpu = int(spec.get("vCPU") or 0)
    threads = 2 if vcpu >= 2 and vcpu % 2 == 0 else 1     # 클라우드 vCPU 는 보통 하이퍼스레드 2 — 메타에 코어 수가 없어 추정
    return {"architecture": spec.get("architecture") or "x86_64", "cpus": vcpu, "cores": max(1, vcpu // threads), "threads": threads}


def _gpu_cards(spec, driver_series):
    n = int(spec.get("acceleratorCount") or 0)
    model = spec.get("acceleratorModel") or ""
    if n <= 0 or not model:
        return []
    vendor = model.split()[0] if model.split() else ""
    return [{"driverIndex": str(i), "vendor": vendor, "model": model, "type": (spec.get("acceleratorType") or "gpu").upper(),
             "memoryTotalGB": spec.get("acceleratorMemoryGB") or 0, "driverVersion": str(driver_series or "")}
            for i in range(n)]


def _os(image):
    os_type = image.get("osType") or ""            # 'Ubuntu 24.04' — 이미지 이름(osDistribution)은 OS 버전이 아니라 넣지 않는다
    parts = os_type.split()
    name = parts[0] if parts else ""
    ver = parts[-1] if len(parts) > 1 else ""
    out = {"prettyName": os_type or "unknown", "name": name, "id": name.lower(), "versionId": ver, "version": ver}
    if name.lower() in ("ubuntu", "debian"):
        out["idLike"] = "debian"
    return out


def node_model(node, spec, image, subnet_cidr, rules, driver_series=None, open_ports=None):
    prefix = subnet_cidr.split("/")[1] if subnet_cidr and "/" in subnet_cidr else "24"
    ip = node.get("privateIP")
    return {
        "hostname": node.get("cspResourceName") or node.get("name") or node.get("id"),
        "machineId": node.get("cspResourceId") or node.get("id"),       # 소스 ↔ 타겟 이력 추적(cm-source-machine-ids)에 쓰인다
        "role": "worker",
        "cpu": _cpu(spec),
        "memory": {"type": "unknown", "totalSize": int(round(float(spec.get("memoryGiB") or 0)))},
        "rootDisk": {"label": "/", "type": node.get("rootDiskType") or "unknown", "totalSize": int(node.get("rootDiskSize") or 0)},
        "interfaces": [{"name": "eth0", "ipv4CidrBlocks": [f"{ip}/{prefix}"] if ip else [], "state": "up"}],
        "firewallTable": firewall_from_sg(rules, open_ports),
        "os": _os(image),
        "gpuCards": _gpu_cards(spec, driver_series),
    }


def build(nodes, vnet_cidrs, driver_series=None, open_ports=None):
    """nodes: [(node, spec, image, subnet_cidr, rules)] → OnpremInfra"""
    return {"network": {"ipv4Networks": {"cidrBlocks": sorted({c for c in vnet_cidrs if c}), "defaultGateways": []}, "ipv6Networks": {}},
            "nodes": [node_model(n, sp, im, sc, rl, driver_series, open_ports) for n, sp, im, sc, rl in nodes]}
