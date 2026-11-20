package billing

import (
	"strings"
	"testing"
)

// TestTokenSumExpr_FourKeys 求和口径守卫（四项定案①级联）：TokenSumExpr 必须四键齐全
// （cache_write 独立分桶后漏键 = 对账与账本 tokens_in 永差一个缓存写量、每窗误报 alarm）。
// 历史三段行没有该键，COALESCE 缺键按 0，合计逐值不变——加键是纯增量口径修复。
func TestTokenSumExpr_FourKeys(t *testing.T) {
	for _, key := range []string{"input", "cache_read", "cache_write", "output"} {
		if got := strings.Count(TokenSumExpr(""), "->>'"+key+"'"); got != 1 {
			t.Fatalf("TokenSumExpr 缺 %s 键（出现 %d 次）：%s", key, got, TokenSumExpr(""))
		}
	}
	// 前缀必须逐键带表别名（多表 JOIN 时不能解析到别家表的 tokens）。
	prefixed := TokenSumExpr("br.")
	if !strings.Contains(prefixed, "br.tokens->>'cache_write'") {
		t.Fatalf("TokenSumExpr 前缀未作用到 cache_write 键：%s", prefixed)
	}
}

// TestInputTokensExpr_ContextShape 上下文长度口径：v2 计价行 cache_write 独立分桶后，
// 「送进模型的提示词侧总量」= input+cache_read+cache_write（三段历史行缓存写折在
// input、缺键按 0，合计不变）。
func TestInputTokensExpr_ContextShape(t *testing.T) {
	for _, key := range []string{"input", "cache_read", "cache_write"} {
		if got := strings.Count(InputTokensExpr(""), "->>'"+key+"'"); got != 1 {
			t.Fatalf("InputTokensExpr 缺 %s 键（出现 %d 次）：%s", key, got, InputTokensExpr(""))
		}
	}
	if strings.Contains(InputTokensExpr(""), "output") {
		t.Fatalf("上下文长度不得混入 output 段：%s", InputTokensExpr(""))
	}
}
