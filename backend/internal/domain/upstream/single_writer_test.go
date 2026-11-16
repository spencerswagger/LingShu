package upstream

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 对账四个计数列的名字（A4 裁定：只有对账服务能写）。
var countColumns = []string{"local_count", "local_tokens", "upstream_count", "upstream_tokens"}

// allowlistCountColumn 是**允许出现**计数列的文件与理由（键为相对 backend 的路径）。
//
// 口径不是「谁写了什么函数」，而是「这四个列名出现在哪些文件里可以接受」：
//   - 定义/写入：window_store.go 的 SaveCounts（唯一写入方，函数体由 SaveCounts 调用点守卫）；
//   - 只读展示：console/ops.go 的运营查询（读不写）；
//   - 结构体与报告：window.go / reconcile 的字段与日志（不含 SQL 写法）。
//
// 任何**新增**文件出现这四个列名都要在这里显式登记，否则用例红 ——
// 这是 Owl 复审 #85 P2 的修法：旧守卫只匹配 `.SaveCounts(`，
// 塞一个 `UPDATE reconcile_windows SET local_count=…` 的旁路文件照样绿。
var allowlistCountColumn = map[string]string{
	"internal/domain/upstream/window_store.go": "SaveCounts：唯一的写入方（字段经 bind 传入）",
	"internal/domain/upstream/window.go":       "WindowCounts 结构体字段定义",
	"internal/domain/upstream/window_test.go":  "结构体断言（测试）",
	"internal/domain/reconcile/reconcile.go":   "对账服务：组装 WindowCounts 并调用 SaveCounts",
	"internal/domain/console/ops.go":           "运营只读查询（SELECT，不写）",
}

// TestReconcileCountsHaveSingleWriter 守住「对账计数列只有一个写入方」（A4 裁定）。
//
// 背景：并行期曾同时存在两套对账（#82 的 usagebilling 一套 + 本包的 reconcile 一套），
// 两套都往 reconcile_windows 写计数与状态。后果不是「重复算一遍」这么轻——
// 一套把窗判 blocked（该拉没拉完）时，另一套把它洗成 ok，漏账从此隐身；
// alarm_streak 也会被两套各自累加，审计升级不可解释。
//
// 守卫分三层（都是源码级、不依赖 DSN）：
//  1. 调用点：`.SaveCounts(` 只许 reconcile 包调（1 处，0 处视为守卫失效）；
//  2. **列名级**：四个计数列只许出现在白名单文件里 —— 绕开 SaveCounts 直接写 raw SQL 也会被抓；
//  3. 写路径：同步轮次的 save() SQL 里不许出现计数列。
func TestReconcileCountsHaveSingleWriter(t *testing.T) {
	backendRoot := filepath.Clean(filepath.Join(workDir(t), "..", "..", ".."))

	callers := 0
	var callOffenders []string
	var colOffenders []string
	colHits := map[string]bool{}

	colRe := regexp.MustCompile(`\b(local_count|local_tokens|upstream_count|upstream_tokens)\b`)

	err := filepath.Walk(backendRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// vendor/测试数据与迁移目录不参与（迁移是 DDL，不是运行时写入方）。
			name := info.Name()
			if name == "vendor" || name == "testdata" || name == "migrations" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		text := string(body)
		rel, _ := filepath.Rel(backendRoot, path)
		rel = filepath.ToSlash(rel)

		// 第 1 层：SaveCounts 调用点。
		if !strings.HasSuffix(path, "_test.go") && strings.Contains(text, ".SaveCounts(") {
			callers++
			if rel != "internal/domain/reconcile/reconcile.go" {
				callOffenders = append(callOffenders, rel)
			}
		}

		// 第 2 层：列名级白名单（SQL 直写 counts 列会在这里被拦）。
		// 测试文件不参与：它们只造数据/断言列名，不进生产二进制（真出现生产写入方
		// 一定是非测试文件，那才是这条守卫要抓的东西）。
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if hits := colRe.FindAllString(text, -1); len(hits) > 0 {
			for _, h := range hits {
				colHits[h] = true
			}
			if _, ok := allowlistCountColumn[rel]; !ok {
				colOffenders = append(colOffenders, rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描源码失败: %v", err)
	}

	if callers != 1 {
		// 0 = 扫描没命中（路径/写法变了，用例形同虚设）；>1 = 出现了第二套对账。
		t.Fatalf("SaveCounts 调用点应为 1 处（reconcile 包），实际 %d 处 —— 0 处说明守卫失效，必须修用例", callers)
	}
	if len(callOffenders) > 0 {
		t.Fatalf("SaveCounts（对账计数列的唯一写入方）被这些文件调用: %v —— "+
			"第二套对账会把 blocked 洗成 ok、把 streak 双计，必须删掉", callOffenders)
	}
	if len(colOffenders) > 0 {
		t.Fatalf("这些文件直接引用了对账计数列但没有登记来源: %v —— "+
			"四个计数列只许对账服务经 SaveCounts 写入；确属只读展示请在 allowlistCountColumn 里登记理由",
			colOffenders)
	}
	// 白名单不能「凑数生效」：四个列名必须都真的在非测试源码里被扫到，否则说明正则或路径漂了。
	for _, col := range countColumns {
		if !colHits[col] {
			t.Fatalf("扫描未命中列名 %q —— 正则/路径漂移，守卫形同虚设", col)
		}
	}

	// 第 3 层：同步轮次的写窗口 SQL 里不许出现计数列。
	saveSQL := windowSaveSQLForTest(t)
	for _, col := range countColumns {
		if strings.Contains(saveSQL, col) {
			t.Fatalf("同步轮次的写窗口语句出现了计数列 %q：计数只许对账服务写（D4）", col)
		}
	}
}

// windowSaveSQLForTest 读 window_store.go 里 save() 的那两条 SQL（同步轮次的写路径）。
func windowSaveSQLForTest(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(workDir(t), "window_store.go"))
	if err != nil {
		t.Fatalf("读取 window_store.go 失败: %v", err)
	}
	text := string(body)
	start := strings.Index(text, "func (s *SQLWindowStore) save(")
	if start < 0 {
		t.Fatalf("未找到 SQLWindowStore.save（重构后请同步更新本用例）")
	}
	end := strings.Index(text[start:], "// SaveCounts 写对账的四个计数列")
	if end < 0 {
		t.Fatalf("未找到 SaveCounts 的注释边界（重构后请同步更新本用例）")
	}
	return text[start : start+end]
}

// workDir 返回当前测试文件所在目录（不依赖 go test 的工作目录）。
func workDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取工作目录失败: %v", err)
	}
	return dir
}
