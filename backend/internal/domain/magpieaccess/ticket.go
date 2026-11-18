// Package magpieaccess 签发并校验「代理侧（magpie）管理面」的访问票据。
//
// 为什么需要票据：灵枢的登录态是 localStorage 里的 Bearer JWT，浏览器**导航**不携带
// Authorization 头，nginx 的 auth_request 子请求因此永远拿不到当前会话 —— 直接拿
// auth_request 判「是不是 ADMIN」必然把管理员也挡在外面。
//
// 做法：管理员在灵枢点按钮 → 灵枢校验会话（RoleAdmin）→ 签发一张 8 小时的票据，
// 用 **HttpOnly cookie**（`magpie_gate`，Path=/magpie/）种到浏览器 → 之后 nginx 每个
// `/magpie/**` 请求都用 auth_request 回灵枢验一次签，过不了直接 403，请求根本到不了 magpie。
//
// 票据全程不进 URL、不进 localStorage、不是灵枢的会话 JWT（拿去调 /api/v1/** 一律 401）。
// 无状态、不落库：轮换 keys/magpie_jump.key 即让所有已发出的票据失效。
package magpieaccess

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// CookieName 是票据在浏览器里的 cookie 名。nginx 的 `$cookie_magpie_gate` 读的就是它。
const CookieName = "magpie_gate"

// CookiePath 限定 cookie 只在管理面路径下发（不跟着灵枢自己的请求到处跑）。
const CookiePath = "/magpie/"

// ticketVersion 写在第一个字段，便于以后改格式时立刻区分新旧。
const ticketVersion = "v1"

// ErrBadTicket 覆盖全部校验失败原因（签名不符、格式不符、过期）。
//
// 对外一律同一条错误：调用方是 nginx 的 auth_request，它只需要「过 / 不过」；
// 把原因细分只会诱使别人拿它做探测面。
var ErrBadTicket = errors.New("magpieaccess: 票据无效")

// Ticket 是票据载荷（明文段，签名保护完整性）。不含任何可拿去冒充灵枢会话的东西。
type Ticket struct {
	UID   int64  `json:"u"`
	Role  string `json:"r"`
	IssAt int64  `json:"i"` // Unix 秒
	ExpAt int64  `json:"e"` // Unix 秒
}

// Signer 用 HMAC-SHA256 签发/校验票据。key 由装配层从 keys/magpie_jump.key 读取
// （与 SM4/JWT 密钥同法自举），为空时 NewSigner 返回 nil —— 调用方据此不挂载签发接口，
// 而不是发一批谁都能伪造的票据。
type Signer struct {
	key []byte
	now func() time.Time
}

// NewSigner 创建签发器；key 为空返回 nil。
func NewSigner(key []byte) *Signer {
	if len(key) == 0 {
		return nil
	}
	k := make([]byte, len(key))
	copy(k, key)
	return &Signer{key: k, now: time.Now}
}

// SetClock 仅供单测注入固定时钟。
func (s *Signer) SetClock(now func() time.Time) {
	if s != nil && now != nil {
		s.now = now
	}
}

// Issue 签发一张票据，返回票据与过期时刻。
func (s *Signer) Issue(uid int64, role string, ttl time.Duration) (string, time.Time, error) {
	if s == nil {
		return "", time.Time{}, ErrBadTicket
	}
	now := s.now()
	exp := now.Add(ttl)
	// nonce：同一秒内两次签发也不同（否则日志/审计里会出现完全相同的串）。
	var n [6]byte
	if _, err := rand.Read(n[:]); err != nil {
		return "", time.Time{}, err
	}
	raw, err := json.Marshal(struct {
		Ticket
		N string `json:"n"`
	}{Ticket: Ticket{UID: uid, Role: role, IssAt: now.Unix(), ExpAt: exp.Unix()}, N: hex.EncodeToString(n[:])})
	if err != nil {
		return "", time.Time{}, err
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	return ticketVersion + "." + body + "." + s.sign(body), exp, nil
}

// Verify 校验票据，返回载荷。
func (s *Signer) Verify(token string) (Ticket, error) {
	if s == nil {
		return Ticket{}, ErrBadTicket
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != ticketVersion {
		return Ticket{}, ErrBadTicket
	}
	if !hmac.Equal([]byte(parts[2]), []byte(s.sign(parts[1]))) {
		return Ticket{}, ErrBadTicket
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Ticket{}, ErrBadTicket
	}
	var t Ticket
	if err := json.Unmarshal(raw, &t); err != nil {
		return Ticket{}, ErrBadTicket
	}
	if t.ExpAt <= s.now().Unix() {
		return Ticket{}, ErrBadTicket
	}
	return t, nil
}

func (s *Signer) sign(body string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(ticketVersion + "." + body))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
