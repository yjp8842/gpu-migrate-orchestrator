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
