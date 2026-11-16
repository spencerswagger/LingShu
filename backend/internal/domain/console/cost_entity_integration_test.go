package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/db"
)

// 实体聚合的集成测试（可选执行，**只在自己的临时库里跑**，绝不碰共享 test/staging 库）。
//
// 运行方式：
//
//	cd backend
//	LLMGATEWAY_TEST_PG_DSN='postgres://user:pass@host:port/postgres?sslmode=disable' \
//	  go test ./internal/domain/console -run TestCostStats_CrossChannelFallback -v
//
// 未设置该变量时整条测试 t.Skip；设置了就 CREATE DATABASE llmgateway-cost-<纳秒>，
// 跑完 pg_terminate_backend + DROP DATABASE，不留痕。
// 要求的角色权限：CREATEDB（与 internal/db 的迁移集成测试同口径）。

// openCostTempDB 建一个临时库并应用全部迁移，返回连接；测试结束自动删库。
func openCostTempDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("LLMGATEWAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 LLMGATEWAY_TEST_PG_DSN，跳过成本实体聚合集成测试")
	}
	admin, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("打开管理连接失败: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.Ping(); err != nil {
		t.Fatalf("管理连接 ping 失败（DSN 是否可达?）: %v", err)
	}

	name := fmt.Sprintf("llmgateway-cost-%d", time.Now().UnixNano())
	if !regexp.MustCompile(`^[a-z0-9-]+$`).MatchString(name) {
		t.Fatalf("临时库名非法: %s", name)
	}
	if _, err := admin.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
		t.Fatalf("创建临时库 %s 失败: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, name)
		if _, derr := admin.Exec(`DROP DATABASE IF EXISTS "` + name + `"`); derr != nil {
			t.Logf("删除临时库 %s 失败: %v", name, derr)
		}
	})

	tempDSN, err := replaceDSNDatabase(dsn, name)
	if err != nil {
		t.Fatalf("构造临时库 DSN 失败: %v", err)
	}
	temp, err := db.Open(tempDSN)
	if err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	t.Cleanup(func() { _ = temp.Close() })
	if err := temp.Ping(); err != nil {
		t.Fatalf("临时库 ping 失败: %v", err)
	}
	if _, err := db.Migrate(context.Background(), temp); err != nil {
		t.Fatalf("应用迁移失败: %v", err)
	}
	return temp
}

// replaceDSNDatabase 把 DSN 的库名换成 name（保留其余参数）。
func replaceDSNDatabase(dsn, name string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	u.Path = "/" + name
	return u.String(), nil
}

// execCostSQL 执行一条初始化 SQL（失败即终止测试）。
func execCostSQL(t *testing.T, d *sql.DB, sqlText string) {
	t.Helper()
	if _, err := d.Exec(sqlText); err != nil {
		t.Fatalf("执行 SQL 失败: %v\nSQL: %s", err, sqlText)
	}
}

// TestCostStats_CrossChannelFallback 断言降级实体桶按渠道分开：
// 同一「对外模型文本 + 内部模型」在渠道 1 / 渠道 2 / 无渠道下必须是**三行**，各自金额正确，
// 且已解析成功的实体行（cm:<id>）不受影响。
// TestCostStats_ModelDim 成本统计在真实迁移库上的实体口径（0024 之后：**渠道维度已下线**）。
//
// 与改造前的差别（有意为之，不是回归）：
//   - 键只由「对外模型文本 + 内部模型」构成（`name:<模型>#<内部模型>`），不再有 `cm:<id>` 与 `@<渠道>`；
//   - 同一文本 + 同一内部模型的账单**合并成一行**（以前按渠道拆开）；
//   - 金额合计不变（分组变化不影响求和）。
func TestCostStats_ModelDim(t *testing.T) {
	d := openCostTempDB(t)

	execCostSQL(t, d, `INSERT INTO users(id, username, password_hash, role) VALUES (7,'dev','x','DEVELOPER')`)
	execCostSQL(t, d, `INSERT INTO external_models(id, external_name) VALUES (31,'gpt-4o')`)
	execCostSQL(t, d, `INSERT INTO billing_records(id,billing_id,user_id,pricing_mode,external_model_name,internal_model_id,r_value,tokens,credits_consumed,cost_credits,status,call_time) VALUES
	 (1,'b1',7,'sale','gpt-4o','gpt-4o-int',1,'{"input":100,"cache_read":0,"output":10}'::jsonb,2.2,1.1,'completed','2026-10-20 10:00:00+08'),
	 (2,'b2',7,'sale','ghost-model','ghost-int',1,'{"input":10,"cache_read":0,"output":0}'::jsonb,0.6,0.3,'completed','2026-10-20 10:01:00+08'),
	 (3,'b3',7,'sale','ghost-model','ghost-int',1,'{"input":10,"cache_read":0,"output":0}'::jsonb,0.8,0.4,'completed','2026-10-20 10:02:00+08'),
	 (4,'b4',7,'sale','ghost-model','ghost-int',1,'{"input":10,"cache_read":0,"output":0}'::jsonb,0.4,0.2,'completed','2026-10-20 10:03:00+08')`)

	h := NewAdmin(d, nil, nil)
	rec := httptest.NewRecorder()
	h.HandleCostStats(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/cost/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body costStatsBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	byKey := make(map[string]costStatsRow, len(body.Data.Rows))
	for _, row := range body.Data.Rows {
		if _, dup := byKey[row.EntityKey]; dup {
			t.Fatalf("EntityKey 重复（前端 row-key 会撞）：%s", row.EntityKey)
		}
		if row.ChannelID != 0 || row.ChannelName != "" || row.ChannelModelID != 0 {
			t.Fatalf("渠道维度必须恒空（表已删）: %+v", row)
		}
		byKey[row.EntityKey] = row
	}

	if len(body.Data.Rows) != 2 {
		t.Fatalf("行数=%d，want 2（按「模型 + 内部模型」聚合），实际=%v", len(body.Data.Rows), keysOf(byKey))
	}
	first, ok := byKey["name:gpt-4o#gpt-4o-int"]
	if !ok {
		t.Fatalf("缺少 gpt-4o 实体行，实际键=%v", keysOf(byKey))
	}
	if first.CostCredits != 1.1 || first.SaleCredits != 2.2 || first.Requests != 1 {
		t.Fatalf("gpt-4o 行金额错: %+v", first)
	}
	ghost, ok := byKey["name:ghost-model#ghost-int"]
	if !ok {
		t.Fatalf("缺少 ghost-model 实体行，实际键=%v", keysOf(byKey))
	}
	if ghost.CostCredits != 0.9 || ghost.SaleCredits != 1.8 || ghost.Requests != 3 {
		t.Fatalf("ghost-model 行应把 3 条同模型账单合并: %+v", ghost)
	}
	// 合计仍等于全部行金额之和（分组变化不影响金额）。
	if body.Data.Total.CostCredits != 2.0 || body.Data.Total.SaleCredits != 4.0 || body.Data.Total.Requests != 4 {
		t.Fatalf("合计错: %+v", body.Data.Total)
	}
}

func keysOf(m map[string]costStatsRow) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
