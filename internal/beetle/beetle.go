// Package beetle 은 CM-Beetle REST 클라이언트다 (v0.6.2 계열, 라이브 Swagger 로 스키마 확인).
//
//   - 추천 POST /recommendation/infra · 생성 POST /migration/ns/{ns}/infra?nameSeed= · GET …/infra/{id}/ssh-ready
//   - 데이터 이관 GET /migration/data/encryptionKey → POST /migration/data → GET /request/{reqId}
//   - 긴 호출은 Prefer: respond-async 로 시작하고 상태를 폴링한다.
//   - dry-run 은 조회만 한다. 추천은 POST 지만 조회이고, 암호화 키 발급은 GET 이지만 일회용 키를 만든다.
//   - 거절 응답 (v0.6.2 배포본에서 확인): 전역 호출 한도 초과는 429 + Retry-After(3초), 비동기 작업 초과 ·
//     Tumblebug 호출 대기 초과는 503 + Retry-After. 둘 다 서버가 받지 않은 요청이라 Retry-After 만큼 쉬고 다시 보낸다.
//     단 동기 생성(MigrateInfra)의 503 은 생성 도중에 날 수 있어 다시 보내지 않는다 — 호출하는 쪽이 nameSeed 로 찾아 이어 간다.
//   - 같은 X-Request-Id 를 다시 쓰면 409 ("already in use") — 이미 받은 요청이라는 뜻이다.
package beetle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"regexp"
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

// Error 는 Beetle 호출 오류다.
type Error struct {
	Msg    string
	Status int
	Body   string
}

func (e *Error) Error() string { return e.Msg }

func errf(format string, a ...any) *Error { return &Error{Msg: fmt.Sprintf(format, a...)} }

var missingRe = regexp.MustCompile(`(?i)not (?:exist|found)|does not exist|no such`)

// IsNotFound 는 Beetle 이 그런 자원(요청 · 인프라)이 없다고 답했는지 본다.
func IsNotFound(err error) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	return e.Status == 404 || (e.Status >= 400 && missingRe.MatchString(e.Body))
}

// Terminal 은 요청 상태 중 끝난 것 (소문자). success 만 성공이다.
var Terminal = map[string]bool{"success": true, "error": true, "failed": true, "completed": true}

// Unwrap 은 Beetle 응답 래퍼 {success, data, message, error} 에서 data 를 꺼낸다 (래퍼가 아니면 그대로).
func Unwrap(resp any) (any, error) {
	m := jsonx.AsM(resp)
	if m == nil {
		return resp, nil
	}
	_, hasData := m["data"]
	_, hasSuccess := m["success"]
	if !hasData && !hasSuccess {
		return resp, nil
	}
	if b, ok := m["success"].(bool); ok && !b {
		return nil, errf("Beetle 오류: %s", jsonx.Str(jsonx.Or(m["error"], m["message"])))
	}
	return m["data"], nil
}

// Client 는 Beetle 하나에 붙는다.
type Client struct {
	EP          config.Endpoint
	Log         *logx.Log
	DryRun      bool
	HTTP        *http.Client
	Sleep       func(time.Duration)
	MinInterval time.Duration

	mu   sync.Mutex
	last time.Time
}

// New 는 클라이언트를 만든다.
func New(ep config.Endpoint, log *logx.Log, dryRun bool) *Client {
	tr := &http.Transport{DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext, Proxy: http.ProxyFromEnvironment,
		TLSHandshakeTimeout: 10 * time.Second}
	return &Client{EP: ep, Log: log, DryRun: dryRun, HTTP: &http.Client{Transport: tr}, Sleep: time.Sleep, MinInterval: 300 * time.Millisecond}
}

// Opts 는 요청 하나의 선택지다.
type Opts struct {
	Params  url.Values
	Body    any
	Headers map[string]string
	Timeout time.Duration
	NoRetry bool
	// RetryBusy 는 503(+Retry-After)을 받지 않은 요청으로 보고 다시 보낸다 — 비동기 시작 요청처럼 서버가 일을 시작하기 전에만 503 을 주는 호출.
	RetryBusy bool
	Allow404  bool
	Write     *bool
}

func boolp(b bool) *bool { return &b }

