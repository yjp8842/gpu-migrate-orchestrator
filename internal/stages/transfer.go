package stages

// 5. transfer — 패키지 폴더를 타겟의 PACKAGE_DIR 로 옮긴다.
//
// Beetle 데이터 이관 API 로만 옮긴다 (소스 → 타겟 rsync, 개인키는 암호화해 넘긴다). 도구는 VM 에 SSH 로 접속하지 않으므로
// 로컬 PC 를 거치는 대안은 없다 — Beetle 이 안 되면 이 단계에서 멈춘다.
// 이관 뒤에는 타겟에서 체크섬과 파일 목록을 원격 명령으로 대조한다.
// 재개 때 앞선 요청이 실패로 끝났거나 대조에서 떨어졌으면, 그 요청이 쓴 타겟 폴더를 <폴더>.failed-<N> 으로 옮기고 새 요청으로 다시 보낸다.

import (
	"fmt"
	"path"
	"strings"

	"gpu-migrate-orchestrator/internal/config"
	"gpu-migrate-orchestrator/internal/jsonx"
	"gpu-migrate-orchestrator/internal/textx"
)

const stTransfer = "transfer"

func init() { register(stTransfer, Stage{Preflight: preTransfer, Run: runTransfer}) }

// VerifyScript 는 타겟에서 패키지를 대조한다. checksums.sha256 은 꼭 있어야 하고(sha256sum -c), package-files.tsv(이름 · 바이트 · sha)가
// 있으면 그것까지 대조한다. 목록이 없으면 그렇다고 적는다 — 예전에는 목록이 없어도 입력이 비어 BAD=0 으로 통과했다.
const VerifyScript = `cd {pkg} || exit 9
[ -f checksums.sha256 ] || { echo "NO checksums.sha256"; exit 1; }
sha256sum -c --quiet checksums.sha256 || { echo "CHECKSUMS BAD"; exit 1; }
if [ ! -f package-files.tsv ]; then echo "CHECKSUMS OK $(grep -c . checksums.sha256) · package-files.tsv 없음 — 목록 대조 생략"; exit 0; fi
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

func verifyRemote(ctx *Context, pkg string) error { return verifyItem(ctx, 0, pkg) }

func verifyItem(ctx *Context, i int, pkg string) error {
	r, err := ctx.Exec("target", strings.ReplaceAll(VerifyScript, "{pkg}", textx.RemotePath(pkg)), 30)
	if err != nil {
		return err
	}
	text := r.Stdout
	if strings.TrimSpace(r.Stderr) != "" {
		text += "\n[stderr]\n" + r.Stderr
	}
	ctx.WriteText(stTransfer, itemFile(i, "verify-remote.txt"), text)
	if !r.Reachable || r.RC != 0 {
		return Errf("타겟 패키지 대조 실패 %s (rc=%d): %s", pkg, r.RC, textx.Tail(strings.TrimSpace(r.Stdout+r.Stderr+" "+r.Detail), 300))
	}
	lines := strings.Split(strings.TrimSpace(r.Stdout), "\n")
	ctx.Log.Info(stTransfer, "타겟 대조 통과 %s: %s", pkg, lines[len(lines)-1])
	return nil
}

// renderItems 는 어댑터의 전송 목록에서 {backup_out} · {package_dir} 를 채운다.
func renderItems(ctx *Context, outDir string) []config.TransferItem {
	pkg := ctx.Adapter.S("package_dir")
	rep := strings.NewReplacer("{backup_out}", strings.TrimRight(outDir, "/"), "{package_dir}", pkg)
	items := ctx.Adapter.TransferItems()
	for i := range items {
		items[i].From, items[i].To = rep.Replace(items[i].From), rep.Replace(items[i].To)
	}
	return items
}

func runTransfer(ctx *Context) (M, error) {
	outDir := ctx.State.KeyS("backup_out")
	if outDir == "" {
		outDir = "<backup_out>"
	}
	pkg := ctx.Adapter.S("package_dir")
	items := renderItems(ctx, outDir)
	if ctx.DryRun {
		for i, it := range items {
			body := M{"source": M{"storageType": "filesystem", "path": it.From}, "destination": M{"storageType": "filesystem", "path": it.To},
				"strategy": jsonx.S(ctx.Case.Transfer(), "strategy"), "encryptionKeyId": "<encryptionKey>"}
			if len(it.Exclude) > 0 {
				body["filter(source · destination)"] = M{"exclude": it.Exclude}
			}
			if it.Delete {
				body["delete"] = true
			}
			ctx.Dry(stTransfer, fmt.Sprintf("Beetle POST /migration/data (%d/%d — 경로의 ~/ 는 실행 때 각 VM 의 홈 기준 절대 경로로 바꾼다)", i+1, len(items)), body)
			if it.Verify {
				ctx.Dry(stTransfer, "Tumblebug cmd_sync(타겟) 패키지 대조", M{"command": []string{Wrap(strings.ReplaceAll(VerifyScript, "{pkg}", it.To))}})
			}
		}
		return M{"dry_run": true, "items": len(items)}, nil
	}
	marker := pkg + "/.gmp-transferred"
	if done, ok, err := ctx.ReadText("target", marker); err != nil {
		return nil, err
	} else if ok && strings.TrimSpace(done) == ctx.State.Data.RunID {
		ctx.Log.Info(stTransfer, "이미 이 run 에서 이관됨 — 대조만 다시 합니다")
		for i, it := range items {
			if it.Verify {
				if err := verifyItem(ctx, i, it.To); err != nil {
					return nil, err
				}
			}
		}
		return M{"skipped": true}, nil
	}
	for i, it := range items {
		doneKey := fmt.Sprintf("transfer_item_%d_done", i+1)
		if ctx.State.KeyS(doneKey) != "" && ctx.State.KeyS(doneKey) == ctx.State.KeyS(itemKey(i)) {
			ctx.Log.Info(stTransfer, "전송 %d/%d 은 이미 끝남 (%s)", i+1, len(items), it.To)
			continue
		}
		resumed, err := resumeItem(ctx, i, it)
		if err != nil {
			return nil, err
		}
		if !resumed && it.Fresh {
			r, err := ctx.Exec("target", fmt.Sprintf("ls -A %s 2>/dev/null | head -3", textx.RemotePath(it.To)), 2)
			if err != nil {
				return nil, err
			}
			if listing := strings.TrimSpace(r.Stdout); listing != "" {
				return nil, Errf("타겟 %s 가 비어 있지 않습니다 (%s) — 묵은 패키지(.restored 포함)는 적재를 건너뛰게 하므로 비운 뒤 다시: rm -rf %s",
					it.To, strings.ReplaceAll(listing, "\n", " "), it.To)
			}
		}
		// rsync 는 대상 경로의 마지막 폴더만 만든다 — 상위 폴더가 없으면 "mkdir … failed: No such file or directory" (10/23 실 운행)
		if parent := path.Dir(strings.TrimRight(it.To, "/")); parent != "." && parent != "/" && parent != "~" {
			if _, err := ctx.ExecOK("target", "mkdir -p "+textx.RemotePath(parent), 2); err != nil {
				return nil, err
			}
		}
		if err := transferItemViaBeetle(ctx, i, it); err != nil {
			return nil, err
		}
		if it.Verify {
			if err := verifyItem(ctx, i, it.To); err != nil {
				vkey := itemKey(i) + "_verify_failed"
				if i == 0 {
					vkey = "beetle_data_verify_failed"
				}
				ctx.State.SetKey(vkey, ctx.State.KeyS(itemKey(i))) // 재개 때 이 요청은 실패로 보고 다시 보낸다
				return nil, err
			}
		}
		ctx.State.SetKey(doneKey, ctx.State.KeyS(itemKey(i)))
	}
	if _, err := ctx.ExecOK("target", fmt.Sprintf("mkdir -p %s && echo %s > %s", textx.RemotePath(pkg), ctx.State.Data.RunID, textx.RemotePath(marker)), 2); err != nil {
		return nil, err
	}
	return M{"method": "beetle", "package_dir": pkg, "items": len(items)}, nil
}
