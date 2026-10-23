package beetle

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gpu-migrate-orchestrator/internal/config"
	"gpu-migrate-orchestrator/internal/jsonx"
	"gpu-migrate-orchestrator/internal/logx"
)

func TestDryRunRecommendAllowedWritesBlocked(t *testing.T) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Write([]byte(`{"success":true,"data":[{"status":"partially-matched"}]}`))
	}))
	defer srv.Close()
	b := New(config.Endpoint{URL: srv.URL + "/beetle"}, logx.Quiet(), true)
	b.Sleep = func(time.Duration) {}
	b.MinInterval = 0
	c, err := b.RecommendInfra(jsonx.M{"desiredCspAndRegionPair": jsonx.M{"csp": "gcp", "region": "asia-northeast3"}}, 5)
	if err != nil || len(c) != 1 {
		t.Fatal(c, err)
	}
	if _, err := b.MigrateInfra("default", jsonx.M{}, "x", true, time.Minute); err == nil {
		t.Fatal("dry-run 에서 생성했다")
	}
	if _, err := b.EncryptionKey(); err == nil {
		t.Fatal("dry-run 에서 일회용 키를 발급받았다")
	}
	if _, err := b.DataMigration(jsonx.M{}, "rid"); err == nil {
		t.Fatal("dry-run 에서 데이터 이관을 시작했다")
	}
	if n != 1 {
		t.Fatal(n)
	}
}

func TestUnwrapError(t *testing.T) {
	if _, err := Unwrap(jsonx.M{"success": false, "message": "bad"}); err == nil {
		t.Fatal("실패 응답을 통과시켰다")
	}
	if v, _ := Unwrap(jsonx.M{"success": true, "data": 1}); v != 1 {
		t.Fatal(v)
	}
}

func TestIsNotFound(t *testing.T) {
	if !IsNotFound(&Error{Status: 404}) || !IsNotFound(&Error{Status: 500, Body: `{"message":"request not found"}`}) {
		t.Fatal("없다는 응답을 못 알아봤다")
	}
	if IsNotFound(&Error{Status: 500, Body: "internal error"}) || IsNotFound(errors.New("연결 실패")) || IsNotFound(nil) {
		t.Fatal("다른 오류를 없다는 것으로 봤다")
	}
}

// 503 + Retry-After(작업 초과)는 시작 전 거절이라 Retry-After 만큼 쉬고 다시 보낸다.
func TestDataMigrationRetriesBusyWithRetryAfter(t *testing.T) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.Header().Set("Retry-After", "5")
			w.WriteHeader(503)
			w.Write([]byte(`{"success":false,"error":"Too many async jobs in progress"}`))
			return
		}
		w.WriteHeader(202)
		w.Write([]byte(`{"success":true,"data":{"reqId":"rid-1"}}`))
	}))
	defer srv.Close()
	b := New(config.Endpoint{URL: srv.URL + "/beetle"}, logx.Quiet(), false)
	var slept []time.Duration
	b.Sleep = func(d time.Duration) { slept = append(slept, d) }
	b.MinInterval = 0
	rid, err := b.DataMigration(jsonx.M{}, "rid-1")
	if err != nil || rid != "rid-1" || n != 2 {
		t.Fatal(rid, err, n)
	}
	if len(slept) != 1 || slept[0] < 5*time.Second || slept[0] > 6*time.Second {
		t.Fatal("Retry-After 를 따르지 않았다", slept)
	}
}

// 같은 X-Request-Id 의 409 는 이미 받아들여진 요청 — 오류가 아니다.
func TestDataMigrationDuplicateIsAccepted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		w.Write([]byte(`{"message":"the X-Request-Id 'rid-1' is already in use; use DELETE /beetle/request/{reqId} to remove it"}`))
	}))
	defer srv.Close()
	b := New(config.Endpoint{URL: srv.URL + "/beetle"}, logx.Quiet(), false)
	b.Sleep = func(time.Duration) {}
	b.MinInterval = 0
	if rid, err := b.DataMigration(jsonx.M{}, "rid-1"); err != nil || rid != "rid-1" {
		t.Fatal(rid, err)
	}
}

// 동기 생성의 503 은 생성 도중일 수 있어 다시 보내지 않는다.
func TestMigrateInfraBusyNotResent(t *testing.T) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if r.URL.Query().Get("useExisting") != "false" || r.URL.Query().Get("nameSeed") != "wr-gcp-10231015" {
			t.Errorf("쿼리 %v", r.URL.RawQuery)
		}
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(503)
	}))
	defer srv.Close()
	b := New(config.Endpoint{URL: srv.URL + "/beetle"}, logx.Quiet(), false)
	b.Sleep = func(time.Duration) {}
	b.MinInterval = 0
	if _, err := b.MigrateInfra("default", jsonx.M{}, "wr-gcp-10231015", false, time.Minute); err == nil || n != 1 {
		t.Fatal("동기 생성을 다시 보냈거나 503 을 넘겼다", err, n)
	}
}

func TestListInfraIDsAndRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"success":true,"data":{"idList":["wr-gcp-10231015-infra101","other"]}}`))
	}))
	defer srv.Close()
	b := New(config.Endpoint{URL: srv.URL + "/beetle"}, logx.Quiet(), false)
	b.MinInterval = 0
	ids, err := b.ListInfraIDs("default")
	if err != nil || len(ids) != 2 || ids[0] != "wr-gcp-10231015-infra101" {
		t.Fatal(ids, err)
	}
	if RetryAfter("3") != 3*time.Second || RetryAfter("999") != 60*time.Second || RetryAfter("") != 0 || RetryAfter("x") != 0 {
		t.Fatal("Retry-After 해석")
	}
}
