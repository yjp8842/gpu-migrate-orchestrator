"""사례(cases/) · 어댑터(adapters/) · 실행 환경(settings.yaml) 읽기와 검증.

사례 파일에는 이 운행에서 바뀌는 값만 둔다. 워크로드가 다른 점은 전부 어댑터에, 주소 · 계정 · 타임아웃은 설정에.
설정 · 사례의 주소 · 계정 · 비밀번호 · 사무실 IP 는 ${이름} · ${이름:-값} 으로 적고 환경변수에서 받는다 (Go 판과 같은 규칙).
"""
import os
import pathlib
import re
import shlex
from dataclasses import dataclass, field

import yaml

# legacy-python/ 으로 옮긴 뒤에도 adapters/ · cases/ · settings.yaml 은 프로젝트 맨 위에 있다
PROJECT_ROOT = pathlib.Path(os.environ.get("GMP_ROOT") or pathlib.Path(__file__).resolve().parent.parent.parent)
STAGES = ["collect", "recommend", "provision", "package", "transfer", "start", "verify", "cleanup"]


class ConfigError(Exception):
    pass


_ENV_REF = re.compile(r"\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}")


def expand_env(data, path):
    """설정 · 사례 값의 ${이름} 을 채운다. 비어 있는 변수는 이름을 모아 한 번에 알린다. 어댑터는 셸 명령이라 쓰지 않는다."""
    missing = set()

    def one(m):
        v = os.environ.get(m.group(1), "")
        if v:
            return v
        if m.group(2) is not None:
            return m.group(3)
        missing.add(m.group(1))
        return m.group(0)

    def walk(v):
        if isinstance(v, str):
            return _ENV_REF.sub(one, v)
        if isinstance(v, dict):
            return {k: walk(x) for k, x in v.items()}
        if isinstance(v, list):
            return [walk(x) for x in v]
        return v

    out = walk(data)
    if missing:
        raise ConfigError(f"{path}: 환경변수가 비어 있습니다: {', '.join(sorted(missing))} — 셸에서 export 하거나 프로젝트 폴더의 .env 에 적으세요 (견본 .env.example)")
    return out


def load_dotenv(path):
    """KEY=VALUE 줄을 읽어 아직 없는 환경변수만 설정한다. 파일이 없으면 넘어간다."""
    path = pathlib.Path(path)
    if not path.is_file():
        return
    for i, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        if line.startswith("export "):
            line = line[len("export "):]
        k, sep, v = line.partition("=")
        k, v = k.strip(), v.strip()
        if not sep or not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", k):
            raise ConfigError(f"{path}:{i}: KEY=VALUE 꼴이 아닙니다")
        if len(v) >= 2 and v[0] == v[-1] and v[0] in "\"'":
            v = v[1:-1]
        os.environ.setdefault(k, v)


def load_yaml(path):
    path = pathlib.Path(path)
    if not path.is_file():
        raise ConfigError(f"파일이 없습니다: {path}")
    with open(path, encoding="utf-8") as f:
        data = yaml.safe_load(f) or {}
    if not isinstance(data, dict):
        raise ConfigError(f"맵 형식이어야 합니다: {path}")
    return data


def _need(d, key, where):
    if key not in d or d[key] in (None, ""):
        raise ConfigError(f"{where} 에 '{key}' 가 필요합니다")
    return d[key]


def _enum(value, allowed, where):
    if value not in allowed:
        raise ConfigError(f"{where} 는 {allowed} 중 하나여야 합니다 (지금 {value!r})")
    return value


def env_string(env):
    """{K: V} → 'K=V K2=V2' (셸 인용). 진입 한 줄 앞에 붙인다."""
    return " ".join(f"{k}={shlex.quote(str(v))}" for k, v in env.items())


class _Strict(dict):
    def __missing__(self, key):
        raise ConfigError(f"템플릿 변수 '{{{key}}}' 의 값이 없습니다")


def render(template, **values):
    return template.format_map(_Strict(values))


@dataclass
class Endpoint:
    name: str
    url: str
    user: str = "default"
    password: str = "default"

    @property
    def auth(self):
        return (self.user, self.password)


