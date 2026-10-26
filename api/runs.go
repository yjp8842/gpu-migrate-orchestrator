package api

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gpu-migrate-orchestrator/internal/config"
	"gpu-migrate-orchestrator/internal/jsonx"
)

// run 폴더 읽기 — 목록 · 상세(단계 · 판정 · 개입) · 로그 · 증적 파일 · 비교 결과. 엔진이 쓴 파일을 그대로 읽기만 한다.

type M = jsonx.M

var (
	safeRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	runIDRe = regexp.MustCompile(`^\d{8}-\d{6}$`)
	testRe  = regexp.MustCompile(`^(T\d(?:-\d)?)\s+(PASS|FAIL|WARN|SKIP)\s+(\d+)\s+(\d+)\s+(\d+)\s+(\d+)\s+(\d+)\s+(.+)$`)
	qRe     = regexp.MustCompile(`^q\d+$`)
)

func runDir(evidence, caseName, runID string) (string, error) {
	if !safeRe.MatchString(caseName) || !runIDRe.MatchString(runID) {
		return "", errors.New("run 경로가 이상합니다")
	}
	return filepath.Join(evidence, caseName, runID), nil
}

func readJSON(p string) M {
	v, err := jsonx.ReadFile(p)
	if err != nil {
		return nil
	}
	return jsonx.AsM(v)
}

// readText 는 파일 끝에서 최대 max 바이트를 읽는다. 없으면 nil.
func readText(p string, max int64) *string {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil
	}
	n := min(st.Size(), max)
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, st.Size()-n); err != nil && err != io.EOF {
		return nil
	}
	s := string(buf)
	return &s
}

