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

// Columns 는 결과표에서 판정에 쓰는 열 이름이다 (워크로드마다 다르면 어댑터 results.columns 로 준다).
type Columns struct{ Result, Pass, Fail, Timestamp string }

// DefaultColumns 는 result=PASS · fail=0 · timestamp.
var DefaultColumns = Columns{Result: "result", Pass: "PASS", Fail: "fail", Timestamp: "timestamp"}

// Evaluate 는 기본 열 이름으로 판정한다.
func Evaluate(resultsText string, baseline int, compareText string, requireCompare bool) jsonx.M {
	return EvaluateColumns(resultsText, baseline, compareText, requireCompare, DefaultColumns)
}

// EvaluateColumns 는 판정 결과(result · summary · metrics · compare · warn)를 돌려준다.
// 이번 회차(baseline 뒤의 마지막 행)의 Result 열이 Pass 값이고 Fail 열(있으면)이 0 이면 PASS.
func EvaluateColumns(resultsText string, baseline int, compareText string, requireCompare bool, cols Columns) jsonx.M {
	rows := ParseCSV(resultsText)
	var newRows []map[string]string
	if len(rows) > baseline {
		newRows = rows[baseline:]
	}
	if len(newRows) == 0 {
		return jsonx.M{"result": "NO-RESULT", "summary": "검증 결과표에 새 행이 없다 (verify 가 돌지 않았다)", "rows": []any{}}
	}
	row := newRows[len(newRows)-1]
	fails := 0
	if v, ok := row[cols.Fail]; ok && cols.Fail != "" {
		fails, _ = strconv.Atoi(strings.TrimSpace(v))
	}
	result := "FAIL"
	if strings.TrimSpace(row[cols.Result]) == cols.Pass && fails == 0 {
		result = "PASS"
	}
	metrics := jsonx.M{} // 이번 회차 행의 판정 열 밖 값 전부 (워크로드가 남긴 수치)
	for k, v := range row {
		if k != cols.Result && k != cols.Timestamp {
			metrics[k] = v
		}
	}
	warn := []any{}
	verdict := jsonx.M{"timestamp": row[cols.Timestamp], "form": row["form"], "metrics": metrics, "rows": toAny(newRows), "compare": nil}
	var cmpRows []map[string]string
	for _, r := range ParseCSV(compareText) {
		if r[cols.Timestamp] == row[cols.Timestamp] {
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
	summary := fmt.Sprintf("검증 %s=%s", cols.Result, row[cols.Result])
	if p, ok := row["pass"]; ok {
		summary = fmt.Sprintf("검증 %s PASS / %s FAIL", p, row[cols.Fail])
	}
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
