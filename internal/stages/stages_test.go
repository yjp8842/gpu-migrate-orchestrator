package stages

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpu-migrate-orchestrator/internal/beetle"
	"gpu-migrate-orchestrator/internal/config"
	"gpu-migrate-orchestrator/internal/crypto"
	"gpu-migrate-orchestrator/internal/jsonx"
	"gpu-migrate-orchestrator/internal/logx"
	"gpu-migrate-orchestrator/internal/state"
	"gpu-migrate-orchestrator/internal/tumblebug"
)

func load(t *testing.T, name string) M {
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	v, _ := jsonx.Decode(b)
	return jsonx.AsM(v)
}

// fakeTB 는 고른 호출만 답한다 — 나머지를 부르면 nil 인터페이스라 panic 으로 시험이 깨진다.
type fakeTB struct {
	Tumblebug
	meta   M
	infra  M
	calls  []string
	cmds   []string
	cmdOut func(cmd string) string
	sg     M
	added  [][]M
	access []M
	// 비동기 원격 명령 — 보낸 xRequestId 와 xRequestId 별 상태 기록
	async       []string
	asyncStatus string
	cmdStatus   map[string][]M
	// 연결의 기본 존 · 존 목록 (ConnZones)
	assigned string
	zones    []string
}

func (f *fakeTB) ConnZones(conn string) (string, []string, error) { return f.assigned, f.zones, nil }

func (f *fakeTB) GetInfra(ns, id string) (M, error) {
	f.calls = append(f.calls, "infra")
	return f.infra, nil
}
func (f *fakeTB) ResolveSpec(ns, id string) (M, error) {
	f.calls = append(f.calls, "spec")
	return jsonx.AsM(f.meta["spec"]), nil
}
func (f *fakeTB) ResolveImage(ns, id string) (M, error) {
	f.calls = append(f.calls, "image")
	return jsonx.AsM(f.meta["image"]), nil
}
func (f *fakeTB) GetVNet(ns, id string) (M, error) {
	f.calls = append(f.calls, "vnet")
	return jsonx.AsM(f.meta["vnet"]), nil
}
func (f *fakeTB) GetSecurityGroup(ns, id string) (M, error) {
	f.calls = append(f.calls, "sg")
	if f.sg != nil {
		return f.sg, nil
	}
	return jsonx.AsM(f.meta["sg"]), nil
}
func (f *fakeTB) AddSGRules(ns, sg string, rules []M) (any, error) {
	f.added = append(f.added, rules)
	return M{}, nil
}
func (f *fakeTB) AccessInfo(ns, id string) ([]M, error) { return f.access, nil }
func (f *fakeTB) CmdAsync(ns, id string, cmds []string, user string, timeoutMin int, xreq string) (string, error) {
	f.async = append(f.async, xreq)
	st := f.asyncStatus
	if st == "" {
		st = "Completed"
	}
	if f.cmdStatus == nil {
		f.cmdStatus = map[string][]M{}
	}
	f.cmdStatus[xreq] = []M{{"xRequestId": xreq, "status": st, "stdout": "out " + xreq}}
	return xreq, nil
}
func (f *fakeTB) CommandStatus(ns, id, node, xreq string) ([]M, error) { return f.cmdStatus[xreq], nil }

// WaitCommand 는 아직 도는 명령을 Completed 로 끝낸 것으로 본다.
func (f *fakeTB) WaitCommand(ns, id string, nodeIDs []string, xreq string, timeout, poll time.Duration, onTick func(...any)) (map[string]M, error) {
	e := f.cmdStatus[xreq]
	last := jsonx.Clone(e[len(e)-1]).(M)
	if !tumblebug.Terminal[jsonx.S(last, "status")] {
		last["status"] = "Completed"
	}
	return map[string]M{nodeIDs[0]: last}, nil
}
func (f *fakeTB) CmdSync(ns, id string, cmds []string, user string, timeoutMin int) ([]M, error) {
	f.cmds = append(f.cmds, cmds...)
	out := ""
	if f.cmdOut != nil {
		out = f.cmdOut(cmds[0])
	}
	return []M{{"nodeId": "g1-1", "stdout": M{"0": out}, "stderr": M{"0": ""}}}, nil
}

type fakeBeetle struct {
	Beetle
	cands   []any
	called  int
	migrate M
	// 데이터 이관 — 보낸 요청 ID 와 요청별 상태
	pub        string
	sent       []string
	bodies     []M
	sendStatus string
	reqs       map[string]M
	// 생성 — 받은 nameSeed · useExisting, 목록 · 삭제
	seed    string
	use     *bool
	ids     []string
	deleted []string
	// 생성 시도별 오류 (차례로 쓰고, 다 쓰면 b.migrate 를 돌려준다) · 보낸 본문
	migrateErrs []error
	migrateSent []M
	// 생성 가능 사전 검사 — 돌려줄 결과 · 오류, 부른 횟수
	valid     M
	validErr  error
	validated int
}

func (b *fakeBeetle) ValidateInfra(ns string, body M, use bool) (M, error) {
	b.validated++
	if b.validErr != nil {
		return nil, b.validErr
	}
	if b.valid != nil {
		return b.valid, nil
	}
	return M{"valid": true, "issues": []any{}}, nil
}

func (b *fakeBeetle) RecommendInfra(req M, limit int) ([]any, error) { b.called++; return b.cands, nil }
func (b *fakeBeetle) MigrateInfra(ns string, body M, seed string, use bool, to time.Duration) (M, error) {
	b.seed, b.use = seed, &use
	b.migrateSent = append(b.migrateSent, body)
	if n := len(b.migrateSent); n <= len(b.migrateErrs) && b.migrateErrs[n-1] != nil {
		b.ids = append(b.ids, seed+"-gpu-infra-01") // 실패한 시도도 Failed 인프라를 남긴다 (10/23 GCP)
		return nil, b.migrateErrs[n-1]
	}
	return b.migrate, nil
}
func (b *fakeBeetle) ListInfraIDs(ns string) ([]string, error) { return b.ids, nil }
func (b *fakeBeetle) DeleteInfra(ns, id string) (any, error) {
	b.deleted = append(b.deleted, id)
	for i, x := range b.ids {
		if x == id {
			b.ids = append(b.ids[:i], b.ids[i+1:]...)
			break
		}
	}
	return M{}, nil
}
func (b *fakeBeetle) EncryptionKey() (M, error) { return M{"keyId": "k1", "publicKey": b.pub}, nil }
func (b *fakeBeetle) DataMigration(body M, xreq string) (string, error) {
	b.sent = append(b.sent, xreq)
	b.bodies = append(b.bodies, body)
	st := b.sendStatus
	if st == "" {
		st = "success"
	}
	b.reqs[xreq] = M{"status": st}
	return xreq, nil
}
func (b *fakeBeetle) RequestStatus(rid string) (M, error) {
	if st, ok := b.reqs[rid]; ok {
		return st, nil
	}
	return nil, &beetle.Error{Msg: "HTTP 404", Status: 404, Body: `{"message":"request not found"}`}
}

