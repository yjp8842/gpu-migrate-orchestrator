// Package api 는 Facade API 다 — 화면(web/)이 쓰는 HTTP API 와 화면 파일을 한 서버(migrate serve)로 낸다.
//
// 이관 자체는 하지 않는다: 고른 값으로 사례 파일을 쓰고, migrate 를 별도 프로세스로 띄우고, 엔진이 남긴 run 폴더를 읽어 화면용으로 정리한다.
// Tumblebug 은 선택지(소스 · 리전) 조회에만, Beetle 은 타겟 삭제에만 직접 부른다. 계정은 서버 안에서만 쓰고 응답에 싣지 않는다.
//
//	GET  /api/info                           열 포트 출발지 · 설정 파일 이름 (비밀값 없음)
//	GET  /api/workloads                      워크로드 목록
//	GET  /api/namespaces                     Tumblebug 네임스페이스 (default 가 맨 앞)
//	GET  /api/sources?ns=                    그 네임스페이스의 Tumblebug 인프라 (소스 후보, 기본 default)
//	GET  /api/regions                        CSP 별 리전 · 존 (Tumblebug 연결)
//	POST /api/preview                        사례 파일 만들기 + dry-run → run-id   (화면의 「사양 추천 받기」)
//	GET  /api/cases/{case}                   고른 값 + 소스의 지금 상태 (사례 원문은 내보내지 않음)
//	POST /api/migrate                        이관 시작 (소스가 Running 일 때만, 기본 --to verify — 정리 앞에서 멈춤) → run-id
//	GET  /api/runs                           run 목록 (?dry=1 이면 dry-run 포함)
//	GET  /api/runs/{case}/{run}              상세 (단계 · 판정 · 추천 · 비교)
//	GET  /api/runs/{case}/{run}/log          로그 (?offset=)
//	GET  /api/runs/{case}/{run}/target       타겟의 지금 상태 · 공인 IP (Tumblebug, 「타겟 화면 열기」용)
//	GET  /api/runs/{case}/{run}/files        증적 파일 목록,  /file?path=…  파일 하나,  /zip  증적 ZIP
//	POST /api/runs/{case}/{run}/note         사람 개입 기록
//	POST /api/runs/{case}/{run}/stop         실행 중지
//	POST /api/runs/{case}/{run}/cleanup      정리 단계 (사례의 cleanup)
//	POST /api/runs/{case}/{run}/delete-target  Beetle 로 만든 타겟 삭제 (인프라 이름을 그대로 보내야 함)
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"gpu-migrate-orchestrator/internal/config"
	"gpu-migrate-orchestrator/internal/jsonx"
	"gpu-migrate-orchestrator/internal/tumblebug"
)

// Server 는 Facade API 서버다.
type Server struct {
	Root         string // 오케스트레이터 폴더 (adapters/ · cases/ · settings.yaml)
	SettingsPath string // 비면 Root 의 settings.yaml (없으면 settings.example.yaml)
	Exe          string // 하위 프로세스로 띄울 migrate — 보통 자기 자신
	Web          fs.FS  // 화면 파일 (web/dist). nil 이면 API 만
	MaxRuns      int    // 동시에 도는 이관 run 상한 (기본 2) — 같은 Tumblebug 호출 제한을 나눠 쓴다

	cache     *cache
	previewMu sync.Mutex // 사전 확인(dry-run)은 한 번에 하나
	tbOnce    sync.Once
	tb        *tumblebug.Client
	tbErr     error
}

func (s *Server) settings() (*config.Settings, error) { return config.LoadSettings(s.SettingsPath) }

func (s *Server) evidence() (string, error) {
	st, err := s.settings()
	if err != nil {
		return "", err
	}
	return st.EvidenceRoot, nil
}

type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