@dataclass
class Settings:
    path: pathlib.Path
    tumblebugs: dict
    beetle: Endpoint | None
    evidence_root: pathlib.Path
    verify_admin_password: str
    timeouts: dict
    pacing: dict
    is_example: bool = False

    @classmethod
    def load(cls, path=None):
        if path is None:
            cand = PROJECT_ROOT / "settings.yaml"
            is_example = not cand.is_file()
            path = cand if not is_example else PROJECT_ROOT / "settings.example.yaml"
        else:
            is_example = False
        path = pathlib.Path(path)
        raw = expand_env(load_yaml(path), path)
        tbs = {}
        for name, ep in (raw.get("tumblebugs") or {}).items():
            user = ep.get("user") or ""
            pw = os.environ.get(f"GMP_TB_PASSWORD_{name.upper()}") or ep.get("password") or ""
            if not user or not pw:
                raise ConfigError(f"{path}: tumblebugs.{name} 의 user · password 가 필요합니다")
            tbs[name] = Endpoint(name, _need(ep, "url", f"tumblebugs.{name}").rstrip("/"), user, pw)
        if not tbs:
            raise ConfigError(f"{path}: tumblebugs 에 최소 하나가 필요합니다")
        beetle = None
        if raw.get("beetle") and raw["beetle"].get("url"):
            b = raw["beetle"]
            user = b.get("user") or ""
            pw = os.environ.get("GMP_BEETLE_PASSWORD") or b.get("password") or ""
            if not user or not pw:
                raise ConfigError(f"{path}: beetle 의 user · password 가 필요합니다")
            beetle = Endpoint("beetle", b["url"].rstrip("/"), user, pw)
        ev = pathlib.Path(raw.get("evidence_root") or "evidence")
        if not ev.is_absolute():
            ev = (path.parent / ev).resolve()
        timeouts = {"provision_min": 30, "package_min": 120, "start_min": 75, "transfer_min": 60}
        timeouts.update(raw.get("timeouts") or {})
        pacing = {"infra_get_interval_sec": 1.0, "default_interval_sec": 0.25}
        pacing.update(raw.get("pacing") or {})
        return cls(path=path, tumblebugs=tbs, beetle=beetle, evidence_root=ev,
                   verify_admin_password=os.environ.get("GMP_VERIFY_ADMIN_PASSWORD", raw.get("verify_admin_password") or ""),
                   timeouts=timeouts, pacing=pacing, is_example=is_example)

    def tumblebug(self, name):
        if name not in self.tumblebugs:
            raise ConfigError(f"settings 에 tumblebugs.{name} 이 없습니다 (있는 것: {', '.join(self.tumblebugs)})")
        return self.tumblebugs[name]


@dataclass
class Adapter:
    """워크로드 어댑터 — 파이프라인이 워크로드에 대해 아는 전부."""
    path: pathlib.Path
    raw: dict

    REQUIRED = ("workload", "form", "repo_raw", "branch", "entry", "backup", "backup_out", "package_dir",
                "results", "resume_unit", "services")

    @classmethod
    def load(cls, name):
        path = PROJECT_ROOT / "adapters" / f"{name}.yaml"
        raw = load_yaml(path)
        if raw.get("todo"):
            raise ConfigError(f"adapters/{name}.yaml 은 아직 자리만 있습니다: {raw['todo']}")
        for k in cls.REQUIRED:
            _need(raw, k, f"adapters/{name}.yaml")
        for k in ("csv", "compare", "evidence"):
            _need(raw["results"], k, f"adapters/{name}.yaml results")
        return cls(path=path, raw=raw)

    def __getitem__(self, key):
        return self.raw[key]

    def get(self, key, default=None):
        return self.raw.get(key, default)

    def render(self, key, **values):
        return render(self.raw[key], **values)


@dataclass
class Case:
    path: pathlib.Path
    raw: dict
    name: str = field(init=False)

    def __post_init__(self):
        r = self.raw
        self.name = _need(r, "case", str(self.path))
        _need(r, "workload", "사례")
        src = _need(r, "source", "사례")
        src.setdefault("tumblebug", "default")
        _need(src, "ns", "source"); _need(src, "infra", "source")
        tgt = _need(r, "target", "사례")
        tgt.setdefault("tumblebug", "default")
        tgt.setdefault("ns", src["ns"])
        _enum(_need(tgt, "provider", "target"), ("beetle", "tumblebug"), "target.provider")
        for k in ("csp", "region", "name"):
            _need(tgt, k, "target")
        if tgt["provider"] == "tumblebug":
            _need(tgt, "spec", "target (provider=tumblebug 는 Tumblebug specId 가 필요)")
        pol = r.setdefault("policy", {})
        _enum(pol.setdefault("gpu", "same-model-first"), ("same-model-first", "vram-at-least"), "policy.gpu")
        pol.setdefault("image", "ubuntu-24.04")
        pol.setdefault("driver_series", "595")          # 워크로드 bootstrap 의 NVIDIA_DRIVER_SERIES 와 같게
        pol.setdefault("root_disk_gb", 100)
        pol.setdefault("open_ports", [])
        _enum(pol.setdefault("on_perf_fail", "stop"), ("stop", "next-candidate"), "policy.on_perf_fail")
        if pol["on_perf_fail"] == "next-candidate":
            raise ConfigError("policy.on_perf_fail=next-candidate 는 1차에서 구현되지 않았습니다 (stop 만)")
        tr = r.setdefault("transfer", {})
        _enum(tr.setdefault("method", "stream"), ("beetle", "stream"), "transfer.method")
        _enum(tr.setdefault("strategy", "direct"), ("direct", "relay"), "transfer.strategy")
        run = r.setdefault("run", {})
        run.setdefault("env", {})
        run.setdefault("golden", True)
        run.setdefault("rebuild_before_backup", True)
        _enum(r.setdefault("cleanup", "suspend"), ("suspend", "terminate", "keep"), "cleanup")

    @classmethod
    def load(cls, path):
        path = pathlib.Path(path)
        return cls(path=path, raw=expand_env(load_yaml(path), path))

    @property
    def source(self):
        return self.raw["source"]

    @property
    def target(self):
        return self.raw["target"]

    @property
    def policy(self):
        return self.raw["policy"]

    @property
    def transfer(self):
        return self.raw["transfer"]

    @property
    def run(self):
        return self.raw["run"]

    @property
    def cleanup(self):
        return self.raw["cleanup"]
