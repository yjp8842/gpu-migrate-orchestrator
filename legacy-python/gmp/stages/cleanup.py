"""8. cleanup — 타겟 정리(suspend | terminate | keep), 타겟 키 파기, summary.md."""
from . import summary
from ._util import StageError

STAGE = "cleanup"


def preflight(ctx):
    return


def run(ctx):
    tgt = ctx.case.target
    action = ctx.case.cleanup
    infra_id = ctx.state.get_key("target_infra_id")
    if ctx.dry_run:
        ctx.dry(STAGE, f"cleanup={action}", {"infra": infra_id or tgt["name"]})
        summary.write(ctx)
        return {"dry_run": True}
    tb = ctx.tb_target
    done = None
    if action == "keep" or not infra_id:
        ctx.log.info(f"타겟 유지 ({infra_id or '인프라 없음'})", STAGE)
        done = "keep"
    elif action == "suspend":
        tb.control(tgt["ns"], infra_id, "suspend")
        ctx.log.info(f"타겟 {infra_id} suspend 요청 — 공인 IP 는 다시 켤 때 바뀐다", STAGE)
        done = "suspend"
    elif action == "terminate":
        infra = tb.get_infra(tgt["ns"], infra_id)
        label = (infra or {}).get("label") or {}
        if label.get("gmp.case") != ctx.case.name and not ctx.state.get_key("beetle_migrate_started"):
            raise StageError(f"{infra_id} 에 이 도구의 라벨(gmp.case={ctx.case.name})이 없어 terminate 하지 않습니다 — 손으로 지우세요")
        tb.delete_infra(tgt["ns"], infra_id, option="terminate")
        ctx.log.info(f"타겟 {infra_id} terminate 요청", STAGE)
        done = "terminate"
    if not ctx.keep_keys:
        ctx.keys.shred("target")
        ctx.log.info("타겟 키 파기 (--keep-keys 로 남길 수 있다)", STAGE)
    summary.write(ctx)
    return {"action": done}
