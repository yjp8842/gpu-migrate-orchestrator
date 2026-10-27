package api

import (
	"archive/zip"
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func baseInput() caseInput {
	return caseInput{Workload: "widgetrag-a", SourceInfra: "widgetrag-aws-src", SourceCSP: "aws", CSP: "gcp", Region: "asia-northeast3", Name: "wr-gcp"}
}

func TestBuildCase(t *testing.T) {
	now := time.Date(2026, 10, 26, 10, 15, 0, 0, time.Local)
	name, doc, err := buildCase(baseInput(), now)
	if err != nil {
		t.Fatal(err)
	}
	if name != "aws-to-gcp-widgetrag-a-10261015" || doc.Target.Provider != "beetle" || doc.Target.Zone != nil {
		t.Fatalf("이름 · 타겟: %s %+v", name, doc.Target)
	}
	if doc.Policy.Image != "source" || doc.Policy.DriverSeries != "" || doc.Policy.OpenPorts[0].Port != 80 || doc.Run.Golden == nil || !*doc.Run.Golden {
		t.Fatalf("정책 · run: %+v %+v", doc.Policy, doc.Run)
	}
	b, err := caseYAML(doc)
	if err != nil {
		t.Fatal(err)
	}
	// 샘플과 같은 키 순서 · null · 빈 문자열 · ${…} 그대로
	text := string(b)
	for _, want := range []string{"case: aws-to-gcp-widgetrag-a-10261015\nworkload: widgetrag-a\nsource:", "zone: null", "spec: null", `driver_series: ""`, "cidr: ${GMP_WEB_ALLOW_CIDR}"} {
		if !strings.Contains(text, want) {
			t.Fatalf("YAML 에 %q 없음:\n%s", want, text)
		}
	}
	var back map[string]any
	if err := yaml.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back["policy"].(map[string]any)["open_ports"].([]any)[0].(map[string]any)["cidr"] != "${GMP_WEB_ALLOW_CIDR}" {
		t.Fatalf("되읽기: %v", back["policy"])
	}

	in := baseInput()
	in.Workload, in.CSP, in.Region, in.Name, in.Zone = "lexai-a", "aws", "ap-northeast-2", "lx-aws", "ap-northeast-2a"
	_, lx, err := buildCase(in, now)
	if err != nil {
		t.Fatal(err)
	}
	if lx.Policy.OpenPorts[0].Port != 3000 || lx.Target.Zone == nil || *lx.Target.Zone != "ap-northeast-2a" || lx.Run.Golden != nil {
		t.Fatalf("lexai-a: %+v %+v", lx.Policy, lx.Run)
	}
	if b, _ := caseYAML(lx); strings.Contains(string(b), "golden") {
		t.Fatalf("lexai 에 golden 이 들어감:\n%s", b)
	}
}

func TestInfraStatus(t *testing.T) {
	for in, want := range map[string]string{"Running:1 (R:1/1)": "Running", "Suspended:1 (S:1/1)": "Suspended", "Failed": "Failed", "": "Unknown"} {
		if got := infraStatus(M{"status": in}); got != want {
			t.Errorf("%q → %q, 기대 %q", in, got, want)
		}
	}
	if infraStatus(nil) != "NotFound" {
		t.Error("없는 인프라")
	}
}

func TestBuildCaseOpenPorts(t *testing.T) {
	in := baseInput()
	rules := []portInput{{Port: 80, CIDR: MyIPCIDR}, {Port: 8080, CIDR: " 10.20.0.0/16 "}}
	in.Policy.OpenPorts = &rules
	_, doc, err := buildCase(in, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := doc.Policy.OpenPorts; len(got) != 2 || got[0].CIDR != MyIPCIDR || got[1] != (portDoc{Port: 8080, CIDR: "10.20.0.0/16"}) {
		t.Fatalf("열 포트 %+v", got)
	}
	none := []portInput{}
	in.Policy.OpenPorts = &none
	if _, doc, _ = buildCase(in, time.Now()); len(doc.Policy.OpenPorts) != 0 {
		t.Fatalf("빈 목록이면 열지 않아야 함: %+v", doc.Policy.OpenPorts)
	}
}

func TestBuildCaseRejects(t *testing.T) {
	disk := 5.0
	ports := func(r ...portInput) func(*caseInput) { return func(c *caseInput) { c.Policy.OpenPorts = &r } }
	cases := map[string]func(*caseInput){
		"모르는 워크로드":    func(c *caseInput) { c.Workload = "x" },
		"타겟 이름":       func(c *caseInput) { c.Name = "WR GCP" },
		"소스 인프라":      func(c *caseInput) { c.SourceInfra = "../x" },
		"리전 이름":       func(c *caseInput) { c.Region = "a b" },
		"CSP 이름":      func(c *caseInput) { c.CSP = "gcp;rm" },
		"루트 디스크":      func(c *caseInput) { c.Policy.RootDiskGB = &disk },
		"이미지 정책":      func(c *caseInput) { c.Policy.Image = "$(id)" },
		"정리는":         func(c *caseInput) { c.Policy.Cleanup = "delete" },
		"sourceInfra": func(c *caseInput) { c.SourceInfra = "" },
		"네임스페이스":      func(c *caseInput) { c.NS = "../ns" },
		"1~65535":     ports(portInput{Port: 0, CIDR: MyIPCIDR}),
		"너무 넓습니다":     ports(portInput{Port: 80, CIDR: "0.0.0.0/0"}),
		"네트워크 주소로":    ports(portInput{Port: 80, CIDR: "10.20.1.5/16"}),
		"IPv4 대역":     ports(portInput{Port: 80, CIDR: "$(id)"}),
		"겹칩니다":        ports(portInput{Port: 80, CIDR: "10.0.0.1/32"}, portInput{Port: 80, CIDR: "10.0.0.1/32"}),
	}
	for want, mod := range cases {
		in := baseInput()
		mod(&in)
		if _, _, err := buildCase(in, time.Now()); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: 오류 %v", want, err)
		}
	}
}

func TestWriteReadCase(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GMP_WEB_ALLOW_CIDR", "")
	if _, _, err := writeCase(root, baseInput()); err == nil || !strings.Contains(err.Error(), "GMP_WEB_ALLOW_CIDR") {
		t.Fatalf(".env 값 없이 「내 IP」를 받으면 안 됨: %v", err)
	}
	t.Setenv("GMP_WEB_ALLOW_CIDR", "203.0.113.254/32")
	name, rel, err := writeCase(root, baseInput())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rel, filepath.Join("cases", "console")+string(filepath.Separator)) {
		t.Fatalf("상대 경로: %s", rel)
	}
	r2, plan, ok, err := readCase(root, name)
	if err != nil || !ok || r2 != rel {
		t.Fatalf("readCase: %v %v %s", err, ok, r2)
	}
	if plan.WorkloadLabel != "WidgetRAG Shell" || plan.Port != 80 || plan.SourceInfra != "widgetrag-aws-src" || plan.Region != "asia-northeast3" || plan.Zone != nil {
		t.Fatalf("plan: %+v", plan)
	}
	if _, _, ok, _ := readCase(root, "aws-to-gcp-none"); ok {
		t.Fatal("없는 사례가 있다고 함")
	}
	if _, _, _, err := readCase(root, "../etc"); err == nil {
		t.Fatal("이상한 이름을 받음")
	}
}