// WaitRequest 는 아직 도는 요청을 성공으로 끝낸 것으로 본다.
func (b *fakeBeetle) WaitRequest(rid string, timeout, poll time.Duration, onTick func(...any)) (M, error) {
	st := jsonx.Clone(b.reqs[rid]).(M)
	if !beetle.Terminal[strings.ToLower(jsonx.S(st, "status"))] {
		st["status"] = "success"
	}
	return st, nil
}

func caseRaw() M {
	return M{"case": "t-case", "workload": "widgetrag-a", "source": M{"ns": "default", "infra": "widgetrag-src-aws"},
		"target": M{"provider": "beetle", "csp": "gcp", "region": "asia-northeast3", "name": "wr-gcp"},
		"policy": M{"open_ports": []any{M{"port": 80, "cidr": "203.0.113.254/32"}}}}
}

func newCtx(t *testing.T, tb Tumblebug, b Beetle, dry bool) *Context {
	t.Helper()
	dir := t.TempDir()
	c, err := config.NewCase(filepath.Join(dir, "x.yaml"), caseRaw(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ad, err := config.LoadAdapter("widgetrag-a")
	if err != nil {
		t.Fatal(err)
	}
	s := &config.Settings{EvidenceRoot: dir, Timeouts: map[string]float64{"provision_min": 30, "package_min": 120, "start_min": 75, "transfer_min": 60},
		Pacing: map[string]float64{"start_poll_sec": 0, "ready_poll_sec": 0}}
	st, err := state.Create(dir, c.Name, "x.yaml", "widgetrag-shell", dry)
	if err != nil {
		t.Fatal(err)
	}
	ctx := &Context{Case: c, Adapter: ad, Settings: s, State: st, Log: logx.Quiet(), DryRun: dry, RunDir: st.RunDir, Sleep: func(time.Duration) {}}
	ctx.NewTumblebug = func(string) (Tumblebug, error) { return tb, nil }
	ctx.NewBeetle = func() (Beetle, error) {
		if b == nil {
			return nil, errors.New("beetle 없음")
		}
		return b, nil
	}
	return ctx
}

func readJSON(t *testing.T, ctx *Context, stage, name string) M {
	v, err := ctx.ReadJSON(stage, name)
	if err != nil {
		t.Fatal(err)
	}
	return jsonx.AsM(v)
}

// ---------- collect ----------

func TestCollectFromMetaWithoutVMCommands(t *testing.T) {
	meta := load(t, "tb-meta-compose.json")
	tb := &fakeTB{meta: meta, infra: jsonx.AsM(meta["infra"])}
	ctx := newCtx(t, tb, nil, false)
	out, err := runCollect(ctx)
	if err != nil || out["gpu"] != "NVIDIA T4" {
		t.Fatal(out, err)
	}
	m := readJSON(t, ctx, "collect", "source-model.json")
	fw := jsonx.ListM(jsonx.ListM(m["nodes"])[0]["firewallTable"])
	if fw[0]["dstPorts"] != "22" || fw[1]["dstPorts"] != "80" {
		t.Fatal(fw)
	}
	if strings.Join(tb.calls, ",") != "infra,spec,image,vnet,sg" || len(tb.cmds) != 0 {
		t.Fatal(tb.calls, tb.cmds)
	}
	if _, ok := readJSON(t, ctx, "collect", "source-meta.json")["securityGroups"]; !ok {
		t.Fatal("source-meta")
	}
}

func TestCollectRunsWhenSourceSuspended(t *testing.T) {
	meta := load(t, "tb-meta-compose.json")
	infra := jsonx.Clone(meta["infra"]).(M)
	infra["status"] = "Suspended:1 (S:1/1)"
	ctx := newCtx(t, &fakeTB{meta: meta, infra: infra}, nil, false)
	if _, err := runCollect(ctx); err != nil {
		t.Fatal(err)
	}
	if ctx.State.KeyS("source_state_at_collect") != "Suspended" {
		t.Fatal(ctx.State.Data.Keys)
	}
}

func TestCollectMissingSource(t *testing.T) {
	if _, err := runCollect(newCtx(t, &fakeTB{}, nil, false)); err == nil {
		t.Fatal("없는 소스를 통과시켰다")
	}
	ctx := newCtx(t, &fakeTB{}, nil, true)
	out, err := runCollect(ctx)
	if err != nil || out["placeholder"] != true || !ctx.State.HasKey("source_placeholder") {
		t.Fatal(out, err)
	}
}

// ---------- recommend ----------

func liveCands(t *testing.T) []any {
	return jsonx.AsList(jsonx.Clone(load(t, "beetle-recommend-live-gcp.json")["data"]))
}

func writeModel(t *testing.T, ctx *Context) {
	model := jsonx.Get(load(t, "beetle-recommend-live-gcp.json"), "request", "onpremiseInfraModel")
	if _, err := ctx.WriteJSON("collect", "source-model.json", model); err != nil {
		t.Fatal(err)
	}
}

func TestRecommendDryRunCallsBeetle(t *testing.T) {
	b := &fakeBeetle{cands: liveCands(t)}
	ctx := newCtx(t, &fakeTB{}, b, true)
	writeModel(t, ctx)
	if _, err := runRecommend(ctx); err != nil {
		t.Fatal(err)
	}
	req := readJSON(t, ctx, "recommend", "migrate-request.json")
	if b.called != 1 || jsonx.S(jsonx.ListM(jsonx.Get(req, "targetInfra", "nodeGroups"))[0], "specId") != "gcp+asia-northeast3+g2-standard-4" {
		t.Fatal(b.called, req)
	}
}

// 사례 target.zone 은 Beetle 생성 요청의 서브넷 zone 으로 들어간다 (Beetle 은 서브넷 존에 VM 을 만든다).
func TestRecommendZonePinsSubnet(t *testing.T) {
	ctx := newCtx(t, &fakeTB{}, &fakeBeetle{cands: liveCands(t)}, true)
	writeModel(t, ctx)
	ctx.Case.Target()["zone"] = "asia-northeast3-b"
	if _, err := runRecommend(ctx); err != nil {
		t.Fatal(err)
	}
	req := readJSON(t, ctx, "recommend", "migrate-request.json")
	subnets := jsonx.ListM(jsonx.Get(req, "targetVNet", "subnetInfoList"))
	if len(subnets) == 0 {
		t.Fatal("서브넷 없음", req)
	}
	for _, sn := range subnets {
		if jsonx.S(sn, "zone") != "asia-northeast3-b" {
			t.Fatal(sn)
		}
	}
	b, _ := os.ReadFile(filepath.Join(ctx.RunDir, "recommend", "rationale.md"))
	if !strings.Contains(string(b), "asia-northeast3-b (사례 target.zone)") {
		t.Fatal(string(b))
	}
}

func TestRecommendPlaceholderFlagged(t *testing.T) {
	ctx := newCtx(t, &fakeTB{}, &fakeBeetle{cands: liveCands(t)}, true)
	writeModel(t, ctx)
	ctx.State.SetKey("source_placeholder", true)
	if _, err := runRecommend(ctx); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(ctx.RunDir, "recommend", "rationale.md"))
	if !strings.Contains(string(b), "자리표시자") || strings.Contains(string(b), "NVIDIA NVIDIA") {
		t.Fatal(string(b))
	}
}

// 생성 가능 사전 검사 — Beetle 검증이 사양 · 이미지 호환을 거부하면 추천 단계에서 멈춘다 (10/27 AWS → NCP)
func TestRecommendPrecheckBlocks(t *testing.T) {
	b := &fakeBeetle{cands: liveCands(t), valid: M{"valid": false, "issues": []any{
		M{"code": "SPEC_IMAGE_INCOMPATIBLE", "severity": "error", "path": "targetInfra.nodeGroups[0]",
			"message": "VM spec 'ncp+kr+gp1t4-g1-h50' and image '23789321' are incompatible for CSP 'ncp'"}}}}
	ctx := newCtx(t, &fakeTB{}, b, true)
	writeModel(t, ctx)
	_, err := runRecommend(ctx)
	if err == nil || !strings.Contains(err.Error(), "생성 가능 사전 검사 실패") || !strings.Contains(err.Error(), "SPEC_IMAGE_INCOMPATIBLE") {
		t.Fatal(err)
	}
	if b.validated != 1 {
		t.Fatal(b.validated)
	}
	if v := jsonx.AsM(ctx.State.Key("target_validation")); v["valid"] != false {
		t.Fatal(v)
	}
	readJSON(t, ctx, "recommend", "validation.json")
	rb, _ := os.ReadFile(filepath.Join(ctx.RunDir, "recommend", "rationale.md"))
	if !strings.Contains(string(rb), "막힘 SPEC_IMAGE_INCOMPATIBLE") {
		t.Fatal(string(rb))
	}
}

// 이름 충돌은 nameSeed 로 이름이 바뀌므로 막지 않는다 · 검증 API 를 못 부르면 막지 않는다
func TestRecommendPrecheckNonBlocking(t *testing.T) {
	b := &fakeBeetle{cands: liveCands(t), valid: M{"valid": false, "issues": []any{
		M{"code": "RESOURCE_ALREADY_EXISTS", "severity": "error", "path": "targetVNet.name", "message": "vNet 'vnet-01' already exists"}}}}
	ctx := newCtx(t, &fakeTB{}, b, true)
	writeModel(t, ctx)
	if _, err := runRecommend(ctx); err != nil {
		t.Fatal(err)
	}
	if v := jsonx.AsM(ctx.State.Key("target_validation")); v["valid"] != true {
		t.Fatal(v)
	}
	b2 := &fakeBeetle{cands: liveCands(t), validErr: errors.New("HTTP 404")}
	ctx2 := newCtx(t, &fakeTB{}, b2, true)
	writeModel(t, ctx2)
	if _, err := runRecommend(ctx2); err != nil {
		t.Fatal(err)
	}
	if v := jsonx.AsM(ctx2.State.Key("target_validation")); v["checked"] != false {
		t.Fatal(v)
	}
}

// Beetle 이 nothing-to-recommend 후보만 주면 그 이유를 그대로 알린다 (10/27 GCP L4 → NCP)
func TestRecommendNothingToRecommend(t *testing.T) {
	b := &fakeBeetle{cands: []any{M{"status": "nothing-to-recommend", "description": "No compatible VM spec and OS image pairs found for GPU infrastructure."}}}
	ctx := newCtx(t, &fakeTB{}, b, true)
	writeModel(t, ctx)
	_, err := runRecommend(ctx)
	if err == nil || !strings.Contains(err.Error(), "No compatible VM spec") {
		t.Fatal(err)
	}
}

func TestRecommendNoCandidates(t *testing.T) {
	ctx := newCtx(t, &fakeTB{}, &fakeBeetle{}, false)
	writeModel(t, ctx)
	if _, err := runRecommend(ctx); err == nil || !strings.Contains(err.Error(), "gcp/asia-northeast3") {
		t.Fatal(err)
	}
}

// ---------- provision ----------

func TestMissingRules(t *testing.T) {
	ports := []M{{"port": 80, "cidr": "203.0.113.254/32"}}
	have := []M{{"Port": "22", "Protocol": "TCP", "Direction": "inbound", "CIDR": "0.0.0.0/0"},
		{"Port": "80", "Protocol": "TCP", "Direction": "inbound", "CIDR": "203.0.113.254/32"}}
	if r := MissingRules(have, ports); len(r) != 0 {
		t.Fatal("Beetle 이 만든 규칙을 다시 넣는다", r)
	}
	if r := MissingRules(have[:1], ports); len(r) != 1 || r[0]["Ports"] != "80" || r[0]["Protocol"] != "tcp" {
		t.Fatal(r)
	}
	if r := MissingRules([]M{{"Port": "80-80", "Protocol": "tcp", "Direction": "inbound", "CIDR": "203.0.113.254/32"}}, append(ports, ports...)); len(r) != 0 {
		t.Fatal(r)
	}
	if r := MissingRules(nil, append(ports, ports...)); len(r) != 1 {
		t.Fatal(r)
	}
}

func TestOpenPortsSkipAndAdd(t *testing.T) {
	tb := &fakeTB{sg: M{"firewallRules": []any{M{"Port": "80", "Protocol": "TCP", "Direction": "inbound", "CIDR": "203.0.113.254/32"}}}}
	ctx := newCtx(t, tb, nil, false)
	if err := openPorts(ctx, tb, "default", []M{{"securityGroupIds": []any{"sg-01"}}}); err != nil || len(tb.added) != 0 {
		t.Fatal(err, tb.added)
	}
	tb2 := &fakeTB{sg: M{"firewallRules": []any{M{"Port": "22", "Protocol": "TCP", "Direction": "inbound", "CIDR": "0.0.0.0/0"}}}}
	ctx2 := newCtx(t, tb2, nil, false)
	if err := openPorts(ctx2, tb2, "default", []M{{"securityGroupIds": []any{"sg-01"}}}); err != nil || len(tb2.added) != 1 || tb2.added[0][0]["Ports"] != "80" {
		t.Fatal(err, tb2.added)
	}
}

func TestProvisionBeetleMissingInfraID(t *testing.T) {
	fb := &fakeBeetle{migrate: M{"status": "Creating"}}
	ctx := newCtx(t, &fakeTB{}, fb, false)
	ctx.WriteJSON("recommend", "migrate-request.json", M{"targetInfra": M{}})
	if _, err := runProvision(ctx); err == nil || !strings.Contains(err.Error(), "Creating") {
		t.Fatal(err)
	}
	if !ctx.State.HasKey("beetle_migrate_started") || ctx.State.KeyS("beetle_name_seed") == "" {
		t.Fatal("부작용 전에 기록하지 않았다")
	}
	if fb.use == nil || *fb.use || fb.seed != ctx.State.KeyS("beetle_name_seed") || !strings.HasPrefix(fb.seed, "wr-gcp-") {
		t.Fatalf("useExisting=false · run 별 nameSeed 로 보내지 않았다: %v %q", fb.use, fb.seed)
	}
}

func zoneReq() M {
	return M{"targetInfra": M{"nodeGroups": []any{M{"connectionName": "gcp-asia-northeast3"}}},
		"targetVNet": M{"cidrBlock": "10.0.0.0/13", "subnetInfoList": []any{M{"name": "subnet-01", "ipv4_CIDR": "10.2.0.0/16"}}}}
}

var errNoCapacity = errors.New(`HTTP 500 — VM wr-gcp-1-vm-1: Operation errors: The zone 'projects/p/zones/asia-northeast3-a' does not have enough resources available to fulfill the request.`)

// 존 자원 부족이면 남은 인프라를 Beetle 로 지우고 다음 존으로 다시 보낸다 — 기본 존(a) → b.
func TestProvisionBeetleZoneFallback(t *testing.T) {
	fb := &fakeBeetle{migrate: M{"id": "x"}, migrateErrs: []error{errNoCapacity}}
	tb := &fakeTB{assigned: "asia-northeast3-a", zones: []string{"asia-northeast3-a", "asia-northeast3-b", "asia-northeast3-c"}}
	ctx := newCtx(t, tb, fb, false)
	id, err := beetleCreate(ctx, fb, tb, "default", "wr-gcp", zoneReq())
	if err != nil || id != "x" {
		t.Fatal(id, err)
	}
	zoneOf := func(body M) string {
		return jsonx.S(jsonx.ListM(jsonx.Get(body, "targetVNet", "subnetInfoList"))[0], "zone")
	}
	if len(fb.migrateSent) != 2 || zoneOf(fb.migrateSent[0]) != "asia-northeast3-a" || zoneOf(fb.migrateSent[1]) != "asia-northeast3-b" {
		t.Fatal(len(fb.migrateSent), fb.migrateSent)
	}
	if len(fb.deleted) != 1 || !strings.HasSuffix(fb.deleted[0], "-gpu-infra-01") {
		t.Fatal("남은 인프라를 지우지 않았다", fb.deleted)
	}
	tries := jsonx.ListM(ctx.State.Key("provision_zone_tries"))
	if len(tries) != 1 || jsonx.S(tries[0], "zone") != "asia-northeast3-a" || ctx.State.KeyS("provision_zone") != "asia-northeast3-b" {
		t.Fatal(tries, ctx.State.KeyS("provision_zone"))
	}
}

// 사례 target.zone 이 있으면 그 존부터, 존 자원 부족이 아닌 오류는 넘기지 않고 멈춘다.
func TestProvisionBeetleZoneOrderAndOtherError(t *testing.T) {
	fb := &fakeBeetle{migrateErrs: []error{errors.New("HTTP 500 — quota exceeded for GPUS_ALL_REGIONS")}}
	tb := &fakeTB{assigned: "asia-northeast3-a", zones: []string{"asia-northeast3-a", "asia-northeast3-b", "asia-northeast3-c"}}
	ctx := newCtx(t, tb, fb, false)
	ctx.Case.Target()["zone"] = "asia-northeast3-c"
	if _, err := beetleCreate(ctx, fb, tb, "default", "wr-gcp", zoneReq()); err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatal(err)
	}
	if got := jsonx.Strings(ctx.State.Key("provision_zones")); strings.Join(got, ",") != "asia-northeast3-c,asia-northeast3-a,asia-northeast3-b" {
		t.Fatal(got)
	}
	if len(fb.migrateSent) != 1 || len(fb.deleted) != 0 || !ctx.State.HasKey("beetle_migrate_started") {
		t.Fatal(len(fb.migrateSent), fb.deleted)
	}
}

