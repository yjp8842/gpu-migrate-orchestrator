package stages

// 6. start — 타겟에서 단일 명령을 분리 실행하고 Tumblebug 원격 명령으로 끝을 기다린다.
//
// 왜 분리하나: 단일 명령은 드라이버 설치 뒤 재부팅한다. 원격 명령 세션이 끊기면 Tumblebug 는 일시 오류로 보고 명령 전체를
// 한 번 더 실행하므로(재개 유닛과 동시에 두 번째 bootstrap 이 돈다), 명령은 즉시 반환시키고(nohup setsid) 진행은 짧은 확인 명령으로 본다.
// 확인 명령은 읽기만 해서 다시 실행돼도 문제가 없다. 재부팅 중에는 닿지 않으므로 기다렸다가 다시 본다.
// 완료 신호는 어댑터 done 이 정한다 — results_rows(기본): results.csv 행 수 증가(verify 가 돌았다) · status_file: 상태 파일의
// 필드가 success 값이 됨(이번 기동 뒤 갱신분만). 재개 유닛이 결과 없이 끝나거나 실패하면, 또는 시간을 넘기면 실패.
// 기동 전에 단일 명령이 이미 돌고 있으면 다시 띄우지 않고 따라가며, 어댑터 existing_check 가 기존 설치를 찾으면 사례
// run.on_existing 에 따라 멈추거나(stop) existing_env 를 붙여 지우고 진행한다(clean).
// 검증용 관리자 비밀번호는 사례 run.env 의 VERIFY_ADMIN_PASSWORD 로 단일 명령에 함께 실린다 (로그에서는 가린다).

import (
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gpu-migrate-orchestrator/internal/jsonx"
	"gpu-migrate-orchestrator/internal/textx"
)

const stStart = "start"

func init() { register(stStart, Stage{Preflight: preStart, Run: runStart}) }

// Probe 는 기동 진행을 읽는 확인 스크립트다 (읽기만 한다). {proc64} 는 단일 명령을 찾는 pgrep 패턴을 base64 로 넣은 것이다 —
// 패턴을 그대로 쓰면 이 확인 명령 자신의 명령줄(bash -c '…')에 패턴이 들어 있어 pgrep 이 자기를 센다.
const Probe = `echo "EXIT=$(cat {exit} 2>/dev/null || echo -)"{unitProbe}{rowsProbe}
pat=$(echo {proc64} | base64 -d); echo "PROC=$(pgrep -fc "$pat" 2>/dev/null || echo 0)"
echo "UPTIME=$(cut -d. -f1 /proc/uptime)"
echo "LAST=$(tail -n 1 {run} 2>/dev/null | cut -c1-160)"{status}`

// unitProbe 는 재개 유닛(어댑터 resume_unit)이 있는 워크로드만 — 재부팅 뒤 이어서 도는 systemd 유닛의 상태.
const unitProbe = `
if [ -f /etc/systemd/system/{unit}.service ] || systemctl cat {unit} >/dev/null 2>&1; then echo UNIT_FILE=yes; else echo UNIT_FILE=no; fi
echo "UNIT=$(systemctl show {unit} -p ActiveState,SubState,Result,ExecMainStatus 2>/dev/null | tr '\n' ' ')"`

// rowsProbe 는 결과표(어댑터 results.csv)가 있는 워크로드만 — 행 수.
const rowsProbe = `
echo "ROWS=$(wc -l < {results} 2>/dev/null || echo 0)"`

// statusProbe 는 done.type=status_file 일 때 상태 파일의 갱신 시각과 필드 값을 읽는다 (JSON 의 "필드": "값" 한 줄).
const statusProbe = `
echo "SMTIME=$(stat -c %Y {spath} 2>/dev/null || echo 0)"
echo "SVAL=$(grep -oE '"{sfield}"[[:space:]]*:[[:space:]]*"[^"]*"' {spath} 2>/dev/null | head -1 | sed -E 's/.*:[[:space:]]*"([^"]*)"$/\1/')"`

