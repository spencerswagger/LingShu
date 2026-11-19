package billing

// 本期口径（2026-11-16 裁定）：`billing_records` 的 magpie_key_name / estimated / upstream_usage
// **三列保留、但禁止写入**。它们不由本批创建，也不由本批删除，只随阶段二的新形处理。
//
// 之所以要专门守这道门：列还在，误写不会报错 —— INSERT 会**静默成功**、账单照出，出账金额却
// 走到一条已经下线的口径上（旧实现按 estimated 出估算钱的路径）。数据库层给不出任何信号，
// 只有「源码里没有写入方」和「写进去之后这三列仍为 NULL」这两条断言拦得住。
//
// 与迁移的关系：0024 的执行前探测② 已按同一裁定改口径（列存在不算异常），本文件就是它引用
// 的那道守卫（见 0024_v2_drop_legacy.sql 头部 ②）。

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// legacyBillingColumns 是本期保留但禁止写入的三列。
var legacyBillingColumns = []string{"magpie_key_name", "estimated", "upstream_usage"}

// billingDomains 是「账单写入/读取」所在的包：这里的代码一旦提到这三列，就是写入方或引用方。
var billingDomains = []string{"billing"}

// TestBilling_LegacyColumnsHaveNoWriter 守 0024 探测②的新口径（列存在可以，写入/引用必须为 0）。
//
// 前半段是源码级：账单域的非测试代码里，去掉注释后不许出现这三个列名。
// 后半段是真实库级：在一个**三列都存在**的库（模拟跑过旧分支的库）上插一条账单，断言三列仍为 NULL。
//
// 反向验证（判据不空转）：往 insertOn 的列清单里加一个 magpie_key_name，前半段立刻红；
// 往插入参数里塞一个值，后半段红。
func TestBilling_LegacyColumnsHaveNoWriter(t *testing.T) {
	// ── 源码级：账单域不得出现这三列（注释除外）────────────────────────
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("取不到测试文件路径")
	}
	backendRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..") // backend/

	scanned := 0
	for _, domain := range billingDomains {
		dir := filepath.Join(backendRoot, "internal", "domain", domain)
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			scanned++
			src := stripGoComments(string(raw))
			for _, col := range legacyBillingColumns {
				if strings.Contains(src, col) {
					t.Errorf("%s 引用了本期禁止写入的账单列 %q（口径：列保留、禁止写入；"+
						"0024 探测② 靠这道守卫而不是「列不存在」兜底）",
						filepath.Base(path), col)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("扫描 %s 失败: %v", dir, err)
		}
	}
	if scanned == 0 {
		t.Fatalf("一个源文件都没扫到（backendRoot 解析错了？%s）", backendRoot)
	}

	// ── 真实库级：三列都在的库上写一条账单，三列必须仍为 NULL ──────────
	conn := openAttributionTempDB(t)
	seedBillingUser(t, conn)
	ctx := context.Background()

	// 模拟「跑过旧分支」的库：把三列加回去。这本身**不是缺陷**（本期保留列）。
	for _, stmt := range []string{
		`ALTER TABLE billing_records ADD COLUMN magpie_key_name TEXT`,
		`ALTER TABLE billing_records ADD COLUMN estimated BOOLEAN`,
		`ALTER TABLE billing_records ADD COLUMN upstream_usage JSONB`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("模拟旧分支的列存在失败（%s）: %v", stmt, err)
		}
	}

	store := NewSqlStore(conn)
	rec := newRealStoreRecord("bill-legacycols-1")
	rec.ChannelKeyID = 7 // 渠道维度仍是运行时实体，落库走 call_logs 快照，不许落这三列
	if _, err := store.Insert(ctx, rec); err != nil {
		t.Fatalf("三列存在时账单写入失败（口径要求：列在也不影响写入）: %v", err)
	}

	var keyName, upstreamUsage sql.NullString
	var estimated sql.NullBool
	if err := conn.QueryRowContext(ctx,
		`SELECT magpie_key_name, estimated, upstream_usage FROM billing_records WHERE billing_id = $1`,
		"bill-legacycols-1").Scan(&keyName, &estimated, &upstreamUsage); err != nil {
		t.Fatalf("回读三列失败: %v", err)
	}
	if keyName.Valid || estimated.Valid || upstreamUsage.Valid {
		t.Fatalf("账单写入路径动了已下线的列：magpie_key_name=%v estimated=%v upstream_usage=%v（必须全 NULL）",
			keyName, estimated, upstreamUsage)
	}
}

// stripGoComments 去掉 // 与 /* */ 注释，保留字符串字面量（列名只会在字符串里出现）。
// 逐字符扫描：// 之前若有未闭合的字符串就按字面量处理，避免把 "http://..." 里的 // 当注释。
func stripGoComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	inLine := false   // 当前在 // 注释里
	inBlock := false  // 当前在 /* */ 注释里
	inString := false // 当前在 "..." 里
	inRaw := false    // 当前在 `...` 里
	escaped := false  // 上一个字符是反斜杠
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inLine {
			if c == '\n' {
				inLine = false
				b.WriteByte(c)
			}
			continue
		}
		if inBlock {
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				inBlock = false
				i++
			}
			continue
		}
		if inRaw {
			b.WriteByte(c)
			if c == '`' {
				inRaw = false
			}
			continue
		}
		if inString {
			b.WriteByte(c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch {
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			inLine = true
			i++
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			inBlock = true
			i++
		case c == '"':
			inString = true
			b.WriteByte(c)
		case c == '`':
			inRaw = true
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
