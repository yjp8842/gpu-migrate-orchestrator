"""6. start — 타겟에서 진입 한 줄을 **분리 실행**하고 SSH 로 끝을 기다린다.

왜 분리하나: 진입 한 줄은 드라이버 설치 뒤 재부팅한다. Tumblebug 원격 명령 세션이 끊기면 Tumblebug 는 일시 오류로 보고 명령 전체를
한 번 더 실행하므로(재개 유닛과 동시에 두 번째 bootstrap 이 돈다), 명령은 즉시 반환시키고(nohup setsid) 진행은 SSH 로 본다.
완료 신호: results.csv 행 수 증가(verify 가 돌았다) · 재개 유닛이 끝남 · 분리 실행의 종료 코드 파일.
비밀 환경변수(VERIFY_ADMIN_PASSWORD)는 Tumblebug 명령 이력에 남지 않게 SSH 로 파일에 써서 source 한다.
"""
import shlex
import time

from ._util import StageError, parse_kv

STAGE = "start"

PROBE = r'''echo "EXIT=$(cat ~/tc-run.exit 2>/dev/null || echo -)"
if [ -f /etc/systemd/system/{unit}.service ] || systemctl cat {unit} >/dev/null 2>&1; then echo UNIT_FILE=yes; else echo UNIT_FILE=no; fi
echo "UNIT=$(systemctl show {unit} -p ActiveState,SubState,Result,ExecMainStatus 2>/dev/null | tr '\n' ' ')"
echo "ROWS=$(wc -l < {results} 2>/dev/null || echo 0)"
echo "PROC=$(pgrep -fc 'scripts/(local|compose)/(bootstrap|deploy).sh' 2>/dev/null || echo 0)"
echo "UPTIME=$(cut -d. -f1 /proc/uptime)"
echo "LAST=$(tail -n 1 ~/tc-run.log 2>/dev/null | cut -c1-160)"'''


def preflight(ctx):
    if ctx.dry_run:
        return
    ctx.node("target"); ctx.key_path("target")
    if not ctx.state.stage("transfer")["status"] == "done":
        raise StageError("transfer 단계가 끝나지 않았습니다")


def _launch(ctx, dst):
    tgt = ctx.case.target
    ad = ctx.adapter
    entry = ctx.remote("entry")
    secrets = ctx.secret_env()
    prefix = ""
    if secrets:
        content = "".join(f"{k}={shlex.quote(v)}\n" for k, v in secrets.items())
        rc, _, err = dst.run("umask 077; cat > ~/.gmp-run.env", input_bytes=content.encode("utf-8"))
        if rc != 0:
            raise StageError(f"비밀 환경 파일을 쓰지 못했습니다: {err.strip()[-200:]}")
        prefix = "set -a; [ -f ~/.gmp-run.env ] && . ~/.gmp-run.env; set +a; "
    inner = f"{prefix}{entry}; echo $? > ~/tc-run.exit"
    launcher = f"rm -f ~/tc-run.exit; nohup setsid bash -c {shlex.quote(inner)} > ~/tc-run.log 2>&1 < /dev/null & echo launched"
    node = ctx.node("target")
    ctx.state.set_key("start_launched", {"at": time.strftime("%Y-%m-%dT%H:%M:%S"), "entry": entry})   # 부작용 전에 기록
    results = ctx.tb_target.cmd_sync(tgt["ns"], ctx.state.get_key("target_infra_id"), [launcher], user_name=node.get("nodeUserName"), timeout_min=2)
    out, err, e = ctx.tb_target.sync_stdout(results, node["nodeId"])
    if e or not out or "launched" not in out[0]:
        raise StageError(f"진입 한 줄 분리 실행에 실패했습니다: {e or err}")
    ctx.log.info("진입 한 줄 분리 실행 — 이후는 SSH 로 폴링 (재부팅 재개 포함)", STAGE)


