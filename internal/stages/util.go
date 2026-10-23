package stages

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gpu-migrate-orchestrator/internal/jsonx"
	"gpu-migrate-orchestrator/internal/logx"
	"gpu-migrate-orchestrator/internal/textx"
	"gpu-migrate-orchestrator/internal/tumblebug"
)

// Error 는 단계 실패다.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// Errf 는 단계 실패를 만든다.
func Errf(format string, a ...any) error { return &Error{Msg: fmt.Sprintf(format, a...)} }

// Stage 는 단계 하나 — Preflight 는 부작용 없이 입력 · 선행 산출물 · 자원 상태를 확인한다.
type Stage struct {
	Preflight func(*Context) error
	Run       func(*Context) (M, error)
}

// Registry 는 단계 이름 → 단계.
var Registry = map[string]Stage{}

func register(name string, s Stage) { Registry[name] = s }

// TickLogger 는 폴링 콜백 — 매번 찍으면 시끄러워 every 번에 한 번만.
func TickLogger(log *logx.Log, stage string, every int) func(...any) {
	n := 0
	return func(args ...any) {
		n++
		if n%every != 1 && every != 1 {
			return
		}
		var parts []string
		for _, a := range args {
			s := fmt.Sprint(a)
			if s != "" && s != "[]" && s != "map[]" {
				parts = append(parts, s)
			}
		}
		log.Info(stage, "… %s", strings.Join(parts, " · "))
	}
}

// RequireRunning 은 인프라가 Running 인지 확인한다.
func RequireRunning(tb Tumblebug, ns, id, what string) (M, error) {
	infra, err := tb.GetInfra(ns, id)
	if err != nil {
		return nil, err
	}
	if infra == nil {
		return nil, Errf("%s 인프라가 없습니다: %s/%s", what, ns, id)
	}
	if st := tumblebug.InfraState(infra); st != "Running" {
		return nil, Errf("%s 인프라 %s 가 Running 이 아닙니다 (지금 %s) — MapUI 나 control API 로 켜 주세요", what, id, st)
	}
	return infra, nil
}

// CmdResultOK 는 commandStatus 항목이 성공(Completed = 종료 0)인지.
func CmdResultOK(entry M) bool { return jsonx.S(entry, "status") == "Completed" }

// DescribeCmd 는 commandStatus 항목 요약.
func DescribeCmd(entry M) string {
	s := jsonx.S(entry, "status")
	d := jsonx.Str(jsonx.Or(entry["resultSummary"], entry["errorMessage"]))
	if d == "" {
		return s
	}
	return s + " — " + d
}

func num(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprint(v)
}

// ---------- 실패한 요청 다시 보내기 ----------

// retireAttempt 는 실패로 끝난 요청을 시도 기록(<key>_tries)에 남기고 키를 지운다 — 다음 시도는 새 ID 로 보낸다.
// 그 시도의 로컬 산출물은 이름에 .try<N> 을 붙여 둔다 (backup.stdout.txt → backup.try1.stdout.txt). 몇 번째 시도였는지 돌려준다.
func retireAttempt(ctx *Context, stage, key, id, status string, files ...string) int {
	tries := jsonx.AsList(ctx.State.Key(key + "_tries"))
	n := len(tries) + 1
	tries = append(tries, M{"id": id, "status": status, "at": time.Now().Format("2006-01-02T15:04:05")})
	ctx.State.SetKey(key+"_tries", tries)
	ctx.State.DelKey(key)
	dir := ctx.State.StageDir(stage)
	for _, f := range files {
		base, rest, _ := strings.Cut(f, ".")
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			os.Rename(filepath.Join(dir, f), filepath.Join(dir, fmt.Sprintf("%s.try%d.%s", base, n, rest)))
		}
	}
	return n
}

// moveAside 는 앞선 시도가 남긴 원격 폴더를 <폴더>.failed-<N> 으로 옮긴다. 지우지 않는 것은 원인을 볼 수 있게 하려는 것이다.
// 폴더가 없으면 아무것도 하지 않는다. 옮길 자리가 이미 있으면 덮지 않고 멈춘다.
func moveAside(ctx *Context, stage, role, path string, try int) error {
	path = strings.TrimRight(path, "/")
	aside := fmt.Sprintf("%s.failed-%d", path, try)
	src, dst := textx.RemotePath(path), textx.RemotePath(aside)
	out, err := ctx.ExecOK(role, fmt.Sprintf("if [ -e %s ]; then [ ! -e %s ] || { echo \"%s 가 이미 있습니다\" >&2; exit 3; }; mv %s %s && echo moved; fi", src, dst, aside, src, dst), 2)
	if err != nil {
		return err
	}
	if strings.Contains(out, "moved") {
		ctx.Log.Info(stage, "앞선 시도가 남긴 %s 를 %s 로 옮겼습니다", path, aside)
	}
	return nil
}
