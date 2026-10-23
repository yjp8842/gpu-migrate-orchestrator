"""단계들이 같이 쓰는 작은 도구."""
import json
import re
import time

from ..clients.tumblebug import TBError


class StageError(Exception):
    pass


def tick_logger(log, stage, every=4):
    """폴링 콜백 — 매번 찍으면 시끄러워 every 번에 한 번만."""
    n = {"i": 0}

    def _tick(*args):
        n["i"] += 1
        if n["i"] % every == 1:
            log.info("… " + " · ".join(str(a) for a in args if a not in (None, "", [], {})), stage)
    return _tick


def parse_kv(text):
    out = {}
    for line in (text or "").splitlines():
        if "=" in line and not line.startswith("#"):
            k, v = line.split("=", 1)
            out[k.strip()] = v.strip().strip('"')
    return out


def parse_json(text, default=None):
    try:
        return json.loads(text)
    except (TypeError, ValueError):
        return default


def gib(value_bytes):
    return round(int(value_bytes) / (1024 ** 3), 1)


def mib_to_gib(mib):
    """nvidia-smi 의 MiB → GiB 반올림 (15360MiB → 15 → 소스 T4 는 16 으로 적는 것이 Beetle 가이드의 권고라 올림 보정)"""
    g = int(mib) / 1024
    return int(g + 0.5) if g - int(g) < 0.5 else int(g) + 1


def require_running(tb, ns, infra_id, what):
    infra = tb.get_infra(ns, infra_id)
    if infra is None:
        raise StageError(f"{what} 인프라가 없습니다: {ns}/{infra_id}")
    state = tb.infra_state(infra)
    if state != "Running":
        raise StageError(f"{what} 인프라 {infra_id} 가 Running 이 아닙니다 (지금 {state}) — MapUI 나 control API 로 켜 주세요")
    return infra


def cmd_result_ok(entry):
    """commandStatus 항목이 성공인지 (Completed = 종료 0)."""
    return (entry or {}).get("status") == "Completed"


def describe_cmd(entry):
    e = entry or {}
    return f"{e.get('status')} — {e.get('resultSummary') or e.get('errorMessage') or ''}".strip(" —")
