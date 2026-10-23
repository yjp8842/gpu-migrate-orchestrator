package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"testing"

	"gpu-migrate-orchestrator/internal/jsonx"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	bundle := jsonx.M{"keyId": "k1", "algorithm": Algorithm, "publicKey": string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}
	enc, err := New(bundle)
	if err != nil {
		t.Fatal(err)
	}
	out, err := enc.Encrypt("-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(out)
	var e struct {
		V         int
		EK, N, CT string
	}
	if err := json.Unmarshal(raw, &e); err != nil || e.V != 1 {
		t.Fatalf("봉투 형식: %s", raw)
	}
	if string(raw[:9]) != `{"v":1,"e` {
		t.Fatalf("필드 순서 · 공백: %s", raw)
	}
	b64 := base64.StdEncoding.DecodeString
	ek, _ := b64(e.EK)
	key, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, priv, ek, nil)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	n, _ := b64(e.N)
	ct, _ := b64(e.CT)
	plain, err := gcm.Open(nil, n, ct, nil)
	if err != nil || string(plain) != "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n" {
		t.Fatalf("복호 %q %v", plain, err)
	}
	if _, err := New(jsonx.M{"keyId": "k", "publicKey": "x", "algorithm": "RSA-OAEP"}); err == nil {
		t.Fatal("모르는 알고리즘을 받았다")
	}
}
