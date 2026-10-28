package stages

// 어댑터로 워크로드 차이를 흡수하는 항목들의 시험 — 전송 목록 · 기동 완료 판정 · 상태 파일 판정 · 기존 설치 · 정리.

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gpu-migrate-orchestrator/internal/config"
	"gpu-migrate-orchestrator/internal/jsonx"
)

// adapterWith 는 widgetrag-a 어댑터에 항목을 더하거나 바꾼 것이다.
func adapterWith(t *testing.T, extra M) *config.Adapter {
	t.Helper()
	ad, err := config.LoadAdapter("widgetrag-a")
	if err != nil {
		t.Fatal(err)
	}
	raw := jsonx.Clone(ad.Raw).(M)
	for k, v := range extra {
		raw[k] = v
	}
	return &config.Adapter{Path: ad.Path, Raw: raw}
}

// ---------- 전송 목록 ----------

func TestTransferItemsDefaultIsPackageOnly(t *testing.T) {
	items := adapterWith(t, nil).TransferItems()
	if len(items) != 1 || items[0].From != "{backup_out}/" || items[0].To != "{package_dir}" || !items[0].Fresh || !items[0].Verify {
		t.Fatal(items)
	}
}

func TestTransferListSendsEachWithFilters(t *testing.T) {
	ok := true
	ctx, tb, fb := transferCtx(t, &ok)
	ctx.Adapter = adapterWith(t, M{"transfer": []any{
		M{"from": "{backup_out}/", "to": "{package_dir}", "fresh": true, "verify": true},
		M{"from": "~/app/", "to": "~/app", "exclude": []any{".venv", "node_modules"}, "delete": true},
	}})
	out, err := runTransfer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	base := "gmp-" + ctx.State.Data.RunID + "-data"
	if len(fb.sent) != 2 || fb.sent[0] != base || fb.sent[1] != base+"2" || out["items"] != 2 {
		t.Fatalf("전송 목록대로 보내지 않았다: %v", fb.sent)
	}
	src, dst := jsonx.AsM(fb.bodies[1]["source"]), jsonx.AsM(fb.bodies[1]["destination"])
	if jsonx.S(src, "path") != "/home/cb-user/app/" || jsonx.S(dst, "path") != "/home/cb-user/app" {
		t.Fatal(src, dst)
	}
	if len(jsonx.Strings(jsonx.Get(src, "filter", "exclude"))) != 2 || len(jsonx.Strings(jsonx.Get(dst, "filter", "exclude"))) != 2 {
		t.Fatal("제외 목록을 양쪽에 넣지 않았다", fb.bodies[1])
	}
	if jsonx.Get(dst, "filesystem", "ssh", "delete") != true || jsonx.Get(jsonx.AsM(fb.bodies[0]["destination"]), "filesystem", "ssh", "delete") != nil {
		t.Fatal("delete 를 잘못 넣었다")
	}
	if cmdsWith(tb, "sha256sum -c") != 1 || cmdsWith(tb, "ls -A") != 1 {
		t.Fatalf("대조 · 빈 폴더 확인은 패키지 건에만: %v", tb.cmds)
	}
	if ctx.State.KeyS("transfer_item_2_done") != base+"2" {
		t.Fatal(ctx.State.Data.Keys)
	}
	// 재개 — 끝난 건은 다시 보내지 않는다
	if _, err := runTransfer(ctx); err != nil || len(fb.sent) != 2 {
		t.Fatal("끝난 전송을 다시 보냈다", fb.sent, err)
	}
}