// 모든 존에서 자원이 없으면 마지막 시도도 지우고, 시도한 존을 대며 멈춘다.
func TestProvisionBeetleAllZonesFull(t *testing.T) {
	fb := &fakeBeetle{migrateErrs: []error{errNoCapacity, errNoCapacity}}
	tb := &fakeTB{assigned: "z-a", zones: []string{"z-a", "z-b"}}
	ctx := newCtx(t, tb, fb, false)
	if _, err := beetleCreate(ctx, fb, tb, "default", "wr-gcp", zoneReq()); err == nil || !strings.Contains(err.Error(), "z-a · z-b") {
		t.Fatal(err)
	}
	if len(fb.deleted) != 2 || len(fb.ids) != 0 || ctx.State.HasKey("beetle_migrate_started") {
		t.Fatal(fb.deleted, fb.ids)
	}
}

// 재개 때 앞선 시도의 인프라가 Failed 로 남아 있으면 지우고 다음 존부터 이어 간다.
func TestProvisionBeetleResumeFailedInfra(t *testing.T) {
	fb := &fakeBeetle{migrate: M{"id": "y"}}
	tb := &fakeTB{assigned: "z-a", zones: []string{"z-a", "z-b"}, infra: M{"status": "Failed:1 (R:0/1)"}}
	ctx := newCtx(t, tb, fb, false)
	ctx.State.SetKey("beetle_name_seed", "wr-gcp-1")
	ctx.State.SetKey("provision_zones", []any{"z-a", "z-b"})
	ctx.State.SetKey("provision_zone", "z-a")
	ctx.State.SetKey("beetle_migrate_started", true)
	fb.ids = []string{"wr-gcp-1-gpu-infra-01"}
	id, err := beetleCreate(ctx, fb, tb, "default", "wr-gcp", zoneReq())
	if err != nil || id != "y" || len(fb.deleted) != 1 || len(fb.migrateSent) != 1 {
		t.Fatal(id, err, fb.deleted, len(fb.migrateSent))
	}
	if z := jsonx.S(jsonx.ListM(jsonx.Get(fb.migrateSent[0], "targetVNet", "subnetInfoList"))[0], "zone"); z != "z-b" {
		t.Fatal(z)
	}
}

