package stages

// 7. verify — 타겟의 결과표 · 비교표 · 증적 폴더를 원격 명령의 출력으로 받아 판정한다.
// 판정이 FAIL 이어도 cleanup 은 돈다 (종료 코드로 알린다).
// 판정과 무관하게 타겟의 서비스 로그 · 앱 로그 · GPU · 모델 상태(어댑터의 diagnostics)를 verify/target/ 에 받는다 — 테스트 이력.

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"gpu-migrate-orchestrator/internal/jsonx"
	"gpu-migrate-orchestrator/internal/judge"
	"gpu-migrate-orchestrator/internal/textx"
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

var diagName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// EvidenceDirScript 는 어댑터의 증적 경로(glob)에 걸리는 것 중 폴더만 골라 가장 최근(이름순 마지막) 하나를 찍는다.
// `~/widgetrag-run/migration-*` 는 증적 폴더 migration-<시각>/ 뿐 아니라 비교표 파일 migration-compare.csv 에도 걸린다.
func EvidenceDirScript(glob string) string {
	return fmt.Sprintf(`for d in %s; do [ -d "$d" ] && printf '%%s\n' "$d"; done | LC_ALL=C sort | tail -1`, glob)
}

// DiagnosticsScript 는 어댑터의 diagnostics 를 타겟의 dir 아래 파일 하나씩으로 모으는 스크립트다.
// 명령이 실패해도 다음 것으로 넘어간다. stderr 는 <이름>.stderr 에 두고, 비어 있으면 지운다.
func DiagnosticsScript(items []M, dir string) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "d=%s; mkdir -p \"$d\"\n", dir)
	for _, it := range items {
		name, cmd := jsonx.S(it, "name"), strings.TrimSpace(jsonx.S(it, "command"))
		if !diagName.MatchString(name) || cmd == "" {
			return "", Errf("어댑터 diagnostics 항목이 잘못됐습니다 (name 은 파일 이름 하나, command 는 비면 안 된다): %v", it)
		}
		fmt.Fprintf(&b, "( %s ) > \"$d/%s\" 2> \"$d/%s.stderr\" < /dev/null || true\n", cmd, name, name)
	}
	b.WriteString("find \"$d\" -name '*.stderr' -size 0 -delete; ls \"$d\"")
	return b.String(), nil
}

// collectDiagnostics 는 타겟 상태를 verify/target/ 으로 받는다. 실패해도 판정은 바꾸지 않고 경고만 남긴다.
func collectDiagnostics(ctx *Context) {
	items := jsonx.ListM(ctx.Adapter.Raw["diagnostics"])
	if len(items) == 0 {
		return
	}
	name := "gmp-diag-" + ctx.State.Data.RunID
	script, err := DiagnosticsScript(items, `"$HOME/`+name+`"`)
	if err != nil {
		ctx.Log.Warn(stVerify, "타겟 상태 회수 건너뜀: %v", err)
		return
	}
	if r, err := ctx.Exec("target", script, 10); err != nil || !r.Reachable || r.RC != 0 {
		ctx.Log.Warn(stVerify, "타겟 상태 수집 명령 실패: %s", errOr(err, textx.Tail(strings.TrimSpace(r.Detail+" "+r.Stderr), 200)))
		return
	}
	local := filepath.Join(ctx.State.StageDir(stVerify), "target")
	if ok, err := ctx.FetchDir("target", "~/"+name, local); err != nil || !ok {
		ctx.Log.Warn(stVerify, "타겟 상태 회수 실패: %s", errOr(err, "폴더 없음"))
		return
	}
	ctx.State.Artifact(stVerify, "target", local)
	ctx.Log.Info(stVerify, "타겟 상태 회수: %d개 항목 → verify/target/ (타겟에는 ~/%s 로 남는다)", len(items), name)
}

func runVerify(ctx *Context) (M, error) {
	res := ctx.Adapter.M("results")
	if ctx.DryRun {
		if items := jsonx.ListM(ctx.Adapter.Raw["diagnostics"]); len(items) > 0 {
			script, err := DiagnosticsScript(items, `"$HOME/gmp-diag-`+ctx.State.Data.RunID+`"`)
			if err != nil {
				return nil, err
			}
			ctx.Dry(stVerify, "Tumblebug cmd_sync(타겟) 상태 수집 → verify/target/", M{"command": []string{Wrap(script)}})
		}
		v := M{"result": "DRY-RUN", "summary": "타겟이 없어 판정하지 않음"}
		ctx.WriteJSON(stVerify, "verdict.json", v)
		ctx.State.SetVerdict(v)
		return v, nil
	}
	if jsonx.S(res, "csv") == "" { // 결과표가 없는 워크로드 — 상태 파일(results.status.require)만으로 판정
		return verifyStatusOnly(ctx, jsonx.AsM(res["status"]))
	}
	resultsText, found, err := ctx.ReadText("target", jsonx.S(res, "csv"))
	if err != nil {
		return nil, err
	}
	// 결과표를 켜야 생기는 워크로드(LexAI 는 이관 때 RUN_VERIFY=1)에서 결과표가 없으면, 예전처럼 상태 파일로만 판정한다 —
	// 결과표가 생기기 전에 만든 사례를 다시 돌려도 NO-RESULT 로 떨어지지 않게. 그렇다고 판정 근거가 줄었다는 것은 남긴다.
	if !found && res["status"] != nil {
		note := fmt.Sprintf("결과표 %s 가 없어 상태 파일로만 판정했다 — 결과표 · 비교표까지 판정하려면 사례 run.env 에 RUN_VERIFY=1", jsonx.S(res, "csv"))
		ctx.Log.Warn(stVerify, "%s", note)
		return verifyStatusOnly(ctx, jsonx.AsM(res["status"]), note)
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
func verifyStatusOnly(ctx *Context, st M, notes ...string) (M, error) {
	path := jsonx.S(st, "path")
	warn := []any{}
	for _, n := range notes {
		warn = append(warn, n)
	}
	v := M{"result": "PASS", "summary": "상태 파일 " + path, "metrics": M{}, "warn": warn}
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
	if r, err := ctx.Exec("target", EvidenceDirScript(ev), 2); err == nil && r.Reachable {
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

// finishVerdict 는 판정을 증적 · 상태에 남기고 로그로 알린다. 그 전에 타겟 상태(어댑터 diagnostics)를 받는다 —
// 판정이 PASS 든 FAIL · NO-RESULT 든 받고, 받지 못해도 판정은 바꾸지 않는다.
func finishVerdict(ctx *Context, v M) M {
	collectDiagnostics(ctx)
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
