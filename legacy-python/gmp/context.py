"""한 run 의 실행 문맥 — 설정 · 상태 · 로그 · 클라이언트 · 키 · 노드 정보를 단계들이 공유한다."""
import json
import pathlib
import shlex

from .clients.ssh import KeyStore, Ssh
from .clients.tumblebug import Tumblebug
from .config import ConfigError, env_string


class Context:
    def __init__(self, case, adapter, settings, state, log, dry_run=False, keep_keys=False):
        self.case = case
        self.adapter = adapter
        self.settings = settings
        self.state = state
        self.log = log
        self.dry_run = dry_run
        self.keep_keys = keep_keys
        self.run_dir = pathlib.Path(state.run_dir)
        self.keys = KeyStore(self.run_dir / "keys")
        self.known_hosts = self.run_dir / "known_hosts"
        self._tb = {}
        self._beetle = None

    # ---------- 클라이언트 ----------
    def tb(self, name):
        if name not in self._tb:
            self._tb[name] = Tumblebug(self.settings.tumblebug(name), self.log, self.settings.pacing, dry_run=self.dry_run)
        return self._tb[name]

    @property
    def tb_source(self):
        return self.tb(self.case.source["tumblebug"])

    @property
    def tb_target(self):
        return self.tb(self.case.target["tumblebug"])

    @property
    def beetle(self):
        if self._beetle is None:
            if not self.settings.beetle:
                raise ConfigError("settings 에 beetle 이 없습니다 — provider=beetle · transfer.method=beetle 사례는 Beetle 주소가 필요합니다")
            from .clients.beetle import Beetle
            self._beetle = Beetle(self.settings.beetle, self.log, dry_run=self.dry_run)
        return self._beetle

    # ---------- 환경변수 ----------
    def run_env(self, extra=None):
        """타겟 진입 한 줄에 붙는 비-비밀 환경변수. FORM 으로 결과표 행을 사례와 묶는다."""
        env = {"FORM": f"{self.case.name}-target"}
        env.update({k: str(v) for k, v in (self.case.run.get("env") or {}).items()})
        if extra:
            env.update({k: str(v) for k, v in extra.items()})
        return env

    def secret_env(self):
        """비밀 환경변수 — Tumblebug 명령 이력에 남지 않게 SSH 로 파일에 써서 source 한다."""
        out = {}
        if self.settings.verify_admin_password:
            out["VERIFY_ADMIN_PASSWORD"] = self.settings.verify_admin_password
        return out

    def env_str(self, extra=None):
        return env_string(self.run_env(extra))

    # ---------- 노드 · SSH ----------
    def set_nodes(self, role, nodes, user_name=None):
        clean = [{k: v for k, v in n.items() if k != "privateKey"} for n in nodes]
        self.state.set_key(f"{role}_nodes", clean)
        if user_name:
            self.state.set_key(f"{role}_user", user_name)

    def nodes(self, role):
        return self.state.get_key(f"{role}_nodes") or []

    def node(self, role):
        ns = self.nodes(role)
        if not ns:
            raise ConfigError(f"{role} 노드 정보가 없습니다 — 앞 단계({'collect' if role == 'source' else 'provision'})가 먼저입니다")
        return ns[0]

    def key_path(self, role):
        p = self.run_dir / "keys" / f"{role}.pem"
        if not p.is_file():
            raise ConfigError(f"{role} 키가 없습니다: {p}")
        return p

    def ssh(self, role):
        n = self.node(role)
        return Ssh(n["publicIP"], n.get("nodeUserName") or self.state.get_key(f"{role}_user") or "cb-user",
                   self.key_path(role), port=n.get("sshPort") or 22, known_hosts=self.known_hosts)

    def save_access(self, role, access):
        """access_info 결과를 노드 정보(키 제외)와 키 파일로 나눠 저장한다. 첫 노드의 키를 쓴다."""
        first = access[0]
        if first.get("privateKey"):
            self.keys.write(role, first["privateKey"])
        self.set_nodes(role, access, user_name=first.get("nodeUserName"))

    # ---------- 산출물 ----------
    def write_json(self, stage, name, obj):
        d = self.state.stage_dir(stage)
        path = d / name
        with open(path, "w", encoding="utf-8") as f:
            json.dump(obj, f, ensure_ascii=False, indent=2)
        self.state.artifact(stage, name, path)
        return path

    def write_text(self, stage, name, text):
        d = self.state.stage_dir(stage)
        path = d / name
        with open(path, "w", encoding="utf-8") as f:
            f.write(text if text.endswith("\n") else text + "\n")
        self.state.artifact(stage, name, path)
        return path

    def read_json(self, stage, name):
        path = self.run_dir / stage / name
        if not path.is_file():
            raise ConfigError(f"{stage}/{name} 이 없습니다 — 앞 단계를 먼저 돌리세요")
        with open(path, encoding="utf-8") as f:
            return json.load(f)

    def dry(self, stage, what, payload):
        """dry-run: 부를 요청을 run/<stage>/dry-run.json 에 쌓는다 (호출은 하지 않는다)."""
        path = self.state.stage_dir(stage) / "dry-run.json"
        items = []
        if path.is_file():
            with open(path, encoding="utf-8") as f:
                items = json.load(f)
        items.append({"what": what, "payload": payload})
        with open(path, "w", encoding="utf-8") as f:
            json.dump(items, f, ensure_ascii=False, indent=2)
        self.log.info(f"dry-run: {what}", stage)

    # ---------- 템플릿 ----------
    def remote(self, key, **values):
        """어댑터의 명령 템플릿을 채운다. 어댑터 공통 변수(repo_raw · branch)는 자동으로 들어간다."""
        base = {"repo_raw": self.adapter["repo_raw"], "branch": self.adapter["branch"], "env": self.env_str()}
        base.update(values)
        return self.adapter.render(key, **base)

    @staticmethod
    def q(s):
        return shlex.quote(s)