func TestIsZoneCapacity(t *testing.T) {
	for msg, want := range map[string]bool{
		"The zone 'x' does not have enough resources available":                                  true,
		"ZONE_RESOURCE_POOL_EXHAUSTED":                                                           true,
		"InsufficientInstanceCapacity: We currently do not have sufficient g4dn.xlarge capacity": true,
		"VcpuLimitExceeded": false,
		"quota exceeded":    false,
	} {
		if IsZoneCapacity(errors.New(msg)) != want {
			t.Fatal(msg)
		}
	}
}

func TestNameSeed(t *testing.T) {
	if s := NameSeed("wr-gcp", "20261023-101530"); s != "wr-gcp-10231015" {
		t.Fatal(s)
	}
	s := NameSeed("Very_Long Target.Name-For-Test", "20261023-101530-4242")
	if len(s) > 20 || !strings.HasSuffix(s, "-10231015") || strings.ContainsAny(s, "_. ") || s[0] == '-' {
		t.Fatal(s)
	}
	if s := NameSeed("___", "20261023-101530"); s != "gmp-10231015" {
		t.Fatal(s)
	}
}

func TestFindSeededResumesBeetleCreate(t *testing.T) {
	fb := &fakeBeetle{ids: []string{"other-infra101", "wr-gcp-10231015-infra101"}}
	if id, err := findSeeded(fb, "default", "wr-gcp-10231015"); err != nil || id != "wr-gcp-10231015-infra101" {
		t.Fatal(id, err)
	}
	fb.ids = append(fb.ids, "wr-gcp-10231015-infra102")
	if _, err := findSeeded(fb, "default", "wr-gcp-10231015"); err == nil {
		t.Fatal("둘 이상을 골랐다")
	}
	if id, _ := findSeeded(&fakeBeetle{ids: []string{"wr-gcp-1023101500"}}, "default", "wr-gcp-10231015"); id != "" {
		t.Fatal("접두어만 같은 다른 seed 를 골랐다", id)
	}
}

