// Package tumblebug 는 CB-Tumblebug REST 클라이언트다 (Basic 인증). 공용 서버의 배포본은 0.13.4 다.
// 아래 규칙은 v0.12.30 소스를 읽고 정한 뒤 배포본 실호출로 확인한 것이다 — 0.13.x 에서 달라진 동작(예: 503 + Retry-After)은 아직 반영하지 않았다.
//
//   - 호출 간격: GET /ns/{ns}/infra[/{id}] 는 출발 IP 당 2 req/s 를 MapUI 사용자와 나눠 쓴다 → 1초 간격. 나머지는 0.25초.
//   - 429 는 Retry-After 가 없다 → 지수 백오프(2→4→…≤60초, 지터, 최대 10회). 429 는 서버가 받지 않은 요청이라 POST 도 다시 보낸다.
//   - 5xx · 네트워크 오류는 GET 만 재시도한다. 생성(infraDynamic) · 원격 명령(cmd) POST 는 자동 재시도하지 않는다.
//   - 재개는 이름 조회(infra)와 우리가 정한 X-Request-Id(cmd)로 이어 간다.
//   - dry-run 은 조회만 한다. 쓰기 여부는 메서드가 아니라 호출별로 정한다 (제어는 GET 이지만 쓰기, searchImage · recommendSpec 은 POST 지만 조회).
package tumblebug

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gpu-migrate-orchestrator/internal/config"
	"gpu-migrate-orchestrator/internal/jsonx"
	"gpu-migrate-orchestrator/internal/logx"
	"gpu-migrate-orchestrator/internal/textx"
)

// M 은 JSON 객체다.
type M = jsonx.M

// Terminal 은 원격 명령이 끝난 상태들이다.
// Completed=종료 0 · CompletedWithError=SSH 는 됐고 종료 코드≠0 · Failed=전송 오류(접속 · 인증 · EOF — VM 재부팅 포함) ·
// Timeout=timeoutMinutes 초과 · Interrupted=Tumblebug 재시작.
var Terminal = map[string]bool{"Completed": true, "CompletedWithError": true, "Failed": true, "Timeout": true, "Cancelled": true, "Interrupted": true}

// SystemNS 는 공용 스펙 · 이미지가 등록된 네임스페이스다 (사용자 ns 에서는 조회되지 않는다).
const SystemNS = "system"

var missingRe = regexp.MustCompile(`(?i)not (?:exist|found)|does not exist|no such|not registered|failed to find`)

// Error 는 Tumblebug 호출 오류다.
type Error struct {
	Msg    string
	Status int
	Body   string
}

func (e *Error) Error() string { return e.Msg }

func errf(format string, a ...any) *Error { return &Error{Msg: fmt.Sprintf(format, a...)} }

// Client 는 Tumblebug 하나에 붙는다.
type Client struct {
	EP     config.Endpoint
	Log    *logx.Log
	DryRun bool
	Pacing map[string]float64
	HTTP   *http.Client
	Sleep  func(time.Duration) // 시험에서 바꾼다

	mu   sync.Mutex
	last map[string]time.Time
}

// New 는 클라이언트를 만든다.
func New(ep config.Endpoint, log *logx.Log, pacing map[string]float64, dryRun bool) *Client {
	p := map[string]float64{"infra_get_interval_sec": 1.0, "default_interval_sec": 0.25}
	for k, v := range pacing {
		p[k] = v
	}
	tr := &http.Transport{DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext, Proxy: http.ProxyFromEnvironment,
		TLSHandshakeTimeout: 10 * time.Second, MaxIdleConnsPerHost: 4}
	return &Client{EP: ep, Log: log, DryRun: dryRun, Pacing: p, HTTP: &http.Client{Transport: tr}, Sleep: time.Sleep, last: map[string]time.Time{}}
}

// Opts 는 요청 하나의 선택지다.
type Opts struct {
	Params   url.Values
	Body     any
	Headers  map[string]string
	Timeout  time.Duration // 응답을 기다리는 시간 (기본 130초)
	Route    string        // "infra_get" 이면 1초 간격
	NoRetry  bool
	Allow404 bool // 없는 자원은 오류 대신 nil
	Write    *bool
}

func boolp(b bool) *bool { return &b }

