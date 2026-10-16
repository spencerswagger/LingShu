package billing

import "testing"

// TestParseTimeConfig 守护「历史 snake_case 种子」与「新 PascalCase 契约」两种形状均可解析，
// 且时段系数能正确算出（避免时段系数恒 0 导致计费为 0、结算失败）。
func TestParseTimeConfig(t *testing.T) {
	pascal := []byte(`{"Timezone":"Asia/Shanghai","Default":1.0,"Periodic":[
		{"Name":"低谷","Start":"00:00","End":"08:00","Coeff":0.5}],"Overrides":[]}`)
	legacy := []byte(`{"timezone":"Asia/Shanghai","default_coeff":1.0,"periodic_segments":[
		{"name":"低谷","start":"00:00","end":"08:00","coeff":0.5}],"date_overrides":[]}`)

	for name, raw := range map[string][]byte{"pascal": pascal, "legacy": legacy} {
		cfg, err := ParseTimeConfig(raw)
		if err != nil {
			t.Fatalf("%s: parse failed: %v", name, err)
		}
		if cfg.Timezone != "Asia/Shanghai" || len(cfg.Periodic) != 1 || cfg.Periodic[0].Coeff != 0.5 {
			t.Fatalf("%s: unexpected parsed config: %+v", name, cfg)
		}
	}
}
