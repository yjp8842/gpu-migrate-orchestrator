package stages

// 4. package — 소스 상태를 확인하고 이관 패키지를 만든다. 소스에는 Tumblebug 원격 명령으로만 들어간다.
//
//  ① 접속 정보 갱신 — 중지 · 재개 뒤에는 공인 IP 가 바뀌므로 매번 Tumblebug 에서 새로 받는다 (개인키는 두지 않는다)
//  ② 상태 확인 — Tumblebug 메타가 모르는 것: 서비스 · LLM 설정(골든 전제) · 식별 대조표의 소스 쪽 (identify.json)
//  ③ 재기동(옵션) — 같은 LLM 설정으로 다시 띄우고 jar 안의 구 설정을 없앤다
//  ④ 패키지 생성 — 비동기 원격 명령 + 상태 조회 (최대 120분). 출력 폴더는 run 시각으로 고정해 state 에 적고,
//     재개 때 MANIFEST.txt 가 이미 있으면 다시 만들지 않는다 (backup 은 비어 있지 않은 폴더를 거부한다).
//     앞선 backup 이 실패로 끝났으면 그 출력 폴더를 <폴더>.failed-<N> 으로 옮기고 새 요청으로 다시 보낸다

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gpu-migrate-orchestrator/internal/inspect"
	"gpu-migrate-orchestrator/internal/jsonx"
	"gpu-migrate-orchestrator/internal/textx"
	"gpu-migrate-orchestrator/internal/tumblebug"
)

const stPackage = "package"

func init() { register(stPackage, Stage{Preflight: prePackage, Run: runPackage}) }

func prePackage(ctx *Context) error {
	if ctx.DryRun {
		return nil
	}
	tb, err := ctx.TBSource()
	if err != nil {
		return err
	}
	src := ctx.Case.Source()
	_, err = RequireRunning(tb, jsonx.S(src, "ns"), jsonx.S(src, "infra"), "소스")
	return err
}

func refreshSourceAccess(ctx *Context) (M, error) {
	src := ctx.Case.Source()
	tb, err := ctx.TBSource()
	if err != nil {
		return nil, err
	}
	access, err := tb.AccessInfo(jsonx.S(src, "ns"), jsonx.S(src, "infra"))
	if err != nil {
		return nil, err
	}
	ctx.SaveAccess("source", access)
	n := access[0]
	ctx.Log.Info(stPackage, "소스 접속 정보 갱신: %s %s (%s)", jsonx.S(n, "nodeId"), jsonx.S(n, "publicIP"), jsonx.S(n, "nodeUserName"))
	return n, nil
}

// inspectSource 는 짧은 동기 원격 명령 한 번으로 소스 상태를 읽는다. (LLM 설정 불일치?, 설명, 식별 결과)
func inspectSource(ctx *Context, node M) (bool, string, M, error) {
	src := ctx.Case.Source()
	tb, err := ctx.TBSource()
	if err != nil {
		return false, "", nil, err
	}
	cmds := inspect.Commands(ctx.Adapter)
	list := make([]string, len(cmds))
	for i, c := range cmds {
		list[i] = c.Command
	}
	res, err := tb.CmdSync(jsonx.S(src, "ns"), jsonx.S(src, "infra"), list, jsonx.S(node, "nodeUserName"), 5)
	if err != nil {
		return false, "", nil, err
	}
	stdout, _, emsg := tumblebug.SyncStdout(res, jsonx.S(node, "nodeId"))
	if emsg != "" && len(stdout) == 0 {
		return false, "", nil, Errf("소스 상태 확인 명령 실패: %s", emsg)
	}
	out := map[string]string{}
	raw := filepath.Join(ctx.State.StageDir(stPackage), "inspect")
	os.MkdirAll(raw, 0o755)
	for i, c := range cmds {
		if i < len(stdout) {
			out[c.Name] = stdout[i]
		}
		os.WriteFile(filepath.Join(raw, c.Name+".txt"), []byte(out[c.Name]), 0o644)
	}
	ident := inspect.Identify(ctx.Adapter, out)
	if _, err := ctx.WriteJSON(stPackage, "identify.json", ident); err != nil {
		return false, "", nil, err
	}
	units := jsonx.ListM(ident["units"])
	active := 0
	for _, u := range units {
		if jsonx.S(u, "activeState") == "active" {
			active++
		}
	}
	gpu := M{}
	if g := jsonx.ListM(ident["gpu"]); len(g) > 0 {
		gpu = g[0]
	}
	ctx.Log.Info(stPackage, "소스 상태: GPU %s · 드라이버 %s · 서비스 active %d/%d · 커밋 %s",
		jsonx.SOr(gpu, "model", "없음"), jsonx.SOr(gpu, "driverVersion", "-"), active, len(units), jsonx.S(ident, "code_ref"))
	if !ctx.Adapter.GoldenCheck() {
		ctx.Log.Info(stPackage, "골든 전제(LLM 설정) 확인 안 함 — 어댑터 golden_check: false")
		return false, "확인 안 함", ident, nil
	}
	tf, sf := ctx.Adapter.HealthFields()
	bad, why := inspect.LLMMismatchFields(jsonx.AsM(ident["health"]), tf, sf, ctx.Case.EnvValue("LLM_TEMPERATURE"), ctx.Case.EnvValue("LLM_SEED"))
	return bad, why, ident, nil
}

