"""CB-Tumblebug REST 클라이언트 (v0.12.30 기준, Basic 인증).

- 호출 간격: GET /ns/{ns}/infra[/{id}] 는 출발 IP 당 2 req/s 를 MapUI 사용자와 나눠 쓴다 → 1초 간격. 나머지는 0.25초.
- 429 는 Retry-After 가 없다 → 지수 백오프(2→4→…≤60초, 지터, 최대 10회).
- 5xx · 네트워크 오류는 GET 만 재시도한다. **POST infraDynamic · POST cmd 는 자동 재시도하지 않는다** (중복 생성 · 중복 실행).
- 재개(resume)는 이름 조회(infra)와 우리가 정한 X-Request-Id(cmd)로 이어 간다.
"""
import random
import re
import time
import uuid

import requests

from ..log import mask

TERMINAL = {"Completed", "CompletedWithError", "Failed", "Timeout", "Cancelled", "Interrupted"}
# 상태 의미 (Tumblebug remoteCommand.go): Completed=종료 0 · CompletedWithError=SSH 는 됐고 종료 코드≠0 ·
# Failed=전송 오류(접속 · 인증 · EOF — VM 재부팅 포함) · Timeout=timeoutMinutes 초과 · Interrupted=Tumblebug 재시작
SUCCESS = {"Completed"}


_MISSING = re.compile(r"not (?:exist|found)|does not exist|no such|not registered|failed to find", re.I)


def _looks_missing(text):
    return bool(_MISSING.search(text or ""))


class TBError(Exception):
    def __init__(self, msg, status=None, body=None):
        super().__init__(msg)
        self.status = status
        self.body = body


