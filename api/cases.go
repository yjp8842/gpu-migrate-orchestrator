package api

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// 화면에서 고른 값으로 사례 파일(YAML)을 만든다 — cases/case.sample.yaml 과 같은 항목 · 같은 기본값.
// 워크로드마다 다른 것(열 포트 · run 블록)은 workloads 에 둔다.

type runDoc struct {
	Env                 map[string]string `yaml:"env"`
	Golden              *bool             `yaml:"golden,omitempty"`
	RebuildBeforeBackup *bool             `yaml:"rebuild_before_backup,omitempty"`
	TimeoutStartMin     int               `yaml:"timeout_start_min"`
	OnExisting          string            `yaml:"on_existing"`
}

// Workload 는 화면의 워크로드 카드 하나다.
type Workload struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Desc  string `json:"desc"`
	Port  int    `json:"port"`
	Ready bool   `json:"ready"` // adapters/<id>.yaml 이 있는지
	run   runDoc
}

func yes() *bool { b := true; return &b }

// 검증용 관리자 비밀번호는 사례에 값을 쓰지 않고 실행 때 .env 에서 채운다.
var workloads = []Workload{
	{ID: "widgetrag-a", Label: "WidgetRAG Shell", Desc: "네이티브 설치", Port: 80, run: runDoc{
		Env:    map[string]string{"LLM_TEMPERATURE": "0", "LLM_SEED": "42", "COMPARE": "1", "CODE_SOURCE": "package", "VERIFY_ADMIN_PASSWORD": "${GMP_VERIFY_ADMIN_PASSWORD}"},
		Golden: yes(), RebuildBeforeBackup: yes(), TimeoutStartMin: 75, OnExisting: "stop"}},
	{ID: "widgetrag-b", Label: "WidgetRAG Compose", Desc: "Docker Compose", Port: 80, run: runDoc{
		Env:    map[string]string{"LLM_TEMPERATURE": "0", "LLM_SEED": "42", "COMPARE": "1", "VERIFY_ADMIN_PASSWORD": "${GMP_VERIFY_ADMIN_PASSWORD}"},
		Golden: yes(), RebuildBeforeBackup: yes(), TimeoutStartMin: 75, OnExisting: "stop"}},
	{ID: "lexai-a", Label: "LexAI 네이티브", Desc: "네이티브 설치", Port: 3000, run: runDoc{Env: map[string]string{}, TimeoutStartMin: 75, OnExisting: "stop"}},
	{ID: "lexai-b", Label: "LexAI Compose", Desc: "Docker Compose", Port: 80, run: runDoc{Env: map[string]string{}, TimeoutStartMin: 75, OnExisting: "stop"}},
}

func findWorkload(id string) (Workload, bool) {
	for _, w := range workloads {
		if w.ID == id {
			return w, true
		}
	}
	return Workload{}, false
}

func listWorkloads(root string) []Workload {
	out := make([]Workload, 0, len(workloads))
	for _, w := range workloads {
		_, err := os.Stat(filepath.Join(root, "adapters", w.ID+".yaml"))
		w.Ready = err == nil
		out = append(out, w)
	}
	return out
}

// caseDoc 은 사례 파일 — 필드 순서가 곧 YAML 순서다.
type caseDoc struct {
	Case     string  `yaml:"case"`
	Workload string  `yaml:"workload"`
	Source   srcDoc  `yaml:"source"`
	Target   tgtDoc  `yaml:"target"`
	Policy   polDoc  `yaml:"policy"`
	Transfer xferDoc `yaml:"transfer"`
	Run      runDoc  `yaml:"run"`
	Cleanup  string  `yaml:"cleanup"`
}

type srcDoc struct {
	Tumblebug string `yaml:"tumblebug"`
	NS        string `yaml:"ns"`
	Infra     string `yaml:"infra"`
}

type tgtDoc struct {
	Provider  string  `yaml:"provider"`
	Tumblebug string  `yaml:"tumblebug"`
	CSP       string  `yaml:"csp"`
	Region    string  `yaml:"region"`
	Zone      *string `yaml:"zone"`
	NS        string  `yaml:"ns"`
	Name      string  `yaml:"name"`
	Spec      *string `yaml:"spec"`
	Image     *string `yaml:"image"`
}

