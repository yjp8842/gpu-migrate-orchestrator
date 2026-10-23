// migrate — GPU 추론서비스 이관 오케스트레이터 CLI.
//
//	migrate run      --case cases/X.yaml [--resume RUN|latest] [--from 단계] [--to 단계] [--dry-run]
//	migrate <단계>   --case cases/X.yaml [--run RUN|latest]       (collect · recommend · provision · package · transfer · start · verify · cleanup)
//	migrate status   --case cases/X.yaml                          읽기 전용 — 소스 인프라 상태 · 최근 run 요약
//	migrate note     --case cases/X.yaml [--stage 단계] "사람이 한 조치"   수동 개입 기록 (수작업 유발 구간 목록의 원천) — summary.md 도 다시 쓴다
//	migrate summary  --case cases/X.yaml [--run RUN|latest]       summary.md 만 다시 쓴다 (state.json 기준, 원격 호출 없음)
//	migrate beetle-selftest                                       Beetle 데이터 이관 암호화 형식 자가 확인
//
// 공통: --settings settings.yaml (기본 프로젝트의 settings.yaml, 없으면 settings.example.yaml)
// 환경변수: 설정 · 사례의 ${이름} 을 채운다. 프로젝트 폴더의 .env 를 먼저 읽는다 (견본 .env.example)
// 종료 코드: 0 정상 · 1 판정 FAIL / NO-RESULT · 2 오류(설정 · 호출 · 단계 실패)
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"

	"gpu-migrate-orchestrator/internal/beetle"
	"gpu-migrate-orchestrator/internal/config"
	"gpu-migrate-orchestrator/internal/crypto"
	"gpu-migrate-orchestrator/internal/jsonx"
	"gpu-migrate-orchestrator/internal/logx"
	"gpu-migrate-orchestrator/internal/stages"
	"gpu-migrate-orchestrator/internal/state"
	"gpu-migrate-orchestrator/internal/tumblebug"
)

const usage = `migrate — GPU 추론서비스 이관 오케스트레이터

  migrate run      --case cases/X.yaml [--resume RUN|latest] [--from 단계] [--to 단계] [--dry-run] [--quiet]
  migrate <단계>   --case cases/X.yaml [--run RUN|latest] [--dry-run]
                   단계: collect recommend provision package transfer start verify cleanup
  migrate status   --case cases/X.yaml
  migrate note     --case cases/X.yaml [--run latest] [--stage 단계] "사람이 한 조치"
  migrate summary  --case cases/X.yaml [--run latest]           summary.md 만 다시 쓴다
  migrate beetle-selftest

  공통: --settings settings.yaml
  종료 코드: 0 정상 · 1 판정 FAIL / NO-RESULT · 2 오류
`

type opts struct {
	settings, casePath, resume, runID, from, to, stage, noteStage, text string
	dryRun, quiet                                                       bool
}

// splitSettings 는 --settings 를 하위 명령 앞뒤 어디에 써도 받는다.
func splitSettings(args []string) (string, []string) {
	path := ""
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--settings" || a == "-settings":
			if i+1 < len(args) {
				path = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--settings=") || strings.HasPrefix(a, "-settings="):
			path = a[strings.Index(a, "=")+1:]
		default:
			rest = append(rest, a)
		}
	}
	return path, rest
}

// parseInterleaved 는 선택지와 위치 인자가 섞여 있어도 읽는다.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func isStage(s string) bool {
	for _, n := range config.Stages {
		if n == s {
			return true
		}
	}
	return false
}

func main() { os.Exit(run(os.Args[1:])) }