// ---------- VM 접근 (원격 명령) ----------

func targetCtx(t *testing.T, tb *fakeTB) *Context {
	ctx := newCtx(t, tb, nil, false)
	ctx.State.SetKey("target_infra_id", "wr-gcp-1")
	ctx.SetNodes("target", []M{{"nodeId": "g1-1", "publicIP": "203.0.113.5", "sshPort": 22, "nodeUserName": "cb-user"}}, "cb-user")
	return ctx
}

func TestWrapAndParse(t *testing.T) {
	w := Wrap(`echo "a b"; exit 3`)
	if !strings.HasPrefix(w, "bash -c '") || !strings.Contains(w, rcMarker) {
		t.Fatal(w)
	}
	body, rc, ok := ParseWrapped("hello\nworld\n\n" + rcMarker + "3\n")
	if !ok || rc != 3 || body != "hello\nworld\n" {
		t.Fatalf("%q %d %v", body, rc, ok)
	}
	if body, rc, ok := ParseWrapped("no-newline\n" + rcMarker + "0\n"); !ok || rc != 0 || body != "no-newline" {
		t.Fatalf("%q %d %v", body, rc, ok)
	}
	if _, _, ok := ParseWrapped("reboot..."); ok {
		t.Fatal("표식 없이 성공으로 읽었다")
	}
}

func TestExecUnreachable(t *testing.T) {
	ctx := targetCtx(t, &fakeTB{cmdOut: func(string) string { return "" }})
	r, err := ctx.Exec("target", "true", 1)
	if err != nil || r.Reachable {
		t.Fatal(r, err)
	}
}

func TestReadTextExpandsHome(t *testing.T) {
	tb := &fakeTB{cmdOut: func(string) string { return "timestamp,result\n\n" + rcMarker + "0\n" }}
	ctx := targetCtx(t, tb)
	text, ok, err := ctx.ReadText("target", "~/widgetrag-run/results.csv")
	if err != nil || !ok || text != "timestamp,result\n" {
		t.Fatalf("%q %v %v", text, ok, err)
	}
	if !strings.Contains(tb.cmds[0], "cat ~/widgetrag-run/results.csv") || strings.Contains(tb.cmds[0], "'~/") {
		t.Fatal("홈 경로가 따옴표 안에 들어갔다:", tb.cmds[0])
	}
	tb.cmdOut = func(string) string { return rcMarker + "3\n" }
	if _, ok, err := ctx.ReadText("target", "~/nope"); ok || err != nil {
		t.Fatal(ok, err)
	}
}

func tarGzB64(t *testing.T, files map[string]string) string {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	tw.WriteHeader(&tar.Header{Name: "migration-x/", Typeflag: tar.TypeDir, Mode: 0o755})
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
		tw.Write([]byte(body))
	}
	tw.Close()
	zw.Close()
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestFetchDirStripsTopAndBlocksEscape(t *testing.T) {
	good := tarGzB64(t, map[string]string{"migration-x/summary.txt": "PASS\n", "migration-x/sub/rows.tsv": "a\tb\n"})
	tb := &fakeTB{cmdOut: func(string) string { return good + "\n" + rcMarker + "0\n" }}
	ctx := targetCtx(t, tb)
	dest := filepath.Join(t.TempDir(), "evidence")
	if ok, err := ctx.FetchDir("target", "~/widgetrag-run/migration-x", dest); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "sub", "rows.tsv")); string(b) != "a\tb\n" {
		t.Fatal(string(b))
	}
	bad := tarGzB64(t, map[string]string{"migration-x/../../escape.txt": "x"})
	tb.cmdOut = func(string) string { return bad + "\n" + rcMarker + "0\n" }
	if _, err := ctx.FetchDir("target", "~/x", filepath.Join(t.TempDir(), "e")); err == nil {
		t.Fatal("폴더 밖 경로를 풀었다")
	}
}

// ---------- Beetle 이관 본문 ----------

func TestBeetleBodyEncryptsKeys(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	enc, err := crypto.New(M{"keyId": "k1", "publicKey": string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))})
	if err != nil {
		t.Fatal(err)
	}
	const secret = "-----BEGIN OPENSSH PRIVATE KEY-----\nSECRET\n-----END OPENSSH PRIVATE KEY-----"
	tb := &fakeTB{access: []M{{"nodeId": "g1-1", "privateKey": secret}}}
	ctx := targetCtx(t, tb)
	ctx.SetNodes("source", []M{{"nodeId": "g1-1", "publicIP": "198.51.100.10", "sshPort": 22, "nodeUserName": "cb-user"}}, "cb-user")
	body, err := BuildBeetleBody(ctx, enc, "~/widgetrag-backup/20261022", "~/widgetrag-package", "gmp-x-data-2")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := jsonx.Marshal(body, false)
	if strings.Contains(string(raw), "SECRET") {
		t.Fatal("개인키가 평문으로 들어갔다")
	}
	if !strings.Contains(jsonx.S(jsonx.AsM(body["source"]), "preCmd"), "gmp-x-data-2") || !strings.Contains(jsonx.S(jsonx.AsM(body["destination"]), "postCmd"), "gmp-x-data-2") {
		t.Fatal("훅 표식에 요청 ID 가 없다:", string(raw))
	}
	if jsonx.S(jsonx.AsM(body["source"]), "path") != "/home/cb-user/widgetrag-backup/20261022/" || body["encryptionKeyId"] != "k1" ||
		jsonx.S(jsonx.AsM(jsonx.Get(body, "destination", "filesystem", "ssh")), "host") != "203.0.113.5" {
		t.Fatal(string(raw))
	}
	pems, _ := filepath.Glob(filepath.Join(ctx.RunDir, "*", "*.pem"))
	if more, _ := filepath.Glob(filepath.Join(ctx.RunDir, "*.pem")); len(pems)+len(more) > 0 {
		t.Fatal("Beetle 이관에 키 파일을 남겼다", pems, more)
	}
}