func fail(code int, format string, a ...any) error {
	return &httpError{code: code, msg: fmt.Sprintf(format, a...)}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// handle 은 (값, 오류) 를 JSON 으로 — 오류는 {error} 와 400(또는 지정한 코드).
func handle(fn func(r *http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := fn(r)
		if err != nil {
			code := http.StatusBadRequest
			var he *httpError
			if errors.As(err, &he) {
				code = he.code
			}
			writeJSON(w, code, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, v)
	}
}

func decode(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return fail(http.StatusBadRequest, "요청 본문을 읽지 못했습니다: %v", err)
	}
	return nil
}

func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// guard 는 다른 사이트가 브라우저를 통해 API 를 부르는 것을 막는다.
//   - Host 는 루프백 이름이어야 한다 (DNS rebinding)
//   - GET 이 아닌 요청은 JSON 이어야 하고, Origin 이 있으면 같은 Host 여야 한다 (CSRF)
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if !isLoopbackHost(host) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "허용되지 않은 Host: " + r.Host})
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
				writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "JSON 요청만 받습니다"})
				return
			}
			if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host && o != "https://"+r.Host {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "다른 출처의 요청입니다: " + o})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Handler 는 API 와 화면 파일을 낸다.
func (s *Server) Handler() http.Handler {
	if s.cache == nil {
		s.cache = &cache{m: map[string]cached{}}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/info", handle(func(r *http.Request) (any, error) {
		st, err := s.settings()
		if err != nil {
			return nil, err
		}
		return map[string]string{"settings": filepath.Base(st.Path), "webAllowCidr": os.Getenv("GMP_WEB_ALLOW_CIDR")}, nil
	}))
	mux.HandleFunc("GET /api/workloads", handle(func(r *http.Request) (any, error) { return listWorkloads(s.Root), nil }))
	mux.HandleFunc("GET /api/namespaces", handle(func(r *http.Request) (any, error) { return s.listNamespaces() }))
	mux.HandleFunc("GET /api/sources", handle(func(r *http.Request) (any, error) {
		ns := r.URL.Query().Get("ns")
		if ns == "" {
			ns = "default"
		}
		return s.listSources(ns)
	}))
	mux.HandleFunc("GET /api/regions", handle(func(r *http.Request) (any, error) { return s.listRegions() }))

	// 「사양 추천 받기」 — 사례 파일을 쓰고 dry-run 으로 추천까지 (자원은 만들지 않는다)
	mux.HandleFunc("POST /api/preview", handle(func(r *http.Request) (any, error) {
		if !s.previewMu.TryLock() {
			return nil, fail(http.StatusTooManyRequests, "다른 사전 확인이 도는 중입니다 — 끝난 뒤 다시")
		}
		defer s.previewMu.Unlock()
		var in caseInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		name, rel, err := writeCase(s.Root, in)
		if err != nil {
			return nil, err
		}
		res, err := s.startMigrate([]string{"run", "--case", rel, "--dry-run", "--to", "provision"}, true, 5*time.Minute)
		if err != nil {
			return nil, err
		}
		if res.RunID == "" {
			return nil, fmt.Errorf("dry-run 이 run 을 만들지 못했습니다: %s", tail(res.output, 6))
		}
		return map[string]any{"caseName": name, "runId": res.RunID, "exitCode": res.ExitCode}, nil
	}))
	mux.HandleFunc("GET /api/cases/{case}", handle(func(r *http.Request) (any, error) {
		_, plan, ok, err := readCase(s.Root, r.PathValue("case"))
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fail(http.StatusNotFound, "사례 파일이 없습니다")
		}
		// 소스 상태는 확인 화면이 Migrate 버튼을 막는 데 쓴다 — 읽지 못하면 이유를 같이 준다
		out := map[string]any{"caseName": r.PathValue("case"), "plan": plan}
		if st, err := s.sourceStatus(plan.NS, plan.SourceInfra); err != nil {
			out["sourceStatusErr"] = err.Error()
		} else {
			out["sourceStatus"] = st
		}
		return out, nil
	}))
	// 이관 시작 — stopBeforeCleanup(기본 true)이면 판정까지만 돌고 정리 앞에서 멈춘다
	mux.HandleFunc("POST /api/migrate", handle(func(r *http.Request) (any, error) {
		var in struct {
			CaseName          string `json:"caseName"`
			StopBeforeCleanup *bool  `json:"stopBeforeCleanup"`
		}
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		rel, plan, ok, err := readCase(s.Root, in.CaseName)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errors.New("사례 파일이 없습니다 — 「새 이관」부터")
		}
		// 소스가 Running 이 아니면 패키지 단계의 원격 명령이 닿지 않는다 — 타겟을 만들기 전에 막는다
		st, err := s.sourceStatus(plan.NS, plan.SourceInfra)
		if err != nil {
			return nil, fail(http.StatusBadGateway, "소스 %s 의 상태를 확인하지 못했습니다: %v", plan.SourceInfra, err)
		}
		if st != "Running" {
			return nil, fail(http.StatusConflict, "소스 %s 가 %s 입니다 — MapUI 에서 Resume 해 Running 이 된 뒤 Migrate 합니다", plan.SourceInfra, st)
		}
		ev, err := s.evidence()
		if err != nil {
			return nil, err
		}
		live := 0
		for _, run := range listRuns(ev, false) {
			if run["status"].(M)["key"] == "running" {
				live++
			}
		}
		if limit := max(1, s.MaxRuns); live >= limit {
			return nil, fail(http.StatusTooManyRequests, "이미 %d개 run 이 도는 중입니다 (동시 상한 %d) — 끝난 뒤 다시", live, limit)
		}
		args := []string{"run", "--case", rel}
		if in.StopBeforeCleanup == nil || *in.StopBeforeCleanup {
			args = append(args, "--to", "verify")
		}
		res, err := s.startMigrate(args, false, time.Minute)
		if err != nil {
			return nil, err
		}
		return map[string]string{"caseName": res.CaseName, "runId": res.RunID}, nil
	}))

	mux.HandleFunc("GET /api/runs", handle(func(r *http.Request) (any, error) {
		ev, err := s.evidence()
		if err != nil {
			return nil, err
		}
		return listRuns(ev, r.URL.Query().Get("dry") == "1"), nil
	}))
	mux.HandleFunc("GET /api/runs/{case}/{run}", handle(func(r *http.Request) (any, error) {
		d, err := s.detail(r)
		if err != nil {
			return nil, err
		}
		return d, nil
	}))
	mux.HandleFunc("GET /api/runs/{case}/{run}/log", handle(func(r *http.Request) (any, error) {
		ev, err := s.evidence()
		if err != nil {
			return nil, err
		}
		off, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		l, err := readLog(ev, r.PathValue("case"), r.PathValue("run"), off)
		if err == nil {
			l["text"] = string(maskContent("log.txt", []byte(l["text"].(string))))
		}
		return l, err
	}))
	mux.HandleFunc("GET /api/runs/{case}/{run}/files", handle(func(r *http.Request) (any, error) {
		ev, err := s.evidence()
		if err != nil {
			return nil, err
		}
		return listFiles(ev, r.PathValue("case"), r.PathValue("run"))
	}))
	mux.HandleFunc("GET /api/runs/{case}/{run}/file", func(w http.ResponseWriter, r *http.Request) {
		ev, err := s.evidence()
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		p, err := filePath(ev, r.PathValue("case"), r.PathValue("run"), r.URL.Query().Get("path"))
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		st, err := os.Stat(p)
		if err != nil || st.Size() > maxFileBytes {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "50MB 보다 큰 파일은 ZIP 으로 받으세요"})
			return
		}
		b, err := os.ReadFile(p)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "파일이 없습니다"})
			return
		}
		if r.URL.Query().Get("download") == "1" {
			w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(p)}))
		}
		// 증적은 화면에서 글자로만 본다 — HTML 로 해석되지 않게 형식을 정하고, 열어도 스크립트가 돌지 않게 sandbox
		switch {
		case binaryExt[strings.ToLower(filepath.Ext(p))]:
			w.Header().Set("Content-Type", "application/octet-stream")
		case strings.HasSuffix(p, ".json") || strings.HasSuffix(p, ".jsonl"):
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
		default:
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		}
		w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(maskContent(p, b))
	})
	mux.HandleFunc("GET /api/runs/{case}/{run}/zip", func(w http.ResponseWriter, r *http.Request) {
		ev, err := s.evidence()
		var dir string
		if err == nil {
			dir, err = runDir(ev, r.PathValue("case"), r.PathValue("run"))
		}
		if err == nil {
			if _, statErr := os.Stat(dir); statErr != nil {
				err = fail(http.StatusNotFound, "run 이 없습니다")
			}
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		name := r.PathValue("case") + "-" + r.PathValue("run") + ".zip"
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
		w.Header().Set("Cache-Control", "no-store")
		writeZip(w, dir, r.PathValue("case")+"/"+r.PathValue("run"))
	})

	mux.HandleFunc("POST /api/runs/{case}/{run}/note", handle(func(r *http.Request) (any, error) {
		d, err := s.detail(r)
		if err != nil {
			return nil, err
		}
		var in struct{ Text, Stage string }
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		text, stage := strings.TrimSpace(in.Text), strings.TrimSpace(in.Stage)
		switch {
		case text == "":
			return nil, errors.New("기록할 내용이 비어 있습니다")
		case utf8.RuneCountInString(text) > 2000:
			return nil, errors.New("기록은 2000자 이하로")
		case strings.HasPrefix(text, "-"):
			return nil, errors.New("기록은 - 로 시작할 수 없습니다")
		case stage != "" && !slices.Contains(config.Stages, stage):
			return nil, fmt.Errorf("모르는 단계: %s", stage)
		}
		return map[string]bool{"ok": true}, s.note(d, r.PathValue("run"), stage, text)
	}))
	mux.HandleFunc("POST /api/runs/{case}/{run}/stop", handle(func(r *http.Request) (any, error) {
		pid, err := s.lockedPID(r)
		if err != nil {
			return nil, err
		}
		return map[string]bool{"stopped": stopRun(pid)}, nil
	}))
	mux.HandleFunc("POST /api/runs/{case}/{run}/cleanup", handle(func(r *http.Request) (any, error) {
		d, err := s.detail(r)
		if err != nil {
			return nil, err
		}
		if pid, _ := s.lockedPID(r); pid > 0 {
			return nil, errors.New("아직 실행 중입니다")
		}
		casePath, err := s.casePath(d)
		if err != nil {
			return nil, err
		}
		if _, err := s.startMigrate([]string{"cleanup", "--case", casePath, "--run", r.PathValue("run")}, false, time.Minute); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, nil
	}))
	// 타겟의 지금 상태 · 공인 IP — 아직 만들지 않았으면 status 가 null
	mux.HandleFunc("GET /api/runs/{case}/{run}/target", handle(func(r *http.Request) (any, error) {
		d, err := s.detail(r)
		if err != nil {
			return nil, err
		}
		infra, _ := d["target"].(M)["infra"].(string)
		if infra == "" {
			return M{"status": nil, "publicIP": nil}, nil
		}
		ns, err := s.targetNS(d)
		if err != nil {
			return nil, err
		}
		live, err := s.targetLive(ns, infra)
		if err != nil {
			return nil, fail(http.StatusBadGateway, "타겟 %s 의 지금 상태를 확인하지 못했습니다: %v", infra, err)
		}
		return live, nil
	}))
	mux.HandleFunc("POST /api/runs/{case}/{run}/delete-target", handle(func(r *http.Request) (any, error) {
		d, err := s.detail(r)
		if err != nil {
			return nil, err
		}
		if pid, _ := s.lockedPID(r); pid > 0 {
			return nil, errors.New("아직 실행 중입니다 — 중지한 뒤 지우세요")
		}
		var in struct{ Confirm string }
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		infra, _ := d["target"].(M)["infra"].(string)
		if infra == "" {
			return nil, errors.New("타겟 인프라가 없습니다")
		}
		if !idRe.MatchString(infra) {
			return nil, errors.New("타겟 인프라 이름이 이상합니다")
		}
		if in.Confirm != infra {
			return nil, errors.New("확인용으로 타겟 인프라 이름을 그대로 보내야 합니다")
		}
		ns, err := s.targetNS(d)
		if err != nil {
			return nil, err
		}
		res, err := s.deleteTarget(ns, infra)
		if err != nil {
			return nil, err
		}
		s.note(d, r.PathValue("run"), "cleanup", "타겟 "+infra+" 를 콘솔에서 Beetle 삭제 API 로 삭제 (vNet · 보안 그룹 · 키 포함)")
		return map[string]any{"ok": true, "result": res}, nil
	}))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "없는 API: " + r.Method + " " + r.URL.Path})
	})

	if s.Web != nil {
		mux.Handle("/", spa(s.Web))
	}
	return headers(s.guard(mux))
}

