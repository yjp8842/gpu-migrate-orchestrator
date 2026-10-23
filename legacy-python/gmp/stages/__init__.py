"""단계 모듈 — 각 모듈은 preflight(ctx) 와 run(ctx) 를 가진다. preflight 는 부작용 없이 입력 · 선행 산출물 · 자원 상태를 확인한다."""
import importlib

from ..config import STAGES


def get(name):
    if name not in STAGES:
        raise KeyError(name)
    return importlib.import_module(f"gmp.stages.{name}")
