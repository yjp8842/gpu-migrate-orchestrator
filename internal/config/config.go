// Package config 는 사례(cases/) · 어댑터(adapters/) · 실행 환경(settings.yaml)을 읽고 검사한다.
//
// 사례 파일에는 이 운행에서 바뀌는 값만 둔다. 워크로드가 다른 점은 전부 어댑터에, 주소 · 계정 · 타임아웃은 설정에.
// 설정 · 사례의 주소 · 계정 · 비밀번호 · 사무실 IP 는 ${이름} 으로 적고 환경변수에서 받는다 (env.go).
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"gpu-migrate-orchestrator/internal/jsonx"
	"gpu-migrate-orchestrator/internal/textx"
)

// Stages 는 사례 1건의 단계 순서다.
var Stages = []string{"collect", "recommend", "provision", "package", "transfer", "start", "verify", "cleanup"}

// Error 는 설정 오류다.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// Errf 는 설정 오류를 만든다.
func Errf(format string, a ...any) error { return &Error{Msg: fmt.Sprintf(format, a...)} }

// ProjectRoot 는 adapters/ 가 있는 프로젝트 폴더다. GMP_ROOT → 실행 파일 폴더 → 현재 폴더부터 위로 찾는다.
func ProjectRoot() string {
	if r := os.Getenv("GMP_ROOT"); r != "" {
		return r
	}
	isRoot := func(d string) bool {
		st, err := os.Stat(filepath.Join(d, "adapters"))
		return err == nil && st.IsDir()
	}
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		if d := filepath.Dir(exe); isRoot(d) {
			return d
		}
	}
	if wd, err := os.Getwd(); err == nil {
		for d := wd; ; d = filepath.Dir(d) {
			if isRoot(d) {
				return d
			}
			if filepath.Dir(d) == d {
				break
			}
		}
		return wd
	}
	return "."
}

// LoadYAML 은 맵 형식 YAML 을 읽는다. 키 순서가 필요한 곳을 위해 노드도 돌려준다.
func LoadYAML(path string) (jsonx.M, *yaml.Node, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, Errf("파일이 없습니다: %s", path)
		}
		return nil, nil, err
	}
	var node yaml.Node
	if err := yaml.Unmarshal(b, &node); err != nil {
		return nil, nil, Errf("%s: YAML 을 읽지 못했습니다: %v", path, err)
	}
	var raw any
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return nil, nil, Errf("%s: YAML 을 읽지 못했습니다: %v", path, err)
	}
	if raw == nil {
		raw = jsonx.M{}
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, nil, Errf("맵 형식이어야 합니다: %s", path)
	}
	return m, &node, nil
}

// nodeKeys 는 YAML 노드에서 경로의 맵 키를 파일 순서대로 돌려준다.
func nodeKeys(n *yaml.Node, path ...string) []string {
	if n == nil {
		return nil
	}
	cur := n
	if cur.Kind == yaml.DocumentNode && len(cur.Content) > 0 {
		cur = cur.Content[0]
	}
	for _, p := range path {
		if cur.Kind != yaml.MappingNode {
			return nil
		}
		var next *yaml.Node
		for i := 0; i+1 < len(cur.Content); i += 2 {
			if cur.Content[i].Value == p {
				next = cur.Content[i+1]
				break
			}
		}
		if next == nil {
			return nil
		}
		cur = next
	}
	if cur.Kind != yaml.MappingNode {
		return nil
	}
	var keys []string
	for i := 0; i+1 < len(cur.Content); i += 2 {
		keys = append(keys, cur.Content[i].Value)
	}
	return keys
}

func need(d jsonx.M, key, where string) (any, error) {
	v, ok := d[key]
	if !ok || v == nil || v == "" {
		return nil, Errf("%s 에 '%s' 가 필요합니다", where, key)
	}
	return v, nil
}

func needM(d jsonx.M, key, where string) (jsonx.M, error) {
	v, err := need(d, key, where)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, Errf("%s 의 '%s' 는 맵이어야 합니다", where, key)
	}
	return m, nil
}

func enum(value any, allowed []string, where string) error {
	s := jsonx.Str(value)
	for _, a := range allowed {
		if s == a {
			return nil
		}
	}
	return Errf("%s 는 %v 중 하나여야 합니다 (지금 %q)", where, allowed, s)
}

func setDefault(m jsonx.M, key string, v any) any {
	if cur, ok := m[key]; ok {
		return cur
	}
	m[key] = v
	return v
}

// KV 는 순서가 있는 환경변수 한 쌍이다.
type KV struct{ K, V string }

// EnvString 은 [{K V}] 를 `K=V K2=V2` 로 (값은 셸 인용). 단일 명령 앞에 붙인다.
func EnvString(env []KV) string {
	parts := make([]string, 0, len(env))
	for _, kv := range env {
		parts = append(parts, kv.K+"="+textx.Quote(kv.V))
	}
	return strings.Join(parts, " ")
}