func TestConfigRejectsBadTransferAndDone(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "adapters"), 0o755)
	t.Setenv("GMP_ROOT", dir)
	// 최소 어댑터 — repo_raw · branch · resume_unit · services · health 없이도 읽혀야 한다
	base := "workload: x\nform: shell\nentry: e\nbackup: b\nbackup_out: o\npackage_dir: p\nproc_pattern: x.sh\nresults: {csv: c}\n"
	write := func(extra string) error {
		os.WriteFile(filepath.Join(dir, "adapters", "x.yaml"), []byte(base+extra), 0o644)
		_, err := config.LoadAdapter("x")
		return err
	}
	if err := write(""); err != nil {
		t.Fatal("compare · evidence 없이도 읽혀야 한다:", err)
	}
	if err := write("transfer: [{from: a}]\n"); err == nil {
		t.Fatal("to 없는 전송을 받았다")
	}
	if err := write("done: {type: status_file, path: s}\n"); err == nil {
		t.Fatal("field · success 없는 status_file 을 받았다")
	}
	if err := write("done: {type: nope}\n"); err == nil {
		t.Fatal("모르는 done.type 을 받았다")
	}
}

// ---------- 패키지 대조 스크립트 ----------

func runVerifyScript(t *testing.T, files map[string]string, sums, list string) (string, int) {
	dir := t.TempDir()
	for n, b := range files {
		os.WriteFile(filepath.Join(dir, n), []byte(b), 0o644)
	}
	if sums != "" {
		os.WriteFile(filepath.Join(dir, "checksums.sha256"), []byte(sums), 0o644)
	}
	if list != "" {
		os.WriteFile(filepath.Join(dir, "package-files.tsv"), []byte(list), 0o644)
	}
	out, err := exec.Command("bash", "-c", strings.ReplaceAll(VerifyScript, "{pkg}", dir)).CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	return string(out), 0
}

func TestVerifyScriptCases(t *testing.T) {
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("sha256sum 없음")
	}
	files := map[string]string{"a.txt": "hello\n"}
	sum := "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03  a.txt\n"
	if out, rc := runVerifyScript(t, files, sum, ""); rc != 0 || !strings.Contains(out, "package-files.tsv 없음") {
		t.Fatal("목록 없는 패키지를 체크섬으로만 대조하지 않았다", out, rc)
	}
	if out, rc := runVerifyScript(t, files, "", ""); rc == 0 {
		t.Fatal("체크섬 파일 없는 패키지를 통과시켰다", out)
	}
	list := "a.txt\t6\t5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03\nmissing.txt\t1\tx\n"
	if out, rc := runVerifyScript(t, files, sum, list); rc == 0 || !strings.Contains(out, "BAD missing.txt") {
		t.Fatal("목록의 빠진 파일을 잡지 못했다", out)
	}
}

// ---------- 기동 ----------

func startCtx(t *testing.T, extra M, route func(cmd string) string) (*Context, *fakeTB) {
	tb := &fakeTB{}
	tb.cmdOut = route
	ctx := targetCtx(t, tb)
	ctx.Adapter = adapterWith(t, extra)
	return ctx, tb
}

func statusAdapter() M {
	return M{"proc_pattern": "deploy/compose/deploy.sh", "logs": M{"run": "~/x-boot.log", "exit": "~/x-boot.exit"},
		"done": M{"type": "status_file", "path": "~/x-run/status.json", "field": "state", "success": []any{"success"}, "failure": []any{"failed"}}}
}

func startRoute(procs, sval, smtime string, extra func(cmd string) (string, bool)) func(string) string {
	ok := rcMarker + "0\n"
	return func(cmd string) string {
		if extra != nil {
			if out, hit := extra(cmd); hit {
				return out
			}
		}
		switch {
		case strings.Contains(cmd, "EXIT="):
			return "EXIT=-\nUNIT_FILE=no\nUNIT=\nROWS=0\nPROC=1\nUPTIME=99\nLAST=진행 중\nSMTIME=" + smtime + "\nSVAL=" + sval + "\n" + ok
		case strings.Contains(cmd, "echo launched"):
			return "launched 1500\n" + ok
		case strings.Contains(cmd, "pgrep"):
			return procs + "\n" + ok
		case strings.Contains(cmd, "wc -l <"):
			return "0\n" + ok
		case strings.Contains(cmd, "gzip -c"):
			return rcMarker + "3\n"
		}
		return ok
	}
}

