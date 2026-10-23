package stages

// run/summary.md — 단계 시각 · 선택 근거 · 판정 · 사람이 개입한 기록을 한 장으로.
// state.json 만으로 다시 만들 수 있다. 단계 실행이 끝날 때마다(성공 · 실패 모두), note 를 남길 때마다, migrate summary 로 다시 쓴다.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gpu-migrate-orchestrator/internal/config"
	"gpu-migrate-orchestrator/internal/jsonx"
	"gpu-migrate-orchestrator/internal/state"
	"gpu-migrate-orchestrator/internal/textx"
)

// WriteSummary 는 run 폴더에 summary.md 를 쓴다.
func WriteSummary(ctx *Context) error { return Summarize(ctx.Case, ctx.State) }

// Summarize 는 사례와 run 상태만으로 summary.md 를 다시 쓴다.
func Summarize(c *config.Case, rs *state.RunState) error {
	st := rs.Data
	src, tgt := c.Source(), c.Target()
	head := fmt.Sprintf("# %s — run %s", st.Case, st.RunID)
	if st.DryRun {
		head += " (DRY-RUN)"
	}
	lines := []string{head, "",
		fmt.Sprintf("- 워크로드: %s · 소스 %s/%s → 타겟 %s/%s (%s)", st.Workload, jsonx.S(src, "ns"), jsonx.S(src, "infra"), jsonx.S(tgt, "csp"), jsonx.S(tgt, "region"), jsonx.S(tgt, "provider")),
		fmt.Sprintf("- 생성: %s · 갱신: %s", st.Created, st.Updated), "",
		"## 단계", "", "| 단계 | 상태 | 시작 | 끝 | 비고 |", "| --- | --- | --- | --- | --- |"}
	for _, n := range config.Stages {
		s := rs.Stage(n)
		lines = append(lines, fmt.Sprintf("| %s | %s | %s | %s | %s |", n, s.Status, textx.Head(s.Started, 19), textx.Head(s.Ended, 19), textx.Head(s.Error, 80)))
	}
	v := jsonx.AsM(st.Verdict)
	lines = append(lines, "", "## 판정", "")
	if v != nil {
		lines = append(lines, fmt.Sprintf("- %s — %s", jsonx.S(v, "result"), jsonx.S(v, "summary")))
		if m := jsonx.AsM(v["metrics"]); len(m) > 0 {
			var parts []string
			for _, k := range jsonx.SortedKeys(m) { // 워크로드가 결과표에 남긴 수치를 그대로
				if s := jsonx.S(m, k); s != "" {
					parts = append(parts, k+"="+s)
				}
			}
			lines = append(lines, "- 지표: "+strings.Join(parts, " · "))
		}
		for _, w := range jsonx.Strings(v["warn"]) {
			lines = append(lines, "- WARN "+w)
		}
		for _, f := range jsonx.Strings(jsonx.Get(v, "compare", "fail_items")) {
			lines = append(lines, "- FAIL "+f)
		}
		for _, f := range jsonx.Strings(v["status_fail"]) {
			lines = append(lines, "- FAIL 상태 "+f)
		}
	} else {
		lines = append(lines, "- (없음)")
	}
	lines = append(lines, "", "## 자원 · 키", "")
	for _, k := range []string{"target_infra_id", "backup_out", "sg_rules_added", "xreq_backup", "xreq_rebuild", "start_outcome", "beetle_name_seed", "provision_zone", "provision_zone_tries", "beetle_data_req", "beetle_data_req_2", "beetle_data_req_3", "beetle_hooks", "source_restored", "staging_left", "status_only"} {
		if val, ok := st.Keys[k]; ok {
			b, _ := jsonx.Marshal(val, false)
			lines = append(lines, fmt.Sprintf("- %s: %s", k, string(b)))
		}
	}
	if b, err := os.ReadFile(filepath.Join(rs.RunDir, "recommend", "rationale.md")); err == nil {
		lines = append(lines, "", "## 추천 · 선택 근거", "")
		for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			if !strings.HasPrefix(l, "# ") {
				lines = append(lines, l)
			}
		}
	}
	lines = append(lines, "", "## 사람이 개입한 지점 (수작업 유발 구간)", "")
	if len(st.Interventions) == 0 {
		lines = append(lines, "- 없음")
	}
	for _, n := range st.Interventions {
		stage := "-"
		if n.Stage != nil && *n.Stage != "" {
			stage = *n.Stage
		}
		lines = append(lines, fmt.Sprintf("- %s [%s] %s", textx.Head(n.At, 19), stage, n.Text))
	}
	return os.WriteFile(filepath.Join(rs.RunDir, "summary.md"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}
