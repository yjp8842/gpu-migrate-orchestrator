// Package stages 는 사례 1건의 8단계와, 단계들이 함께 쓰는 실행 문맥이다.
//
// VM 을 보는 일 — 상태 확인 · 파일 읽기 · 스크립트 — 은 Tumblebug 원격 명령으로 한다 (Exec · ReadText · FetchFile · FetchDir).
// 패키지는 Beetle 데이터 이관으로 옮긴다. 도구는 VM 에 SSH 로 접속하지 않는다.
// 개인키는 Beetle 이관 요청에 넣을 때만 Tumblebug 에서 메모리로 받아 바로 암호화한다 (파일로 두지 않는다).
package stages

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gpu-migrate-orchestrator/internal/beetle"
	"gpu-migrate-orchestrator/internal/config"
	"gpu-migrate-orchestrator/internal/jsonx"
	"gpu-migrate-orchestrator/internal/logx"
	"gpu-migrate-orchestrator/internal/state"
	"gpu-migrate-orchestrator/internal/textx"
	"gpu-migrate-orchestrator/internal/tumblebug"
)

// M 은 JSON 객체다.
type M = jsonx.M

// Tumblebug 는 단계들이 쓰는 Tumblebug 호출이다 (시험에서 가짜로 바꾼다).
type Tumblebug interface {
	GetInfra(ns, id string) (M, error)
	AccessInfo(ns, id string) ([]M, error)
	WaitInfra(ns, id string, want []string, timeout, poll time.Duration, onTick func(...any)) (M, error)
	InfraDynamic(ns string, body M, timeout time.Duration) (M, error)
	Control(ns, id, action string) (any, error)
	DeleteInfra(ns, id, option string) (any, error)
	AddSGRules(ns, sg string, rules []M) (any, error)
	GetVNet(ns, id string) (M, error)
	GetSecurityGroup(ns, id string) (M, error)
	ResolveSpec(ns, id string) (M, error)
	ResolveImage(ns, id string) (M, error)
	FindImage(provider, region, osType string) (M, error)
	RecommendSpec(filters []tumblebug.Filter, priority string, limit int) ([]M, error)
	CmdAsync(ns, id string, cmds []string, user string, timeoutMin int, xreq string) (string, error)
	CmdSync(ns, id string, cmds []string, user string, timeoutMin int) ([]M, error)
	CommandStatus(ns, id, node, xreq string) ([]M, error)
	WaitCommand(ns, id string, nodeIDs []string, xreq string, timeout, poll time.Duration, onTick func(...any)) (map[string]M, error)
}

// Beetle 은 단계들이 쓰는 Beetle 호출이다.
type Beetle interface {
	RecommendInfra(req M, limit int) ([]any, error)
	MigrateInfra(ns string, body M, nameSeed string, useExisting bool, timeout time.Duration) (M, error)
	EncryptionKey() (M, error)
	DataMigration(body M, xreq string) (string, error)
	RequestStatus(reqID string) (M, error)
	WaitRequest(reqID string, timeout, poll time.Duration, onTick func(...any)) (M, error)
}

// Context 는 한 run 의 실행 문맥이다.
type Context struct {
	Case     *config.Case
	Adapter  *config.Adapter
	Settings *config.Settings
	State    *state.RunState
	Log      *logx.Log
	DryRun   bool
	RunDir   string
	Sleep    func(time.Duration)

	// 시험에서 바꾼다.
	NewTumblebug func(name string) (Tumblebug, error)
	NewBeetle    func() (Beetle, error)

	tbs    map[string]Tumblebug
	beetle Beetle
}

