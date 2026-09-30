// Package jwtx 提供基于 RS256 的 JWT 签发与校验，用于管理/开发者会话。
// 使用 PEM 格式的 RSA 私钥/公钥文件初始化 Manager。
package jwtx

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"os"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	issuer   = "llmgateway"
	audience = "llmgateway-console"
)

// Claims 是签发到 JWT 中的自定义会话载荷，含 RFC 7519 全量标准字段。
type Claims struct {
	Username string `json:"username"`
	Role     string `json:"role"`
	Ver      int    `json:"ver"` // 会话代数：与 users.token_version 比对，仅最新有效
	jwt.RegisteredClaims
}

// UserID 从 Subject 解析用户 ID。
func (c *Claims) UserID() (int64, error) {
	return strconv.ParseInt(c.Subject, 10, 64)
}

// Manager 持有 RSA 密钥对与令牌有效期，负责签/解 JWT。
type Manager struct {
	priv *rsa.PrivateKey
	pub  *rsa.PublicKey
	ttl  time.Duration
}

// NewManager 从 PEM 私钥/公钥文件加载密钥，创建 JWT 管理器。
func NewManager(privPath, pubPath string, ttlMinutes int) (*Manager, error) {
	pb, err := os.ReadFile(privPath)
	if err != nil {
		return nil, err
	}
	priv, err := jwt.ParseRSAPrivateKeyFromPEM(pb)
	if err != nil {
		return nil, err
	}
	qb, err := os.ReadFile(pubPath)
	if err != nil {
		return nil, err
	}
	pub, err := jwt.ParseRSAPublicKeyFromPEM(qb)
	if err != nil {
		return nil, err
	}
	return &Manager{priv: priv, pub: pub, ttl: time.Duration(ttlMinutes) * time.Minute}, nil
}

// Sign 为指定用户签发 RS256 JWT，携带 ver 会话代数。
func (m *Manager) Sign(userID int64, username, role string, ver int) (string, error) {
	now := time.Now()
	claims := Claims{
		Username: username,
		Role:     role,
		Ver:      ver,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Subject:   strconv.FormatInt(userID, 10),
			Audience:  jwt.ClaimStrings{audience},
			ExpiresAt: jwt.NewNumericDate(now.Add(m.ttl)),
			NotBefore: jwt.NewNumericDate(now),
			IssuedAt:  jwt.NewNumericDate(now),
			ID:        newJTI(),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(m.priv)
}

// Parse 严格校验签名、算法、iss/aud/exp/nbf/iat，返回完整 claims。
func (m *Manager) Parse(token string) (*Claims, error) {
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return m.pub, nil
	},
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(issuer),
		jwt.WithAudience(audience),
		jwt.WithExpirationRequired())
	if err != nil {
		return nil, err
	}
	return claims, nil
}

func newJTI() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(b)
}
