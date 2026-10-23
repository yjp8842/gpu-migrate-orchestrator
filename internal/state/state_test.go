package state

import "testing"

func TestCreateMarkResume(t *testing.T) {
	d := t.TempDir()
	st, err := Create(d, "case-x", "cases/x.yaml", "widgetrag-shell", false)
	if err != nil {
		t.Fatal(err)
	}
	st.SetKey("target_infra_id", "wr-aws-tgt-01")
	st.SetKey("results_baseline_rows", 3)
	st.Mark("collect", "running", "", nil)
	st.Mark("collect", "done", "", map[string]any{"gpu": "T4"})
	st.Mark("recommend", "failed", "boom", nil)
	st.Note("VPC 한도 상향 요청", "provision", "")
	again, err := Open(d, "case-x", st.Data.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if !again.IsDone("collect") || again.Stage("recommend").Status != "failed" {
		t.Fatal(again.Data.Stages)
	}
	if again.KeyS("target_infra_id") != "wr-aws-tgt-01" || len(again.Data.Interventions) != 1 {
		t.Fatal(again.Data.Keys)
	}
	if LatestRunID(d, "case-x") != st.Data.RunID || LatestRunID(d, "nope") != "" {
		t.Fatal("latest")
	}
	if !again.HasKey("results_baseline_rows") || again.KeyS("results_baseline_rows") != "3" {
		t.Fatal(again.Key("results_baseline_rows"))
	}
}

func TestLockRejectsSecond(t *testing.T) {
	d := t.TempDir()
	st, _ := Create(d, "c", "x", "w", false)
	if err := st.Lock(); err != nil {
		t.Fatal(err)
	}
	other, _ := Open(d, "c", st.Data.RunID)
	if err := other.Lock(); err == nil {
		t.Fatal("두 번째 잠금이 통과했다")
	}
	st.Unlock()
}