// NewContext 는 실제 클라이언트를 쓰는 문맥을 만든다.
func NewContext(c *config.Case, ad *config.Adapter, s *config.Settings, st *state.RunState, log *logx.Log, dryRun bool) (*Context, error) {
	ctx := &Context{Case: c, Adapter: ad, Settings: s, State: st, Log: log, DryRun: dryRun,
		RunDir: st.RunDir, Sleep: time.Sleep, tbs: map[string]Tumblebug{}}
	ctx.NewTumblebug = func(name string) (Tumblebug, error) {
		ep, err := s.Tumblebug(name)
		if err != nil {
			return nil, err
		}
		return tumblebug.New(ep, log, s.Pacing, dryRun), nil
	}
	ctx.NewBeetle = func() (Beetle, error) {
		if s.Beetle == nil {
			return nil, config.Errf("settings 에 beetle 이 없습니다 — provider=beetle · transfer.method=beetle 사례는 Beetle 주소가 필요합니다")
		}
		return beetle.New(*s.Beetle, log, dryRun), nil
	}
	return ctx, nil
}

// ---------- 클라이언트 ----------

// TB 는 이름으로 Tumblebug 클라이언트를 (한 번 만들어 둔다).
func (c *Context) TB(name string) (Tumblebug, error) {
	if c.tbs == nil {
		c.tbs = map[string]Tumblebug{}
	}
	if tb, ok := c.tbs[name]; ok {
		return tb, nil
	}
	tb, err := c.NewTumblebug(name)
	if err != nil {
		return nil, err
	}
	c.tbs[name] = tb
	return tb, nil
}

// TBSource · TBTarget 은 소스 · 타겟이 사는 Tumblebug.
func (c *Context) TBSource() (Tumblebug, error) { return c.TB(jsonx.S(c.Case.Source(), "tumblebug")) }
func (c *Context) TBTarget() (Tumblebug, error) { return c.TB(jsonx.S(c.Case.Target(), "tumblebug")) }

// Beetle 은 Beetle 클라이언트 (settings 에 없으면 오류).
func (c *Context) Beetle() (Beetle, error) {
	if c.beetle != nil {
		return c.beetle, nil
	}
	b, err := c.NewBeetle()
	if err != nil {
		return nil, err
	}
	c.beetle = b
	return b, nil
}

// ---------- 환경변수 ----------

// RunEnv 는 타겟 단일 명령에 붙는 비밀 아닌 환경변수다. FORM 으로 결과표 행을 사례와 묶는다.
func (c *Context) RunEnv() []config.KV {
	env := []config.KV{{K: "FORM", V: c.Case.Name + "-target"}}
	for _, kv := range c.Case.Env() {
		if kv.K == "FORM" {
			env[0].V = kv.V
			continue
		}
		env = append(env, kv)
	}
	return env
}

// EnvStr 은 RunEnv 를 `K=V K2=V2` 로.
func (c *Context) EnvStr() string { return config.EnvString(c.RunEnv()) }

// Remote 는 어댑터의 명령 템플릿을 채운다. repo_raw · branch · env 는 자동으로 들어간다.
func (c *Context) Remote(key string, values map[string]string) (string, error) {
	base := map[string]string{"repo_raw": c.Adapter.S("repo_raw"), "branch": c.Adapter.S("branch"), "env": c.EnvStr()}
	for k, v := range values {
		base[k] = v
	}
	return c.Adapter.Render(key, base)
}

// ---------- 노드 ----------

// SetNodes 는 노드 정보를 개인키 없이 state 에 적는다.
func (c *Context) SetNodes(role string, nodes []M, user string) {
	clean := make([]M, 0, len(nodes))
	for _, n := range nodes {
		m := M{}
		for k, v := range n {
			if k != "privateKey" {
				m[k] = v
			}
		}
		clean = append(clean, m)
	}
	c.State.SetKey(role+"_nodes", clean)
	if user != "" {
		c.State.SetKey(role+"_user", user)
	}
}

// Nodes 는 state 의 노드 정보.
func (c *Context) Nodes(role string) []M { return jsonx.ListM(c.State.Key(role + "_nodes")) }

// Node 는 첫 노드 (없으면 오류).
func (c *Context) Node(role string) (M, error) {
	ns := c.Nodes(role)
	if len(ns) == 0 {
		prev := "provision"
		if role == "source" {
			prev = "package"
		}
		return nil, config.Errf("%s 노드 정보가 없습니다 — 앞 단계(%s)가 먼저입니다", role, prev)
	}
	return ns[0], nil
}

