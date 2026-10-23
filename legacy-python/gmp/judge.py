"""판정 — 타겟 verify 가 남긴 results.csv 의 새 행과 같은 timestamp 의 migration-compare.csv 행으로 PASS/FAIL 을 정한다."""
import csv
import io


def parse_csv(text):
    if not text or not text.strip():
        return []
    return list(csv.DictReader(io.StringIO(text)))


def new_result_rows(results_text, baseline_rows):
    rows = parse_csv(results_text)
    return rows[baseline_rows:] if len(rows) > baseline_rows else []


def evaluate(results_text, baseline_rows, compare_text=None, require_compare=False):
    """verdict dict: result PASS|FAIL|NO-RESULT, summary, metrics, compare 집계, warn 목록."""
    new_rows = new_result_rows(results_text, baseline_rows)
    if not new_rows:
        return {"result": "NO-RESULT", "summary": "검증 결과표에 새 행이 없다 (verify 가 돌지 않았다)", "rows": []}
    row = new_rows[-1]
    fails = int(row.get("fail") or 0)
    verdict = {"result": "PASS" if row.get("result") == "PASS" and fails == 0 else "FAIL",
               "timestamp": row.get("timestamp"), "form": row.get("form"),
               "metrics": {k: row.get(k) for k in ("pass", "fail", "cold_sec", "p50_sec", "p95_sec", "sources", "os_docs", "db_msgs")},
               "rows": new_rows, "compare": None, "warn": []}
    cmp_rows = [r for r in parse_csv(compare_text) if r.get("timestamp") == row.get("timestamp")] if compare_text else []
    if cmp_rows:
        by = {}
        for r in cmp_rows:
            by[r.get("result")] = by.get(r.get("result"), 0) + 1
        verdict["compare"] = {"rows": len(cmp_rows), "by_result": by,
                              "fail_items": [f"{r.get('test')} {r.get('item')}: {r.get('note')}" for r in cmp_rows if r.get("result") == "FAIL"][:30],
                              "warn_items": [f"{r.get('test')} {r.get('item')}: {r.get('note')}" for r in cmp_rows if r.get("result") == "WARN"][:30]}
        verdict["warn"] = verdict["compare"]["warn_items"]
        if by.get("FAIL"):
            verdict["result"] = "FAIL"
    elif require_compare:
        verdict["result"] = "FAIL"
        verdict["warn"].append("COMPARE=1 인데 이번 회차의 비교 행이 없다")
    verdict["summary"] = (f"검증 {row.get('pass')} PASS / {row.get('fail')} FAIL"
                          + (f" · 비교 {verdict['compare']['by_result']}" if verdict["compare"] else " · 비교 행 없음")
                          + (f" · WARN {len(verdict['warn'])}건" if verdict["warn"] else ""))
    return verdict
