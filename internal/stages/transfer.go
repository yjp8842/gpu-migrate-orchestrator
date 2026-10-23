package stages

// 5. transfer — 패키지 폴더를 타겟의 PACKAGE_DIR 로 옮긴다.
//
// Beetle 데이터 이관 API 로만 옮긴다 (소스 → 타겟 rsync, 개인키는 암호화해 넘긴다). 도구는 VM 에 SSH 로 접속하지 않으므로
// 로컬 PC 를 거치는 대안은 없다 — Beetle 이 안 되면 이 단계에서 멈춘다.
// 이관 뒤에는 타겟에서 체크섬과 파일 목록을 원격 명령으로 대조한다.
// 재개 때 앞선 요청이 실패로 끝났거나 대조에서 떨어졌으면, 그 요청이 쓴 타겟 폴더를 <폴더>.failed-<N> 으로 옮기고 새 요청으로 다시 보낸다.

import (
	"fmt"
	"strings"

	"gpu-migrate-orchestrator/internal/jsonx"
	"gpu-migrate-orchestrator/internal/textx"
)

const stTransfer = "transfer"

func init() { register(stTransfer, Stage{Preflight: preTransfer, Run: runTransfer}) }

// VerifyScript 는 타겟에서 패키지를 대조한다 (sha256sum -c + package-files.tsv 의 바이트 · sha).
const VerifyScript = `cd {pkg} || exit 9
sha256sum -c --quiet checksums.sha256 || { echo "CHECKSUMS BAD"; exit 1; }
bad=0; n=0
while IFS=$'\t' read -r name bytes sha; do
  [ -n "$name" ] || continue; n=$((n+1))
  if [ ! -f "$name" ] || [ "$(stat -c %s "$name")" != "$bytes" ] || [ "$(sha256sum "$name" | cut -d" " -f1)" != "$sha" ]; then echo "BAD $name"; bad=$((bad+1)); fi
done < package-files.tsv
echo "FILES=$n BAD=$bad"; [ "$bad" = 0 ]`

func preTransfer(ctx *Context) error {
	if ctx.DryRun {
		return nil
	}
	if ctx.State.KeyS("backup_out") == "" {
		return Errf("backup_out 이 없습니다 — package 단계가 먼저입니다")
	}
	if _, err := ctx.Node("source"); err != nil {
		return err
	}
	if _, err := ctx.Node("target"); err != nil {
		return err
	}
	_, err := ctx.Beetle()
	return err
}

func verifyRemote(ctx *Context, pkg string) error {
	r, err := ctx.Exec("target", strings.ReplaceAll(VerifyScript, "{pkg}", pkg), 30)
	if err != nil {
		return err
	}
	text := r.Stdout
	if strings.TrimSpace(r.Stderr) != "" {
		text += "\n[stderr]\n" + r.Stderr
	}
	ctx.WriteText(stTransfer, "verify-remote.txt", text)
	if !r.Reachable || r.RC != 0 {
		return Errf("타겟 패키지 대조 실패 (rc=%d): %s", r.RC, textx.Tail(strings.TrimSpace(r.Stdout+r.Stderr+" "+r.Detail), 300))
	}
	lines := strings.Split(strings.TrimSpace(r.Stdout), "\n")
	ctx.Log.Info(stTransfer, "타겟 패키지 대조 통과: %s", lines[len(lines)-1])
	return nil
}

func runTransfer(ctx *Context) (M, error) {
	outDir := ctx.State.KeyS("backup_out")
	if outDir == "" {
		outDir = "<backup_out>"
	}
	pkg := ctx.Adapter.S("package_dir")
	if ctx.DryRun {
		ctx.Dry(stTransfer, "Beetle POST /migration/data", M{"source": M{"storageType": "filesystem", "path": strings.TrimRight(outDir, "/") + "/"},
			"destination": M{"storageType": "filesystem", "path": pkg}, "strategy": jsonx.S(ctx.Case.Transfer(), "strategy"), "encryptionKeyId": "<encryptionKey>"})
		ctx.Dry(stTransfer, "Tumblebug cmd_sync(타겟) 패키지 대조", M{"command": []string{Wrap(strings.ReplaceAll(VerifyScript, "{pkg}", pkg))}})
		return M{"dry_run": true}, nil
	}
	marker := pkg + "/.gmp-transferred"
	if done, ok, err := ctx.ReadText("target", marker); err != nil {
		return nil, err
	} else if ok && strings.TrimSpace(done) == ctx.State.Data.RunID {
		ctx.Log.Info(stTransfer, "이미 이 run 에서 이관됨 — 대조만 다시 합니다")
		if err := verifyRemote(ctx, pkg); err != nil {
			return nil, err
		}
		return M{"skipped": true}, nil
	}
	resumed, err := resumeBeetle(ctx, pkg)
	if err != nil {
		return nil, err
	}
	if !resumed {
		r, err := ctx.Exec("target", fmt.Sprintf("ls -A %s 2>/dev/null | head -3", pkg), 2)
		if err != nil {
			return nil, err
		}
		if listing := strings.TrimSpace(r.Stdout); listing != "" {
			return nil, Errf("타겟 %s 가 비어 있지 않습니다 (%s) — 묵은 패키지(.restored 포함)는 적재를 건너뛰게 하므로 비운 뒤 다시: rm -rf %s",
				pkg, strings.ReplaceAll(listing, "\n", " "), pkg)
		}
	}
	if err := transferViaBeetle(ctx, outDir, pkg); err != nil {
		return nil, err
	}
	if err := verifyRemote(ctx, pkg); err != nil {
		ctx.State.SetKey("beetle_data_verify_failed", ctx.State.KeyS("beetle_data_req")) // 재개 때 이 요청은 실패로 보고 다시 보낸다
		return nil, err
	}
	if _, err := ctx.ExecOK("target", fmt.Sprintf("echo %s > %s", ctx.State.Data.RunID, marker), 2); err != nil {
		return nil, err
	}
	return M{"method": "beetle", "package_dir": pkg}, nil
}
