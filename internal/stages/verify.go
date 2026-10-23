package stages

// 7. verify — 타겟의 결과표 · 비교표 · 증적 폴더를 원격 명령의 출력으로 받아 판정한다.
// 판정이 FAIL 이어도 cleanup 은 돈다 (종료 코드로 알린다).

import (
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
	d := ctx.State.StageDir(stVerify)
	resultsText, _, err := ctx.ReadText("target", jsonx.S(res, "csv"))
	if err != nil {
		return nil, err
	}
	compareText, _, err := ctx.ReadText("target", jsonx.S(res, "compare"))
	if err != nil {
		return nil, err
	}
	ctx.WriteText(stVerify, "results.csv", resultsText)
	if compareText != "" {
		ctx.WriteText(stVerify, "migration-compare.csv", compareText)
	}
	latest := ""
	if r, err := ctx.Exec("target", "ls -1d "+jsonx.S(res, "evidence")+" 2>/dev/null | tail -1", 2); err == nil && r.Reachable {
		latest = strings.TrimSpace(r.Stdout)
	}
	if latest != "" {
		if ok, err := ctx.FetchDir("target", latest, filepath.Join(d, "evidence")); err != nil || !ok {
			ctx.Log.Warn(stVerify, "증적 폴더 회수 실패: %s", errOr(err, "폴더 없음"))
		} else {
			ctx.State.Artifact(stVerify, "evidence", filepath.Join(d, "evidence"))
		}
	}
	baseline := jsonx.Int(ctx.State.Key("results_baseline_rows"))
	v := judge.Evaluate(resultsText, baseline, compareText, ctx.Case.EnvValue("COMPARE") == "1")
	v["evidence_dir"] = latest
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
	return v, nil
}
