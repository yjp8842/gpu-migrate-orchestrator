package api

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// migrate 를 별도 프로세스로 띄운다 — 이관은 Facade 서버 안에서 돌지 않는다.
// 자기 프로세스 그룹으로 떼어 내고 출력은 파일로 받아서, serve 를 끄거나 다시 켜도 도는 run 은 계속 간다.
// 진행 상황은 run 폴더(state.json · log.txt)로 보고, 도는지는 .lock 의 pid 로 안다 (runs.go runPID).

var runLine = regexp.MustCompile(`run (\d{8}-\d{6}) · 사례 ([a-z0-9][a-z0-9._-]*)`)

type started struct {
	RunID    string `json:"runId"`
	CaseName string `json:"caseName"`
	ExitCode *int   `json:"exitCode,omitempty"`
	output   string
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// startMigrate 는 migrate 를 띄우고 첫 줄 「run <id> · 사례 <이름>」이 나오면 돌려준다.
// wait 이면 끝날 때까지 기다린다 (dry-run · note 같은 짧은 명령). timeout 이 지나도 프로세스는 그대로 둔다.
func (s *Server) startMigrate(args []string, wait bool, timeout time.Duration) (started, error) {
	if _, err := os.Stat(s.Exe); err != nil {
		return started{}, fmt.Errorf("migrate 실행 파일이 없습니다: %s", s.Exe)
	}
	dir := filepath.Join(consoleCases(s.Root), ".proc")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return started{}, err
	}
	outPath := filepath.Join(dir, time.Now().Format("20060102-150405.000000")+".log")
	out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return started{}, err
	}
	cmd := exec.Command(s.Exe, args...)
	cmd.Dir = s.Root
	cmd.Env = os.Environ()
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		out.Close()
		return started{}, err
	}
	done := make(chan int, 1)
	go func() {
		err := cmd.Wait()
		out.Close()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			code = -1
		}
		done <- code
	}()
	read := func() string {
		b, _ := os.ReadFile(outPath)
		return string(b)
	}
	deadline := time.After(timeout)
	var res started
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case code := <-done:
			text := read()
			if m := runLine.FindStringSubmatch(text); m != nil {
				res.RunID, res.CaseName = m[1], m[2]
			}
			res.ExitCode, res.output = &code, text
			if res.RunID == "" && code != 0 && code != 1 {
				msg := tail(text, 6)
				if msg == "" {
					msg = fmt.Sprintf("migrate 종료 코드 %d", code)
				}
				return res, errors.New(msg)
			}
			os.Remove(outPath)
			return res, nil
		case <-tick.C:
			if res.RunID == "" {
				if m := runLine.FindStringSubmatch(read()); m != nil {
					res.RunID, res.CaseName = m[1], m[2]
					if !wait {
						return res, nil
					}
				}
			}
		case <-deadline:
			if res.RunID != "" {
				return res, nil
			}
			return res, fmt.Errorf("migrate 응답이 %s 안에 없습니다: %s", timeout, strings.Join(args, " "))
		}
	}
}

// stopRun 은 도는 run 에 SIGINT — migrate 는 받으면 지금 단계를 「중단」으로 적고 끝낸다 (--resume 으로 이어 감).
func stopRun(pid int) bool {
	return pid > 0 && syscall.Kill(pid, syscall.SIGINT) == nil
}
