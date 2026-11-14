// remark.go 是资金流水备注的**唯一来源**（契约 §8.3）。
//
// 为什么把文案提成常量：钱包守恒断言按「user_id × 60s 窗口 × 备注类别」聚合，
// 备注文案就是分组键。散落在各调用点的字面量一旦被改一个字，断言会静默失效
// （不报错、只是再也对不上账），所以文案只能从这里取。
//
// 本期不补 billing_id（Ark 已定，二期随状态机做）：三类流水靠 user + 时间窗 + 类别三者守恒，
// 不靠单笔关联。
package billing

// 三类资金流水备注（顺序即结算生命周期：预扣 → 差额 → 退回）。
const (
	// FlowRemarkReserve 是下发上游前的预扣冻结。
	FlowRemarkReserve = "调用预扣"
	// FlowRemarkDelta 是拿到 usage 后的多退少补（正数补扣、负数退回）。
	FlowRemarkDelta = "实际调用差额"
	// FlowRemarkRefund 是失败/无内容时把预扣整笔退回。
	FlowRemarkRefund = "请求失败退回预扣"
)

// FlowRemarks 返回全部备注类别（守恒断言与测试按它遍历，避免漏类别）。
func FlowRemarks() []string {
	return []string{FlowRemarkReserve, FlowRemarkDelta, FlowRemarkRefund}
}

// IsFlowRemark 判定文案是否属于已知类别（未知文案说明有新链路绕过了常量）。
func IsFlowRemark(remark string) bool {
	for _, r := range FlowRemarks() {
		if r == remark {
			return true
		}
	}
	return false
}