func run(argv []string) int {
	settingsPath, args := splitSettings(argv)
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(usage)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	// 주소 · 계정 · 비밀번호는 환경변수로 받는다 — 프로젝트 폴더의 .env(gitignore)가 있으면 먼저 읽는다 (셸 쪽이 우선)
	if err := config.LoadDotEnv(filepath.Join(config.ProjectRoot(), ".env")); err != nil {
		fmt.Fprintln(os.Stderr, "오류:", err)
		return 2
	}
	cmd, rest := args[0], args[1:]
	o := opts{settings: settingsPath}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.casePath, "case", "", "cases/*.yaml")
	common := func() {
		fs.BoolVar(&o.dryRun, "dry-run", false, "")
		fs.BoolVar(&o.quiet, "quiet", false, "")
	}
	switch {
	case cmd == "run":
		common()
		fs.StringVar(&o.resume, "resume", "", "")
		fs.StringVar(&o.from, "from", "", "")
		fs.StringVar(&o.to, "to", "", "")
	case isStage(cmd):
		common()
		def := "latest"
		if cmd == "collect" {
			def = ""
		}
		fs.StringVar(&o.runID, "run", def, "")
		o.stage = cmd
	case cmd == "status":
	case cmd == "note":
		fs.StringVar(&o.runID, "run", "latest", "")
		fs.StringVar(&o.noteStage, "stage", "", "")
	case cmd == "summary":
		fs.StringVar(&o.runID, "run", "latest", "")
	case cmd == "beetle-selftest":
	default:
		fmt.Fprintf(os.Stderr, "[migrate] 모르는 명령: %s\n\n%s", cmd, usage)
		return 2
	}
	pos, err := parseInterleaved(fs, rest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[migrate] %v\n\n%s", err, usage)
		return 2
	}
	if cmd == "note" {
		o.text = strings.Join(pos, " ")
	} else if len(pos) > 0 {
		fmt.Fprintf(os.Stderr, "[migrate] 알 수 없는 인자: %v\n", pos)
		return 2
	}
	if cmd != "beetle-selftest" && o.casePath == "" {
		fmt.Fprintln(os.Stderr, "[migrate] --case 가 필요합니다")
		return 2
	}
	for _, s := range []string{o.from, o.to} {
		if s != "" && !isStage(s) {
			fmt.Fprintf(os.Stderr, "[migrate] 모르는 단계: %s (%s)\n", s, strings.Join(config.Stages, " · "))
			return 2
		}
	}
	var rc int
	switch {
	case cmd == "run" || isStage(cmd):
		rc, err = cmdRun(o)
	case cmd == "status":
		rc, err = cmdStatus(o)
	case cmd == "note":
		rc, err = cmdNote(o)
	case cmd == "summary":
		rc, err = cmdSummary(o)
	case cmd == "beetle-selftest":
		rc, err = cmdSelftest(o)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "[migrate] 오류: %v\n", err)
		return 2
	}
	return rc
}

func openOrCreate(o opts, c *config.Case, ad *config.Adapter, s *config.Settings) (*state.RunState, error) {
	runID := o.runID
	if runID == "" {
		runID = o.resume
	}
	if runID == "latest" {
		runID = state.LatestRunID(s.EvidenceRoot, c.Name)
		if runID == "" {
			return nil, fmt.Errorf("%s 의 run 이 없습니다", c.Name)
		}
	}
	if runID != "" {
		st, err := state.Open(s.EvidenceRoot, c.Name, runID)
		if err != nil {
			return nil, err
		}
		if st.Data.DryRun && !o.dryRun {
			return nil, fmt.Errorf("run %s 은 dry-run 이었습니다 — 실 운행은 새 run 으로 시작하세요", runID)
		}
		return st, nil
	}
	return state.Create(s.EvidenceRoot, c.Name, c.Path, ad.S("workload")+"-"+ad.S("form"), o.dryRun)
}

func plan(o opts, st *state.RunState) ([]string, error) {
	if o.stage != "" {
		if st.IsDone(o.stage) {
			st.Mark(o.stage, "pending", "", nil) // 명시한 단계는 done 이어도 다시 돈다
		}
		return []string{o.stage}, nil
	}
	i0, i1 := 0, len(config.Stages)-1
	for i, n := range config.Stages {
		if n == o.from {
			i0 = i
		}
		if n == o.to {
			i1 = i
		}
	}
	if i1 < i0 {
		return nil, config.Errf("--to 가 --from 보다 앞입니다")
	}
	names := config.Stages[i0 : i1+1]
	if o.from != "" {
		for _, n := range names {
			if st.IsDone(n) {
				st.Mark(n, "pending", "", nil)
			}
		}
	}
	return names, nil
}

var (
	curMu    sync.Mutex
	curStage string
)

func setCurrent(s string) {
	curMu.Lock()
	curStage = s
	curMu.Unlock()
}

func runStage(ctx *stages.Context, name string) (res jsonx.M, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("예상 밖 오류: %v\n%s", r, debug.Stack())
		}
	}()
	st, ok := stages.Registry[name]
	if !ok {
		return nil, fmt.Errorf("모르는 단계: %s", name)
	}
	if err := st.Preflight(ctx); err != nil {
		return nil, err
	}
	return st.Run(ctx)
}

