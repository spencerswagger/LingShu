package upstream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReconcileCountsHaveSingleWriter 守住「对账计数列只有一个写入方」（A4 裁定）。
//
// 背景：并行期曾同时存在两套对账（#82 的 usagebilling 一套 + 本包的 reconcile 一套），
// 两套都往 reconcile_windows 写计数与状态。后果不是「重复算一遍」这么轻——
// 一套把窗判 blocked（该拉没拉完）时，另一套把它洗成 ok，漏账从此隐身；
// alarm_streak 也会被两套各自累加，审计升级不可解释。
//
// 现在的分工（唯一）：
//   - `SaveCounts`（四计数列 + status + alarm_streak）**只由 reconcile 包调用**；
//   - 同步轮次只写 status / detail / scroll_offset / checked_at（`save`，SQL 里不许出现计数列）。
//
// 这条用例是源码级的：不依赖 DSN，任何新增的第二个写入方都会立刻红。
func TestReconcileCountsHaveSingleWriter(t *testing.T) {
	backendRoot := filepath.Clean(filepath.Join(workDir(t), "..", "..", ".."))

	var offenders []string
	callers := 0
	err := filepath.Walk(backendRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// vendor/测试数据目录不参与（本仓库没有 vendor，留着防将来）。
			if name := info.Name(); name == "vendor" || name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		text := string(body)
		if !strings.Contains(text, ".SaveCounts(") {
			return nil
		}
		callers++
		rel, _ := filepath.Rel(backendRoot, path)
		// 唯一允许的调用方：reconcile 包（对账服务）。
		if filepath.ToSlash(rel) != "internal/domain/reconcile/reconcile.go" {
			offenders = append(offenders, filepath.ToSlash(rel))
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
	if len(offenders) > 0 {
		t.Fatalf("SaveCounts（对账计数列的唯一写入方）被这些文件调用: %v —— "+
			"第二套对账会把 blocked 洗成 ok、把 streak 双计，必须删掉", offenders)
	}

	// 同步轮次的写窗口 SQL 里不许出现计数列（出现即等于第二写入方）。
	saveSQL := windowSaveSQLForTest(t)
	for _, col := range []string{"local_count", "local_tokens", "upstream_count", "upstream_tokens"} {
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
	// save() 的函数体起点到 SaveCounts 注释为止，就是同步轮次的写路径。
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
	// 测试文件与源码同目录，用 runtime 定位更稳，但保持零依赖：借 os.Getwd（go test 的 cwd 即包目录）。
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取工作目录失败: %v", err)
	}
	return dir
}