func (c *Client) pace(route string) {
	key := "default_interval_sec"
	if route == "infra_get" {
		key = "infra_get_interval_sec"
	}
	interval := time.Duration(c.Pacing[key] * float64(time.Second))
	c.mu.Lock()
	last := c.last[route]
	c.mu.Unlock()
	if wait := time.Until(last.Add(interval)); wait > 0 {
		c.Sleep(wait)
	}
	c.mu.Lock()
	c.last[route] = time.Now()
	c.mu.Unlock()
}

func (c *Client) backoff(delay time.Duration) time.Duration {
	c.Sleep(delay + time.Duration(mrand.Float64()*float64(time.Second)))
	if delay *= 2; delay > 60*time.Second {
		delay = 60 * time.Second
	}
	return delay
}

// Req 는 요청 하나를 보낸다. 돌려주는 값은 JSON(숫자는 json.Number), 없는 자원이면 nil.
func (c *Client) Req(method, path string, o Opts) (any, error) {
	write := method != "GET"
	if o.Write != nil {
		write = *o.Write
	}
	if c.DryRun && write {
		return nil, errf("dry-run 에서는 Tumblebug 에 쓰지 않습니다: %s %s", method, path)
	}
	u := c.EP.URL + path
	if len(o.Params) > 0 {
		u += "?" + o.Params.Encode()
	}
	var payload []byte
	if o.Body != nil {
		b, err := jsonx.Marshal(o.Body, false)
		if err != nil {
			return nil, err
		}
		payload = b
	}
	timeout := o.Timeout
	if timeout == 0 {
		timeout = 130 * time.Second
	}
	retry := !o.NoRetry
	attempt, delay := 0, 2*time.Second
	for {
		c.pace(o.Route)
		t0 := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), timeout+10*time.Second)
		req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(payload))
		if err != nil {
			cancel()
			return nil, err
		}
		req.SetBasicAuth(c.EP.User, c.EP.Password)
		req.Header.Set("Accept", "application/json")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range o.Headers {
			req.Header.Set(k, v)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			cancel()
			c.Log.HTTP(method, u, "EXC", time.Since(t0), o.Body, err.Error())
			if method == "GET" && retry && attempt < 5 {
				attempt++
				delay = c.backoff(delay)
				continue
			}
			return nil, errf("%s %s: 연결 실패 (%v)", method, path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		c.Log.HTTP(method, u, resp.StatusCode, time.Since(t0), o.Body, "")
		text := string(body)
		st := resp.StatusCode
		if o.Allow404 && (st == 404 || (st == 500 && missingRe.MatchString(text))) {
			return nil, nil // Tumblebug 는 없는 자원을 404 가 아니라 500 + "does not exist" 로 답하기도 한다 — 재시도하지 않는다
		}
		if st == 429 || (st >= 500 && method == "GET" && retry) {
			attempt++
			if attempt > 10 {
				return nil, &Error{Msg: fmt.Sprintf("%s %s: %d 가 계속됩니다 (호출 제한 · 서버 오류)", method, path, st), Status: st, Body: textx.Head(text, 300)}
			}
			delay = c.backoff(delay)
			continue
		}
		if st >= 400 {
			return nil, &Error{Msg: fmt.Sprintf("%s %s: HTTP %d — %s", method, path, st, logx.Mask(textx.Head(text, 400))), Status: st, Body: text}
		}
		if len(bytes.TrimSpace(body)) == 0 {
			return M{}, nil
		}
		v, err := jsonx.Decode(body)
		if err != nil {
			return M{"_text": text}, nil
		}
		return v, nil
	}
}

func (c *Client) reqM(method, path string, o Opts) (M, error) {
	v, err := c.Req(method, path, o)
	if err != nil || v == nil {
		return nil, err
	}
	if m := jsonx.AsM(v); m != nil {
		return m, nil
	}
	return M{"_value": v}, nil
}

// ---------- 인프라 조회 ----------

// ListInfra 는 인프라 ID 목록.
func (c *Client) ListInfra(ns string) ([]string, error) {
	d, err := c.reqM("GET", "/ns/"+ns+"/infra", Opts{Params: url.Values{"option": {"id"}}, Route: "infra_get"})
	if err != nil {
		return nil, err
	}
	return jsonx.Strings(d["output"]), nil
}

