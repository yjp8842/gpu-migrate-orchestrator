"""run 디렉토리와 state.json — 단계 상태 · 멱등 키 · 산출물 · 사람이 개입한 기록.

멱등 키(인프라 이름 · X-Request-Id · backup 출력 경로)는 부작용을 일으키기 **전에** 적는다. 그래야 중단 뒤
`run --resume` 이 같은 자원을 다시 만들지 않고 조회로 이어 간다.
"""
import fcntl
import json
import os
import pathlib
import time

from .config import STAGES


class StateError(Exception):
    pass


def _now():
    return time.strftime("%Y-%m-%dT%H:%M:%S%z")


class RunState:
    def __init__(self, run_dir):
        self.run_dir = pathlib.Path(run_dir)
        self.path = self.run_dir / "state.json"
        self.data = None
        self._lock_fh = None

    # ---------- 생성 · 열기 ----------
    @classmethod
    def create(cls, evidence_root, case_name, case_path, workload, dry_run=False):
        run_id = time.strftime("%Y%m%d-%H%M%S")
        run_dir = pathlib.Path(evidence_root) / case_name / run_id
        if run_dir.exists():
            run_id += f"-{os.getpid()}"
            run_dir = pathlib.Path(evidence_root) / case_name / run_id
        run_dir.mkdir(parents=True)
        st = cls(run_dir)
        st.data = {
            "case": case_name, "case_path": str(case_path), "workload": workload, "run_id": run_id,
            "dry_run": dry_run, "created": _now(), "updated": _now(),
            "stages": {s: {"status": "pending"} for s in STAGES},
            "keys": {}, "interventions": [], "verdict": None,
        }
        st.save()
        return st

    @classmethod
    def open(cls, evidence_root, case_name, run_id):
        run_dir = pathlib.Path(evidence_root) / case_name / run_id
        st = cls(run_dir)
        if not st.path.is_file():
            raise StateError(f"run 을 찾지 못했습니다: {st.path}")
        st.load()
        return st

    @staticmethod
    def latest_run_id(evidence_root, case_name):
        base = pathlib.Path(evidence_root) / case_name
        if not base.is_dir():
            return None
        runs = sorted(p.name for p in base.iterdir() if (p / "state.json").is_file())
        return runs[-1] if runs else None

    def load(self):
        with open(self.path, encoding="utf-8") as f:
            self.data = json.load(f)

    def save(self):
        self.data["updated"] = _now()
        tmp = self.path.with_suffix(".json.tmp")
        with open(tmp, "w", encoding="utf-8") as f:
            json.dump(self.data, f, ensure_ascii=False, indent=2)
        os.replace(tmp, self.path)

    # ---------- 잠금 (같은 run 을 두 프로세스가 돌리지 않게) ----------
    def lock(self):
        fh = open(self.run_dir / ".lock", "w")
        try:
            fcntl.flock(fh, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            fh.close()
            raise StateError(f"다른 프로세스가 이 run 을 돌리고 있습니다: {self.run_dir}")
        self._lock_fh = fh
        self._lock_fh.write(str(os.getpid()))
        self._lock_fh.flush()

    def unlock(self):
        if self._lock_fh:
            fcntl.flock(self._lock_fh, fcntl.LOCK_UN)
            self._lock_fh.close()
            self._lock_fh = None

    # ---------- 단계 ----------
    def stage(self, name):
        return self.data["stages"][name]

    def stage_dir(self, name):
        d = self.run_dir / name
        d.mkdir(parents=True, exist_ok=True)
        return d

    def mark(self, name, status, error=None, **extra):
        s = self.stage(name)
        s["status"] = status
        if status == "running":
            s["started"] = _now()
            s.pop("error", None)
        if status in ("done", "failed", "skipped"):
            s["ended"] = _now()
        if error:
            s["error"] = str(error)
        s.update(extra)
        self.save()

    def artifact(self, name, label, path):
        s = self.stage(name)
        s.setdefault("artifacts", {})[label] = str(path)
        self.save()

    def is_done(self, name):
        return self.stage(name)["status"] == "done"

    # ---------- 멱등 키 ----------
    def set_key(self, key, value):
        self.data["keys"][key] = value
        self.save()

    def get_key(self, key, default=None):
        return self.data["keys"].get(key, default)

    # ---------- 사람의 개입 ----------
    def note(self, text, stage=None, by="operator"):
        self.data["interventions"].append({"at": _now(), "stage": stage, "by": by, "text": text})
        self.save()

    def set_verdict(self, verdict):
        self.data["verdict"] = verdict
        self.save()

    def summary_lines(self):
        d = self.data
        lines = [f"run {d['run_id']} · 사례 {d['case']} · 워크로드 {d['workload']}" + (" · DRY-RUN" if d.get("dry_run") else "")]
        for s in STAGES:
            st = d["stages"][s]
            extra = ""
            if st.get("error"):
                extra = f" — {st['error'][:100]}"
            lines.append(f"  {s:10} {st['status']:8} {st.get('started', '')[:19]:19} {st.get('ended', '')[:19]:19}{extra}")
        if d.get("verdict"):
            lines.append(f"  판정: {d['verdict'].get('result')} ({d['verdict'].get('summary', '')})")
        if d["interventions"]:
            lines.append(f"  개입 {len(d['interventions'])}건:")
            for n in d["interventions"]:
                lines.append(f"    {n['at'][:19]} [{n.get('stage') or '-'}] {n['text']}")
        return lines
