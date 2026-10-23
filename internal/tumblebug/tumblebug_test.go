package tumblebug

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gpu-migrate-orchestrator/internal/config"
	"gpu-migrate-orchestrator/internal/jsonx"
	"gpu-migrate-orchestrator/internal/logx"
)

type resp struct {
	status int
	body   string
}

type rec struct {
	method, path, query, xreq string
	body                      string
}

func fixture(t *testing.T, name string) string {
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func serve(t *testing.T, dry bool, rs ...resp) (*Client, *[]rec) {
	var mu sync.Mutex
	var got []rec
	i := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, rec{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("X-Request-Id"), string(b)})
		cur := resp{500, "no more"}
		if i < len(rs) {
			cur = rs[i]
		}
		i++
		mu.Unlock()
		w.WriteHeader(cur.status)
		w.Write([]byte(cur.body))
	}))
	t.Cleanup(srv.Close)
	c := New(config.Endpoint{Name: "t", URL: srv.URL + "/tumblebug", User: "u", Password: "p"}, logx.Quiet(),
		map[string]float64{"infra_get_interval_sec": 0, "default_interval_sec": 0}, dry)
	c.Sleep = func(time.Duration) {}
	return c, &got
}

func TestAccessInfoMixedCase(t *testing.T) {
	c, _ := serve(t, false, resp{200, fixture(t, "tb-accessinfo.json")})
	nodes, err := c.AccessInfo("default", "widgetrag-shell-aws")
	if err != nil {
		t.Fatal(err)
	}
	if nodes[0]["publicIP"] != "198.51.100.67" || nodes[0]["nodeUserName"] != "cb-user" || !strings.Contains(jsonx.S(nodes[0], "privateKey"), "PRIVATE KEY") {
		t.Fatal(nodes[0])
	}
}

func TestNodesAndState(t *testing.T) {
	v, _ := jsonx.Decode([]byte(fixture(t, "tb-infra.json")))
	infra := jsonx.AsM(v)
	if InfraState(infra) != "Running" {
		t.Fatal(InfraState(infra))
	}
	n := Nodes(infra)[0]
	if jsonx.Strings(n["securityGroupIds"])[0] != "widgetrag-shell-aws-g1" || n["cspResourceId"] != "i-0123456789abcdef0" {
		t.Fatal(n)
	}
}

func Test429BackoffThenSuccess(t *testing.T) {
	c, got := serve(t, false, resp{429, "rate limit exceeded"}, resp{429, "rate limit exceeded"}, resp{200, `{"output":["a","b"]}`})
	ids, err := c.ListInfra("default")
	if err != nil || strings.Join(ids, ",") != "a,b" || len(*got) != 3 {
		t.Fatal(ids, err, len(*got))
	}
}

func TestPostNotRetriedOn500(t *testing.T) {
	c, got := serve(t, false, resp{500, "boom"})
	if _, err := c.CmdAsync("default", "x", []string{"true"}, "", 30, "gmp-1"); err == nil || len(*got) != 1 {
		t.Fatal(err, len(*got))
	}
}

func TestCmdAsyncRequestIDAndTimeoutCap(t *testing.T) {
	c, got := serve(t, false, resp{202, `{"xRequestId":"gmp-1","message":"started"}`})
	rid, err := c.CmdAsync("default", "x", []string{"sleep 1"}, "cb-user", 999, "gmp-1")
	if err != nil || rid != "gmp-1" {
		t.Fatal(rid, err)
	}
	r := (*got)[0]
	if r.xreq != "gmp-1" || r.query != "async=true" || !strings.Contains(r.body, `"timeoutMinutes":120`) || !strings.Contains(r.body, `"userName":"cb-user"`) {
		t.Fatal(r)
	}
}

func TestWaitCommandTerminal(t *testing.T) {
	c, _ := serve(t, false, resp{200, fixture(t, "tb-commandstatus.json")})
	done, err := c.WaitCommand("default", "x", []string{"g1-1"}, "gmp-20261022-100000-backup-abc123", 10*time.Second, 0, nil)
	if err != nil || done["g1-1"]["status"] != "Completed" {
		t.Fatal(done, err)
	}
}

