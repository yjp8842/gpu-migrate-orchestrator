package stages

// Beetle 데이터 이관 — 라이브 스키마(transx.DataMigrationModel, 10/22 확인) 기준.
//
//	{source|destination: {storageType: "filesystem", path, filesystem: {accessType: "ssh", ssh: {host, port, username, privateKey, archive, compress}},
//	                      preCmd(소스, 이관 전) | postCmd(목적지, 이관 뒤)},
//	 strategy: auto|direct|relay, encryptionKeyId}
//
// privateKey 는 Tumblebug 에서 메모리로 받아 바로 암호화한다(파일로 두지 않는다). 비동기(Prefer: respond-async) + 우리가 정한 X-Request-Id.
//
// 경로는 각 VM 의 홈 기준 절대 경로로 넘긴다. direct(에이전트 포워딩)는 소스 VM 에서
// `rsync -avz -e 'ssh …' <소스 경로>/ <user>@<타겟>:<타겟 경로>` 를 돌리는데, 타겟 쪽 `~` 는 소스 셸이 풀지 않고 rsync 에 맡겨진다.
// relay 는 양쪽이 모두 원격 경로가 된다. 그 처리에 기대지 않으려고 `~/` 를 미리 푼다. rsync 는 두 VM 모두에 있어야 한다.

import (
	"fmt"
	"strings"
	"time"

	"gpu-migrate-orchestrator/internal/beetle"
	"gpu-migrate-orchestrator/internal/crypto"
	"gpu-migrate-orchestrator/internal/jsonx"
	"gpu-migrate-orchestrator/internal/textx"
)

// RsyncCheck 는 rsync 가 있는지 보고 없으면 설치한다 (마지막 줄이 실패하면 설치도 못 한 것).
const RsyncCheck = `if command -v rsync >/dev/null 2>&1; then echo present; else sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -q rsync >/dev/null && echo installed; fi; command -v rsync >/dev/null`

// absPath 는 `~/` 로 시작하는 경로를 그 VM 의 홈 기준 절대 경로로 바꾼다.
func absPath(ctx *Context, role, p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	out, err := ctx.ExecOK(role, `printf '%s\n' "$HOME"`, 2)
	if err != nil {
		return "", err
	}
	home := strings.TrimRight(strings.TrimSpace(out), "/")
	if !strings.HasPrefix(home, "/") {
		return "", Errf("%s VM 의 홈 경로를 읽지 못했습니다: %q", role, out)
	}
	return home + strings.TrimPrefix(p, "~"), nil
}

// prepareBeetleEnds 는 두 VM 에 rsync 를 갖추고, Beetle 에 넘길 절대 경로를 돌려준다.
func prepareBeetleEnds(ctx *Context, outDir, pkg string) (string, string, error) {
	for _, role := range []string{"source", "target"} {
		out, err := ctx.ExecOK(role, RsyncCheck, 5)
		if err != nil {
			return "", "", Errf("%s VM 에 rsync 가 없고 설치하지 못했습니다 — Beetle 이관은 두 VM 모두 rsync 가 필요합니다: %v", role, err)
		}
		if strings.Contains(out, "installed") {
			ctx.Log.Info(stTransfer, "%s VM 에 rsync 설치", role)
		}
	}
	src, err := absPath(ctx, "source", outDir)
	if err != nil {
		return "", "", err
	}
	dst, err := absPath(ctx, "target", pkg)
	if err != nil {
		return "", "", err
	}
	ctx.State.SetKey("beetle_paths", M{"source": src, "destination": dst})
	ctx.Log.Info(stTransfer, "Beetle 경로: 소스 %s → 타겟 %s", src, dst)
	return src, dst, nil
}

func beetleSide(node M, keyPEM string, enc *crypto.Encryptor, path string) (M, error) {
	ek, err := enc.Encrypt(keyPEM)
	if err != nil {
		return nil, err
	}
	port := jsonx.Int(node["sshPort"])
	if port == 0 {
		port = 22
	}
	return M{"storageType": "filesystem", "path": path, "filesystem": M{"accessType": "ssh", "ssh": M{
		"host": jsonx.S(node, "publicIP"), "port": port, "username": jsonx.SOr(node, "nodeUserName", "cb-user"),
		"privateKey": ek, "archive": true, "compress": true, "connectTimeout": 30}}}, nil
}

// BuildBeetleBody 는 데이터 이관 요청 본문을 만든다. rid 는 이 요청의 ID — 훅 표식에 넣는다.
func BuildBeetleBody(ctx *Context, enc *crypto.Encryptor, outDir, pkg, rid string) (M, error) {
	s, err := ctx.Node("source")
	if err != nil {
		return nil, err
	}
	t, err := ctx.Node("target")
	if err != nil {
		return nil, err
	}
	srcKey, err := ctx.PrivateKey("source")
	if err != nil {
		return nil, err
	}
	dstKey, err := ctx.PrivateKey("target")
	if err != nil {
		return nil, err
	}
	src, err := beetleSide(s, srcKey, enc, strings.TrimRight(outDir, "/")+"/") // 끝 슬래시 — 폴더 '안의 것'을 옮긴다 (rsync 규칙)
	if err != nil {
		return nil, err
	}
	dst, err := beetleSide(t, dstKey, enc, pkg)
	if err != nil {
		return nil, err
	}
	// preCmd · postCmd 는 실행 여부만 확인하려고 짧은 표식을 남긴다 (구버전은 실행하지 않았다). 패키지는 package 단계가 이미 만들었다.
	// 표식에 요청 ID 를 넣어, 다시 보낸 요청에서 앞선 시도의 표식을 이번 것으로 읽지 않게 한다.
	src["preCmd"] = fmt.Sprintf("echo gmp-precmd %s > /tmp/gmp-precmd.txt", rid)
	dst["postCmd"] = fmt.Sprintf("{ echo gmp-postcmd %s; ls -la %s; } > /tmp/gmp-postcmd.txt 2>&1", rid, pkg)
	return M{"source": src, "destination": dst, "strategy": jsonx.SOr(ctx.Case.Transfer(), "strategy", "direct"), "encryptionKeyId": enc.KeyID}, nil
}

