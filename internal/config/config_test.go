package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gpu-migrate-orchestrator/internal/jsonx"
)

func root(t *testing.T) string {
	t.Helper()
	return ProjectRoot()
}

// caseEnv 는 사례 파일의 ${…} 를 채울 시험용 값이다.
func caseEnv(t *testing.T) {
	t.Setenv("GMP_WEB_ALLOW_CIDR", "203.0.113.254/32")
	t.Setenv("GMP_VERIFY_ADMIN_PASSWORD", "test-pw")
}

func TestCasesAndAdaptersLoad(t *testing.T) {
	caseEnv(t)
	for _, name := range []string{"aws-to-aws-widgetrag-a", "aws-to-aws-widgetrag-a-beetle", "aws-to-gcp-widgetrag-a", "aws-to-aws-lexai-a", "aws-to-gcp-lexai-a"} {
		c, err := LoadCase(filepath.Join(root(t), "cases", name+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		ad, err := LoadAdapter(c.Raw["workload"].(string))
		if err != nil {
			t.Fatal(err)
		}
		if c.Name != name {
			t.Fatalf("name %s", c.Name)
		}
		if f := ad.S("form"); f != "shell" && f != "compose" {
			t.Fatalf("form %s", f)
		}
		if c.Policy()["on_perf_fail"] != "stop" {
			t.Fatal("on_perf_fail")
		}
		if c.Transfer()["method"] != "beetle" {
			t.Fatalf("%s: 이관 기본은 Beetle 이어야 한다 (지금 %v)", name, c.Transfer()["method"])
		}
		if p := c.OpenPorts(); len(p) != 1 || p[0]["cidr"] != "203.0.113.254/32" {
			t.Fatalf("%s: open_ports 의 ${GMP_WEB_ALLOW_CIDR} 가 채워지지 않았다: %v", name, p)
		}
	}
}

func TestCaseMissingEnvNamed(t *testing.T) {
	t.Setenv("GMP_WEB_ALLOW_CIDR", "")
	t.Setenv("GMP_VERIFY_ADMIN_PASSWORD", "")
	_, err := LoadCase(filepath.Join(root(t), "cases", "aws-to-aws-widgetrag-a.yaml"))
	if err == nil || !strings.Contains(err.Error(), "GMP_VERIFY_ADMIN_PASSWORD, GMP_WEB_ALLOW_CIDR") {
		t.Fatalf("빈 환경변수를 이름으로 알리지 않았다: %v", err)
	}
}

// 샘플은 채울 줄이 비어 있어 그대로는 멈추고, 채우면 기본값 그대로 읽힌다.
func TestCaseSample(t *testing.T) {
	caseEnv(t)
	path := filepath.Join(root(t), "cases", "case.sample.yaml")
	if _, err := LoadCase(path); err == nil || !strings.Contains(err.Error(), "'case'") {
		t.Fatalf("빈 샘플이 멈추지 않았다: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fill := map[string]string{"case:": "case: sample-filled", "workload:": "workload: widgetrag-a", "  infra:": "  infra: src",
		"  csp:": "  csp: aws", "  region:": "  region: ap-northeast-2", "  name:": "  name: wr"}
	lines := strings.Split(string(b), "\n")
	for i, l := range lines {
		for k, v := range fill {
			if strings.HasPrefix(l, k+" ") && strings.Contains(l, "# 채움") {
				lines[i] = v
			}
		}
	}
	filled := filepath.Join(t.TempDir(), "sample-filled.yaml")
	if err := os.WriteFile(filled, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCase(filled)
	if err != nil {
		t.Fatal(err)
	}
	if c.Target()["provider"] != "beetle" || c.Policy()["driver_series"] != "" || c.Cleanup() != "suspend" {
		t.Fatalf("샘플 기본값: %v %v %v", c.Target()["provider"], c.Policy()["driver_series"], c.Cleanup())
	}
	if _, err := LoadAdapter(c.Raw["workload"].(string)); err != nil {
		t.Fatal(err)
	}
}

func TestEnvOrderFollowsFile(t *testing.T) {
	caseEnv(t)
	c, err := LoadCase(filepath.Join(root(t), "cases", "aws-to-aws-widgetrag-a.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := EnvString(c.Env()); got != "LLM_TEMPERATURE=0 LLM_SEED=42 COMPARE=1 CODE_SOURCE=package VERIFY_ADMIN_PASSWORD=test-pw" {
		t.Fatalf("env 순서: %s", got)
	}
}

func base() map[string]any {
	return map[string]any{"case": "x", "workload": "widgetrag-a", "source": map[string]any{"ns": "default", "infra": "s"}}
}

func TestTumblebugProviderRequiresSpec(t *testing.T) {
	r := base()
	r["target"] = map[string]any{"provider": "tumblebug", "csp": "aws", "region": "r", "name": "t"}
	if _, err := NewCase("x.yaml", r, nil); err == nil {
		t.Fatal("spec 없이 통과했다")
	}
}

func TestNextCandidateNotImplemented(t *testing.T) {
	r := base()
	r["target"] = map[string]any{"provider": "beetle", "csp": "gcp", "region": "r", "name": "t"}
	r["policy"] = map[string]any{"on_perf_fail": "next-candidate"}
	if _, err := NewCase("x.yaml", r, nil); err == nil {
		t.Fatal("next-candidate 가 통과했다")
	}
}

func TestDefaults(t *testing.T) {
	r := base()
	r["target"] = map[string]any{"provider": "beetle", "csp": "gcp", "region": "r", "name": "t"}
	c, err := NewCase("x.yaml", r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Transfer()["method"] != "beetle" || c.Policy()["driver_series"] != "" || c.Target()["ns"] != "default" || c.Cleanup() != "suspend" {
		t.Fatalf("기본값: %v %v", c.Transfer(), c.Policy())
	}
}

func TestStreamTransferRemoved(t *testing.T) {
	r := base()
	r["target"] = map[string]any{"provider": "beetle", "csp": "gcp", "region": "r", "name": "t"}
	r["transfer"] = map[string]any{"method": "stream"}
	if _, err := NewCase("x.yaml", r, nil); err == nil || !strings.Contains(err.Error(), "SSH") {
		t.Fatalf("stream 이 통과했다: %v", err)
	}
}

func TestSettingsSecretPasswordRemoved(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.yaml")
	body := "tumblebugs:\n  default:\n    url: http://tb/tumblebug\nverify_admin_password: x\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSettings(p); err == nil || !strings.Contains(err.Error(), "run.env") {
		t.Fatalf("verify_admin_password 가 조용히 통과했다: %v", err)
	}
}

func TestPlaceholderAdapterMessage(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "adapters"), 0o755)
	os.WriteFile(filepath.Join(dir, "adapters", "todo-x.yaml"), []byte("todo: \"인터페이스 정렬 뒤에 채운다\"\nworkload: x\n"), 0o644)
	t.Setenv("GMP_ROOT", dir)
	_, err := LoadAdapter("todo-x")
	if err == nil || !strings.Contains(err.Error(), "자리만") {
		t.Fatalf("err=%v", err)
	}
}

func TestEnvStringAndRender(t *testing.T) {
	if got := EnvString([]KV{{"A", "1"}, {"B", "x y"}}); got != "A=1 B='x y'" {
		t.Fatal(got)
	}
	if got, _ := Render("{a}-{b}", map[string]string{"a": "x", "b": "y"}); got != "x-y" {
		t.Fatal(got)
	}
	if got, _ := Render("echo '{{}}' {a}", map[string]string{"a": "1"}); got != "echo '{}' 1" {
		t.Fatal(got)
	}
	if _, err := Render("{missing}", nil); err == nil {
		t.Fatal("빈 자리를 통과시켰다")
	}
}

func TestSettingsExample(t *testing.T) {
	t.Setenv("GMP_TB_URL", "http://tb.example:1323/tumblebug/")
	t.Setenv("GMP_TB_USER", "u")
	t.Setenv("GMP_TB_PASSWORD", "p")
	t.Setenv("GMP_BEETLE_URL", "")
	t.Setenv("GMP_EVIDENCE_ROOT", "")
	s, err := LoadSettings(filepath.Join(root(t), "settings.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if tb := s.Tumblebugs["default"]; tb.URL != "http://tb.example:1323/tumblebug" || tb.User != "u" || tb.Password != "p" {
		t.Fatal(s.Tumblebugs)
	}
	if s.Beetle != nil || filepath.Base(s.EvidenceRoot) != "evidence" {
		t.Fatalf("Beetle 주소가 없으면 Beetle 은 꺼지고, 증적은 기본 폴더여야 한다: %v %s", s.Beetle, s.EvidenceRoot)
	}
	if s.Timeouts["start_min"] < 60 || s.Pacing["start_poll_sec"] != 60 {
		t.Fatal(s.Timeouts, s.Pacing)
	}
}

func TestSettingsExampleNeedsEnv(t *testing.T) {
	for _, k := range []string{"GMP_TB_URL", "GMP_TB_USER", "GMP_TB_PASSWORD"} {
		t.Setenv(k, "")
	}
	_, err := LoadSettings(filepath.Join(root(t), "settings.example.yaml"))
	if err == nil || !strings.Contains(err.Error(), "GMP_TB_PASSWORD, GMP_TB_URL, GMP_TB_USER") {
		t.Fatalf("빈 환경변수를 이름으로 알리지 않았다: %v", err)
	}
}

func TestSettingsNeedAccount(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.yaml")
	if err := os.WriteFile(p, []byte("tumblebugs:\n  default:\n    url: http://tb/tumblebug\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSettings(p); err == nil || !strings.Contains(err.Error(), "user · password") {
		t.Fatalf("계정 없이 통과했다: %v", err)
	}
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("GMP_T_A", "x")
	t.Setenv("GMP_T_EMPTY", "")
	m := map[string]any{"a": "${GMP_T_A}/y", "b": []any{"${GMP_T_EMPTY:-def}", 3}, "c": map[string]any{"d": "$HOME ${GMP_T_A}"}}
	if err := ExpandEnv(m, "x.yaml"); err != nil {
		t.Fatal(err)
	}
	if m["a"] != "x/y" || m["b"].([]any)[0] != "def" || m["c"].(map[string]any)["d"] != "$HOME x" {
		t.Fatal(m)
	}
	err := ExpandEnv(map[string]any{"a": "${GMP_T_NONE_2}", "b": "${GMP_T_NONE_1}"}, "x.yaml")
	if err == nil || !strings.Contains(err.Error(), "GMP_T_NONE_1, GMP_T_NONE_2") {
		t.Fatalf("%v", err)
	}
}

func TestLoadDotEnv(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	body := "# 주석\nGMP_T_NEW=\"a b\"\nexport GMP_T_EXP='c'\nGMP_T_SET=file\n\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GMP_T_SET", "shell")
	t.Setenv("GMP_T_NEW", "")
	os.Unsetenv("GMP_T_NEW")
	t.Setenv("GMP_T_EXP", "")
	os.Unsetenv("GMP_T_EXP")
	if err := LoadDotEnv(p); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("GMP_T_NEW") != "a b" || os.Getenv("GMP_T_EXP") != "c" || os.Getenv("GMP_T_SET") != "shell" {
		t.Fatalf("%q %q %q", os.Getenv("GMP_T_NEW"), os.Getenv("GMP_T_EXP"), os.Getenv("GMP_T_SET"))
	}
	if err := LoadDotEnv(filepath.Join(t.TempDir(), "없음")); err != nil {
		t.Fatal("없는 파일은 넘어가야 한다", err)
	}
	if err := os.WriteFile(p, []byte("not a line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := LoadDotEnv(p); err == nil {
		t.Fatal("잘못된 줄을 넘겼다")
	}
}

// WidgetRAG Shell 패키지는 실행 파일 층 · 기준선 · 골든이 다 있어야 한다 — 하나라도 아니면 package 단계가 경고한다.
func TestWidgetragShellPackageExpect(t *testing.T) {
	ad, err := LoadAdapter("widgetrag-a")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"APP_STATUS": "included", "BASELINE_STATUS": "ok", "GOLDEN_STATUS": "ok"}
	got := ad.PackageExpect()
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("package_expect %s = %q, want %q (전체 %v)", k, got[k], v, got)
		}
	}
}

// TestAdapterCommandsParse 는 어댑터의 명령이 bash 문법으로 읽히는지 본다 (실행은 하지 않는다).
// entry · backup · restart · rebuild 는 템플릿이라 값을 채운 뒤 본다 — {이름} 을 값으로 바꾸므로 셸의 { … } 는 {{ … }} 로
// 적어야 하고, 빠뜨리면 여기서 걸린다. source_restore · existing_check · diagnostics 는 그대로 보낸다.
func TestAdapterCommandsParse(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash 없음")
	}
	files, _ := filepath.Glob(filepath.Join(root(t), "adapters", "*.yaml"))
	vals := map[string]string{"env": "A=1 B=2", "source_env": "LLM_TEMPERATURE=0 LLM_SEED=42", "package_dir": "~/pkg",
		"out_dir": "~/out/1", "golden": "1", "ts": "20261028-120000", "repo_raw": "https://example.invalid/r", "branch": "b"}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".yaml")
		ad, err := LoadAdapter(name)
		if err != nil {
			t.Fatal(err)
		}
		cmds, raw := map[string]string{}, map[string]bool{}
		for _, k := range []string{"entry", "backup", "restart", "rebuild", "source_restore", "existing_check"} {
			if s, ok := ad.Raw[k].(string); ok && s != "" {
				cmds[k] = s
				raw[k] = k == "source_restore" || k == "existing_check"
			}
		}
		for i, d := range jsonx.ListM(ad.Raw["diagnostics"]) {
			k := fmt.Sprintf("diagnostics[%d]", i)
			cmds[k], raw[k] = jsonx.S(d, "command"), true
		}
		for k, tmpl := range cmds {
			cmd := tmpl
			if !raw[k] {
				var err error
				if cmd, err = Render(tmpl, vals); err != nil {
					t.Errorf("%s %s: %v", name, k, err)
					continue
				}
			}
			if out, err := exec.Command("bash", "-n", "-c", cmd).CombinedOutput(); err != nil {
				t.Errorf("%s %s 가 bash 문법이 아니다: %v %s\n%s", name, k, err, out, cmd)
			}
		}
	}
}
