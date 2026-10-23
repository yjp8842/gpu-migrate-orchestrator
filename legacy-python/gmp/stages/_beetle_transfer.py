"""Beetle 데이터 이관 호출 — 라이브 스키마(transx.DataMigrationModel, 2026-10-22 확인) 기준.

  {source|destination: {storageType: "filesystem", path, filesystem: {accessType: "ssh", ssh: {host, port, username, privateKey, archive, compress}},
                        preCmd(소스, 전송 전) | postCmd(목적지, 전송 뒤), filter?},
   strategy: auto|direct|relay, encryptionKeyId}
privateKey 는 crypto.Encryptor 로 암호화한다(봉투 형식은 beetle-selftest 로 확인됨). 비동기(Prefer: respond-async) + 우리가 정한 X-Request-Id.
"""
from .. import crypto
from ._util import StageError, tick_logger

STAGE = "transfer"


def _side(node, key_pem, enc, path, archive=True):
    return {"storageType": "filesystem", "path": path,
            "filesystem": {"accessType": "ssh",
                           "ssh": {"host": node["publicIP"], "port": int(node.get("sshPort") or 22),
                                   "username": node.get("nodeUserName") or "cb-user",
                                   "privateKey": enc.encrypt(key_pem), "archive": archive, "compress": True,
                                   "connectTimeout": 30}}}


def build_body(ctx, enc, out_dir, pkg):
    s, t = ctx.node("source"), ctx.node("target")
    with open(ctx.key_path("source"), encoding="utf-8") as f:
        src_key = f.read()
    with open(ctx.key_path("target"), encoding="utf-8") as f:
        dst_key = f.read()
    src = _side(s, src_key, enc, out_dir.rstrip("/") + "/")       # 끝 슬래시 — 디렉토리 '안의 것'을 옮긴다 (rsync 규칙)
    dst = _side(t, dst_key, enc, pkg)
    # preCmd 는 실행 여부를 확인하려고 짧은 표식만 남긴다 (구버전은 실행하지 않았다). backup 은 package 단계가 이미 했다.
    src["preCmd"] = f"echo gmp-precmd {ctx.state.data['run_id']} > /tmp/gmp-precmd.txt"
    dst["postCmd"] = f"ls -la {pkg} > /tmp/gmp-postcmd.txt 2>&1"
    return {"source": src, "destination": dst, "strategy": ctx.case.transfer.get("strategy", "direct"),
            "encryptionKeyId": enc.key_id}


def transfer_via_beetle(ctx, out_dir, pkg):
    b = ctx.beetle
    rid = ctx.state.get_key("beetle_data_req")
    if not rid:
        rid = f"gmp-{ctx.state.data['run_id']}-data"
        ctx.state.set_key("beetle_data_req", rid)                    # 부작용 전에 기록
        enc = crypto.Encryptor(b.encryption_key())
        body = build_body(ctx, enc, out_dir, pkg)
        b.data_migration(body, x_request_id=rid)
        ctx.log.info(f"Beetle 데이터 이관 시작 (reqId {rid}, strategy {body['strategy']})", STAGE)
    else:
        ctx.log.info(f"Beetle 데이터 이관은 이미 시작됨 (reqId {rid}) — 상태만 본다", STAGE)
    st = b.wait_request(rid, timeout_sec=int(ctx.settings.timeouts["transfer_min"]) * 60, on_tick=tick_logger(ctx.log, STAGE))
    ctx.write_json(STAGE, "beetle-request.json", {k: v for k, v in st.items() if k != "requestInfo"})
    if str(st.get("status") or "").lower() != "success":
        raise StageError(f"Beetle 데이터 이관이 성공으로 끝나지 않았습니다: {st.get('status')} — {str(st.get('errorResponse') or '')[:300]}")
    # preCmd · postCmd 가 실제로 돌았는지 (v0.6.2 에서 처음 확인하는 동작)
    pre = ctx.ssh("source").fetch_text("/tmp/gmp-precmd.txt")
    post = ctx.ssh("target").fetch_text("/tmp/gmp-postcmd.txt")
    ctx.state.set_key("beetle_hooks", {"preCmd_ran": bool(pre and ctx.state.data["run_id"] in pre), "postCmd_ran": bool(post)})
    ctx.log.info(f"Beetle 훅 실행 확인: preCmd {'실행됨' if pre else '흔적 없음'} · postCmd {'실행됨' if post else '흔적 없음'}", STAGE)