func transferViaBeetle(ctx *Context, outDir, pkg string) error {
	b, err := ctx.Beetle()
	if err != nil {
		return err
	}
	rid := ctx.State.KeyS("beetle_data_req")
	if rid == "" {
		srcPath, dstPath, err := prepareBeetleEnds(ctx, outDir, pkg)
		if err != nil {
			return err
		}
		rid = fmt.Sprintf("gmp-%s-data", ctx.State.Data.RunID)
		if n := len(jsonx.AsList(ctx.State.Key("beetle_data_req_tries"))); n > 0 {
			rid = fmt.Sprintf("%s-%d", rid, n+1) // 다시 보내는 요청은 새 ID 로
		}
		ctx.State.SetKey("beetle_data_req", rid) // 부작용 전에 기록
		bundle, err := b.EncryptionKey()
		if err != nil {
			return err
		}
		enc, err := crypto.New(bundle)
		if err != nil {
			return err
		}
		body, err := BuildBeetleBody(ctx, enc, srcPath, dstPath, rid)
		if err != nil {
			return err
		}
		if _, err := b.DataMigration(body, rid); err != nil {
			return err
		}
		ctx.Log.Info(stTransfer, "Beetle 데이터 이관 시작 (reqId %s, strategy %s)", rid, jsonx.S(body, "strategy"))
	}
	st, err := b.WaitRequest(rid, time.Duration(ctx.Settings.TimeoutMin("transfer_min"))*time.Minute, 15*time.Second, TickLogger(ctx.Log, stTransfer, 4))
	if err != nil {
		return err
	}
	keep := M{}
	for k, v := range st {
		if k != "requestInfo" {
			keep[k] = v
		}
	}
	ctx.WriteJSON(stTransfer, "beetle-request.json", keep)
	if strings.ToLower(jsonx.S(st, "status")) != "success" {
		raw, _ := jsonx.Marshal(st["errorResponse"], false)
		return Errf("Beetle 데이터 이관이 성공으로 끝나지 않았습니다: %s — %s", jsonx.S(st, "status"), textx.Head(string(raw), 300))
	}
	// preCmd · postCmd 가 이 요청에서 실제로 돌았는지 (v0.6.2 에서 처음 확인하는 동작)
	pre, preOK, _ := ctx.ReadText("source", "/tmp/gmp-precmd.txt")
	post, postOK, _ := ctx.ReadText("target", "/tmp/gmp-postcmd.txt")
	preRan, postRan := preOK && strings.Contains(pre, rid), postOK && strings.Contains(post, rid)
	ctx.State.SetKey("beetle_hooks", M{"preCmd_ran": preRan, "postCmd_ran": postRan})
	ctx.Log.Info(stTransfer, "Beetle 훅 실행 확인: preCmd %s · postCmd %s", ranStr(preRan), ranStr(postRan))
	return nil
}

// resumeBeetle 은 이 run 이 앞서 보낸 Beetle 요청을 본다. 도는 중이거나 성공했으면 true — 그 요청을 이어서 보고,
// 타겟 폴더의 내용은 그 요청이 쓴 것이라 비어 있는지 보지 않는다. 실패로 끝났거나, Beetle 에 기록이 없거나
// (보내기 전에 멈췄거나 Beetle 이 다시 떴다), 성공했지만 타겟 대조에서 떨어졌으면 시도 기록에 남기고
// 그 요청이 쓴 타겟 폴더를 옆으로 옮긴 뒤 false — 새 ID 로 다시 보낸다.
func resumeBeetle(ctx *Context, pkg string) (bool, error) {
	rid := ctx.State.KeyS("beetle_data_req")
	if rid == "" {
		return false, nil
	}
	b, err := ctx.Beetle()
	if err != nil {
		return false, err
	}
	var status, why string
	if ctx.State.KeyS("beetle_data_verify_failed") == rid {
		status, why = "verify-failed", "이관은 성공했지만 타겟 패키지 대조에서 떨어짐"
	} else {
		st, err := b.RequestStatus(rid)
		if err != nil && !beetle.IsNotFound(err) {
			return false, err
		}
		status = strings.ToLower(jsonx.S(st, "status"))
		switch {
		case err != nil || status == "":
			status, why = "missing", "Beetle 에 기록이 없음 — 보내기 전에 멈췄거나 Beetle 이 다시 떴다"
		case !beetle.Terminal[status] || status == "success":
			ctx.Log.Info(stTransfer, "Beetle 데이터 이관은 이미 시작됨 (reqId %s, %s) — 상태를 이어서 본다", rid, status)
			return true, nil
		default:
			raw, _ := jsonx.Marshal(st["errorResponse"], false)
			why = status + " — " + textx.Head(string(raw), 200)
		}
	}
	n := retireAttempt(ctx, stTransfer, "beetle_data_req", rid, status, "beetle-request.json", "verify-remote.txt")
	ctx.State.DelKey("beetle_data_verify_failed")
	ctx.Log.Warn(stTransfer, "앞선 Beetle 요청 %s 가 실패로 끝났습니다 (%s) — %d번째 시도로 다시 보낸다", rid, why, n+1)
	return false, moveAside(ctx, stTransfer, "target", pkg, n)
}

func ranStr(b bool) string {
	if b {
		return "실행됨"
	}
	return "흔적 없음"
}
