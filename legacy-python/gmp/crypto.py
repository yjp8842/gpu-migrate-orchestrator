"""Beetle 데이터 이관의 비밀 필드 암호화 — 서버(test/encrypt)로 확인한 형식 (2026-10-22, Beetle UI 프록시 3080).

봉투(필드 값 하나) = base64( JSON {"v": 1, "ek": b64(RSA-OAEP-SHA256 으로 감싼 32바이트 AES 키),
                                     "n": b64(12바이트 nonce), "ct": b64(AES-256-GCM 암호문 + 16바이트 태그)} )
keyId 는 봉투가 아니라 모델의 `encryptionKeyId` 에 둔다. 키 번들 = GET /migration/data/encryptionKey 의 data
{keyId, algorithm: "RSA-OAEP-256+AES-256-GCM", publicKey(PEM), expiresAt} — 30분 · 1회용.
cryptography 패키지는 쓸 때 가져온다 (Tumblebug 만 쓰는 경로엔 필요 없다).
"""
import base64
import json
import os


class CryptoError(Exception):
    pass


def _lib():
    try:
        from cryptography.hazmat.primitives import hashes, serialization
        from cryptography.hazmat.primitives.asymmetric import padding
        from cryptography.hazmat.primitives.ciphers.aead import AESGCM
    except ImportError as e:
        raise CryptoError("cryptography 패키지가 필요합니다: .venv/bin/pip install cryptography") from e
    return hashes, serialization, padding, AESGCM


def _b64(b):
    return base64.b64encode(b).decode("ascii")


class Encryptor:
    """번들 하나로 여러 필드를 암호화한다 (필드마다 새 AES 키 · nonce)."""

    ALGORITHM = "RSA-OAEP-256+AES-256-GCM"

    def __init__(self, bundle):
        if not bundle:
            raise CryptoError("encryptionKey 번들이 비어 있습니다")
        self.key_id = bundle.get("keyId")
        pem = bundle.get("publicKey")
        if not self.key_id or not pem:
            raise CryptoError(f"번들에 keyId · publicKey 가 없습니다: {sorted(bundle)}")
        alg = bundle.get("algorithm")
        if alg and alg != self.ALGORITHM:
            raise CryptoError(f"알 수 없는 알고리즘: {alg} (이 도구는 {self.ALGORITHM} 만 안다)")
        hashes, serialization, padding, AESGCM = _lib()
        self._hashes, self._padding, self._AESGCM = hashes, padding, AESGCM
        self.pub = serialization.load_pem_public_key(pem.encode("utf-8"))

    def encrypt(self, plaintext):
        aes_key = os.urandom(32)
        nonce = os.urandom(12)
        ct = self._AESGCM(aes_key).encrypt(nonce, plaintext.encode("utf-8"), None)
        ek = self.pub.encrypt(aes_key, self._padding.OAEP(mgf=self._padding.MGF1(self._hashes.SHA256()),
                                                           algorithm=self._hashes.SHA256(), label=None))
        env = {"v": 1, "ek": _b64(ek), "n": _b64(nonce), "ct": _b64(ct)}
        return _b64(json.dumps(env, separators=(",", ":")).encode("utf-8"))


def _sample_model(pk_src, pk_dst):
    """test 엔드포인트용 가짜 모델 — 문서용 IP(198.51.100.0/24)와 가짜 키만 쓴다."""
    def side(path, host, pk):
        return {"storageType": "filesystem", "path": path,
                "filesystem": {"accessType": "ssh", "ssh": {"host": host, "port": 22, "username": "selftest", "privateKey": pk}}}
    return {"source": side("/tmp/gmp-selftest-src", "198.51.100.10", pk_src),
            "destination": side("/tmp/gmp-selftest-dst", "198.51.100.11", pk_dst), "strategy": "direct"}


def selftest(beetle, log):
    """우리 봉투를 서버가 푸는지 확인한다 — 가짜 값만 보낸다. 성공하면 True."""
    sample = ("gmp-selftest-sample-src", "gmp-selftest-sample-dst")
    bundle = beetle.encryption_key()
    log.info(f"encryptionKey: keyId {bundle.get('keyId')} · {bundle.get('algorithm')} · 만료 {bundle.get('expiresAt')}")
    enc = Encryptor(bundle)
    model = _sample_model(enc.encrypt(sample[0]), enc.encrypt(sample[1]))
    model["encryptionKeyId"] = enc.key_id
    dec = beetle.test_decrypt(model)
    data = dec.get("data") if isinstance(dec, dict) and "data" in dec else dec
    got = (((data or {}).get("source") or {}).get("filesystem") or {}).get("ssh", {}).get("privateKey"), \
          (((data or {}).get("destination") or {}).get("filesystem") or {}).get("ssh", {}).get("privateKey")
    ok = got == sample
    if ok:
        log.info("우리 봉투를 서버가 풀었습니다 — source · destination privateKey 모두 일치")
    else:
        log.warn(f"복호 결과가 다릅니다: {got!r} (기대 {sample!r}) — 응답: {str(dec)[:300]}")
    return ok
