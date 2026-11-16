// model_errors_test.go 固化 55 条矩阵挖出的三条 #81 缺陷：
//
//	P0-① 未启用/不存在模型曾返回 500 internal_error（准入只认 sql.ErrNoRows，漏了 model.ErrNotFound）；
//	P0-② 未映射代理模型曾返回 403 model_not_allowed，与「未开通 callerKey 的 403」混在一起；
//	P1-③ 顶层重复 model 曾把每一处都换成代理模型名（只该改生效的那一处）。
package magpie

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/team/llmgateway/internal/domain/model"
)

// errTestBoom 代表「真正的服务故障」（DB 挂了之类），必须仍是 500。
var errTestBoom = errors.New("db connection refused")

// ---- P0-① / P0-②：状态码与归类码 ----

// 生产路径：model.Service.GetByExternalName 对「不存在 / 已停用」都返回 model.ErrNotFound。
// 旧实现把它当未知错误 → 500「服务内部错误」（矩阵 B-11 实测 0ms、未打上游、0 账单）。
func TestAdmit_ModelErrNotFound_Is404Not500(t *testing.T) {
	a := NewAdmitter(
		&fakeTokens{tok: activeToken()},
		&fakeModels{err: model.ErrNotFound},
		&fakeKeys{key: &Key{ID: 1, TokenID: 7, KeyCipher: ""}},
		testCredKey, NewPrefixProvider(nil),
	)
	dec, deny := a.Admit(context.Background(), "plain-token", "tenant-gpt")
	if deny == nil {
		t.Fatalf("不存在的模型必须拒绝，实际放行: %+v", dec)
	}
	if deny.Status != http.StatusNotFound || deny.Code != CodeModelNotFound {
		t.Fatalf("deny = %d/%s，期望 404/model_not_found（不得是 500 internal_error）", deny.Status, deny.Code)
	}
	if deny.Message != modelUnavailableMessage {
		t.Fatalf("文案=%q，期望与「不存在」同一句（不泄漏该名字存在但被停用）: %q", deny.Message, modelUnavailableMessage)
	}
}

// 真服务故障仍必须是 500（别把「数据库挂了」也写成 404）。
func TestAdmit_OtherLookupError_Stays500(t *testing.T) {
	fk := &fakeModels{err: errTestBoom}
	a := NewAdmitter(&fakeTokens{tok: activeToken()}, fk, &fakeKeys{}, testCredKey, NewPrefixProvider(nil))
	_, deny := a.Admit(context.Background(), "plain-token", "m")
	if deny == nil || deny.Status != http.StatusInternalServerError || deny.Code != CodeInternal {
		t.Fatalf("未知错误应保持 500/internal_error，got %+v", deny)
	}
}

// 未映射（enabled 但没有 magpie_model）与「未开通 callerKey」必须用不同状态码 + 不同归类码，
// 文案也不得相同（矩阵 P-10：调用方要能分清「模型没接进来」和「我没这个权限」）。
func TestAdmit_UnmappedVsNoPermission_AreDistinguishable(t *testing.T) {
	cipher, err := encryptForTest("caller-secret")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	unmapped := &model.ExternalModel{ID: 3, ExternalName: "u", Enabled: true}

	m := &fakeModels{byName: map[string]*model.ExternalModel{"u": unmapped}}
	a := newTestAdmitter(m, &fakeKeys{key: &Key{ID: 1, KeyCipher: cipher}}, nil)
	_, denyModel := a.Admit(context.Background(), "plain-token", "u")
	if denyModel == nil || denyModel.Status != http.StatusNotFound || denyModel.Code != CodeModelNotFound {
		t.Fatalf("未映射模型应 404/model_not_found，got %+v", denyModel)
	}
	if denyModel.Message != modelNotLinkedMessage {
		t.Fatalf("未映射文案=%q，期望 %q", denyModel.Message, modelNotLinkedMessage)
	}

	okModel := &model.ExternalModel{ID: 1, ExternalName: "m", Enabled: true, MagpieModel: mp("upstream/m")}
	a2 := newTestAdmitter(
		&fakeModels{byName: map[string]*model.ExternalModel{"m": okModel}},
		&fakeKeys{err: ErrKeyNotProvisioned}, nil,
	)
	_, denyPerm := a2.Admit(context.Background(), "plain-token", "m")
	if denyPerm == nil || denyPerm.Status != http.StatusForbidden || denyPerm.Code != CodeMagpieKeyMissing {
		t.Fatalf("未开通 callerKey 应 403/magpie_key_missing，got %+v", denyPerm)
	}
	if denyModel.Status == denyPerm.Status || denyModel.Code == denyPerm.Code || denyModel.Message == denyPerm.Message {
		t.Fatalf("两种情况不得混淆: 模型=%+v 权限=%+v", denyModel, denyPerm)
	}
}

