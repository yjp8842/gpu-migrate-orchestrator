// Package logx 는 콘솔 + run/log.txt + run/http.jsonl 을 쓴다.
// 비밀값(개인키 · 비밀번호 · Authorization)은 어디에도 평문으로 남기지 않는다.
package logx

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"gpu-migrate-orchestrator/internal/jsonx"
)

var masks = []struct {
	re  *regexp.Regexp
	rep string
}{
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`), "<PRIVATE KEY>"},
	{regexp.MustCompile(`(?i)("(?:privateKey|password|nodeUserPassword|secretAccessKey|sessionToken|accessKeyId)"\s*:\s*")[^"]*(")`), "${1}<masked>${2}"},
	{regexp.MustCompile(`((?:VERIFY_ADMIN_PASSWORD|LOCAL_ADMIN_PASSWORD|AWS_SECRET_ACCESS_KEY|AWS_SESSION_TOKEN|AWS_ACCESS_KEY_ID|GMP_[A-Z_]*PASSWORD)=)[^\s'"]+`), "${1}<masked>"},
	{regexp.MustCompile(`(?i)(Authorization:\s*Basic\s+)\S+`), "${1}<masked>"},
}

// Mask 는 로그 · 증적에 쓰기 전에 비밀값을 가린다.
func Mask(s string) string {
	for _, m := range masks {
		s = m.re.ReplaceAllString(s, m.rep)
	}
	return s
}

// Log 는 한 run 의 로그다. RunDir 이 비면 콘솔에만 쓴다.
type Log struct {
	Verbose bool // INFO 도 콘솔에
	Silent  bool // 콘솔에 아무것도 (시험용)
	mu      sync.Mutex
	fh      *os.File
	http    *os.File
}

// New 는 runDir 아래 log.txt · http.jsonl 을 연다 (runDir 이 비면 콘솔만).
func New(runDir string, verbose bool) (*Log, error) {
	l := &Log{Verbose: verbose}
	if runDir == "" {
		return l, nil
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return nil, err
	}
	var err error
	if l.fh, err = os.OpenFile(filepath.Join(runDir, "log.txt"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err != nil {
		return nil, err
	}
	if l.http, err = os.OpenFile(filepath.Join(runDir, "http.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err != nil {
		return nil, err
	}
	return l, nil
}

// Console 은 콘솔에만 쓰는 로그다.
func Console() *Log { return &Log{Verbose: true} }

// Quiet 는 아무것도 찍지 않는 로그다 (시험용).
func Quiet() *Log { return &Log{Silent: true} }

func (l *Log) emit(level, stage, msg string) {
	if l == nil {
		return
	}
	ts := time.Now().Format("15:04:05")
	prefix := "[" + ts + "] "
	if stage != "" {
		prefix += "[" + stage + "] "
	}
	if level != "INFO" {
		prefix += level + " "
	}
	line := prefix + Mask(msg)
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.Silent:
	case level != "INFO":
		fmt.Fprintln(os.Stderr, line)
	case l.Verbose:
		fmt.Fprintln(os.Stdout, line)
	}
	if l.fh != nil {
		fmt.Fprintln(l.fh, line)
	}
}

// Info · Warn · Error 는 단계 이름과 함께 한 줄을 쓴다.
func (l *Log) Info(stage, format string, a ...any)  { l.emit("INFO", stage, sprintf(format, a...)) }
func (l *Log) Warn(stage, format string, a ...any)  { l.emit("WARN", stage, sprintf(format, a...)) }
func (l *Log) Error(stage, format string, a ...any) { l.emit("ERROR", stage, sprintf(format, a...)) }

func sprintf(format string, a ...any) string {
	if len(a) == 0 {
		return format
	}
	return fmt.Sprintf(format, a...)
}

// HTTP 는 요청 한 건을 http.jsonl 에 쓴다 — 본문은 해시만 (내용은 남기지 않는다).
func (l *Log) HTTP(method, url string, status any, elapsed time.Duration, body any, note string) {
	if l == nil || l.http == nil {
		return
	}
	rec := jsonx.M{"ts": time.Now().Format("2006-01-02T15:04:05"), "method": method, "url": Mask(url),
		"status": status, "elapsed_ms": elapsed.Milliseconds()}
	if body != nil {
		var raw []byte
		if s, ok := body.(string); ok {
			raw = []byte(s)
		} else {
			raw, _ = jsonx.Marshal(body, false)
		}
		sum := sha256.Sum256(raw)
		rec["body_sha256"] = hex.EncodeToString(sum[:])[:16]
		rec["body_bytes"] = len(raw)
	}
	if note != "" {
		rec["note"] = Mask(note)
	}
	b, _ := jsonx.Marshal(rec, false)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.http.Write(append(b, '\n'))
}

// Close 는 파일을 닫는다.
func (l *Log) Close() {
	if l == nil {
		return
	}
	for _, f := range []*os.File{l.fh, l.http} {
		if f != nil {
			f.Close()
		}
	}
}
