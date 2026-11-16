package magpie

import (
	"bytes"
	"strings"
	"testing"
)

// TestRewriteModelJSON_PreservesAllOtherBytes 是「只换 model、其余字节原样」的核心契约测试：
// 用带空白、乱序键、Unicode、指数数字与转义序列的请求体，断言**逐字节**等价（除 model 值）。
func TestRewriteModelJSON_PreservesAllOtherBytes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "标准体",
			body: `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":false}`,
			want: `{"model":"upstream/gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":false}`,
		},
		{
			name: "乱序键 + 古怪空白（保留原格式）",
			body: "{\n  \"temperature\" : 0.70 ,\n\t\"model\"\t:\t\"m-1\"  ,\n  \"top_p\":1e-3\n}",
			want: "{\n  \"temperature\" : 0.70 ,\n\t\"model\"\t:\t\"upstream/m-1\"  ,\n  \"top_p\":1e-3\n}",
		},
		{
			name: "Unicode 与转义序列不动",
			body: `{"model":"m","messages":[{"content":"中文 \u4e2d \"quote\" \\ backslash"}]}`,
			want: `{"model":"up/m","messages":[{"content":"中文 \u4e2d \"quote\" \\ backslash"}]}`,
		},
		{
			name: "嵌套对象里同名键不算（只改顶层）",
			body: `{"model":"m","metadata":{"model":"inner"},"stream":true}`,
			want: `{"model":"up/m","metadata":{"model":"inner"},"stream":true}`,
		},
		{
			name: "顶层重复 model → 只改生效的那一处（后者生效）",
			body: `{"model":"a","model":"b"}`,
			want: `{"model":"a","model":"up/x"}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := RewriteModelJSON([]byte(c.body), magpieModelOf(c.want))
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			if string(got) != c.want {
				t.Fatalf("改写后字节不符\n got: %q\nwant: %q", string(got), c.want)
			}
		})
	}
}

// magpieModelOf 从期望体里取出目标 model 值（避免每个用例重复写一遍）。
func magpieModelOf(want string) string {
	m, err := ModelFromJSON([]byte(want))
	if err != nil {
		panic(err)
	}
	return m
}

// TestRewriteModelJSON_RejectsBadBodies 断言非法请求体被拒（不静默转发原体）。
func TestRewriteModelJSON_RejectsBadBodies(t *testing.T) {
	cases := []struct {
		name string
		body string
		want error
	}{
		{name: "不是对象", body: `["model"]`, want: ErrBodyNotObject},
		{name: "空体", body: ``, want: ErrBodyNotObject},
		{name: "缺 model", body: `{"messages":[]}`, want: ErrMissingModel},
		{name: "model 非字符串", body: `{"model":123}`, want: ErrMissingModel},
		{name: "截断 JSON", body: `{"model":"a"`, want: ErrBodyNotObject},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := RewriteModelJSON([]byte(c.body), "up")
			if err == nil {
				t.Fatalf("期望报错，实际通过")
			}
			if !strings.Contains(err.Error(), c.want.Error()) {
				t.Fatalf("错误类型不符: %v", err)
			}
		})
	}
}

// TestRewriteModelJSON_DoesNotMutateInput 断言不改入参（并发/重试安全）。
func TestRewriteModelJSON_DoesNotMutateInput(t *testing.T) {
	body := []byte(`{"model":"a","x":1}`)
	orig := append([]byte(nil), body...)
	if _, err := RewriteModelJSON(body, "much-longer-upstream-model-name"); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if !bytes.Equal(body, orig) {
		t.Fatalf("入参被改写: %q", body)
	}
}

// TestGeminiPathModel 覆盖 Gemini「model 在 path」的识别与改写（含 :action 与查询串）。
func TestGeminiPathModel(t *testing.T) {
	cases := []struct {
		path      string
		wantModel string
		wantPath  string
	}{
		{
			path:      "/v1beta/models/gemini-2.0-flash:generateContent",
			wantModel: "gemini-2.0-flash",
			wantPath:  "/v1beta/models/upstream/gemini:generateContent",
		},
		{
			path:      "/v1beta/models/gemini-2.0-flash:streamGenerateContent",
			wantModel: "gemini-2.0-flash",
			wantPath:  "/v1beta/models/upstream/gemini:streamGenerateContent",
		},
		{
			path:      "/v1beta/models/gemini-2.0-flash:countTokens",
			wantModel: "gemini-2.0-flash",
			wantPath:  "/v1beta/models/upstream/gemini:countTokens",
		},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			if got := pathModel(c.path); got != c.wantModel {
				t.Fatalf("pathModel = %q, 期望 %q", got, c.wantModel)
			}
			got, err := rewritePathModel(c.path, "upstream/gemini")
			if err != nil {
				t.Fatalf("rewritePathModel: %v", err)
			}
			if got != c.wantPath {
				t.Fatalf("改写后 path = %q, 期望 %q", got, c.wantPath)
			}
		})
	}
}

// TestClassify 锁定协议识别口径：只认契约路径，其余不猜。
func TestClassify(t *testing.T) {
	cases := []struct {
		method, path string
		wantProto    Protocol
		wantBillable bool
		wantStream   bool
		wantPathMod  bool
		wantErr      bool
	}{
		{method: "POST", path: "/v1/chat/completions", wantProto: ProtocolOpenAIChat, wantBillable: true},
		{method: "POST", path: "/v1/responses", wantProto: ProtocolOpenAIResponses, wantBillable: true},
		{method: "POST", path: "/v1/messages", wantProto: ProtocolAnthropic, wantBillable: true},
		{method: "POST", path: "/v1/messages/count_tokens", wantProto: ProtocolAnthropic, wantBillable: false},
		{method: "POST", path: "/v1beta/models/m:generateContent", wantProto: ProtocolGemini, wantBillable: true, wantPathMod: true},
		{method: "POST", path: "/v1beta/models/m:streamGenerateContent", wantProto: ProtocolGemini, wantBillable: true, wantStream: true, wantPathMod: true},
		{method: "POST", path: "/v1beta/models/m:countTokens", wantProto: ProtocolGemini, wantBillable: false, wantPathMod: true},
		{method: "POST", path: "/v1/embeddings", wantErr: true},
		{method: "GET", path: "/v1/chat/completions", wantErr: true},
		{method: "POST", path: "/v1beta/models/m:predict", wantErr: true},
		{method: "POST", path: "/v1beta/models/m", wantErr: true},
		{method: "POST", path: "/v1beta/models/:generateContent", wantErr: true},
		{method: "POST", path: "/v1/chat/completions/x", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			r, err := Classify(c.method, c.path)
			if c.wantErr {
				if err == nil {
					t.Fatalf("期望不支持，实际 %+v", r)
				}
				return
			}
			if err != nil {
				t.Fatalf("classify: %v", err)
			}
			if r.Protocol != c.wantProto || r.Billable != c.wantBillable ||
				r.StreamHint != c.wantStream || r.ModelInPath != c.wantPathMod {
				t.Fatalf("route 不符: %+v", r)
			}
		})
	}
}