// GetInfra 는 없으면 nil. (상태 문자열 예: 'Running:1 (R:1/1)' · 'Suspended:1 (S:1/1)' · 'Failed')
func (c *Client) GetInfra(ns, id string) (M, error) {
	return c.reqM("GET", "/ns/"+ns+"/infra/"+id, Opts{Route: "infra_get", Allow404: true})
}

// Nodes 는 InfraInfo.node[] 에서 필요한 것만 꺼낸다.
func Nodes(infra M) []M {
	var out []M
	for _, n := range jsonx.ListM(infra["node"]) {
		sg := n["securityGroupIds"]
		if !jsonx.Truthy(sg) {
			sg = []any{}
		}
		out = append(out, M{"nodeId": jsonx.Or(n["id"], n["name"]), "name": n["name"], "status": n["status"],
			"publicIP": n["publicIP"], "privateIP": n["privateIP"], "sshPort": jsonx.Or(n["sshPort"], 22),
			"cspResourceId": n["cspResourceId"], "specId": n["specId"], "imageId": n["imageId"],
			"nodeGroupId": n["nodeGroupId"], "createdTime": n["createdTime"], "securityGroupIds": sg, "sshKeyId": n["sshKeyId"]})
	}
	return out
}

// InfraState 는 'Running:1 (R:1/1)' → 'Running'.
func InfraState(infra M) string {
	s := jsonx.S(infra, "status")
	s, _, _ = strings.Cut(s, ":")
	s, _, _ = strings.Cut(s, " ")
	return s
}

// AccessInfo 는 노드별 접속 정보 (개인키 포함). 응답은 대소문자가 섞여 있다 — 바깥은 PascalCase, 노드는 camelCase.
func (c *Client) AccessInfo(ns, id string) ([]M, error) {
	d, err := c.reqM("GET", "/ns/"+ns+"/infra/"+id, Opts{Route: "infra_get",
		Params: url.Values{"option": {"accessinfo"}, "accessInfoOption": {"showSshKey"}}})
	if err != nil {
		return nil, err
	}
	var out []M
	groups := jsonx.ListM(jsonx.Or(d["InfraNodeGroupAccessInfo"], d["infraNodeGroupAccessInfo"]))
	for _, g := range groups {
		for _, n := range jsonx.ListM(jsonx.Or(g["NodeAccessInfo"], g["nodeAccessInfo"])) {
			out = append(out, M{"nodeId": n["nodeId"], "publicIP": n["publicIP"], "privateIP": n["privateIP"],
				"sshPort": jsonx.Or(n["sshPort"], 22), "privateKey": n["privateKey"],
				"nodeUserName": jsonx.Or(n["nodeUserName"], "cb-user"),
				"nodeGroupId":  jsonx.Or(g["NodeGroupId"], g["nodeGroupId"])})
		}
	}
	if len(out) == 0 {
		return nil, errf("접속 정보가 비어 있습니다: %s/%s", ns, id)
	}
	return out, nil
}

// WaitInfra 는 상태가 want 중 하나가 될 때까지 기다린다.
func (c *Client) WaitInfra(ns, id string, want []string, timeout, poll time.Duration, onTick func(...any)) (M, error) {
	t0 := time.Now()
	for {
		infra, err := c.GetInfra(ns, id)
		if err != nil {
			return nil, err
		}
		state := InfraState(infra)
		if onTick != nil {
			onTick(state)
		}
		for _, w := range want {
			if state == w {
				return infra, nil
			}
		}
		if state == "Failed" || state == "Terminated" || state == "Terminating" {
			return nil, errf("인프라 %s 상태가 %s 입니다", id, state)
		}
		if time.Since(t0) > timeout {
			return nil, errf("인프라 %s 가 %d초 안에 %v 가 되지 않았습니다 (지금 %s)", id, int(timeout.Seconds()), want, state)
		}
		c.Sleep(poll)
	}
}

// ---------- 생성 · 제어 · 삭제 ----------

var infraDynamicKeys = map[string]bool{"name": true, "description": true, "label": true, "policyOnPartialFailure": true,
	"installMonAgent": true, "nodeGroups": true, "systemLabel": true, "vNetTemplateId": true, "sgTemplateId": true}
