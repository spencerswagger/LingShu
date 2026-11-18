package magpieaccess

import (
	"strings"
	"testing"
	"time"
)

func testSigner(t *testing.T, seed byte) *Signer {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = seed
	}
	s := NewSigner(key)
	if s == nil {
		t.Fatal("NewSigner 返回 nil")
	}
	return s
}

func TestTicket_Roundtrip(t *testing.T) {
	s := testSigner(t, 1)
	tok, exp, err := s.Issue(7, "ADMIN", time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if tok == "" || !strings.HasPrefix(tok, "v1.") {
		t.Fatalf("票据形状不符：%q", tok)
	}
	if !exp.After(time.Now()) {
		t.Fatalf("过期时间不在未来：%v", exp)
	}
	got, err := s.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.UID != 7 || got.Role != "ADMIN" {
		t.Fatalf("载荷不符：%+v", got)
	}
}

func TestTicket_Expired(t *testing.T) {
	s := testSigner(t, 3)
	base := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return base })
	tok, _, err := s.Issue(1, "ADMIN", time.Second)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	// 到点即失效（<= now 判过期，避免「刚好这一秒还能用」的边界）。
	s.SetClock(func() time.Time { return base.Add(time.Second) })
	if _, err := s.Verify(tok); err == nil {
		t.Fatal("过期票据仍被接受")
	}
}

func TestTicket_Tampered(t *testing.T) {
	s := testSigner(t, 4)
	tok, _, _ := s.Issue(1, "ADMIN", time.Minute)
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("票据段数不符：%q", tok)
	}
	cases := map[string]string{
		"改载荷": parts[0] + "." + parts[1] + "x." + parts[2],
		"改签名": parts[0] + "." + parts[1] + "." + parts[2] + "x",
		"去签名": parts[0] + "." + parts[1] + ".",
		"换版本": "v2." + parts[1] + "." + parts[2],
		"截断":  parts[0] + "." + parts[1],
		"乱码":  "not-a-ticket",
		"空串":  "",
	}
	for name, in := range cases {
		if _, err := s.Verify(in); err == nil {
			t.Fatalf("%s：畸形票据被接受了（%q）", name, in)
		}
	}
}

// 换一把密钥（轮换 keys/magpie_jump.key）后，旧票据必须全部失效。
func TestTicket_KeyRotationInvalidates(t *testing.T) {
	a := testSigner(t, 5)
	b := testSigner(t, 6)
	tok, _, _ := a.Issue(1, "ADMIN", time.Minute)
	if _, err := a.Verify(tok); err != nil {
		t.Fatalf("同密钥校验失败：%v", err)
	}
	if _, err := b.Verify(tok); err == nil {
		t.Fatal("换密钥后旧票据仍被接受")
	}
}

func TestNewSigner_EmptyKeyIsNil(t *testing.T) {
	if NewSigner(nil) != nil || NewSigner([]byte{}) != nil {
		t.Fatal("空密钥必须返回 nil（装配层据此不挂载接口）")
	}
	var s *Signer
	if _, _, err := s.Issue(1, "ADMIN", time.Minute); err == nil {
		t.Fatal("nil 签发器不应签发成功")
	}
	if _, err := s.Verify("x"); err == nil {
		t.Fatal("nil 签发器不应校验通过")
	}
}

// 同一秒内两次签发必须不同（载荷带 nonce），否则日志/审计里会出现完全相同的串。
func TestTicket_IssueDiffersPerCall(t *testing.T) {
	s := testSigner(t, 7)
	s.SetClock(func() time.Time { return time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC) })
	a, _, _ := s.Issue(1, "ADMIN", time.Minute)
	b, _, _ := s.Issue(1, "ADMIN", time.Minute)
	if a == b {
		t.Fatal("同一秒内两次签发的票据完全相同（nonce 没生效）")
	}
}
