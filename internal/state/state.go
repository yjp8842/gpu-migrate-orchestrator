// Package state 는 run 폴더와 state.json — 단계 상태 · 재개용 키 · 산출물 · 수동 개입 기록 — 을 다룬다.
//
// 재개용 키(인프라 이름 · X-Request-Id · 패키지 출력 경로)는 부작용을 일으키기 전에 적는다.
// 그래야 중단 뒤 `run --resume` 이 같은 자원을 다시 만들지 않고 조회로 이어 간다.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"syscall"
	"time"

	"gpu-migrate-orchestrator/internal/config"
	"gpu-migrate-orchestrator/internal/jsonx"
	"gpu-migrate-orchestrator/internal/textx"
)

// Error 는 상태 오류다.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func errf(format string, a ...any) error { return &Error{Msg: fmt.Sprintf(format, a...)} }

func now() string { return time.Now().Format("2006-01-02T15:04:05-0700") }

// Stage 는 단계 하나의 상태다.
type Stage struct {
	Status    string            `json:"status"`
	Started   string            `json:"started,omitempty"`
	Ended     string            `json:"ended,omitempty"`
	Error     string            `json:"error,omitempty"`
	Result    any               `json:"result,omitempty"`
	Artifacts map[string]string `json:"artifacts,omitempty"`
}

// StageMap 은 단계 순서대로 JSON 에 쓴다.
type StageMap map[string]*Stage

// MarshalJSON 은 config.Stages 순서를 지킨다.
func (m StageMap) MarshalJSON() ([]byte, error) {
	buf := []byte{'{'}
	first := true
	write := func(k string, v *Stage) error {
		kb, _ := json.Marshal(k)
		vb, err := jsonx.Marshal(v, false)
		if err != nil {
			return err
		}
		if !first {
			buf = append(buf, ',')
		}
		first = false
		buf = append(append(append(buf, kb...), ':'), vb...)
		return nil
	}
	seen := map[string]bool{}
	for _, k := range config.Stages {
		if v, ok := m[k]; ok {
			if err := write(k, v); err != nil {
				return nil, err
			}
			seen[k] = true
		}
	}
	var rest []string
	for k := range m {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		if err := write(k, m[k]); err != nil {
			return nil, err
		}
	}
	return append(buf, '}'), nil
}

// Intervention 은 사람이 한 조치 한 건이다.
type Intervention struct {
	At    string  `json:"at"`
	Stage *string `json:"stage"`
	By    string  `json:"by"`
	Text  string  `json:"text"`
}

// Data 는 state.json 의 내용이다.
type Data struct {
	Case          string         `json:"case"`
	CasePath      string         `json:"case_path"`
	Workload      string         `json:"workload"`
	RunID         string         `json:"run_id"`
	DryRun        bool           `json:"dry_run"`
	Created       string         `json:"created"`
	Updated       string         `json:"updated"`
	Stages        StageMap       `json:"stages"`
	Keys          map[string]any `json:"keys"`
	Interventions []Intervention `json:"interventions"`
	Verdict       any            `json:"verdict"`
}

// RunState 는 run 폴더 하나다.
type RunState struct {
	RunDir string
	Path   string
	Data   *Data
	lock   *os.File
}

func newState(runDir string) *RunState {
	return &RunState{RunDir: runDir, Path: filepath.Join(runDir, "state.json")}
}

// Create 는 새 run 폴더와 state.json 을 만든다.
func Create(evidenceRoot, caseName, casePath, workload string, dryRun bool) (*RunState, error) {
	runID := time.Now().Format("20060102-150405")
	runDir := filepath.Join(evidenceRoot, caseName, runID)
	if _, err := os.Stat(runDir); err == nil {
		runID += "-" + strconv.Itoa(os.Getpid())
		runDir = filepath.Join(evidenceRoot, caseName, runID)
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return nil, err
	}
	st := newState(runDir)
	st.Data = &Data{Case: caseName, CasePath: casePath, Workload: workload, RunID: runID, DryRun: dryRun,
		Created: now(), Updated: now(), Stages: StageMap{}, Keys: map[string]any{}, Interventions: []Intervention{}}
	for _, s := range config.Stages {
		st.Data.Stages[s] = &Stage{Status: "pending"}
	}
	if err := st.Save(); err != nil {
		return nil, err
	}
	return st, nil
}

// Open 은 기존 run 을 연다.
func Open(evidenceRoot, caseName, runID string) (*RunState, error) {
	st := newState(filepath.Join(evidenceRoot, caseName, runID))
	if _, err := os.Stat(st.Path); err != nil {
		return nil, errf("run 을 찾지 못했습니다: %s", st.Path)
	}
	if err := st.Load(); err != nil {
		return nil, err
	}
	return st, nil
}

// LatestRunID 는 가장 최근 run 의 ID (없으면 "").
func LatestRunID(evidenceRoot, caseName string) string {
	entries, err := os.ReadDir(filepath.Join(evidenceRoot, caseName))
	if err != nil {
		return ""
	}
	var runs []string
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(evidenceRoot, caseName, e.Name(), "state.json")); err == nil {
			runs = append(runs, e.Name())
		}
	}
	if len(runs) == 0 {
		return ""
	}
	sort.Strings(runs)
	return runs[len(runs)-1]
}

// Load 는 state.json 을 읽는다.
func (s *RunState) Load() error {
	b, err := os.ReadFile(s.Path)
	if err != nil {
		return err
	}
	var d Data
	if err := jsonx.DecodeInto(b, &d); err != nil {
		return errf("%s 를 읽지 못했습니다: %v", s.Path, err)
	}
	if d.Keys == nil {
		d.Keys = map[string]any{}
	}
	if d.Stages == nil {
		d.Stages = StageMap{}
	}
	for _, n := range config.Stages {
		if d.Stages[n] == nil {
			d.Stages[n] = &Stage{Status: "pending"}
		}
	}
	if d.Interventions == nil {
		d.Interventions = []Intervention{}
	}
	s.Data = &d
	return nil
}

