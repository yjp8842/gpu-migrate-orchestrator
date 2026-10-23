"""4. package — 소스에 들어가 상태를 확인하고 이관 패키지를 만든다.

  ① 접속 정보 갱신 — 중지 · 재개 뒤에는 공인 IP 가 바뀌므로 매번 Tumblebug 에서 새로 받는다 (키 · 노드 정보)
  ② 상태 확인 — Tumblebug 메타가 모르는 것: 서비스 · LLM 설정(골든 전제) · 식별 대조표의 소스 쪽 (identify.json)
  ③ 재기동(옵션) — 결정적 LLM 설정 반영 · jar 안의 구 설정 제거
  ④ 패키지 생성 — Tumblebug 비동기 원격 명령 + 상태 조회 (최대 120분). 출력 디렉토리는 run 시각으로 고정해 state 에 적고,
     resume 때 MANIFEST.txt 가 이미 있으면 다시 뜨지 않는다 (backup 은 비어 있지 않은 디렉토리를 거부한다)
"""
from .. import inspect_vm
from ._util import StageError, cmd_result_ok, describe_cmd, require_running, tick_logger

STAGE = "package"


def preflight(ctx):
    src = ctx.case.source
    if ctx.dry_run:
        return
    require_running(ctx.tb_source, src["ns"], src["infra"], "소스")


def _refresh_access(ctx):
    src = ctx.case.source
    access = ctx.tb_source.access_info(src["ns"], src["infra"])
    ctx.save_access("source", access)
    n = access[0]
    ctx.log.info(f"소스 접속 정보 갱신: {n['nodeId']} {n['publicIP']} ({n['nodeUserName']})", STAGE)
    return n


def _inspect(ctx, node):
    """소스 상태 확인 — 짧은 동기 원격 명령 한 번. 식별 결과를 남기고 LLM 설정 불일치를 돌려준다."""
    src = ctx.case.source
    cmds = inspect_vm.commands(ctx.adapter)
    results = ctx.tb_source.cmd_sync(src["ns"], src["infra"], [c for _, c in cmds], user_name=node.get("nodeUserName"), timeout_min=5)
    stdout, _, err = ctx.tb_source.sync_stdout(results, node["nodeId"])
    if err:
        raise StageError(f"소스 상태 확인 명령 실패: {err}")
    out = {name: (stdout[i] if i < len(stdout) else "") for i, (name, _) in enumerate(cmds)}
    raw = ctx.state.stage_dir(STAGE) / "inspect"
    raw.mkdir(exist_ok=True)
    for name, text in out.items():
        (raw / f"{name}.txt").write_text(text, encoding="utf-8")
    ident = inspect_vm.identify(ctx.adapter, out)
    ctx.write_json(STAGE, "identify.json", ident)
    active = [u["unit"] for u in ident["units"] if u.get("activeState") == "active"]
    gpu = (ident["gpu"] or [{}])[0]
    ctx.log.info(f"소스 상태: GPU {gpu.get('model') or '없음'} · 드라이버 {gpu.get('driverVersion') or '-'} · 서비스 active {len(active)}/{len(ident['units'])} · 커밋 {ident['code_ref']}", STAGE)
    bad, why = inspect_vm.llm_mismatch(ident["health"], ctx.case.run.get("env") or {})
    return bad, why, ident


def _run_remote(ctx, label, command, timeout_min):
    """소스에서 명령 하나를 비동기로 보내고 끝까지 기다린다. X-Request-Id 를 먼저 적어 resume 이 같은 명령을 다시 보내지 않게."""
    src = ctx.case.source
    tb = ctx.tb_source
    node = ctx.node("source")
    key = f"xreq_{label}"
    rid = ctx.state.get_key(key)
    if not rid:
        rid = tb.new_request_id(f"gmp-{ctx.state.data['run_id']}-{label}")
        ctx.state.set_key(key, rid)
        tb.cmd_async(src["ns"], src["infra"], [command], user_name=node.get("nodeUserName"), timeout_min=timeout_min, x_request_id=rid)
        ctx.log.info(f"{label} 시작 (xRequestId {rid})", STAGE)
    else:
        ctx.log.info(f"{label} 는 이미 보냈음 (xRequestId {rid}) — 상태를 이어서 기다린다", STAGE)
    done = tb.wait_command(src["ns"], src["infra"], [node["nodeId"]], rid, timeout_sec=timeout_min * 60 + 300, poll=15,
                           on_tick=tick_logger(ctx.log, STAGE, every=8))
    entry = done[node["nodeId"]]
    with open(ctx.state.stage_dir(STAGE) / f"{label}.stdout.txt", "w", encoding="utf-8") as f:
        f.write((entry.get("stdout") or "") + "\n--- stderr ---\n" + (entry.get("stderr") or ""))
    if not cmd_result_ok(entry):
        raise StageError(f"{label} 실패: {describe_cmd(entry)} — {label}.stdout.txt 끝부분: {(entry.get('stderr') or entry.get('stdout') or '')[-300:]}")
    return entry


