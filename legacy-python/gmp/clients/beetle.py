"""CM-Beetle REST 클라이언트 (v0.6.2 문서 기준 — 실 연결 검증 전).

- 추천 `POST /recommendation/infra` · 생성 `POST /migration/ns/{ns}/infra?nameSeed=` · `GET …/infra/{id}/ssh-ready`
- 데이터 이관 `GET /migration/data/encryptionKey` → `POST /migration/data` → `GET /request/{reqId}`
- 긴 호출은 `Prefer: respond-async` 로 202 + reqId 를 받고 폴링한다.
"""
import random
import time

import requests

from ..log import mask


def unwrap(resp):
    """Beetle 응답 래퍼 {success, data, message, error} 에서 data 를 꺼낸다 (래퍼가 아니면 그대로)."""
    if isinstance(resp, dict) and ("data" in resp or "success" in resp):
        if resp.get("success") is False:
            raise BeetleError(f"Beetle 오류: {resp.get('error') or resp.get('message')}")
        return resp.get("data")
    return resp


class BeetleError(Exception):
    def __init__(self, msg, status=None, body=None):
        super().__init__(msg)
        self.status = status
        self.body = body


class Beetle:
    def __init__(self, endpoint, log, dry_run=False, session=None, min_interval=0.3):
        self.ep = endpoint
        self.log = log
        self.dry_run = dry_run
        self.s = session or requests.Session()
        self.s.auth = endpoint.auth
        self.s.headers["Accept"] = "application/json"
        self.min_interval = min_interval
        self._last = 0.0

    def _req(self, method, path, *, params=None, json_body=None, headers=None, timeout=(10, 300), retry=True, allow404=False,
             write=None):
        # dry-run 은 조회만 한다. 쓰기 여부는 호출별로 — 추천은 POST 지만 조회이고, 암호화 키 발급은 GET 이지만 일회용 키를 만든다.
        if write is None:
            write = method != "GET"
        if self.dry_run and write:
            raise BeetleError(f"dry-run 에서는 Beetle 에 쓰지 않습니다: {method} {path}")
        url = f"{self.ep.url}{path}"
        attempt, delay = 0, 2.0
        while True:
            wait = self._last + self.min_interval - time.monotonic()
            if wait > 0:
                time.sleep(wait)
            self._last = time.monotonic()
            t0 = time.monotonic()
            try:
                r = self.s.request(method, url, params=params, json=json_body, headers=headers, timeout=timeout)
            except requests.RequestException as e:
                self.log.http(method, url, "EXC", time.monotonic() - t0, body=json_body, note=repr(e))
                if method == "GET" and retry and attempt < 5:
                    attempt += 1
                    time.sleep(delay + random.uniform(0, 1)); delay = min(delay * 2, 60)
                    continue
                raise BeetleError(f"{method} {path}: 연결 실패 ({e.__class__.__name__})") from e
            self.log.http(method, r.url, r.status_code, time.monotonic() - t0, body=json_body)
            if r.status_code == 429 or (r.status_code >= 500 and method == "GET" and retry):
                attempt += 1
                if attempt > 10:
                    raise BeetleError(f"{method} {path}: {r.status_code} 가 계속됩니다", r.status_code, r.text[:300])
                time.sleep(delay + random.uniform(0, 1)); delay = min(delay * 2, 60)
                continue
            if allow404 and r.status_code == 404:
                return None
            if r.status_code >= 400:
                raise BeetleError(f"{method} {path}: HTTP {r.status_code} — {mask(r.text[:400])}", r.status_code, r.text)
            if not r.content:
                return {}
            try:
                return r.json()
            except ValueError:
                return {"_text": r.text}

    # ---------- 추천 · 생성 ----------
    def recommend_infra(self, request, limit=5):
        """→ [cloudmodel.RecommendedInfra] (우선순위 순). 읽기 전용 — 자원을 만들지 않는다."""
        pair = request.get("desiredCspAndRegionPair") or {}
        return unwrap(self._req("POST", "/recommendation/infra", json_body=request,
                                params={"desiredCsp": pair.get("csp"), "desiredRegion": pair.get("region"), "limit": limit},
                                timeout=(10, 300), retry=False, write=False)) or []

    def migrate_infra(self, ns, body, name_seed=None, use_existing=True, timeout_sec=1800):
        """동기 생성 → controller.MigrateInfraResponse (id · status · node[] · creationErrors). 자동 재시도 없음."""
        params = {"useExisting": str(use_existing).lower()}
        if name_seed:
            params["nameSeed"] = name_seed
        return unwrap(self._req("POST", f"/migration/ns/{ns}/infra", json_body=body, params=params,
                                timeout=(10, timeout_sec), retry=False)) or {}

    def get_infra(self, ns, infra_id):
        return self._req("GET", f"/migration/ns/{ns}/infra/{infra_id}", allow404=True)

    def ssh_ready(self, ns, infra_id):
        """인프라당 30초에 1회 제한 — 호출 쪽에서 간격을 둔다."""
        data = unwrap(self._req("GET", f"/migration/ns/{ns}/infra/{infra_id}/ssh-ready", params={"option": "detail"}, timeout=(10, 60)))
        return bool((data or {}).get("ready")), data

    def delete_infra(self, ns, infra_id):
        return self._req("DELETE", f"/migration/ns/{ns}/infra/{infra_id}", params={"option": "terminate"}, timeout=(10, 900), retry=False)

    # ---------- 비동기 요청 ----------
    def request_status(self, req_id):
        """common.RequestDetails {status: Handling|Success|Error, startTime, endTime, errorResponse, responseData}"""
        return unwrap(self._req("GET", f"/request/{req_id}", timeout=(10, 60))) or {}

    def wait_request(self, req_id, timeout_sec, poll=15, on_tick=None):
        t0 = time.monotonic()
        while True:
            st = self.request_status(req_id)
            status = str(st.get("status") or "").lower()
            if on_tick:
                on_tick(status, int(time.monotonic() - t0))
            if status in ("success", "error", "failed", "completed"):
                return st
            if time.monotonic() - t0 > timeout_sec:
                raise BeetleError(f"요청 {req_id} 가 {timeout_sec}초 안에 끝나지 않았습니다 (지금 {status or '?'})")
            time.sleep(poll)

    # ---------- 데이터 이관 ----------
    def encryption_key(self):
        """일회용 RSA 공개키 번들 {keyId, algorithm, publicKey, expiresAt} (30분 · 1회)."""
        return unwrap(self._req("GET", "/migration/data/encryptionKey", timeout=(10, 60), write=True))

    def data_migration(self, body, x_request_id):
        """비동기(Prefer: respond-async)로 시작한다 — 기본은 동기라 큰 패키지면 HTTP 가 수십 분 열린다.
        요청 ID 는 우리가 정해 state 에 먼저 적는다 → 재개 때 같은 ID 로 상태만 본다."""
        d = self._req("POST", "/migration/data", json_body=body, timeout=(10, 120), retry=False,
                      headers={"Prefer": "respond-async", "X-Request-Id": x_request_id})
        data = unwrap(d) or {}
        return data.get("reqId") or x_request_id

    def test_encrypt(self, body):
        """[TEST ONLY] body = {model: 평문 DataMigrationModel, publicKeyBundle} → 암호화된 모델"""
        return self._req("POST", "/migration/data/test/encrypt", json_body=body, timeout=(10, 60), retry=False)

    def test_decrypt(self, body):
        return self._req("POST", "/migration/data/test/decrypt", json_body=body, timeout=(10, 60), retry=False)