// Req 는 요청 하나를 보낸다.
func (c *Client) Req(method, path string, o Opts) (any, error) {
	write := method != "GET"
	if o.Write != nil {
		write = *o.Write
	}
	if c.DryRun && write {
		return nil, errf("dry-run 에서는 Beetle 에 쓰지 않습니다: %s %s", method, path)
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
		timeout = 300 * time.Second
	}
	retry := !o.NoRetry
	attempt, delay := 0, 2*time.Second
	backoff := func() {
		c.Sleep(delay + time.Duration(mrand.Float64()*float64(time.Second)))
		if delay *= 2; delay > 60*time.Second {
			delay = 60 * time.Second
		}
	}
	for {
		c.mu.Lock()
		wait := time.Until(c.last.Add(c.MinInterval))
		c.mu.Unlock()
		if wait > 0 {
			c.Sleep(wait)
		}
		c.mu.Lock()
		c.last = time.Now()
		c.mu.Unlock()
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
				backoff()
				continue
			}
			return nil, errf("%s %s: 연결 실패 (%v)", method, path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		c.Log.HTTP(method, u, resp.StatusCode, time.Since(t0), o.Body, "")
		st := resp.StatusCode
		text := string(body)
		if st == 429 || (st == 503 && o.RetryBusy) || (st >= 500 && method == "GET" && retry) {
			attempt++
			if attempt > 10 {
				return nil, &Error{Msg: fmt.Sprintf("%s %s: %d 가 계속됩니다", method, path, st), Status: st, Body: textx.Head(text, 300)}
			}
			if ra := RetryAfter(resp.Header.Get("Retry-After")); ra > 0 {
				c.Sleep(ra + time.Duration(mrand.Float64()*float64(time.Second)))
			} else {
				backoff()
			}
			continue
		}
		if o.Allow404 && st == 404 {
			return nil, nil
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

// RetryAfter 는 Retry-After 헤더(초)를 기간으로 (없거나 이상하면 0). 너무 길게 묶이지 않게 60초로 자른다.
func RetryAfter(h string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(h))
	if err != nil || n <= 0 {
		return 0
	}
	if n > 60 {
		n = 60
	}
	return time.Duration(n) * time.Second
}

// IsDuplicateRequest 는 같은 X-Request-Id 로 다시 보내 409 를 받은 경우다 — 그 요청은 이미 서버에 있다.
func IsDuplicateRequest(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == 409 && strings.Contains(strings.ToLower(e.Body), "already in use")
}

func (c *Client) data(method, path string, o Opts) (any, error) {
	v, err := c.Req(method, path, o)
	if err != nil || v == nil {
		return v, err
	}
	return Unwrap(v)
}

// ---------- 추천 · 생성 ----------

// RecommendInfra 는 후보 목록 (우선순위 순). 조회라 자원을 만들지 않는다.
func (c *Client) RecommendInfra(request M, limit int) ([]any, error) {
	pair := jsonx.AsM(request["desiredCspAndRegionPair"])
	p := url.Values{"limit": {fmt.Sprint(limit)}}
	if s := jsonx.S(pair, "csp"); s != "" {
		p.Set("desiredCsp", s)
	}
	if s := jsonx.S(pair, "region"); s != "" {
		p.Set("desiredRegion", s)
	}
	d, err := c.data("POST", "/recommendation/infra", Opts{Body: request, Params: p, Timeout: 300 * time.Second, NoRetry: true, Write: boolp(false)})
	if err != nil {
		return nil, err
	}
	return jsonx.AsList(d), nil
}

// ValidateInfra 는 생성 요청 사전 검사 (POST /validation/ns/{ns}/infra) → {valid, issues[{code, severity, path, message}]}.
// MigrateInfra 가 만들기 직전에 하는 검사(사양 · 이미지 호환, 필수 값, 연결 이름, 이름 충돌)를 자원 없이 미리 돌린다 — dry-run 에서도 부른다.
// 검사에 걸려도 HTTP 200 (valid=false) 이고, 400 은 요청 형식 오류다.
func (c *Client) ValidateInfra(ns string, body M, useExisting bool) (M, error) {
	p := url.Values{"useExisting": {fmt.Sprint(useExisting)}}
	d, err := c.data("POST", "/validation/ns/"+ns+"/infra", Opts{Body: body, Params: p, Timeout: 120 * time.Second, Write: boolp(false)})
	if err != nil {
		return nil, err
	}
	if m := jsonx.AsM(d); m != nil {
		return m, nil
	}
	return nil, errf("Beetle 검증 응답을 읽지 못했습니다: %v", d)
}

// MigrateInfra 는 동기 생성 → MigrateInfraResponse (id · status · node[] · creationErrors). 자동 재시도 없음.
func (c *Client) MigrateInfra(ns string, body M, nameSeed string, useExisting bool, timeout time.Duration) (M, error) {
	p := url.Values{"useExisting": {fmt.Sprint(useExisting)}}
	if nameSeed != "" {
		p.Set("nameSeed", nameSeed)
	}
	d, err := c.data("POST", "/migration/ns/"+ns+"/infra", Opts{Body: body, Params: p, Timeout: timeout, NoRetry: true})
	if err != nil {
		return nil, err
	}
	if m := jsonx.AsM(d); m != nil {
		return m, nil
	}
	return M{}, nil
}

// ListInfraIDs 는 Beetle 이 이 ns 에 만든 인프라 ID 목록이다 (GET …/infra?option=id → data.idList).
func (c *Client) ListInfraIDs(ns string) ([]string, error) {
	d, err := c.data("GET", "/migration/ns/"+ns+"/infra", Opts{Params: url.Values{"option": {"id"}}, Timeout: 60 * time.Second})
	if err != nil {
		return nil, err
	}
	return jsonx.Strings(jsonx.AsM(d)["idList"]), nil
}

// GetInfra 는 Beetle 이 아는 인프라 (없으면 nil).
func (c *Client) GetInfra(ns, id string) (any, error) {
	return c.Req("GET", "/migration/ns/"+ns+"/infra/"+id, Opts{Allow404: true})
}

// SSHReady 는 인프라당 30초에 1회 제한 — 호출 쪽에서 간격을 둔다.
func (c *Client) SSHReady(ns, id string) (bool, M, error) {
	d, err := c.data("GET", "/migration/ns/"+ns+"/infra/"+id+"/ssh-ready", Opts{Params: url.Values{"option": {"detail"}}, Timeout: 60 * time.Second})
	if err != nil {
		return false, nil, err
	}
	m := jsonx.AsM(d)
	return jsonx.Truthy(m["ready"]), m, nil
}

// DeleteInfra 는 Beetle 로 인프라를 지운다.
func (c *Client) DeleteInfra(ns, id string) (any, error) {
	return c.Req("DELETE", "/migration/ns/"+ns+"/infra/"+id, Opts{Params: url.Values{"option": {"terminate"}}, Timeout: 900 * time.Second, NoRetry: true})
}

// ---------- 비동기 요청 ----------

// RequestStatus 는 RequestDetails {status: Handling|Success|Error, …}.
func (c *Client) RequestStatus(reqID string) (M, error) {
	d, err := c.data("GET", "/request/"+reqID, Opts{Timeout: 60 * time.Second})
	if err != nil {
		return nil, err
	}
	if m := jsonx.AsM(d); m != nil {
		return m, nil
	}
	return M{}, nil
}

// WaitRequest 는 요청이 끝날 때까지 기다린다.
func (c *Client) WaitRequest(reqID string, timeout, poll time.Duration, onTick func(...any)) (M, error) {
	t0 := time.Now()
	for {
		st, err := c.RequestStatus(reqID)
		if err != nil {
			return nil, err
		}
		status := strings.ToLower(jsonx.S(st, "status"))
		if onTick != nil {
			onTick(status, int(time.Since(t0).Seconds()))
		}
		if Terminal[status] {
			return st, nil
		}
		if time.Since(t0) > timeout {
			if status == "" {
				status = "?"
			}
			return nil, errf("요청 %s 가 %d초 안에 끝나지 않았습니다 (지금 %s)", reqID, int(timeout.Seconds()), status)
		}
		c.Sleep(poll)
	}
}

// ---------- 데이터 이관 ----------

// EncryptionKey 는 일회용 RSA 공개키 번들 {keyId, algorithm, publicKey, expiresAt} (30분 · 1회).
func (c *Client) EncryptionKey() (M, error) {
	d, err := c.data("GET", "/migration/data/encryptionKey", Opts{Timeout: 60 * time.Second, Write: boolp(true)})
	if err != nil {
		return nil, err
	}
	return jsonx.AsM(d), nil
}

// DataMigration 은 비동기(Prefer: respond-async)로 시작한다. 요청 ID 는 우리가 정해 state 에 먼저 적는다.
// 503(작업 초과)은 시작 전 거절이라 Retry-After 뒤 다시 보낸다. 409(같은 ID)는 앞서 보낸 요청이 이미 받아들여진 것이라
// 오류로 보지 않는다 — 호출하는 쪽은 그 ID 로 상태를 이어서 본다.
func (c *Client) DataMigration(body M, xreq string) (string, error) {
	d, err := c.data("POST", "/migration/data", Opts{Body: body, Timeout: 120 * time.Second, NoRetry: true, RetryBusy: true,
		Headers: map[string]string{"Prefer": "respond-async", "X-Request-Id": xreq}})
	if IsDuplicateRequest(err) {
		c.Log.Info("beetle", "요청 %s 는 이미 Beetle 에 있습니다(409) — 상태를 이어서 봅니다", xreq)
		return xreq, nil
	}
	if err != nil {
		return "", err
	}
	if rid := jsonx.S(jsonx.AsM(d), "reqId"); rid != "" {
		return rid, nil
	}
	return xreq, nil
}

// TestEncrypt 는 [TEST ONLY] 평문 모델을 서버가 암호화한 결과.
func (c *Client) TestEncrypt(body M) (any, error) {
	return c.Req("POST", "/migration/data/test/encrypt", Opts{Body: body, Timeout: 60 * time.Second, NoRetry: true})
}

// TestDecrypt 는 [TEST ONLY] 암호화된 모델을 서버가 푼 결과.
func (c *Client) TestDecrypt(body M) (any, error) {
	return c.Req("POST", "/migration/data/test/decrypt", Opts{Body: body, Timeout: 60 * time.Second, NoRetry: true})
}
