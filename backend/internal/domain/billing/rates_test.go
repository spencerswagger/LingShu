package billing

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
)

// TestWireRates_ZeroMigration 证明「零迁移」：存储结构 Rates 直接 marshal 仍为 snake，
// 对外视图 WireRates marshal 为 PascalCase，且两者可无损互转。
func TestWireRates_ZeroMigration(t *testing.T) {
	r := Rates{"input": 1, "output": 2, "cache_read": 0.1}

	// 存储不变：Rates 直接序列化仍为 snake 键。
	native, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal Rates: %v", err)
	}
	wantNative := `{"cache_read":0.1,"input":1,"output":2}`
	if string(native) != wantNative {
		t.Fatalf("Rates marshal = %s, want %s (存储键必须保持 snake)", native, wantNative)
	}

	// 对外契约：WireRates 序列化为 PascalCase 键。
	wire, err := json.Marshal(WireRates(r))
	if err != nil {
		t.Fatalf("marshal WireRates: %v", err)
	}
	wantWire := `{"CacheRead":0.1,"Input":1,"Output":2}`
	if string(wire) != wantWire {
		t.Fatalf("WireRates marshal = %s, want %s", wire, wantWire)
	}

	// round-trip：PascalCase JSON → 内部 snake → 与原始 Rates 等价。
	var back WireRates
	if err := json.Unmarshal(wire, &back); err != nil {
		t.Fatalf("unmarshal WireRates: %v", err)
	}
	if !reflect.DeepEqual(Rates(back), r) {
		t.Fatalf("round-trip Rates = %v, want %v", Rates(back), r)
	}

	// 向后兼容：旧 snake 请求体解析后仍归一到内部 snake 键。
	var legacy WireRates
	if err := json.Unmarshal(native, &legacy); err != nil {
		t.Fatalf("unmarshal legacy snake: %v", err)
	}
	if !reflect.DeepEqual(Rates(legacy), r) {
		t.Fatalf("legacy snake unmarshal = %v, want %v", Rates(legacy), r)
	}

	// nil 语义：仍序列化为 null（与 Rates nil 一致）。
	nilJSON, err := json.Marshal(WireRates(nil))
	if err != nil {
		t.Fatalf("marshal nil WireRates: %v", err)
	}
	if string(nilJSON) != "null" {
		t.Fatalf("nil WireRates marshal = %s, want null", nilJSON)
	}
}

// TestWireUsage_ZeroMigration 证明 Usage（billing_records.tokens 存储结构）保持 snake，
// 对外视图 WireUsage 为 PascalCase。
func TestWireUsage_ZeroMigration(t *testing.T) {
	u := Usage{Input: 1, Output: 2, CacheRead: 3}

	native, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal Usage: %v", err)
	}
	wantNative := `{"input":1,"output":2,"cache_read":3}`
	if string(native) != wantNative {
		t.Fatalf("Usage marshal = %s, want %s (存储键必须保持 snake)", native, wantNative)
	}

	wire, err := json.Marshal(WireUsage(u))
	if err != nil {
		t.Fatalf("marshal WireUsage: %v", err)
	}
	wantWire := `{"Input":1,"Output":2,"CacheRead":3}`
	if string(wire) != wantWire {
		t.Fatalf("WireUsage marshal = %s, want %s", wire, wantWire)
	}

	// 视图与存储结构可直接互转（字段同名同类型，仅 tag 不同）。
	if !reflect.DeepEqual(Usage(WireUsage(u)), u) {
		t.Fatalf("WireUsage<->Usage 转换不等价: %+v", Usage(WireUsage(u)))
	}
}

// TestNormalizeRatesJSON_BothKeyStylesSameCost：PascalCase 与 snake 两种键名输入
// 必须归一成同一份 snake JSON——否则 cost-rates 落库 Pascal 后 poller 读不出 → 成本静默算 0。
func TestNormalizeRatesJSON_BothKeyStylesSameCost(t *testing.T) {
	rnd := func() float64 { return float64(rand.Intn(10_000)) / 100 }
	for i := 0; i < 20; i++ {
		input, output, cacheRead := rnd(), rnd(), rnd()
		pascal := []byte(fmt.Sprintf(`{"Input":%v,"Output":%v,"CacheRead":%v}`, input, output, cacheRead))
		snake := []byte(fmt.Sprintf(`{"input":%v,"output":%v,"cache_read":%v}`, input, output, cacheRead))

		var rp, rs Rates
		if err := json.Unmarshal(NormalizeRatesJSON(pascal), &rp); err != nil {
			t.Fatalf("round %d pascal: %v", i, err)
		}
		if err := json.Unmarshal(NormalizeRatesJSON(snake), &rs); err != nil {
			t.Fatalf("round %d snake: %v", i, err)
		}
		if rp["input"] != rs["input"] || rp["output"] != rs["output"] || rp["cache_read"] != rs["cache_read"] {
			t.Fatalf("round %d 两种键名归一后不一致：pascal=%v snake=%v", i, rp, rs)
		}
		// 与 poller 同口径的成本计算：两种键名输入必须算出同一成本。
		u := Usage{Input: rand.Int63n(1_000_000), Output: rand.Int63n(1_000_000), CacheRead: rand.Int63n(1_000_000)}
		cp := float64(u.Input)*rp["input"] + float64(u.Output)*rp["output"] + float64(u.CacheRead)*rp["cache_read"]
		cs := float64(u.Input)*rs["input"] + float64(u.Output)*rs["output"] + float64(u.CacheRead)*rs["cache_read"]
		if cp != cs {
			t.Fatalf("round %d 成本不一致：pascal=%v snake=%v", i, cp, cs)
		}
	}
}
