package idgen

import (
	"encoding/json"
	"testing"
	"time"
)

func TestGenerator_Next_UniqueAndMonotonic(t *testing.T) {
	g, err := NewGenerator(1)
	if err != nil {
		t.Fatalf("NewGenerator(1): %v", err)
	}
	seen := map[int64]bool{}
	prev := int64(0)
	for i := 0; i < 5000; i++ {
		id := g.Next()
		if id <= 0 {
			t.Fatalf("id 必须为正: %d", id)
		}
		if seen[id] {
			t.Fatalf("重复 ID: %d", id)
		}
		seen[id] = true
		if id <= prev {
			t.Fatalf("ID 应单调递增: prev=%d cur=%d", prev, id)
		}
		prev = id
	}
}

func TestNew_DefaultSingleton(t *testing.T) {
	a, b := New(), New()
	if a == b {
		t.Fatal("两次 New() 不应产生相同 ID")
	}
}

// TestGenerator_ClockRollback_NoBlockNoDuplicate 模拟时钟大幅回拨：
// 逻辑时间戳被推到未来，Next() 不得忙等阻塞，且仍须正数、唯一、单调递增。
func TestGenerator_ClockRollback_NoBlockNoDuplicate(t *testing.T) {
	g, err := NewGenerator(1)
	if err != nil {
		t.Fatalf("NewGenerator(1): %v", err)
	}
	g.mu.Lock()
	g.lastStamp = time.Now().UnixMilli() - epochMs + 10*60*1000 // 未来 10 分钟
	g.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		seen := map[int64]bool{}
		prev := int64(0)
		for i := 0; i < 10000; i++ {
			id := g.Next()
			if id <= 0 {
				t.Errorf("回拨后 ID 必须为正: %d", id)
				return
			}
			if seen[id] {
				t.Errorf("回拨后产生重复 ID: %d", id)
				return
			}
			seen[id] = true
			if id <= prev {
				t.Errorf("回拨后 ID 应单调递增: prev=%d cur=%d", prev, id)
				return
			}
			prev = id
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("时钟回拨后 Next() 阻塞超时（不应忙等自旋）")
	}
}

// TestGenerator_SequenceOverflow_AdvancesLogicalClock 同毫秒序列耗尽时应借用下一
// 逻辑毫秒，而不是忙等下一墙钟毫秒（回拨期间后者会长时间持锁阻塞）。
func TestGenerator_SequenceOverflow_AdvancesLogicalClock(t *testing.T) {
	g, err := NewGenerator(0)
	if err != nil {
		t.Fatalf("NewGenerator(0): %v", err)
	}
	g.mu.Lock()
	g.lastStamp = time.Now().UnixMilli() - epochMs + 60*1000 // 未来，确保走 clamp 分支
	g.sequence = maxSequence
	base := g.lastStamp
	g.mu.Unlock()

	if id := g.Next(); id <= 0 {
		t.Fatalf("ID 必须为正: %d", id)
	}

	g.mu.Lock()
	gotStamp, gotSeq := g.lastStamp, g.sequence
	g.mu.Unlock()
	if gotStamp != base+1 || gotSeq != 0 {
		t.Fatalf("序列耗尽应借用下一逻辑毫秒: lastStamp=%d want=%d seq=%d", gotStamp, base+1, gotSeq)
	}
}

func TestIDs_JSON_Roundtrip(t *testing.T) {
	ids := IDs{1, 2, 3}
	b, err := json.Marshal(ids)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `["1","2","3"]` {
		t.Fatalf("marshal 结果不符: %s", b)
	}

	var fromStr, fromNum IDs
	if err := json.Unmarshal([]byte(`["1","2","3"]`), &fromStr); err != nil {
		t.Fatalf("unmarshal string array: %v", err)
	}
	if len(fromStr) != 3 || fromStr[2] != 3 {
		t.Fatalf("unmarshal string array 值不符: %v", fromStr)
	}
	if err := json.Unmarshal([]byte(`[4,5,6]`), &fromNum); err != nil {
		t.Fatalf("unmarshal number array: %v", err)
	}
	if len(fromNum) != 3 || fromNum[0] != 4 {
		t.Fatalf("unmarshal number array 值不符: %v", fromNum)
	}
}
