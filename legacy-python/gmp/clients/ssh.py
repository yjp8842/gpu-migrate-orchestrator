"""SSH · scp · 스트림 전송 — 키는 run 아래 keys/(0700)에 0600 으로만 둔다. 명령줄 · 환경변수로 키 내용을 넘기지 않는다."""
import os
import pathlib
import shlex
import subprocess
import time


class SshError(Exception):
    pass


class KeyStore:
    def __init__(self, directory):
        self.dir = pathlib.Path(directory)
        self.dir.mkdir(parents=True, exist_ok=True)
        os.chmod(self.dir, 0o700)

    def write(self, name, pem):
        path = self.dir / f"{name}.pem"
        if not pem.endswith("\n"):
            pem += "\n"
        if path.exists():
            with open(path, encoding="utf-8") as f:
                if f.read() == pem:
                    return path
            path.unlink()
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            f.write(pem)
        return path

    def shred(self, name):
        path = self.dir / f"{name}.pem"
        if path.exists():
            with open(path, "r+b") as f:
                f.write(b"\0" * os.path.getsize(path))
            path.unlink()


class Ssh:
    def __init__(self, host, user, key_path, port=22, known_hosts=None, connect_timeout=15):
        self.host = host
        self.user = user
        self.key = str(key_path)
        self.port = int(port or 22)
        self.known_hosts = str(known_hosts) if known_hosts else os.devnull
        self.connect_timeout = connect_timeout

    def _opts(self):
        return ["-i", self.key, "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes",
                "-o", "StrictHostKeyChecking=accept-new", "-o", f"UserKnownHostsFile={self.known_hosts}",
                "-o", f"ConnectTimeout={self.connect_timeout}", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=4",
                "-o", "LogLevel=ERROR"]

    @property
    def target(self):
        return f"{self.user}@{self.host}"

    def ssh_argv(self, remote_cmd):
        return ["ssh", "-p", str(self.port), *self._opts(), self.target, remote_cmd]

    def run(self, remote_cmd, timeout=120, input_bytes=None):
        """(rc, stdout, stderr). 타임아웃이면 rc=124."""
        try:
            p = subprocess.run(self.ssh_argv(remote_cmd), input=input_bytes, capture_output=True, timeout=timeout)
        except subprocess.TimeoutExpired:
            return 124, "", f"timeout {timeout}s"
        return p.returncode, p.stdout.decode("utf-8", "replace"), p.stderr.decode("utf-8", "replace")

    def run_ok(self, remote_cmd, timeout=120):
        rc, out, err = self.run(remote_cmd, timeout=timeout)
        if rc != 0:
            raise SshError(f"ssh {self.target} '{remote_cmd[:80]}…' 실패 (rc={rc}): {err.strip()[-300:]}")
        return out

    def wait_ready(self, timeout_sec=600, poll=15):
        t0 = time.monotonic()
        while True:
            rc, _, err = self.run("true", timeout=30)
            if rc == 0:
                return True
            if time.monotonic() - t0 > timeout_sec:
                raise SshError(f"{self.target} 에 {timeout_sec}초 안에 SSH 로 들어가지 못했습니다: {err.strip()[-200:]}")
            time.sleep(poll)

    def scp_from(self, remote_path, local_path, timeout=900, recursive=False):
        argv = ["scp", "-P", str(self.port), *self._opts()]
        if recursive:
            argv.append("-r")
        argv += [f"{self.target}:{remote_path}", str(local_path)]
        try:
            p = subprocess.run(argv, capture_output=True, timeout=timeout)
        except subprocess.TimeoutExpired:
            raise SshError(f"scp {remote_path} 타임아웃 ({timeout}s)")
        if p.returncode != 0:
            raise SshError(f"scp {remote_path} 실패: {p.stderr.decode('utf-8', 'replace').strip()[-300:]}")
        return local_path

    def fetch_text(self, remote_path, timeout=120):
        """작은 텍스트 파일을 문자열로 (없으면 None)."""
        rc, out, _ = self.run(f"cat {shlex.quote(remote_path)} 2>/dev/null", timeout=timeout)
        return out if rc == 0 else None


def stream(src, src_cmd, dst, dst_cmd, timeout=3600):
    """소스에서 내보낸 바이트를 로컬을 거쳐 타겟으로 — `ssh 소스 'tar -cf - .' | ssh 타겟 'tar -xf -'`.
    (rc_src, rc_dst, stderr) 를 돌려준다."""
    p_src = subprocess.Popen(src.ssh_argv(src_cmd), stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    p_dst = subprocess.Popen(dst.ssh_argv(dst_cmd), stdin=p_src.stdout, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    p_src.stdout.close()
    try:
        out_dst, err_dst = p_dst.communicate(timeout=timeout)
        err_src = p_src.communicate(timeout=60)[1]
    except subprocess.TimeoutExpired:
        p_src.kill()
        p_dst.kill()
        raise SshError(f"스트림 전송 타임아웃 ({timeout}s)")
    errs = (err_src.decode("utf-8", "replace") + err_dst.decode("utf-8", "replace")).strip()
    return p_src.returncode, p_dst.returncode, errs