// ---------- 실패한 요청 다시 보내기 ----------

func sourceCtx(t *testing.T, tb *fakeTB) *Context {
	ctx := targetCtx(t, tb)
	ctx.SetNodes("source", []M{{"nodeId": "g1-1", "publicIP": "198.51.100.10", "sshPort": 22, "nodeUserName": "cb-user"}}, "cb-user")
	return ctx
}

func remoteFile(t *testing.T, ctx *Context, stage, name string) string {
	b, err := os.ReadFile(filepath.Join(ctx.State.StageDir(stage), name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRunRemoteResendsFailedRequest(t *testing.T) {
	tb := &fakeTB{cmdStatus: map[string][]M{"old-1": {{"xRequestId": "old-1", "status": "CompletedWithError", "resultSummary": "exit 1"}}}}
	ctx := sourceCtx(t, tb)
	ctx.State.SetKey("xreq_backup", "old-1")
	os.WriteFile(filepath.Join(ctx.State.StageDir(stPackage), "backup.stdout.txt"), []byte("앞선 시도"), 0o644)
	moved := 0
	if _, err := runRemote(ctx, "backup", "bash package.sh backup x", 120, func(try int) error { moved = try; return nil }); err != nil {
		t.Fatal(err)
	}
	if len(tb.async) != 1 || tb.async[0] == "old-1" || ctx.State.KeyS("xreq_backup") != tb.async[0] {
		t.Fatalf("새 ID 로 다시 보내지 않았다: %v · %s", tb.async, ctx.State.KeyS("xreq_backup"))
	}
	tries := jsonx.ListM(ctx.State.Key("xreq_backup_tries"))
	if len(tries) != 1 || tries[0]["id"] != "old-1" || tries[0]["status"] != "CompletedWithError" || moved != 1 {
		t.Fatalf("시도 기록 %v · 흔적 치우기 %d", tries, moved)
	}
	if remoteFile(t, ctx, stPackage, "backup.try1.stdout.txt") != "앞선 시도" || !strings.Contains(remoteFile(t, ctx, stPackage, "backup.stdout.txt"), tb.async[0]) {
		t.Fatal("앞선 시도의 출력을 남기지 않았다")
	}
}

func TestRunRemoteMissingRecordResends(t *testing.T) {
	tb := &fakeTB{}
	ctx := sourceCtx(t, tb)
	ctx.State.SetKey("xreq_rebuild", "never-sent")
	if _, err := runRemote(ctx, "rebuild", "./40-start-apps.sh --rebuild", 20, nil); err != nil {
		t.Fatal(err)
	}
	tries := jsonx.ListM(ctx.State.Key("xreq_rebuild_tries"))
	if len(tb.async) != 1 || len(tries) != 1 || tries[0]["status"] != "missing" {
		t.Fatalf("기록 없는 요청을 다시 보내지 않았다: %v %v", tb.async, tries)
	}
}

func TestRunRemoteWaitsOnRunningOrDoneRequest(t *testing.T) {
	for _, st := range []string{"Handling", "Completed"} {
		tb := &fakeTB{cmdStatus: map[string][]M{"old-1": {{"xRequestId": "old-1", "status": st}}}}
		ctx := sourceCtx(t, tb)
		ctx.State.SetKey("xreq_backup", "old-1")
		if _, err := runRemote(ctx, "backup", "x", 120, func(int) error { t.Fatal("흔적을 치웠다"); return nil }); err != nil {
			t.Fatal(st, err)
		}
		if len(tb.async) != 0 || ctx.State.KeyS("xreq_backup") != "old-1" || ctx.State.Key("xreq_backup_tries") != nil {
			t.Fatalf("%s 인 요청을 다시 보냈다: %v", st, tb.async)
		}
	}
}

func TestRunRemoteFailureStillStopsFirstRun(t *testing.T) {
	tb := &fakeTB{asyncStatus: "CompletedWithError"}
	ctx := sourceCtx(t, tb)
	if _, err := runRemote(ctx, "backup", "x", 120, nil); err == nil || !strings.Contains(err.Error(), "backup 실패") {
		t.Fatalf("첫 실행의 실패를 넘겼다: %v", err)
	}
	if len(tb.async) != 1 {
		t.Fatalf("같은 실행 안에서 다시 보냈다: %v", tb.async)
	}
}

func TestMoveAsideCommand(t *testing.T) {
	tb := &fakeTB{cmdOut: func(string) string { return "moved\n" + rcMarker + "0\n" }}
	ctx := sourceCtx(t, tb)
	if err := moveAside(ctx, stPackage, "source", "~/widgetrag-backup/20261022-1/", 2); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tb.cmds[0], "mv ~/widgetrag-backup/20261022-1 ~/widgetrag-backup/20261022-1.failed-2") || strings.Contains(tb.cmds[0], "rm ") {
		t.Fatal(tb.cmds[0])
	}
	tb.cmdOut = func(string) string { return rcMarker + "3\n" }
	if err := moveAside(ctx, stPackage, "source", "~/x", 1); err == nil {
		t.Fatal("옮길 자리가 있는데 넘어갔다")
	}
}

// transferCtx 는 Beetle 이관 단계를 끝까지 돌릴 수 있는 가짜 환경이다. verifyOK 가 false 면 타겟 패키지 대조가 떨어진다.
func transferCtx(t *testing.T, verifyOK *bool) (*Context, *fakeTB, *fakeBeetle) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	fb := &fakeBeetle{pub: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), reqs: map[string]M{}}
	tb := &fakeTB{access: []M{{"nodeId": "g1-1", "privateKey": "-----BEGIN OPENSSH PRIVATE KEY-----\nK\n-----END OPENSSH PRIVATE KEY-----"}}}
	tb.cmdOut = func(cmd string) string {
		ok := rcMarker + "0\n"
		last := ""
		if len(fb.sent) > 0 {
			last = fb.sent[len(fb.sent)-1]
		}
		switch {
		case strings.Contains(cmd, "cat ~/") && strings.Contains(cmd, ".gmp-transferred"):
			return rcMarker + "3\n"
		case strings.Contains(cmd, "if [ -e"):
			return "moved\n" + ok
		case strings.Contains(cmd, "command -v rsync"):
			return "present\n" + ok
		case strings.Contains(cmd, `"$HOME"`):
			return "/home/cb-user\n" + ok
		case strings.Contains(cmd, "gmp-precmd.txt") || strings.Contains(cmd, "gmp-postcmd.txt"):
			return "gmp-hook " + last + "\n" + ok
		case strings.Contains(cmd, "sha256sum -c"):
			if !*verifyOK {
				return "BAD a.txt\nFILES=3 BAD=1\n" + rcMarker + "1\n"
			}
			return "FILES=3 BAD=0\n" + ok
		}
		return ok
	}
	ctx := sourceCtx(t, tb)
	ctx.NewBeetle = func() (Beetle, error) { return fb, nil }
	ctx.State.SetKey("backup_out", "~/widgetrag-backup/20261022-1")
	return ctx, tb, fb
}

