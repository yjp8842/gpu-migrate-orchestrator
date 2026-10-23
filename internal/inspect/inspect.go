// Package inspect 는 소스 VM 안의 상태 확인이다 — Tumblebug 메타가 모르는 것만 읽는다.
//
//   - 서비스 · LLM 설정이 지금 어떤가 (골든 응답의 전제: temperature · seed)
//   - 식별 대조표의 소스 쪽: 유닛 · 환경변수 키(값 제외) · 설정 · 커밋 · 패키지 버전 · 이미지 태그
//   - 실제 GPU · 드라이버 (메타의 주문 사양과 대조용 증적)
//
// 패키지 생성 단계가 소스에 원격 명령을 보낼 때 한 번 같이 읽는다 (각 항목을 command 원소 하나씩 — 출력이 순서로 맞는다).
package inspect

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"gpu-migrate-orchestrator/internal/config"
	"gpu-migrate-orchestrator/internal/jsonx"
	"gpu-migrate-orchestrator/internal/textx"
)

// Cmd 는 이름 붙은 원격 명령 하나다.
type Cmd struct{ Name, Command string }

// Commands 는 어댑터의 식별 위치로 상태 확인 명령을 만든다.
func Commands(ad *config.Adapter) []Cmd {
	ident := ad.M("identify")
	tree := jsonx.SOr(ident, "tree", "~")
	var units []string
	for _, u := range jsonx.Strings(ident["units"]) {
		units = append(units, fmt.Sprintf("systemctl show %s -p Id,FragmentPath,ExecStart,User,EnvironmentFile,ActiveState,UnitFileState 2>/dev/null; echo ---", u))
	}
	unitCmd := strings.Join(units, " ; ")
	if unitCmd == "" {
		unitCmd = "echo no-units"
	}
	var composes []string
	for _, f := range jsonx.Strings(ident["compose"]) {
		composes = append(composes, fmt.Sprintf("grep -E 'image:' %s 2>/dev/null", f))
	}
	composeCmd := strings.Join(composes, " ; ")
	if composeCmd == "" {
		composeCmd = "echo"
	}
	envCmd, cfgCmd := "echo", "echo"
	if f := jsonx.S(ident, "env_file"); f != "" {
		envCmd = fmt.Sprintf("sudo grep -oE '^[A-Z_][A-Z0-9_]*=' %s 2>/dev/null | sort || true", f)
	}
	if f := jsonx.S(ident, "config"); f != "" {
		cfgCmd = fmt.Sprintf("cat %s 2>/dev/null || true", f)
	}
	healthCmd := "echo '{}'" // health 주소가 없는 워크로드
	if health := ad.S("health"); health != "" {
		healthCmd = fmt.Sprintf("curl -s --max-time 5 %s || echo '{}'", health)
	}
	// 버전을 남길 패키지(dpkg 이름)와 버전 명령은 워크로드마다 다르다 — identify.packages · identify.versions.
	// GPU 드라이버 패키지는 공통으로 남긴다.
	pkgCmd := `dpkg-query -W -f='${Package}=${Version}\n' 'nvidia-driver-*' 2>/dev/null | grep -v '=$'`
	if pk := jsonx.Strings(ident["packages"]); len(pk) > 0 {
		pkgCmd = fmt.Sprintf(`dpkg-query -W -f='${Package}=${Version}\n' %s 2>/dev/null; `, strings.Join(pk, " ")) + pkgCmd
	}
	for _, v := range jsonx.Strings(ident["versions"]) {
		pkgCmd += "; " + v + " 2>/dev/null | tail -1"
	}
	codeRef := jsonx.S(ident, "code_ref")
	if codeRef == "" {
		codeRef = fmt.Sprintf("git -C %s rev-parse --short HEAD 2>/dev/null || echo unknown", tree)
	}
	return []Cmd{
		{"gpu", "nvidia-smi --query-gpu=index,uuid,name,driver_version,pci.bus_id,memory.total --format=csv,noheader,nounits 2>/dev/null || echo NOGPU"},
		{"cuda", "nvidia-smi 2>/dev/null | grep -o 'CUDA Version: [0-9.]*' | awk 'NR==1' || true"},
		{"os", "cat /etc/os-release"},
		{"kernel", "uname -r"},
		{"pkgs", pkgCmd},
		{"units", unitCmd},
		{"env_keys", envCmd},
		{"config", cfgCmd},
		{"code_ref", codeRef},
		{"compose", composeCmd},
		{"health", healthCmd},
	}
}

