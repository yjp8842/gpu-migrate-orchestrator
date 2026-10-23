"""5. transfer — 패키지 디렉토리를 타겟의 PACKAGE_DIR 로 옮긴다.

stream: `ssh 소스 'tar -cf - .' | ssh 타겟 'tar -xf -'` (운영자 호스트 경유 — Beetle 없이 되는 길).
beetle: Beetle 데이터 이관 API (SSH→SSH rsync, 키는 암호화해 전달). 전송 뒤에는 어느 쪽이든 타겟에서 체크섬과 파일 목록을 대조한다.
"""
from ..clients.ssh import stream
from ._util import StageError

STAGE = "transfer"

VERIFY_SCRIPT = r'''cd {pkg} || exit 9
sha256sum -c --quiet checksums.sha256 || {{ echo "CHECKSUMS BAD"; exit 1; }}
bad=0; n=0
while IFS=$'\t' read -r name bytes sha; do
  [ -n "$name" ] || continue; n=$((n+1))
  if [ ! -f "$name" ] || [ "$(stat -c %s "$name")" != "$bytes" ] || [ "$(sha256sum "$name" | cut -d" " -f1)" != "$sha" ]; then echo "BAD $name"; bad=$((bad+1)); fi
done < package-files.tsv
echo "FILES=$n BAD=$bad"; [ "$bad" = 0 ]'''


def preflight(ctx):
    if ctx.dry_run:
        return
    ctx.state.get_key("backup_out") or (_ for _ in ()).throw(StageError("backup_out 이 없습니다 — package 단계가 먼저입니다"))
    ctx.node("source"); ctx.node("target")
    if ctx.case.transfer["method"] == "beetle":
        ctx.beetle


def _verify_remote(ctx, dst, pkg):
    rc, out, err = dst.run(VERIFY_SCRIPT.format(pkg=pkg), timeout=900)
    ctx.write_text(STAGE, "verify-remote.txt", out + ("\n[stderr]\n" + err if err.strip() else ""))
    if rc != 0:
        raise StageError(f"타겟 패키지 대조 실패 (rc={rc}): {(out + err).strip()[-300:]}")
    ctx.log.info(f"타겟 패키지 대조 통과: {out.strip().splitlines()[-1]}", STAGE)


def run(ctx):
    out_dir = ctx.state.get_key("backup_out") or "<backup_out>"
    pkg = ctx.adapter["package_dir"]
    method = ctx.case.transfer["method"]
    if ctx.dry_run:
        if method == "stream":
            ctx.dry(STAGE, "stream", {"source": f"tar -C {out_dir} -cf - .", "target": f"mkdir -p {pkg} && tar -xf - -C {pkg}"})
        else:
            ctx.dry(STAGE, "Beetle POST /migration/data", {"source": {"storageType": "filesystem", "path": out_dir}, "destination": {"storageType": "filesystem", "path": pkg},
                                                             "strategy": ctx.case.transfer["strategy"], "encryptionKeyId": "<encryptionKey>"})
        return {"dry_run": True}
    src, dst = ctx.ssh("source"), ctx.ssh("target")
    marker = f"{pkg}/.gmp-transferred"
    done = dst.fetch_text(marker)
    if done and done.strip() == ctx.state.data["run_id"]:
        ctx.log.info("이미 이 run 에서 전송됨 — 대조만 다시 합니다", STAGE)
        _verify_remote(ctx, dst, pkg)
        return {"skipped": True}
    rc, listing, _ = dst.run(f"ls -A {pkg} 2>/dev/null | head -3")
    if listing.strip():
        raise StageError(f"타겟 {pkg} 가 비어 있지 않습니다 ({listing.strip().replace(chr(10), ' ')}) — 묵은 패키지(.restored 포함)는 복원을 건너뛰게 하므로 비운 뒤 다시: ssh 타겟 'rm -rf {pkg}'")
    if method == "stream":
        dst.run_ok(f"mkdir -p {pkg}")
        ctx.log.info(f"스트림 전송: 소스 {out_dir} → (로컬 경유) → 타겟 {pkg}", STAGE)
        rc_s, rc_d, errs = stream(src, f"tar -C {out_dir} -cf - .", dst, f"tar -xf - -C {pkg}", timeout=int(ctx.settings.timeouts["transfer_min"]) * 60)
        if rc_s != 0 and "ermission" in errs:
            ctx.log.warn("소스 tar 가 권한으로 실패 — sudo tar 로 다시", STAGE)
            dst.run(f"rm -rf {pkg} && mkdir -p {pkg}")
            rc_s, rc_d, errs = stream(src, f"sudo tar -C {out_dir} -cf - .", dst, f"tar -xf - -C {pkg}", timeout=int(ctx.settings.timeouts["transfer_min"]) * 60)
        if rc_s != 0 or rc_d != 0:
            raise StageError(f"스트림 전송 실패 (소스 rc={rc_s}, 타겟 rc={rc_d}): {errs[-300:]}")
    else:
        from ..stages._beetle_transfer import transfer_via_beetle
        transfer_via_beetle(ctx, out_dir, pkg)
    _verify_remote(ctx, dst, pkg)
    dst.run(f"echo {ctx.state.data['run_id']} > {marker}")
    return {"method": method, "package_dir": pkg}
