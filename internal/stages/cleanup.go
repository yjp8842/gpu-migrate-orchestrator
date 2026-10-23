package stages

// 8. cleanup — 타겟 정리(suspend · terminate · keep), 소스 되돌리기(어댑터 source_restore), summary.md.
//
// Beetle 로 만든 타겟의 terminate 는 Beetle 삭제 API 로 한다 — Tumblebug 로 인프라만 지우면 Beetle 이 만든 vNet · 보안 그룹 ·
// SSH 키가 남는다. relay 전략의 스테이징 사본(Beetle 컨테이너 /tmp/transx-staging/<소스 폴더 이름>)은 다음 이관이 시작될 때만
// 비워지고 API 로 지울 수 없어, 지울 명령을 로그와 summary 에 남긴다.
// 실패로 멈춘 run 도 `migrate cleanup --case … --run <id>` 로 이 단계만 돌려 정리 · 소스 되돌리기를 할 수 있다.

import (
	"path"
	"strings"

	"gpu-migrate-orchestrator/internal/jsonx"
)

// stagingLeftovers 는 relay 전략일 때 Beetle 컨테이너에 남는 스테이징 경로 (transx createLocalStagingLocation 규칙).
func stagingLeftovers(ctx *Context) []string {
	if jsonx.S(ctx.Case.Transfer(), "strategy") != "relay" {
		return nil
	}
	var out []string
	for _, it := range renderItems(ctx, ctx.State.KeyS("backup_out")) {
		base := path.Base(strings.TrimRight(it.From, "/"))
		if base != "." && base != "/" && base != "" {
			out = append(out, "/tmp/transx-staging/"+base)
		}
	}
	return out
}

// restoreSource 는 어댑터 source_restore 를 소스에서 돌린다 (패키지 생성 때 멈춘 서비스를 다시 켜는 등).
func restoreSource(ctx *Context) error {
	cmd := ctx.Adapter.S("source_restore")
	if cmd == "" {
		return nil
	}
	if _, err := ctx.Node("source"); err != nil {
		ctx.Log.Info(stCleanup, "소스 노드 정보가 없어 source_restore 를 건너뜁니다 (package 단계 전에 멈춤)")
		return nil
	}
	if _, err := ctx.ExecOK("source", cmd, 10); err != nil {
		return Errf("소스 되돌리기(source_restore) 실패: %v", err)
	}
	ctx.State.SetKey("source_restored", true)
	ctx.Log.Info(stCleanup, "소스 되돌리기 완료 (source_restore)")
	return nil
}

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
		ctx.Dry(stCleanup, "cleanup="+action, M{"infra": infra, "via": map[bool]string{true: "beetle", false: "tumblebug"}[jsonx.S(tgt, "provider") == "beetle"],
			"source_restore": ctx.Adapter.S("source_restore"), "staging_left": stagingLeftovers(ctx)})
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
	case action == "terminate" && ctx.State.HasKey("beetle_migrate_started"):
		// Beetle 이 만든 타겟 — Beetle 삭제 API 로 지워야 Beetle 이 만든 vNet · 보안 그룹 · SSH 키까지 지워진다
		b, err := ctx.Beetle()
		if err != nil {
			return nil, err
		}
		if _, err := b.DeleteInfra(ns, id); err != nil {
			return nil, err
		}
		ctx.Log.Info(stCleanup, "타겟 %s terminate 요청 (Beetle — vNet · 보안 그룹 · SSH 키 포함)", id)
		done = "terminate"
	case action == "terminate":
		infra, err := tb.GetInfra(ns, id)
		if err != nil {
			return nil, err
		}
		if jsonx.S(jsonx.AsM(infra["label"]), "gmp.case") != ctx.Case.Name {
			return nil, Errf("%s 에 이 도구의 라벨(gmp.case=%s)이 없어 terminate 하지 않습니다 — 손으로 지우세요", id, ctx.Case.Name)
		}
		if _, err := tb.DeleteInfra(ns, id, "terminate"); err != nil {
			return nil, err
		}
		ctx.Log.Info(stCleanup, "타겟 %s terminate 요청", id)
		done = "terminate"
	}
	if err := restoreSource(ctx); err != nil {
		return nil, err
	}
	if left := stagingLeftovers(ctx); len(left) > 0 {
		ctx.State.SetKey("staging_left", left)
		ctx.Log.Warn(stCleanup, "relay 스테이징 사본이 Beetle 컨테이너에 남습니다 — 서버에서: sudo docker exec cm-beetle rm -rf %s", strings.Join(left, " "))
	}
	if err := WriteSummary(ctx); err != nil {
		return nil, err
	}
	return M{"action": done, "source_restored": ctx.State.HasKey("source_restored")}, nil
}