var cudaRe = regexp.MustCompile(`CUDA Version: ([0-9.]+)`)

func parseGPU(text, cuda string) []any {
	cards := []any{}
	if strings.TrimSpace(text) == "" || strings.Contains(text, "NOGPU") {
		return cards
	}
	cv := ""
	if m := cudaRe.FindStringSubmatch(cuda); m != nil {
		cv = m[1]
	}
	for _, line := range strings.Split(text, "\n") {
		parts := strings.Split(line, ",")
		if len(parts) < 6 {
			continue
		}
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		var mem any = parts[5]
		if textx.IsDigits(parts[5]) {
			mem = textx.MibToGib(parts[5])
		}
		cards = append(cards, jsonx.M{"index": parts[0], "uuid": parts[1], "model": parts[2], "driverVersion": parts[3],
			"cudaVersion": cv, "pciBusId": parts[4], "memoryTotalGB": mem})
	}
	return cards
}

func parseUnits(text string) []any {
	units := []any{}
	for _, block := range strings.Split(text, "---") {
		kv := textx.ParseKV(block)
		if kv["Id"] == "" {
			continue
		}
		units = append(units, jsonx.M{"unit": kv["Id"], "fragmentPath": kv["FragmentPath"], "execStart": kv["ExecStart"],
			"user": kv["User"], "environmentFile": kv["EnvironmentFile"], "activeState": kv["ActiveState"], "unitFileState": kv["UnitFileState"]})
	}
	return units
}

// Identify 는 원격 명령 출력(이름 → stdout)을 식별 결과로 바꾼다.
func Identify(ad *config.Adapter, out map[string]string) jsonx.M {
	osr := textx.ParseKV(out["os"])
	pkgs := []any{}
	for _, l := range strings.Split(out["pkgs"], "\n") {
		if strings.TrimSpace(l) != "" {
			pkgs = append(pkgs, l)
		}
	}
	keys := []any{}
	for _, l := range strings.Fields(out["env_keys"]) {
		keys = append(keys, strings.TrimRight(l, "="))
	}
	images := []any{}
	for _, l := range strings.Split(out["compose"], "\n") {
		if strings.Contains(l, "image:") {
			images = append(images, strings.TrimSpace(l))
		}
	}
	health := jsonx.AsM(textx.ParseJSON(out["health"]))
	if health == nil {
		health = jsonx.M{}
	}
	return jsonx.M{
		"form": ad.S("form"), "workload": ad.S("workload"),
		"gpu": parseGPU(out["gpu"], out["cuda"]),
		"os":  osr["PRETTY_NAME"], "kernel": strings.TrimSpace(out["kernel"]),
		"packages": pkgs, "units": parseUnits(out["units"]), "env_keys": keys, "config": out["config"],
		"code_ref":       strings.ReplaceAll(strings.TrimSpace(out["code_ref"]), "APP_CODE_REF=", ""),
		"compose_images": images, "services": ad.Raw["services"], "health": health,
	}
}

func sameNum(a, b string) bool {
	fa, e1 := strconv.ParseFloat(strings.TrimSpace(a), 64)
	fb, e2 := strconv.ParseFloat(strings.TrimSpace(b), 64)
	if e1 == nil && e2 == nil {
		return fa == fb
	}
	return a == b
}

// LLMMismatch 는 (불일치?, 설명). 사례의 LLM_TEMPERATURE · LLM_SEED 와 소스의 /health 값을 비교한다.
func LLMMismatch(health jsonx.M, wantTemp, wantSeed string) (bool, string) {
	return LLMMismatchFields(health, "llm_temperature", "llm_seed", wantTemp, wantSeed)
}

// LLMMismatchFields 는 health 응답의 필드 이름을 어댑터에서 받는 판이다 (워크로드마다 이름이 다르다).
func LLMMismatchFields(health jsonx.M, tempField, seedField, wantTemp, wantSeed string) (bool, string) {
	temp, seed := jsonx.S(health, tempField), jsonx.S(health, seedField)
	bad := (wantTemp != "" && !sameNum(temp, wantTemp)) || (wantSeed != "" && seed != wantSeed)
	or := func(s, d string) string {
		if s == "" {
			return d
		}
		return s
	}
	return bad, fmt.Sprintf("소스 temperature %s · seed %s / 사례 %s · %s", or(temp, "?"), or(seed, "없음"), or(wantTemp, "-"), or(wantSeed, "-"))
}
