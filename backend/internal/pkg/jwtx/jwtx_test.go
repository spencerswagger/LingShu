package jwtx

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func writePEM(t *testing.T, dir, name string, pemBytes []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, pemBytes, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func generateKeyFiles(t *testing.T) (privPath, pubPath string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(priv),
	})
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: pubDER,
	})
	dir := t.TempDir()
	return writePEM(t, dir, "jwt_priv.pem", privPEM),
		writePEM(t, dir, "jwt_pub.pem", pubPEM)
}

func TestSignParse(t *testing.T) {
	privPath, pubPath := generateKeyFiles(t)
	m, err := NewManager(privPath, pubPath, 60)
	if err != nil {
		t.Fatal(err)
	}

	token, err := m.Sign(7, "alice", "admin")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := m.Parse(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.UserID != 7 || claims.Username != "alice" || claims.Role != "admin" {
		t.Fatalf("claims mismatch: %+v", claims)
	}
}

func TestParseTamperedTokenFails(t *testing.T) {
	privPath, pubPath := generateKeyFiles(t)
	m, err := NewManager(privPath, pubPath, 60)
	if err != nil {
		t.Fatal(err)
	}
	token, err := m.Sign(1, "bob", "user")
	if err != nil {
		t.Fatal(err)
	}
	// 篡改 payload 中一个字符，签名必然失效
	tampered := token[:len(token)-4] + "AAAA"
	if _, err := m.Parse(tampered); err == nil {
		t.Fatal("tampered token should fail to parse")
	}
}

func TestParseExpiredFails(t *testing.T) {
	privPath, pubPath := generateKeyFiles(t)
	// ttl = -1 分钟，立即过期
	m, err := NewManager(privPath, pubPath, -1)
	if err != nil {
		t.Fatal(err)
	}
	token, err := m.Sign(2, "carol", "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Parse(token); err == nil {
		t.Fatal("expired token should fail to parse")
	}
}

func TestNewManagerBadKeys(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(bad, []byte("not a pem"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewManager(bad, bad, 60); err == nil {
		t.Fatal("bad private key should fail")
	}
}