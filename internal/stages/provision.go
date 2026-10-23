package stages

// 3. provision — 타겟 VM 을 만들고(없으면) 원격 명령이 닿을 때까지 기다린 뒤 노드 정보를 보관한다.
//
// 재실행 안정성: 인프라 이름을 먼저 state 에 적고, 같은 이름이 이미 있으면 만들지 않고 조회로 이어 간다 (infraDynamic 은 자동 재시도 금지).
// 개인키는 여기서 받아 두지 않는다 — 필요한 단계가 그때 Tumblebug 에서 받는다.

import (
	"fmt"
	"strings"
	"time"

	"gpu-migrate-orchestrator/internal/jsonx"
	"gpu-migrate-orchestrator/internal/sourcemodel"
	"gpu-migrate-orchestrator/internal/tumblebug"
)

const stProvision = "provision"

func init() { register(stProvision, Stage{Preflight: preProvision, Run: runProvision}) }

func preProvision(ctx *Context) error {
	_, err := ctx.ReadJSON(stRecommend, "migrate-request.json")
	return err
}

func ruleKey(port, proto, dir, cidr string) string {
	return sourcemodel.NormalizePort(port) + "|" + strings.ToLower(proto) + "|" + strings.ToLower(dir) + "|" + strings.TrimSpace(cidr)
}

// MissingRules 는 사례의 열 포트 가운데 보안 그룹에 아직 없는 인바운드 규칙만 (POST 형식 {Ports, Protocol, Direction, CIDR}).
// Beetle 경로는 소스 방화벽 표(열 포트 포함)로 Beetle 이 이미 규칙을 만들었으므로 보통 빈 목록이다 —
// 같은 규칙을 다시 넣으면 CSP 가 중복으로 거부할 수 있다.
func MissingRules(existing []M, ports []M) []M {
	have := map[string]bool{}
	for _, r := range existing {
		have[ruleKey(sourcemodel.RulePort(r), jsonx.S(r, "Protocol"), jsonx.S(r, "Direction"), jsonx.S(r, "CIDR"))] = true
	}
	out := []M{}
	for _, p := range ports {
		rule := M{"Ports": jsonx.Str(p["port"]), "Protocol": jsonx.SOr(p, "protocol", "tcp"), "Direction": "inbound", "CIDR": jsonx.S(p, "cidr")}
		k := ruleKey(jsonx.S(rule, "Ports"), jsonx.S(rule, "Protocol"), "inbound", jsonx.S(rule, "CIDR"))
		if !have[k] {
			have[k] = true
			out = append(out, rule)
		}
	}
	return out
}

func openPorts(ctx *Context, tb Tumblebug, ns string, nodes []M) error {
	ports := ctx.Case.OpenPorts()
	if len(ports) == 0 || ctx.State.HasKey("sg_rules_added") {
		return nil
	}
	sgIDs := jsonx.Strings(nodes[0]["securityGroupIds"])
	if len(sgIDs) == 0 {
		ctx.Log.Warn(stProvision, "노드의 보안 그룹 ID 를 찾지 못해 포트 규칙을 더하지 못했습니다")
		return nil
	}
	sg, err := tb.GetSecurityGroup(ns, sgIDs[0])
	if err != nil {
		return err
	}
	rules := MissingRules(jsonx.ListM(jsonx.AsM(sg)["firewallRules"]), ports)
	if len(rules) == 0 {
		ctx.Log.Info(stProvision, "보안 그룹 %s 에 열 포트 규칙이 이미 있습니다 — 추가하지 않음", sgIDs[0])
	} else {
		if _, err := tb.AddSGRules(ns, sgIDs[0], rules); err != nil {
			return err
		}
		var desc []string
		for _, r := range rules {
			desc = append(desc, fmt.Sprintf("%s/%s from %s", jsonx.S(r, "Ports"), jsonx.S(r, "Protocol"), jsonx.S(r, "CIDR")))
		}
		ctx.Log.Info(stProvision, "보안 그룹 %s 에 규칙 추가: %s", sgIDs[0], strings.Join(desc, ", "))
	}
	ctx.State.SetKey("sg_rules_added", M{"sg": sgIDs[0], "rules": rules})
	return nil
}

