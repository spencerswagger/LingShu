package subscription

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/db"
)

// openSubscriptionTempDB 建临时库并用**真实迁移链**建表（无 DSN / 无 CREATEDB 时跳过）。
//
// 与 amortize / upstream 包的同名 helper 同口径：手写 DDL 只能证明「代码能写进我自己造的表」；
// DATE 与时区这类问题只有在真库上才看得见。
// 返回：目标库句柄、管理库 DSN、临时库名（后者用于 ALTER DATABASE 改会话时区）。
func openSubscriptionTempDB(t *testing.T) (*sql.DB, string, string) {
	t.Helper()
	dsn := os.Getenv("LLMGATEWAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 LLMGATEWAY_TEST_PG_DSN，跳过真实库用例")
	}
	admin, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("打开管理连接失败: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	name := fmt.Sprintf("v2_sub_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(context.Background(), "CREATE DATABASE "+name); err != nil {
		t.Skipf("无法创建临时库（账号是否没有 CREATEDB 权限？）: %v", err)
	}
	t.Cleanup(func() {
		if a, derr := db.Open(dsn); derr == nil {
			_, _ = a.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			_ = a.Close()
		}
	})

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("解析 DSN 失败: %v", err)
	}
	u.Path = "/" + name
	target, err := db.Open(u.String())
	if err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	t.Cleanup(func() { _ = target.Close() })

	if _, err := db.Migrate(context.Background(), target); err != nil {
		t.Fatalf("应用迁移失败: %v", err)
	}
	return target, u.String(), name
}