type portDoc struct {
	Port int    `yaml:"port"`
	CIDR string `yaml:"cidr"`
}

type polDoc struct {
	GPU          string    `yaml:"gpu"`
	Image        string    `yaml:"image"`
	DriverSeries string    `yaml:"driver_series"`
	RootDiskGB   int       `yaml:"root_disk_gb"`
	OpenPorts    []portDoc `yaml:"open_ports"`
	OnPerfFail   string    `yaml:"on_perf_fail"`
}

type xferDoc struct {
	Method   string `yaml:"method"`
	Strategy string `yaml:"strategy"`
}

// caseInput 은 화면이 보내는 값이다.
type caseInput struct {
	Workload    string `json:"workload"`
	NS          string `json:"ns"` // Tumblebug 네임스페이스 — 소스 · 타겟 모두 (비면 default)
	SourceInfra string `json:"sourceInfra"`
	SourceCSP   string `json:"sourceCsp"`
	CSP         string `json:"csp"`
	Region      string `json:"region"`
	Zone        string `json:"zone"`
	Name        string `json:"name"`
	Policy      struct {
		GPU        string   `json:"gpu"`
		Image      string   `json:"image"`
		RootDiskGB *float64 `json:"root_disk_gb"`
		Strategy   string   `json:"strategy"`
		Cleanup    string   `json:"cleanup"`
	} `json:"policy"`
}