func runProvision(ctx *Context) (M, error) {
	tgt := ctx.Case.Target()
	ns, provider, name := jsonx.S(tgt, "ns"), jsonx.S(tgt, "provider"), jsonx.S(tgt, "name")
	reqAny, err := ctx.ReadJSON(stRecommend, "migrate-request.json")
	if err != nil {
		return nil, err
	}
	req := jsonx.AsM(reqAny)
	if ctx.DryRun {
		what := "Tumblebug POST /infraDynamic"
		if provider != "tumblebug" {
			what = fmt.Sprintf("Beetle POST /migration/ns/%s/infra?nameSeed=%s&useExisting=false", ns, NameSeed(name, ctx.State.Data.RunID))
		}
		ctx.Dry(stProvision, what, req)
		ctx.SetNodes("target", []M{{"nodeId": "g1-1", "publicIP": "<target.publicIP>", "sshPort": 22, "nodeUserName": "cb-user"}}, "")
		if _, err := ctx.WriteJSON(stProvision, "target.json", M{"dry_run": true, "infra": name}); err != nil {
			return nil, err
		}
		return M{"dry_run": true}, nil
	}
	tb, err := ctx.TBTarget()
	if err != nil {
		return nil, err
	}
	id := ctx.State.KeyS("target_infra_id")
	var infra M
	if id != "" {
		if infra, err = tb.GetInfra(ns, id); err != nil {
			return nil, err
		}
	}
	if infra == nil && provider == "tumblebug" {
		id = name
		ctx.State.SetKey("target_infra_id", id) // 부작용 전에 기록
		if infra, err = tb.GetInfra(ns, id); err != nil {
			return nil, err
		}
		if infra != nil {
			if other := jsonx.S(jsonx.AsM(infra["label"]), "gmp.case"); other != "" && other != ctx.Case.Name {
				return nil, Errf("인프라 %s 가 이미 있고 다른 사례(%s)의 것입니다 — target.name 을 바꾸세요", id, other)
			}
			ctx.Log.Warn(stProvision, "인프라 %s 가 이미 있어 만들지 않고 이어 갑니다 (상태 %s)", id, tumblebug.InfraState(infra))
		} else {
			ng := jsonx.ListM(req["nodeGroups"])
			ctx.Log.Info(stProvision, "인프라 생성 요청: %s — %s · %s (동기, 수 분)", id, jsonx.S(ng[0], "specId"), jsonx.S(ng[0], "imageId"))
			created, err := tb.InfraDynamic(ns, req, time.Duration(ctx.Settings.TimeoutMin("provision_min"))*time.Minute)
			if err != nil {
				return nil, err
			}
			ctx.Log.Info(stProvision, "생성 응답: %s", jsonx.S(created, "status"))
			if ce := created["creationErrors"]; jsonx.Truthy(ce) {
				b, _ := jsonx.Marshal(ce, false)
				return nil, Errf("생성 오류: %s", headStr(string(b), 300))
			}
		}
	} else if infra == nil {
		b, err := ctx.Beetle()
		if err != nil {
			return nil, err
		}
		seed := ctx.State.KeyS("beetle_name_seed")
		if ctx.State.HasKey("beetle_migrate_started") {
			// 앞선 생성 요청의 응답을 못 받고 멈췄다 — run 마다 고유한 nameSeed 로 Beetle 이 만든 인프라를 찾아 이어 간다
			found, err := findSeeded(b, ns, seed)
			if err != nil {
				return nil, err
			}
			if found == "" {
				return nil, Errf("Beetle 생성을 시작했지만 nameSeed %s 로 만든 인프라가 없습니다 — Beetle UI 에서 ID 를 찾아 state.json 의 keys.target_infra_id 에 적고 다시 돌리세요", seed)
			}
			id = found
			ctx.State.SetKey("target_infra_id", id)
			ctx.Log.Warn(stProvision, "앞선 Beetle 생성 요청이 만든 인프라 %s 를 찾아 이어 갑니다", id)
		} else {
			if seed == "" {
				seed = NameSeed(name, ctx.State.Data.RunID)
				ctx.State.SetKey("beetle_name_seed", seed) // 부작용 전에 기록
			}
			ctx.State.SetKey("beetle_migrate_started", true)
			// ★ useExisting=false — 기본값 true 는 같은 이름의 vNet · SSH 키 · 보안 그룹을 재사용해, 다른 run(사례)의
			//   자원을 끌어다 쓰고 정리 때 함께 지울 수 있다. nameSeed 도 run 마다 달라 이름이 겹치지 않는다.
			ctx.Log.Info(stProvision, "Beetle 생성 요청: nameSeed %s · useExisting=false (동기, 수 분)", seed)
			data, err := b.MigrateInfra(ns, req, seed, false, 30*time.Minute)
			if err != nil {
				return nil, err
			}
			id = jsonx.Str(jsonx.Or(data["id"], data["name"]))
			if id == "" {
				raw, _ := jsonx.Marshal(data, false)
				return nil, Errf("Beetle 생성 응답에 인프라 ID 가 없습니다: %s", headStr(string(raw), 300))
			}
			ctx.State.SetKey("target_infra_id", id)
			ctx.Log.Info(stProvision, "Beetle 가 만든 인프라: %s (%s)", id, jsonx.S(data, "status"))
		}
	}

	infra, err = tb.WaitInfra(ns, id, []string{"Running"}, 15*time.Minute, 20*time.Second, TickLogger(ctx.Log, stProvision, 4))
	if err != nil {
		return nil, err
	}
	nodes := tumblebug.Nodes(infra)
	access, err := tb.AccessInfo(ns, id)
	if err != nil {
		return nil, err
	}
	for _, a := range access { // SG · spec 정보는 infra 쪽에만 있다
		for _, n := range nodes {
			if jsonx.S(n, "nodeId") == jsonx.S(a, "nodeId") {
				for _, k := range []string{"securityGroupIds", "specId", "imageId", "cspResourceId", "status"} {
					a[k] = n[k]
				}
			}
		}
	}
	ctx.SaveAccess("target", access)
	node := access[0]
	ctx.Log.Info(stProvision, "타겟 노드 %s %s (%s) · %s", jsonx.S(node, "nodeId"), jsonx.S(node, "publicIP"), jsonx.S(node, "nodeUserName"), jsonx.S(node, "cspResourceId"))
	ctx.Log.Info(stProvision, "원격 명령이 닿을 때까지 대기 (cloud-init 포함, 최대 10분)")
	if err := ctx.WaitReachable("target", 10*time.Minute, time.Duration(ctx.Settings.Pacing["ready_poll_sec"])*time.Second); err != nil {
		return nil, err
	}
	if err := openPorts(ctx, tb, ns, access); err != nil {
		return nil, err
	}
	clean := []any{}
	for _, n := range access {
		m := M{}
		for k, v := range n {
			if k != "privateKey" {
				m[k] = v
			}
		}
		clean = append(clean, m)
	}
	if _, err := ctx.WriteJSON(stProvision, "target.json", M{"infra": id, "status": infra["status"], "nodes": clean}); err != nil {
		return nil, err
	}
	return M{"infra": id, "ip": node["publicIP"]}, nil
}

