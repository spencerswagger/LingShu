package jwtx

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
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

func newManager(t *testing.T) *Manager {
	t.Helper()
	privPath, pubPath := generateKeyFiles(t)
	m, err := NewManager(privPath, pubPath, 60)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (m *Manager) privKey() *rsa.PrivateKey {
	return m.priv
}

func TestSignParse(t *testing.T) {
	m := newManager(t)

	token, err := m.Sign(7, "alice", "admin", 1)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := m.Parse(token)
	if err != nil {
		t.Fatal(err)
	}
	uid, err := claims.UserID()
	if err != nil {
		t.Fatal(err)
	}
	if uid != 7 || claims.Username != "alice" || claims.Role != "admin" {
		t.Fatalf("claims mismatch: %+v", claims)
	}
}

func TestParseTamperedTokenFails(t *testing.T) {
	m := newManager(t)
	token, err := m.Sign(1, "bob", "user", 1)
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
	token, err := m.Sign(2, "carol", "user", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Parse(token); err == nil {
		t.Fatal("expired token should fail to parse")
	}
}

func TestManager_ClaimsRoundTrip(t *testing.T) {
	m := newManager(t)
	tok, err := m.Sign(42, "alice", "ADMIN", 7)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	c, err := m.Parse(tok)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Subject != "42" || c.Issuer != "llmgateway" || c.Role != "ADMIN" || c.Ver != 7 || c.Username != "alice" {
		t.Fatalf("unexpected claims: %+v", c)
	}
	if len(c.Audience) != 1 || c.Audience[0] != "llmgateway-console" {
		t.Fatalf("unexpected audience: %v", c.Audience)
	}
	if c.ID == "" {
		t.Fatal("jti should be set")
	}
	if c.NotBefore == nil || c.IssuedAt == nil || c.ExpiresAt == nil {
		t.Fatal("nbf/iat/exp should be set")
	}
}

func TestManager_Parse_RejectsWrongIssuer(t *testing.T) {
	m := newManager(t)
	// 手工构造 iss=evil 的 token（复用签名方法不允许覆盖 iss，故直接对 claims 对象签名）。
	now := time.Now()
	claims := Claims{
		Username: "alice", Role: "ADMIN", Ver: 1,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: "evil", Subject: "1", Audience: jwt.ClaimStrings{"llmgateway-console"},
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)), IssuedAt: jwt.NewNumericDate(now),
		},
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(m.privKey())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := m.Parse(tok); err == nil {
		t.Fatal("should reject wrong issuer")
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