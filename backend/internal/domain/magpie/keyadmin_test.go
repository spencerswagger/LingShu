// keyadmin_test.go 覆盖 magpie_keys 的写入方（管理端绑定端点）：
// 幂等 upsert、明文只进密文列、校验口径、约束翻译（409/404），以及 GET/DELETE 口径。
package magpie

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/team/llmgateway/internal/pkg/crypto"
)

// fakeBinder 记录写入的参数（断言「落库的是密文」需要看到实际参数）。
type fakeBinder struct {
	row       *Key
	err       error
	gotCipher string
	gotName   string
	disabled  bool
}

func (f *fakeBinder) ByTokenID(context.Context, int64) (*Key, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.row == nil {
		return nil, ErrKeyNotProvisioned
	}
	return f.row, nil
}

func (f *fakeBinder) Provision(_ context.Context, tokenID int64, keyName, keyCipher string) (*Key, error) {
	f.gotName, f.gotCipher = keyName, keyCipher
	if f.err != nil {
		return nil, f.err
	}
	return &Key{ID: 1, TokenID: tokenID, KeyName: keyName, KeyCipher: keyCipher, State: KeyStateActive}, nil
}

func (f *fakeBinder) Disable(context.Context, int64) error {
	if f.err != nil {
		return f.err
	}
	f.disabled = true
	return nil
}

func newKeyAdminWith(b KeyBinder) *KeyAdminHandler { return NewKeyAdminHandler(b, testCredKey) }

func keyReq(method, tokenID, body string) *http.Request {
	r := httptest.NewRequest(method, "/api/v1/admin/tokens/"+tokenID+"/magpie-key", strings.NewReader(body))
	r.SetPathValue("id", tokenID)
	return r
}

type envelope struct {
	Code    int             `json:"Code"`
	Message string          `json:"Message"`
	Data    json.RawMessage `json:"Data"`
}

func decodeBindingView(t *testing.T, w *httptest.ResponseRecorder) keyBindingView {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	var view keyBindingView
	if len(env.Data) > 0 {
		_ = json.Unmarshal(env.Data, &view)
	}
	return view
}

// PUT 成功：落库的是密文（可解回明文），回包不含明文。
func TestKeyAdmin_PutStoresCipherOnly(t *testing.T) {
	b := &fakeBinder{}
	h := newKeyAdminWith(b)

	w := httptest.NewRecorder()
	h.HandlePut(w, keyReq(http.MethodPut, "7", `{"KeyName":" core-k1 ","CallerKey":" secret-plain "}`))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if b.gotName != "core-k1" {
		t.Fatalf("KeyName 应 trim: %q", b.gotName)
	}
	if b.gotCipher == "" || strings.Contains(b.gotCipher, "secret-plain") {
		t.Fatalf("落库内容必须是密文: %q", b.gotCipher)
	}
	plain, err := crypto.SM4Decrypt(testCredKey, b.gotCipher)
	if err != nil || string(plain) != "secret-plain" {
		t.Fatalf("密文回环失败: plain=%q err=%v", plain, err)
	}
	view := decodeBindingView(t, w)
	if !view.Bound || view.State != KeyStateActive || view.KeyName != "core-k1" {
		t.Fatalf("回包=%s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "secret-plain") {
		t.Fatalf("回包泄漏明文: %s", w.Body.String())
	}
}

// PUT 校验：KeyName/CallerKey 必填、有长度上限、坏 JSON 400。
func TestKeyAdmin_PutValidation(t *testing.T) {
	h := newKeyAdminWith(&fakeBinder{})
	cases := map[string]string{
		"空 KeyName":    `{"KeyName":"  ","CallerKey":"k"}`,
		"空 CallerKey":  `{"KeyName":"core-k1","CallerKey":" "}`,
		"坏 JSON":       `{`,
		"KeyName 过长":   `{"KeyName":"` + strings.Repeat("k", maxKeyNameLen+1) + `","CallerKey":"k"}`,
		"CallerKey 过长": `{"KeyName":"core-k1","CallerKey":"` + strings.Repeat("c", maxCallerKeyLen+1) + `"}`,
	}
	for name, body := range cases {
		w := httptest.NewRecorder()
		h.HandlePut(w, keyReq(http.MethodPut, "7", body))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s", name, w.Code, w.Body.String())
		}
	}
}

