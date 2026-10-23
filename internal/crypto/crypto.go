// Package crypto 는 Beetle 데이터 이관의 비밀 필드(SSH 개인키) 암호화다 — 서버(test/decrypt)로 확인한 형식.
//
// 봉투(필드 값 하나) = base64( JSON {"v": 1, "ek": b64(RSA-OAEP-SHA256 으로 감싼 32바이트 AES 키),
// "n": b64(12바이트 nonce), "ct": b64(AES-256-GCM 암호문 + 16바이트 태그)} )
// keyId 는 봉투가 아니라 모델의 encryptionKeyId 에 둔다. 키 번들 = GET /migration/data/encryptionKey 의 data
// {keyId, algorithm: "RSA-OAEP-256+AES-256-GCM", publicKey(PEM), expiresAt} — 30분 · 1회용.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"

	"gpu-migrate-orchestrator/internal/jsonx"
)

// Algorithm 은 이 도구가 아는 유일한 알고리즘이다.
const Algorithm = "RSA-OAEP-256+AES-256-GCM"

// Encryptor 는 번들 하나로 여러 필드를 암호화한다 (필드마다 새 AES 키 · nonce).
type Encryptor struct {
	KeyID string
	pub   *rsa.PublicKey
}

// New 는 encryptionKey 번들로 Encryptor 를 만든다.
func New(bundle jsonx.M) (*Encryptor, error) {
	if bundle == nil {
		return nil, fmt.Errorf("encryptionKey 번들이 비어 있습니다")
	}
	keyID, pemText := jsonx.S(bundle, "keyId"), jsonx.S(bundle, "publicKey")
	if keyID == "" || pemText == "" {
		return nil, fmt.Errorf("번들에 keyId · publicKey 가 없습니다: %v", jsonx.SortedKeys(bundle))
	}
	if alg := jsonx.S(bundle, "algorithm"); alg != "" && alg != Algorithm {
		return nil, fmt.Errorf("알 수 없는 알고리즘: %s (이 도구는 %s 만 안다)", alg, Algorithm)
	}
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, fmt.Errorf("공개키 PEM 을 읽지 못했습니다")
	}
	var pub *rsa.PublicKey
	if k, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		rk, ok := k.(*rsa.PublicKey)
		if !ok {
			return nil, fmt.Errorf("RSA 공개키가 아닙니다")
		}
		pub = rk
	} else if rk, err2 := x509.ParsePKCS1PublicKey(block.Bytes); err2 == nil {
		pub = rk
	} else {
		return nil, fmt.Errorf("공개키를 읽지 못했습니다: %v", err)
	}
	return &Encryptor{KeyID: keyID, pub: pub}, nil
}

type envelope struct {
	V  int    `json:"v"`
	EK string `json:"ek"`
	N  string `json:"n"`
	CT string `json:"ct"`
}

// Encrypt 는 평문 하나를 봉투로 바꾼다.
func (e *Encryptor) Encrypt(plain string) (string, error) {
	key := make([]byte, 32)
	nonce := make([]byte, 12)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	ct := gcm.Seal(nil, nonce, []byte(plain), nil)
	ek, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, e.pub, key, nil)
	if err != nil {
		return "", err
	}
	b64 := base64.StdEncoding.EncodeToString
	raw, err := jsonx.Marshal(envelope{V: 1, EK: b64(ek), N: b64(nonce), CT: b64(ct)}, false)
	if err != nil {
		return "", err
	}
	return b64(raw), nil
}

// SampleModel 은 test 엔드포인트용 가짜 모델이다 — 문서용 IP(198.51.100.0/24)와 가짜 키만 쓴다.
func SampleModel(pkSrc, pkDst string) jsonx.M {
	side := func(path, host, pk string) jsonx.M {
		return jsonx.M{"storageType": "filesystem", "path": path,
			"filesystem": jsonx.M{"accessType": "ssh", "ssh": jsonx.M{"host": host, "port": 22, "username": "selftest", "privateKey": pk}}}
	}
	return jsonx.M{"source": side("/tmp/gmp-selftest-src", "198.51.100.10", pkSrc),
		"destination": side("/tmp/gmp-selftest-dst", "198.51.100.11", pkDst), "strategy": "direct"}
}

// SelftestSamples 는 selftest 가 암호화해 보내는 가짜 평문이다.
var SelftestSamples = [2]string{"gmp-selftest-sample-src", "gmp-selftest-sample-dst"}

// DecryptedKeys 는 test/decrypt 응답에서 source · destination 의 privateKey 를 꺼낸다.
func DecryptedKeys(resp any) (string, string) {
	data := resp
	if m := jsonx.AsM(resp); m != nil {
		if d, ok := m["data"]; ok {
			data = d
		}
	}
	return jsonx.Str(jsonx.Get(data, "source", "filesystem", "ssh", "privateKey")),
		jsonx.Str(jsonx.Get(data, "destination", "filesystem", "ssh", "privateKey"))
}
