"""콘솔 + run/log.txt + run/http.jsonl. 비밀값(개인키 · 비밀번호 · Authorization)은 어디에도 평문으로 남기지 않는다."""
import hashlib
import json
import re
import sys
import time

_MASKS = [
    (re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----", re.S), "<PRIVATE KEY>"),
    (re.compile(r'("(?:privateKey|password|nodeUserPassword|secretAccessKey|sessionToken|accessKeyId)"\s*:\s*")[^"]*(")', re.I),
     r"\1<masked>\2"),
    (re.compile(r"((?:VERIFY_ADMIN_PASSWORD|LOCAL_ADMIN_PASSWORD|AWS_SECRET_ACCESS_KEY|AWS_SESSION_TOKEN|AWS_ACCESS_KEY_ID|GMP_[A-Z_]*PASSWORD)=)[^\s'\"]+"),
     r"\1<masked>"),
    (re.compile(r"(Authorization:\s*Basic\s+)\S+", re.I), r"\1<masked>"),
]


def mask(text):
    """로그 · 증적에 쓰기 전에 비밀값을 가린다 (문자열이 아니면 그대로)."""
    if not isinstance(text, str):
        return text
    for pat, rep in _MASKS:
        text = pat.sub(rep, text)
    return text


class Log:
    def __init__(self, run_dir=None, verbose=True):
        self.run_dir = run_dir
        self.verbose = verbose
        self._fh = None
        self._http = None
        if run_dir is not None:
            run_dir.mkdir(parents=True, exist_ok=True)
            self._fh = open(run_dir / "log.txt", "a", encoding="utf-8")
            self._http = open(run_dir / "http.jsonl", "a", encoding="utf-8")

    def _emit(self, level, msg, stage):
        ts = time.strftime("%H:%M:%S")
        prefix = f"[{ts}] [{stage}] " if stage else f"[{ts}] "
        line = f"{prefix}{'' if level == 'INFO' else level + ' '}{mask(str(msg))}"
        stream = sys.stderr if level in ("WARN", "ERROR") else sys.stdout
        if self.verbose or level != "INFO":
            print(line, file=stream, flush=True)
        if self._fh:
            self._fh.write(line + "\n")
            self._fh.flush()

    def info(self, msg, stage=None):
        self._emit("INFO", msg, stage)

    def warn(self, msg, stage=None):
        self._emit("WARN", msg, stage)

    def error(self, msg, stage=None):
        self._emit("ERROR", msg, stage)

    def http(self, method, url, status, elapsed, body=None, note=None):
        """요청 한 건을 http.jsonl 에 — 본문은 해시만 (내용은 남기지 않는다)."""
        if not self._http:
            return
        rec = {"ts": time.strftime("%Y-%m-%dT%H:%M:%S"), "method": method, "url": mask(url),
               "status": status, "elapsed_ms": int(elapsed * 1000)}
        if body is not None:
            raw = body if isinstance(body, str) else json.dumps(body, ensure_ascii=False, sort_keys=True)
            rec["body_sha256"] = hashlib.sha256(raw.encode("utf-8")).hexdigest()[:16]
            rec["body_bytes"] = len(raw.encode("utf-8"))
        if note:
            rec["note"] = mask(str(note))
        self._http.write(json.dumps(rec, ensure_ascii=False) + "\n")
        self._http.flush()

    def close(self):
        for fh in (self._fh, self._http):
            if fh:
                fh.close()