func parseTime(s string) (time.Time, bool) {
	for _, l := range []string{"2006-01-02T15:04:05-0700", time.RFC3339, "2006-01-02T15:04:05"} {
		if t, err := time.Parse(l, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func secs(a, b string) any {
	ta, ok1 := parseTime(a)
	tb, ok2 := parseTime(b)
	if !ok1 || !ok2 {
		return nil
	}
	return max(0, int(math.Round(tb.Sub(ta).Seconds())))
}

// runPID 는 이 run 을 잡고 있는 migrate 의 pid 다 — .lock 에 적힌 pid 가 살아 있고 도는 단계가 있을 때만.
// 콘솔이 띄웠든 터미널에서 띄웠든, 콘솔을 다시 켰든 같은 방법으로 안다.
func runPID(dir string, state M) int {
	running := false
	for _, s := range config.Stages {
		if jsonx.S(jsonx.AsM(jsonx.AsM(state["stages"])[s]), "status") == "running" {
			running = true
		}
	}
	if !running {
		return 0
	}
	b, err := os.ReadFile(filepath.Join(dir, ".lock"))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0
	}
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return 0
	}
	return pid
}

func label(key, text string) M { return M{"key": key, "label": text} }

// overallStatus 는 run 하나의 상태 — 진행 중 · 판정 · 중단 · 미완.
func overallStatus(state M, live bool) M {
	st := jsonx.AsM(state["stages"])
	status := func(s string) string { return jsonx.S(jsonx.AsM(st[s]), "status") }
	if live {
		return label("running", "진행 중")
	}
	for _, s := range config.Stages {
		if status(s) == "failed" {
			return label("failed", "중단 ("+s+")")
		}
	}
	for _, s := range config.Stages {
		if status(s) == "running" {
			return label("stopped", "멈춤")
		}
	}
	switch v := jsonx.S(jsonx.AsM(state["verdict"]), "result"); v {
	case "PASS":
		return label("pass", "PASS")
	case "FAIL", "NO-RESULT":
		return label("fail", v)
	}
	return label("partial", "미완")
}

// publicIP 는 화면이 「타겟 화면 열기」 링크로 만드는 값이라 IP 꼴일 때만 낸다.
func publicIP(s string) any {
	if !validIP(s) {
		return nil
	}
	return s
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func stageList(state M) []M {
	st := jsonx.AsM(state["stages"])
	out := make([]M, 0, len(config.Stages))
	for _, name := range config.Stages {
		s := jsonx.AsM(st[name])
		started, ended := jsonx.S(s, "started"), jsonx.S(s, "ended")
		out = append(out, M{"name": name, "status": jsonx.SOr(s, "status", "pending"), "started": nilIfEmpty(started), "ended": nilIfEmpty(ended),
			"seconds": secs(started, ended), "error": nilIfEmpty(jsonx.S(s, "error"))})
	}
	return out
}

func summarize(caseName, runID string, state M, live bool) M {
	stages := stageList(state)
	done := 0
	var current, pending, first, last M
	for _, s := range stages {
		switch s["status"] {
		case "done":
			done++
		case "running":
			if current == nil || current["status"] != "running" {
				current = s
			}
		case "failed":
			if current == nil {
				current = s
			}
		case "pending":
			if pending == nil {
				pending = s
			}
		}
		if first == nil && s["started"] != nil {
			first = s
		}
		if s["ended"] != nil {
			last = s
		}
	}
	stage := ""
	switch {
	case current != nil:
		stage = current["name"].(string)
	case done == len(stages):
		stage = "cleanup"
	case pending != nil:
		stage = pending["name"].(string)
	}
	started := jsonx.S(state, "created")
	if first != nil {
		started = first["started"].(string)
	}
	end := jsonx.S(state, "updated")
	if live {
		end = time.Now().Format("2006-01-02T15:04:05-0700")
	} else if last != nil {
		end = last["ended"].(string)
	}
	keys := jsonx.AsM(state["keys"])
	return M{
		"caseName": caseName, "runId": runID, "workload": jsonx.S(state, "workload"), "dryRun": jsonx.Truthy(state["dry_run"]),
		"status": overallStatus(state, live), "progress": int(math.Round(float64(done) / float64(len(stages)) * 100)), "stage": stage,
		"targetInfra": nilIfEmpty(jsonx.S(keys, "target_infra_id")), "zone": nilIfEmpty(jsonx.S(keys, "provision_zone")),
		"started": started, "elapsed": secs(started, end), "interventions": len(jsonx.AsList(state["interventions"])),
	}
}

func listRuns(evidence string, includeDry bool) []M {
	out := []M{}
	cases, err := os.ReadDir(evidence)
	if err != nil {
		return out
	}
	for _, c := range cases {
		if !c.IsDir() || !safeRe.MatchString(c.Name()) {
			continue
		}
		runs, _ := os.ReadDir(filepath.Join(evidence, c.Name()))
		for _, r := range runs {
			if !r.IsDir() || !runIDRe.MatchString(r.Name()) {
				continue
			}
			dir := filepath.Join(evidence, c.Name(), r.Name())
			state := readJSON(filepath.Join(dir, "state.json"))
			if state == nil || (jsonx.Truthy(state["dry_run"]) && !includeDry) {
				continue
			}
			out = append(out, summarize(c.Name(), r.Name(), state, runPID(dir, state) > 0))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["runId"].(string) > out[j]["runId"].(string) })
	return out
}

func sourceSummary(dir string) any {
	m := readJSON(filepath.Join(dir, "collect", "source-model.json"))
	nodes := jsonx.ListM(m["nodes"])
	if len(nodes) == 0 {
		return nil
	}
	n := nodes[0]
	meta := readJSON(filepath.Join(dir, "collect", "source-meta.json"))
	mn := M{}
	if l := jsonx.ListM(meta["nodes"]); len(l) > 0 {
		mn = l[0]
	}
	spec := ""
	if id := jsonx.S(mn, "specId"); id != "" {
		spec = id[strings.LastIndex(id, "+")+1:]
	}
	gpu := ""
	if cards := jsonx.ListM(n["gpuCards"]); len(cards) > 0 {
		gpu = fmt.Sprintf("%s × %d · %vGB", jsonx.S(cards[0], "model"), len(cards), cards[0]["memoryTotalGB"])
	}
	fw := []string{}
	for _, r := range jsonx.ListM(n["firewallTable"]) {
		if jsonx.S(r, "direction") != "inbound" {
			continue
		}
		s := jsonx.S(r, "dstPorts")
		if c := jsonx.S(r, "srcCIDR"); c != "" && c != "0.0.0.0/0" {
			s += " ← " + c
		}
		fw = append(fw, s)
	}
	return M{
		"infra": nilIfEmpty(jsonx.S(jsonx.AsM(meta["infra"]), "id")), "region": nilIfEmpty(jsonx.S(jsonx.AsM(mn["region"]), "region")), "spec": nilIfEmpty(spec),
		"machine": n["machineId"], "vcpu": jsonx.Get(n, "cpu", "cpus"), "memoryGB": jsonx.Get(n, "memory", "totalSize"),
		"disk": jsonx.Get(n, "rootDisk", "totalSize"), "os": jsonx.Get(n, "os", "prettyName"), "gpu": gpu, "firewall": fw,
	}
}

func recommendSummary(dir string) M {
	req := readJSON(filepath.Join(dir, "provision", "migrate-request.sent.json"))
	if req == nil {
		req = readJSON(filepath.Join(dir, "recommend", "migrate-request.json"))
	}
	var picked, ng, img M
	if l := jsonx.ListM(req["targetSpecList"]); len(l) > 0 {
		picked = l[0]
	}
	if l := jsonx.ListM(jsonx.AsM(req["targetInfra"])["nodeGroups"]); len(l) > 0 {
		ng = l[0]
	}
	if l := jsonx.ListM(req["targetOsImageList"]); len(l) > 0 {
		img = l[0]
	}
	specOf := func(s M) M {
		return M{"id": s["id"], "name": s["cspSpecName"], "gpu": s["acceleratorModel"], "vram": s["acceleratorMemoryGB"],
			"vcpu": s["vCPU"], "memoryGiB": s["memoryGiB"], "cost": s["costPerHour"]}
	}
	out := M{"rationale": nil, "region": nil, "spec": nil, "image": nil, "rootDisk": nil, "zone": nil, "ports": []string{}, "candidates": []M{}}
	if t := readText(filepath.Join(dir, "recommend", "rationale.md"), 2_000_000); t != nil {
		out["rationale"] = *t
	}
	switch {
	case picked != nil:
		out["spec"] = specOf(picked)
		if parts := strings.Split(jsonx.S(picked, "id"), "+"); len(parts) > 1 {
			out["region"] = parts[1]
		}
	case ng != nil:
		out["spec"] = M{"id": ng["specId"]}
	}
	if img != nil {
		name := jsonx.S(img, "osDistribution")
		if name == "" {
			name = jsonx.S(img, "name")
		}
		out["image"] = M{"id": img["id"], "os": img["osType"], "name": name, "gpuImage": jsonx.Truthy(img["isGPUImage"])}
	}
	if ng != nil {
		out["rootDisk"] = ng["rootDiskSize"]
	}
	if l := jsonx.ListM(jsonx.AsM(req["targetVNet"])["subnetInfoList"]); len(l) > 0 {
		out["zone"] = nilIfEmpty(jsonx.S(l[0], "zone"))
	}
	ports := []string{}
	for _, sg := range jsonx.ListM(req["targetSecurityGroupList"]) {
		for _, r := range jsonx.ListM(sg["firewallRules"]) {
			if strings.EqualFold(jsonx.S(r, "Direction"), "inbound") {
				ports = append(ports, jsonx.S(r, "Ports")+" ← "+jsonx.S(r, "CIDR"))
			}
		}
	}
	out["ports"] = ports
	cands := []M{}
	if v, err := jsonx.ReadFile(filepath.Join(dir, "recommend", "candidates.json")); err == nil {
		for i, c := range jsonx.ListM(v) {
			if i == 8 {
				break
			}
			s := M{}
			if l := jsonx.ListM(c["targetSpecList"]); len(l) > 0 {
				s = l[0]
			}
			x := specOf(s)
			x["picked"] = picked != nil && jsonx.S(s, "id") == jsonx.S(picked, "id")
			cands = append(cands, x)
		}
	}
	out["candidates"] = cands
	return out
}

// compareSummary 는 이관 동등성 결과 — verify/evidence 의 summary.txt(테스트별 줄)와 rows.tsv(골든 문항).
func compareSummary(dir string) M {
	ev := filepath.Join(dir, "verify", "evidence")
	tests, golden, warns, diffs := []M{}, []M{}, []string{}, []string{}
	var headline any
	if t := readText(filepath.Join(ev, "summary.txt"), 2_000_000); t != nil {
		lines := strings.Split(*t, "\n")
		headline = lines[0]
		for _, line := range lines {
			if m := testRe.FindStringSubmatch(line); m != nil {
				n := func(i int) int { v, _ := strconv.Atoi(m[i]); return v }
				tests = append(tests, M{"test": m[1], "result": m[2], "pass": n(3), "fail": n(4), "warn": n(5), "skip": n(6), "info": n(7), "item": strings.TrimSpace(m[8])})
			}
		}
	}
	if t := readText(filepath.Join(ev, "rows.tsv"), 2_000_000); t != nil {
		for _, line := range strings.Split(*t, "\n") {
			f := strings.Split(line, "\t")
			if len(f) < 6 {
				continue
			}
			if f[0] == "T2" && qRe.MatchString(f[1]) {
				golden = append(golden, M{"q": f[1], "source": f[2], "target": f[3], "result": f[4], "note": f[5]})
			} else if f[4] == "WARN" {
				warns = append(warns, fmt.Sprintf("%s %s — %s → %s", f[0], f[1], f[2], f[3]))
			}
		}
	}
	if es, err := os.ReadDir(filepath.Join(ev, "golden-diff")); err == nil {
		for _, e := range es {
			if strings.HasSuffix(e.Name(), ".diff") {
				diffs = append(diffs, e.Name())
			}
		}
		sort.Strings(diffs)
	}
	return M{"headline": headline, "tests": tests, "golden": golden, "warns": warns, "diffs": diffs}
}

func runDetail(evidence, caseName, runID string) (M, error) {
	dir, err := runDir(evidence, caseName, runID)
	if err != nil {
		return nil, err
	}
	state := readJSON(filepath.Join(dir, "state.json"))
	if state == nil {
		return nil, nil
	}
	keys := jsonx.AsM(state["keys"])
	tnode := M{}
	if l := jsonx.ListM(keys["target_nodes"]); len(l) > 0 {
		tnode = l[0]
	}
	orList := func(v any) any {
		if v == nil {
			return []any{}
		}
		return v
	}
	d := summarize(caseName, runID, state, runPID(dir, state) > 0)
	d["casePath"] = state["case_path"]
	d["stages"] = stageList(state)
	d["verdict"] = state["verdict"]
	d["interventions"] = orList(state["interventions"])
	d["target"] = M{"infra": nilIfEmpty(jsonx.S(keys, "target_infra_id")), "zone": nilIfEmpty(jsonx.S(keys, "provision_zone")),
		"zones": orList(keys["provision_zones"]), "tries": orList(keys["provision_zone_tries"]),
		"publicIP": publicIP(jsonx.S(tnode, "publicIP")), "user": nilIfEmpty(jsonx.S(keys, "target_user"))}
	d["package"] = M{"backupOut": nilIfEmpty(jsonx.S(keys, "backup_out"))}
	var verifyRemote any
	if t := readText(filepath.Join(dir, "transfer", "verify-remote.txt"), 2_000_000); t != nil {
		verifyRemote = *t
	}
	d["transfer"] = M{"verify": verifyRemote}
	d["source"] = sourceSummary(dir)
	d["recommend"] = recommendSummary(dir)
	d["compare"] = compareSummary(dir)
	return d, nil
}

// readLog 는 offset 부터 끝까지 (화면이 2초마다 이어 읽는다). 한 번에 최대 500KB.
func readLog(evidence, caseName, runID string, offset int64) (M, error) {
	dir, err := runDir(evidence, caseName, runID)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(dir, "log.txt"))
	if err != nil {
		return M{"text": "", "offset": 0}, nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	from := min(max(0, offset), size)
	if size-from > 512000 {
		from = size - 512000
	}
	buf := make([]byte, size-from)
	if _, err := f.ReadAt(buf, from); err != nil && err != io.EOF {
		return nil, err
	}
	return M{"text": string(buf), "offset": size}, nil
}

func listFiles(evidence, caseName, runID string) (M, error) {
	dir, err := runDir(evidence, caseName, runID)
	if err != nil {
		return nil, err
	}
	files := []M{}
	var total int64
	filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() || e.Name() == ".lock" {
			return nil
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		total += info.Size()
		files = append(files, M{"path": filepath.ToSlash(rel), "size": info.Size()})
		return nil
	})
	return M{"files": files, "total": total}, nil
}

// filePath 는 증적 안의 일반 파일 하나 — run 폴더 밖으로 나가는 경로 · 심볼릭 링크(링크를 따라가면 밖일 수 있다) · .lock 은 거절.
func filePath(evidence, caseName, runID, rel string) (string, error) {
	dir, err := runDir(evidence, caseName, runID)
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if !strings.HasPrefix(p, dir+string(filepath.Separator)) || filepath.Base(p) == ".lock" {
		return "", errors.New("경로가 run 폴더 밖입니다")
	}
	realDir, err1 := filepath.EvalSymlinks(dir)
	realP, err2 := filepath.EvalSymlinks(p)
	if err1 != nil || err2 != nil {
		return "", errors.New("파일이 없습니다")
	}
	if !strings.HasPrefix(realP, realDir+string(filepath.Separator)) {
		return "", errors.New("경로가 run 폴더 밖입니다")
	}
	st, err := os.Lstat(p)
	if err != nil || !st.Mode().IsRegular() {
		return "", errors.New("일반 파일이 아닙니다")
	}
	return p, nil
}

// writeZip 은 run 폴더 전체를 <case>/<run>/… 로 묶는다 (.lock 제외).
func writeZip(w io.Writer, dir, prefix string) error {
	zw := zip.NewWriter(w)
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() || e.Name() == ".lock" || !e.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		info, err := e.Info()
		if err != nil {
			return err
		}
		h, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		h.Name = prefix + "/" + filepath.ToSlash(rel)
		h.Method = zip.Deflate
		out, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		_, err = out.Write(maskContent(p, b)) // 지난 run 의 증적도 비밀값은 가려서 나간다
		return err
	})
	if err != nil {
		return err
	}
	return zw.Close()
}
