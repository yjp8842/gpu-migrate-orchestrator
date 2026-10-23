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
	if r, err := ctx.Exec("target", EvidenceDirScript(jsonx.S(res, "evidence")), 2); err == nil && r.Reachable {
		latest = strings.TrimSpace(r.Stdout)
	}
	if latest != "" {
		if ok, err := ctx.FetchDir("target", latest, filepath.Join(d, "evidence")); err != nil || !ok {
			ctx.Log.Warn(stVerify, "증적 폴더 회수 실패: %s", errOr(err, "폴더 없음"))
		} else {
			ctx.State.Artifact(stVerify, "evidence", filepath.Join(d, "evidence"))
		}
	}
	collectDiagnostics(ctx)
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
