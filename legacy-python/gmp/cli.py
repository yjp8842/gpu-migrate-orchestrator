"""migrate — 이관 오케스트레이터 CLI.

  migrate run      --case cases/X.yaml [--resume RUN|latest] [--from 단계] [--to 단계] [--dry-run] [--keep-keys]
  migrate <단계>   --case cases/X.yaml --run RUN|latest          (collect · recommend · provision · package · transfer · start · verify · cleanup)
  migrate status   --case cases/X.yaml                           읽기 전용 — 소스 인프라 상태 · 최근 run 요약
  migrate note     --case cases/X.yaml "사람이 한 조치"            수작업 개입 기록 (수작업 유발 구간 목록의 원천)
  migrate beetle-selftest                                        Beetle 데이터 이관 암호화 형식 자가 검증
종료 코드: 0 정상 · 1 판정 FAIL / NO-RESULT · 2 오류(설정 · 호출 · 단계 실패)
"""
import argparse
import sys
import traceback

from . import stages as stage_registry
from .clients.ssh import SshError
from .clients.tumblebug import TBError
from .config import PROJECT_ROOT, STAGES, Adapter, Case, ConfigError, Settings, load_dotenv
from .context import Context
from .log import Log
from .state import RunState, StateError
from .stages._util import StageError


def _open_or_create(args, case, adapter, settings, log_console):
    ev = settings.evidence_root
    run_id = getattr(args, "run", None) or getattr(args, "resume", None)
    if run_id == "latest":
        run_id = RunState.latest_run_id(ev, case.name)
        if not run_id:
            raise StateError(f"{case.name} 의 run 이 없습니다")
    if run_id:
        st = RunState.open(ev, case.name, run_id)
        if st.data.get("dry_run") and not args.dry_run:
            raise StateError(f"run {run_id} 은 dry-run 이었습니다 — 실 운행은 새 run 으로 시작하세요")
    else:
        st = RunState.create(ev, case.name, case.path, adapter["workload"] + "-" + adapter["form"], dry_run=args.dry_run)
    log = Log(st.run_dir, verbose=not getattr(args, "quiet", False))
    return st, log


def _plan(args, state):
    names = STAGES
    if getattr(args, "stage", None):
        names = [args.stage]
    else:
        i0 = STAGES.index(args.from_stage) if getattr(args, "from_stage", None) else 0
        i1 = STAGES.index(args.to_stage) if getattr(args, "to_stage", None) else len(STAGES) - 1
        if i1 < i0:
            raise ConfigError("--to 가 --from 보다 앞입니다")
        names = STAGES[i0:i1 + 1]
    if getattr(args, "from_stage", None) or getattr(args, "stage", None):
        for n in names:                              # 명시한 단계는 done 이어도 다시 돈다
            if state.is_done(n):
                state.mark(n, "pending")
    return names


def _execute(ctx, names):
    for name in names:
        if ctx.state.is_done(name):
            ctx.log.info(f"{name}: 이미 완료 — 건너뜀", name)
            continue
        mod = stage_registry.get(name)
        ctx.log.info(f"=== {name} ===", name)
        ctx.state.mark(name, "running")
        try:
            mod.preflight(ctx)
            result = mod.run(ctx)
        except KeyboardInterrupt:
            ctx.state.mark(name, "failed", error="중단(Ctrl-C) — run --resume 로 이어 갈 수 있다")
            ctx.log.warn(f"{name} 중단 — 다시: migrate run --case {ctx.case.path} --resume {ctx.state.data['run_id']}", name)
            return 2
        except (StageError, TBError, SshError, ConfigError, StateError) as e:
            ctx.state.mark(name, "failed", error=str(e))
            ctx.log.error(f"{name} 실패: {e}", name)
            ctx.log.error(f"고친 뒤 다시: migrate run --case {ctx.case.path} --resume {ctx.state.data['run_id']}  (개입 기록: migrate note --case … \"…\")", name)
            return 2
        except Exception as e:                       # noqa: BLE001 — 예상 밖 오류도 state 에 남긴다
            ctx.state.mark(name, "failed", error=f"{e.__class__.__name__}: {e}")
            ctx.log.error(f"{name} 예상 밖 오류: {e}\n{traceback.format_exc()}", name)
            return 2
        ctx.state.mark(name, "done", result=result if isinstance(result, dict) else None)
        if name == "verify" and ctx.state.data.get("verdict", {}) and ctx.state.data["verdict"].get("result") in ("FAIL", "NO-RESULT"):
            ctx.log.warn("판정이 FAIL 이라 cleanup 뒤 종료 코드 1 로 끝납니다", name)
    v = ctx.state.data.get("verdict") or {}
    return 1 if v.get("result") in ("FAIL", "NO-RESULT") else 0


