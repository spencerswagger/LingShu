// Package jwtx 提供基于 RS256 的 JWT 签发与校验，用于管理/开发者会话。
// 使用 PEM 格式的 RSA 私钥/公钥文件初始化 Manager。
package jwtx

import (
	"crypto/rsa"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims 是签发到 JWT 中的自定义会话载荷。
type Claims struct {
	UserID   int64  `json:"uid"`
	Username string `json:"username"`
	Role     string `json:"role"`
	jwt.RegisteredClaims
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

// Sign 为指定用户签发一个 RS256 JWT。
func (m *Manager) Sign(userID int64, username, role string) (string, error) {
	now := time.Now()
	claims := Claims{
		UserID:   userID,
		Username: username,
		Role:     role,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(m.ttl)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(m.priv)
}

// Parse 校验 JWT 签名与有效期并解析载荷，非 RS256 算法一律拒绝。
func (m *Manager) Parse(token string) (*Claims, error) {
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return m.pub, nil
	}, jwt.WithValidMethods([]string{"RS256"}))
	if err != nil {
		return nil, err
	}
	return claims, nil
}