def run(ctx):
    ad = ctx.adapter
    run_id = ctx.state.data["run_id"]
    out_dir = ctx.state.get_key("backup_out") or ad.render("backup_out", ts=run_id)
    ctx.state.set_key("backup_out", out_dir)
    golden = 1 if ctx.case.run.get("golden") else 0
    backup_cmd = ctx.remote("backup", golden=golden, out_dir=out_dir)
    restart_key = "rebuild" if ad.get("rebuild") else ("restart" if ad.get("restart") else None)
    restart_cmd = ctx.remote(restart_key) if restart_key else None
    if ctx.dry_run:
        src = ctx.case.source
        ctx.dry(STAGE, f"Tumblebug access_info {src['ns']}/{src['infra']} (접속 정보 갱신)", {"params": {"option": "accessinfo", "accessInfoOption": "showSshKey"}})
        ctx.dry(STAGE, "Tumblebug cmd_sync(소스) 상태 확인", {"command": [c for _, c in inspect_vm.commands(ad)], "timeoutMinutes": 5})
        if ctx.case.run.get("rebuild_before_backup") and restart_cmd:
            ctx.dry(STAGE, f"Tumblebug cmd_async(소스) {restart_key}", {"command": [restart_cmd], "timeoutMinutes": 20})
        ctx.dry(STAGE, "Tumblebug cmd_async(소스) backup", {"command": [backup_cmd], "timeoutMinutes": ctx.settings.timeouts["package_min"]})
        ctx.set_nodes("source", [{"nodeId": "g1-1", "publicIP": "<source.publicIP>", "sshPort": 22, "nodeUserName": "cb-user"}])
        ctx.write_text(STAGE, "MANIFEST.txt", "(dry-run)")
        return {"backup_out": out_dir, "dry_run": True}

    node = _refresh_access(ctx)
    bad, why, ident = _inspect(ctx, node)
    ssh = ctx.ssh("source")
    if ssh.fetch_text(f"{out_dir}/MANIFEST.txt"):
        ctx.log.info(f"패키지가 이미 있어 backup 을 건너뜁니다: {out_dir}", STAGE)
    else:
        rebuild = ctx.case.run.get("rebuild_before_backup") and restart_cmd
        if bad and not rebuild and ctx.case.run.get("golden"):
            raise StageError(f"골든 응답의 전제가 맞지 않습니다 ({why}) — run.rebuild_before_backup 을 켜거나 소스를 사례의 LLM 설정으로 다시 띄우세요")
        if bad:
            ctx.log.warn(f"LLM 설정 불일치 — {why} — 재기동으로 맞춘다", STAGE)
        if rebuild:
            ctx.log.info(f"소스 재기동({restart_key}) — 결정적 설정 반영 · jar 안의 구 설정 제거", STAGE)
            _run_remote(ctx, restart_key, restart_cmd, 20)
        ctx.log.info(f"backup → {out_dir} (GOLDEN={golden} 이면 골든 질문에 수 분)", STAGE)
        _run_remote(ctx, "backup", backup_cmd, int(ctx.settings.timeouts["package_min"]))
    local = ctx.state.stage_dir(STAGE)
    for name in ("MANIFEST.txt", "package.env", "package-files.tsv", "checksums.sha256"):
        try:
            ssh.scp_from(f"{out_dir}/{name}", local / name, timeout=120)
            ctx.state.artifact(STAGE, name, local / name)
        except Exception as e:                        # noqa: BLE001 — 보관용 파일이라 없어도 진행
            ctx.log.warn(f"{name} 가져오기 실패: {e}", STAGE)
    env = {}
    p = local / "package.env"
    if p.is_file():
        for line in open(p, encoding="utf-8"):
            if "=" in line:
                k, v = line.rstrip("\n").split("=", 1)
                env[k] = v
    app = env.get("APP_STATUS", "?")
    if ad["form"] == "shell" and app != "included":
        ctx.log.warn(f"패키지에 실행 파일 층이 없습니다 (APP_STATUS={app}) — 소스가 이전 코드면 최신 브랜치로 다시 띄워야 한다", STAGE)
    ctx.log.info(f"패키지 {out_dir}: 파일 {env.get('PACKAGE_FILES_COUNT', '?')}개 · 기준선 {env.get('BASELINE_STATUS', '?')} · 골든 {env.get('GOLDEN_STATUS', '?')} · 실행 파일 층 {app}", STAGE)
    return {"backup_out": out_dir, "app_status": app, "files": env.get("PACKAGE_FILES_COUNT"), "code_ref": ident.get("code_ref")}