// Save 는 임시 파일에 쓴 뒤 바꿔치기한다.
func (s *RunState) Save() error {
	s.Data.Updated = now()
	b, err := jsonx.Marshal(s.Data, true)
	if err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path)
}

func (s *RunState) save() {
	if err := s.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "[migrate] state.json 저장 실패: %v\n", err)
	}
}

// Lock 은 같은 run 을 두 프로세스가 돌리지 못하게 잠근다.
func (s *RunState) Lock() error {
	f, err := os.OpenFile(filepath.Join(s.RunDir, ".lock"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return errf("다른 프로세스가 이 run 을 돌리고 있습니다: %s", s.RunDir)
	}
	f.WriteString(strconv.Itoa(os.Getpid()))
	s.lock = f
	return nil
}

// Unlock 은 잠금을 푼다.
func (s *RunState) Unlock() {
	if s.lock != nil {
		syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		s.lock.Close()
		s.lock = nil
	}
}

// Stage 는 단계 상태.
func (s *RunState) Stage(name string) *Stage {
	st := s.Data.Stages[name]
	if st == nil {
		st = &Stage{Status: "pending"}
		s.Data.Stages[name] = st
	}
	return st
}

// StageDir 은 단계 산출물 폴더 (없으면 만든다).
func (s *RunState) StageDir(name string) string {
	d := filepath.Join(s.RunDir, name)
	os.MkdirAll(d, 0o755)
	return d
}

// Mark 는 단계 상태를 바꾼다. errMsg · result 는 비어 있으면 그대로 둔다.
func (s *RunState) Mark(name, status, errMsg string, result any) {
	st := s.Stage(name)
	st.Status = status
	if status == "running" {
		st.Started = now()
		st.Error = ""
	}
	if status == "done" || status == "failed" || status == "skipped" {
		st.Ended = now()
	}
	if errMsg != "" {
		st.Error = errMsg
	}
	if result != nil {
		st.Result = jsonx.RoundTrip(result)
	}
	s.save()
}

// Artifact 는 단계 산출물 경로를 적는다.
func (s *RunState) Artifact(name, label, path string) {
	st := s.Stage(name)
	if st.Artifacts == nil {
		st.Artifacts = map[string]string{}
	}
	st.Artifacts[label] = path
	s.save()
}

// IsDone 은 단계가 끝났는지.
func (s *RunState) IsDone(name string) bool { return s.Stage(name).Status == "done" }

// SetKey 는 재개용 키를 적는다 (JSON 으로 한 번 오간 값으로 넣어, 다시 읽은 값과 타입이 같게).
func (s *RunState) SetKey(key string, v any) {
	s.Data.Keys[key] = jsonx.RoundTrip(v)
	s.save()
}

// DelKey 는 재개용 키를 지운다.
func (s *RunState) DelKey(key string) {
	delete(s.Data.Keys, key)
	s.save()
}

// Key 는 재개용 키 (없으면 nil).
func (s *RunState) Key(key string) any { return s.Data.Keys[key] }

// KeyS 는 키를 문자열로.
func (s *RunState) KeyS(key string) string { return jsonx.Str(s.Data.Keys[key]) }

// HasKey 는 키가 참 값으로 있는지 (파이썬의 `if state.get_key(k)`).
func (s *RunState) HasKey(key string) bool { return jsonx.Truthy(s.Data.Keys[key]) }

// Note 는 사람이 한 조치를 기록한다.
func (s *RunState) Note(text, stage, by string) {
	var sp *string
	if stage != "" {
		sp = &stage
	}
	if by == "" {
		by = "operator"
	}
	s.Data.Interventions = append(s.Data.Interventions, Intervention{At: now(), Stage: sp, By: by, Text: text})
	s.save()
}

// SetVerdict 는 판정을 적는다.
func (s *RunState) SetVerdict(v any) {
	s.Data.Verdict = jsonx.RoundTrip(v)
	s.save()
}

// VerdictResult 는 판정 결과 문자열 (없으면 "").
func (s *RunState) VerdictResult() string { return jsonx.S(jsonx.AsM(s.Data.Verdict), "result") }

// SummaryLines 는 콘솔용 요약이다.
func (s *RunState) SummaryLines() []string {
	d := s.Data
	head := fmt.Sprintf("run %s · 사례 %s · 워크로드 %s", d.RunID, d.Case, d.Workload)
	if d.DryRun {
		head += " · DRY-RUN"
	}
	lines := []string{head}
	for _, n := range config.Stages {
		st := s.Stage(n)
		extra := ""
		if st.Error != "" {
			extra = " — " + textx.Head(st.Error, 100)
		}
		lines = append(lines, fmt.Sprintf("  %-10s %-8s %-19s %-19s%s", n, st.Status, textx.Head(st.Started, 19), textx.Head(st.Ended, 19), extra))
	}
	if v := jsonx.AsM(d.Verdict); v != nil {
		lines = append(lines, fmt.Sprintf("  판정: %s (%s)", jsonx.S(v, "result"), jsonx.S(v, "summary")))
	}
	if len(d.Interventions) > 0 {
		lines = append(lines, fmt.Sprintf("  개입 %d건:", len(d.Interventions)))
		for _, n := range d.Interventions {
			st := "-"
			if n.Stage != nil && *n.Stage != "" {
				st = *n.Stage
			}
			lines = append(lines, fmt.Sprintf("    %s [%s] %s", textx.Head(n.At, 19), st, n.Text))
		}
	}
	return lines
}
