package billing

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestWireRates_ZeroMigration 证明「零迁移」：存储结构 Rates 直接 marshal 仍为 snake，
// 对外视图 WireRates marshal 为 PascalCase，且两者可无损互转。
func TestWireRates_ZeroMigration(t *testing.T) {
	r := Rates{"input": 1, "output": 2, "cache_read": 0.1, "cache_write": 0.3, "reasoning": 4}

	// 存储不变：Rates 直接序列化仍为 snake 键。
	native, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal Rates: %v", err)
	}
	wantNative := `{"cache_read":0.1,"cache_write":0.3,"input":1,"output":2,"reasoning":4}`
	if string(native) != wantNative {
		t.Fatalf("Rates marshal = %s, want %s (存储键必须保持 snake)", native, wantNative)
	}

	// 对外契约：WireRates 序列化为 PascalCase 键。
	wire, err := json.Marshal(WireRates(r))
	if err != nil {
		t.Fatalf("marshal WireRates: %v", err)
	}
	wantWire := `{"CacheRead":0.1,"CacheWrite":0.3,"Input":1,"Output":2,"Reasoning":4}`
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
	u := Usage{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4, Reasoning: 5}

	native, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal Usage: %v", err)
	}
	wantNative := `{"input":1,"output":2,"cache_read":3,"cache_write":4,"reasoning":5}`
	if string(native) != wantNative {
		t.Fatalf("Usage marshal = %s, want %s (存储键必须保持 snake)", native, wantNative)
	}

	wire, err := json.Marshal(WireUsage(u))
	if err != nil {
		t.Fatalf("marshal WireUsage: %v", err)
	}
	wantWire := `{"Input":1,"Output":2,"CacheRead":3,"CacheWrite":4,"Reasoning":5}`
	if string(wire) != wantWire {
		t.Fatalf("WireUsage marshal = %s, want %s", wire, wantWire)
	}

	// 视图与存储结构可直接互转（字段同名同类型，仅 tag 不同）。
	if !reflect.DeepEqual(Usage(WireUsage(u)), u) {
		t.Fatalf("WireUsage<->Usage 转换不等价: %+v", Usage(WireUsage(u)))
	}
}
