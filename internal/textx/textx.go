// Package textx 는 단계들이 같이 쓰는 작은 문자열 도구다.
package textx

import (
	"math"
	"regexp"
	"strconv"
	"strings"

	"gpu-migrate-orchestrator/internal/jsonx"
)

var shellSafe = regexp.MustCompile(`^[\w@%+=:,./-]+$`)

// Quote 는 파이썬 shlex.quote 와 같다.
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// RemotePath 는 원격 셸에 넘길 경로를 인용한다. 앞의 `~/` 는 따옴표 밖에 둬서 홈 디렉토리로 풀리게 한다.
// (경로 전체를 따옴표로 감싸면 `~` 가 글자 그대로 남아 홈 아래 파일을 못 찾는다.)
func RemotePath(p string) string {
	if p == "~" {
		return "~"
	}
	if strings.HasPrefix(p, "~/") {
		return "~/" + Quote(p[2:])
	}
	return Quote(p)
}

// ParseKV 는 `K=V` 줄을 읽는다 (# 로 시작하는 줄은 건너뛴다, 값의 큰따옴표는 벗긴다).
func ParseKV(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.Contains(line, "=") || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	return out
}

// ParseJSON 은 실패하면 nil.
func ParseJSON(text string) any {
	v, err := jsonx.Decode([]byte(text))
	if err != nil {
		return nil
	}
	return v
}

// MibToGib 은 nvidia-smi 의 MiB 를 GiB 로 반올림한다 (15360 → 15).
func MibToGib(mib string) int {
	n, err := strconv.ParseFloat(strings.TrimSpace(mib), 64)
	if err != nil {
		return 0
	}
	return int(math.Floor(n/1024 + 0.5))
}

// Head 는 앞에서 n 글자 (바이트가 아니라 글자 단위 — 한글이 깨지지 않게).
func Head(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// Tail 은 뒤에서 n 글자.
func Tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// IsDigits 는 숫자로만 된 문자열인지.
func IsDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