func TestStartStatusFileDone(t *testing.T) {
	ctx, tb := startCtx(t, statusAdapter(), startRoute("0", "success", "2000", nil))
	out, err := runStart(ctx)
	if err != nil || !strings.Contains(jsonx.S(out, "why"), "state=success") {
		t.Fatal(out, err)
	}
	launch := ""
	for _, c := range tb.cmds {
		if strings.Contains(c, "echo launched") {
			launch = c
		}
	}
	if !strings.Contains(launch, "x-boot.log") || !strings.Contains(launch, "x-boot.exit") || strings.Contains(launch, "tc-run") {
		t.Fatal("어댑터 로그 경로를 쓰지 않았다:", launch)
	}
	if jsonx.Int(ctx.State.Key("start_launch_epoch")) != 1500 {
		t.Fatal(ctx.State.Data.Keys)
	}
	b64 := base64.StdEncoding.EncodeToString([]byte("deploy/compose/deploy.sh"))
	for _, c := range tb.cmds {
		if strings.Contains(c, "pgrep") && (!strings.Contains(c, b64) || strings.Contains(c, "deploy/compose/deploy.sh")) {
			t.Fatal("pgrep 패턴이 명령줄에 그대로 들어가 자기를 센다:", c)
		}
	}
}

func TestStartStatusFileFailure(t *testing.T) {
	ctx, _ := startCtx(t, statusAdapter(), startRoute("0", "failed", "2000", nil))
	if _, err := runStart(ctx); err == nil || !strings.Contains(err.Error(), "state=failed") {
		t.Fatal(err)
	}
}

func TestStartDoesNotRelaunchRunning(t *testing.T) {
	ctx, tb := startCtx(t, statusAdapter(), startRoute("2", "success", "2000", nil))
	if _, err := runStart(ctx); err != nil {
		t.Fatal(err)
	}
	if cmdsWith(tb, "echo launched") != 0 || jsonx.Int(jsonx.AsM(ctx.State.Key("start_launched"))["already_running"]) != 2 {
		t.Fatal("돌고 있는 단일 명령을 또 띄웠다", tb.cmds)
	}
}

func TestStartExistingInstall(t *testing.T) {
	extra := statusAdapter()
	extra["existing_check"] = "test -d ~/x-data && echo ~/x-data"
	extra["existing_env"] = "FORCE_CLEAN=1"
	found := func(cmd string) (string, bool) {
		if strings.Contains(cmd, "test -d ~/x-data") {
			return "/home/cb-user/x-data\n" + rcMarker + "0\n", true
		}
		return "", false
	}
	ctx, tb := startCtx(t, extra, startRoute("0", "success", "2000", found))
	if _, err := runStart(ctx); err == nil || !strings.Contains(err.Error(), "기존 설치") || cmdsWith(tb, "echo launched") != 0 {
		t.Fatal("기존 설치가 있는데 띄웠다", err)
	}
	ctx2, tb2 := startCtx(t, extra, startRoute("0", "success", "2000", found))
	ctx2.Case.Run()["on_existing"] = "clean"
	if _, err := runStart(ctx2); err != nil || cmdsWith(tb2, "FORCE_CLEAN=1") != 1 {
		t.Fatal("on_existing=clean 인데 existing_env 를 붙이지 않았다", err, tb2.cmds)
	}
}

// ---------- 판정 — 상태 파일 ----------