// probeScript 는 어댑터 값으로 확인 스크립트를 채운다.
func probeScript(ctx *Context) string {
	ad := ctx.Adapter
	run, exit := ad.Logs()
	status := ""
	if d := ad.Done(); d.Type == "status_file" {
		status = strings.NewReplacer("{spath}", textx.RemotePath(d.Path), "{sfield}", d.Field).Replace(statusProbe)
	}
	up, rp := "", ""
	if unit := ad.S("resume_unit"); unit != "" {
		up = strings.ReplaceAll(unitProbe, "{unit}", unit)
	}
	if csv := jsonx.S(ad.M("results"), "csv"); csv != "" {
		rp = strings.ReplaceAll(rowsProbe, "{results}", textx.RemotePath(csv))
	}
	return strings.NewReplacer("{unitProbe}", up, "{rowsProbe}", rp,
		"{proc64}", base64.StdEncoding.EncodeToString([]byte(ad.ProcPattern())), "{exit}", textx.RemotePath(exit),
		"{run}", textx.RemotePath(run), "{status}", status).Replace(Probe)
}

func preStart(ctx *Context) error {
	if ctx.DryRun {
		return nil
	}
	if _, err := ctx.Node("target"); err != nil {
		return err
	}
	if ctx.State.Stage(stTransfer).Status != "done" {
		return Errf("transfer 단계가 끝나지 않았습니다")
	}
	return nil
}

func launcher(entry, runLog, exitFile string) string {
	exit, run := textx.RemotePath(exitFile), textx.RemotePath(runLog)
	inner := entry + "; echo $? > " + exit
	return "rm -f " + exit + "; nohup setsid bash -c " + textx.Quote(inner) + " > " + run + " 2>&1 < /dev/null & echo launched $(date +%s)"
}