// 未配置 SM4 密钥 → 500（不能静默存明文）。
func TestKeyAdmin_PutWithoutCredKeyFails(t *testing.T) {
	h := NewKeyAdminHandler(&fakeBinder{}, nil)
	w := httptest.NewRecorder()
	h.HandlePut(w, keyReq(http.MethodPut, "7", `{"KeyName":"k1","CallerKey":"s"}`))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

// 约束翻译：key_name 唯一冲突 → 409；token 外键失败 → 404；其它 → 500。
func TestKeyAdmin_PutConstraintErrors(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{"同名占用", &pgconn.PgError{Code: "23505"}, http.StatusConflict},
		{"令牌不存在", &pgconn.PgError{Code: "23503"}, http.StatusNotFound},
		{"其它错误", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		h := newKeyAdminWith(&fakeBinder{err: c.err})
		w := httptest.NewRecorder()
		h.HandlePut(w, keyReq(http.MethodPut, "7", `{"KeyName":"core-k1","CallerKey":"secret"}`))
		if w.Code != c.status {
			t.Fatalf("%s: status=%d want %d body=%s", c.name, w.Code, c.status, w.Body.String())
		}
	}
}

// GET：有映射回元数据（无密文）；无映射回 Bound=false。
func TestKeyAdmin_Get(t *testing.T) {
	h := newKeyAdminWith(&fakeBinder{row: &Key{ID: 1, TokenID: 7, KeyName: "core-k1", KeyID: "kid-9", KeyCipher: "cipher", State: KeyStateActive}})
	w := httptest.NewRecorder()
	h.HandleGet(w, keyReq(http.MethodGet, "7", ""))
	view := decodeBindingView(t, w)
	if !view.Bound || !view.HasKeyID || view.KeyName != "core-k1" {
		t.Fatalf("回包=%s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "cipher") {
		t.Fatalf("回包不该带密文: %s", w.Body.String())
	}

	h2 := newKeyAdminWith(&fakeBinder{})
	w2 := httptest.NewRecorder()
	h2.HandleGet(w2, keyReq(http.MethodGet, "8", ""))
	if decodeBindingView(t, w2).Bound {
		t.Fatalf("无映射应为 Bound=false: %s", w2.Body.String())
	}
}

// DELETE：停用成功回 DISABLED；没有映射 → 404。
func TestKeyAdmin_Delete(t *testing.T) {
	b := &fakeBinder{}
	h := newKeyAdminWith(b)
	w := httptest.NewRecorder()
	h.HandleDelete(w, keyReq(http.MethodDelete, "7", ""))
	if !b.disabled || decodeBindingView(t, w).State != KeyStateDisabled {
		t.Fatalf("回包=%s disabled=%v", w.Body.String(), b.disabled)
	}

	h2 := newKeyAdminWith(&fakeBinder{err: ErrKeyNotProvisioned})
	w2 := httptest.NewRecorder()
	h2.HandleDelete(w2, keyReq(http.MethodDelete, "9", ""))
	if w2.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", w2.Code, w2.Body.String())
	}
}

// 路径 id 非法 → 400。
func TestKeyAdmin_BadPathID(t *testing.T) {
	h := newKeyAdminWith(&fakeBinder{})
	r := httptest.NewRequest(http.MethodPut, "/api/v1/admin/tokens/abc/magpie-key", strings.NewReader(`{}`))
	r.SetPathValue("id", "abc")
	w := httptest.NewRecorder()
	h.HandlePut(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", w.Code)
	}
}
