// Package crypto 提供国密合规的加密原语：
//   - SM3 加盐口令哈希（用于账户口令存储与校验）
//   - SM4-GCM 字段加密（用于渠道 API Key 等敏感字段落库）
//
// 口令哈希存储格式："$pbkdf2-sm3$<iter>$<salt hex>$<hash hex>"（PBKDF2-HMAC-SM3）
// 字段密文存储格式：hex(nonce || ciphertext)（SM4-GCM，random nonce 前置）
package crypto

import (
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/emmansun/gmsm/sm3"
	"github.com/emmansun/gmsm/sm4"
)

// hashSaltLen 口令加盐长度（字节）。
const hashSaltLen = 16

// pbkdf2Iterations 口令哈希迭代次数（PBKDF2-HMAC-SM3）。
const pbkdf2Iterations = 200000

// HashPassword 对明文口令做 PBKDF2-HMAC-SM3，返回
// "$pbkdf2-sm3$<iter>$<salt hex>$<hash hex>"。每次调用使用随机盐。
func HashPassword(password string) (string, error) {
	salt := make([]byte, hashSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	dk := pbkdf2Key([]byte(password), salt, pbkdf2Iterations, sm3.Size)
	return fmt.Sprintf("$pbkdf2-sm3$%d$%x$%x", pbkdf2Iterations, salt, dk), nil
}

// VerifyPassword 校验口令是否匹配 PBKDF2-HMAC-SM3 存储值，常量时间比较。
func VerifyPassword(password, stored string) bool {
	if !strings.HasPrefix(stored, "$pbkdf2-sm3$") {
		return false
	}
	parts := strings.Split(stored, "$") // ["", "pbkdf2-sm3", iter, salt, hash]
	if len(parts) != 5 {
		return false
	}
	iter, err := strconv.Atoi(parts[2])
	if err != nil || iter <= 0 {
		return false
	}
	salt, err := hex.DecodeString(parts[3])
	if err != nil || len(salt) == 0 {
		return false
	}
	want, err := hex.DecodeString(parts[4])
	if err != nil {
		return false
	}
	dk := pbkdf2Key([]byte(password), salt, iter, sm3.Size)
	return len(dk) == len(want) && subtle.ConstantTimeCompare(dk, want) == 1
}

// pbkdf2Key 实现 PBKDF2（RFC 2898）with HMAC-SM3。
func pbkdf2Key(password, salt []byte, iter, keyLen int) []byte {
	prf := func(data []byte) []byte {
		m := hmac.New(sm3.New, password)
		_, _ = m.Write(data)
		return m.Sum(nil)
	}
	hashLen := sm3.Size
	numBlocks := (keyLen + hashLen - 1) / hashLen
	var dk []byte
	for block := 1; block <= numBlocks; block++ {
		buf := make([]byte, 0, len(salt)+4)
		buf = append(buf, salt...)
		buf = append(buf, byte(block>>24), byte(block>>16), byte(block>>8), byte(block))
		u := prf(buf)
		t := append([]byte(nil), u...)
		for i := 1; i < iter; i++ {
			u = prf(u)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:keyLen]
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