var nodeGroupKeys = map[string]bool{"name": true, "nodeGroupSize": true, "label": true, "description": true, "specId": true,
	"imageId": true, "rootDiskType": true, "rootDiskSize": true, "connectionName": true, "zone": true,
	"vNetTemplateId": true, "sgTemplateId": true, "distributeSubnets": true}

func unknownKeys(m M, allowed map[string]bool) []string {
	var bad []string
	for k := range m {
		if !allowed[k] {
			bad = append(bad, k)
		}
	}
	sort.Strings(bad)
	return bad
}

// ValidateInfraDynamic 은 infraDynamic 본문의 키와 정수 필드를 검사한다.
func ValidateInfraDynamic(body M) error {
	if bad := unknownKeys(body, infraDynamicKeys); len(bad) > 0 {
		return errf("infraDynamic 에 모르는 필드: %v", bad)
	}
	for _, ng := range jsonx.ListM(body["nodeGroups"]) {
		if bad := unknownKeys(ng, nodeGroupKeys); len(bad) > 0 {
			return errf("nodeGroups 에 모르는 필드: %v", bad)
		}
		for _, k := range []string{"nodeGroupSize", "rootDiskSize"} {
			if v, ok := ng[k]; ok && !jsonx.IsInt(v) {
				return errf("nodeGroups.%s 는 정수여야 합니다 (문자열이면 거부된다)", k)
			}
		}
	}
	return nil
}

// InfraDynamic 은 동기 생성 — 자동 재시도 없음. 재개는 호출 전에 GetInfra(name) 으로 판단한다.
func (c *Client) InfraDynamic(ns string, body M, timeout time.Duration) (M, error) {
	if err := ValidateInfraDynamic(body); err != nil {
		return nil, err
	}
	return c.reqM("POST", "/ns/"+ns+"/infraDynamic", Opts{Body: body, Timeout: timeout, NoRetry: true})
}

// Control 은 suspend · resume · reboot · terminate.
func (c *Client) Control(ns, id, action string) (any, error) {
	return c.Req("GET", "/ns/"+ns+"/control/infra/"+id, Opts{Params: url.Values{"action": {action}}, Timeout: 300 * time.Second,
		NoRetry: true, Write: boolp(true)})
}

// DeleteInfra 는 인프라를 지운다.
func (c *Client) DeleteInfra(ns, id, option string) (any, error) {
	return c.Req("DELETE", "/ns/"+ns+"/infra/"+id, Opts{Params: url.Values{"option": {option}}, Timeout: 600 * time.Second, NoRetry: true})
}

// AddSGRules 는 기존 규칙에 더한다. rules: [{"Ports": "80", "Protocol": "tcp", "Direction": "inbound", "CIDR": "x.x.x.x/32"}]
// (MapUI 의 Update Firewall 은 통째로 교체라 쓰지 않는다)
func (c *Client) AddSGRules(ns, sg string, rules []M) (any, error) {
	return c.Req("POST", "/ns/"+ns+"/resources/securityGroup/"+sg+"/rules", Opts{Body: M{"firewallRules": rules},
		Timeout: 120 * time.Second, NoRetry: true})
}

// ---------- 네트워크 ----------

// GetVNet 은 cidrBlock · subnetInfoList[{id, ipv4_CIDR, zone}].
func (c *Client) GetVNet(ns, id string) (M, error) {
	return c.reqM("GET", "/ns/"+ns+"/resources/vNet/"+id, Opts{Allow404: true})
}

// GetSecurityGroup 은 firewallRules: [{Port, Protocol, Direction, CIDR}].
func (c *Client) GetSecurityGroup(ns, id string) (M, error) {
	return c.reqM("GET", "/ns/"+ns+"/resources/securityGroup/"+id, Opts{Allow404: true})
}

// ---------- 이미지 · 스펙 ----------

// GetImage 는 없으면 nil.
func (c *Client) GetImage(ns, id string) (M, error) {
	return c.reqM("GET", "/ns/"+ns+"/resources/image/"+id, Opts{Allow404: true, NoRetry: true})
}

// GetSpec 은 없으면 nil.
func (c *Client) GetSpec(ns, id string) (M, error) {
	return c.reqM("GET", "/ns/"+ns+"/resources/spec/"+id, Opts{Allow404: true, NoRetry: true})
}

