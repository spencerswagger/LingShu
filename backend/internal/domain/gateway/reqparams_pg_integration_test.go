package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/team/llmgateway/internal/db"
)

// 本文件是 PR #63 验收「8KB 校准」的**真实库实测**（可选执行）：
//
//	cd backend
//	LLMGATEWAY_TEST_PG_DSN='postgres://user:pass@host:port/db?sslmode=disable' \
//	  go test ./internal/domain/gateway -run TestReqParams_PgColumnSize -v
//
// 未设置该变量时 t.Skip。断言口径与 Gate 一致：`pg_column_size(req_params) ≤ 8192`，
// 覆盖「压线 / 多字节中文与 emoji / 60 个参数 / 受保护参数超预算」等 case，
// 同时验证 jsonbSizeBound 是真实落盘大小的**上界**（预算就是按它算的）。

// openProbeConn 取一条独立连接（TEMP 表按会话存活，测试结束自动消失，不污染库）。
func openProbeConn(t *testing.T) *sql.Conn {
	t.Helper()
	dsn := os.Getenv("LLMGATEWAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 LLMGATEWAY_TEST_PG_DSN，跳过 pg_column_size 实测")
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("打开连接失败: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	conn, err := d.Conn(context.Background())
	if err != nil {
		t.Fatalf("取连接失败: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.ExecContext(context.Background(), `CREATE TEMP TABLE req_params_probe(p jsonb)`); err != nil {
		t.Fatalf("建临时表失败: %v", err)
	}
	return conn
}

// pgColumnSize 实测 jsonb 落盘大小（与 Gate 的断言同口径）。
func pgColumnSize(t *testing.T, conn *sql.Conn, raw []byte) int {
	t.Helper()
	var size int
	if err := conn.QueryRowContext(context.Background(),
		`INSERT INTO req_params_probe(p) VALUES ($1::jsonb) RETURNING pg_column_size(p)`,
		string(raw)).Scan(&size); err != nil {
		t.Fatalf("测量 pg_column_size 失败: %v", err)
	}
	return size
}

// TestReqParams_PgColumnSize 逐 case 断言「落库 ≤ 8192」与「估算上界覆盖实测」。
func TestReqParams_PgColumnSize(t *testing.T) {
	conn := openProbeConn(t)

	cases := []struct {
		name  string
		body  []byte
		check func(t *testing.T, snap reqParamsSnapshot, n int)
	}{
		{
			// Gate 的现场：60 个参数（含中文），要求可对账。
			name: "gate_60_params_cjk",
			body: func() []byte {
				var sb strings.Builder
				sb.WriteString(`{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"hi"}],
					"temperature":0.7,"max_tokens":2048,"top_p":1,"response_format":{"type":"json_object"}`)
				for i := 0; i < 60; i++ {
					sb.WriteString(`,"参数_`)
					sb.WriteString(strconv.Itoa(i)) // 键唯一（模拟 Gate 的 60 个真实参数）
					sb.WriteString(`":"` + strings.Repeat("中文内容", 1+i*3) + `"`)
				}
				sb.WriteString(`}`)
				return []byte(sb.String())
			}(),
			check: func(t *testing.T, snap reqParamsSnapshot, n int) {
				got := len(snap.Params) + len(snap.Dropped)
				if got != n {
					t.Fatalf("可对账不变量失败：params=%d + dropped=%d = %d, want %d", len(snap.Params), len(snap.Dropped), got, n)
				}
			},
		},
		{
			// 压线 case：多个 1.9KB 参数 + 少量小参数，逼到预算边缘。
			name: "near_limit_multi_byte",
			body: func() []byte {
				var sb strings.Builder
				sb.WriteString(`{"model":"m","stream":false,"messages":[],"temperature":1,"max_tokens":1,"top_p":1,
					"response_format":{"type":"text"}`)
				// 每个值 ~1.9KB（低于 2KB 单字段上限，不会被换成标记），4 个≈7.6KB → 压线。
				for i := 0; i < 4; i++ {
					sb.WriteString(`, "edge_` + string(rune('a'+i)) + `":"` + strings.Repeat("😀中", 280) + `"`)
				}
				sb.WriteString(`}`)
				return []byte(sb.String())
			}(),
			check: nil,
		},
		{
			// 极端核心字段：超长上游 URL（应被截断且记名）。
			name:  "huge_upstream_url",
			body:  []byte(`{"model":"m","messages":[],"temperature":1,"max_tokens":1,"top_p":1,"response_format":{"type":"text"}}`),
			check: nil,
		},
		{
			// 受保护参数自身超预算：走到最后一层「丢受保护参数但逐个记名」。
			name: "protected_overflow",
			body: func() []byte {
				var sb strings.Builder
				sb.WriteString(`{"model":"m","messages":[]`)
				// 每个受保护值 ~1.9KB（低于 2KB 上限 → 不被换标记），12 个合计 ≈22KB → 必然触发
				// 「丢受保护参数」的最后一层，用来验证该层也逐个记名（P2）。
				for i, k := range []string{"temperature", "max_tokens", "max_completion_tokens", "top_p", "response_format", "n", "seed", "stop", "tool_choice", "reasoning_effort", "thinking", "enable_thinking"} {
					sb.WriteString(`, "` + k + `":"` + strings.Repeat("中", 630) + strconv.Itoa(i) + `"`)
				}
				sb.WriteString(`}`)
				return []byte(sb.String())
			}(),
			check: func(t *testing.T, snap reqParamsSnapshot, n int) {
				got := len(snap.Params) + len(snap.Dropped)
				if got != n {
					t.Fatalf("受保护参数溢出后可对账失败：params=%d + dropped=%d = %d, want %d",
						len(snap.Params), len(snap.Dropped), got, n)
				}
				if len(snap.Dropped) == 0 {
					t.Fatalf("22KB 受保护参数不可能全塞进 8KB，dropped 却为空：%+v", snap)
				}
				if !snap.OverBudget {
					t.Fatalf("走到最后一层应标记 over_budget=true")
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := newReqParamsSource(c.body)
			var n int
			var top map[string]json.RawMessage
			if err := json.Unmarshal(c.body, &top); err == nil {
				for k := range top {
					switch k {
					case "model", "stream", "messages", "tools", "functions", "input":
						continue
					}
					n++
				}
			}
			upstream := "https://up.example.com/v1/chat/completions"
			if c.name == "huge_upstream_url" {
				upstream = "https://up.example.com/" + strings.Repeat("segment/", 400) + "chat/completions?api-key=sk-should-not-leak"
			}
			raw := src.buildReqParams(reqParamsInput{
				Endpoint:         "/v1/chat/completions",
				InboundProtocol:  "openai-chat",
				UpstreamProtocol: "openai-chat",
				UpstreamURL:      upstream,
				TokenID:          42,
				Model:            "gpt-4",
				Stream:           true,
			})
			if raw == nil {
				t.Fatal("snapshot is nil")
			}
			if strings.Contains(string(raw), "sk-should-not-leak") {
				t.Fatal("上游 URL 的 query 泄漏进快照")
			}
			var snap reqParamsSnapshot
			if err := json.Unmarshal(raw, &snap); err != nil {
				t.Fatalf("反序列化快照失败: %v", err)
			}
			size := pgColumnSize(t, conn, raw)
			if size > reqParamsMaxBytes {
				t.Fatalf("pg_column_size=%d > %d（JSON 文本 %d 字节，params=%d dropped=%d）",
					size, reqParamsMaxBytes, len(raw), len(snap.Params), len(snap.Dropped))
			}
			bound := jsonbSizeBound(raw, coreKVCount+len(snap.Params)+len(snap.Summarized)+len(snap.Dropped))
			if size > bound {
				t.Fatalf("估算上界不足：pg_column_size=%d > jsonbSizeBound=%d（需上调余量）", size, bound)
			}
			if c.check != nil {
				c.check(t, snap, n)
			}
			t.Logf("%s: pg_column_size=%d / bound=%d / json=%d / params=%d / dropped=%d / summarized=%d",
				c.name, size, bound, len(raw), len(snap.Params), len(snap.Dropped), len(snap.Summarized))
		})
	}
}