func TestInfraDynamicKeyAllowlist(t *testing.T) {
	if ValidateInfraDynamic(M{"name": "x", "nodeGroup": []any{}}) == nil {
		t.Fatal("모르는 키를 통과시켰다")
	}
	if ValidateInfraDynamic(M{"name": "x", "nodeGroups": []any{M{"name": "g1", "nodeGroupSize": 1, "rootDiskSize": "100"}}}) == nil {
		t.Fatal("문자열 디스크 크기를 통과시켰다")
	}
	if err := ValidateInfraDynamic(M{"name": "x", "policyOnPartialFailure": "rollback",
		"nodeGroups": []any{M{"name": "g1", "nodeGroupSize": 1, "specId": "s", "imageId": "i", "rootDiskSize": 100}}}); err != nil {
		t.Fatal(err)
	}
}

func TestDryRunReadsButNeverWrites(t *testing.T) {
	c, got := serve(t, true, resp{200, `{"id":"x","node":[]}`})
	if m, err := c.GetInfra("default", "x"); err != nil || m["id"] != "x" {
		t.Fatal(m, err) // 읽기 전용 조회는 한다
	}
	if _, err := c.CmdSync("default", "x", []string{"true"}, "", 1); err == nil {
		t.Fatal("dry-run 에서 원격 명령을 보냈다")
	}
	if _, err := c.Control("default", "x", "suspend"); err == nil {
		t.Fatal("dry-run 에서 제어를 보냈다 (GET 이지만 쓰기)")
	}
	if len(*got) != 1 {
		t.Fatal(len(*got))
	}
}

func TestDryRunAllowsReadOnlyPosts(t *testing.T) {
	c, got := serve(t, true, resp{200, `{"imageList":[]}`}, resp{200, `[]`})
	if _, err := c.SearchImage("system", "aws", "r", "ubuntu 24.04", true, 20); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RecommendSpec([]Filter{F("vCPU", ">=", 4)}, "cost", 10); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 2 || !strings.Contains((*got)[1].body, `"weight":1`) || !strings.Contains((*got)[1].body, `"operand":"4"`) {
		t.Fatal(*got)
	}
}

func TestMissingResourceReturnsNilWithoutRetry(t *testing.T) {
	c, got := serve(t, false, resp{500, `{"message":"infra widgetrag-src-aws does not exist"}`}, resp{500, `{"message":"vNet x does not exist"}`},
		resp{500, `{"message":"the image aws+x does not exist"}`})
	if m, err := c.GetInfra("default", "widgetrag-src-aws"); m != nil || err != nil {
		t.Fatal(m, err)
	}
	if m, err := c.GetVNet("default", "x"); m != nil || err != nil {
		t.Fatal(m, err)
	}
	if m, err := c.GetImage("default", "aws+x"); m != nil || err != nil {
		t.Fatal(m, err)
	}
	if len(*got) != 3 {
		t.Fatal(len(*got))
	}
}

func TestServerErrorStillRetried(t *testing.T) {
	c, got := serve(t, false, resp{500, "db busy"}, resp{200, `{"id":"x"}`})
	if m, err := c.GetInfra("default", "x"); err != nil || m["id"] != "x" || len(*got) != 2 {
		t.Fatal(m, err, len(*got))
	}
}

func TestFindImagePrefersNonMinimal(t *testing.T) {
	c, got := serve(t, false, resp{200, `{"imageCount":2,"imageList":[
	 {"id":"ami-min","cspImageName":"ami-min","isBasicImage":true,"osDistribution":"ubuntu-minimal/images/ubuntu-noble-24.04-minimal"},
	 {"id":"ami-srv","cspImageName":"ami-srv","isBasicImage":true,"osDistribution":"ubuntu/images/ubuntu-noble-24.04-server"}]}`})
	pick, err := c.FindImage("aws", "ap-northeast-2", "ubuntu 24.04")
	if err != nil || pick["id"] != "ami-srv" || !strings.Contains((*got)[0].path, "/ns/system/resources/searchImage") {
		t.Fatal(pick, err)
	}
}

func TestSyncStdoutOrder(t *testing.T) {
	res := []M{{"nodeId": "g1-1", "stdout": M{"1": "b", "0": "a", "10": "k", "2": "c"}, "stderr": M{}}}
	out, _, e := SyncStdout(res, "g1-1")
	if strings.Join(out, "") != "abck" || e != "" {
		t.Fatal(out, e)
	}
}