func execute(ctx *stages.Context, names []string) int {
	for _, name := range names {
		if ctx.State.IsDone(name) {
			ctx.Log.Info(name, "%s: 이미 완료 — 건너뜀", name)
			continue
		}
		ctx.Log.Info(name, "=== %s ===", name)
		ctx.State.Mark(name, "running", "", nil)
		setCurrent(name)
		res, err := runStage(ctx, name)
		setCurrent("")
		if err != nil {
			ctx.State.Mark(name, "failed", err.Error(), nil)
			ctx.Log.Error(name, "%s 실패: %v", name, err)
			ctx.Log.Error(name, "고친 뒤 다시: migrate run --case %s --resume %s  (개입 기록: migrate note --case … \"…\")", ctx.Case.Path, ctx.State.Data.RunID)
			return 2
		}
		var result any
		if res != nil {
			result = res
		}
		ctx.State.Mark(name, "done", "", result)
		if name == "verify" {
			if r := ctx.State.VerdictResult(); r == "FAIL" || r == "NO-RESULT" {
				ctx.Log.Warn(name, "판정이 %s 라 cleanup 뒤 종료 코드 1 로 끝납니다", r)
			}
		}
	}
	if r := ctx.State.VerdictResult(); r == "FAIL" || r == "NO-RESULT" {
		return 1
	}
	return 0
}

func cmdRun(o opts) (int, error) {
	s, err := config.LoadSettings(o.settings)
	if err != nil {
		return 2, err
	}
	c, err := config.LoadCase(o.casePath)
	if err != nil {
		return 2, err
	}
	ad, err := config.LoadAdapter(jsonx.S(c.Raw, "workload"))
	if err != nil {
		return 2, err
	}
	if s.IsExample {
		logx.Console().Warn("", "settings.yaml 이 없어 settings.example.yaml 을 씁니다 — 실 운행 전에 복사해 채우세요")
	}
	st, err := openOrCreate(o, c, ad, s)
	if err != nil {
		return 2, err
	}
	log, err := logx.New(st.RunDir, !o.quiet)
	if err != nil {
		return 2, err
	}
	defer log.Close()
	if err := st.Lock(); err != nil {
		return 2, err
	}
	defer st.Unlock()
	ctx, err := stages.NewContext(c, ad, s, st, log, o.dryRun)
	if err != nil {
		return 2, err
	}
	names, err := plan(o, st)
	if err != nil {
		return 2, err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		curMu.Lock()
		name := curStage
		curMu.Unlock()
		if name != "" {
			st.Mark(name, "failed", "중단(Ctrl-C) — run --resume 로 이어 갈 수 있다", nil)
			log.Warn(name, "%s 중단 — 다시: migrate run --case %s --resume %s", name, c.Path, st.Data.RunID)
		}
		st.Unlock()
		os.Exit(2)
	}()
	dry := ""
	if o.dryRun {
		dry = " · DRY-RUN"
	}
	log.Info("", "run %s · 사례 %s · 단계 %s%s", st.Data.RunID, c.Name, strings.Join(names, " → "), dry)
	log.Info("", "증적: %s", st.RunDir)
	rc := execute(ctx, names)
	// 요약은 단계 상태가 모두 적힌 뒤에 쓴다 — 실패로 멈춘 run 도 그 시점의 요약이 남는다
	if err := stages.WriteSummary(ctx); err != nil {
		log.Warn("", "summary.md 를 쓰지 못했습니다: %v", err)
	}
	for _, l := range st.SummaryLines() {
		fmt.Println(l)
	}
	return rc, nil
}

func cmdStatus(o opts) (int, error) {
	s, err := config.LoadSettings(o.settings)
	if err != nil {
		return 2, err
	}
	c, err := config.LoadCase(o.casePath)
	if err != nil {
		return 2, err
	}
	src := c.Source()
	ep, err := s.Tumblebug(jsonx.S(src, "tumblebug"))
	if err != nil {
		return 2, err
	}
	tb := tumblebug.New(ep, logx.Console(), s.Pacing, false)
	infra, err := tb.GetInfra(jsonx.S(src, "ns"), jsonx.S(src, "infra"))
	if err != nil {
		return 2, err
	}
	if infra == nil {
		fmt.Printf("소스 인프라 없음: %s/%s\n", jsonx.S(src, "ns"), jsonx.S(src, "infra"))
	} else {
		fmt.Printf("소스 %s: %s\n", jsonx.S(src, "infra"), jsonx.S(infra, "status"))
		for _, n := range tumblebug.Nodes(infra) {
			fmt.Printf("  노드 %s %s ip=%s spec=%s image=%s csp=%s\n", jsonx.S(n, "nodeId"), jsonx.S(n, "status"), jsonx.S(n, "publicIP"),
				jsonx.S(n, "specId"), jsonx.S(n, "imageId"), jsonx.S(n, "cspResourceId"))
		}
	}
	if rid := state.LatestRunID(s.EvidenceRoot, c.Name); rid != "" {
		st, err := state.Open(s.EvidenceRoot, c.Name, rid)
		if err != nil {
			return 2, err
		}
		fmt.Println("최근 run:")
		for _, l := range st.SummaryLines() {
			fmt.Println("  " + l)
		}
	} else {
		fmt.Println("run 없음")
	}
	return 0, nil
}