func TestSummarizeInfraAndRegions(t *testing.T) {
	s := summarizeInfra(M{"id": "src", "status": "Running:1 (R:1/1)", "description": "Made via cb-mapui", "node": []any{M{
		"connectionName": "aws-ap-northeast-2", "region": M{"region": "ap-northeast-2", "zone": "ap-northeast-2a"}, "cspSpecName": "g4dn.xlarge",
		"spec":  M{"acceleratorModel": "NVIDIA T4", "acceleratorCount": 1.0, "acceleratorMemoryGB": 17.0, "vCPU": 4.0, "memoryGiB": 16.0},
		"image": M{"osType": "Ubuntu 24.04"}, "publicIP": "1.2.3.4"}}})
	if s["status"] != "Running" || s["csp"] != "aws" || s["gpu"] != "NVIDIA T4 · 17GB" || s["madeBy"] != "MapUI" || s["spec"] != "g4dn.xlarge" {
		t.Fatalf("summarizeInfra: %v", s)
	}
	g := groupRegions([]M{{"configName": "gcp-asia-northeast3", "providerName": "gcp", "regionDetail": M{"regionId": "asia-northeast3",
		"location": M{"display": "South Korea (Seoul)"}, "zones": []any{"a", "b"}}, "regionZoneInfo": M{"assignedZone": "a"}}})
	r := g["gcp"][0]
	if r["connection"] != "gcp-asia-northeast3" || r["label"] != "South Korea (Seoul)" || r["defaultZone"] != "a" || len(r["zones"].([]string)) != 2 {
		t.Fatalf("groupRegions: %v", r)
	}
}