class Tumblebug:
    def __init__(self, endpoint, log, pacing=None, dry_run=False, session=None):
        self.ep = endpoint
        self.log = log
        self.dry_run = dry_run
        self.pacing = {"infra_get_interval_sec": 1.0, "default_interval_sec": 0.25}
        self.pacing.update(pacing or {})
        self.s = session or requests.Session()
        self.s.auth = endpoint.auth
        self.s.headers["Accept"] = "application/json"
        self._last = {}

    # ---------- 공통 ----------
    def _pace(self, route_class):
        interval = self.pacing["infra_get_interval_sec" if route_class == "infra_get" else "default_interval_sec"]
        last = self._last.get(route_class, 0.0)
        wait = last + interval - time.monotonic()
        if wait > 0:
            time.sleep(wait)
        self._last[route_class] = time.monotonic()

    def _req(self, method, path, *, params=None, json_body=None, headers=None, timeout=(10, 130),
             route_class="default", retry=True, allow404=False, raw=False, write=None):
        # dry-run 은 조회만 한다. 쓰기 여부는 메서드가 아니라 호출별로 — 제어(suspend · terminate)는 GET 이지만 쓰기이고,
        # searchImage · recommendSpec 은 POST 지만 조회다.
        if write is None:
            write = method != "GET"
        if self.dry_run and write:
            raise TBError(f"dry-run 에서는 Tumblebug 에 쓰지 않습니다: {method} {path}")
        url = f"{self.ep.url}{path}"
        attempt, delay = 0, 2.0
        while True:
            self._pace(route_class)
            t0 = time.monotonic()
            try:
                r = self.s.request(method, url, params=params, json=json_body, headers=headers, timeout=timeout)
            except requests.RequestException as e:
                self.log.http(method, url, "EXC", time.monotonic() - t0, body=json_body, note=repr(e))
                if method == "GET" and retry and attempt < 5:
                    attempt += 1
                    time.sleep(delay + random.uniform(0, 1))
                    delay = min(delay * 2, 60)
                    continue
                raise TBError(f"{method} {path}: 연결 실패 ({e.__class__.__name__})") from e
            self.log.http(method, r.url, r.status_code, time.monotonic() - t0, body=json_body)
            if allow404 and (r.status_code == 404 or (r.status_code == 500 and _looks_missing(r.text))):
                return None      # Tumblebug 는 없는 자원을 404 가 아니라 500 + "does not exist" 로 답하기도 한다 — 재시도하지 않는다
            if r.status_code == 429 or (r.status_code >= 500 and method == "GET" and retry):
                attempt += 1
                if attempt > 10:
                    raise TBError(f"{method} {path}: {r.status_code} 가 계속됩니다 (호출 제한 · 서버 오류)", r.status_code, r.text[:300])
                time.sleep(delay + random.uniform(0, 1))
                delay = min(delay * 2, 60)
                continue
            if r.status_code >= 400:
                raise TBError(f"{method} {path}: HTTP {r.status_code} — {mask(r.text[:400])}", r.status_code, r.text)
            if raw:
                return r
            if not r.content:
                return {}
            try:
                return r.json()
            except ValueError:
                return {"_text": r.text}

    # ---------- 인프라 조회 ----------
    def list_infra(self, ns):
        d = self._req("GET", f"/ns/{ns}/infra", params={"option": "id"}, route_class="infra_get")
        return d.get("output") or []

    def get_infra(self, ns, infra_id):
        """없으면 None. (상태 문자열 예: 'Running:1 (R:1/1)' · 'Suspended:1 (S:1/1)' · 'Failed')"""
        return self._req("GET", f"/ns/{ns}/infra/{infra_id}", route_class="infra_get", allow404=True)

    @staticmethod
    def nodes(infra):
        """InfraInfo.node[] 에서 필요한 것만."""
        out = []
        for n in (infra or {}).get("node") or []:
            out.append({"nodeId": n.get("id") or n.get("name"), "name": n.get("name"), "status": n.get("status"),
                        "publicIP": n.get("publicIP"), "privateIP": n.get("privateIP"), "sshPort": n.get("sshPort") or 22,
                        "cspResourceId": n.get("cspResourceId"), "specId": n.get("specId"), "imageId": n.get("imageId"),
                        "nodeGroupId": n.get("nodeGroupId"), "createdTime": n.get("createdTime"),
                        "securityGroupIds": n.get("securityGroupIds") or [], "sshKeyId": n.get("sshKeyId")})
        return out

    @staticmethod
    def infra_state(infra):
        """'Running:1 (R:1/1)' → 'Running'"""
        s = (infra or {}).get("status") or ""
        return s.split(":")[0].split(" ")[0]

    def access_info(self, ns, infra_id):
        """노드별 접속 정보 (개인키 포함). 응답은 대소문자가 섞여 있다 — 바깥은 PascalCase, 노드는 camelCase."""
        d = self._req("GET", f"/ns/{ns}/infra/{infra_id}",
                      params={"option": "accessinfo", "accessInfoOption": "showSshKey"}, route_class="infra_get")
        out = []
        for g in d.get("InfraNodeGroupAccessInfo") or d.get("infraNodeGroupAccessInfo") or []:
            for n in g.get("NodeAccessInfo") or g.get("nodeAccessInfo") or []:
                out.append({"nodeId": n.get("nodeId"), "publicIP": n.get("publicIP"), "privateIP": n.get("privateIP"),
                            "sshPort": n.get("sshPort") or 22, "privateKey": n.get("privateKey"),
                            "nodeUserName": n.get("nodeUserName") or "cb-user",
                            "nodeGroupId": g.get("NodeGroupId") or g.get("nodeGroupId")})
        if not out:
            raise TBError(f"접속 정보가 비어 있습니다: {ns}/{infra_id}")
        return out

    def wait_infra(self, ns, infra_id, want=("Running",), timeout_sec=900, poll=20, on_tick=None):
        t0 = time.monotonic()
        while True:
            infra = self.get_infra(ns, infra_id)
            state = self.infra_state(infra)
            if on_tick:
                on_tick(state, infra)
            if state in want:
                return infra
            if state in ("Failed", "Terminated", "Terminating"):
                raise TBError(f"인프라 {infra_id} 상태가 {state} 입니다")
            if time.monotonic() - t0 > timeout_sec:
                raise TBError(f"인프라 {infra_id} 가 {timeout_sec}초 안에 {want} 가 되지 않았습니다 (지금 {state})")
            time.sleep(poll)

    # ---------- 생성 · 제어 · 삭제 ----------
    INFRA_DYNAMIC_KEYS = {"name", "description", "label", "policyOnPartialFailure", "installMonAgent", "nodeGroups",
                          "systemLabel", "vNetTemplateId", "sgTemplateId"}
    NODEGROUP_KEYS = {"name", "nodeGroupSize", "label", "description", "specId", "imageId", "rootDiskType",
                      "rootDiskSize", "connectionName", "zone", "vNetTemplateId", "sgTemplateId", "distributeSubnets"}

    @classmethod
    def validate_infra_dynamic(cls, body):
        bad = set(body) - cls.INFRA_DYNAMIC_KEYS
        if bad:
            raise TBError(f"infraDynamic 에 모르는 필드: {sorted(bad)}")
        for ng in body.get("nodeGroups") or []:
            bad = set(ng) - cls.NODEGROUP_KEYS
            if bad:
                raise TBError(f"nodeGroups 에 모르는 필드: {sorted(bad)}")
            for k in ("nodeGroupSize", "rootDiskSize"):
                if k in ng and not isinstance(ng[k], int):
                    raise TBError(f"nodeGroups.{k} 는 정수여야 합니다 (문자열이면 거부된다)")
        return body

    def infra_dynamic(self, ns, body, timeout_sec=1800):
        """동기 생성 — 자동 재시도 없음. 재개는 호출 전에 get_infra(name) 으로 판단한다."""
        self.validate_infra_dynamic(body)
        return self._req("POST", f"/ns/{ns}/infraDynamic", json_body=body, timeout=(10, timeout_sec), retry=False)

    def control(self, ns, infra_id, action):
        return self._req("GET", f"/ns/{ns}/control/infra/{infra_id}", params={"action": action}, timeout=(10, 300), retry=False,
                         write=True)

    def delete_infra(self, ns, infra_id, option="terminate"):
        return self._req("DELETE", f"/ns/{ns}/infra/{infra_id}", params={"option": option}, timeout=(10, 600), retry=False)

    def add_sg_rules(self, ns, sg_id, rules):
        """rules: [{"Ports": "80", "Protocol": "tcp", "Direction": "inbound", "CIDR": "x.x.x.x/32"}] — 기존 규칙에 추가한다
        (MapUI 의 Update Firewall 은 통째로 교체라 쓰지 않는다)."""
        return self._req("POST", f"/ns/{ns}/resources/securityGroup/{sg_id}/rules",
                         json_body={"firewallRules": rules}, timeout=(10, 120), retry=False)

    # ---------- 네트워크 ----------
    def get_vnet(self, ns, vnet_id):
        """cidrBlock · subnetInfoList[{id, ipv4_CIDR, zone}]"""
        return self._req("GET", f"/ns/{ns}/resources/vNet/{vnet_id}", allow404=True)

    def get_security_group(self, ns, sg_id):
        """firewallRules: [{Port, Protocol, Direction, CIDR}]"""
        return self._req("GET", f"/ns/{ns}/resources/securityGroup/{sg_id}", allow404=True)

    # ---------- 이미지 · 스펙 ----------
    def get_image(self, ns, image_id):
        return self._req("GET", f"/ns/{ns}/resources/image/{image_id}", allow404=True, retry=False)

    def get_spec(self, ns, spec_id):
        return self._req("GET", f"/ns/{ns}/resources/spec/{spec_id}", allow404=True, retry=False)

    SYSTEM_NS = "system"   # 공용 스펙 · 이미지가 등록된 네임스페이스 (사용자 ns 에서는 조회되지 않는다)

    def resolve_spec(self, ns, spec_id):
        """system → ns 순으로 찾는다."""
        return self.get_spec(self.SYSTEM_NS, spec_id) or self.get_spec(ns, spec_id)

    def resolve_image(self, ns, image_id):
        return self.get_image(self.SYSTEM_NS, image_id) or self.get_image(ns, image_id)

    def find_image(self, provider, region, os_type, prefer_not=("minimal",)):
        """searchImage(ns=system) 로 기본 이미지 중 하나를 고른다 — 이름에 prefer_not 이 든 것(minimal 등)은 뒤로."""
        found = self.search_image(self.SYSTEM_NS, provider, region, os_type)
        basic = [i for i in found if i.get("isBasicImage")] or found
        def rank(i):
            name = (i.get("osDistribution") or i.get("name") or i.get("cspImageName") or "").lower()
            return (any(p in name for p in prefer_not), name)
        return sorted(basic, key=rank)[0] if basic else None

    def search_image(self, ns, provider, region, os_type, basic_only=True, max_results=20):
        d = self._req("POST", f"/ns/{ns}/resources/searchImage",
                      json_body={"providerName": provider, "regionName": region, "osType": os_type,
                                 "osArchitecture": "x86_64", "includeBasicImageOnly": basic_only,
                                 "includeDeprecatedImage": False, "maxResults": max_results}, write=False)
        return d.get("imageList") or []

    def recommend_spec(self, filters, priority="cost", limit=10):
        """POST /tumblebug/recommendSpec — 조건(필터)으로 사양 목록을 정렬해 받는다. Beetle 추천의 축소판으로,
        Beetle 이 없을 때 소스 사양(GPU 모델 · VRAM · vCPU · 메모리)을 조건으로 바꿔 묻는다.
        filters: [(metric, operator, operand)] 예 ("acceleratorModel", "contains"?, ...) 는 지원 여부가 버전마다 달라
        여기서는 ">=" · "<=" · "==" 만 쓰고 모델명은 우리가 결과에서 거른다."""
        policy = []
        for metric, op, val in filters:
            policy.append({"metric": metric, "condition": [{"operator": op, "operand": str(val)}]})
        # 타입 주의 (v0.12.30 소스): weight 는 float, limit 은 int, operand 는 문자열 — 틀리면 400 Unmarshal type error
        body = {"filter": {"policy": policy},
                "priority": {"policy": [{"metric": priority, "weight": 1.0}]},
                "limit": int(limit)}
        d = self._req("POST", "/recommendSpec", json_body=body, timeout=(10, 240), write=False)
        return d if isinstance(d, list) else (d.get("output") or d.get("specList") or [])

    # ---------- 원격 명령 ----------
    @staticmethod
    def new_request_id(prefix):
        return f"{prefix}-{uuid.uuid4().hex[:12]}"

    def cmd_async(self, ns, infra_id, commands, user_name=None, timeout_min=30, x_request_id=None):
        """즉시 반환 — xRequestId 를 돌려준다. 각 command 원소는 SSH exec 하나. timeoutMinutes 는 최대 120."""
        timeout_min = max(1, min(int(timeout_min), 120))
        body = {"command": list(commands), "timeoutMinutes": timeout_min}
        if user_name:
            body["userName"] = user_name
        headers = {"X-Request-Id": x_request_id} if x_request_id else None
        d = self._req("POST", f"/ns/{ns}/cmd/infra/{infra_id}", params={"async": "true"}, json_body=body,
                      headers=headers, timeout=(10, 60), retry=False)
        rid = d.get("xRequestId") or x_request_id
        if not rid:
            raise TBError("원격 명령의 xRequestId 를 받지 못했습니다")
        return rid

    def cmd_sync(self, ns, infra_id, commands, user_name=None, timeout_min=30):
        """결과까지 기다린다 — 짧은 명령(수집 · 상태 확인)용. 긴 명령은 cmd_async + wait_command."""
        timeout_min = max(1, min(int(timeout_min), 120))
        body = {"command": list(commands), "timeoutMinutes": timeout_min}
        if user_name:
            body["userName"] = user_name
        d = self._req("POST", f"/ns/{ns}/cmd/infra/{infra_id}", json_body=body,
                      timeout=(10, timeout_min * 60 + 120), retry=False)
        return d.get("results") or []

    @staticmethod
    def sync_stdout(results, node_id=None):
        """cmd_sync 결과에서 (노드 하나의) stdout 을 명령 순서대로 리스트로."""
        for r in results:
            if node_id and r.get("nodeId") != node_id:
                continue
            so = r.get("stdout") or {}
            se = r.get("stderr") or {}
            keys = sorted(so.keys(), key=lambda k: int(k))
            return [so[k] for k in keys], [se.get(k, "") for k in keys], r.get("error") or ""
        return [], [], "결과 없음"

    def command_status(self, ns, infra_id, node_id, x_request_id):
        d = self._req("GET", f"/ns/{ns}/infra/{infra_id}/node/{node_id}/commandStatus",
                      params={"xRequestId": x_request_id})
        return d.get("commands") or []

    def wait_command(self, ns, infra_id, node_ids, x_request_id, timeout_sec, poll=15, on_tick=None):
        """모든 노드의 해당 xRequestId 명령이 끝날 때까지. {nodeId: 마지막 상태 항목}."""
        t0 = time.monotonic()
        while True:
            done, pending = {}, []
            for nid in node_ids:
                entries = self.command_status(ns, infra_id, nid, x_request_id)
                last = entries[-1] if entries else None
                if last and last.get("status") in TERMINAL:
                    done[nid] = last
                else:
                    pending.append((nid, last.get("status") if last else "없음"))
            if on_tick:
                on_tick(done, pending, int(time.monotonic() - t0))
            if not pending:
                return done
            if time.monotonic() - t0 > timeout_sec:
                raise TBError(f"원격 명령 {x_request_id} 가 {timeout_sec}초 안에 끝나지 않았습니다: {pending}")
            time.sleep(poll)