def cmd_run(args):
    settings = Settings.load(args.settings)
    case = Case.load(args.case)
    adapter = Adapter.load(case.raw["workload"])
    console = Log(None)
    if settings.is_example:
        console.warn(f"settings.yaml 이 없어 {settings.path.name} 을 씁니다 — 실 운행 전에 복사해 채우세요")
    state, log = _open_or_create(args, case, adapter, settings, console)
    state.lock()
    try:
        ctx = Context(case, adapter, settings, state, log, dry_run=args.dry_run, keep_keys=args.keep_keys)
        names = _plan(args, state)
        log.info(f"run {state.data['run_id']} · 사례 {case.name} · 단계 {' → '.join(names)}" + (" · DRY-RUN" if args.dry_run else ""))
        log.info(f"증적: {state.run_dir}")
        rc = _execute(ctx, names)
        for line in state.summary_lines():
            print(line)
        return rc
    finally:
        state.unlock()
        log.close()


def cmd_status(args):
    settings = Settings.load(args.settings)
    case = Case.load(args.case)
    console = Log(None)
    from .clients.tumblebug import Tumblebug
    tb = Tumblebug(settings.tumblebug(case.source["tumblebug"]), console, settings.pacing)
    infra = tb.get_infra(case.source["ns"], case.source["infra"])
    if infra is None:
        print(f"소스 인프라 없음: {case.source['ns']}/{case.source['infra']}")
    else:
        print(f"소스 {case.source['infra']}: {infra.get('status')}")
        for n in tb.nodes(infra):
            print(f"  노드 {n['nodeId']} {n['status']} ip={n['publicIP']} spec={n['specId']} image={n['imageId']} csp={n['cspResourceId']}")
    rid = RunState.latest_run_id(settings.evidence_root, case.name)
    if rid:
        st = RunState.open(settings.evidence_root, case.name, rid)
        print("최근 run:")
        for line in st.summary_lines():
            print("  " + line)
    else:
        print("run 없음")
    return 0


def cmd_note(args):
    settings = Settings.load(args.settings)
    case = Case.load(args.case)
    rid = args.run if args.run and args.run != "latest" else RunState.latest_run_id(settings.evidence_root, case.name)
    if not rid:
        raise StateError(f"{case.name} 의 run 이 없습니다")
    st = RunState.open(settings.evidence_root, case.name, rid)
    st.note(args.text, stage=args.stage)
    print(f"기록됨: run {rid} — {args.text}")
    return 0


def cmd_selftest(args):
    settings = Settings.load(args.settings)
    if not settings.beetle:
        raise ConfigError("settings 에 beetle 이 없습니다")
    from . import crypto
    from .clients.beetle import Beetle
    console = Log(None)
    ok = crypto.selftest(Beetle(settings.beetle, console), console)
    return 0 if ok else 1


def build_parser():
    p = argparse.ArgumentParser(prog="migrate", description="GPU 추론서비스 이관 오케스트레이터")
    p.add_argument("--settings", help="settings.yaml 경로 (기본 프로젝트의 settings.yaml, 없으면 settings.example.yaml)")
    sub = p.add_subparsers(dest="cmd", required=True)

    def common(sp, need_case=True):
        if need_case:
            sp.add_argument("--case", required=True, help="cases/*.yaml")
        sp.add_argument("--dry-run", action="store_true",
                        help="조회(소스 메타 · 추천 · 스펙 · 이미지)는 실제로 하고, 생성 · 제어 · 원격 명령 · 전송은 요청 본문만 run/<단계>/dry-run.json 에 쓴다")
        sp.add_argument("--keep-keys", action="store_true", help="cleanup 뒤에도 타겟 키 파일을 남긴다")
        sp.add_argument("--quiet", action="store_true")

    r = sub.add_parser("run", help="단계를 순서대로 (기본 전부)")
    common(r)
    r.add_argument("--resume", metavar="RUN_ID|latest", help="기존 run 을 이어서 (완료된 단계는 건너뜀)")
    r.add_argument("--from", dest="from_stage", choices=STAGES)
    r.add_argument("--to", dest="to_stage", choices=STAGES)
    r.set_defaults(func=cmd_run, run=None, stage=None)
    for s in STAGES:
        sp = sub.add_parser(s, help=f"{s} 단계만")
        common(sp)
        sp.add_argument("--run", metavar="RUN_ID|latest", default=None if s == "collect" else "latest", help="대상 run (collect 는 비우면 새 run)")
        sp.set_defaults(func=cmd_run, stage=s, resume=None, from_stage=None, to_stage=None)
    st = sub.add_parser("status", help="읽기 전용 — 소스 인프라 상태와 최근 run")
    st.add_argument("--case", required=True)
    st.set_defaults(func=cmd_status)
    nt = sub.add_parser("note", help="사람이 한 조치를 run 에 기록")
    nt.add_argument("--case", required=True)
    nt.add_argument("--run", default="latest")
    nt.add_argument("--stage", default=None)
    nt.add_argument("text")
    nt.set_defaults(func=cmd_note)
    bs = sub.add_parser("beetle-selftest", help="Beetle 데이터 이관 암호화 형식 확인")
    bs.set_defaults(func=cmd_selftest)
    return p


def main(argv=None):
    args = build_parser().parse_args(argv)
    try:
        load_dotenv(PROJECT_ROOT / ".env")
        return args.func(args)
    except (ConfigError, StateError, TBError, SshError, StageError) as e:
        print(f"[migrate] 오류: {e}", file=sys.stderr)
        return 2
    except KeyboardInterrupt:
        print("[migrate] 중단", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
