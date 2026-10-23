package stages

// 6. start — 타겟에서 단일 명령을 분리 실행하고 Tumblebug 원격 명령으로 끝을 기다린다.
//
// 왜 분리하나: 단일 명령은 드라이버 설치 뒤 재부팅한다. 원격 명령 세션이 끊기면 Tumblebug 는 일시 오류로 보고 명령 전체를
// 한 번 더 실행하므로(재개 유닛과 동시에 두 번째 bootstrap 이 돈다), 명령은 즉시 반환시키고(nohup setsid) 진행은 짧은 확인 명령으로 본다.
// 확인 명령은 읽기만 해서 다시 실행돼도 문제가 없다. 재부팅 중에는 닿지 않으므로 기다렸다가 다시 본다.
// 완료 신호: results.csv 행 수 증가(verify 가 돌았다). 행 없이 재개 유닛이 끝나거나 실패하면, 또는 시간을 넘기면 실패.
// 검증용 관리자 비밀번호는 사례 run.env 의 VERIFY_ADMIN_PASSWORD 로 단일 명령에 함께 실린다 (로그에서는 가린다).

import (
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

// Probe 는 기동 진행을 읽는 확인 스크립트다 (읽기만 한다).
const Probe = `echo "EXIT=$(cat ~/tc-run.exit 2>/dev/null || echo -)"
if [ -f /etc/systemd/system/{unit}.service ] || systemctl cat {unit} >/dev/null 2>&1; then echo UNIT_FILE=yes; else echo UNIT_FILE=no; fi
echo "UNIT=$(systemctl show {unit} -p ActiveState,SubState,Result,ExecMainStatus 2>/dev/null | tr '\n' ' ')"
echo "ROWS=$(wc -l < {results} 2>/dev/null || echo 0)"
echo "PROC=$(pgrep -fc 'scripts/(local|compose)/(bootstrap|deploy).sh' 2>/dev/null || echo 0)"
echo "UPTIME=$(cut -d. -f1 /proc/uptime)"
echo "LAST=$(tail -n 1 ~/tc-run.log 2>/dev/null | cut -c1-160)"`

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

func launcher(entry string) string {
	inner := entry + "; echo $? > ~/tc-run.exit"
	return "rm -f ~/tc-run.exit; nohup setsid bash -c " + textx.Quote(inner) + " > ~/tc-run.log 2>&1 < /dev/null & echo launched"
}

func launch(ctx *Context) error {
	entry, err := ctx.Remote("entry", nil)
	if err != nil {
		return err
	}
	ctx.State.SetKey("start_launched", M{"at": time.Now().Format("2006-01-02T15:04:05"), "entry": entry}) // 부작용 전에 기록
	r, err := ctx.Exec("target", launcher(entry), 2)
	if err != nil {
		return err
	}
	if !r.Reachable || !strings.Contains(r.Stdout, "launched") {
		return Errf("단일 명령 분리 실행에 실패했습니다: %s", textx.Tail(strings.TrimSpace(r.Detail+" "+r.Stderr), 300))
	}
	ctx.Log.Info(stStart, "단일 명령 분리 실행 — 이후는 확인 명령으로 따라간다 (재부팅 재개 포함)")
	return nil
}

func runStart(ctx *Context) (M, error) {
	ad := ctx.Adapter
	unit := ad.S("resume_unit")
	results := jsonx.S(ad.M("results"), "csv")
	if ctx.DryRun {
		entry, err := ctx.Remote("entry", nil)
		if err != nil {
			return nil, err
		}
		ctx.Dry(stStart, "Tumblebug cmd_sync(타겟) nohup setsid", M{"entry": entry, "launcher": launcher(entry)})
		return M{"dry_run": true}, nil
	}
	if ctx.State.Key("results_baseline_rows") == nil {
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
		if err := launch(ctx); err != nil {
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
	probe := strings.NewReplacer("{unit}", unit, "{results}", results).Replace(Probe)
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
		if rows > baseline {
			status, why = "done", fmt.Sprintf("검증 결과 행 %d→%d", baseline, rows)
			break
		}
		if !unitSeen && exit != "-" && exit != "" && procs == 0 {
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
				status, why = "failed", "재개 유닛은 끝났으나 검증 결과 행이 늘지 않음 ("+kv["UNIT"]+")"
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
	if ok, err := ctx.FetchFile("target", jsonx.S(ad.M("logs"), "run"), filepath.Join(d, "tc-run.log")); err != nil || !ok {
		ctx.Log.Warn(stStart, "tc-run.log 회수 실패: %s", errOr(err, "파일 없음"))
	} else {
		ctx.State.Artifact(stStart, "tc-run.log", filepath.Join(d, "tc-run.log"))
	}
	if r, err := ctx.Exec("target", fmt.Sprintf("sudo journalctl -u %s --no-pager -o short-iso 2>/dev/null | tail -n 400", unit), 3); err == nil && strings.TrimSpace(r.Stdout) != "" {
		ctx.WriteText(stStart, "resume-journal.log", r.Stdout)
	}
	elapsed := int(time.Since(t0).Seconds())
	ctx.State.SetKey("start_outcome", M{"status": status, "why": why, "elapsed_sec": elapsed})
	if status != "done" {
		return nil, Errf("타겟 기동이 끝나지 않았습니다 — %s", why)
	}
	ctx.Log.Info(stStart, "타겟 기동 완료 — %s (%d초)", why, elapsed)
	return M{"why": why, "elapsed_sec": elapsed}, nil
}