// SaveAccess 는 접속 정보에서 노드 정보만 남긴다 (개인키는 파일로 두지 않는다).
func (c *Context) SaveAccess(role string, access []M) {
	user := ""
	if len(access) > 0 {
		user = jsonx.S(access[0], "nodeUserName")
	}
	c.SetNodes(role, access, user)
}

// infra 는 역할(source · target)의 Tumblebug · ns · 인프라 ID.
func (c *Context) infra(role string) (Tumblebug, string, string, error) {
	if role == "source" {
		tb, err := c.TBSource()
		return tb, jsonx.S(c.Case.Source(), "ns"), jsonx.S(c.Case.Source(), "infra"), err
	}
	id := c.State.KeyS("target_infra_id")
	if id == "" {
		return nil, "", "", config.Errf("타겟 인프라 ID 가 없습니다 — provision 단계가 먼저입니다")
	}
	tb, err := c.TBTarget()
	return tb, jsonx.S(c.Case.Target(), "ns"), id, err
}

// ---------- VM 접근 — Tumblebug 원격 명령 ----------

const rcMarker = "__GMP_RC="

// ExecResult 는 원격 명령 한 건의 결과다. Reachable=false 면 VM 에 닿지 못했다(재부팅 중 등).
type ExecResult struct {
	RC        int
	Stdout    string
	Stderr    string
	Reachable bool
	Detail    string
}

// Wrap 은 스크립트를 bash 로 돌리고 끝에 종료 코드 표식을 찍게 감싼다 — Tumblebug 응답 형식에 기대지 않고 종료 코드를 읽으려고.
func Wrap(script string) string {
	return "bash -c " + textx.Quote(script) + `; printf '\n` + rcMarker + `%s\n' "$?"`
}

// ParseWrapped 는 Wrap 으로 감싼 명령의 출력에서 (본문, 종료 코드, 표식이 있었는지)를 꺼낸다.
func ParseWrapped(out string) (string, int, bool) {
	i := strings.LastIndex(out, "\n"+rcMarker)
	if i < 0 {
		if strings.HasPrefix(out, rcMarker) {
			i = -1
		} else {
			return out, -1, false
		}
	}
	body := ""
	if i >= 0 {
		body = out[:i]
	}
	rest := out[i+1+len(rcMarker):]
	rest = strings.TrimSpace(strings.SplitN(rest, "\n", 2)[0])
	rc, err := strconv.Atoi(rest)
	if err != nil {
		return body, -1, false
	}
	return body, rc, true
}

// Exec 는 역할의 첫 노드에서 스크립트 하나를 Tumblebug 원격 명령으로 돌린다.
func (c *Context) Exec(role, script string, timeoutMin int) (ExecResult, error) {
	tb, ns, id, err := c.infra(role)
	if err != nil {
		return ExecResult{}, err
	}
	node, err := c.Node(role)
	if err != nil {
		return ExecResult{}, err
	}
	user := jsonx.S(node, "nodeUserName")
	if user == "" {
		user = c.State.KeyS(role + "_user")
	}
	res, err := tb.CmdSync(ns, id, []string{Wrap(script)}, user, timeoutMin)
	if err != nil {
		return ExecResult{Detail: err.Error()}, err
	}
	stdout, stderr, emsg := tumblebug.SyncStdout(res, jsonx.S(node, "nodeId"))
	out, errs := "", ""
	if len(stdout) > 0 {
		out, errs = stdout[0], stderr[0]
	}
	body, rc, ok := ParseWrapped(out)
	if !ok {
		return ExecResult{RC: -1, Stdout: out, Stderr: errs, Reachable: false, Detail: emsg}, nil
	}
	return ExecResult{RC: rc, Stdout: body, Stderr: errs, Reachable: true, Detail: emsg}, nil
}

