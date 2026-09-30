// Package crypto 提供国密合规的加密原语：
//   - SM3 加盐口令哈希（用于账户口令存储与校验）
//   - SM4-GCM 字段加密（用于渠道 API Key 等敏感字段落库）
//
// 口令哈希存储格式：hex(salt) + "$" + hex(SM3(salt || password))
// 字段密文存储格式：hex(nonce || ciphertext)（SM4-GCM，random nonce 前置）
package crypto

import (
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/emmansun/gmsm/sm3"
	"github.com/emmansun/gmsm/sm4"
)

// hashSaltLen 口令加盐长度（字节）。
const hashSaltLen = 16

// HashPassword 对明文口令加盐后做 SM3 哈希，返回 hex(salt)$hex(hash)。
// 每次调用使用随机盐，保证同一口令多次哈希结果不同。
func HashPassword(password string) (string, error) {
	salt := make([]byte, hashSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	h := sm3.Sum(append(salt, []byte(password)...))
	return hex.EncodeToString(salt) + "$" + hex.EncodeToString(h[:]), nil
}

// VerifyPassword 校验口令是否匹配存储的哈希值。
// 使用 constant-time 比较，避免时序侧信道。
func VerifyPassword(password, stored string) bool {
	parts := strings.SplitN(stored, "$", 2)
	if len(parts) != 2 {
		return false
	}
	salt, err := hex.DecodeString(parts[0])
	if err != nil || len(salt) == 0 {
		return false
	}
	h := sm3.Sum(append(salt, []byte(password)...))
	want, err := hex.DecodeString(parts[1])
	if err != nil {
		return false
	}
	return len(h) == len(want) && subtle.ConstantTimeCompare(h[:], want) == 1
}

// SM4Encrypt 使用 SM4-GCM 对明文加密，random nonce 前置，
// 返回 hex(nonce || ciphertext)。
func SM4Encrypt(key, plaintext []byte) (string, error) {
	block, err := sm4.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nil, nonce, plaintext, nil)
	return hex.EncodeToString(append(nonce, ct...)), nil
}

// SM4Decrypt 解密 SM4Encrypt 的输出。
func SM4Decrypt(key []byte, cipherText string) ([]byte, error) {
	raw, err := hex.DecodeString(cipherText)
	if err != nil {
		return nil, err
	}
	block, err := sm4.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(raw) < gcm.NonceSize()+gcm.Overhead() {
		return nil, errors.New("ciphertext too short")
	}
	nonce := raw[:gcm.NonceSize()]
	ct := raw[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ct, nil)
}