def run(ctx):
    ad = ctx.adapter
    unit = ad["resume_unit"]
    results = ad["results"]["csv"]
    if ctx.dry_run:
        ctx.dry(STAGE, "Tumblebug cmd_sync(타겟) nohup setsid", {"entry": ctx.remote("entry"), "secret_env_keys": list(ctx.secret_env())})
        return {"dry_run": True}
    dst = ctx.ssh("target")
    if ctx.state.get_key("results_baseline_rows") is None:
        rc, out, _ = dst.run(f"wc -l < {results} 2>/dev/null || echo 0")
        ctx.state.set_key("results_baseline_rows", int((out.strip() or "0").split()[0]) if out.strip().split()[0].isdigit() else 0)
    baseline = ctx.state.get_key("results_baseline_rows")
    if not ctx.state.get_key("start_launched"):
        _launch(ctx, dst)
    else:
        ctx.log.info("이미 분리 실행했음 — 폴링만 이어 간다", STAGE)
    timeout = int(ctx.case.run.get("timeout_start_min") or ctx.settings.timeouts["start_min"]) * 60
    t0 = time.monotonic()
    unit_seen = ctx.state.get_key("unit_seen") or False
    unit_active_seen = ctx.state.get_key("unit_active_seen") or False
    grace_ticks = 0
    outcome = None
    last_line = ""
    probe = PROBE.format(unit=unit, results=results)
    while True:
        rc, out, err = dst.run(probe, timeout=60)
        if rc != 0:
            ctx.log.info("SSH 응답 없음 — 재부팅 중으로 보고 기다립니다", STAGE)
            time.sleep(30)
            if time.monotonic() - t0 > timeout:
                outcome = ("failed", "타임아웃 (SSH 응답 없음)")
                break
            continue
        kv = parse_kv(out)
        rows = int(kv.get("ROWS", "0") or 0)
        unit_state = parse_kv(kv.get("UNIT", "").replace(" ", "\n"))
        active = unit_state.get("ActiveState", "")
        if kv.get("UNIT_FILE") == "yes" or active in ("activating", "active"):
            if not unit_seen:
                ctx.state.set_key("unit_seen", True)
            unit_seen = True
        if active in ("activating", "active"):
            if not unit_active_seen:
                ctx.state.set_key("unit_active_seen", True)
            unit_active_seen = True
        if kv.get("LAST") and kv["LAST"] != last_line:
            last_line = kv["LAST"]
            ctx.log.info(f"타겟: {last_line}", STAGE)
        procs = int(kv.get("PROC", "0") or 0)
        exit_code = kv.get("EXIT", "-")
        if rows > baseline:
            outcome = ("done", f"검증 결과 행 {baseline}→{rows}")
            break
        if not unit_seen and exit_code not in ("-", "") and procs == 0:
            outcome = ("done" if exit_code == "0" else "failed", f"진입 한 줄 종료 코드 {exit_code} (재개 유닛 없이 끝남)")
            break
        if unit_seen and active in ("inactive", "failed") and procs == 0 and unit_active_seen:
            if unit_state.get("Result") == "exit-code" or unit_state.get("ExecMainStatus", "0") not in ("", "0"):
                outcome = ("failed", f"재개 유닛 실패: {kv.get('UNIT')}")
                break
            grace_ticks += 1
            if grace_ticks >= 3:
                outcome = ("failed", f"재개 유닛은 끝났으나 검증 결과 행이 늘지 않음 ({kv.get('UNIT')})")
                break
        if time.monotonic() - t0 > timeout:
            outcome = ("failed", f"타임아웃 {timeout // 60}분 — 마지막: {last_line}")
            break
        time.sleep(30)
    # 로그 회수 (성공 · 실패 모두)
    d = ctx.state.stage_dir(STAGE)
    try:
        dst.scp_from(ad["logs"]["run"], d / "tc-run.log", timeout=120)
    except Exception as e:                           # noqa: BLE001
        ctx.log.warn(f"tc-run.log 회수 실패: {e}", STAGE)
    rc, jr, _ = dst.run(f"sudo journalctl -u {unit} --no-pager -o short-iso 2>/dev/null | tail -n 400", timeout=120)
    if jr.strip():
        ctx.write_text(STAGE, "resume-journal.log", jr)
    dst.run("rm -f ~/.gmp-run.env")
    status, why = outcome
    ctx.state.set_key("start_outcome", {"status": status, "why": why, "elapsed_sec": int(time.monotonic() - t0)})
    if status != "done":
        raise StageError(f"타겟 기동이 끝나지 않았습니다 — {why}")
    ctx.log.info(f"타겟 기동 완료 — {why} ({int(time.monotonic() - t0)}초)", STAGE)
    return {"why": why, "elapsed_sec": int(time.monotonic() - t0)}