// ExecOK 는 종료 코드가 0 이 아니거나 닿지 못하면 오류.
func (c *Context) ExecOK(role, script string, timeoutMin int) (string, error) {
	r, err := c.Exec(role, script, timeoutMin)
	if err != nil {
		return "", err
	}
	if !r.Reachable {
		return "", Errf("%s VM 에 원격 명령이 닿지 않았습니다: %s", role, textx.Tail(r.Detail+" "+r.Stderr, 300))
	}
	if r.RC != 0 {
		return r.Stdout, Errf("%s 원격 명령 실패 (rc=%d): %s", role, r.RC, textx.Tail(strings.TrimSpace(r.Stdout+"\n"+r.Stderr), 300))
	}
	return r.Stdout, nil
}

// ReadText 는 작은 텍스트 파일을 읽는다 (없으면 ok=false). 앞의 `~/` 는 홈으로 풀린다.
func (c *Context) ReadText(role, path string) (string, bool, error) {
	p := textx.RemotePath(path)
	r, err := c.Exec(role, fmt.Sprintf("[ -f %s ] || exit 3; cat %s", p, p), 5)
	if err != nil {
		return "", false, err
	}
	if !r.Reachable {
		return "", false, Errf("%s VM 에 원격 명령이 닿지 않았습니다: %s", role, textx.Tail(r.Detail, 200))
	}
	if r.RC != 0 {
		return "", false, nil
	}
	return r.Stdout, true, nil
}

func decodeB64(out string) ([]byte, error) {
	clean := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' {
			return -1
		}
		return r
	}, out)
	return base64.StdEncoding.DecodeString(clean)
}

// FetchFile 은 원격 파일을 gzip + base64 로 받아 local 에 쓴다 (없으면 false).
func (c *Context) FetchFile(role, path, local string) (bool, error) {
	p := textx.RemotePath(path)
	r, err := c.Exec(role, fmt.Sprintf("[ -f %s ] || exit 3; gzip -c %s | base64 -w0", p, p), 10)
	if err != nil {
		return false, err
	}
	if !r.Reachable {
		return false, Errf("%s VM 에 원격 명령이 닿지 않았습니다: %s", role, textx.Tail(r.Detail, 200))
	}
	if r.RC == 3 {
		return false, nil
	}
	if r.RC != 0 {
		return false, Errf("%s 파일 %s 를 가져오지 못했습니다 (rc=%d): %s", role, path, r.RC, textx.Tail(r.Stderr, 200))
	}
	raw, err := decodeB64(r.Stdout)
	if err != nil {
		return false, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return false, err
	}
	defer zr.Close()
	f, err := os.Create(local)
	if err != nil {
		return false, err
	}
	defer f.Close()
	_, err = io.Copy(f, zr)
	return err == nil, err
}

// FetchDir 은 원격 폴더를 tar.gz + base64 로 받아 localDir 아래에 푼다 (폴더 이름 한 단계는 벗긴다, 없으면 false).
func (c *Context) FetchDir(role, path, localDir string) (bool, error) {
	p := textx.RemotePath(path)
	r, err := c.Exec(role, fmt.Sprintf(`[ -d %s ] || exit 3; tar -C "$(dirname %s)" -czf - "$(basename %s)" | base64 -w0`, p, p, p), 10)
	if err != nil {
		return false, err
	}
	if !r.Reachable {
		return false, Errf("%s VM 에 원격 명령이 닿지 않았습니다: %s", role, textx.Tail(r.Detail, 200))
	}
	if r.RC == 3 {
		return false, nil
	}
	if r.RC != 0 {
		return false, Errf("%s 폴더 %s 를 가져오지 못했습니다 (rc=%d): %s", role, path, r.RC, textx.Tail(r.Stderr, 200))
	}
	raw, err := decodeB64(r.Stdout)
	if err != nil {
		return false, err
	}
	return true, untarStrip(raw, localDir)
}