// procCount 는 단일 명령이 지금 돌고 있는지 센다 (패턴은 base64 로 넘겨 자기 자신을 세지 않는다).
func procCount(ctx *Context) (int, error) {
	pat := base64.StdEncoding.EncodeToString([]byte(ctx.Adapter.ProcPattern()))
	out, err := ctx.ExecOK("target", fmt.Sprintf(`pat=$(echo %s | base64 -d); pgrep -fc "$pat" 2>/dev/null || echo 0`, pat), 2)
	if err != nil {
		return 0, err
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n, nil
}

// existingInstall 은 어댑터의 existing_check 로 타겟에 기존 설치가 있는지 본다. 있으면 사례 run.on_existing 에 따라
// 멈추거나(stop, 기본), 어댑터의 existing_env(예: FORCE_CLEAN=1)를 단일 명령에 붙인다(clean). 붙일 환경변수를 돌려준다.
func existingInstall(ctx *Context) (string, error) {
	check := ctx.Adapter.S("existing_check")
	if check == "" {
		return "", nil
	}
	r, err := ctx.Exec("target", check, 2)
	if err != nil {
		return "", err
	}
	if !r.Reachable || r.RC != 0 {
		return "", nil // 0 이 아니면 기존 설치 없음
	}
	what := textx.Tail(strings.TrimSpace(r.Stdout), 200)
	if jsonx.SOr(ctx.Case.Run(), "on_existing", "stop") != "clean" {
		return "", Errf("타겟에 기존 설치가 있습니다 (%s) — 새 VM 으로 하거나, 사례 run.on_existing: clean 으로 지우고 진행하세요", what)
	}
	env := ctx.Adapter.S("existing_env")
	if env == "" {
		return "", Errf("타겟에 기존 설치가 있는데 어댑터에 existing_env(지우고 진행할 환경변수)가 없습니다 (%s)", what)
	}
	ctx.Log.Warn(stStart, "타겟에 기존 설치가 있습니다 (%s) — run.on_existing=clean 이라 %s 로 진행", what, env)
	return env, nil
}

func launch(ctx *Context) error {
	extra, err := existingInstall(ctx)
	if err != nil {
		return err
	}
	env := ctx.EnvStr()
	if extra != "" {
		env = strings.TrimSpace(env + " " + extra)
	}
	entry, err := ctx.Remote("entry", map[string]string{"env": env})
	if err != nil {
		return err
	}
	runLog, exitFile := ctx.Adapter.Logs()
	ctx.State.SetKey("start_launched", M{"at": time.Now().Format("2006-01-02T15:04:05"), "entry": entry}) // 부작용 전에 기록
	r, err := ctx.Exec("target", launcher(entry, runLog, exitFile), 2)
	if err != nil {
		return err
	}
	if !r.Reachable || !strings.Contains(r.Stdout, "launched") {
		return Errf("단일 명령 분리 실행에 실패했습니다: %s", textx.Tail(strings.TrimSpace(r.Detail+" "+r.Stderr), 300))
	}
	if f := strings.Fields(r.Stdout[strings.Index(r.Stdout, "launched"):]); len(f) > 1 {
		if ep, err := strconv.ParseInt(f[1], 10, 64); err == nil {
			ctx.State.SetKey("start_launch_epoch", ep) // 타겟 시계 — 상태 파일이 이 뒤에 갱신됐는지 본다
		}
	}
	ctx.Log.Info(stStart, "단일 명령 분리 실행 — 이후는 확인 명령으로 따라간다 (재부팅 재개 포함)")
	return nil
}

func runStart(ctx *Context) (M, error) {
	ad := ctx.Adapter
	unit := ad.S("resume_unit")
	results := jsonx.S(ad.M("results"), "csv")
	done := ad.Done()
	runLog, exitFile := ad.Logs()
	if ctx.DryRun {
		entry, err := ctx.Remote("entry", nil)
		if err != nil {
			return nil, err
		}
		ctx.Dry(stStart, "Tumblebug cmd_sync(타겟) nohup setsid", M{"entry": entry, "launcher": launcher(entry, runLog, exitFile),
			"done": M{"type": done.Type, "path": done.Path, "field": done.Field, "success": done.Success}, "probe": probeScript(ctx)})
		return M{"dry_run": true}, nil
	}
	if ctx.State.Key("results_baseline_rows") == nil && results != "" {
		out, err := ctx.ExecOK("target", fmt.Sprintf("wc -l < %s 2>/dev/null || echo 0", results), 2)
		if err != nil {
			return nil, err
		}
		n := 0
		if f := strings.Fields(out); len(f) > 0 {
			n, _ = strconv.Atoi(f[0])
		}
		ctx.State.SetKey("results_baseline_rows", n)
	}
	baseline := jsonx.Int(ctx.State.Key("results_baseline_rows"))
	if !ctx.State.HasKey("start_launched") {
		// 이미 돌고 있으면 다시 띄우지 않는다 — 두 번째 단일 명령이 같은 설치를 동시에 건드린다
		if n, err := procCount(ctx); err != nil {
			return nil, err
		} else if n > 0 {
			ctx.Log.Warn(stStart, "타겟에서 단일 명령이 이미 돌고 있습니다 (%d개) — 다시 띄우지 않고 따라간다", n)
			ctx.State.SetKey("start_launched", M{"at": time.Now().Format("2006-01-02T15:04:05"), "already_running": n})
		} else if err := launch(ctx); err != nil {
			return nil, err
		}
	} else {
		ctx.Log.Info(stStart, "이미 분리 실행했음 — 확인만 이어 간다")
	}
	tmin := jsonx.Int(ctx.Case.Run()["timeout_start_min"])
	if tmin == 0 {
		tmin = ctx.Settings.TimeoutMin("start_min")
	}
	timeout := time.Duration(tmin) * time.Minute
	poll := time.Duration(ctx.Settings.Pacing["start_poll_sec"]) * time.Second
	if poll <= 0 {
		poll = time.Minute
	}
	t0 := time.Now()
	unitSeen, unitActiveSeen := ctx.State.HasKey("unit_seen"), ctx.State.HasKey("unit_active_seen")
	grace, lastLine, away := 0, "", false
	var status, why string
	probe := probeScript(ctx)
	launchEpoch := int64(jsonx.Int(ctx.State.Key("start_launch_epoch")))
	for {
		r, err := ctx.Exec("target", probe, 2)
		if err != nil || !r.Reachable {
			if !away {
				ctx.Log.Info(stStart, "원격 명령이 닿지 않음 — 재부팅 중으로 보고 기다립니다")
				away = true
			}
			if time.Since(t0) > timeout {
				status, why = "failed", "타임아웃 (원격 명령이 닿지 않음)"
				break
			}
			ctx.Sleep(poll)
			continue
		}
		if away {
			ctx.Log.Info(stStart, "다시 닿음")
			away = false
		}
		kv := textx.ParseKV(r.Stdout)
		rows, _ := strconv.Atoi(strings.TrimSpace(kv["ROWS"]))
		us := textx.ParseKV(strings.ReplaceAll(kv["UNIT"], " ", "\n"))
		active := us["ActiveState"]
		if kv["UNIT_FILE"] == "yes" || active == "activating" || active == "active" {
			if !unitSeen {
				ctx.State.SetKey("unit_seen", true)
			}
			unitSeen = true
		}
		if active == "activating" || active == "active" {
			if !unitActiveSeen {
				ctx.State.SetKey("unit_active_seen", true)
			}
			unitActiveSeen = true
		}
		if l := kv["LAST"]; l != "" && l != lastLine {
			lastLine = l
			ctx.Log.Info(stStart, "타겟: %s", l)
		}
		procs, _ := strconv.Atoi(strings.TrimSpace(kv["PROC"]))
		exit := kv["EXIT"]
		if done.Type == "status_file" {
			mt, _ := strconv.ParseInt(strings.TrimSpace(kv["SMTIME"]), 10, 64)
			val := strings.TrimSpace(kv["SVAL"])
			if mt > 0 && mt >= launchEpoch { // 이번 기동 뒤에 갱신된 상태 파일만 본다 (옛 run 의 success 를 읽지 않게)
				if contains(done.Success, val) {
					status, why = "done", fmt.Sprintf("%s 의 %s=%s", done.Path, done.Field, val)
					break
				}
				if contains(done.Failure, val) {
					status, why = "failed", fmt.Sprintf("%s 의 %s=%s — %s 확인", done.Path, done.Field, val, runLog)
					break
				}
			}
		} else if rows > baseline {
			status, why = "done", fmt.Sprintf("검증 결과 행 %d→%d", baseline, rows)
			break
		}
		if !unitSeen && exit != "-" && exit != "" && procs == 0 {
			// 상태 파일로 판정하는 워크로드는 종료 코드 0 만으로 끝내지 않는다 — 상태 파일이 success 가 될 때까지 몇 번 더 본다
			if exit == "0" && done.Type == "status_file" {
				if grace++; grace >= 3 {
					status, why = "failed", fmt.Sprintf("단일 명령은 0 으로 끝났으나 %s 의 %s 가 성공 값이 아님 (%s)", done.Path, done.Field, strings.TrimSpace(kv["SVAL"]))
					break
				}
				ctx.Sleep(poll)
				continue
			}
			status = "failed"
			if exit == "0" {
				status = "done"
			}
			why = fmt.Sprintf("단일 명령 종료 코드 %s (재개 유닛 없이 끝남)", exit)
			break
		}
		if unitSeen && (active == "inactive" || active == "failed") && procs == 0 && unitActiveSeen {
			if us["Result"] == "exit-code" || (us["ExecMainStatus"] != "" && us["ExecMainStatus"] != "0") {
				status, why = "failed", "재개 유닛 실패: "+kv["UNIT"]
				break
			}
			if grace++; grace >= 3 {
				status, why = "failed", "재개 유닛은 끝났으나 완료 판정(결과 행 · 상태 파일)이 나오지 않음 ("+kv["UNIT"]+")"
				break
			}
		}
		if time.Since(t0) > timeout {
			status, why = "failed", fmt.Sprintf("타임아웃 %d분 — 마지막: %s", tmin, lastLine)
			break
		}
		ctx.Sleep(poll)
	}
	// 로그 회수 (성공 · 실패 모두)
	d := ctx.State.StageDir(stStart)
	if ok, err := ctx.FetchFile("target", runLog, filepath.Join(d, "tc-run.log")); err != nil || !ok {
		ctx.Log.Warn(stStart, "tc-run.log 회수 실패: %s", errOr(err, "파일 없음"))
	} else {
		ctx.State.Artifact(stStart, "tc-run.log", filepath.Join(d, "tc-run.log"))
	}
	if unit != "" {
		if r, err := ctx.Exec("target", fmt.Sprintf("sudo journalctl -u %s --no-pager -o short-iso 2>/dev/null | tail -n 400", unit), 3); err == nil && strings.TrimSpace(r.Stdout) != "" {
			ctx.WriteText(stStart, "resume-journal.log", r.Stdout)
		}
	}
	elapsed := int(time.Since(t0).Seconds())
	ctx.State.SetKey("start_outcome", M{"status": status, "why": why, "elapsed_sec": elapsed})
	if status != "done" {
		return nil, Errf("타겟 기동이 끝나지 않았습니다 — %s", why)
	}
	ctx.Log.Info(stStart, "타겟 기동 완료 — %s (%d초)", why, elapsed)
	return M{"why": why, "elapsed_sec": elapsed}, nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
