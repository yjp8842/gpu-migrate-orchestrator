"""7. verify — 타겟의 결과표 · 비교표 · 증적 디렉토리를 가져와 판정한다. 판정이 FAIL 이어도 cleanup 은 돈다 (종료 코드로 알린다)."""
from .. import judge
from ._util import StageError

STAGE = "verify"


def preflight(ctx):
    if ctx.dry_run:
        return
    ctx.node("target"); ctx.key_path("target")


def run(ctx):
    ad = ctx.adapter
    if ctx.dry_run:
        verdict = {"result": "DRY-RUN", "summary": "호출 없음"}
        ctx.write_json(STAGE, "verdict.json", verdict)
        ctx.state.set_verdict(verdict)
        return verdict
    dst = ctx.ssh("target")
    d = ctx.state.stage_dir(STAGE)
    results_text = dst.fetch_text(ad["results"]["csv"]) or ""
    compare_text = dst.fetch_text(ad["results"]["compare"]) or ""
    ctx.write_text(STAGE, "results.csv", results_text or "")
    if compare_text:
        ctx.write_text(STAGE, "migration-compare.csv", compare_text)
    rc, out, _ = dst.run(f"ls -1d {ad['results']['evidence']} 2>/dev/null | tail -1")
    latest = out.strip().splitlines()[-1] if out.strip() else ""
    if latest:
        try:
            dst.scp_from(latest, d / "evidence", timeout=600, recursive=True)
            ctx.state.artifact(STAGE, "evidence", d / "evidence")
        except Exception as e:                       # noqa: BLE001
            ctx.log.warn(f"증적 디렉토리 회수 실패: {e}", STAGE)
    baseline = int(ctx.state.get_key("results_baseline_rows") or 0)
    require_compare = str(ctx.case.run["env"].get("COMPARE", "0")) == "1"
    verdict = judge.evaluate(results_text, baseline, compare_text, require_compare=require_compare)
    verdict["evidence_dir"] = latest
    ctx.write_json(STAGE, "verdict.json", verdict)
    ctx.state.set_verdict(verdict)
    level = ctx.log.info if verdict["result"] == "PASS" else ctx.log.warn
    level(f"판정 {verdict['result']} — {verdict.get('summary')}", STAGE)
    for w in verdict.get("warn") or []:
        ctx.log.warn(f"  WARN {w}", STAGE)
    for f in (verdict.get("compare") or {}).get("fail_items") or []:
        ctx.log.warn(f"  FAIL {f}", STAGE)
    return verdict
