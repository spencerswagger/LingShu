package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/team/llmgateway/internal/domain/router"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// 本文件按**契约裁定**钉住三个字段的语义（裁定口径见设计文档 §1.2）：
//
//  1. `error.upstream_status`：只在「上游确实返回了非 2xx」时出现，且必须位于 `error` 对象内；
//     **无上游码时不写该字段**（不是 `null`、也不是 0）——它是「上游转述 vs 网关判定」的唯一判别符。
//  2. `error.message`：上游原文，只做**精确遮蔽本次凭据** + 512B UTF-8 截断；
//     不做样式清洗（否则 request-id / 模型名里的高熵串会被一起抹掉，读到的就不是原文）。
//  3. `error.code` / `error.type`：code 由网关按对外状态归类；type 保留上游原文（待拍板项，见文档）。

// rawErrorObject 取响应体里的 error 对象**原始键集合**（用于断言字段「不存在」而不是「值为空」）。
func rawErrorObject(t *testing.T, rec *httptest.ResponseRecorder) map[string]json.RawMessage {
	t.Helper()
	var body struct {
		Error map[string]json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("对外响应必须是 JSON: %v (%s)", err, rec.Body.String())
	}
	if body.Error == nil {
		t.Fatalf("对外响应必须带 error 对象: %s", rec.Body.String())
	}
	return body.Error
}

// TestContract_UpstreamStatusOmittedWhenAbsent 无上游码的三条路径：字段必须**不存在**（不是 null/0）。
func TestContract_UpstreamStatusOmittedWhenAbsent(t *testing.T) {
	t.Run("连接失败", func(t *testing.T) {
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		deadURL := dead.URL
		dead.Close()

		h := build(t, deadURL)
		rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", streamFlagged(false), "req-nocode-conn")
		assertUpstreamStatusAbsent(t, rec)
	})

	t.Run("已送达后超时", func(t *testing.T) {
		live := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		defer live.Close()
		h := build(t, live.URL)
		h.gw.client = &http.Client{Transport: &nonStreamDeliveredFailTransport{}}
		rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", streamFlagged(false), "req-nocode-timeout")
		assertUpstreamStatusAbsent(t, rec)
	})

	t.Run("网关自身拒绝(模型未配置)", func(t *testing.T) {
		h := build(t, "http://unused")
		h.rrouter.result = nil
		h.rrouter.err = &router.RouteError{Kind: router.KindModelNotFound,
			HTTPStatus: http.StatusNotFound, Code: resp.CodeNotFound, Message: "model not found"}
		rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t",
			`{"model":"ghost","stream":false,"messages":[{"role":"user","content":"x"}]}`, "req-nocode-reject")
		assertUpstreamStatusAbsent(t, rec)
	})
}

// assertUpstreamStatusAbsent 断言 error 对象里**没有** upstream_status 键，且顶层也没有。
func assertUpstreamStatusAbsent(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	errObj := rawErrorObject(t, rec)
	if _, exists := errObj["upstream_status"]; exists {
		t.Fatalf("无上游码时不得写 error.upstream_status（应缺省，而非 null/0）: %s", rec.Body.String())
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &top); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, exists := top["upstream_status"]; exists {
		t.Fatalf("upstream_status 不得出现在响应体顶层: %s", rec.Body.String())
	}
}

// TestContract_MessageExactMaskOnly 对外 message 只遮蔽本次凭据：request-id、模型名等**高熵内容必须保留**
// （样式清洗会把它们一并抹成 ***，调用方读到的就不是上游原文）。
func TestContract_MessageExactMaskOnly(t *testing.T) {
	const cred = "sk-live-CHANNELKEY-9f2c4b7e1d"
	const upstreamReqID = "req_9f2c4b7e1d8a4c3fb0e5d7a1c2b3d4e5" // 32 位高熵串（样式清洗会误伤）
	const modelName = "gpt-4o-2024-11-20"
	body := `{"error":{"type":"invalid_request_error","message":"bad key ` + cred +
		` (rid=` + upstreamReqID + ` model=` + modelName + `)"}}`

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(body))
	}))
	defer up.Close()

	h := build(t, up.URL)
	h.chans.keys[1].CredentialPlain = cred
	rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", streamFlagged(false), "req-mask-exact")

	msg, _ := errField(mustJSON(t, rec), "message").(string)
	if strings.Contains(msg, cred) {
		t.Fatalf("本次渠道凭据必须被遮蔽: %q", msg)
	}
	if !strings.Contains(msg, "***") {
		t.Fatalf("凭据位置应替换成 ***: %q", msg)
	}
	for _, keep := range []string{upstreamReqID, modelName} {
		if !strings.Contains(msg, keep) {
			t.Fatalf("非凭据内容 %q 必须原样保留（样式清洗属日志专用）: %q", keep, msg)
		}
	}
	// 401 也照旧透原文（不按状态码区别对待）。
	if !strings.HasPrefix(msg, "bad key") {
		t.Fatalf("401 的 message 也必须是上游原文, got %q", msg)
	}
}

// TestContract_MessageTruncatedTo512 对外 message 按 UTF-8 边界截断到 512 字节。
//
// 注：上游响应体读取上限是 4096 字节（`upstreamErrBytes`，同时是 resp_body 的存储上限，本单不改）。
// 报文超过该上限时 JSON 会被读断，message 回落到「原文前缀」而不是解析出的 error.message——
// 这仍是原文、仍被 512 字节封顶，只是不再是纯净的 message 字段。
func TestContract_MessageTruncatedTo512(t *testing.T) {
	long := strings.Repeat("区域未开通，请到控制台切换 Global。", 200)
	body, err := json.Marshal(map[string]any{"error": map[string]any{
		"type": "invalid_request_error", "message": long,
	}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(body)
	}))
	defer up.Close()

	h := build(t, up.URL)
	rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", streamFlagged(false), "req-trunc")
	msg, _ := errField(mustJSON(t, rec), "message").(string)
	if len(msg) > errSummaryMaxBytes {
		t.Fatalf("message 应截断到 %d 字节内, got %d", errSummaryMaxBytes, len(msg))
	}
	if !strings.Contains(msg, "区域未开通") {
		t.Fatalf("截断必须保留可读内容, got %q", msg)
	}
	if !utf8.ValidString(msg) {
		t.Fatalf("截断必须按 UTF-8 边界，不得切出半个字符: %q", msg)
	}
}

// mustJSON 解析响应体为 map（测试辅助）。
func mustJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	return out
}
