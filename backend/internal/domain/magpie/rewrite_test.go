package magpie

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
)

// TestRewriteModelJSON_ByteIdenticalAfterRevert 是「只换 model」的硬断言：
// 把改写后的 model 换回原值，必须与入站请求体**逐字节**相同。
//
// 为什么不直接断言「含新 model」：那挡不住「重新序列化」（键序/空白/数字表示全变，
// 上游做指纹校验时会直接暴露），换回来比对才挡得住。
func TestRewriteModelJSON_ByteIdenticalAfterRevert(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	cases := []struct{ name, body, model, upstream string }{
		{"普通", `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`, "gpt-4o", "vendor/gpt-4o"},
		{"紧凑无空格", `{"model":"a","stream":true}`, "a", "b"},
		{"缩进与换行", "{\n  \"model\" : \"m1\" ,\n  \"x\": 1\n}", "m1", "m2"},
		{"键序非字典序", `{"z":1,"model":"m","a":2}`, "m", "mm"},
		{"数字与转义", `{"model":"m","t":1.0e3,"s":"a\"b\\c"}`, "m", "n"},
		{"中文字段", `{"model":"m","提示":"你好"}`, "m", "n"},
		{"model 在末尾", `{"messages":[],"model":"tail"}`, "tail", "head"},
		{"嵌套同名键", `{"model":"outer","x":{"model":"inner"}}`, "outer", "outer2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := RewriteModelJSON([]byte(c.body), c.upstream)
			if err != nil {
				t.Fatalf("改写失败: %v", err)
			}
			back, err := RewriteModelJSON(got, c.model)
			if err != nil {
				t.Fatalf("换回失败: %v", err)
			}
			// 嵌套同名键：只有顶层该被改，内层的 "inner" 不许动。
			if bytes.Contains(got, []byte(`"model":"inner"`)) == false &&
				bytes.Contains([]byte(c.body), []byte(`"model":"inner"`)) {
				t.Fatalf("嵌套 model 不该被改写：%s", got)
			}
			if !bytes.Equal(back, []byte(c.body)) {
				t.Fatalf("换回后必须逐字节相同：\n got %q\nwant %q", back, c.body)
			}
			// 长度差必须恰好等于「新值 − 旧值」的字节差：多改一个字节都会露。
			wantDelta := len(c.upstream) - len(c.model)
			if len(got)-len(c.body) != wantDelta {
				t.Fatalf("改写只该动 model 值：长度差 %d，应为 %d", len(got)-len(c.body), wantDelta)
			}
		})
	}
	// 随机体再压一遍（字段名/长度/嵌套随机）。
	for i := 0; i < 40; i++ {
		payload := randomJSON(rng, 3)
		obj, ok := payload.(map[string]any)
		if !ok {
			continue
		}
		obj["model"] = randomString(rng, 5+i)
		raw, err := json.Marshal(obj)
		if err != nil {
			t.Fatalf("序列化失败: %v", err)
		}
		model := obj["model"].(string)
		got, err := RewriteModelJSON(raw, "up-"+model)
		if err != nil {
			t.Fatalf("改写失败: %v", err)
		}
		back, err := RewriteModelJSON(got, model)
		if err != nil {
			t.Fatalf("换回失败: %v", err)
		}
		if !bytes.Equal(back, raw) {
			t.Fatalf("随机体换回后必须逐字节相同（第 %d 轮）", i)
		}
	}
}

// TestModelFromJSON_Errors 覆盖非法/缺字段/非字符串三类入参。
func TestModelFromJSON_Errors(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string // 期望的错误子串（空=成功）
	}{
		{"正常", `{"model":"m"}`, ""},
		{"非对象", `[1,2]`, "JSON 对象"},
		{"缺 model", `{"a":1}`, "缺少 model"},
		{"model 非字符串", `{"model":123}`, "缺少 model"},
		{"坏 JSON", `{"model":`, "JSON 对象"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ModelFromJSON([]byte(c.body))
			if c.want == "" {
				if err != nil || got != "m" {
					t.Fatalf("应解析成功，got %q err=%v", got, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误应包含 %q，got %v", c.want, err)
			}
		})
	}
}

// TestClassify_Endpoints 覆盖六条入站路径 + 拒绝清单（放开任意 path 等于开放代理）。
func TestClassify_Endpoints(t *testing.T) {
	ok := []struct {
		path     string
		protocol Protocol
		billable bool
		modelIn  bool
	}{
		{"/v1/chat/completions", ProtocolOpenAIChat, true, false},
		{"/v1/responses", ProtocolOpenAIResponses, true, false},
		{"/v1/messages", ProtocolAnthropic, true, false},
		{"/v1/messages/count_tokens", ProtocolAnthropic, false, false},
		{"/v1beta/models/gemini-2.0:generateContent", ProtocolGemini, true, true},
		{"/v1beta/models/gemini-2.0:streamGenerateContent", ProtocolGemini, true, true},
		{"/v1beta/models/gemini-2.0:countTokens", ProtocolGemini, false, true},
	}
	for _, c := range ok {
		t.Run(c.path, func(t *testing.T) {
			r, err := Classify("POST", c.path)
			if err != nil {
				t.Fatalf("应识别: %v", err)
			}
			if r.Protocol != c.protocol || r.Billable != c.billable || r.ModelInPath != c.modelIn {
				t.Fatalf("识别结果不符: %+v", r)
			}
		})
	}
	bad := []string{
		"/v1/completions", "/v1/embeddings", "/v1/unknown",
		"/v1beta/models/x:deleteModel", "/v1beta/models/:generateContent",
		"/v1beta/models/a/b:generateContent", "/admin/channels",
	}
	for _, p := range bad {
		t.Run("拒绝"+p, func(t *testing.T) {
			if _, err := Classify("POST", p); err == nil {
				t.Fatalf("%s 不该被当作可转发端点", p)
			}
		})
	}
	if _, err := Classify("GET", "/v1/chat/completions"); err == nil {
		t.Fatal("非 POST 应拒绝")
	}
}

// TestPathModelRewrite 覆盖 Gemini 的 path model 改写（其余段逐字不动）。
func TestPathModelRewrite(t *testing.T) {
	p := "/v1beta/models/gemini-2.0-flash:streamGenerateContent"
	if got := PathModel(p); got != "gemini-2.0-flash" {
		t.Fatalf("取 model 段失败: %q", got)
	}
	got, err := RewritePathModel(p, "vendor/model-x")
	if err != nil {
		t.Fatalf("改写失败: %v", err)
	}
	if got != "/v1beta/models/vendor/model-x:streamGenerateContent" {
		t.Fatalf("改写结果不符: %q", got)
	}
}

// TestStripQueryParam_NoReencode 断言剔除 key 后其余参数逐字保留（不重新编码）。
func TestStripQueryParam_NoReencode(t *testing.T) {
	cases := []struct{ in, want string }{
		{"key=secret&alt=sse&x=%2F%20a", "alt=sse&x=%2F%20a"},
		{"a=1&key=s&b=2", "a=1&b=2"},
		{"keyonly=1&key=2", "keyonly=1"},
		{"", ""},
	}
	for _, c := range cases {
		if got := StripQueryParam(c.in, "key"); got != c.want {
			t.Fatalf("StripQueryParam(%q) = %q want %q", c.in, got, c.want)
		}
	}
}