func TestVerifyStatusRequire(t *testing.T) {
	for _, tc := range []struct{ check, want string }{{"PASS", "PASS"}, {"FAIL", "FAIL"}} {
		route := func(cmd string) string {
			ok := rcMarker + "0\n"
			switch {
			case strings.Contains(cmd, "x-run/results.csv"):
				return "timestamp,form,pass,fail,result\n2026-10-23 10:00:00,t,10,0,PASS\n" + ok
			case strings.Contains(cmd, "x-run/status.json"):
				return `{"state": "success", "data_check": "` + tc.check + `"}` + "\n" + ok
			}
			return ok
		}
		ctx, tb := startCtx(t, M{"results": M{"csv": "~/x-run/results.csv",
			"status": M{"path": "~/x-run/status.json", "require": M{"data_check": []any{"PASS"}}}}}, route)
		v, err := runVerify(ctx)
		if err != nil || jsonx.S(v, "result") != tc.want {
			t.Fatal(tc, v, err)
		}
		if cmdsWith(tb, "ls -1d") != 0 {
			t.Fatal("증적 폴더가 없는 어댑터에서 ls 를 돌렸다")
		}
	}
}

// 결과표를 켜야 생기는 워크로드에서 결과표가 없으면 상태 파일로만 판정하고, 그렇다고 WARN 에 남긴다.
func TestVerifyMissingResultsFallsBackToStatus(t *testing.T) {
	route := func(cmd string) string {
		switch {
		case strings.Contains(cmd, "x-run/results.csv"):
			return rcMarker + "3\n"
		case strings.Contains(cmd, "x-run/status.json"):
			return `{"state": "success", "data_check": "PASS"}` + "\n" + rcMarker + "0\n"
		}
		return rcMarker + "0\n"
	}
	ctx, _ := startCtx(t, M{"results": M{"csv": "~/x-run/results.csv",
		"status": M{"path": "~/x-run/status.json", "require": M{"data_check": []any{"PASS"}}}}}, route)
	v, err := runVerify(ctx)
	if err != nil || jsonx.S(v, "result") != "PASS" {
		t.Fatal(v, err)
	}
	if w := jsonx.Strings(v["warn"]); len(w) != 1 || !strings.Contains(w[0], "RUN_VERIFY=1") {
		t.Fatal("결과표가 없다는 것을 남기지 않았다", v)
	}
	// 상태 파일이 없는 어댑터는 그대로 NO-RESULT (WidgetRAG)
	ctx2, _ := startCtx(t, M{"results": M{"csv": "~/x-run/results.csv"}}, route)
	if v, err := runVerify(ctx2); err != nil || jsonx.S(v, "result") != "NO-RESULT" {
		t.Fatal("상태 파일이 없는 어댑터에서 결과표가 없는데 NO-RESULT 가 아니다", v, err)
	}
}

// ---------- 정리 ----------

func TestCleanupBeetleTerminateRestoreAndStaging(t *testing.T) {
	ok := rcMarker + "0\n"
	tb := &fakeTB{cmdOut: func(string) string { return ok }}
	fb := &fakeBeetle{}
	ctx := sourceCtx(t, tb)
	ctx.NewBeetle = func() (Beetle, error) { return fb, nil }
	ctx.Adapter = adapterWith(t, M{"source_restore": "sudo systemctl start x-app"})
	ctx.Case.Raw["cleanup"] = "terminate"
	ctx.Case.Transfer()["strategy"] = "relay"
	ctx.State.SetKey("beetle_migrate_started", true)
	ctx.State.SetKey("backup_out", "~/x-backup/20261023-1")
	if _, err := runCleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fb.deleted) != 1 || fb.deleted[0] != "wr-gcp-1" {
		t.Fatal("Beetle 로 만든 타겟을 Beetle 로 지우지 않았다", fb.deleted)
	}
	if cmdsWith(tb, "systemctl start x-app") != 1 || !ctx.State.HasKey("source_restored") {
		t.Fatal("소스를 되돌리지 않았다", tb.cmds)
	}
	if left := jsonx.Strings(ctx.State.Key("staging_left")); len(left) != 1 || left[0] != "/tmp/transx-staging/20261023-1" {
		t.Fatal(left)
	}
}

// ---------- 결과표가 없는 워크로드 (상태 파일만) ----------