var (
	caseNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	nsRe       = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`) // Tumblebug 네임스페이스 ID
	idRe       = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	cspRe      = regexp.MustCompile(`^[a-z0-9]{2,16}$`)
	imageRe    = regexp.MustCompile(`^(source|keep|ubuntu-\d{2}\.\d{2})$`)
)

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func oneOf(v string, allowed ...string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

// buildCase 는 사례와 이름을 만든다. 입력 검사도 여기서 한다 — 값은 사례 파일을 거쳐 VM 원격 명령까지 간다.
func buildCase(in caseInput, now time.Time) (string, caseDoc, error) {
	wl, ok := findWorkload(in.Workload)
	if !ok {
		return "", caseDoc{}, fmt.Errorf("모르는 워크로드: %s", in.Workload)
	}
	for k, v := range map[string]string{"sourceInfra": in.SourceInfra, "sourceCsp": in.SourceCSP, "csp": in.CSP, "region": in.Region, "name": in.Name} {
		if v == "" {
			return "", caseDoc{}, fmt.Errorf("%s 가 비어 있습니다", k)
		}
	}
	ns := or(in.NS, "default")
	switch {
	case !nsRe.MatchString(ns):
		return "", caseDoc{}, fmt.Errorf("네임스페이스 이름이 이상합니다")
	case !caseNameRe.MatchString(in.Name):
		return "", caseDoc{}, fmt.Errorf("타겟 이름은 영소문자 · 숫자 · - 만 (63자 이하)")
	case !idRe.MatchString(in.SourceInfra):
		return "", caseDoc{}, fmt.Errorf("소스 인프라 이름이 이상합니다")
	case !cspRe.MatchString(in.SourceCSP) || !cspRe.MatchString(in.CSP):
		return "", caseDoc{}, fmt.Errorf("CSP 이름이 이상합니다")
	case !idRe.MatchString(in.Region):
		return "", caseDoc{}, fmt.Errorf("리전 이름이 이상합니다")
	case in.Zone != "" && !idRe.MatchString(in.Zone):
		return "", caseDoc{}, fmt.Errorf("존 이름이 이상합니다")
	}
	p := in.Policy
	gpu, image := or(p.GPU, "same-model-first"), or(p.Image, "source")
	strategy, cleanup := or(p.Strategy, "direct"), or(p.Cleanup, "suspend")
	disk := 100.0
	if p.RootDiskGB != nil {
		disk = *p.RootDiskGB
	}
	switch {
	case !oneOf(gpu, "same-model-first", "vram-at-least"):
		return "", caseDoc{}, fmt.Errorf("GPU 정책은 same-model-first · vram-at-least")
	case !imageRe.MatchString(image):
		return "", caseDoc{}, fmt.Errorf("이미지 정책은 source · keep · ubuntu-XX.XX")
	case !oneOf(cleanup, "suspend", "terminate", "keep"):
		return "", caseDoc{}, fmt.Errorf("정리는 suspend · terminate · keep")
	case !oneOf(strategy, "direct", "relay"):
		return "", caseDoc{}, fmt.Errorf("이관 방식은 direct · relay")
	case disk != float64(int(disk)) || disk < 30 || disk > 2000:
		return "", caseDoc{}, fmt.Errorf("루트 디스크는 30~2000GB 정수")
	}
	name := strings.ToLower(fmt.Sprintf("%s-to-%s-%s-%s", in.SourceCSP, in.CSP, in.Workload, now.Format("01021504")))
	var zone *string
	if in.Zone != "" {
		z := in.Zone
		zone = &z
	}
	doc := caseDoc{
		Case: name, Workload: in.Workload,
		Source: srcDoc{Tumblebug: "default", NS: ns, Infra: in.SourceInfra},
		Target: tgtDoc{Provider: "beetle", Tumblebug: "default", CSP: in.CSP, Region: strings.ToLower(in.Region), Zone: zone, NS: ns, Name: in.Name},
		Policy: polDoc{GPU: gpu, Image: image, DriverSeries: "", RootDiskGB: int(disk),
			OpenPorts: []portDoc{{Port: wl.Port, CIDR: "${GMP_WEB_ALLOW_CIDR}"}}, OnPerfFail: "stop"},
		Transfer: xferDoc{Method: "beetle", Strategy: strategy},
		Run:      wl.run,
		Cleanup:  cleanup,
	}
	return name, doc, nil
}

const caseHead = "# GPU 이관 콘솔이 만든 사례 파일 — 화면에서 고른 값. 항목 설명은 cases/case.sample.yaml\n"

// caseYAML 은 저장소의 사례 파일처럼 두 칸 들여쓰기로 쓴다.
func caseYAML(doc caseDoc) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(caseHead)
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func consoleCases(root string) string { return filepath.Join(root, "cases", "console") }

// writeCase 는 cases/console/<이름>.yaml 을 쓰고 저장소 기준 상대 경로를 돌려준다.
func writeCase(root string, in caseInput) (name, rel string, err error) {
	name, doc, err := buildCase(in, time.Now())
	if err != nil {
		return "", "", err
	}
	b, err := caseYAML(doc)
	if err != nil {
		return "", "", err
	}
	dir := consoleCases(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	file := filepath.Join(dir, name+".yaml")
	if err := os.WriteFile(file, b, 0o644); err != nil {
		return "", "", err
	}
	rel, _ = filepath.Rel(root, file)
	return name, rel, nil
}

// casePlan 은 화면에 보여 줄 고른 값이다 — 사례 파일 원문은 화면으로 내보내지 않는다.
type casePlan struct {
	Workload      string  `json:"workload"`
	WorkloadLabel string  `json:"workloadLabel"`
	NS            string  `json:"ns"`
	Port          int     `json:"port"`
	SourceInfra   string  `json:"sourceInfra"`
	CSP           string  `json:"csp"`
	Region        string  `json:"region"`
	Zone          *string `json:"zone"`
	Cleanup       string  `json:"cleanup"`
}

// readCase 는 콘솔이 만든 사례를 읽는다. 없으면 ok=false.
func readCase(root, name string) (rel string, plan casePlan, ok bool, err error) {
	if !caseNameRe.MatchString(name) {
		return "", casePlan{}, false, fmt.Errorf("사례 이름이 이상합니다")
	}
	file := filepath.Join(consoleCases(root), name+".yaml")
	b, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return "", casePlan{}, false, nil
	}
	if err != nil {
		return "", casePlan{}, false, err
	}
	var doc caseDoc
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return "", casePlan{}, false, err
	}
	wl, _ := findWorkload(doc.Workload)
	plan = casePlan{Workload: doc.Workload, WorkloadLabel: or(wl.Label, doc.Workload), NS: doc.Source.NS, Port: wl.Port, SourceInfra: doc.Source.Infra,
		CSP: doc.Target.CSP, Region: doc.Target.Region, Zone: doc.Target.Zone, Cleanup: doc.Cleanup}
	rel, _ = filepath.Rel(root, file)
	return rel, plan, true, nil
}