// 订阅池拦截仍是 403 model_not_allowed（「未对租户开放」是权限口径，不是「没这个模型」）。
func TestAdmit_SubscriptionPool_Stays403(t *testing.T) {
	cipher, _ := encryptForTest("caller-secret")
	subPool := &model.ExternalModel{ID: 4, ExternalName: "c", Enabled: true, MagpieModel: mp("claude/claude-sonnet-4")}
	a := newTestAdmitter(
		&fakeModels{byName: map[string]*model.ExternalModel{"c": subPool}},
		&fakeKeys{key: &Key{ID: 1, KeyCipher: cipher}}, nil,
	)
	_, deny := a.Admit(context.Background(), "plain-token", "c")
	if deny == nil || deny.Status != http.StatusForbidden || deny.Code != CodeModelNotAllowed {
		t.Fatalf("订阅池应 403/model_not_allowed，got %+v", deny)
	}
}

// HTTP 层复现矩阵用例：同一请求体，未启用模型（ErrNotFound）不再出 500，且响应体不含内部细节。
func TestHandler_UnknownModel_HTTP404(t *testing.T) {
	cfg := baseTestConfig()
	// 只需要「facade 已启用」：准入在打上游之前就拒绝，地址不会被访问。
	cfg.BaseURL = "http://upstream.invalid"
	h := NewHandler(Deps{
		Config:        cfg,
		Tokens:        &fakeTokens{tok: activeToken()},
		Models:        &fakeModels{err: model.ErrNotFound},
		Keys:          &fakeKeys{key: &Key{ID: 1, TokenID: 7, KeyName: "core-k1", State: KeyStateActive}},
		CredentialKey: testCredKey,
		Prefixes:      NewPrefixProvider(nil),
	})
	if h == nil {
		t.Fatalf("facade 未启用：NewHandler 返回 nil")
	}
	mux := http.NewServeMux()
	h.Mount(mux)
	gw := httptest.NewServer(mux)
	defer gw.Close()

	body := `{"model":"not-enabled-model","messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer plain-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := gw.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("状态码=%d，期望 404（旧实现 500）", resp.StatusCode)
	}
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Error.Code != CodeModelNotFound {
		t.Fatalf("error.code=%q，期望 %q", payload.Error.Code, CodeModelNotFound)
	}
	if payload.Error.Code == CodeInternal || strings.Contains(payload.Error.Message, "内部") {
		t.Fatalf("不得回 internal_error/内部细节: %+v", payload.Error)
	}
	for _, leak := range []string{"http://", "127.0.0.1", "magpie", "external_models"} {
		if strings.Contains(payload.Error.Message, leak) {
			t.Fatalf("文案泄漏内部信息 %q: %s", leak, payload.Error.Message)
		}
	}
}

// ---- P1-③：重复顶层 model 只改生效的那一处 ----

// 三个输入（重复 model / 乱序键 / 异常空白）都断言：只改生效处，其余字节逐字不变。
// 校验手法：把输出里生效处再换回原值，必须与输入**逐字节**相同。
func TestRewriteModelJSON_DuplicateModelOnlyEffectiveRewritten(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "重复 model（后者生效）",
			body: `{"model":"a","model":"b"}`,
			want: `{"model":"a","model":"mp-openai"}`,
		},
		{
			name: "重复 model + 乱序键",
			body: `{"stream":true,"model":"a","temperature":0.2,"model":"b","top_p":0.9}`,
			want: `{"stream":true,"model":"a","temperature":0.2,"model":"mp-openai","top_p":0.9}`,
		},
		{
			name: "重复 model + 异常空白与换行",
			body: "{\n\t\"model\" :\t\"a\" ,\n  \"messages\":[] ,\n\t\"model\"\n:\n\"b\"\n}",
			want: "{\n\t\"model\" :\t\"a\" ,\n  \"messages\":[] ,\n\t\"model\"\n:\n\"mp-openai\"\n}",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := RewriteModelJSON([]byte(c.body), "mp-openai")
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			if string(got) != c.want {
				t.Fatalf("改写后字节不符\n got: %q\nwant: %q", string(got), c.want)
			}
			// 生效值（最后一处）必须是代理模型名，且模型解析口径一致。
			if m, err := ModelFromJSON(got); err != nil || m != "mp-openai" {
				t.Fatalf("生效 model=%q err=%v，期望 mp-openai", m, err)
			}
			// 其余字节逐字不变：把生效处换回原值 == 输入。
			back, err := RewriteModelJSON(got, "b")
			if err != nil {
				t.Fatalf("rewrite back: %v", err)
			}
			if string(back) != c.body {
				t.Fatalf("除生效 model 外字节被改动\n got: %q\nwant: %q", string(back), c.body)
			}
		})
	}
}

// 输入切片不得被就地改写（原契约，别为了少一次分配破坏它）。
func TestRewriteModelJSON_DuplicateDoesNotMutateInput(t *testing.T) {
	body := []byte(`{"model":"a","model":"b"}`)
	orig := string(body)
	if _, err := RewriteModelJSON(body, "mp-openai"); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if string(body) != orig {
		t.Fatalf("入参被就地改写: %q", string(body))
	}
}
