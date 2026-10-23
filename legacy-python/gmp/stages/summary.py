"""run/summary.md — 단계 시각 · 선택 근거 · 판정 · 사람이 개입한 기록을 한 장으로."""
from ..config import STAGES


def write(ctx):
    st = ctx.state.data
    lines = [f"# {st['case']} — run {st['run_id']}" + (" (DRY-RUN)" if st.get("dry_run") else ""), "",
             f"- 워크로드: {st['workload']} · 소스 {ctx.case.source['ns']}/{ctx.case.source['infra']} → 타겟 {ctx.case.target['csp']}/{ctx.case.target['region']} ({ctx.case.target['provider']})",
             f"- 생성: {st['created']} · 갱신: {st['updated']}", ""]
    lines += ["## 단계", "", "| 단계 | 상태 | 시작 | 끝 | 비고 |", "| --- | --- | --- | --- | --- |"]
    for s in STAGES:
        d = st["stages"][s]
        lines.append(f"| {s} | {d['status']} | {d.get('started', '')[:19]} | {d.get('ended', '')[:19]} | {(d.get('error') or '')[:80]} |")
    v = st.get("verdict")
    lines += ["", "## 판정", "", f"- {v.get('result')} — {v.get('summary', '')}" if v else "- (없음)"]
    if v and v.get("metrics"):
        lines.append("- 지표: " + " · ".join(f"{k}={val}" for k, val in v["metrics"].items() if val not in (None, "")))
    for w in (v or {}).get("warn") or []:
        lines.append(f"- WARN {w}")
    for f in ((v or {}).get("compare") or {}).get("fail_items") or []:
        lines.append(f"- FAIL {f}")
    keys = st.get("keys") or {}
    lines += ["", "## 자원 · 키", ""]
    for k in ("target_infra_id", "backup_out", "sg_rules_added", "xreq_backup", "xreq_rebuild", "start_outcome", "beetle_data_req"):
        if k in keys:
            lines.append(f"- {k}: {keys[k]}")
    rat = ctx.run_dir / "recommend" / "rationale.md"
    if rat.is_file():
        lines += ["", "## 추천 · 선택 근거", ""] + [l for l in rat.read_text(encoding="utf-8").splitlines() if not l.startswith("# ")]
    lines += ["", "## 사람이 개입한 지점 (수작업 유발 구간)", ""]
    if st["interventions"]:
        for n in st["interventions"]:
            lines.append(f"- {n['at'][:19]} [{n.get('stage') or '-'}] {n['text']}")
    else:
        lines.append("- 없음")
    path = ctx.run_dir / "summary.md"
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")
    return path