// Render 는 `{name}` 자리를 채운다. `{{` · `}}` 는 중괄호 글자. 값이 없는 자리가 있으면 오류.
func Render(tmpl string, values map[string]string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(tmpl); {
		c := tmpl[i]
		switch {
		case c == '{' && i+1 < len(tmpl) && tmpl[i+1] == '{':
			b.WriteByte('{')
			i += 2
		case c == '}' && i+1 < len(tmpl) && tmpl[i+1] == '}':
			b.WriteByte('}')
			i += 2
		case c == '{':
			j := strings.IndexByte(tmpl[i:], '}')
			if j < 0 {
				return "", Errf("템플릿의 중괄호가 닫히지 않았습니다: %s", tmpl)
			}
			name := tmpl[i+1 : i+j]
			v, ok := values[name]
			if !ok {
				return "", Errf("템플릿 변수 '{%s}' 의 값이 없습니다", name)
			}
			b.WriteString(v)
			i += j + 1
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), nil
}

// ---------------- 실행 환경 ----------------

// Endpoint 는 Tumblebug · Beetle 주소와 계정이다.
type Endpoint struct {
	Name, URL, User, Password string
}

// Settings 는 settings.yaml 이다.
type Settings struct {
	Path         string
	Tumblebugs   map[string]Endpoint
	Beetle       *Endpoint
	EvidenceRoot string
	Timeouts     map[string]float64
	Pacing       map[string]float64
	IsExample    bool
}

// LoadSettings 는 path 가 비면 프로젝트의 settings.yaml, 없으면 settings.example.yaml 을 읽는다.
func LoadSettings(path string) (*Settings, error) {
	isExample := false
	if path == "" {
		root := ProjectRoot()
		path = filepath.Join(root, "settings.yaml")
		if _, err := os.Stat(path); err != nil {
			isExample = true
			path = filepath.Join(root, "settings.example.yaml")
		}
	}
	raw, _, err := LoadYAML(path)
	if err != nil {
		return nil, err
	}
	if jsonx.S(raw, "verify_admin_password") != "" {
		return nil, Errf("%s: verify_admin_password 는 더 이상 쓰지 않습니다 — 사례 run.env 의 VERIFY_ADMIN_PASSWORD 로 주세요 (도구는 VM 에 SSH 로 접속하지 않습니다)", path)
	}
	if err := ExpandEnv(raw, path); err != nil {
		return nil, err
	}
	s := &Settings{Path: path, Tumblebugs: map[string]Endpoint{}, IsExample: isExample,
		Timeouts: map[string]float64{"provision_min": 30, "package_min": 120, "start_min": 75, "transfer_min": 60},
		Pacing: map[string]float64{"infra_get_interval_sec": 1.0, "default_interval_sec": 0.25,
			"start_poll_sec": 60, "ready_poll_sec": 15}} // 기동 확인 간격 — 원격 명령 기록이 쌓이므로 1분
	for name, v := range jsonx.AsM(raw["tumblebugs"]) {
		ep := jsonx.AsM(v)
		u, err := need(ep, "url", "tumblebugs."+name)
		if err != nil {
			return nil, err
		}
		pw := jsonx.S(ep, "password")
		if env := os.Getenv("GMP_TB_PASSWORD_" + strings.ToUpper(name)); env != "" {
			pw = env
		}
		user := jsonx.S(ep, "user")
		if user == "" || pw == "" {
			return nil, Errf("%s: tumblebugs.%s 의 user · password 가 필요합니다", path, name)
		}
		s.Tumblebugs[name] = Endpoint{Name: name, URL: strings.TrimRight(jsonx.Str(u), "/"), User: user, Password: pw}
	}
	if len(s.Tumblebugs) == 0 {
		return nil, Errf("%s: tumblebugs 에 최소 하나가 필요합니다", path)
	}
	if b := jsonx.AsM(raw["beetle"]); b != nil && jsonx.S(b, "url") != "" {
		pw := jsonx.S(b, "password")
		if env := os.Getenv("GMP_BEETLE_PASSWORD"); env != "" {
			pw = env
		}
		user := jsonx.S(b, "user")
		if user == "" || pw == "" {
			return nil, Errf("%s: beetle 의 user · password 가 필요합니다", path)
		}
		s.Beetle = &Endpoint{Name: "beetle", URL: strings.TrimRight(jsonx.S(b, "url"), "/"), User: user, Password: pw}
	}
	ev := jsonx.S(raw, "evidence_root")
	if ev == "" {
		ev = "evidence"
	}
	if !filepath.IsAbs(ev) {
		abs, err := filepath.Abs(filepath.Join(filepath.Dir(path), ev))
		if err == nil {
			ev = abs
		}
	}
	s.EvidenceRoot = filepath.Clean(ev)
	for k, v := range jsonx.AsM(raw["timeouts"]) {
		s.Timeouts[k] = jsonx.F(v)
	}
	for k, v := range jsonx.AsM(raw["pacing"]) {
		s.Pacing[k] = jsonx.F(v)
	}
	return s, nil
}

// Tumblebug 는 이름으로 Tumblebug 주소를 찾는다.
func (s *Settings) Tumblebug(name string) (Endpoint, error) {
	ep, ok := s.Tumblebugs[name]
	if !ok {
		names := make([]string, 0, len(s.Tumblebugs))
		for n := range s.Tumblebugs {
			names = append(names, n)
		}
		sort.Strings(names)
		return Endpoint{}, Errf("settings 에 tumblebugs.%s 이 없습니다 (있는 것: %s)", name, strings.Join(names, ", "))
	}
	return ep, nil
}

// TimeoutMin 은 제한 시간(분)을 정수로.
func (s *Settings) TimeoutMin(name string) int { return int(s.Timeouts[name]) }

// ---------------- 어댑터 ----------------

// Adapter 는 워크로드 어댑터 — 파이프라인이 워크로드에 대해 아는 전부다.
type Adapter struct {
	Path string
	Raw  jsonx.M
}

var adapterRequired = []string{"workload", "form", "repo_raw", "branch", "entry", "backup", "backup_out", "package_dir",
	"results", "resume_unit", "services"}

// LoadAdapter 는 adapters/<name>.yaml 을 읽는다.
func LoadAdapter(name string) (*Adapter, error) {
	path := filepath.Join(ProjectRoot(), "adapters", name+".yaml")
	raw, _, err := LoadYAML(path)
	if err != nil {
		return nil, err
	}
	if t := jsonx.S(raw, "todo"); t != "" {
		return nil, Errf("adapters/%s.yaml 은 아직 자리만 있습니다: %s", name, t)
	}
	where := "adapters/" + name + ".yaml"
	for _, k := range adapterRequired {
		if _, err := need(raw, k, where); err != nil {
			return nil, err
		}
	}
	res, err := needM(raw, "results", where)
	if err != nil {
		return nil, err
	}
	for _, k := range []string{"csv", "compare", "evidence"} {
		if _, err := need(res, k, where+" results"); err != nil {
			return nil, err
		}
	}
	return &Adapter{Path: path, Raw: raw}, nil
}

// S 는 어댑터 값을 문자열로.
func (a *Adapter) S(key string) string { return jsonx.S(a.Raw, key) }

// M 은 어댑터의 하위 맵 (없으면 빈 맵).
func (a *Adapter) M(key string) jsonx.M {
	if m := jsonx.AsM(a.Raw[key]); m != nil {
		return m
	}
	return jsonx.M{}
}

// Has 는 키 값이 비어 있지 않은지.
func (a *Adapter) Has(key string) bool { return jsonx.Truthy(a.Raw[key]) }

// Render 는 어댑터의 명령 템플릿을 채운다.
func (a *Adapter) Render(key string, values map[string]string) (string, error) {
	t, ok := a.Raw[key].(string)
	if !ok {
		return "", Errf("어댑터에 명령 '%s' 가 없습니다", key)
	}
	return Render(t, values)
}

// ---------------- 사례 ----------------

// Case 는 사례 파일 하나 — 이 운행에서 바뀌는 값만.
type Case struct {
	Path     string
	Raw      jsonx.M
	Name     string
	envOrder []string
}

// LoadCase 는 사례 파일을 읽고 검사한다.
func LoadCase(path string) (*Case, error) {
	raw, node, err := LoadYAML(path)
	if err != nil {
		return nil, err
	}
	if err := ExpandEnv(raw, path); err != nil {
		return nil, err
	}
	return NewCase(path, raw, nodeKeys(node, "run", "env"))
}

// NewCase 는 기본값을 채우고 검사한다. envOrder 는 run.env 의 키 순서 (비면 정렬 순).
func NewCase(path string, r jsonx.M, envOrder []string) (*Case, error) {
	name, err := need(r, "case", path)
	if err != nil {
		return nil, err
	}
	if _, err := need(r, "workload", "사례"); err != nil {
		return nil, err
	}
	src, err := needM(r, "source", "사례")
	if err != nil {
		return nil, err
	}
	setDefault(src, "tumblebug", "default")
	for _, k := range []string{"ns", "infra"} {
		if _, err := need(src, k, "source"); err != nil {
			return nil, err
		}
	}
	tgt, err := needM(r, "target", "사례")
	if err != nil {
		return nil, err
	}
	setDefault(tgt, "tumblebug", "default")
	setDefault(tgt, "ns", src["ns"])
	prov, err := need(tgt, "provider", "target")
	if err != nil {
		return nil, err
	}
	if err := enum(prov, []string{"beetle", "tumblebug"}, "target.provider"); err != nil {
		return nil, err
	}
	for _, k := range []string{"csp", "region", "name"} {
		if _, err := need(tgt, k, "target"); err != nil {
			return nil, err
		}
	}
	if jsonx.Str(prov) == "tumblebug" {
		if _, err := need(tgt, "spec", "target (provider=tumblebug 는 Tumblebug specId 가 필요)"); err != nil {
			return nil, err
		}
	}
	pol := jsonx.AsM(setDefault(r, "policy", jsonx.M{}))
	if pol == nil {
		return nil, Errf("policy 는 맵이어야 합니다")
	}
	if err := enum(setDefault(pol, "gpu", "same-model-first"), []string{"same-model-first", "vram-at-least"}, "policy.gpu"); err != nil {
		return nil, err
	}
	setDefault(pol, "image", "ubuntu-24.04")
	setDefault(pol, "driver_series", "595") // 워크로드 bootstrap 의 NVIDIA_DRIVER_SERIES 와 같게
	setDefault(pol, "root_disk_gb", 100)
	setDefault(pol, "open_ports", []any{})
	if err := enum(setDefault(pol, "on_perf_fail", "stop"), []string{"stop", "next-candidate"}, "policy.on_perf_fail"); err != nil {
		return nil, err
	}
	if jsonx.S(pol, "on_perf_fail") == "next-candidate" {
		return nil, Errf("policy.on_perf_fail=next-candidate 는 1차에서 구현되지 않았습니다 (stop 만)")
	}
	tr := jsonx.AsM(setDefault(r, "transfer", jsonx.M{}))
	if jsonx.S(tr, "method") == "stream" {
		return nil, Errf("transfer.method=stream 은 없앴습니다 — 패키지는 Beetle 데이터 이관(beetle)으로만 옮깁니다 (도구는 VM 에 SSH 로 접속하지 않습니다)")
	}
	if err := enum(setDefault(tr, "method", "beetle"), []string{"beetle"}, "transfer.method"); err != nil {
		return nil, err
	}
	if err := enum(setDefault(tr, "strategy", "direct"), []string{"direct", "relay"}, "transfer.strategy"); err != nil {
		return nil, err
	}
	run := jsonx.AsM(setDefault(r, "run", jsonx.M{}))
	setDefault(run, "env", jsonx.M{})
	setDefault(run, "golden", true)
	setDefault(run, "rebuild_before_backup", true)
	if err := enum(setDefault(r, "cleanup", "suspend"), []string{"suspend", "terminate", "keep"}, "cleanup"); err != nil {
		return nil, err
	}
	return &Case{Path: path, Raw: r, Name: jsonx.Str(name), envOrder: envOrder}, nil
}

// Source · Target · Policy · Transfer · Run 은 사례의 하위 맵이다.
func (c *Case) Source() jsonx.M   { return jsonx.AsM(c.Raw["source"]) }
func (c *Case) Target() jsonx.M   { return jsonx.AsM(c.Raw["target"]) }
func (c *Case) Policy() jsonx.M   { return jsonx.AsM(c.Raw["policy"]) }
func (c *Case) Transfer() jsonx.M { return jsonx.AsM(c.Raw["transfer"]) }
func (c *Case) Run() jsonx.M      { return jsonx.AsM(c.Raw["run"]) }

// Cleanup 은 정리 방식 (suspend · terminate · keep).
func (c *Case) Cleanup() string { return jsonx.S(c.Raw, "cleanup") }

// OpenPorts 는 policy.open_ports.
func (c *Case) OpenPorts() []jsonx.M { return jsonx.ListM(c.Policy()["open_ports"]) }

// Env 는 run.env 를 파일 순서대로.
func (c *Case) Env() []KV {
	env := jsonx.AsM(c.Run()["env"])
	keys := c.envOrder
	if len(keys) == 0 {
		keys = jsonx.SortedKeys(env)
	}
	out := make([]KV, 0, len(env))
	seen := map[string]bool{}
	for _, k := range keys {
		if v, ok := env[k]; ok && !seen[k] {
			out = append(out, KV{k, jsonx.Str(v)})
			seen[k] = true
		}
	}
	for _, k := range jsonx.SortedKeys(env) { // 순서 정보에 없는 키(코드에서 더한 것)는 뒤에
		if !seen[k] {
			out = append(out, KV{k, jsonx.Str(env[k])})
		}
	}
	return out
}

// EnvValue 는 run.env 의 값 하나.
func (c *Case) EnvValue(key string) string { return jsonx.S(jsonx.AsM(c.Run()["env"]), key) }
