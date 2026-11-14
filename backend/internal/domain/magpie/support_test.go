package magpie

import (
	"net/http"

	"github.com/team/llmgateway/internal/pkg/crypto"
)

// testCredKey 是测试用 SM4 密钥（16 字节，与 security.sm4_key 同口径）。
var testCredKey = []byte("0123456789abcdef")

// encryptForTest 按生产口径加密 callerKey 明文（magpie_keys.key_cipher）。
func encryptForTest(plain string) (string, error) {
	return crypto.SM4Encrypt(testCredKey, []byte(plain))
}

// httpHeaderOf 把 map 转成 http.Header（测试构造请求头用）。
func httpHeaderOf(m map[string][]string) http.Header {
	h := http.Header{}
	for k, v := range m {
		h[k] = v
	}
	return h
}