func cmdsWith(tb *fakeTB, part string) int {
	n := 0
	for _, c := range tb.cmds {
		if strings.Contains(c, part) {
			n++
		}
	}
	return n
}

func TestTransferResendsFailedBeetleRequest(t *testing.T) {
	ok := true
	ctx, tb, fb := transferCtx(t, &ok)
	first := "gmp-" + ctx.State.Data.RunID + "-data"
	ctx.State.SetKey("beetle_data_req", first)
	fb.reqs[first] = M{"status": "Error", "errorResponse": M{"message": "dial tcp: i/o timeout"}}
	os.WriteFile(filepath.Join(ctx.State.StageDir(stTransfer), "beetle-request.json"), []byte(`{"status":"Error"}`), 0o644)
	if _, err := runTransfer(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fb.sent) != 1 || fb.sent[0] != first+"-2" || ctx.State.KeyS("beetle_data_req") != first+"-2" {
		t.Fatalf("새 ID 로 다시 보내지 않았다: %v", fb.sent)
	}
	if cmdsWith(tb, "mv ~/widgetrag-package ~/widgetrag-package.failed-1") != 1 {
		t.Fatal("앞선 시도가 쓴 타겟 폴더를 옆으로 옮기지 않았다", tb.cmds)
	}
	if tries := jsonx.ListM(ctx.State.Key("beetle_data_req_tries")); len(tries) != 1 || tries[0]["status"] != "error" {
		t.Fatalf("시도 기록 %v", tries)
	}
	if remoteFile(t, ctx, stTransfer, "beetle-request.try1.json") != `{"status":"Error"}` {
		t.Fatal("앞선 시도의 응답을 남기지 않았다")
	}
	if h := jsonx.AsM(ctx.State.Key("beetle_hooks")); h["preCmd_ran"] != true || h["postCmd_ran"] != true {
		t.Fatalf("훅 확인 %v", h)
	}
	body := fb.bodies[0]
	if src, dst := jsonx.S(jsonx.AsM(body["source"]), "path"), jsonx.S(jsonx.AsM(body["destination"]), "path"); src != "/home/cb-user/widgetrag-backup/20261022-1/" || dst != "/home/cb-user/widgetrag-package" {
		t.Fatalf("Beetle 경로가 절대 경로가 아니다: %s → %s", src, dst)
	}
	if cmdsWith(tb, "command -v rsync") != 2 {
		t.Fatal("두 VM 의 rsync 를 확인하지 않았다", tb.cmds)
	}
}

func TestTransferStopsWithoutRsync(t *testing.T) {
	ok := true
	ctx, tb, fb := transferCtx(t, &ok)
	base := tb.cmdOut
	tb.cmdOut = func(cmd string) string {
		if strings.Contains(cmd, "command -v rsync") {
			return "E: Unable to locate package rsync\n" + rcMarker + "1\n"
		}
		return base(cmd)
	}
	if _, err := runTransfer(ctx); err == nil || !strings.Contains(err.Error(), "rsync") {
		t.Fatalf("rsync 없이 넘어갔다: %v", err)
	}
	if len(fb.sent) != 0 || ctx.State.KeyS("beetle_data_req") != "" {
		t.Fatal("rsync 가 없는데 Beetle 요청을 보냈거나 요청 ID 를 적었다")
	}
}

func TestAbsPath(t *testing.T) {
	tb := &fakeTB{cmdOut: func(string) string { return "/home/cb-user/\n" + rcMarker + "0\n" }}
	ctx := sourceCtx(t, tb)
	for in, want := range map[string]string{"~/a/b": "/home/cb-user/a/b", "~": "/home/cb-user", "/opt/x": "/opt/x"} {
		if got, err := absPath(ctx, "source", in); err != nil || got != want {
			t.Fatalf("%s → %s (%v)", in, got, err)
		}
	}
	tb.cmdOut = func(string) string { return "\n" + rcMarker + "0\n" }
	if _, err := absPath(ctx, "source", "~/a"); err == nil {
		t.Fatal("빈 홈을 받아들였다")
	}
}

// ---------- 소스 명령에 넘기는 값 ----------

func TestSourceCommandsGetOnlyLLMSettings(t *testing.T) {
	ctx := newCtx(t, &fakeTB{}, nil, false)
	raw := caseRaw()
	raw["run"] = M{"env": M{"LLM_TEMPERATURE": "0", "LLM_SEED": "42", "COMPARE": "1", "CODE_SOURCE": "package", "VERIFY_ADMIN_PASSWORD": "pw"}}
	c, err := config.NewCase(filepath.Join(t.TempDir(), "x.yaml"), raw, []string{"LLM_TEMPERATURE", "LLM_SEED", "COMPARE", "CODE_SOURCE", "VERIFY_ADMIN_PASSWORD"})
	if err != nil {
		t.Fatal(err)
	}
	ctx.Case = c
	rebuild, err := ctx.Remote("rebuild", nil)
	if err != nil || rebuild != "cd ~/WidgetRAG/scripts/local && LLM_TEMPERATURE=0 LLM_SEED=42 ./40-start-apps.sh --rebuild" {
		t.Fatalf("소스 재기동: %q %v", rebuild, err)
	}
	entry, _ := ctx.Remote("entry", nil)
	for _, want := range []string{"FORM=t-case-target", "CODE_SOURCE=package", "COMPARE=1", "VERIFY_ADMIN_PASSWORD=pw"} {
		if !strings.Contains(entry, want) {
			t.Fatalf("타겟 단일 명령에 %s 가 없다: %s", want, entry)
		}
	}
	ad, err := config.LoadAdapter("widgetrag-b")
	if err != nil {
		t.Fatal(err)
	}
	ctx.Adapter = ad
	restart, _ := ctx.Remote("restart", nil)
	if strings.Contains(restart, "COMPARE") || strings.Contains(restart, "FORM") || strings.Contains(restart, "PASSWORD") || !strings.Contains(restart, "LLM_SEED=42") {
		t.Fatalf("Compose 소스 재기동에 타겟용 값이 섞였다: %s", restart)
	}
}