// runRemote 는 소스에서 명령 하나를 비동기로 보내고 끝까지 기다린다. X-Request-Id 를 먼저 적어 재개가 같은 명령을 다시 보내지 않게.
// 재개 때 적어 둔 요청이 있으면 상태부터 본다. 도는 중이거나 성공이면 그 요청을 이어서 보고, 실패로 끝났거나
// Tumblebug 에 기록이 없으면(보내기 전에 멈춤) 시도 기록에 남기고 새 ID 로 다시 보낸다. beforeResend 는 다시 보내기 전에
// 앞선 시도의 흔적을 치운다 (인자는 실패한 시도의 번호).
func runRemote(ctx *Context, label, command string, timeoutMin int, beforeResend func(try int) error) (M, error) {
	src := ctx.Case.Source()
	ns, id := jsonx.S(src, "ns"), jsonx.S(src, "infra")
	tb, err := ctx.TBSource()
	if err != nil {
		return nil, err
	}
	node, err := ctx.Node("source")
	if err != nil {
		return nil, err
	}
	nid := jsonx.S(node, "nodeId")
	key := "xreq_" + label
	rid := ctx.State.KeyS(key)
	if rid != "" {
		prev, err := lastCommand(tb, ns, id, nid, rid)
		if err != nil {
			return nil, err
		}
		status := jsonx.S(prev, "status")
		if prev == nil || (tumblebug.Terminal[status] && !CmdResultOK(prev)) {
			why := "Tumblebug 에 기록이 없음 — 보내기 전에 멈춘 것으로 본다"
			if prev == nil {
				status = "missing"
			} else {
				why = DescribeCmd(prev)
			}
			n := retireAttempt(ctx, stPackage, key, rid, status, label+".stdout.txt")
			ctx.Log.Warn(stPackage, "앞선 %s 요청 %s 가 실패로 끝났습니다 (%s) — %d번째 시도로 다시 보낸다", label, rid, why, n+1)
			if beforeResend != nil {
				if err := beforeResend(n); err != nil {
					return nil, err
				}
			}
			rid = ""
		} else {
			ctx.Log.Info(stPackage, "%s 는 이미 보냈음 (xRequestId %s, %s) — 상태를 이어서 기다린다", label, rid, status)
		}
	}
	if rid == "" {
		rid = tumblebug.NewRequestID(fmt.Sprintf("gmp-%s-%s", ctx.State.Data.RunID, label))
		ctx.State.SetKey(key, rid)
		if _, err := tb.CmdAsync(ns, id, []string{command}, jsonx.S(node, "nodeUserName"), timeoutMin, rid); err != nil {
			return nil, err
		}
		ctx.Log.Info(stPackage, "%s 시작 (xRequestId %s)", label, rid)
	}
	done, err := tb.WaitCommand(ns, id, []string{nid}, rid, time.Duration(timeoutMin*60+300)*time.Second, 15*time.Second, TickLogger(ctx.Log, stPackage, 8))
	if err != nil {
		return nil, err
	}
	entry := done[nid]
	text := jsonx.S(entry, "stdout") + "\n--- stderr ---\n" + jsonx.S(entry, "stderr")
	os.WriteFile(filepath.Join(ctx.State.StageDir(stPackage), label+".stdout.txt"), []byte(text), 0o644)
	if !CmdResultOK(entry) {
		tail := jsonx.S(entry, "stderr")
		if tail == "" {
			tail = jsonx.S(entry, "stdout")
		}
		return nil, Errf("%s 실패: %s — %s.stdout.txt 끝부분: %s", label, DescribeCmd(entry), label, textx.Tail(tail, 300))
	}
	return entry, nil
}

