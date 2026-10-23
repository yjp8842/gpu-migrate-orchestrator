"""소스 VM 안의 상태 확인 — Tumblebug 메타가 모르는 것만 읽는다.

  · 서비스 · LLM 설정이 지금 어떤가 (골든 응답의 전제: temperature · seed)
  · 식별 대조표의 소스 쪽: 유닛 · 환경변수 키(값 제외) · 설정 · 커밋 · 패키지 버전 · 이미지 태그 · 실행 파일 층 여부
  · 실제 GPU · 드라이버 (메타의 주문 사양과 대조용 증적)
패키지 생성 단계가 소스에 명령을 보낼 때 한 번 같이 읽는다 (각 항목을 command 원소 하나씩 — 출력이 인덱스로 맞는다).
"""
import re

from .stages._util import mib_to_gib, parse_json, parse_kv


def commands(adapter):
    ident = adapter.get("identify") or {}
    tree = ident.get("tree", "~")
    return [
        ("gpu", "nvidia-smi --query-gpu=index,uuid,name,driver_version,pci.bus_id,memory.total --format=csv,noheader,nounits 2>/dev/null || echo NOGPU"),
        ("cuda", "nvidia-smi 2>/dev/null | grep -o 'CUDA Version: [0-9.]*' | awk 'NR==1' || true"),
        ("os", "cat /etc/os-release"),
        ("kernel", "uname -r"),
        ("pkgs", "dpkg-query -W -f='${Package}=${Version}\\n' openjdk-17-jdk nginx opensearch python3.12 docker-ce nvidia-container-toolkit 2>/dev/null; "
                 "dpkg-query -W -f='${Package}=${Version}\\n' 'nvidia-driver-*' 2>/dev/null | grep -v '=$'; ollama -v 2>/dev/null | tail -1"),
        ("units", " ; ".join(f"systemctl show {u} -p Id,FragmentPath,ExecStart,User,EnvironmentFile,ActiveState,UnitFileState 2>/dev/null; echo ---"
                             for u in ident.get("units") or []) or "echo no-units"),
        ("env_keys", f"sudo grep -oE '^[A-Z_][A-Z0-9_]*=' {ident['env_file']} 2>/dev/null | sort || true" if ident.get("env_file") else "echo"),
        ("config", f"cat {ident['config']} 2>/dev/null || true" if ident.get("config") else "echo"),
        ("code_ref", f"git -C {tree} rev-parse --short HEAD 2>/dev/null || grep APP_CODE_REF {tree}/app-tree.env 2>/dev/null || echo unknown"),
        ("compose", " ; ".join(f"grep -E 'image:' {f} 2>/dev/null" for f in ident.get("compose") or []) or "echo"),
        ("health", f"curl -s --max-time 5 {adapter.get('health') or 'http://127.0.0.1:8000/health'} || echo '{{}}'"),
    ]


def _parse_gpu(text, cuda_text):
    cards = []
    if not text or "NOGPU" in text:
        return cards
    m = re.search(r"CUDA Version: ([0-9.]+)", cuda_text or "")
    for line in text.splitlines():
        parts = [p.strip() for p in line.split(",")]
        if len(parts) < 6:
            continue
        idx, uuid, name, drv, pci, mem = parts[:6]
        cards.append({"index": idx, "uuid": uuid, "model": name, "driverVersion": drv, "cudaVersion": m.group(1) if m else "",
                      "pciBusId": pci, "memoryTotalGB": mib_to_gib(mem) if mem.isdigit() else mem})
    return cards


def _parse_units(text):
    units = []
    for block in (text or "").split("---"):
        kv = parse_kv(block)
        if kv.get("Id"):
            units.append({"unit": kv.get("Id"), "fragmentPath": kv.get("FragmentPath"), "execStart": kv.get("ExecStart"),
                          "user": kv.get("User"), "environmentFile": kv.get("EnvironmentFile"),
                          "activeState": kv.get("ActiveState"), "unitFileState": kv.get("UnitFileState")})
    return units


def identify(adapter, out):
    """원격 명령 출력(name → stdout) → 식별 결과."""
    osr = parse_kv(out.get("os", ""))
    return {
        "form": adapter["form"], "workload": adapter["workload"],
        "gpu": _parse_gpu(out.get("gpu", ""), out.get("cuda", "")),
        "os": osr.get("PRETTY_NAME", ""), "kernel": out.get("kernel", "").strip(),
        "packages": [l for l in out.get("pkgs", "").splitlines() if l.strip()],
        "units": _parse_units(out.get("units", "")),
        "env_keys": [l.rstrip("=") for l in out.get("env_keys", "").split()],
        "config": out.get("config", ""),
        "code_ref": out.get("code_ref", "").strip().replace("APP_CODE_REF=", ""),
        "compose_images": [l.strip() for l in out.get("compose", "").splitlines() if "image:" in l],
        "services": adapter["services"],
        "health": parse_json(out.get("health", ""), {}) or {},
    }


def llm_mismatch(health, run_env):
    """(불일치?, 설명). 사례의 LLM_TEMPERATURE · LLM_SEED 와 소스의 /health 값을 비교한다."""
    temp, seed = str(health.get("llm_temperature", "")), str(health.get("llm_seed", ""))
    want_t, want_s = str(run_env.get("LLM_TEMPERATURE", "")), str(run_env.get("LLM_SEED", ""))
    def same_num(a, b):
        try:
            return float(a) == float(b)
        except ValueError:
            return a == b
    bad = (want_t and not same_num(temp, want_t)) or (want_s and seed != want_s)
    return bool(bad), f"소스 temperature {temp or '?'} · seed {seed or '없음'} / 사례 {want_t or '-'} · {want_s or '-'}"