func TestOverallStatus(t *testing.T) {
	st := func(stages M, verdict M) M { return M{"stages": stages, "verdict": verdict} }
	if overallStatus(st(M{}, nil), true)["key"] != "running" {
		t.Fatal("running")
	}
	if overallStatus(st(M{"provision": M{"status": "failed"}}, nil), false)["label"] != "중단 (provision)" {
		t.Fatal("failed")
	}
	if overallStatus(st(M{"verify": M{"status": "done"}}, M{"result": "PASS"}), false)["key"] != "pass" {
		t.Fatal("pass")
	}
	if overallStatus(st(M{"start": M{"status": "running"}}, nil), false)["key"] != "stopped" {
		t.Fatal("stopped")
	}
}

func TestCompareSummary(t *testing.T) {
	dir := t.TempDir()
	ev := filepath.Join(dir, "verify", "evidence")
	os.MkdirAll(filepath.Join(ev, "golden-diff"), 0o755)
	os.WriteFile(filepath.Join(ev, "summary.txt"), []byte("이관 동등성: 테스트1 PASS · 테스트2 FAIL\n\ntest   판정      PASS  FAIL  WARN  SKIP  INFO  항목\nT1-1   PASS       1     0     0     0     0  서비스 수·역할\nT2     FAIL      17     3     3     0     0  LLM 응답 완전 일치(골든)\n"), 0o644)
	os.WriteFile(filepath.Join(ev, "rows.tsv"), []byte("T2\tq01\ta:1 p:2\ta:1 p:2\tPASS\t완전 일치\nT2\tq04\ta:1 p:2\ta:9 p:2\tFAIL\t첫 차이 2자\nT1-3\truntime:gpu\tTesla T4\tNVIDIA L4\tWARN\t값 다름\n"), 0o644)
	os.WriteFile(filepath.Join(ev, "golden-diff", "q04.diff"), []byte("x"), 0o644)
	c := compareSummary(dir)
	tests := c["tests"].([]M)
	if c["headline"] != "이관 동등성: 테스트1 PASS · 테스트2 FAIL" || len(tests) != 2 || tests[1]["fail"] != 3 || len(c["golden"].([]M)) != 2 {
		t.Fatalf("compareSummary: %v", c)
	}
	if w := c["warns"].([]string); len(w) != 1 || w[0] != "T1-3 runtime:gpu — Tesla T4 → NVIDIA L4" {
		t.Fatalf("warns: %v", w)
	}
	if d := c["diffs"].([]string); len(d) != 1 || d[0] != "q04.diff" {
		t.Fatalf("diffs: %v", d)
	}
}