func TestTransferWaitsOnRunningBeetleRequest(t *testing.T) {
	ok := true
	ctx, tb, fb := transferCtx(t, &ok)
	rid := "gmp-" + ctx.State.Data.RunID + "-data"
	ctx.State.SetKey("beetle_data_req", rid)
	fb.reqs[rid] = M{"status": "Handling"}
	fb.sent = []string{rid} // 훅 표식은 이 요청의 것
	if _, err := runTransfer(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fb.sent) != 1 || cmdsWith(tb, "ls -A") != 0 || cmdsWith(tb, "if [ -e") != 0 {
		t.Fatalf("도는 요청을 두고 다시 보냈거나 폴더를 건드렸다: %v", tb.cmds)
	}
}

func TestTransferVerifyFailureResendsOnResume(t *testing.T) {
	ok := false
	ctx, tb, fb := transferCtx(t, &ok)
	if _, err := runTransfer(ctx); err == nil {
		t.Fatal("대조 실패를 넘겼다")
	}
	first := fb.sent[0]
	if ctx.State.KeyS("beetle_data_verify_failed") != first {
		t.Fatal("대조 실패를 기록하지 않았다")
	}
	ok = true
	if _, err := runTransfer(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fb.sent) != 2 || fb.sent[1] != first+"-2" || ctx.State.HasKey("beetle_data_verify_failed") || cmdsWith(tb, ".failed-1") != 1 {
		t.Fatalf("대조에서 떨어진 이관을 다시 보내지 않았다: %v", fb.sent)
	}
}

// ---------- 판정 단계의 타겟 상태 회수 ----------

func TestDiagnosticsScript(t *testing.T) {
	s, err := DiagnosticsScript([]M{{"name": "gpu.txt", "command": "nvidia-smi"}, {"name": "app-logs.tar.gz", "command": "tar -czf - -C ~/x logs"}}, `"$HOME/gmp-diag-r1"`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`d="$HOME/gmp-diag-r1"; mkdir -p "$d"`, `( nvidia-smi ) > "$d/gpu.txt" 2> "$d/gpu.txt.stderr" < /dev/null || true`, `"$d/app-logs.tar.gz"`, `-size 0 -delete`} {
		if !strings.Contains(s, want) {
			t.Fatalf("스크립트에 %q 가 없다:\n%s", want, s)
		}
	}
	for _, bad := range []M{{"name": "../x", "command": "id"}, {"name": "a/b", "command": "id"}, {"name": "ok.txt", "command": " "}} {
		if _, err := DiagnosticsScript([]M{bad}, `"$HOME/d"`); err == nil {
			t.Fatalf("잘못된 항목을 받아들였다: %v", bad)
		}
	}
}

func TestAdaptersDiagnosticsValid(t *testing.T) {
	for _, name := range []string{"widgetrag-a", "widgetrag-b"} {
		ad, err := config.LoadAdapter(name)
		if err != nil {
			t.Fatal(err)
		}
		items := jsonx.ListM(ad.Raw["diagnostics"])
		if len(items) < 4 {
			t.Fatalf("%s: diagnostics 가 %d개뿐이다", name, len(items))
		}
		if _, err := DiagnosticsScript(items, `"$HOME/d"`); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestCollectDiagnostics(t *testing.T) {
	tb := &fakeTB{}
	ctx := targetCtx(t, tb)
	dir := "gmp-diag-" + ctx.State.Data.RunID
	tb.cmdOut = func(cmd string) string {
		switch {
		case strings.Contains(cmd, "mkdir -p"):
			return "gpu.txt\nmodels.txt\n" + rcMarker + "0\n"
		case strings.Contains(cmd, "tar -C"):
			return tarGzB64(t, map[string]string{dir + "/gpu.txt": "Tesla T4\n", dir + "/models.txt": "gemma3:4b\n"}) + "\n" + rcMarker + "0\n"
		}
		return rcMarker + "0\n"
	}
	collectDiagnostics(ctx)
	got, err := os.ReadFile(filepath.Join(ctx.State.StageDir(stVerify), "target", "gpu.txt"))
	if err != nil || string(got) != "Tesla T4\n" {
		t.Fatalf("타겟 상태를 받지 못했다: %q %v", got, err)
	}
	if cmdsWith(tb, `$HOME/`+dir) != 1 || cmdsWith(tb, "~/"+dir) != 1 {
		t.Fatalf("수집 · 회수 명령이 run 폴더 이름을 쓰지 않았다: %v", tb.cmds)
	}
	// 수집이 실패해도 멈추지 않는다 (판정은 그대로)
	tb.cmdOut = func(string) string { return rcMarker + "1\n" }
	collectDiagnostics(ctx)
}

func TestEvidenceDirScriptSkipsFiles(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"migration-20261022-010101", "migration-20261023-020921"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "migration-compare.csv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("bash", "-c", EvidenceDirScript(dir+"/migration-*")).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != filepath.Join(dir, "migration-20261023-020921") {
		t.Fatalf("증적 폴더 대신 %q 를 골랐다", got)
	}
	if out, _ := exec.Command("bash", "-c", EvidenceDirScript(t.TempDir()+"/migration-*")).Output(); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("폴더가 없는데 %q 를 골랐다", out)
	}
}

// ---------- summary.md ----------

func TestSummaryRewrittenAfterNoteAndFinalStatus(t *testing.T) {
	ctx := newCtx(t, &fakeTB{}, nil, false)
	read := func() string {
		b, err := os.ReadFile(filepath.Join(ctx.RunDir, "summary.md"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	ctx.State.Mark("cleanup", "running", "", nil)
	if err := WriteSummary(ctx); err != nil {
		t.Fatal(err)
	}
	if s := read(); !strings.Contains(s, "| cleanup | running |") || !strings.Contains(s, "- 없음") {
		t.Fatalf("처음 요약이 이상하다:\n%s", s)
	}
	ctx.State.Mark("cleanup", "done", "", nil)
	ctx.State.Note("증적 폴더를 다시 받으려고 verify 재실행", "verify", "")
	if err := Summarize(ctx.Case, ctx.State); err != nil {
		t.Fatal(err)
	}
	s := read()
	if !strings.Contains(s, "| cleanup | done |") || !strings.Contains(s, "[verify] 증적 폴더를 다시 받으려고 verify 재실행") || strings.Contains(s, "- 없음") {
		t.Fatalf("note · 최종 상태가 요약에 반영되지 않았다:\n%s", s)
	}
}
