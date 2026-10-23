package config

// 설정 · 사례 파일의 ${이름} · ${이름:-기본값} 을 환경변수로 채운다. 주소 · 계정 · 비밀번호 · 사무실 IP 를 저장소에 두지 않으려는 것이다.
// 어댑터는 셸 명령을 담고 있어 바꾸지 않는다. 값은 셸 환경변수 또는 프로젝트 폴더의 .env(gitignore)에서 온다 — 셸 쪽이 우선.

import (
	"os"
	"regexp"
	"sort"
	"strings"

	"gpu-migrate-orchestrator/internal/jsonx"
)

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func expandString(s string, missing map[string]bool) string {
	return envRef.ReplaceAllStringFunc(s, func(m string) string {
		g := envRef.FindStringSubmatch(m)
		if v := os.Getenv(g[1]); v != "" {
			return v
		}
		if g[2] != "" {
			return g[3]
		}
		missing[g[1]] = true
		return m
	})
}

func expandValue(v any, missing map[string]bool) any {
	switch t := v.(type) {
	case string:
		return expandString(t, missing)
	case map[string]any:
		for k, x := range t {
			t[k] = expandValue(x, missing)
		}
	case []any:
		for i, x := range t {
			t[i] = expandValue(x, missing)
		}
	}
	return v
}

// ExpandEnv 는 파일에서 읽은 값의 ${이름} 을 채운다. 비어 있는 변수는 이름을 모두 모아 한 번에 알린다.
func ExpandEnv(m jsonx.M, path string) error {
	missing := map[string]bool{}
	expandValue(map[string]any(m), missing)
	if len(missing) == 0 {
		return nil
	}
	names := make([]string, 0, len(missing))
	for n := range missing {
		names = append(names, n)
	}
	sort.Strings(names)
	return Errf("%s: 환경변수가 비어 있습니다: %s — 셸에서 export 하거나 프로젝트 폴더의 .env 에 적으세요 (견본 .env.example)", path, strings.Join(names, ", "))
}

// LoadDotEnv 는 KEY=VALUE 줄을 읽어 아직 없는 환경변수만 설정한다. 파일이 없으면 아무것도 하지 않는다.
func LoadDotEnv(path string) error {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || !envName.MatchString(k) {
			return Errf("%s:%d: KEY=VALUE 꼴이 아닙니다", path, i+1)
		}
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
	return nil
}
