package stages

// 8. cleanup — 타겟 정리(suspend · terminate · keep), summary.md.

import (
	"gpu-migrate-orchestrator/internal/jsonx"
)

const stCleanup = "cleanup"

func init() {
	register(stCleanup, Stage{Preflight: func(*Context) error { return nil }, Run: runCleanup})
}

func runCleanup(ctx *Context) (M, error) {
	tgt := ctx.Case.Target()
	action := ctx.Case.Cleanup()
	id := ctx.State.KeyS("target_infra_id")
	ns := jsonx.S(tgt, "ns")
	if ctx.DryRun {
		infra := id
		if infra == "" {
			infra = jsonx.S(tgt, "name")
		}
		ctx.Dry(stCleanup, "cleanup="+action, M{"infra": infra})
		if err := WriteSummary(ctx); err != nil {
			return nil, err
		}
		return M{"dry_run": true}, nil
	}
	tb, err := ctx.TBTarget()
	if err != nil {
		return nil, err
	}
	done := ""
	switch {
	case action == "keep" || id == "":
		label := id
		if label == "" {
			label = "인프라 없음"
		}
		ctx.Log.Info(stCleanup, "타겟 유지 (%s)", label)
		done = "keep"
	case action == "suspend":
		if _, err := tb.Control(ns, id, "suspend"); err != nil {
			return nil, err
		}
		ctx.Log.Info(stCleanup, "타겟 %s suspend 요청 — 공인 IP 는 다시 켤 때 바뀐다", id)
		done = "suspend"
	case action == "terminate":
		infra, err := tb.GetInfra(ns, id)
		if err != nil {
			return nil, err
		}
		if jsonx.S(jsonx.AsM(infra["label"]), "gmp.case") != ctx.Case.Name && !ctx.State.HasKey("beetle_migrate_started") {
			return nil, Errf("%s 에 이 도구의 라벨(gmp.case=%s)이 없어 terminate 하지 않습니다 — 손으로 지우세요", id, ctx.Case.Name)
		}
		if _, err := tb.DeleteInfra(ns, id, "terminate"); err != nil {
			return nil, err
		}
		ctx.Log.Info(stCleanup, "타겟 %s terminate 요청", id)
		done = "terminate"
	}
	if err := WriteSummary(ctx); err != nil {
		return nil, err
	}
	return M{"action": done}, nil
}