func TestConfigStatusOnlyAdapter(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "adapters"), 0o755)
	t.Setenv("GMP_ROOT", dir)
	base := "workload: x\nform: shell\nentry: e\nbackup: b\nbackup_out: o\npackage_dir: p\nproc_pattern: x.sh\n"
	load := func(extra string) (*config.Adapter, error) {
		os.WriteFile(filepath.Join(dir, "adapters", "x.yaml"), []byte(base+extra), 0o644)
		return config.LoadAdapter("x")
	}
	statusOnly := "results: {status: {path: ~/x-run/status.json, require: {state: [success]}}}\n"
	ad, err := load(statusOnly + "done: {path: ~/x-run/status.json, field: state, success: [success]}\n")
	if err != nil {
		t.Fatal("상태 파일만 있는 워크로드를 받지 않았다:", err)
	}
	if ad.Done().Type != "status_file" || ad.GoldenCheck() {
		t.Fatal("결과표가 없으면 done 기본은 status_file, health 가 없으면 골든 확인은 끈다", ad.Done(), ad.GoldenCheck())
	}
	if _, err := load("results: {}\n"); err == nil {
		t.Fatal("판정 근거(csv · status)가 없는데 받았다")
	}
	if _, err := load("results: {status: {path: s}}\n"); err == nil {
		t.Fatal("csv 없이 require 없는 상태 파일을 받았다")
	}
	if _, err := load(statusOnly + "done: {type: results_rows}\n"); err == nil {
		t.Fatal("결과표가 없는데 done=results_rows 를 받았다")
	}
	if _, err := load("results: {csv: c}\n"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "adapters", "y.yaml"), []byte("workload: y\nform: shell\nentry: e\nbackup: b\nbackup_out: o\npackage_dir: p\nresults: {csv: c}\n"), 0o644)
	if _, err := config.LoadAdapter("y"); err == nil || !strings.Contains(err.Error(), "proc_pattern") {
		t.Fatal("proc_pattern 없이 받았다 (기본값을 두지 않는다)", err)
	}
}

func statusOnlyAdapter() M {
	ad := statusAdapter()
	ad["results"] = M{"status": M{"path": "~/x-run/status.json", "require": M{"state": []any{"success"}, "data_check": []any{"PASS"}}}}
	ad["resume_unit"] = nil
	ad["health"] = nil
	return ad
}

func TestProbeWithoutUnitAndResults(t *testing.T) {
	ctx, _ := startCtx(t, statusOnlyAdapter(), startRoute("0", "success", "2000", nil))
	p := probeScript(ctx)
	if strings.Contains(p, "UNIT_FILE") || strings.Contains(p, "ROWS=") || strings.Contains(p, "{") {
		t.Fatal("재개 유닛 · 결과표가 없는 워크로드에 그 확인을 넣었다:\n" + p)
	}
	if !strings.Contains(p, "SMTIME=") || !strings.Contains(p, "PROC=") {
		t.Fatal(p)
	}
}

func TestStartStatusOnlyNoBaselineNoJournal(t *testing.T) {
	ctx, tb := startCtx(t, statusOnlyAdapter(), startRoute("0", "success", "2000", nil))
	if _, err := runStart(ctx); err != nil {
		t.Fatal(err)
	}
	if cmdsWith(tb, "wc -l <") != 0 || cmdsWith(tb, "journalctl") != 0 || ctx.State.HasKey("results_baseline_rows") {
		t.Fatal("결과표 · 재개 유닛이 없는데 행 수 · 저널을 읽었다", tb.cmds)
	}
}