// NameSeed 는 Beetle nameSeed 를 run 마다 고유하게 만든다 — 사례의 target.name 뒤에 run 시각(월일시분)을 붙인다.
// Beetle 규칙: 20자 이하 · 영숫자로 시작 · 영숫자와 '-' 만.
func NameSeed(name, runID string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	base := strings.Trim(b.String(), "-")
	if base == "" {
		base = "gmp"
	}
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, runID)
	suffix := digits
	if len(digits) >= 12 {
		suffix = digits[4:12] // 20261023-101530 → 10231015
	}
	if max := 20 - 1 - len(suffix); len(base) > max {
		base = strings.TrimRight(base[:max], "-")
	}
	return base + "-" + suffix
}

// findSeeded 는 Beetle 이 nameSeed 로 만든 인프라 ID 를 찾는다 (Beetle 은 'seed' → 'seed-infra101' 처럼 앞에 붙인다).
func findSeeded(b Beetle, ns, seed string) (string, error) {
	if seed == "" {
		return "", nil
	}
	ids, err := b.ListInfraIDs(ns)
	if err != nil {
		return "", err
	}
	var hits []string
	for _, id := range ids {
		if id == seed || strings.HasPrefix(id, seed+"-") {
			hits = append(hits, id)
		}
	}
	if len(hits) > 1 {
		return "", Errf("nameSeed %s 로 시작하는 인프라가 여럿입니다 (%s) — state.json 의 keys.target_infra_id 에 직접 적으세요", seed, strings.Join(hits, ", "))
	}
	if len(hits) == 1 {
		return hits[0], nil
	}
	return "", nil
}

func headStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
