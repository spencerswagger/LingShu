// Package pgtest 是「真库用例」的最小脚手架：以 LLMGATEWAY_TEST_PG_DSN 为管理连接，
// 建一次性库、跑全部迁移、测试结束无条件删库。
//
// 为什么需要它：真库用例只在「DSN 可达且**目标库已迁移**」时才有意义。若用例靠
// 「库里没有表就 t.Skip」自我保护，CI 里 DSN 指向空库时就会永久跳过——测试是绿的，
// 但一次真库读写都没发生（假绿）。自建库 + 迁移把这条路径变成真跑：建不出来/迁移不了就直接红。
//
// 只在 _test.go 里被引用，不进入生产二进制。
package pgtest

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/db"
)

// dsnEnv 管理连接的环境变量名（与 internal/db、billing、channel、gateway 的真库用例一致）。
const dsnEnv = "LLMGATEWAY_TEST_PG_DSN"

var dbNameRe = regexp.MustCompile(`^[a-z0-9-]+$`)

// OpenTempMigrated 返回一条指向「新建 + 已跑完全部迁移」的临时库的连接。
// 未设置 LLMGATEWAY_TEST_PG_DSN 时 t.Skip（本机没有 PG 的开发者不阻塞）；其余任何失败都 t.Fatal，
// 绝不「跳过」——跳过等于把真库守卫变成摆设。
func OpenTempMigrated(t *testing.T, prefix string) *sql.DB {
	t.Helper()
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skipf("未设置 %s，跳过真库用例（需要可建库的 PG）", dsnEnv)
	}
	admin, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("打开管理连接失败: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.Ping(); err != nil {
		t.Fatalf("管理连接 ping 失败（DSN 是否可达?）: %v", err)
	}

	name := fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	if !dbNameRe.MatchString(name) {
		t.Fatalf("临时库名非法: %s", name)
	}
	if _, err := admin.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
		t.Fatalf("创建临时库 %s 失败（连接用户需要 CREATEDB 权限）: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, name)
		if _, derr := admin.Exec(`DROP DATABASE IF EXISTS "` + name + `"`); derr != nil {
			t.Logf("删除临时库 %s 失败: %v", name, derr)
		}
	})

	tmpl, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("解析 DSN 失败: %v", err)
	}
	tmpl.Path = "/" + name
	temp, err := db.Open(tmpl.String())
	if err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	t.Cleanup(func() { _ = temp.Close() })
	if _, err := db.Migrate(context.Background(), temp); err != nil {
		t.Fatalf("临时库迁移失败: %v", err)
	}
	t.Logf("临时库已就绪: %s（已跑全部迁移）", name)
	return temp
}
