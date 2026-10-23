package logx

import (
	"strings"
	"testing"
)

func TestMaskSecrets(t *testing.T) {
	s := "x \"privateKey\":\"abc\" VERIFY_ADMIN_PASSWORD=pw -----BEGIN RSA PRIVATE KEY-----\nK\n-----END RSA PRIVATE KEY----- Authorization: Basic Zm9v"
	m := Mask(s)
	if strings.Contains(m, "abc") || strings.Contains(m, "=pw") || strings.Contains(m, "Zm9v") || !strings.Contains(m, "<PRIVATE KEY>") {
		t.Fatal(m)
	}
}