// ResolveSpec 은 system → ns 순으로 찾는다.
func (c *Client) ResolveSpec(ns, id string) (M, error) {
	m, err := c.GetSpec(SystemNS, id)
	if err != nil || m != nil {
		return m, err
	}
	return c.GetSpec(ns, id)
}

// ResolveImage 는 system → ns 순으로 찾는다.
func (c *Client) ResolveImage(ns, id string) (M, error) {
	m, err := c.GetImage(SystemNS, id)
	if err != nil || m != nil {
		return m, err
	}
	return c.GetImage(ns, id)
}

// FindImage 는 searchImage(ns=system) 로 기본 이미지 중 하나를 고른다 — 이름에 minimal 이 든 것은 뒤로.
func (c *Client) FindImage(provider, region, osType string) (M, error) {
	found, err := c.SearchImage(SystemNS, provider, region, osType, true, 20)
	if err != nil {
		return nil, err
	}
	var basic []M
	for _, i := range found {
		if jsonx.Truthy(i["isBasicImage"]) {
			basic = append(basic, i)
		}
	}
	if len(basic) == 0 {
		basic = found
	}
	if len(basic) == 0 {
		return nil, nil
	}
	name := func(i M) string {
		return strings.ToLower(jsonx.Str(jsonx.Or(jsonx.Or(i["osDistribution"], i["name"]), i["cspImageName"])))
	}
	sort.SliceStable(basic, func(a, b int) bool {
		ma, mb := strings.Contains(name(basic[a]), "minimal"), strings.Contains(name(basic[b]), "minimal")
		if ma != mb {
			return !ma
		}
		return name(basic[a]) < name(basic[b])
	})
	return basic[0], nil
}

// SearchImage 는 POST 지만 조회다.
func (c *Client) SearchImage(ns, provider, region, osType string, basicOnly bool, max int) ([]M, error) {
	d, err := c.reqM("POST", "/ns/"+ns+"/resources/searchImage", Opts{Write: boolp(false), Body: M{
		"providerName": provider, "regionName": region, "osType": osType, "osArchitecture": "x86_64",
		"includeBasicImageOnly": basicOnly, "includeDeprecatedImage": false, "maxResults": max}})
	if err != nil {
		return nil, err
	}
	return jsonx.ListM(d["imageList"]), nil
}

// Filter 는 recommendSpec 조건 하나다 (metric operator operand).
type Filter struct {
	Metric, Op string
	Value      any
}

// F 는 조건 하나를 만든다.
func F(metric, op string, value any) Filter { return Filter{Metric: metric, Op: op, Value: value} }

// RecommendSpec 은 POST /tumblebug/recommendSpec — 조건으로 사양 목록을 정렬해 받는다 (Beetle 추천의 축소판).
// 타입 주의 (v0.12.30 소스, 배포본 0.13.4 실호출로 확인): weight 는 float, limit 은 int, operand 는 문자열 — 틀리면 400 Unmarshal type error.
func (c *Client) RecommendSpec(filters []Filter, priority string, limit int) ([]M, error) {
	var policy []any
	for _, f := range filters {
		policy = append(policy, M{"metric": f.Metric, "condition": []any{M{"operator": f.Op, "operand": jsonx.Str(f.Value)}}})
	}
	body := M{"filter": M{"policy": policy}, "priority": M{"policy": []any{M{"metric": priority, "weight": 1.0}}}, "limit": limit}
	v, err := c.Req("POST", "/recommendSpec", Opts{Body: body, Timeout: 240 * time.Second, Write: boolp(false)})
	if err != nil {
		return nil, err
	}
	if l := jsonx.ListM(v); l != nil {
		return l, nil
	}
	d := jsonx.AsM(v)
	return jsonx.ListM(jsonx.Or(d["output"], d["specList"])), nil
}

// ---------- 원격 명령 ----------

