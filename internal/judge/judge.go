// Package judge 는 판정이다 — 타겟 verify 가 남긴 results.csv 의 새 행과 같은 timestamp 의 migration-compare.csv 행으로
// PASS · FAIL · NO-RESULT 를 정한다. 판정 기준은 워크로드 쪽에 있고 여기서는 집계만 한다.
package judge

import (
	"encoding/csv"
	"fmt"
	"strconv"
	"strings"

	"gpu-migrate-orchestrator/internal/jsonx"
)

// ParseCSV 는 머리줄을 키로 쓴 행 목록이다.
func ParseCSV(text string) []map[string]string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	r := csv.NewReader(strings.NewReader(text))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	recs, err := r.ReadAll()
	if err != nil || len(recs) == 0 {
		return nil
	}
	head := recs[0]
	var rows []map[string]string
	for _, rec := range recs[1:] {
		row := map[string]string{}
		for i, h := range head {
			if i < len(rec) {
				row[h] = rec[i]
			}
		}
		rows = append(rows, row)
	}
	return rows
}

func toAny(rows []map[string]string) []any {
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		m := jsonx.M{}
		for k, v := range r {
			m[k] = v
		}
		out = append(out, m)
	}
	return out
}

// Evaluate 는 판정 결과(result · summary · metrics · compare · warn)를 돌려준다.
func Evaluate(resultsText string, baseline int, compareText string, requireCompare bool) jsonx.M {
	rows := ParseCSV(resultsText)
	var newRows []map[string]string
	if len(rows) > baseline {
		newRows = rows[baseline:]
	}
	if len(newRows) == 0 {
		return jsonx.M{"result": "NO-RESULT", "summary": "검증 결과표에 새 행이 없다 (verify 가 돌지 않았다)", "rows": []any{}}
	}
	row := newRows[len(newRows)-1]
	fails, _ := strconv.Atoi(strings.TrimSpace(row["fail"]))
	result := "FAIL"
	if row["result"] == "PASS" && fails == 0 {
		result = "PASS"
	}
	metrics := jsonx.M{}
	for _, k := range []string{"pass", "fail", "cold_sec", "p50_sec", "p95_sec", "sources", "os_docs", "db_msgs"} {
		if v, ok := row[k]; ok {
			metrics[k] = v
		} else {
			metrics[k] = nil
		}
	}
	warn := []any{}
	verdict := jsonx.M{"timestamp": row["timestamp"], "form": row["form"], "metrics": metrics, "rows": toAny(newRows), "compare": nil}
	var cmpRows []map[string]string
	for _, r := range ParseCSV(compareText) {
		if r["timestamp"] == row["timestamp"] {
			cmpRows = append(cmpRows, r)
		}
	}
	var byStr string
	if len(cmpRows) > 0 {
		by := jsonx.M{}
		order := []string{}
		var failItems, warnItems []any
		for _, r := range cmpRows {
			k := r["result"]
			if _, ok := by[k]; !ok {
				order = append(order, k)
				by[k] = 0
			}
			by[k] = by[k].(int) + 1
			item := fmt.Sprintf("%s %s: %s", r["test"], r["item"], r["note"])
			if k == "FAIL" && len(failItems) < 30 {
				failItems = append(failItems, item)
			}
			if k == "WARN" && len(warnItems) < 30 {
				warnItems = append(warnItems, item)
			}
		}
		if failItems == nil {
			failItems = []any{}
		}
		if warnItems == nil {
			warnItems = []any{}
		}
		verdict["compare"] = jsonx.M{"rows": len(cmpRows), "by_result": by, "fail_items": failItems, "warn_items": warnItems}
		warn = warnItems
		if jsonx.Truthy(by["FAIL"]) {
			result = "FAIL"
		}
		var parts []string
		for _, k := range order {
			parts = append(parts, fmt.Sprintf("'%s': %d", k, by[k]))
		}
		byStr = "{" + strings.Join(parts, ", ") + "}"
	} else if requireCompare {
		result = "FAIL"
		warn = append(warn, "COMPARE=1 인데 이번 회차의 비교 행이 없다")
	}
	verdict["result"] = result
	verdict["warn"] = warn
	summary := fmt.Sprintf("검증 %s PASS / %s FAIL", row["pass"], row["fail"])
	if byStr != "" {
		summary += " · 비교 " + byStr
	} else {
		summary += " · 비교 행 없음"
	}
	if len(warn) > 0 {
		summary += fmt.Sprintf(" · WARN %d건", len(warn))
	}
	verdict["summary"] = summary
	return verdict
}