func (s *Server) detail(r *http.Request) (M, error) {
	ev, err := s.evidence()
	if err != nil {
		return nil, err
	}
	d, err := runDetail(ev, r.PathValue("case"), r.PathValue("run"))
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, fail(http.StatusNotFound, "run 이 없습니다")
	}
	return d, nil
}

// lockedPID 는 이 run 을 잡고 있는 migrate 의 pid (없으면 0) — 신호를 보내기 전에 쓴다.
func (s *Server) lockedPID(r *http.Request) (int, error) {
	ev, err := s.evidence()
	if err != nil {
		return 0, err
	}
	dir, err := runDir(ev, r.PathValue("case"), r.PathValue("run"))
	if err != nil {
		return 0, err
	}
	return lockedPID(dir), nil
}

// casePath 는 state.json 의 사례 경로 — 저장소 안의 cases/ 아래 파일만 migrate 에 넘긴다.
func (s *Server) casePath(d M) (string, error) {
	p, _ := d["casePath"].(string)
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(s.Root, p)
	}
	rel, err := filepath.Rel(filepath.Join(s.Root, "cases"), filepath.Clean(abs))
	if p == "" || err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || !strings.HasSuffix(p, ".yaml") {
		return "", fmt.Errorf("사례 경로가 cases/ 밖입니다: %q", p)
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("사례 파일이 없습니다: %s", p)
	}
	return abs, nil
}

