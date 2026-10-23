package judge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpu-migrate-orchestrator/internal/jsonx"
)

func fix(t *testing.T, name string) string {
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestNewRowPassWithWarn(t *testing.T) {
	v := Evaluate(fix(t, "results.csv"), 1, fix(t, "migration-compare.csv"), true)
	if v["result"] != "PASS" || v["timestamp"] != "2026-10-22 02:10:31" {
		t.Fatal(v)
	}
	by := jsonx.AsM(jsonx.Get(v, "compare", "by_result"))
	if by["PASS"] != 4 {
		t.Fatal(by)
	}
	warn := jsonx.Strings(v["warn"])
	if len(warn) != 1 || !strings.Contains(warn[0], "runtime:gpu") {
		t.Fatal(warn)
	}
}

func TestNoNewRow(t *testing.T) {
	if v := Evaluate(fix(t, "results.csv"), 2, fix(t, "migration-compare.csv"), false); v["result"] != "NO-RESULT" {
		t.Fatal(v)
	}
}

func TestCompareFailFlipsVerdict(t *testing.T) {
	bad := fix(t, "migration-compare.csv") + "2026-10-22 02:10:31,f,shell-native,p,T1-3,db:company,3,2,FAIL,값 다름\n"
	v := Evaluate(fix(t, "results.csv"), 1, bad, false)
	if v["result"] != "FAIL" || len(jsonx.AsList(jsonx.Get(v, "compare", "fail_items"))) != 1 {
		t.Fatal(v)
	}
}

func TestRequireCompareWithoutRows(t *testing.T) {
	if v := Evaluate(fix(t, "results.csv"), 1, "", true); v["result"] != "FAIL" {
		t.Fatal(v)
	}
}

func TestEvaluateColumns(t *testing.T) {
	cols := Columns{Result: "outcome", Pass: "OK", Fail: "errors", Timestamp: "run_at"}
	v := EvaluateColumns("run_at,outcome,errors\n1,OK,0\n2,OK,3\n", 1, "", false, cols)
	if v["result"] != "FAIL" || v["timestamp"] != "2" {
		t.Fatal("오류 열이 0 이 아닌데 PASS", v)
	}
	v = EvaluateColumns("run_at,outcome\n1,OK\n", 0, "", false, Columns{Result: "outcome", Pass: "OK", Timestamp: "run_at"})
	if v["result"] != "PASS" {
		t.Fatal("오류 열이 없는 결과표를 PASS 로 보지 않았다", v)
	}
}