func cmdNote(o opts) (int, error) {
	if strings.TrimSpace(o.text) == "" {
		return 2, errors.New("기록할 내용이 필요합니다: migrate note --case … \"사람이 한 조치\"")
	}
	s, err := config.LoadSettings(o.settings)
	if err != nil {
		return 2, err
	}
	c, err := config.LoadCase(o.casePath)
	if err != nil {
		return 2, err
	}
	rid := o.runID
	if rid == "" || rid == "latest" {
		rid = state.LatestRunID(s.EvidenceRoot, c.Name)
	}
	if rid == "" {
		return 2, fmt.Errorf("%s 의 run 이 없습니다", c.Name)
	}
	st, err := state.Open(s.EvidenceRoot, c.Name, rid)
	if err != nil {
		return 2, err
	}
	st.Note(o.text, o.noteStage, "")
	fmt.Printf("기록됨: run %s — %s\n", rid, o.text)
	if err := stages.Summarize(c, st); err != nil {
		fmt.Fprintln(os.Stderr, "summary.md 를 다시 쓰지 못했습니다:", err)
	}
	return 0, nil
}

// cmdSummary 는 state.json 으로 summary.md 만 다시 쓴다 (원격 호출 없음).
func cmdSummary(o opts) (int, error) {
	s, err := config.LoadSettings(o.settings)
	if err != nil {
		return 2, err
	}
	c, err := config.LoadCase(o.casePath)
	if err != nil {
		return 2, err
	}
	rid := o.runID
	if rid == "" || rid == "latest" {
		rid = state.LatestRunID(s.EvidenceRoot, c.Name)
	}
	if rid == "" {
		return 2, fmt.Errorf("%s 의 run 이 없습니다", c.Name)
	}
	st, err := state.Open(s.EvidenceRoot, c.Name, rid)
	if err != nil {
		return 2, err
	}
	if err := stages.Summarize(c, st); err != nil {
		return 2, err
	}
	fmt.Printf("다시 씀: %s\n", filepath.Join(st.RunDir, "summary.md"))
	return 0, nil
}

func cmdSelftest(o opts) (int, error) {
	s, err := config.LoadSettings(o.settings)
	if err != nil {
		return 2, err
	}
	if s.Beetle == nil {
		return 2, config.Errf("settings 에 beetle 이 없습니다")
	}
	log := logx.Console()
	b := beetle.New(*s.Beetle, log, false)
	bundle, err := b.EncryptionKey()
	if err != nil {
		return 2, err
	}
	log.Info("", "encryptionKey: keyId %s · %s · 만료 %s", jsonx.S(bundle, "keyId"), jsonx.S(bundle, "algorithm"), jsonx.S(bundle, "expiresAt"))
	enc, err := crypto.New(bundle)
	if err != nil {
		return 2, err
	}
	e1, err := enc.Encrypt(crypto.SelftestSamples[0])
	if err != nil {
		return 2, err
	}
	e2, err := enc.Encrypt(crypto.SelftestSamples[1])
	if err != nil {
		return 2, err
	}
	model := crypto.SampleModel(e1, e2)
	model["encryptionKeyId"] = enc.KeyID
	dec, err := b.TestDecrypt(model)
	if err != nil {
		return 2, err
	}
	g1, g2 := crypto.DecryptedKeys(dec)
	if g1 == crypto.SelftestSamples[0] && g2 == crypto.SelftestSamples[1] {
		log.Info("", "우리 봉투를 서버가 풀었습니다 — source · destination privateKey 모두 일치")
		return 0, nil
	}
	raw, _ := jsonx.Marshal(dec, false)
	log.Warn("", "복호 결과가 다릅니다: %q · %q (기대 %q · %q) — 응답: %.300s", g1, g2, crypto.SelftestSamples[0], crypto.SelftestSamples[1], string(raw))
	return 1, nil
}
