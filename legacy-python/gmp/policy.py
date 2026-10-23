"""추천 검토 · 보정 — Beetle 후보 가운데 고르고, 넘기기 전에 우리 전제대로 고친다. 원본과 보정본을 둘 다 남긴다."""
import copy
import re


def _gpu_of_candidate(c):
    specs = c.get("targetSpecList") or []
    if not specs:
        return None
    s = specs[0]
    return {"model": s.get("acceleratorModel") or "", "count": s.get("acceleratorCount") or 0,
            "vram": s.get("acceleratorMemoryGB") or 0, "spec": s.get("cspSpecName") or s.get("id") or s.get("name"),
            "vcpu": s.get("vCPU"), "mem": s.get("memoryGiB"), "cost": s.get("costPerHour")}


def _norm_model(m):
    """'NVIDIA T4' · 'Tesla T4' · 'T4' → 't4' — 칩 이름만 남긴다."""
    m = (m or "").lower()
    m = re.sub(r"\b(nvidia|tesla|amd|radeon|instinct)\b", " ", m)
    return re.sub(r"\s+", " ", m).strip()


def select_candidate(candidates, source_gpu, mode="same-model-first"):
    """(index, reason). 벤더가 다르면 후보가 아니다. same-model-first 는 같은 칩을 우선하고, 없으면 VRAM 이상 중 가까운 것."""
    want_model = _norm_model(source_gpu.get("model"))
    want_vram = float(source_gpu.get("memoryTotalGB") or 0)
    want_vendor = (source_gpu.get("vendor") or "").lower()
    scored = []
    for i, c in enumerate(candidates):
        g = _gpu_of_candidate(c)
        if not g or not g["model"]:
            continue
        if want_vendor and want_vendor not in g["model"].lower() and want_vendor != "nvidia":
            continue
        if want_vendor == "nvidia" and ("amd" in g["model"].lower() or "radeon" in g["model"].lower()):
            continue
        same = _norm_model(g["model"]) == want_model
        vram_ok = float(g["vram"] or 0) >= want_vram - 1   # Beetle 이 16 을 17 로 표기하는 경우가 있어 1 GiB 여유
        scored.append((i, g, same, vram_ok))
    if not scored:
        return None, "벤더가 맞고 GPU 가 있는 후보가 없다"
    if mode == "same-model-first":
        same = [x for x in scored if x[2] and x[3]]
        if same:
            i, g, _, _ = min(same, key=lambda x: (x[1]["cost"] or 9e9))
            return i, f"같은 GPU 모델({g['model']}) 중 가장 싼 {g['spec']} (VRAM {g['vram']}GB, {g['cost']}/h) — LLM 완전 일치의 전제"
    ok = [x for x in scored if x[3]]
    if not ok:
        return None, f"VRAM {want_vram}GB 이상인 후보가 없다"
    i, g, _, _ = min(ok, key=lambda x: (float(x[1]["vram"] or 0), x[1]["cost"] or 9e9))
    note = "같은 모델이 없어 " if mode == "same-model-first" else ""
    return i, f"{note}VRAM 이상 중 가장 가까운 {g['spec']} ({g['model']} {g['vram']}GB, {g['cost']}/h) — GPU 가 달라 테스트2 는 WARN"


def image_fits(image, want, driver_series=None):
    """추천 이미지가 정책과 맞는지 — (맞음, 이유). want 예 'ubuntu-24.04'.
    GPU 이미지(드라이버 내장)는 드라이버 시리즈까지 같아야 맞다 — 다르면 bootstrap 이 드라이버를 다시 깔지 않아
    소스와 버전이 갈린다(nvidia-smi 가 이미 동작하므로)."""
    if not image:
        return False, "추천 이미지 정보 없음"
    text = " ".join(str(image.get(k) or "") for k in ("osType", "osDistribution", "name", "cspImageName")).lower()
    distro, _, version = (want or "").lower().partition("-")
    if distro and distro not in text:
        return False, f"배포판이 {distro} 가 아니다"
    if version and version not in text:
        return False, f"버전 {version} 이 아니다"
    if image.get("isGPUImage"):
        if not driver_series:
            return True, "GPU 이미지 (드라이버 시리즈 지정 없음)"
        if re.search(rf"(nvidia[^0-9]*{re.escape(str(driver_series))}\b|-{re.escape(str(driver_series))}-)", text):
            return True, f"{distro} {version} + NVIDIA {driver_series} 내장 — 드라이버 설치 단계가 건너뛰어진다"
        return False, f"GPU 이미지의 드라이버가 {driver_series} 계열이 아니다"
    return True, f"순정 {distro} {version}"


def adjust_candidate(candidate, policy, image_override=None, open_ports=None, image_info=None):
    """보정본과 변경 목록. image_override 는 Tumblebug 이미지 ID — 정해지면 추천 이미지를 바꾼다.
    image_info 는 Tumblebug 의 ImageInfo(같은 모델) — 있으면 targetOsImageList 를 그것으로 바꾸고 nodeGroups 의 cspImageName 도 맞춘다."""
    c = copy.deepcopy(candidate)
    changes = []
    disk = int(policy.get("root_disk_gb") or 0)
    for ng in (c.get("targetInfra") or {}).get("nodeGroups") or (c.get("targetInfra") or {}).get("subGroups") or []:
        if disk and int(ng.get("rootDiskSize") or 0) < disk:
            changes.append(f"rootDiskSize {ng.get('rootDiskSize')} → {disk}")
            ng["rootDiskSize"] = disk
        if image_override and ng.get("imageId") != image_override:
            changes.append(f"imageId {ng.get('imageId')} → {image_override}")
            ng["imageId"] = image_override
            if image_info and image_info.get("cspImageName"):
                ng["cspImageName"] = image_info["cspImageName"]
    if image_override:
        c["targetOsImageList"] = [image_info] if image_info else [{"id": image_override}]
    for p in open_ports or []:
        rule = {"Ports": str(p["port"]), "Protocol": p.get("protocol", "tcp"), "Direction": "inbound", "CIDR": p["cidr"]}
        for sg in c.get("targetSecurityGroupList") or []:
            rules = sg.get("firewallRules") or []      # Beetle 은 규칙이 없으면 null 로 준다
            sg["firewallRules"] = rules
            if rule not in rules:
                rules.append(rule)
                changes.append(f"SG {sg.get('name', '')} 에 {rule['Ports']}/{rule['Protocol']} from {rule['CIDR']}")
    return c, changes
