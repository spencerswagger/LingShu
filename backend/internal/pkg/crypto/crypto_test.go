package crypto

import (
	"strings"
	"testing"
)

func TestSM3HashAndVerify(t *testing.T) {
	h, err := HashPassword("admin123")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h, "$") {
		t.Fatal("stored hash should contain '$' separator")
	}
	if !VerifyPassword("admin123", h) {
		t.Fatal("verify should pass")
	}
	if VerifyPassword("wrong", h) {
		t.Fatal("verify should fail")
	}
	// 同一口令两次哈希 salt 不同
	h2, err := HashPassword("admin123")
	if err != nil {
		t.Fatal(err)
	}
	if h == h2 {
		t.Fatal("salt should randomize stored hash")
	}
}

func TestVerifyPasswordMalformed(t *testing.T) {
	if VerifyPassword("x", "not-a-valid-format") {
		t.Fatal("malformed stored hash should fail")
	}
	if VerifyPassword("x", "$00") {
		t.Fatal("empty salt should fail")
	}
}

func TestSM4Roundtrip(t *testing.T) {
	key := []byte("0123456789abcdef") // 16B SM4 密钥
	pt := "sk-aliyun-secret-123"
	ct, err := SM4Encrypt(key, []byte(pt))
	if err != nil {
		t.Fatal(err)
	}
	dt, err := SM4Decrypt(key, ct)
	if err != nil {
		t.Fatal(err)
	}
	if string(dt) != pt {
		t.Fatal("roundtrip mismatch")
	}
}

func TestSM4CiphertextRandomized(t *testing.T) {
	key := []byte("0123456789abcdef")
	pt := []byte("sk-aliyun-secret-123")
	c1, err := SM4Encrypt(key, pt)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := SM4Encrypt(key, pt)
	if err != nil {
		t.Fatal(err)
	}
	if c1 == c2 {
		t.Fatal("same plaintext+key should produce different ciphertext (random nonce)")
	}
}

func TestSM4WrongKeyFails(t *testing.T) {
	key := []byte("0123456789abcdef")
	ct, err := SM4Encrypt(key, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	wrongKey := []byte("fedcba9876543210")
	if _, err := SM4Decrypt(wrongKey, ct); err == nil {
		t.Fatal("decrypt with wrong key should fail")
	}
}

func TestHashPassword_PBKDF2Format(t *testing.T) {
	h, err := HashPassword("s3cret-pw!A")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(h, "$pbkdf2-sm3$200000$") {
		t.Fatalf("unexpected format: %q", h)
	}
	if !VerifyPassword("s3cret-pw!A", h) {
		t.Fatal("verify should pass")
	}
	if VerifyPassword("wrong", h) {
		t.Fatal("verify wrong password should fail")
	}
	if VerifyPassword("s3cret-pw!A", "bogus") {
		t.Fatal("verify malformed stored should fail")
	}
}