func untarStrip(raw []byte, dest string) error {
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.ToSlash(filepath.Clean(h.Name))
		if name == ".." || strings.HasPrefix(name, "../") || strings.HasPrefix(h.Name, "/") || strings.Contains("/"+filepath.ToSlash(h.Name)+"/", "/../") {
			return Errf("받은 압축에 폴더 밖 경로가 있습니다: %s", h.Name)
		}
		_, rest, ok := strings.Cut(name, "/")
		if !ok || rest == "" {
			continue // 맨 위 폴더 자체
		}
		target := filepath.Join(dest, filepath.FromSlash(rest))
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) {
			return Errf("받은 압축에 폴더 밖 경로가 있습니다: %s", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
}

// WaitReachable 은 원격 명령이 성공할 때까지 기다린다 (생성 직후 cloud-init · SSH 준비).
func (c *Context) WaitReachable(role string, timeout, poll time.Duration) error {
	if poll <= 0 {
		poll = 15 * time.Second
	}
	t0 := time.Now()
	last := ""
	for {
		r, err := c.Exec(role, "true", 2)
		if err == nil && r.Reachable && r.RC == 0 {
			return nil
		}
		if err != nil {
			last = err.Error()
		} else {
			last = r.Detail
		}
		if time.Since(t0) > timeout {
			return Errf("%s VM 에 %d초 안에 원격 명령이 닿지 않았습니다: %s", role, int(timeout.Seconds()), textx.Tail(last, 200))
		}
		c.Sleep(poll)
	}
}

// ---------- 개인키 (Beetle 이관 요청에만) ----------

// PrivateKey 는 역할 노드의 개인키를 Tumblebug 에서 받아 메모리로 돌려준다 (파일로 두지 않는다).
func (c *Context) PrivateKey(role string) (string, error) {
	tb, ns, id, err := c.infra(role)
	if err != nil {
		return "", err
	}
	access, err := tb.AccessInfo(ns, id)
	if err != nil {
		return "", err
	}
	want := ""
	if n, err := c.Node(role); err == nil {
		want = jsonx.S(n, "nodeId")
	}
	for _, a := range access {
		if want == "" || jsonx.S(a, "nodeId") == want {
			if k := jsonx.S(a, "privateKey"); k != "" {
				return k, nil
			}
		}
	}
	return "", Errf("%s 노드의 개인키를 받지 못했습니다", role)
}

// ---------- 산출물 ----------

// WriteJSON 은 run/<stage>/<name> 에 JSON 을 쓰고 산출물로 적는다.
func (c *Context) WriteJSON(stage, name string, v any) (string, error) {
	path := filepath.Join(c.State.StageDir(stage), name)
	if err := jsonx.WriteFile(path, v); err != nil {
		return "", err
	}
	c.State.Artifact(stage, name, path)
	return path, nil
}

// WriteText 는 run/<stage>/<name> 에 글을 쓴다 (끝 줄바꿈 보장).
func (c *Context) WriteText(stage, name, text string) (string, error) {
	path := filepath.Join(c.State.StageDir(stage), name)
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		return "", err
	}
	c.State.Artifact(stage, name, path)
	return path, nil
}

// ReadJSON 은 앞 단계 산출물을 읽는다.
func (c *Context) ReadJSON(stage, name string) (any, error) {
	path := filepath.Join(c.RunDir, stage, name)
	if _, err := os.Stat(path); err != nil {
		return nil, config.Errf("%s/%s 이 없습니다 — 앞 단계를 먼저 돌리세요", stage, name)
	}
	return jsonx.ReadFile(path)
}

// Dry 는 dry-run 에서 보낼 요청을 run/<stage>/dry-run.json 에 쌓는다 (호출은 하지 않는다).
func (c *Context) Dry(stage, what string, payload any) {
	path := filepath.Join(c.State.StageDir(stage), "dry-run.json")
	var items []any
	if v, err := jsonx.ReadFile(path); err == nil {
		items = jsonx.AsList(v)
	}
	items = append(items, M{"what": what, "payload": payload})
	jsonx.WriteFile(path, items)
	c.Log.Info(stage, "dry-run: %s", what)
}
