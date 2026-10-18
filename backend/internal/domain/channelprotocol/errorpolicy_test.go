package channelprotocol

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/team/llmgateway/internal/pkg/idgen"
)

// TestMatchError 表驱动覆盖错误策略匹配契约：
//   - 优先级：status+code > status+type > 仅 status（与策略出现顺序无关）；
//   - scope=models 时仅 ModelIDs 内的模型命中；
//   - MessageRegex 命中/不命中/非法表达式（视为不命中）；
//   - 空策略列表返回 (nil,false)。
func TestMatchError(t *testing.T) {
	channel := ErrorScopeChannel
	models := ErrorScopeModels

	tests := []struct {
		name     string
		policies []ErrorPolicy
		scope    string
		modelID  int64
		status   int
		errType  string
		errCode  string
		msg      string
		want     int // 期望命中的策略下标；-1 表示无命中
	}{
		{
			name: "status+code 优先于 status+type 优先于仅 status",
			policies: []ErrorPolicy{
				{Scope: channel, Note: "only-status", Match: ErrorMatch{Status: 422}},
				{Scope: channel, Note: "status+type", Match: ErrorMatch{Status: 422, ErrorType: "invalid_request_error"}},
				{Scope: channel, Note: "status+code", Match: ErrorMatch{Status: 422, ErrorCode: "context_length_exceeded"}},
			},
			status: 422, errType: "invalid_request_error", errCode: "context_length_exceeded", msg: "boom",
			want: 2,
		},
		{
			name: "调换顺序仍返回最具体者（status+code）",
			policies: []ErrorPolicy{
				{Scope: channel, Note: "status+code", Match: ErrorMatch{Status: 422, ErrorCode: "c"}},
				{Scope: channel, Note: "status+type", Match: ErrorMatch{Status: 422, ErrorType: "t"}},
				{Scope: channel, Note: "only-status", Match: ErrorMatch{Status: 422}},
			},
			status: 422, errType: "t", errCode: "c", msg: "boom",
			want: 0,
		},
		{
			name: "scope=models 且模型在 ModelIDs 内 → 命中",
			policies: []ErrorPolicy{
				{Scope: models, ModelIDs: idgen.IDs{7, 8}, Note: "models", Match: ErrorMatch{Status: 404}},
			},
			modelID: 7, status: 404,
			want: 0,
		},
		{
			name: "scope=models 且模型不在 ModelIDs 内 → 不命中",
			policies: []ErrorPolicy{
				{Scope: models, ModelIDs: idgen.IDs{7, 8}, Note: "models", Match: ErrorMatch{Status: 404}},
			},
			modelID: 9, status: 404,
			want: -1,
		},
		{
			name: "MessageRegex 命中",
			policies: []ErrorPolicy{
				{Scope: channel, Note: "regex", Match: ErrorMatch{Status: 400, MessageRegex: `^boom:\s`}},
			},
			status: 400, msg: "boom: bad request",
			want: 0,
		},
		{
			name: "MessageRegex 不命中",
			policies: []ErrorPolicy{
				{Scope: channel, Note: "regex", Match: ErrorMatch{Status: 400, MessageRegex: `^boom:\s`}},
			},
			status: 400, msg: "unrelated message",
			want: -1,
		},
		{
			name: "MessageRegex 非法表达式视为不命中（不 panic）",
			policies: []ErrorPolicy{
				{Scope: channel, Note: "bad-regex", Match: ErrorMatch{Status: 400, MessageRegex: `(`}},
			},
			status: 400, msg: "anything",
			want: -1,
		},
		{
			name: "status 不匹配 → 不命中",
			policies: []ErrorPolicy{
				{Scope: channel, Note: "x", Match: ErrorMatch{Status: 500}},
			},
			status: 422,
			want:   -1,
		},
		{
			name: "ErrorType/ErrorCode 不匹配 → 不命中",
			policies: []ErrorPolicy{
				{Scope: channel, Note: "x", Match: ErrorMatch{Status: 422, ErrorCode: "wanted"}},
			},
			status: 422, errCode: "other",
			want: -1,
		},
		{
			name:     "空策略列表 → (nil,false)",
			policies: nil,
			status:   422,
			want:     -1,
		},
		{
			name: "可选 scope 收紧：scope=channel 时 models 策略不命中",
			policies: []ErrorPolicy{
				{Scope: models, ModelIDs: idgen.IDs{1}, Note: "m", Match: ErrorMatch{Status: 429}},
			},
			scope: channel, modelID: 1, status: 429,
			want: -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := MatchError(tt.policies, tt.scope, tt.modelID, tt.status, tt.errType, tt.errCode, tt.msg)
			if tt.want < 0 {
				if ok || got != nil {
					t.Fatalf("期望无命中，got ok=%v policy=%+v", ok, got)
				}
				return
			}
			if !ok || got == nil {
				t.Fatalf("期望命中策略 #%d，got ok=%v", tt.want, ok)
			}
			if got != &tt.policies[tt.want] {
				t.Fatalf("命中策略不符：want #%d(%s) got (%s)", tt.want, tt.policies[tt.want].Note, got.Note)
			}
		})
	}
}

// TestErrorPolicyModelIDsJSON 验证雪花 ID 精度契约：
//   - 编码：modelIds 以 JSON 字符串数组输出（避免超出 JS 安全整数而丢精度）；
//   - 解码：兼容字符串数组与历史整数数组两种形式。
func TestErrorPolicyModelIDsJSON(t *testing.T) {
	snowflake := int64(1234567890123456789)
	p := ErrorPolicy{
		Scope:    ErrorScopeModels,
		ModelIDs: idgen.IDs{snowflake, 2},
		Match:    ErrorMatch{Status: 404},
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := `"modelIds":["1234567890123456789","2"]`; !strings.Contains(string(b), want) {
		t.Fatalf("modelIds 应以字符串数组编码：got %s，want 含 %s", b, want)
	}

	var back ErrorPolicy
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back.ModelIDs) != 2 || back.ModelIDs[0] != snowflake || back.ModelIDs[1] != 2 {
		t.Fatalf("字符串数组往返不符：%+v", back.ModelIDs)
	}

	var legacy ErrorPolicy
	if err := json.Unmarshal([]byte(`{"scope":"models","modelIds":[123,456],"match":{}}`), &legacy); err != nil {
		t.Fatalf("历史整数数组应兼容解析：%v", err)
	}
	if len(legacy.ModelIDs) != 2 || legacy.ModelIDs[0] != 123 || legacy.ModelIDs[1] != 456 {
		t.Fatalf("整数数组兼容解析不符：%+v", legacy.ModelIDs)
	}
}