// NewRequestID 는 prefix 뒤에 무작위 12자리를 붙인다.
func NewRequestID(prefix string) string {
	b := make([]byte, 6)
	rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

func cmdBody(cmds []string, user string, timeoutMin int) M {
	if timeoutMin < 1 {
		timeoutMin = 1
	}
	if timeoutMin > 120 {
		timeoutMin = 120
	}
	body := M{"command": cmds, "timeoutMinutes": timeoutMin}
	if user != "" {
		body["userName"] = user
	}
	return body
}

// CmdAsync 는 즉시 반환하고 xRequestId 를 돌려준다. 각 command 원소는 SSH exec 하나. timeoutMinutes 는 최대 120.
func (c *Client) CmdAsync(ns, id string, cmds []string, user string, timeoutMin int, xreq string) (string, error) {
	var h map[string]string
	if xreq != "" {
		h = map[string]string{"X-Request-Id": xreq}
	}
	d, err := c.reqM("POST", "/ns/"+ns+"/cmd/infra/"+id, Opts{Params: url.Values{"async": {"true"}}, Body: cmdBody(cmds, user, timeoutMin),
		Headers: h, Timeout: 60 * time.Second, NoRetry: true})
	if err != nil {
		return "", err
	}
	rid := jsonx.S(d, "xRequestId")
	if rid == "" {
		rid = xreq
	}
	if rid == "" {
		return "", errf("원격 명령의 xRequestId 를 받지 못했습니다")
	}
	return rid, nil
}

// CmdSync 는 결과까지 기다린다 — 짧은 명령(상태 확인 · 분리 실행)용.
func (c *Client) CmdSync(ns, id string, cmds []string, user string, timeoutMin int) ([]M, error) {
	body := cmdBody(cmds, user, timeoutMin)
	d, err := c.reqM("POST", "/ns/"+ns+"/cmd/infra/"+id, Opts{Body: body,
		Timeout: time.Duration(jsonx.Int(body["timeoutMinutes"])*60+120) * time.Second, NoRetry: true})
	if err != nil {
		return nil, err
	}
	return jsonx.ListM(d["results"]), nil
}

// SyncStdout 은 CmdSync 결과에서 노드 하나의 stdout · stderr 를 명령 순서대로 꺼낸다.
func SyncStdout(results []M, nodeID string) (stdout, stderr []string, errMsg string) {
	for _, r := range results {
		if nodeID != "" && jsonx.S(r, "nodeId") != nodeID {
			continue
		}
		so, se := jsonx.AsM(r["stdout"]), jsonx.AsM(r["stderr"])
		keys := make([]string, 0, len(so))
		for k := range so {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(a, b int) bool {
			ia, _ := strconv.Atoi(keys[a])
			ib, _ := strconv.Atoi(keys[b])
			return ia < ib
		})
		for _, k := range keys {
			stdout = append(stdout, jsonx.Str(so[k]))
			stderr = append(stderr, jsonx.Str(se[k]))
		}
		return stdout, stderr, jsonx.S(r, "error")
	}
	return nil, nil, "결과 없음"
}

// CommandStatus 는 노드 하나의 명령 상태 목록.
func (c *Client) CommandStatus(ns, id, node, xreq string) ([]M, error) {
	d, err := c.reqM("GET", "/ns/"+ns+"/infra/"+id+"/node/"+node+"/commandStatus", Opts{Params: url.Values{"xRequestId": {xreq}}})
	if err != nil {
		return nil, err
	}
	return jsonx.ListM(d["commands"]), nil
}

// WaitCommand 는 모든 노드의 해당 xRequestId 명령이 끝날 때까지 기다린다. {nodeId: 마지막 상태 항목}.
func (c *Client) WaitCommand(ns, id string, nodeIDs []string, xreq string, timeout, poll time.Duration, onTick func(...any)) (map[string]M, error) {
	t0 := time.Now()
	for {
		done := map[string]M{}
		var pending []string
		for _, nid := range nodeIDs {
			entries, err := c.CommandStatus(ns, id, nid, xreq)
			if err != nil {
				return nil, err
			}
			var last M
			if len(entries) > 0 {
				last = entries[len(entries)-1]
			}
			if last != nil && Terminal[jsonx.S(last, "status")] {
				done[nid] = last
			} else {
				st := "없음"
				if last != nil {
					st = jsonx.S(last, "status")
				}
				pending = append(pending, nid+":"+st)
			}
		}
		if onTick != nil {
			onTick(len(done), strings.Join(pending, ","), int(time.Since(t0).Seconds()))
		}
		if len(pending) == 0 {
			return done, nil
		}
		if time.Since(t0) > timeout {
			return nil, errf("원격 명령 %s 가 %d초 안에 끝나지 않았습니다: %v", xreq, int(timeout.Seconds()), pending)
		}
		c.Sleep(poll)
	}
}