// targetNS 는 사례의 타겟 네임스페이스 (비면 소스와 같음) — Beetle 삭제에 쓴다.
func (s *Server) targetNS(d M) (string, error) {
	p, err := s.casePath(d)
	if err != nil {
		return "", err
	}
	c, err := config.LoadCase(p)
	if err != nil {
		return "", err
	}
	ns := jsonx.S(c.Target(), "ns")
	if !nsRe.MatchString(ns) {
		return "", fmt.Errorf("사례의 타겟 네임스페이스가 이상합니다: %q", ns)
	}
	return ns, nil
}

func (s *Server) note(d M, runID, stage, text string) error {
	casePath, err := s.casePath(d)
	if err != nil {
		return err
	}
	args := []string{"note", "--case", casePath, "--run", runID}
	if stage != "" {
		args = append(args, "--stage", stage)
	}
	_, err = s.startMigrate(append(args, text), true, 30*time.Second)
	return err
}

// spa 는 화면 파일을 내고, 없는 경로는 index.html 로 (화면 쪽 라우터가 처리).
func spa(web fs.FS) http.Handler {
	files := http.FileServerFS(web)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" {
			p = "index.html"
		}
		if st, err := fs.Stat(web, p); err != nil || st.IsDir() {
			w.Header().Set("Cache-Control", "no-store")
			http.ServeFileFS(w, r, web, "index.html")
			return
		}
		if strings.HasPrefix(p, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable") // 파일 이름에 내용 해시가 붙는다
		}
		files.ServeHTTP(w, r)
	})
}

// ListenAndServe 는 루프백 주소에만 붙는다 — 로그인이 없는 로컬 도구라 다른 PC 에서는 SSH 터널로 연다.
func (s *Server) ListenAndServe(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if !isLoopbackHost(host) {
		return fmt.Errorf("serve 는 루프백(127.0.0.1 · localhost · ::1)에만 붙습니다: %s — 다른 PC 에서는 ssh -L 5180:127.0.0.1:5180 으로 여세요", addr)
	}
	srv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: time.Minute, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}
	return srv.ListenAndServe()
}

const maxFileBytes = 50 << 20