func TestStartStatusFileIgnoresExitZero(t *testing.T) {
	// 단일 명령이 0 으로 끝났어도 상태 파일이 success 가 아니면 완료가 아니다
	route := func(cmd string) string {
		ok := rcMarker + "0\n"
		switch {
		case strings.Contains(cmd, "EXIT="):
			return "EXIT=0\nPROC=0\nUPTIME=99\nLAST=끝\nSMTIME=2000\nSVAL=running\n" + ok
		case strings.Contains(cmd, "echo launched"):
			return "launched 1500\n" + ok
		case strings.Contains(cmd, "pgrep"):
			return "0\n" + ok
		}
		return ok
	}
	ctx, _ := startCtx(t, statusOnlyAdapter(), route)
	if _, err := runStart(ctx); err == nil || !strings.Contains(err.Error(), "성공 값이 아님") {
		t.Fatal("종료 코드 0 만으로 끝냈다:", err)
	}
}

func TestVerifyStatusOnly(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"state": "success", "data_check": "PASS"}`, "PASS"},
		{`{"state": "success", "data_check": "FAIL"}`, "FAIL"},
		{"", "NO-RESULT"},
	} {
		route := func(cmd string) string {
			if strings.Contains(cmd, "x-run/status.json") {
				if tc.body == "" {
					return rcMarker + "1\n"
				}
				return tc.body + "\n" + rcMarker + "0\n"
			}
			return rcMarker + "0\n"
		}
		ctx, tb := startCtx(t, statusOnlyAdapter(), route)
		v, err := runVerify(ctx)
		if err != nil || jsonx.S(v, "result") != tc.want {
			t.Fatal(tc, v, err)
		}
		if cmdsWith(tb, "results.csv") != 0 {
			t.Fatal("결과표가 없는 어댑터에서 results.csv 를 읽었다")
		}
	}
}

func TestVerifyCustomColumns(t *testing.T) {
	route := func(cmd string) string {
		if strings.Contains(cmd, "x-run/out.csv") {
			return "run_at,outcome,errors,latency\n2026-10-23,OK,0,1.2\n" + rcMarker + "0\n"
		}
		return rcMarker + "0\n"
	}
	ctx, _ := startCtx(t, M{"results": M{"csv": "~/x-run/out.csv",
		"columns": M{"result": "outcome", "pass": "OK", "fail": "errors", "timestamp": "run_at"}}}, route)
	v, err := runVerify(ctx)
	if err != nil || jsonx.S(v, "result") != "PASS" || jsonx.S(v, "timestamp") != "2026-10-23" || jsonx.S(jsonx.AsM(v["metrics"]), "latency") != "1.2" {
		t.Fatal(v, err)
	}
}

func TestAbsHome(t *testing.T) {
	for _, c := range [][3]string{{"~/a/", "cb-user", "/home/cb-user/a/"}, {"~", "ubuntu", "/home/ubuntu"}, {"~/x", "root", "/root/x"}, {"/opt/x", "cb-user", "/opt/x"}} {
		if got := AbsHome(c[0], c[1]); got != c[2] {
			t.Fatal(c, got)
		}
	}
}

func TestSourceImageWant(t *testing.T) {
	for _, c := range []struct {
		os   M
		want string
	}{
		{M{"name": "Ubuntu", "version": "22.04"}, "ubuntu-22.04"},
		{M{"id": "ubuntu", "versionId": "24.04", "name": "Ubuntu"}, "ubuntu-24.04"},
		{M{"name": "Ubuntu", "version": "22.04.5 LTS (Jammy Jellyfish)"}, "ubuntu-22.04"},
		{M{}, ""},
	} {
		if got := sourceImageWant(M{"nodes": []any{M{"os": c.os}}}); got != c.want {
			t.Fatal(c, got)
		}
	}
}

func TestTransferCreatesTargetParent(t *testing.T) {
	ok := true
	ctx, tb, _ := transferCtx(t, &ok)
	ctx.Adapter = adapterWith(t, M{"package_dir": "~/x-backup/latest"})
	if _, err := runTransfer(ctx); err != nil {
		t.Fatal(err)
	}
	if cmdsWith(tb, "mkdir -p ~/x-backup") == 0 {
		t.Fatal("타겟 상위 폴더를 먼저 만들지 않았다", tb.cmds)
	}
}
