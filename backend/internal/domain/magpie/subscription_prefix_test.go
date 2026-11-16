// subscription_prefix_test.go 固化「订阅池渠道默认不对租户开放」这条硬约束的匹配口径。
//
// 回归背景：早先只做字面前缀匹配，代理侧不带 provider 段的写法（claude-sonnet-4、codex-latest）
// 会被放行——等于把订阅池渠道开给租户。
package magpie

import (
	"context"
	"net/http"
	"testing"

	"github.com/team/llmgateway/internal/domain/model"
)

func TestIsSubscriptionModel_FamilyMatching(t *testing.T) {
	prefixes := DefaultSubscriptionPrefixes
	cases := []struct {
		model string
		want  bool
		why   string
	}{
		{"claude/claude-sonnet-4", true, "provider 段写法"},
		{"claude-sonnet-4", true, "无 provider 段（回归：曾被放行）"},
		{"claude-3-5-haiku-20241022", true, "无 provider 段 + 版本后缀"},
		{"codex-latest", true, "无 provider 段（回归：曾被放行）"},
		{"codex/gpt-5-codex", true, "provider 段写法"},
		{"grok-2", true, "短族名 + 版本"},
		{"gemini-code-assist/x", true, "多段族名"},
		{"copilot/gpt-4o", true, "provider 段写法"},
		{"openai/gpt-4o", false, "普通渠道模型不该被拦"},
		{"anthropic/claude-sonnet-4", false, "API 渠道（第一段 anthropic）不是订阅池，不得误拦"},
		{"gemini/gemini-2.0-flash", false, "Gemini 官方渠道不是订阅池"},
		{"claudette-x", false, "同前缀的别的模型不能误拦"},
		{"", false, "空串"},
	}
	for _, c := range cases {
		if got := isSubscriptionModel(c.model, prefixes); got != c.want {
			t.Fatalf("isSubscriptionModel(%q)=%v want %v（%s）", c.model, got, c.want, c.why)
		}
	}
}

// 名单可配置：自定义前缀同样按族匹配，且空项/纯斜杠不误伤。
func TestIsSubscriptionModel_CustomPrefixes(t *testing.T) {
	prefixes := []string{" My-Sub/ ", "", "/", "vendor-x/"}
	cases := map[string]bool{
		"my-sub/model-a":   true,
		"my-sub-model-a":   true,
		"vendor-x/model-b": true,
		"openai/gpt-4o":    false,
	}
	for model, want := range cases {
		if got := isSubscriptionModel(model, prefixes); got != want {
			t.Fatalf("isSubscriptionModel(%q)=%v want %v", model, got, want)
		}
	}
}

// TestAdmit_DeniesFamilyFormsAtAdmission 断言拦截发生在准入层（403），
// 而不只是单测里的判定函数：无 provider 段的写法同样被拒。
func TestAdmit_DeniesFamilyFormsAtAdmission(t *testing.T) {
	for _, magpieModel := range []string{"claude-sonnet-4", "codex-latest", "claude/claude-sonnet-4"} {
		models := &fakeModels{byName: map[string]*model.ExternalModel{
			"tenant-x": {ID: 1, ExternalName: "tenant-x", Enabled: true, MagpieModel: mp(magpieModel)},
		}}
		cipher, err := encryptForTest("caller-secret")
		if err != nil {
			t.Fatalf("encrypt: %v", err)
		}
		a := newTestAdmitter(models, &fakeKeys{key: &Key{ID: 1, TokenID: 7, KeyName: "k1", KeyCipher: cipher, State: KeyStateActive}}, NewPrefixProvider(nil))
		dec, deny := a.Admit(context.Background(), "tenant-token", "tenant-x")
		if deny == nil {
			t.Fatalf("%q 应被拦截，实际放行: %+v", magpieModel, dec)
		}
		if deny.Status != http.StatusForbidden || deny.Code != CodeModelNotAllowed {
			t.Fatalf("%q 拒绝口径=%+v", magpieModel, deny)
		}
	}
	// 普通渠道模型仍放行（防止拦截面扩大）。
	models := &fakeModels{byName: map[string]*model.ExternalModel{
		"tenant-gpt": {ID: 2, ExternalName: "tenant-gpt", Enabled: true, MagpieModel: mp("openai/gpt-4o")},
	}}
	cipher, _ := encryptForTest("caller-secret")
	a := newTestAdmitter(models, &fakeKeys{key: &Key{ID: 1, TokenID: 7, KeyName: "k1", KeyCipher: cipher, State: KeyStateActive}}, NewPrefixProvider(nil))
	if _, deny := a.Admit(context.Background(), "tenant-token", "tenant-gpt"); deny != nil {
		t.Fatalf("普通渠道模型不该被拦: %+v", deny)
	}
}
