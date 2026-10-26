package api

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"gpu-migrate-orchestrator/internal/logx"
)

// headers 는 모든 응답에 붙는다 — 다른 사이트에 끼워 넣기 · 형식 추측 · 외부 자원 불러오기를 막는다.
func headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// lockedPID 는 run 을 실제로 잡고 있는 migrate 의 pid 다 — .lock 의 flock 이 잡혀 있을 때만.
// 중지(SIGINT)처럼 다른 프로세스에 신호를 보낼 때는 pid 가 살아 있는지만으로는 부족하다 (pid 재사용).
func lockedPID(dir string) int {
	p := filepath.Join(dir, ".lock")
	f, err := os.Open(p)
	if err != nil {
		return 0
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err == nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN) // 아무도 안 잡고 있다 — 끝난 run
		return 0
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return 0
	}
	return pid
}

// 내보내는 증적에서 비밀값을 한 번 더 가린다 — 가림이 들어가기 전에 만든 지난 run 의 증적도 화면 · ZIP 으로는 가려서 나간다.
var binaryExt = map[string]bool{".gz": true, ".tgz": true, ".tar": true, ".zip": true, ".png": true, ".jpg": true, ".pdf": true}

func maskContent(name string, b []byte) []byte {
	if binaryExt[strings.ToLower(filepath.Ext(name))] || !utf8.Valid(b) {
		return b
	}
	return []byte(logx.Mask(string(b)))
}

// validIP 는 화면이 링크로 만드는 공인 IP 가 IP 꼴인지 본다.
func validIP(s string) bool { return net.ParseIP(s) != nil }