// lastCommand 는 노드 하나에서 그 xRequestId 의 마지막 상태 항목이다 (기록이 없으면 nil).
func lastCommand(tb Tumblebug, ns, id, nid, rid string) (M, error) {
	entries, err := tb.CommandStatus(ns, id, nid, rid)
	if err != nil {
		var te *tumblebug.Error
		if errors.As(err, &te) && te.Status == 404 {
			return nil, nil
		}
		return nil, err
	}
	var last M
	for _, e := range entries {
		if x := jsonx.S(e, "xRequestId"); x == "" || x == rid {
			last = e
		}
	}
	return last, nil
}

func runPackage(ctx *Context) (M, error) {
	ad := ctx.Adapter
	runID := ctx.State.Data.RunID
	outDir := ctx.State.KeyS("backup_out")
	if outDir == "" {
		var err error
		if outDir, err = ad.Render("backup_out", map[string]string{"ts": runID}); err != nil {
			return nil, err
		}
	}
	ctx.State.SetKey("backup_out", outDir)
	golden := "0"
	if jsonx.Truthy(ctx.Case.Run()["golden"]) {
		golden = "1"
	}
	backupCmd, err := ctx.Remote("backup", map[string]string{"golden": golden, "out_dir": outDir})
	if err != nil {
		return nil, err
	}
	restartKey := ""
	if ad.Has("rebuild") {
		restartKey = "rebuild"
	} else if ad.Has("restart") {
		restartKey = "restart"
	}
	restartCmd := ""
	if restartKey != "" {
		if restartCmd, err = ctx.Remote(restartKey, nil); err != nil {
			return nil, err
		}
	}
	rebuild := jsonx.Truthy(ctx.Case.Run()["rebuild_before_backup"]) && restartCmd != ""
	pkgMin := ctx.Settings.TimeoutMin("package_min")
	if ctx.DryRun {
		src := ctx.Case.Source()
		ctx.Dry(stPackage, fmt.Sprintf("Tumblebug access_info %s/%s (접속 정보 갱신)", jsonx.S(src, "ns"), jsonx.S(src, "infra")),
			M{"params": M{"option": "accessinfo", "accessInfoOption": "showSshKey"}})
		var list []string
		for _, c := range inspect.Commands(ad) {
			list = append(list, c.Command)
		}
		ctx.Dry(stPackage, "Tumblebug cmd_sync(소스) 상태 확인", M{"command": list, "timeoutMinutes": 5})
		if rebuild {
			ctx.Dry(stPackage, "Tumblebug cmd_async(소스) "+restartKey, M{"command": []string{restartCmd}, "timeoutMinutes": 20})
		}
		ctx.Dry(stPackage, "Tumblebug cmd_async(소스) backup", M{"command": []string{backupCmd}, "timeoutMinutes": pkgMin})
		ctx.SetNodes("source", []M{{"nodeId": "g1-1", "publicIP": "<source.publicIP>", "sshPort": 22, "nodeUserName": "cb-user"}}, "")
		if _, err := ctx.WriteText(stPackage, "MANIFEST.txt", "(dry-run)"); err != nil {
			return nil, err
		}
		return M{"backup_out": outDir, "dry_run": true}, nil
	}

	node, err := refreshSourceAccess(ctx)
	if err != nil {
		return nil, err
	}
	bad, why, ident, err := inspectSource(ctx, node)
	if err != nil {
		return nil, err
	}
	marker := textx.RemotePath(outDir + "/" + ad.PackageMarker()) // 패키지가 다 만들어졌다는 표식 (어댑터 package_marker)
	r, err := ctx.Exec("source", "test -f "+marker, 2)
	if err != nil {
		return nil, err
	}
	if r.Reachable && r.RC == 0 {
		ctx.Log.Info(stPackage, "패키지가 이미 있어 backup 을 건너뜁니다: %s", outDir)
	} else {
		if bad && !rebuild && jsonx.Truthy(ctx.Case.Run()["golden"]) {
			return nil, Errf("골든 응답의 전제가 맞지 않습니다 (%s) — run.rebuild_before_backup 을 켜거나 소스를 사례의 LLM 설정으로 다시 띄우세요", why)
		}
		if bad {
			ctx.Log.Warn(stPackage, "LLM 설정 불일치 — %s — 재기동으로 맞춘다", why)
		}
		if rebuild {
			ctx.Log.Info(stPackage, "소스 재기동(%s) — 같은 LLM 설정 반영 · jar 안의 구 설정 제거", restartKey)
			if _, err := runRemote(ctx, restartKey, restartCmd, 20, nil); err != nil {
				return nil, err
			}
		}
		ctx.Log.Info(stPackage, "backup → %s (golden=%s)", outDir, golden)
		// backup 은 비어 있지 않은 출력 폴더를 거부하므로, 앞선 시도가 남긴 폴더는 옆으로 옮기고 다시 보낸다
		moveOut := func(try int) error { return moveAside(ctx, stPackage, "source", outDir, try) }
		if _, err := runRemote(ctx, "backup", backupCmd, pkgMin, moveOut); err != nil {
			return nil, err
		}
	}
	env := map[string]string{}
	// checksums.sha256 은 인터페이스(이관 뒤 대조)라 꼭 있어야 하고, 나머지는 있는 워크로드만 증적으로 받는다.
	for _, name := range []string{"checksums.sha256", "MANIFEST.txt", "package.env", "package-files.tsv"} {
		text, ok, err := ctx.ReadText("source", outDir+"/"+name)
		if err != nil {
			return nil, err
		}
		if !ok {
			if name == "checksums.sha256" {
				return nil, Errf("패키지 %s 에 checksums.sha256 이 없습니다 — 워크로드 backup 이 체크섬을 남겨야 이관 뒤 대조할 수 있습니다", outDir)
			}
			continue
		}
		if _, err := ctx.WriteText(stPackage, name, text); err != nil {
			return nil, err
		}
		if name == "package.env" {
			for _, line := range strings.Split(text, "\n") {
				if k, v, ok := strings.Cut(line, "="); ok {
					env[k] = v
				}
			}
		}
	}
	or := func(k string) string {
		if v, ok := env[k]; ok {
			return v
		}
		return "?"
	}
	// 어댑터 package_expect 의 값과 다르면 경고만 한다 (예: 실행 파일 층이 패키지에 들어 있어야 하는 워크로드 APP_STATUS=included)
	expect := ad.PackageExpect()
	for _, k := range sortedKeys(expect) {
		if got := or(k); got != expect[k] {
			ctx.Log.Warn(stPackage, "패키지 package.env 의 %s=%s (기대 %s)", k, got, expect[k])
		}
	}
	ctx.Log.Info(stPackage, "패키지 %s 준비됨 (package.env 키 %d개)", outDir, len(env))
	return M{"backup_out": outDir, "package_env": env, "code_ref": ident["code_ref"]}, nil
}

func errOr(err error, def string) string {
	if err != nil {
		return err.Error()
	}
	return def
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
