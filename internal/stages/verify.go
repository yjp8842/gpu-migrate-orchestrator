package stages

// 7. verify — 타겟의 결과표 · 비교표 · 증적 폴더를 원격 명령의 출력으로 받아 판정한다.
// 판정이 FAIL 이어도 cleanup 은 돈다 (종료 코드로 알린다).

import (
	"fmt"
	"path/filepath"
	"strings"

	"gpu-migrate-orchestrator/internal/jsonx"
	"gpu-migrate-orchestrator/internal/judge"
)

const stVerify = "verify"

func init() { register(stVerify, Stage{Preflight: preVerify, Run: runVerify}) }

func preVerify(ctx *Context) error {
	if ctx.DryRun {
		return nil
	}
	_, err := ctx.Node("target")
	return err
}

func runVerify(ctx *Context) (M, error) {
	res := ctx.Adapter.M("results")
	if ctx.DryRun {
		v := M{"result": "DRY-RUN", "summary": "타겟이 없어 판정하지 않음"}
		ctx.WriteJSON(stVerify, "verdict.json", v)
		ctx.State.SetVerdict(v)
		return v, nil
	}
	if jsonx.S(res, "csv") == "" { // 결과표가 없는 워크로드 — 상태 파일(results.status.require)만으로 판정
		return verifyStatusOnly(ctx, jsonx.AsM(res["status"]))
	}
	resultsText, _, err := ctx.ReadText("target", jsonx.S(res, "csv"))
	if err != nil {
		return nil, err
	}
	compareText := ""
	if path := jsonx.S(res, "compare"); path != "" { // 골든 비교표가 있는 워크로드만
		if compareText, _, err = ctx.ReadText("target", path); err != nil {
			return nil, err
		}
	}
	ctx.WriteText(stVerify, "results.csv", resultsText)
	if compareText != "" {
		ctx.WriteText(stVerify, "migration-compare.csv", compareText)
	}
	latest := fetchEvidence(ctx, res)
	baseline := jsonx.Int(ctx.State.Key("results_baseline_rows"))
	c := ctx.Adapter.ResultColumns()
	v := judge.EvaluateColumns(resultsText, baseline, compareText, ctx.Case.EnvValue("COMPARE") == "1",
		judge.Columns{Result: c.Result, Pass: c.Pass, Fail: c.Fail, Timestamp: c.Timestamp})
	v["evidence_dir"] = latest
	if st := jsonx.AsM(res["status"]); st != nil {
		applyStatusRequire(ctx, v, st)
	}
	return finishVerdict(ctx, v), nil
}

// verifyStatusOnly 는 상태 파일의 require 만으로 판정한다. 파일을 읽지 못하면 NO-RESULT (워크로드가 돌지 않았다).
func verifyStatusOnly(ctx *Context, st M) (M, error) {
	path := jsonx.S(st, "path")
	v := M{"result": "PASS", "summary": "상태 파일 " + path, "metrics": M{}, "warn": []any{}}
	if _, ok, err := ctx.ReadText("target", path); err != nil || !ok {
		v = M{"result": "NO-RESULT", "summary": fmt.Sprintf("상태 파일 %s 를 읽지 못함 — 워크로드가 돌지 않았다", path), "warn": []any{}}
		return finishVerdict(ctx, v), nil
	}
	applyStatusRequire(ctx, v, st)
	v["evidence_dir"] = fetchEvidence(ctx, ctx.Adapter.M("results"))
	ctx.State.SetKey("status_only", true)
	return finishVerdict(ctx, v), nil
}

// fetchEvidence 는 어댑터 results.evidence(글롭)의 마지막 폴더를 run 폴더로 받는다 (없으면 "").
func fetchEvidence(ctx *Context, res M) string {
	ev := jsonx.S(res, "evidence")
	if ev == "" { // 증적 폴더가 있는 워크로드만 (비우면 ls 가 현재 폴더를 읽는다)
		return ""
	}
	latest := ""
	if r, err := ctx.Exec("target", "ls -1d "+ev+" 2>/dev/null | tail -1", 2); err == nil && r.Reachable {
		latest = strings.TrimSpace(r.Stdout)
	}
	if latest == "" {
		return ""
	}
	d := ctx.State.StageDir(stVerify)
	if ok, err := ctx.FetchDir("target", latest, filepath.Join(d, "evidence")); err != nil || !ok {
		ctx.Log.Warn(stVerify, "증적 폴더 회수 실패: %s", errOr(err, "폴더 없음"))
	} else {
		ctx.State.Artifact(stVerify, "evidence", filepath.Join(d, "evidence"))
	}
	return latest
}

// finishVerdict 는 판정을 증적 · 상태에 남기고 로그로 알린다.
func finishVerdict(ctx *Context, v M) M {
	ctx.WriteJSON(stVerify, "verdict.json", v)
	ctx.State.SetVerdict(v)
	if jsonx.S(v, "result") == "PASS" {
		ctx.Log.Info(stVerify, "판정 %s — %s", jsonx.S(v, "result"), jsonx.S(v, "summary"))
	} else {
		ctx.Log.Warn(stVerify, "판정 %s — %s", jsonx.S(v, "result"), jsonx.S(v, "summary"))
	}
	for _, w := range jsonx.Strings(v["warn"]) {
		ctx.Log.Warn(stVerify, "  WARN %s", w)
	}
	for _, f := range jsonx.Strings(jsonx.Get(v, "compare", "fail_items")) {
		ctx.Log.Warn(stVerify, "  FAIL %s", f)
	}
	return v
}

// applyStatusRequire 는 어댑터 results.status 로 워크로드 상태 파일의 필드를 판정에 더한다 — 결과표만으로는 드러나지 않는
// 판정(예: 데이터 보존 비교 data_check)을 워크로드가 상태 파일에 남기는 경우. require 의 필드 값이 허용 목록에 없으면 FAIL.
//
//	results: { status: { path: ~/x-run/status.json, require: { data_check: [PASS], state: [success] } } }
func applyStatusRequire(ctx *Context, v M, st M) {
	path := jsonx.S(st, "path")
	text, ok, err := ctx.ReadText("target", path)
	fail := func(msg string) {
		v["result"] = "FAIL"
		items := jsonx.AsList(v["status_fail"])
		v["status_fail"] = append(items, msg)
		v["summary"] = jsonx.S(v, "summary") + " · 상태 " + msg
	}
	if err != nil || !ok {
		fail(fmt.Sprintf("%s 를 읽지 못함", path))
		return
	}
	ctx.WriteText(stVerify, filepath.Base(path), text)
	parsed, err := jsonx.Decode([]byte(text))
	data := jsonx.AsM(parsed)
	if err != nil || data == nil {
		fail(fmt.Sprintf("%s 가 JSON 이 아님", path))
		return
	}
	req := jsonx.AsM(st["require"])
	for _, k := range jsonx.SortedKeys(req) {
		got := jsonx.Str(data[k])
		allowed := jsonx.Strings(req[k])
		if !contains(allowed, got) {
			fail(fmt.Sprintf("%s=%s (허용 %s)", k, got, strings.Join(allowed, "|")))
		}
	}
	v["status"] = data
}