func TestRunPID(t *testing.T) {
	dir := t.TempDir()
	running := M{"stages": M{"start": M{"status": "running"}}}
	os.WriteFile(filepath.Join(dir, ".lock"), []byte(strconv.Itoa(os.Getpid())), 0o644)
	if runPID(dir, running) != os.Getpid() {
		t.Fatal("살아 있는 pid 를 못 봄")
	}
	if runPID(dir, M{"stages": M{"start": M{"status": "done"}}}) != 0 {
		t.Fatal("끝난 run 을 도는 것으로 봄")
	}
	os.WriteFile(filepath.Join(dir, ".lock"), []byte("999999999"), 0o644)
	if runPID(dir, running) != 0 {
		t.Fatal("죽은 pid 를 도는 것으로 봄")
	}
}

func TestFilePathAndZip(t *testing.T) {
	ev := t.TempDir()
	dir := filepath.Join(ev, "c1", "20261026-101500")
	os.MkdirAll(filepath.Join(dir, "verify"), 0o755)
	os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{}`), 0o644)
	os.WriteFile(filepath.Join(dir, ".lock"), []byte("1"), 0o644)
	os.WriteFile(filepath.Join(dir, "verify", "a.txt"), []byte("hello"), 0o644)
	for _, bad := range []string{"../../x", "../20261026-101501/state.json", "/etc/passwd"} {
		if p, err := filePath(ev, "c1", "20261026-101500", bad); err == nil && !strings.HasPrefix(p, dir+string(filepath.Separator)) {
			t.Fatalf("%s → %s 를 받음", bad, p)
		}
	}
	if _, err := filePath(ev, "c1", "20261026-101500", "../../x"); err == nil {
		t.Fatal("run 폴더 밖 경로를 받음")
	}
	if _, err := runDir(ev, "../c1", "20261026-101500"); err == nil {
		t.Fatal("이상한 사례 이름을 받음")
	}
	var buf bytes.Buffer
	if err := writeZip(&buf, dir, "c1/20261026-101500"); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "c1/20261026-101500/state.json,c1/20261026-101500/verify/a.txt" {
		t.Fatalf("ZIP: %v", names)
	}
}

func TestGuard(t *testing.T) {
	h := (&Server{Root: t.TempDir()}).Handler()
	do := func(method, host, ctype, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://"+host+"/api/workloads", strings.NewReader("{}"))
		r.Host = host
		if ctype != "" {
			r.Header.Set("Content-Type", ctype)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	const host = "127.0.0.1:5180"
	ok := do("GET", host, "", "")
	if ok.Code != http.StatusOK || ok.Header().Get("X-Frame-Options") != "DENY" || !strings.Contains(ok.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("루프백 GET: %d %v", ok.Code, ok.Header())
	}
	if c := do("GET", "evil.example:5180", "", "").Code; c != http.StatusForbidden {
		t.Fatalf("다른 Host: %d", c)
	}
	if c := do("POST", host, "text/plain", "").Code; c != http.StatusUnsupportedMediaType {
		t.Fatalf("폼 POST: %d", c)
	}
	if c := do("POST", host, "application/json", "http://evil.example").Code; c != http.StatusForbidden {
		t.Fatalf("다른 출처 POST: %d", c)
	}
	// 같은 출처 JSON POST 는 지나간다 — workloads 에는 POST 가 없어 404
	if c := do("POST", host, "application/json", "http://"+host).Code; c != http.StatusNotFound {
		t.Fatalf("같은 출처 POST: %d", c)
	}
}

func TestListenOnlyLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:5180", ":5180", "192.168.0.10:5180"} {
		if err := (&Server{}).ListenAndServe(addr); err == nil || !strings.Contains(err.Error(), "루프백") {
			t.Fatalf("%s: %v", addr, err)
		}
	}
}

func TestFilePathRejectsLinks(t *testing.T) {
	ev := t.TempDir()
	dir := filepath.Join(ev, "c1", "20261026-101500")
	os.MkdirAll(dir, 0o755)
	secret := filepath.Join(t.TempDir(), "secret.env")
	os.WriteFile(secret, []byte("X=1"), 0o600)
	os.Symlink(secret, filepath.Join(dir, "link.txt"))
	os.Symlink(filepath.Dir(secret), filepath.Join(dir, "linkdir"))
	os.WriteFile(filepath.Join(dir, ".lock"), []byte("1"), 0o644)
	for _, rel := range []string{"link.txt", "linkdir/secret.env", ".lock", "none.txt"} {
		if _, err := filePath(ev, "c1", "20261026-101500", rel); err == nil {
			t.Fatalf("%s 를 받음", rel)
		}
	}
}

func TestMaskContent(t *testing.T) {
	in := []byte(`{"entry":"LLM_SEED=42 VERIFY_ADMIN_PASSWORD=s3cr3t bash"}`)
	if out := string(maskContent("state.json", in)); strings.Contains(out, "s3cr3t") {
		t.Fatalf("가리지 않음: %s", out)
	}
	gz := []byte("VERIFY_ADMIN_PASSWORD=s3cr3t")
	if string(maskContent("app-logs.tar.gz", gz)) != string(gz) {
		t.Fatal("압축 파일을 건드림")
	}
}

func TestCasePath(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "cases", "console"), 0o755)
	os.WriteFile(filepath.Join(root, "cases", "console", "a.yaml"), []byte("case: a"), 0o644)
	s := &Server{Root: root}
	if p, err := s.casePath(M{"casePath": "cases/console/a.yaml"}); err != nil || !strings.HasSuffix(p, "a.yaml") {
		t.Fatalf("정상 경로: %s %v", p, err)
	}
	for _, bad := range []string{"", "../x.yaml", "/etc/passwd", "cases/../settings.yaml", "-h", "cases/console/none.yaml"} {
		if _, err := s.casePath(M{"casePath": bad}); err == nil {
			t.Fatalf("%q 를 받음", bad)
		}
	}
}

func TestLockedPID(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, ".lock")
	os.WriteFile(lock, []byte(strconv.Itoa(os.Getpid())), 0o644)
	if lockedPID(dir) != 0 {
		t.Fatal("아무도 안 잡은 잠금을 잡힌 것으로 봄 (pid 재사용 위험)")
	}
	f, _ := os.OpenFile(lock, os.O_RDWR, 0)
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if lockedPID(dir) != os.Getpid() {
		t.Fatal("잡힌 잠금의 pid 를 못 봄")
	}
}

func TestNamespace(t *testing.T) {
	now := time.Now()
	_, doc, err := buildCase(baseInput(), now)
	if err != nil || doc.Source.NS != "default" || doc.Target.NS != "default" {
		t.Fatalf("기본 네임스페이스: %v %+v %+v", err, doc.Source, doc.Target)
	}
	in := baseInput()
	in.NS = "team-a"
	_, doc, err = buildCase(in, now)
	if err != nil || doc.Source.NS != "team-a" || doc.Target.NS != "team-a" {
		t.Fatalf("고른 네임스페이스: %v %+v %+v", err, doc.Source, doc.Target)
	}
	root := t.TempDir()
	t.Setenv("GMP_WEB_ALLOW_CIDR", "203.0.113.254/32")
	name, _, err := writeCase(root, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, plan, ok, _ := readCase(root, name); !ok || plan.NS != "team-a" {
		t.Fatalf("plan 네임스페이스: %+v", plan)
	}
	got := namespaces([]M{{"id": "zeta"}, {"id": "default", "description": "기본"}, {"id": "alpha"}, {"id": "Bad NS"}, {"id": "system"}})
	ids := []string{}
	for _, n := range got {
		ids = append(ids, n["id"].(string))
	}
	if strings.Join(ids, ",") != "default,alpha,zeta" {
		t.Fatalf("네임스페이스 목록: %v", ids)
	}
	if _, err := (&Server{}).listSources("../x"); err == nil {
		t.Fatal("이상한 네임스페이스로 조회함")
	}
}
