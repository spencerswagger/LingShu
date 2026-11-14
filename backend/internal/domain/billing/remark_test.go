// remark_test.go 守住「三类流水备注只有一处来源」（契约 §8.3）。
//
// 备注文案是守恒断言的分组键：谁在调用点另写字面量，断言就会静默对不上账，
// 所以这里既断言常量本身，也做一次源码级扫描（不许在别处出现这三个字面量）。
package billing

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFlowRemarks_ThreeCategories(t *testing.T) {
	want := []string{"调用预扣", "实际调用差额", "请求失败退回预扣"}
	got := FlowRemarks()
	if len(got) != len(want) {
		t.Fatalf("类别数=%d want %d (%v)", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("第 %d 类=%q want %q", i, got[i], w)
		}
		if !IsFlowRemark(w) {
			t.Fatalf("IsFlowRemark(%q)=false", w)
		}
	}
	if IsFlowRemark("随便写的备注") {
		t.Fatal("未知文案不该被判成已知类别")
	}
}

// 源码级扫描：三个字面量只允许出现在 remark.go（其余文件必须走常量）。
func TestFlowRemarks_NoStrayLiterals(t *testing.T) {
	root := filepath.Join("..", "..") // internal/
	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "remark.go") || strings.HasSuffix(path, "remark_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, lit := range []string{"调用预扣", "实际调用差额", "请求失败退回预扣"} {
			if strings.Contains(string(raw), lit) {
				offenders = append(offenders, path+" 含 "+lit)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("备注字面量必须走 billing.FlowRemark* 常量：\n%s", strings.Join(offenders, "\n"))
	}
}
